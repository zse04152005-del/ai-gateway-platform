package streaming

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
)

func TestFailoverGateNeverStartsBackupAfterModelOutput(t *testing.T) {
	gate := newTestFailoverGate(t, 3)
	var starts []AttemptStart
	starter := func(_ context.Context, start AttemptStart) error {
		starts = append(starts, start)
		return nil
	}
	primary, err := gate.StartInitial(context.Background(), "deployment-primary", starter)
	if err != nil {
		t.Fatalf("StartInitial() error = %v", err)
	}
	var forwarded []adapter.ChunkKind
	sink := func(_ context.Context, chunk adapter.NormalizedChunk) error {
		forwarded = append(forwarded, chunk.Kind)
		return nil
	}
	for _, kind := range []adapter.ChunkKind{adapter.ChunkMessageStart, adapter.ChunkHeartbeat} {
		if err := gate.Forward(context.Background(), primary, failoverChunk(kind), sink); err != nil {
			t.Fatalf("Forward(%s) error = %v", kind, err)
		}
	}
	backup, err := gate.StartFailover(
		context.Background(), primary, eligibleFirstTokenFailure(), "deployment-backup-a", starter,
	)
	if err != nil {
		t.Fatalf("StartFailover(before output) error = %v", err)
	}
	if backup.Number() != 2 || len(starts) != 2 || !starts[1].Failover {
		t.Fatalf("backup token/starts = %+v/%+v", backup, starts)
	}
	if err := gate.Forward(context.Background(), primary, failoverChunk(adapter.ChunkContentDelta), sink); !errors.Is(err, ErrFailoverState) {
		t.Fatalf("stale primary Forward() error = %v, want ErrFailoverState", err)
	}
	if err := gate.Forward(context.Background(), backup, failoverChunk(adapter.ChunkContentDelta), sink); err != nil {
		t.Fatalf("backup model Forward() error = %v", err)
	}

	startCount := len(starts)
	if _, err := gate.StartFailover(
		context.Background(), backup, eligibleFirstTokenFailure(), "deployment-backup-b", starter,
	); !errors.Is(err, ErrFailoverAfterOutput) {
		t.Fatalf("StartFailover(after output) error = %v, want ErrFailoverAfterOutput", err)
	}
	if len(starts) != startCount {
		t.Fatalf("post-output starter calls = %d, want %d", len(starts), startCount)
	}
	if len(forwarded) != 3 || forwarded[2] != adapter.ChunkContentDelta {
		t.Fatalf("forwarded kinds = %v", forwarded)
	}
	snapshot := gate.Snapshot()
	if snapshot.AttemptsStarted != 2 || snapshot.CurrentAttempt != 2 || !snapshot.ModelOutputStarted ||
		snapshot.FirstModelOutputAttempt != 2 || snapshot.ModelChunksForwarded != 1 ||
		snapshot.NonModelChunksForwarded != 2 || snapshot.PostOutputFailoverDenials != 1 {
		t.Fatalf("Snapshot() = %+v", snapshot)
	}
}

func TestFailoverGateMarksOutputBeforeSinkAndFailsClosed(t *testing.T) {
	gate := newTestFailoverGate(t, 2)
	var starts atomic.Int64
	starter := func(context.Context, AttemptStart) error {
		starts.Add(1)
		return nil
	}
	token, err := gate.StartInitial(context.Background(), "deployment-primary", starter)
	if err != nil {
		t.Fatalf("StartInitial() error = %v", err)
	}
	writeFailure := errors.New("client socket write failed")
	err = gate.Forward(context.Background(), token, failoverChunk(adapter.ChunkToolDelta), func(context.Context, adapter.NormalizedChunk) error {
		return writeFailure
	})
	if !errors.Is(err, writeFailure) {
		t.Fatalf("Forward(write failure) error = %v", err)
	}
	if _, err := gate.StartFailover(
		context.Background(), token, eligibleFirstTokenFailure(), "deployment-backup", starter,
	); !errors.Is(err, ErrFailoverAfterOutput) {
		t.Fatalf("failover after uncertain write error = %v", err)
	}
	if starts.Load() != 1 {
		t.Fatalf("starter calls = %d, want 1", starts.Load())
	}
	snapshot := gate.Snapshot()
	if !snapshot.ModelOutputStarted || snapshot.ModelChunksForwarded != 0 || snapshot.PostOutputFailoverDenials != 1 {
		t.Fatalf("Snapshot() = %+v", snapshot)
	}
}

