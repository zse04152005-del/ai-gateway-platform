package streaming

import (
	"context"
	"errors"
	"reflect"
	"sync"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
)

const maximumFailoverAttempts = 32

var (
	// ErrFailoverInvalid means configuration, identity, or a callback is unsafe.
	ErrFailoverInvalid = errors.New("stream failover input is invalid")
	// ErrFailoverState means an Attempt token is stale or lifecycle calls are out of order.
	ErrFailoverState = errors.New("stream failover lifecycle state is invalid")
	// ErrFailoverAfterOutput means client-visible model output made the response irreversible.
	ErrFailoverAfterOutput = errors.New("stream failover forbidden after model output")
	// ErrFailoverNotEligible means the preceding failure is not a trusted pre-output timeout.
	ErrFailoverNotEligible = errors.New("stream failure is not eligible for failover")
	// ErrFailoverLimit means the request-scoped physical Attempt allowance is exhausted.
	ErrFailoverLimit = errors.New("stream failover attempt limit reached")
	// ErrFailoverClosed means the response has reached a terminal state.
	ErrFailoverClosed = errors.New("stream failover gate is closed")
)

// FailoverOptions bounds the physical Attempts that may contribute to one
// client response. P08 composes this hard count with total-time and budget
// policy; this component owns the irreversible output boundary.
type FailoverOptions struct {
	MaxAttempts int
}

// AttemptStart contains only safe routing identity and monotonic numbering.
type AttemptStart struct {
	Number       int
	DeploymentID string
	Failover     bool
}

// AttemptStarter starts exactly one physical upstream Attempt. The gate calls
// it only after atomically reserving the Attempt and invalidating its predecessor.
type AttemptStarter func(context.Context, AttemptStart) error

// ChunkSink serially projects one normalized fact to the existing client response.
type ChunkSink func(context.Context, adapter.NormalizedChunk) error

// AttemptToken is an opaque capability for the currently active Attempt.
// Zero values and tokens from another gate are rejected.
type AttemptToken struct {
	gateID     uint64
	generation uint64
	number     int
}

// Number returns the physical Attempt number without exposing gate internals.
func (token AttemptToken) Number() int {
	return token.number
}

// FailoverSnapshot contains no provider error, model content, or deployment ID.
type FailoverSnapshot struct {
	AttemptsStarted           int
	CurrentAttempt            int
	ModelOutputStarted        bool
	FirstModelOutputAttempt   int
	ModelChunksForwarded      uint64
	NonModelChunksForwarded   uint64
	PostOutputFailoverDenials uint64
	Closed                    bool
}

// FailoverGate serializes Attempt replacement with downstream model-output
// commitment. Marking output precedes the sink call deliberately: an uncertain
// partial socket write is treated conservatively and can never trigger a backup.
type FailoverGate struct {
	options FailoverOptions
	gateID  uint64

	mu                        sync.Mutex
	generation                uint64
	attemptsStarted           int
	currentAttempt            int
	modelOutputStarted        bool
	firstModelOutputAttempt   int
	modelChunksForwarded      uint64
	nonModelChunksForwarded   uint64
	postOutputFailoverDenials uint64
	closed                    bool
	forwardMu                 sync.Mutex
}

var failoverGateSequence struct {
	sync.Mutex
	value uint64
}

// NewFailoverGate creates one request-scoped failover boundary.
func NewFailoverGate(options FailoverOptions) (*FailoverGate, error) {
	if options.MaxAttempts < 1 || options.MaxAttempts > maximumFailoverAttempts {
		return nil, ErrFailoverInvalid
	}
	failoverGateSequence.Lock()
	failoverGateSequence.value++
	gateID := failoverGateSequence.value
	failoverGateSequence.Unlock()
	return &FailoverGate{options: options, gateID: gateID}, nil
}

// StartInitial reserves and starts the first physical Attempt.
func (gate *FailoverGate) StartInitial(
	ctx context.Context,
	deploymentID string,
	starter AttemptStarter,
) (AttemptToken, error) {
	if gate == nil {
		return AttemptToken{}, ErrFailoverInvalid
	}
	if err := validateFailoverStart(ctx, deploymentID, starter); err != nil {
		return AttemptToken{}, err
	}
	gate.mu.Lock()
	if gate.closed {
		gate.mu.Unlock()
		return AttemptToken{}, ErrFailoverClosed
	}
	if gate.attemptsStarted != 0 {
		gate.mu.Unlock()
		return AttemptToken{}, ErrFailoverState
	}
	token := gate.reserveAttemptLocked()
	gate.mu.Unlock()

	err := starter(ctx, AttemptStart{Number: token.number, DeploymentID: deploymentID})
	return token, err
}

