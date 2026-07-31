package routing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCircuitBreakerClosedOpenHalfOpenAndRecovered(t *testing.T) {
	t.Parallel()
	clock := newCircuitClock()
	options := circuitTestOptions()
	breaker := mustCircuitBreaker(t, options, clock.Now)
	deploymentID := routeUUID(7, 401)

	snapshot := snapshotCircuit(t, breaker, deploymentID)
	if snapshot.Tracked || snapshot.State != CircuitClosed || !snapshot.Healthy {
		t.Fatalf("initial snapshot = %+v", snapshot)
	}
	for attempt := uint32(1); attempt <= options.FailureThreshold; attempt++ {
		completeCircuit(t, acquireCircuit(t, breaker, deploymentID), CircuitFailed)
		snapshot = snapshotCircuit(t, breaker, deploymentID)
		wantState := CircuitClosed
		if attempt == options.FailureThreshold {
			wantState = CircuitOpen
		}
		if snapshot.State != wantState || snapshot.TotalFailures != uint64(attempt) {
			t.Fatalf("failure %d snapshot = %+v", attempt, snapshot)
		}
	}
	if healthy, err := breaker.Healthy(context.Background(), deploymentID); err != nil || healthy {
		t.Fatalf("Healthy(open) = %v/%v", healthy, err)
	}
	if _, err := breaker.Acquire(context.Background(), deploymentID); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Acquire(open) error = %v", err)
	}

	clock.Advance(options.OpenDuration)
	if healthy, err := breaker.Healthy(context.Background(), deploymentID); err != nil || !healthy {
		t.Fatalf("Healthy(half-open) = %v/%v", healthy, err)
	}
	first := acquireCircuit(t, breaker, deploymentID)
	second := acquireCircuit(t, breaker, deploymentID)
	if _, err := breaker.Acquire(context.Background(), deploymentID); !errors.Is(err, ErrHalfOpenSaturated) {
		t.Fatalf("Acquire(saturated) error = %v", err)
	}
	completeCircuit(t, first, CircuitSucceeded)
	snapshot = snapshotCircuit(t, breaker, deploymentID)
	if snapshot.State != CircuitHalfOpen || snapshot.HalfOpenSuccesses != 1 || snapshot.InFlight != 1 {
		t.Fatalf("first recovery snapshot = %+v", snapshot)
	}
	completeCircuit(t, second, CircuitSucceeded)
	snapshot = snapshotCircuit(t, breaker, deploymentID)
	if snapshot.State != CircuitClosed || !snapshot.Healthy || snapshot.InFlight != 0 ||
		snapshot.ConsecutiveFailures != 0 || snapshot.TotalSuccesses != 2 || snapshot.RejectedHalfOpen != 1 {
		t.Fatalf("recovered snapshot = %+v", snapshot)
	}
	if err := second.Complete(context.Background(), CircuitSucceeded); !errors.Is(err, ErrCircuitPermitCompleted) {
		t.Fatalf("duplicate Complete() error = %v", err)
	}
}

func TestCircuitBreakerHalfOpenFailureReopensAndIgnoresLateGeneration(t *testing.T) {
	t.Parallel()
	clock := newCircuitClock()
	options := circuitTestOptions()
	options.FailureThreshold = 2
	breaker := mustCircuitBreaker(t, options, clock.Now)
	deploymentID := routeUUID(7, 402)
	for range options.FailureThreshold {
		completeCircuit(t, acquireCircuit(t, breaker, deploymentID), CircuitFailed)
	}
	clock.Advance(options.OpenDuration)
	first := acquireCircuit(t, breaker, deploymentID)
	late := acquireCircuit(t, breaker, deploymentID)
	completeCircuit(t, first, CircuitFailed)
	if err := late.Complete(context.Background(), CircuitSucceeded); err != nil {
		t.Fatalf("late Complete() error = %v", err)
	}
	snapshot := snapshotCircuit(t, breaker, deploymentID)
	if snapshot.State != CircuitOpen || snapshot.InFlight != 0 || snapshot.TotalFailures != 3 || snapshot.TotalSuccesses != 0 {
		t.Fatalf("reopened snapshot = %+v", snapshot)
	}
	clock.Advance(options.OpenDuration - time.Nanosecond)
	if healthy, _ := breaker.Healthy(context.Background(), deploymentID); healthy {
		t.Fatal("reopened circuit recovered before the new cooldown")
	}
	clock.Advance(time.Nanosecond)
	if healthy, _ := breaker.Healthy(context.Background(), deploymentID); !healthy {
		t.Fatal("reopened circuit did not enter half-open at the new deadline")
	}
}

