package streaming

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
)

func TestTimeoutControllerFirstTokenIgnoresHeadersAndHeartbeats(t *testing.T) {
	controller := newTestTimeoutController(t, TimeoutOptions{
		FirstTokenTimeout: 60 * time.Millisecond,
		NoProgressTimeout: 200 * time.Millisecond,
		TotalTimeout:      time.Second,
	})
	upstream := newTimeoutTestStream()
	stream := attachTestStream(t, controller, upstream)
	if err := controller.RecordGatewayHeartbeat(); err != nil {
		t.Fatalf("RecordGatewayHeartbeat() error = %v", err)
	}
	upstream.emit(timeoutTestResult{chunk: timeoutChunk(adapter.ChunkMessageStart)})
	upstream.emit(timeoutTestResult{chunk: timeoutChunk(adapter.ChunkHeartbeat)})
	for index := range 2 {
		if _, err := stream.Next(context.Background()); err != nil {
			t.Fatalf("pre-model Next(%d) error = %v", index, err)
		}
	}

	started := time.Now()
	_, err := stream.Next(context.Background())
	if !errors.Is(err, ErrFirstTokenTimeout) || !errors.Is(err, ErrStreamingTimeout) ||
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first-token Next() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("first-token cancellation took %s", elapsed)
	}
	failure := controller.Failure()
	if failure == nil || failure.Kind() != TimeoutFirstToken || !failure.HeadersReceived() ||
		failure.ModelOutputStarted() || !failure.RetryEligibleBeforeOutput() || failure.PartialFailure() {
		t.Fatalf("first-token failure = %+v", failure)
	}
	snapshot := controller.Snapshot()
	if snapshot.HeadersReceivedAt == nil || snapshot.FirstModelEventAt != nil || snapshot.UpstreamEvents != 2 ||
		snapshot.GatewayHeartbeats != 1 || !snapshot.TimedOut || snapshot.TimeoutKind != TimeoutFirstToken {
		t.Fatalf("first-token snapshot = %+v", snapshot)
	}
	assertTimeoutReleased(t, controller, upstream, ErrFirstTokenTimeout)
}

func TestTimeoutControllerNoProgressStartsAfterModelOutputAndResets(t *testing.T) {
	controller := newTestTimeoutController(t, TimeoutOptions{
		FirstTokenTimeout: 200 * time.Millisecond,
		NoProgressTimeout: 70 * time.Millisecond,
		TotalTimeout:      time.Second,
	})
	upstream := newTimeoutTestStream()
	stream := attachTestStream(t, controller, upstream)
	upstream.emit(timeoutTestResult{chunk: timeoutChunk(adapter.ChunkContentDelta)})
	if _, err := stream.Next(context.Background()); err != nil {
		t.Fatalf("model Next() error = %v", err)
	}
	time.Sleep(35 * time.Millisecond)
	upstream.emit(timeoutTestResult{chunk: timeoutChunk(adapter.ChunkHeartbeat)})
	if _, err := stream.Next(context.Background()); err != nil {
		t.Fatalf("provider heartbeat Next() error = %v", err)
	}
	time.Sleep(40 * time.Millisecond)
	select {
	case <-controller.Context().Done():
		t.Fatalf("no-progress timer was not reset: %v", context.Cause(controller.Context()))
	default:
	}
	if err := controller.RecordGatewayHeartbeat(); err != nil {
		t.Fatalf("RecordGatewayHeartbeat() error = %v", err)
	}

	_, err := stream.Next(context.Background())
	if !errors.Is(err, ErrNoProgressTimeout) {
		t.Fatalf("no-progress Next() error = %v", err)
	}
	failure := controller.Failure()
	if failure == nil || failure.Kind() != TimeoutNoProgress || !failure.ModelOutputStarted() ||
		failure.RetryEligibleBeforeOutput() || !failure.PartialFailure() {
		t.Fatalf("no-progress failure = %+v", failure)
	}
	snapshot := controller.Snapshot()
	if snapshot.FirstModelEventAt == nil || snapshot.LastUpstreamEventAt == nil || snapshot.UpstreamEvents != 2 ||
		snapshot.GatewayHeartbeats != 1 || !snapshot.PartialFailure {
		t.Fatalf("no-progress snapshot = %+v", snapshot)
	}
	assertTimeoutReleased(t, controller, upstream, ErrNoProgressTimeout)
}

