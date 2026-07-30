package streaming

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/provideradapter"
)

const maximumStreamTimeout = 24 * time.Hour

var (
	// ErrTimeoutConfiguration means streaming deadline inputs are unsafe.
	ErrTimeoutConfiguration = errors.New("stream timeout configuration is invalid")
	// ErrTimeoutState means lifecycle methods were called out of order.
	ErrTimeoutState = errors.New("stream timeout lifecycle state is invalid")
	// ErrStreamClosed means the guarded stream was explicitly closed.
	ErrStreamClosed = errors.New("guarded stream is closed")
	// ErrStreamingTimeout classifies all controller-owned streaming deadlines.
	ErrStreamingTimeout = errors.New("streaming deadline exceeded")
	// ErrFirstTokenTimeout means no client-visible model delta arrived in time.
	ErrFirstTokenTimeout = errors.New("first model token deadline exceeded")
	// ErrNoProgressTimeout means an established model stream stopped advancing.
	ErrNoProgressTimeout = errors.New("stream no-progress deadline exceeded")
	// ErrTotalStreamTimeout means the whole gateway streaming allowance expired.
	ErrTotalStreamTimeout = errors.New("total stream deadline exceeded")
)

// TimeoutKind is a stable, content-free streaming deadline classification.
type TimeoutKind string

const (
	// TimeoutFirstToken identifies the post-header, pre-model-output phase.
	TimeoutFirstToken TimeoutKind = "first_token"
	// TimeoutNoProgress identifies a stalled stream after model output began.
	TimeoutNoProgress TimeoutKind = "no_progress"
	// TimeoutTotal identifies exhaustion of the complete attempt allowance.
	TimeoutTotal TimeoutKind = "total"
)

// TimeoutOptions bounds the full attempt and the two post-header phases.
type TimeoutOptions struct {
	FirstTokenTimeout time.Duration
	NoProgressTimeout time.Duration
	TotalTimeout      time.Duration
}

// TimeoutFailure preserves the retry/partial boundary without provider data.
type TimeoutFailure struct {
	kind                      TimeoutKind
	headersReceived           bool
	modelOutputStarted        bool
	retryEligibleBeforeOutput bool
	partialFailure            bool
}

// Kind returns the deadline phase.
func (failure *TimeoutFailure) Kind() TimeoutKind {
	if failure == nil {
		return ""
	}
	return failure.kind
}

// HeadersReceived reports whether a usable Provider HTTP response existed.
func (failure *TimeoutFailure) HeadersReceived() bool {
	return failure != nil && failure.headersReceived
}

// ModelOutputStarted reports whether a client-visible model delta was seen.
func (failure *TimeoutFailure) ModelOutputStarted() bool {
	return failure != nil && failure.modelOutputStarted
}

// RetryEligibleBeforeOutput reports the narrow P08 transparent-failover fact.
func (failure *TimeoutFailure) RetryEligibleBeforeOutput() bool {
	return failure != nil && failure.retryEligibleBeforeOutput
}

// PartialFailure reports whether the timeout followed model output.
func (failure *TimeoutFailure) PartialFailure() bool {
	return failure != nil && failure.partialFailure
}

// Error returns a stable message without wrapping provider or client content.
func (failure *TimeoutFailure) Error() string {
	if failure == nil {
		return "<nil>"
	}
	switch failure.kind {
	case TimeoutFirstToken:
		return ErrFirstTokenTimeout.Error()
	case TimeoutNoProgress:
		return ErrNoProgressTimeout.Error()
	case TimeoutTotal:
		return ErrTotalStreamTimeout.Error()
	default:
		return ErrStreamingTimeout.Error()
	}
}

// Unwrap supports errors.Is for the generic deadline, phase sentinel, and the
// standard deadline classification.
func (failure *TimeoutFailure) Unwrap() []error {
	if failure == nil {
		return nil
	}
	phase := ErrStreamingTimeout
	switch failure.kind {
	case TimeoutFirstToken:
		phase = ErrFirstTokenTimeout
	case TimeoutNoProgress:
		phase = ErrNoProgressTimeout
	case TimeoutTotal:
		phase = ErrTotalStreamTimeout
	}
	return []error{ErrStreamingTimeout, phase, context.DeadlineExceeded}
}