func TestCircuitBreakerIgnoredOutcomeDoesNotChangeFailureEvidence(t *testing.T) {
	t.Parallel()
	clock := newCircuitClock()
	options := circuitTestOptions()
	options.FailureThreshold = 2
	breaker := mustCircuitBreaker(t, options, clock.Now)
	deploymentID := routeUUID(7, 403)
	completeCircuit(t, acquireCircuit(t, breaker, deploymentID), CircuitFailed)
	completeCircuit(t, acquireCircuit(t, breaker, deploymentID), CircuitIgnored)
	snapshot := snapshotCircuit(t, breaker, deploymentID)
	if snapshot.State != CircuitClosed || snapshot.ConsecutiveFailures != 1 || snapshot.TotalIgnored != 1 {
		t.Fatalf("ignored snapshot = %+v", snapshot)
	}
	completeCircuit(t, acquireCircuit(t, breaker, deploymentID), CircuitFailed)
	if snapshot = snapshotCircuit(t, breaker, deploymentID); snapshot.State != CircuitOpen {
		t.Fatalf("post-ignored threshold snapshot = %+v", snapshot)
	}
	clock.Advance(options.OpenDuration)
	completeCircuit(t, acquireCircuit(t, breaker, deploymentID), CircuitIgnored)
	if snapshot = snapshotCircuit(t, breaker, deploymentID); snapshot.State != CircuitHalfOpen || snapshot.InFlight != 0 || snapshot.TotalIgnored != 2 {
		t.Fatalf("half-open ignored snapshot = %+v", snapshot)
	}
}

func TestCircuitBreakerHalfOpenConcurrencyIsStrictlyBounded(t *testing.T) {
	t.Parallel()
	clock := newCircuitClock()
	options := circuitTestOptions()
	options.FailureThreshold = 2
	options.HalfOpenMaximumProbes = 3
	options.HalfOpenSuccessThreshold = 2
	breaker := mustCircuitBreaker(t, options, clock.Now)
	deploymentID := routeUUID(7, 404)
	for range options.FailureThreshold {
		completeCircuit(t, acquireCircuit(t, breaker, deploymentID), CircuitFailed)
	}
	clock.Advance(options.OpenDuration)

	var permitsMutex sync.Mutex
	permits := make([]*CircuitPermit, 0, options.HalfOpenMaximumProbes)
	var saturated atomic.Uint64
	var workers sync.WaitGroup
	for range 64 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			permit, err := breaker.Acquire(context.Background(), deploymentID)
			if errors.Is(err, ErrHalfOpenSaturated) {
				saturated.Add(1)
				return
			}
			if err != nil {
				t.Errorf("Acquire() error = %v", err)
				return
			}
			permitsMutex.Lock()
			permits = append(permits, permit)
			permitsMutex.Unlock()
		}()
	}
	workers.Wait()
	if len(permits) != int(options.HalfOpenMaximumProbes) || saturated.Load() != 61 {
		t.Fatalf("permits/saturated = %d/%d", len(permits), saturated.Load())
	}
	for _, permit := range permits {
		completeCircuit(t, permit, CircuitSucceeded)
	}
	if snapshot := snapshotCircuit(t, breaker, deploymentID); snapshot.State != CircuitClosed || snapshot.InFlight != 0 {
		t.Fatalf("concurrent recovery snapshot = %+v", snapshot)
	}
}

func TestCircuitBreakerCapacityEvictsOnlyIdleClosedState(t *testing.T) {
	t.Parallel()
	clock := newCircuitClock()
	options := circuitTestOptions()
	options.MaximumDeployments = 1
	options.FailureThreshold = 2
	breaker := mustCircuitBreaker(t, options, clock.Now)
	firstID := routeUUID(7, 405)
	secondID := routeUUID(7, 406)
	for range options.FailureThreshold {
		completeCircuit(t, acquireCircuit(t, breaker, firstID), CircuitFailed)
	}
	if _, err := breaker.Acquire(context.Background(), secondID); !errors.Is(err, ErrCircuitCapacity) {
		t.Fatalf("Acquire(capacity protected) error = %v", err)
	}
	first := snapshotCircuit(t, breaker, firstID)
	if first.State != CircuitOpen || first.RejectedCapacity != 1 {
		t.Fatalf("protected snapshot = %+v", first)
	}

	clock.Advance(options.OpenDuration)
	permit := acquireCircuit(t, breaker, firstID)
	completeCircuit(t, permit, CircuitSucceeded)
	permit = acquireCircuit(t, breaker, firstID)
	completeCircuit(t, permit, CircuitSucceeded)
	second := acquireCircuit(t, breaker, secondID)
	completeCircuit(t, second, CircuitIgnored)
	first = snapshotCircuit(t, breaker, firstID)
	if first.Tracked || first.EvictedClosedCircuits != 1 {
		t.Fatalf("evicted first snapshot = %+v", first)
	}
}