func TestTimeoutControllerTotalDeadlineBeforeAndAfterModelOutput(t *testing.T) {
	t.Run("before headers", func(t *testing.T) {
		controller := newTestTimeoutController(t, TimeoutOptions{
			FirstTokenTimeout: time.Second, NoProgressTimeout: time.Second, TotalTimeout: 35 * time.Millisecond,
		})
		select {
		case <-controller.Context().Done():
		case <-time.After(250 * time.Millisecond):
			t.Fatal("total timeout did not cancel pre-header Context")
		}
		upstream := newTimeoutTestStream()
		stream, err := controller.Attach(upstream)
		if stream != nil || !errors.Is(err, ErrTotalStreamTimeout) {
			t.Fatalf("Attach after total timeout = %#v/%v", stream, err)
		}
		failure := controller.Failure()
		if failure == nil || failure.HeadersReceived() || failure.ModelOutputStarted() ||
			failure.RetryEligibleBeforeOutput() || failure.PartialFailure() {
			t.Fatalf("pre-header total failure = %+v", failure)
		}
		if upstream.closes.Load() != 1 {
			t.Fatalf("late attached stream closes = %d", upstream.closes.Load())
		}
	})

	t.Run("after model output", func(t *testing.T) {
		controller := newTestTimeoutController(t, TimeoutOptions{
			FirstTokenTimeout: time.Second, NoProgressTimeout: time.Second, TotalTimeout: 65 * time.Millisecond,
		})
		upstream := newTimeoutTestStream()
		stream := attachTestStream(t, controller, upstream)
		upstream.emit(timeoutTestResult{chunk: timeoutChunk(adapter.ChunkToolDelta)})
		if _, err := stream.Next(context.Background()); err != nil {
			t.Fatalf("tool Next() error = %v", err)
		}
		_, err := stream.Next(context.Background())
		if !errors.Is(err, ErrTotalStreamTimeout) || errors.Is(err, ErrNoProgressTimeout) {
			t.Fatalf("post-output total Next() error = %v", err)
		}
		failure := controller.Failure()
		if failure == nil || failure.Kind() != TimeoutTotal || !failure.ModelOutputStarted() ||
			failure.RetryEligibleBeforeOutput() || !failure.PartialFailure() {
			t.Fatalf("post-output total failure = %+v", failure)
		}
		assertTimeoutReleased(t, controller, upstream, ErrTotalStreamTimeout)
	})
}

func TestTimeoutControllerCallerCancellationAndTerminalErrorsReleaseStream(t *testing.T) {
	controller := newTestTimeoutController(t, testTimeoutOptions())
	upstream := newTimeoutTestStream()
	stream := attachTestStream(t, controller, upstream)
	callCtx, cancel := context.WithCancelCause(context.Background())
	privateCause := errors.New("test caller cancelled")
	cancel(privateCause)
	if _, err := stream.Next(callCtx); !errors.Is(err, context.Canceled) || !errors.Is(err, privateCause) {
		t.Fatalf("cancelled Next() error = %v", err)
	}
	if controller.Failure() != nil || upstream.closes.Load() != 1 {
		t.Fatalf("caller cancellation failure/closes = %+v/%d", controller.Failure(), upstream.closes.Load())
	}

	terminalController := newTestTimeoutController(t, testTimeoutOptions())
	terminalUpstream := newTimeoutTestStream()
	terminalStream := attachTestStream(t, terminalController, terminalUpstream)
	upstreamFailure := errors.New("safe scripted upstream failure")
	terminalUpstream.emit(timeoutTestResult{err: upstreamFailure})
	if _, err := terminalStream.Next(context.Background()); !errors.Is(err, upstreamFailure) {
		t.Fatalf("upstream failure Next() error = %v", err)
	}
	if _, err := terminalStream.Next(context.Background()); !errors.Is(err, upstreamFailure) {
		t.Fatalf("repeated terminal Next() error = %v", err)
	}
	if terminalUpstream.closes.Load() != 1 {
		t.Fatalf("terminal upstream closes = %d", terminalUpstream.closes.Load())
	}
}

func TestTimeoutControllerValidationCloseAndFailureContract(t *testing.T) {
	valid := testTimeoutOptions()
	tests := []struct {
		name    string
		parent  context.Context
		options TimeoutOptions
	}{
		{name: "nil parent", options: valid},
		{name: "first zero", parent: context.Background(), options: TimeoutOptions{NoProgressTimeout: time.Second, TotalTimeout: time.Second}},
		{name: "no progress zero", parent: context.Background(), options: TimeoutOptions{FirstTokenTimeout: time.Second, TotalTimeout: time.Second}},
		{name: "total zero", parent: context.Background(), options: TimeoutOptions{FirstTokenTimeout: time.Second, NoProgressTimeout: time.Second}},
		{name: "first too large", parent: context.Background(), options: TimeoutOptions{FirstTokenTimeout: maximumStreamTimeout + 1, NoProgressTimeout: time.Second, TotalTimeout: time.Second}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controller, err := NewTimeoutController(test.parent, test.options)
			if controller != nil || !errors.Is(err, ErrTimeoutConfiguration) {
				t.Fatalf("NewTimeoutController() = %#v/%v", controller, err)
			}
		})
	}

	controller := newTestTimeoutController(t, valid)
	if controller.Context() == nil || (*TimeoutController)(nil).Context() != nil ||
		(*TimeoutController)(nil).Snapshot() != (TimeoutSnapshot{}) || (*TimeoutController)(nil).Failure() != nil {
		t.Fatal("nil controller accessor contract failed")
	}
	if err := controller.RecordGatewayHeartbeat(); !errors.Is(err, ErrTimeoutState) {
		t.Fatalf("pre-header RecordGatewayHeartbeat() error = %v", err)
	}
	upstream := newTimeoutTestStream()
	stream := attachTestStream(t, controller, upstream)
	duplicate := newTimeoutTestStream()
	if got, err := controller.Attach(duplicate); got != nil || !errors.Is(err, ErrTimeoutState) || duplicate.closes.Load() != 1 {
		t.Fatalf("duplicate Attach() = %#v/%v, closes=%d", got, err, duplicate.closes.Load())
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := stream.Close(); err != nil || upstream.closes.Load() != 1 {
		t.Fatalf("second Close() error/closes = %v/%d", err, upstream.closes.Load())
	}
	if _, err := stream.Next(context.Background()); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("Next after Close() error = %v", err)
	}
	if err := controller.RecordGatewayHeartbeat(); !errors.Is(err, ErrTimeoutState) {
		t.Fatalf("terminal RecordGatewayHeartbeat() error = %v", err)
	}
	if (*TimeoutController)(nil).Close() != nil || (*GuardedStream)(nil).Close() != nil {
		t.Fatal("nil Close contract failed")
	}

	failure := &TimeoutFailure{kind: TimeoutFirstToken}
	if failure.Error() != ErrFirstTokenTimeout.Error() || !errors.Is(failure, ErrStreamingTimeout) ||
		!errors.Is(failure, ErrFirstTokenTimeout) || !errors.Is(failure, context.DeadlineExceeded) {
		t.Fatalf("TimeoutFailure contract = %q/%v", failure.Error(), failure.Unwrap())
	}
	if (*TimeoutFailure)(nil).Error() != "<nil>" || (*TimeoutFailure)(nil).Unwrap() != nil {
		t.Fatal("nil TimeoutFailure contract failed")
	}
}