// TimeoutSnapshot is safe for metrics, Attempt classification, and tests.
type TimeoutSnapshot struct {
	StartedAt                 time.Time
	HeadersReceivedAt         *time.Time
	FirstModelEventAt         *time.Time
	LastUpstreamEventAt       *time.Time
	UpstreamEvents            uint64
	GatewayHeartbeats         uint64
	ModelOutputStarted        bool
	TimedOut                  bool
	TimeoutKind               TimeoutKind
	RetryEligibleBeforeOutput bool
	PartialFailure            bool
}

// TimeoutController owns one attempt Context from before dialing until stream
// termination. Use Context for BuildRequest and DoStream, then call Attach only
// after a usable HTTP response header has been received.
type TimeoutController struct {
	ctx     context.Context
	cancel  context.CancelCauseFunc
	options TimeoutOptions

	mu                   sync.Mutex
	startedAt            time.Time
	headersReceivedAt    *time.Time
	firstModelEventAt    *time.Time
	lastUpstreamEventAt  *time.Time
	upstreamEvents       uint64
	gatewayHeartbeats    uint64
	stream               provideradapter.ChunkStream
	guarded              *GuardedStream
	terminalErr          error
	timeoutFailure       *TimeoutFailure
	totalTimer           *time.Timer
	firstTokenTimer      *time.Timer
	noProgressTimer      *time.Timer
	noProgressGeneration uint64
	closeOnce            sync.Once
	closeErr             error
}

// NewTimeoutController starts the total attempt allowance. Post-header timers
// do not begin until Attach, so HTTP header arrival cannot be confused with a
// client-visible model event.
func NewTimeoutController(parent context.Context, options TimeoutOptions) (*TimeoutController, error) {
	if parent == nil || !validStreamTimeout(options.FirstTokenTimeout) ||
		!validStreamTimeout(options.NoProgressTimeout) || !validStreamTimeout(options.TotalTimeout) {
		return nil, ErrTimeoutConfiguration
	}
	ctx, cancel := context.WithCancelCause(parent)
	controller := &TimeoutController{
		ctx: ctx, cancel: cancel, options: options, startedAt: time.Now().UTC(),
	}
	controller.mu.Lock()
	controller.totalTimer = time.AfterFunc(options.TotalTimeout, func() {
		controller.timeout(TimeoutTotal, 0)
	})
	controller.mu.Unlock()
	return controller, nil
}

// Context must be used for adapter construction and the upstream HTTP request.
func (controller *TimeoutController) Context() context.Context {
	if controller == nil {
		return nil
	}
	return controller.ctx
}

// Attach marks the exact HTTP-header boundary, takes ownership of the opened
// upstream stream, and starts the absolute first-model-token timer.
func (controller *TimeoutController) Attach(upstream provideradapter.ChunkStream) (*GuardedStream, error) {
	if controller == nil || isNilChunkStream(upstream) {
		if !isNilChunkStream(upstream) {
			_ = upstream.Close()
		}
		return nil, ErrTimeoutState
	}
	now := time.Now().UTC()
	controller.mu.Lock()
	if controller.headersReceivedAt != nil || controller.stream != nil || controller.guarded != nil {
		controller.mu.Unlock()
		_ = upstream.Close()
		return nil, ErrTimeoutState
	}
	if controller.terminalErr != nil {
		terminalErr := controller.terminalErr
		controller.mu.Unlock()
		_ = upstream.Close()
		return nil, terminalErr
	}
	controller.headersReceivedAt = &now
	controller.stream = upstream
	guarded := &GuardedStream{controller: controller, upstream: upstream}
	controller.guarded = guarded
	controller.firstTokenTimer = time.AfterFunc(controller.options.FirstTokenTimeout, func() {
		controller.timeout(TimeoutFirstToken, 0)
	})
	controller.mu.Unlock()
	return guarded, nil
}