func TestFailoverGateSerializesOutputAndReplacementRace(t *testing.T) {
	for iteration := range 500 {
		gate := newTestFailoverGate(t, 2)
		var starts atomic.Int64
		starter := func(context.Context, AttemptStart) error {
			starts.Add(1)
			return nil
		}
		primary, err := gate.StartInitial(context.Background(), "deployment-primary", starter)
		if err != nil {
			t.Fatalf("iteration %d StartInitial() error = %v", iteration, err)
		}
		var writes atomic.Int64
		ready := make(chan struct{})
		var wait sync.WaitGroup
		wait.Add(2)
		var forwardErr, failoverErr error
		go func() {
			defer wait.Done()
			<-ready
			forwardErr = gate.Forward(
				context.Background(), primary, failoverChunk(adapter.ChunkReasoningDelta),
				func(context.Context, adapter.NormalizedChunk) error {
					writes.Add(1)
					return nil
				},
			)
		}()
		go func() {
			defer wait.Done()
			<-ready
			_, failoverErr = gate.StartFailover(
				context.Background(), primary, eligibleFirstTokenFailure(), "deployment-backup", starter,
			)
		}()
		close(ready)
		wait.Wait()

		switch {
		case forwardErr == nil:
			if !errors.Is(failoverErr, ErrFailoverAfterOutput) || writes.Load() != 1 || starts.Load() != 1 {
				t.Fatalf("iteration %d output-wins result = forward:%v failover:%v writes:%d starts:%d",
					iteration, forwardErr, failoverErr, writes.Load(), starts.Load())
			}
		case errors.Is(forwardErr, ErrFailoverState):
			if failoverErr != nil || writes.Load() != 0 || starts.Load() != 2 {
				t.Fatalf("iteration %d failover-wins result = forward:%v failover:%v writes:%d starts:%d",
					iteration, forwardErr, failoverErr, writes.Load(), starts.Load())
			}
		default:
			t.Fatalf("iteration %d unexpected forward/failover errors = %v/%v", iteration, forwardErr, failoverErr)
		}
	}
}

func TestFailoverGateRejectsUntrustedFailureLimitsAndInvalidLifecycle(t *testing.T) {
	var nilGate *FailoverGate
	starter := func(context.Context, AttemptStart) error { return nil }
	if _, err := nilGate.StartInitial(context.Background(), "deployment-primary", starter); !errors.Is(err, ErrFailoverInvalid) {
		t.Fatalf("nil StartInitial() error = %v", err)
	}
	if _, err := NewFailoverGate(FailoverOptions{}); !errors.Is(err, ErrFailoverInvalid) {
		t.Fatalf("NewFailoverGate(zero) error = %v", err)
	}
	if _, err := NewFailoverGate(FailoverOptions{MaxAttempts: maximumFailoverAttempts + 1}); !errors.Is(err, ErrFailoverInvalid) {
		t.Fatalf("NewFailoverGate(overflow) error = %v", err)
	}
	gate := newTestFailoverGate(t, 1)
	token, err := gate.StartInitial(context.Background(), "deployment-primary", starter)
	if err != nil {
		t.Fatalf("StartInitial() error = %v", err)
	}
	if _, err := gate.StartInitial(context.Background(), "deployment-duplicate", starter); !errors.Is(err, ErrFailoverState) {
		t.Fatalf("duplicate StartInitial() error = %v", err)
	}
	if _, err := gate.StartFailover(context.Background(), token, nil, "deployment-backup", starter); !errors.Is(err, ErrFailoverNotEligible) {
		t.Fatalf("nil failure error = %v", err)
	}
	notEligible := &TimeoutFailure{kind: TimeoutTotal, headersReceived: true}
	if _, err := gate.StartFailover(
		context.Background(), token, notEligible, "deployment-backup", starter,
	); !errors.Is(err, ErrFailoverNotEligible) {
		t.Fatalf("total timeout error = %v", err)
	}
	if _, err := gate.StartFailover(
		context.Background(), token, eligibleFirstTokenFailure(), "deployment-backup", starter,
	); !errors.Is(err, ErrFailoverLimit) {
		t.Fatalf("attempt limit error = %v", err)
	}
	other := newTestFailoverGate(t, 2)
	if err := gate.Forward(
		context.Background(), AttemptToken{gateID: other.gateID, generation: 1, number: 1},
		failoverChunk(adapter.ChunkMessageStart), func(context.Context, adapter.NormalizedChunk) error { return nil },
	); !errors.Is(err, ErrFailoverState) {
		t.Fatalf("foreign token Forward() error = %v", err)
	}
	if err := gate.Close(token); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := gate.Close(token); err != nil {
		t.Fatalf("idempotent Close() error = %v", err)
	}
	if err := gate.Close(AttemptToken{gateID: gate.gateID, generation: token.generation + 1, number: 2}); !errors.Is(err, ErrFailoverState) {
		t.Fatalf("closed stale Close() error = %v", err)
	}
	if _, err := gate.StartFailover(
		context.Background(), token, eligibleFirstTokenFailure(), "deployment-backup", starter,
	); !errors.Is(err, ErrFailoverClosed) {
		t.Fatalf("closed StartFailover() error = %v", err)
	}
	if !gate.Snapshot().Closed {
		t.Fatal("closed snapshot = false")
	}
}

func newTestFailoverGate(t *testing.T, maxAttempts int) *FailoverGate {
	t.Helper()
	gate, err := NewFailoverGate(FailoverOptions{MaxAttempts: maxAttempts})
	if err != nil {
		t.Fatalf("NewFailoverGate() error = %v", err)
	}
	return gate
}

func eligibleFirstTokenFailure() *TimeoutFailure {
	return &TimeoutFailure{
		kind: TimeoutFirstToken, headersReceived: true, retryEligibleBeforeOutput: true,
	}
}

func failoverChunk(kind adapter.ChunkKind) adapter.NormalizedChunk {
	chunk := timeoutChunk(kind)
	chunk.Sequence = 1
	return chunk
}