// StartFailover atomically decides whether a trusted first-token timeout may
// start a replacement Attempt. Model output recorded by Forward always wins,
// even if a caller supplies a stale or otherwise contradictory failure.
func (gate *FailoverGate) StartFailover(
	ctx context.Context,
	previous AttemptToken,
	failure *TimeoutFailure,
	deploymentID string,
	starter AttemptStarter,
) (AttemptToken, error) {
	if err := validateFailoverStart(ctx, deploymentID, starter); err != nil {
		return AttemptToken{}, err
	}
	if gate == nil {
		return AttemptToken{}, ErrFailoverInvalid
	}
	gate.mu.Lock()
	if gate.closed {
		gate.mu.Unlock()
		return AttemptToken{}, ErrFailoverClosed
	}
	if !gate.currentTokenLocked(previous) {
		gate.mu.Unlock()
		return AttemptToken{}, ErrFailoverState
	}
	if gate.modelOutputStarted {
		gate.postOutputFailoverDenials++
		gate.mu.Unlock()
		return AttemptToken{}, ErrFailoverAfterOutput
	}
	if failure == nil || failure.ModelOutputStarted() || !failure.RetryEligibleBeforeOutput() {
		gate.mu.Unlock()
		return AttemptToken{}, ErrFailoverNotEligible
	}
	if gate.attemptsStarted >= gate.options.MaxAttempts {
		gate.mu.Unlock()
		return AttemptToken{}, ErrFailoverLimit
	}
	token := gate.reserveAttemptLocked()
	gate.mu.Unlock()

	err := starter(ctx, AttemptStart{Number: token.number, DeploymentID: deploymentID, Failover: true})
	return token, err
}

// Forward validates current Attempt ownership, irreversibly records the first
// model-output boundary, and only then invokes the downstream sink. Calls are
// serialized so different Attempts can never interleave client events.
func (gate *FailoverGate) Forward(
	ctx context.Context,
	token AttemptToken,
	chunk adapter.NormalizedChunk,
	sink ChunkSink,
) error {
	if gate == nil || ctx == nil || chunk.Validate() != nil || isNilChunkSink(sink) {
		return ErrFailoverInvalid
	}
	if err := contextCancellation(ctx); err != nil {
		return err
	}
	gate.forwardMu.Lock()
	defer gate.forwardMu.Unlock()

	modelOutput := isModelOutput(chunk.Kind)
	gate.mu.Lock()
	if gate.closed {
		gate.mu.Unlock()
		return ErrFailoverClosed
	}
	if !gate.currentTokenLocked(token) {
		gate.mu.Unlock()
		return ErrFailoverState
	}
	if modelOutput && !gate.modelOutputStarted {
		gate.modelOutputStarted = true
		gate.firstModelOutputAttempt = token.number
	}
	gate.mu.Unlock()

	if err := sink(ctx, chunk.Clone()); err != nil {
		return err
	}
	gate.mu.Lock()
	if modelOutput {
		gate.modelChunksForwarded++
	} else {
		gate.nonModelChunksForwarded++
	}
	gate.mu.Unlock()
	return nil
}

// Close makes the current response terminal. It is idempotent for the current
// token and rejects stale ownership.
func (gate *FailoverGate) Close(token AttemptToken) error {
	if gate == nil {
		return ErrFailoverInvalid
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if !gate.currentTokenLocked(token) {
		return ErrFailoverState
	}
	if gate.closed {
		return nil
	}
	gate.closed = true
	return nil
}

// Snapshot returns request-scoped, content-free state evidence.
func (gate *FailoverGate) Snapshot() FailoverSnapshot {
	if gate == nil {
		return FailoverSnapshot{}
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return FailoverSnapshot{
		AttemptsStarted: gate.attemptsStarted, CurrentAttempt: gate.currentAttempt,
		ModelOutputStarted: gate.modelOutputStarted, FirstModelOutputAttempt: gate.firstModelOutputAttempt,
		ModelChunksForwarded: gate.modelChunksForwarded, NonModelChunksForwarded: gate.nonModelChunksForwarded,
		PostOutputFailoverDenials: gate.postOutputFailoverDenials, Closed: gate.closed,
	}
}

func (gate *FailoverGate) reserveAttemptLocked() AttemptToken {
	gate.attemptsStarted++
	gate.currentAttempt = gate.attemptsStarted
	gate.generation++
	return AttemptToken{gateID: gate.gateID, generation: gate.generation, number: gate.currentAttempt}
}

func (gate *FailoverGate) currentTokenLocked(token AttemptToken) bool {
	return token.gateID != 0 && token.gateID == gate.gateID && token.generation == gate.generation &&
		token.number == gate.currentAttempt
}

func validateFailoverStart(ctx context.Context, deploymentID string, starter AttemptStarter) error {
	if ctx == nil || deploymentID == "" || len(deploymentID) > 128 || isNilAttemptStarter(starter) {
		return ErrFailoverInvalid
	}
	if err := contextCancellation(ctx); err != nil {
		return err
	}
	return nil
}

func isNilAttemptStarter(starter AttemptStarter) bool {
	return starter == nil
}

func isNilChunkSink(sink ChunkSink) bool {
	if sink == nil {
		return true
	}
	value := reflect.ValueOf(sink)
	return value.Kind() == reflect.Func && value.IsNil()
}