// RecordGatewayHeartbeat records gateway-owned liveness evidence but
// deliberately does not change the first-token or no-progress clocks.
func (controller *TimeoutController) RecordGatewayHeartbeat() error {
	if controller == nil {
		return ErrTimeoutState
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.headersReceivedAt == nil || controller.terminalErr != nil {
		return ErrTimeoutState
	}
	controller.gatewayHeartbeats++
	return nil
}

// Snapshot returns only timing and state facts; no model or provider content is
// retained by the controller.
func (controller *TimeoutController) Snapshot() TimeoutSnapshot {
	if controller == nil {
		return TimeoutSnapshot{}
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	snapshot := TimeoutSnapshot{
		StartedAt:           controller.startedAt,
		HeadersReceivedAt:   cloneTime(controller.headersReceivedAt),
		FirstModelEventAt:   cloneTime(controller.firstModelEventAt),
		LastUpstreamEventAt: cloneTime(controller.lastUpstreamEventAt),
		UpstreamEvents:      controller.upstreamEvents,
		GatewayHeartbeats:   controller.gatewayHeartbeats,
		ModelOutputStarted:  controller.firstModelEventAt != nil,
	}
	if controller.timeoutFailure != nil {
		snapshot.TimedOut = true
		snapshot.TimeoutKind = controller.timeoutFailure.kind
		snapshot.RetryEligibleBeforeOutput = controller.timeoutFailure.retryEligibleBeforeOutput
		snapshot.PartialFailure = controller.timeoutFailure.partialFailure
	}
	return snapshot
}

// Failure returns an immutable copy of the controller-owned timeout, if any.
func (controller *TimeoutController) Failure() *TimeoutFailure {
	if controller == nil {
		return nil
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return cloneTimeoutFailure(controller.timeoutFailure)
}

// Close cancels the lifecycle and releases an attached upstream stream.
func (controller *TimeoutController) Close() error {
	if controller == nil {
		return nil
	}
	controller.terminate(ErrStreamClosed, nil)
	controller.closeAttached()
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return controller.closeErr
}

// GuardedStream applies controller state transitions around one upstream
// ChunkStream. Calls to Next are serialized just like the provider adapters.
type GuardedStream struct {
	controller *TimeoutController
	upstream   provideradapter.ChunkStream
	nextMu     sync.Mutex
}

// Next returns one upstream fact while preserving timeout causes over transport
// errors produced by closing a timed-out response Body.
func (stream *GuardedStream) Next(ctx context.Context) (adapter.NormalizedChunk, error) {
	if stream == nil || stream.controller == nil || isNilChunkStream(stream.upstream) || ctx == nil {
		return adapter.NormalizedChunk{}, ErrTimeoutState
	}
	stream.nextMu.Lock()
	defer stream.nextMu.Unlock()
	if terminalErr := stream.controller.currentTerminal(); terminalErr != nil {
		return adapter.NormalizedChunk{}, terminalErr
	}
	if err := contextCancellation(ctx); err != nil {
		stream.controller.terminate(err, nil)
		return adapter.NormalizedChunk{}, err
	}

	nextCtx, nextCancel := context.WithCancelCause(stream.controller.ctx)
	stopCaller := context.AfterFunc(ctx, func() {
		nextCancel(contextCancellation(ctx))
	})
	chunk, err := stream.upstream.Next(nextCtx)
	stopCaller()
	nextCancel(nil)

	if terminalErr := stream.controller.currentTerminal(); terminalErr != nil {
		return adapter.NormalizedChunk{}, terminalErr
	}
	if ctx.Err() != nil {
		cancellation := contextCancellation(ctx)
		stream.controller.terminate(cancellation, nil)
		return adapter.NormalizedChunk{}, cancellation
	}
	if err != nil {
		stream.controller.terminate(err, nil)
		return adapter.NormalizedChunk{}, err
	}
	if err := chunk.Validate(); err != nil {
		stream.controller.terminate(ErrTimeoutState, nil)
		return adapter.NormalizedChunk{}, ErrTimeoutState
	}
	if observeErr := stream.controller.observe(chunk); observeErr != nil {
		return adapter.NormalizedChunk{}, observeErr
	}
	return chunk, nil
}

// Close is idempotent and closes the controller lifecycle.
func (stream *GuardedStream) Close() error {
	if stream == nil || stream.controller == nil {
		return nil
	}
	return stream.controller.Close()
}

func (controller *TimeoutController) observe(chunk adapter.NormalizedChunk) error {
	now := time.Now().UTC()
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.terminalErr != nil {
		return controller.terminalErr
	}
	controller.upstreamEvents++
	controller.lastUpstreamEventAt = &now
	if controller.firstModelEventAt == nil && isModelOutput(chunk.Kind) {
		controller.firstModelEventAt = &now
		if controller.firstTokenTimer != nil {
			controller.firstTokenTimer.Stop()
			controller.firstTokenTimer = nil
		}
		controller.resetNoProgressTimerLocked()
		return nil
	}
	if controller.firstModelEventAt != nil {
		controller.resetNoProgressTimerLocked()
	}
	return nil
}

func (controller *TimeoutController) resetNoProgressTimerLocked() {
	if controller.noProgressTimer != nil {
		controller.noProgressTimer.Stop()
	}
	controller.noProgressGeneration++
	generation := controller.noProgressGeneration
	controller.noProgressTimer = time.AfterFunc(controller.options.NoProgressTimeout, func() {
		controller.timeout(TimeoutNoProgress, generation)
	})
}

func (controller *TimeoutController) timeout(kind TimeoutKind, generation uint64) {
	controller.mu.Lock()
	if controller.terminalErr != nil || (kind == TimeoutFirstToken && controller.firstModelEventAt != nil) ||
		(kind == TimeoutNoProgress && (controller.firstModelEventAt == nil || generation != controller.noProgressGeneration)) {
		controller.mu.Unlock()
		return
	}
	failure := &TimeoutFailure{
		kind: kind, headersReceived: controller.headersReceivedAt != nil,
		modelOutputStarted: controller.firstModelEventAt != nil,
	}
	failure.retryEligibleBeforeOutput = kind == TimeoutFirstToken && failure.headersReceived && !failure.modelOutputStarted
	failure.partialFailure = failure.modelOutputStarted
	controller.terminalErr = failure
	controller.timeoutFailure = cloneTimeoutFailure(failure)
	controller.stopTimersLocked()
	controller.mu.Unlock()
	controller.cancel(failure)
	controller.closeAttached()
}

func (controller *TimeoutController) terminate(cause error, timeoutFailure *TimeoutFailure) {
	if cause == nil {
		cause = ErrStreamClosed
	}
	controller.mu.Lock()
	if controller.terminalErr != nil {
		controller.mu.Unlock()
		return
	}
	controller.terminalErr = cause
	controller.timeoutFailure = cloneTimeoutFailure(timeoutFailure)
	controller.stopTimersLocked()
	controller.mu.Unlock()
	controller.cancel(cause)
	controller.closeAttached()
}

func (controller *TimeoutController) stopTimersLocked() {
	if controller.totalTimer != nil {
		controller.totalTimer.Stop()
		controller.totalTimer = nil
	}
	if controller.firstTokenTimer != nil {
		controller.firstTokenTimer.Stop()
		controller.firstTokenTimer = nil
	}
	if controller.noProgressTimer != nil {
		controller.noProgressTimer.Stop()
		controller.noProgressTimer = nil
	}
	controller.noProgressGeneration++
}

func (controller *TimeoutController) closeAttached() {
	controller.mu.Lock()
	upstream := controller.stream
	controller.mu.Unlock()
	if isNilChunkStream(upstream) {
		return
	}
	controller.closeOnce.Do(func() {
		err := upstream.Close()
		controller.mu.Lock()
		controller.closeErr = err
		controller.mu.Unlock()
	})
}

func (controller *TimeoutController) currentTerminal() error {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return controller.terminalErr
}

func validStreamTimeout(value time.Duration) bool {
	return value > 0 && value <= maximumStreamTimeout
}

func isModelOutput(kind adapter.ChunkKind) bool {
	return kind == adapter.ChunkContentDelta || kind == adapter.ChunkReasoningDelta || kind == adapter.ChunkToolDelta
}

func contextCancellation(ctx context.Context) error {
	if ctx == nil || ctx.Err() == nil {
		return nil
	}
	if cause := context.Cause(ctx); cause != nil {
		return errors.Join(ctx.Err(), cause)
	}
	return ctx.Err()
}

func isNilChunkStream(stream provideradapter.ChunkStream) bool {
	if stream == nil {
		return true
	}
	value := reflect.ValueOf(stream)
	return value.Kind() == reflect.Pointer && value.IsNil()
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneTimeoutFailure(failure *TimeoutFailure) *TimeoutFailure {
	if failure == nil {
		return nil
	}
	cloned := *failure
	return &cloned
}

var _ provideradapter.ChunkStream = (*GuardedStream)(nil)