type timeoutTestResult struct {
	chunk adapter.NormalizedChunk
	err   error
}

type timeoutTestStream struct {
	results   chan timeoutTestResult
	closed    chan struct{}
	closeOnce sync.Once
	closes    atomic.Int32
}

func newTimeoutTestStream() *timeoutTestStream {
	return &timeoutTestStream{results: make(chan timeoutTestResult, 8), closed: make(chan struct{})}
}

func (stream *timeoutTestStream) Next(ctx context.Context) (adapter.NormalizedChunk, error) {
	select {
	case result := <-stream.results:
		return result.chunk, result.err
	case <-stream.closed:
		return adapter.NormalizedChunk{}, io.ErrClosedPipe
	case <-ctx.Done():
		return adapter.NormalizedChunk{}, ctx.Err()
	}
}

func (stream *timeoutTestStream) Close() error {
	stream.closeOnce.Do(func() {
		stream.closes.Add(1)
		close(stream.closed)
	})
	return nil
}

func (stream *timeoutTestStream) emit(result timeoutTestResult) {
	stream.results <- result
}

func newTestTimeoutController(t *testing.T, options TimeoutOptions) *TimeoutController {
	t.Helper()
	controller, err := NewTimeoutController(context.Background(), options)
	if err != nil {
		t.Fatalf("NewTimeoutController() error = %v", err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	return controller
}

func attachTestStream(t *testing.T, controller *TimeoutController, upstream *timeoutTestStream) *GuardedStream {
	t.Helper()
	stream, err := controller.Attach(upstream)
	if err != nil {
		t.Fatalf("Attach() error = %v", err)
	}
	return stream
}

func testTimeoutOptions() TimeoutOptions {
	return TimeoutOptions{FirstTokenTimeout: time.Second, NoProgressTimeout: time.Second, TotalTimeout: 2 * time.Second}
}

func timeoutChunk(kind adapter.ChunkKind) adapter.NormalizedChunk {
	chunk := adapter.NormalizedChunk{
		Sequence: 1, Kind: kind, ChoiceIndex: 0, ProviderEventType: "test", ObservedAt: time.Now().UTC(),
	}
	switch kind {
	case adapter.ChunkMessageStart:
		chunk.Role = adapter.RoleAssistant
	case adapter.ChunkContentDelta:
		chunk.ContentDelta = "visible"
	case adapter.ChunkReasoningDelta:
		chunk.ReasoningDelta = "reasoning"
	case adapter.ChunkToolDelta:
		chunk.ToolDelta = &adapter.ToolCallDelta{Index: 0, ID: "call_test", Name: "lookup", ArgumentsFragment: "{"}
	case adapter.ChunkHeartbeat:
		// Provider event type is the only heartbeat payload.
	case adapter.ChunkUsageDelta, adapter.ChunkMessageEnd, adapter.ChunkProviderExtension:
		panic("timeoutChunk does not construct terminal, usage, or extension fixtures")
	}
	return chunk
}

func assertTimeoutReleased(t *testing.T, controller *TimeoutController, upstream *timeoutTestStream, target error) {
	t.Helper()
	if !errors.Is(context.Cause(controller.Context()), target) {
		t.Fatalf("controller Context cause = %v", context.Cause(controller.Context()))
	}
	if upstream.closes.Load() != 1 {
		t.Fatalf("upstream closes = %d", upstream.closes.Load())
	}
}