func TestCircuitBreakerValidationAndSafeSnapshot(t *testing.T) {
	t.Parallel()
	mutations := []func(*CircuitOptions){
		func(options *CircuitOptions) { options.FailureThreshold = 1 },
		func(options *CircuitOptions) { options.OpenDuration = 0 },
		func(options *CircuitOptions) { options.HalfOpenMaximumProbes = 0 },
		func(options *CircuitOptions) { options.HalfOpenSuccessThreshold = 0 },
		func(options *CircuitOptions) { options.MaximumDeployments = 0 },
	}
	for index, mutate := range mutations {
		options := DefaultCircuitOptions()
		mutate(&options)
		if _, err := NewCircuitBreaker(options, time.Now); !errors.Is(err, ErrCircuitInvalid) {
			t.Errorf("NewCircuitBreaker(case %d) error = %v", index, err)
		}
	}
	if _, err := NewCircuitBreaker(DefaultCircuitOptions(), nil); !errors.Is(err, ErrCircuitInvalid) {
		t.Fatalf("NewCircuitBreaker(nil clock) error = %v", err)
	}
	breaker := mustCircuitBreaker(t, circuitTestOptions(), time.Now)
	if _, err := breaker.Healthy(context.Background(), "unsafe"); !errors.Is(err, ErrCircuitInvalid) {
		t.Fatalf("Healthy(invalid ID) error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := breaker.Acquire(cancelled, routeUUID(7, 407)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire(cancelled) error = %v", err)
	}
	permit := acquireCircuit(t, breaker, routeUUID(7, 407))
	if err := permit.Complete(context.Background(), "unsafe"); !errors.Is(err, ErrCircuitInvalid) {
		t.Fatalf("Complete(invalid outcome) error = %v", err)
	}
	completeCircuit(t, permit, CircuitIgnored)

	raw, err := json.Marshal(snapshotCircuit(t, breaker, routeUUID(7, 407)))
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	for _, forbidden := range []string{"private-provider-error", "https://", "secret_reference"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("snapshot leaked %q: %s", forbidden, raw)
		}
	}
	if _, err := (*CircuitBreaker)(nil).Snapshot(context.Background(), routeUUID(7, 407)); !errors.Is(err, ErrCircuitInvalid) {
		t.Fatalf("nil breaker Snapshot() error = %v", err)
	}
}

type circuitClock struct {
	mutex sync.Mutex
	now   time.Time
}

func newCircuitClock() *circuitClock {
	return &circuitClock{now: time.Date(2026, 7, 31, 6, 0, 0, 0, time.UTC)}
}

func (clock *circuitClock) Now() time.Time {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	return clock.now
}

func (clock *circuitClock) Advance(duration time.Duration) {
	clock.mutex.Lock()
	clock.now = clock.now.Add(duration)
	clock.mutex.Unlock()
}

func circuitTestOptions() CircuitOptions {
	return CircuitOptions{
		FailureThreshold: 3, OpenDuration: 10 * time.Second,
		HalfOpenMaximumProbes: 2, HalfOpenSuccessThreshold: 2,
		MaximumDeployments: 100,
	}
}

func mustCircuitBreaker(t *testing.T, options CircuitOptions, now func() time.Time) *CircuitBreaker {
	t.Helper()
	breaker, err := NewCircuitBreaker(options, now)
	if err != nil {
		t.Fatalf("NewCircuitBreaker() error = %v", err)
	}
	return breaker
}

func acquireCircuit(t *testing.T, breaker *CircuitBreaker, deploymentID string) *CircuitPermit {
	t.Helper()
	permit, err := breaker.Acquire(context.Background(), deploymentID)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	return permit
}

func completeCircuit(t *testing.T, permit *CircuitPermit, outcome CircuitOutcome) {
	t.Helper()
	if err := permit.Complete(context.Background(), outcome); err != nil {
		t.Fatalf("Complete(%s) error = %v", outcome, err)
	}
}

func snapshotCircuit(t *testing.T, breaker *CircuitBreaker, deploymentID string) CircuitSnapshot {
	t.Helper()
	snapshot, err := breaker.Snapshot(context.Background(), deploymentID)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	return snapshot
}
