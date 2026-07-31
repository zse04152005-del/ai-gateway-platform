package routing

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPassiveHealthLowSamplesNeverTriggerDegradedState(t *testing.T) {
	t.Parallel()
	clock := newPassiveClock()
	health := mustPassiveHealth(t, passiveTestOptions(10, 0.5, 100), clock.Now)
	deploymentID := routeUUID(6, 301)
	for range 9 {
		observePassive(t, health, PassiveObservation{
			DeploymentID: deploymentID, Outcome: PassiveTimedOut, TotalLatency: time.Second,
		})
	}
	snapshot := snapshotPassive(t, health, deploymentID)
	if snapshot.State != PassiveStateWarmup || !snapshot.Healthy || snapshot.SampleSufficient ||
		snapshot.RequestCount != 9 || snapshot.TimeoutCount != 9 || snapshot.FailureRatio != 1 {
		t.Fatalf("low-sample snapshot = %+v", snapshot)
	}
	if healthy, err := health.Healthy(context.Background(), deploymentID); err != nil || !healthy {
		t.Fatalf("Healthy(low samples) = %v/%v", healthy, err)
	}

	observePassive(t, health, PassiveObservation{
		DeploymentID: deploymentID, Outcome: PassiveTimedOut, TotalLatency: time.Second,
	})
	snapshot = snapshotPassive(t, health, deploymentID)
	if snapshot.State != PassiveStateDegraded || snapshot.Healthy || !snapshot.SampleSufficient || snapshot.FailureRatio != 1 {
		t.Fatalf("threshold snapshot = %+v", snapshot)
	}
	for range 11 {
		observePassive(t, health, PassiveObservation{
			DeploymentID: deploymentID, Outcome: PassiveSucceeded, ProviderStatus: 200,
			TotalLatency: 100 * time.Millisecond,
		})
	}
	snapshot = snapshotPassive(t, health, deploymentID)
	if snapshot.State != PassiveStateHealthy || !snapshot.Healthy || snapshot.FailureSignalCount != 10 ||
		math.Abs(snapshot.FailureRatio-(10.0/21.0)) > 1e-12 {
		t.Fatalf("recovered ratio snapshot = %+v", snapshot)
	}
}

func TestPassiveHealthCountsEveryOutcomeAndLatencyQuantiles(t *testing.T) {
	t.Parallel()
	clock := newPassiveClock()
	health := mustPassiveHealth(t, passiveTestOptions(4, 0.5, 100), clock.Now)
	deploymentID := routeUUID(6, 302)
	firstTokens := []time.Duration{5 * time.Millisecond, 15 * time.Millisecond, 55 * time.Millisecond}
	observations := []PassiveObservation{
		{DeploymentID: deploymentID, Outcome: PassiveSucceeded, ProviderStatus: 200, FirstTokenLatency: &firstTokens[0], TotalLatency: 10 * time.Millisecond},
		{DeploymentID: deploymentID, Outcome: PassiveRateLimited, ProviderStatus: 429, FirstTokenLatency: &firstTokens[1], TotalLatency: 20 * time.Millisecond},
		{DeploymentID: deploymentID, Outcome: PassiveServerError, ProviderStatus: 503, FirstTokenLatency: &firstTokens[2], TotalLatency: 60 * time.Millisecond},
		{DeploymentID: deploymentID, Outcome: PassiveTimedOut, TotalLatency: 40 * time.Millisecond},
		{DeploymentID: deploymentID, Outcome: PassiveCancelled, TotalLatency: 50 * time.Millisecond},
		{DeploymentID: deploymentID, Outcome: PassiveOtherFailure, TotalLatency: 30 * time.Millisecond},
	}
	for _, observation := range observations {
		observePassive(t, health, observation)
	}
	snapshot := snapshotPassive(t, health, deploymentID)
	if snapshot.PolicyVersion != passiveHealthPolicyVersion || snapshot.State != PassiveStateDegraded || snapshot.Healthy ||
		snapshot.RequestCount != 6 || snapshot.SuccessCount != 1 || snapshot.RateLimitCount != 1 ||
		snapshot.ServerErrorCount != 1 || snapshot.TimeoutCount != 1 || snapshot.CancelledCount != 1 ||
		snapshot.OtherFailureCount != 1 || snapshot.HealthSampleCount != 4 || snapshot.FailureSignalCount != 3 || snapshot.FailureRatio != 0.75 {
		t.Fatalf("outcome snapshot = %+v", snapshot)
	}
	wantFirstToken := LatencyStatistics{
		Count: 3, Average: 25 * time.Millisecond, Maximum: 55 * time.Millisecond,
		P50UpperBound: 25 * time.Millisecond, P95UpperBound: 100 * time.Millisecond, P99UpperBound: 100 * time.Millisecond,
	}
	wantTotal := LatencyStatistics{
		Count: 6, Average: 35 * time.Millisecond, Maximum: 60 * time.Millisecond,
		P50UpperBound: 50 * time.Millisecond, P95UpperBound: 100 * time.Millisecond, P99UpperBound: 100 * time.Millisecond,
	}
	if snapshot.FirstTokenLatency != wantFirstToken || snapshot.TotalLatency != wantTotal {
		t.Fatalf("latencies = %+v/%+v, want %+v/%+v", snapshot.FirstTokenLatency, snapshot.TotalLatency, wantFirstToken, wantTotal)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, forbidden := range []string{"endpoint", "secret", "provider_error", "prompt", "response"} {
		if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
			t.Fatalf("snapshot contains %q: %s", forbidden, encoded)
		}
	}
}

func TestPassiveHealthCancelledAndOtherFailuresDoNotDiluteHealthSamples(t *testing.T) {
	t.Parallel()
	clock := newPassiveClock()
	health := mustPassiveHealth(t, passiveTestOptions(10, 0.5, 100), clock.Now)
	deploymentID := routeUUID(6, 304)
	for index := 0; index < 100; index++ {
		outcome := PassiveCancelled
		if index%2 == 0 {
			outcome = PassiveOtherFailure
		}
		observePassive(t, health, PassiveObservation{
			DeploymentID: deploymentID, Outcome: outcome, TotalLatency: time.Millisecond,
		})
	}
	snapshot := snapshotPassive(t, health, deploymentID)
	if snapshot.RequestCount != 100 || snapshot.HealthSampleCount != 0 || snapshot.SampleSufficient ||
		snapshot.State != PassiveStateWarmup || !snapshot.Healthy || snapshot.FailureRatio != 0 {
		t.Fatalf("non-health failures snapshot = %+v", snapshot)
	}
	for range 9 {
		observePassive(t, health, PassiveObservation{
			DeploymentID: deploymentID, Outcome: PassiveTimedOut, TotalLatency: time.Second,
		})
	}
	snapshot = snapshotPassive(t, health, deploymentID)
	if snapshot.HealthSampleCount != 9 || snapshot.State != PassiveStateWarmup || !snapshot.Healthy {
		t.Fatalf("low provider sample snapshot = %+v", snapshot)
	}
	observePassive(t, health, PassiveObservation{
		DeploymentID: deploymentID, Outcome: PassiveTimedOut, TotalLatency: time.Second,
	})
	snapshot = snapshotPassive(t, health, deploymentID)
	if snapshot.HealthSampleCount != 10 || snapshot.State != PassiveStateDegraded || snapshot.Healthy || snapshot.FailureRatio != 1 {
		t.Fatalf("sufficient provider sample snapshot = %+v", snapshot)
	}
}

func TestPassiveHealthSlidingWindowExpiresOldFailureSamples(t *testing.T) {
	t.Parallel()
	clock := newPassiveClock()
	options := PassiveHealthOptions{
		Window: 4 * time.Second, BucketWidth: time.Second,
		MinimumSamples: 4, FailureRatioThreshold: 0.5, MaximumDeployments: 10,
	}
	health := mustPassiveHealth(t, options, clock.Now)
	deploymentID := routeUUID(6, 303)
	for index := 0; index < 4; index++ {
		observePassive(t, health, PassiveObservation{
			DeploymentID: deploymentID, Outcome: PassiveServerError,
			ProviderStatus: 503, TotalLatency: time.Second,
		})
		if index < 3 {
			clock.Advance(time.Second)
		}
	}
	if snapshot := snapshotPassive(t, health, deploymentID); snapshot.State != PassiveStateDegraded || snapshot.RequestCount != 4 {
		t.Fatalf("initial window = %+v", snapshot)
	}

	clock.Advance(1100 * time.Millisecond)
	snapshot := snapshotPassive(t, health, deploymentID)
	if snapshot.State != PassiveStateWarmup || !snapshot.Healthy || snapshot.RequestCount != 3 || snapshot.ServerErrorCount != 3 {
		t.Fatalf("expired oldest bucket = %+v", snapshot)
	}
	for range 4 {
		observePassive(t, health, PassiveObservation{
			DeploymentID: deploymentID, Outcome: PassiveSucceeded,
			ProviderStatus: 200, TotalLatency: 100 * time.Millisecond,
		})
	}
	snapshot = snapshotPassive(t, health, deploymentID)
	if snapshot.State != PassiveStateHealthy || !snapshot.Healthy || snapshot.RequestCount != 7 || snapshot.FailureRatio >= 0.5 {
		t.Fatalf("healthy sufficient window = %+v", snapshot)
	}
	clock.Advance(4 * time.Second)
	snapshot = snapshotPassive(t, health, deploymentID)
	if snapshot.State != PassiveStateWarmup || snapshot.RequestCount != 0 || !snapshot.Healthy {
		t.Fatalf("fully expired window = %+v", snapshot)
	}
}

func TestPassiveHealthBoundsDeploymentMemoryWithDeterministicLRUEviction(t *testing.T) {
	t.Parallel()
	clock := newPassiveClock()
	health := mustPassiveHealth(t, passiveTestOptions(2, 0.5, 2), clock.Now)
	firstID := routeUUID(6, 311)
	secondID := routeUUID(6, 312)
	thirdID := routeUUID(6, 313)
	fourthID := routeUUID(6, 314)
	for _, deploymentID := range []string{firstID, secondID, thirdID} {
		observePassive(t, health, PassiveObservation{
			DeploymentID: deploymentID, Outcome: PassiveSucceeded,
			ProviderStatus: 200, TotalLatency: time.Millisecond,
		})
		clock.Advance(time.Second)
	}
	first := snapshotPassive(t, health, firstID)
	second := snapshotPassive(t, health, secondID)
	if first.RequestCount != 0 || first.EvictedDeployments != 1 || second.RequestCount != 1 {
		t.Fatalf("first eviction snapshots = %+v/%+v", first, second)
	}
	observePassive(t, health, PassiveObservation{
		DeploymentID: secondID, Outcome: PassiveSucceeded,
		ProviderStatus: 200, TotalLatency: time.Millisecond,
	})
	clock.Advance(time.Second)
	observePassive(t, health, PassiveObservation{
		DeploymentID: fourthID, Outcome: PassiveSucceeded,
		ProviderStatus: 200, TotalLatency: time.Millisecond,
	})
	third := snapshotPassive(t, health, thirdID)
	if third.RequestCount != 0 || third.EvictedDeployments != 2 || len(health.deployments) != 2 {
		t.Fatalf("second eviction snapshot/state = %+v/%d", third, len(health.deployments))
	}
}

func TestPassiveHealthConcurrentObserveSnapshotAndHealthy(t *testing.T) {
	t.Parallel()
	clock := newPassiveClock()
	health := mustPassiveHealth(t, passiveTestOptions(100, 0.5, 100), clock.Now)
	deploymentID := routeUUID(6, 321)
	const goroutines = 64
	const observationsPerGoroutine = 200
	var wait sync.WaitGroup
	errorChannel := make(chan error, goroutines)
	for worker := 0; worker < goroutines; worker++ {
		worker := worker
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := 0; index < observationsPerGoroutine; index++ {
				outcome := PassiveSucceeded
				status := 200
				if (worker+index)%4 == 0 {
					outcome = PassiveRateLimited
					status = 429
				}
				if err := health.Observe(context.Background(), PassiveObservation{
					DeploymentID: deploymentID, Outcome: outcome,
					ProviderStatus: status, TotalLatency: 10 * time.Millisecond,
				}); err != nil {
					errorChannel <- err
					return
				}
				if index%25 == 0 {
					if _, err := health.Snapshot(context.Background(), deploymentID); err != nil {
						errorChannel <- err
						return
					}
					if _, err := health.Healthy(context.Background(), deploymentID); err != nil {
						errorChannel <- err
						return
					}
				}
			}
		}()
	}
	wait.Wait()
	close(errorChannel)
	for err := range errorChannel {
		t.Fatal(err)
	}
	snapshot := snapshotPassive(t, health, deploymentID)
	wantRequests := uint64(goroutines * observationsPerGoroutine)
	if snapshot.RequestCount != wantRequests || snapshot.RateLimitCount != wantRequests/4 ||
		snapshot.SuccessCount != wantRequests-wantRequests/4 || snapshot.State != PassiveStateHealthy {
		t.Fatalf("concurrent snapshot = %+v", snapshot)
	}
}

func TestPassiveHealthOptionsObservationAndContextBoundaries(t *testing.T) {
	t.Parallel()
	validOptions := passiveTestOptions(2, 0.5, 10)
	if err := DefaultPassiveHealthOptions().Validate(); err != nil {
		t.Fatalf("DefaultPassiveHealthOptions().Validate() error = %v", err)
	}
	invalidOptions := []PassiveHealthOptions{
		{},
		{Window: 500 * time.Millisecond, BucketWidth: 100 * time.Millisecond, MinimumSamples: 2, FailureRatioThreshold: 0.5, MaximumDeployments: 1},
		{Window: time.Second, BucketWidth: 300 * time.Millisecond, MinimumSamples: 2, FailureRatioThreshold: 0.5, MaximumDeployments: 1},
		{Window: 100 * time.Second, BucketWidth: 100 * time.Millisecond, MinimumSamples: 2, FailureRatioThreshold: 0.5, MaximumDeployments: 1},
		{Window: 2 * time.Second, BucketWidth: time.Second, MinimumSamples: 1, FailureRatioThreshold: 0.5, MaximumDeployments: 1},
		{Window: 2 * time.Second, BucketWidth: time.Second, MinimumSamples: 2, FailureRatioThreshold: math.NaN(), MaximumDeployments: 1},
		{Window: 2 * time.Second, BucketWidth: time.Second, MinimumSamples: 2, FailureRatioThreshold: math.Inf(1), MaximumDeployments: 1},
		{Window: 2 * time.Second, BucketWidth: time.Second, MinimumSamples: 2, FailureRatioThreshold: 0, MaximumDeployments: 1},
		{Window: 2 * time.Second, BucketWidth: time.Second, MinimumSamples: 2, FailureRatioThreshold: 1.1, MaximumDeployments: 1},
		{Window: 2 * time.Second, BucketWidth: time.Second, MinimumSamples: 2, FailureRatioThreshold: 0.5, MaximumDeployments: 0},
	}
	for _, options := range invalidOptions {
		if err := options.Validate(); !errors.Is(err, ErrPassiveHealthInvalid) {
			t.Fatalf("Validate(%+v) error = %v", options, err)
		}
		if _, err := NewPassiveHealth(options, time.Now); !errors.Is(err, ErrPassiveHealthInvalid) {
			t.Fatalf("NewPassiveHealth(%+v) error = %v", options, err)
		}
	}
	if _, err := NewPassiveHealth(validOptions, nil); !errors.Is(err, ErrPassiveHealthInvalid) {
		t.Fatalf("NewPassiveHealth(nil clock) error = %v", err)
	}

	deploymentID := routeUUID(6, 331)
	firstToken := time.Millisecond
	validObservation := PassiveObservation{
		DeploymentID: deploymentID, Outcome: PassiveSucceeded, ProviderStatus: 200,
		FirstTokenLatency: &firstToken, TotalLatency: 2 * time.Millisecond,
	}
	invalidObservations := []PassiveObservation{
		{},
		{DeploymentID: deploymentID, Outcome: "unknown", TotalLatency: time.Millisecond},
		{DeploymentID: deploymentID, Outcome: PassiveSucceeded, ProviderStatus: 500, TotalLatency: time.Millisecond},
		{DeploymentID: deploymentID, Outcome: PassiveRateLimited, ProviderStatus: 503, TotalLatency: time.Millisecond},
		{DeploymentID: deploymentID, Outcome: PassiveServerError, ProviderStatus: 429, TotalLatency: time.Millisecond},
		{DeploymentID: deploymentID, Outcome: PassiveTimedOut, ProviderStatus: 700, TotalLatency: time.Millisecond},
		{DeploymentID: deploymentID, Outcome: PassiveOtherFailure, TotalLatency: -1},
		{DeploymentID: deploymentID, Outcome: PassiveOtherFailure, TotalLatency: maximumPassiveLatency + 1},
		{DeploymentID: deploymentID, Outcome: PassiveSucceeded, ProviderStatus: 200, FirstTokenLatency: durationPointer(-1), TotalLatency: time.Millisecond},
		{DeploymentID: deploymentID, Outcome: PassiveSucceeded, ProviderStatus: 200, FirstTokenLatency: durationPointer(2 * time.Millisecond), TotalLatency: time.Millisecond},
	}
	for _, observation := range invalidObservations {
		if err := observation.Validate(); !errors.Is(err, ErrPassiveHealthInvalid) {
			t.Fatalf("Validate(%+v) error = %v", observation, err)
		}
	}
	if err := validObservation.Validate(); err != nil {
		t.Fatalf("valid observation error = %v", err)
	}

	clock := newPassiveClock()
	health := mustPassiveHealth(t, validOptions, clock.Now)
	if err := health.Observe(nil, validObservation); !errors.Is(err, ErrPassiveHealthInvalid) { //nolint:staticcheck // explicit nil boundary
		t.Fatalf("Observe(nil) error = %v", err)
	}
	if _, err := health.Snapshot(nil, deploymentID); !errors.Is(err, ErrPassiveHealthInvalid) { //nolint:staticcheck // explicit nil boundary
		t.Fatalf("Snapshot(nil) error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := health.Observe(cancelled, validObservation); !errors.Is(err, context.Canceled) {
		t.Fatalf("Observe(cancelled) error = %v", err)
	}
	if _, err := health.Snapshot(cancelled, deploymentID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Snapshot(cancelled) error = %v", err)
	}
	if _, err := health.Snapshot(context.Background(), "bad"); !errors.Is(err, ErrPassiveHealthInvalid) {
		t.Fatalf("Snapshot(bad id) error = %v", err)
	}
	unknown := snapshotPassive(t, health, deploymentID)
	if unknown.State != PassiveStateWarmup || !unknown.Healthy || unknown.RequestCount != 0 {
		t.Fatalf("unknown snapshot = %+v", unknown)
	}
	var nilHealth *PassiveHealth
	if err := nilHealth.Observe(context.Background(), validObservation); !errors.Is(err, ErrPassiveHealthInvalid) {
		t.Fatalf("nil Observe() error = %v", err)
	}
	if _, err := nilHealth.Snapshot(context.Background(), deploymentID); !errors.Is(err, ErrPassiveHealthInvalid) {
		t.Fatalf("nil Snapshot() error = %v", err)
	}
	zeroClockHealth := mustPassiveHealth(t, validOptions, func() time.Time { return time.Time{} })
	if err := zeroClockHealth.Observe(context.Background(), validObservation); !errors.Is(err, ErrPassiveHealthInvalid) {
		t.Fatalf("zero clock Observe() error = %v", err)
	}
	if _, err := zeroClockHealth.Snapshot(context.Background(), deploymentID); !errors.Is(err, ErrPassiveHealthInvalid) {
		t.Fatalf("zero clock Snapshot() error = %v", err)
	}
}

func TestPassiveLatencyTailAndSaturatingArithmetic(t *testing.T) {
	t.Parallel()
	var aggregate latencyAggregate
	aggregate.add(2 * time.Minute)
	statistics := aggregate.statistics()
	if statistics.P50UpperBound != 2*time.Minute || statistics.P95UpperBound != 2*time.Minute || statistics.P99UpperBound != 2*time.Minute {
		t.Fatalf("tail statistics = %+v", statistics)
	}
	if saturatingIncrement(math.MaxUint64) != math.MaxUint64 || saturatingSum(math.MaxUint64-1, 2) != math.MaxUint64 {
		t.Fatal("saturating arithmetic wrapped")
	}
	left := latencyAggregate{count: math.MaxUint64, totalNanos: math.MaxUint64, maximum: time.Second}
	right := latencyAggregate{count: 1, totalNanos: 1, maximum: 2 * time.Second}
	left.merge(right)
	if left.count != math.MaxUint64 || left.totalNanos != math.MaxUint64 || left.maximum != 2*time.Second {
		t.Fatalf("saturated merge = %+v", left)
	}
}

type passiveClock struct {
	mutex sync.Mutex
	now   time.Time
}

func newPassiveClock() *passiveClock {
	return &passiveClock{now: time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)}
}

func (clock *passiveClock) Now() time.Time {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	return clock.now
}

func (clock *passiveClock) Advance(duration time.Duration) {
	clock.mutex.Lock()
	clock.now = clock.now.Add(duration)
	clock.mutex.Unlock()
}

func passiveTestOptions(minimumSamples uint64, threshold float64, maximumDeployments int) PassiveHealthOptions {
	return PassiveHealthOptions{
		Window: 10 * time.Second, BucketWidth: time.Second,
		MinimumSamples: minimumSamples, FailureRatioThreshold: threshold,
		MaximumDeployments: maximumDeployments,
	}
}

func mustPassiveHealth(t *testing.T, options PassiveHealthOptions, now func() time.Time) *PassiveHealth {
	t.Helper()
	health, err := NewPassiveHealth(options, now)
	if err != nil {
		t.Fatalf("NewPassiveHealth() error = %v", err)
	}
	return health
}

func observePassive(t *testing.T, health *PassiveHealth, observation PassiveObservation) {
	t.Helper()
	if err := health.Observe(context.Background(), observation); err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
}

func snapshotPassive(t *testing.T, health *PassiveHealth, deploymentID string) PassiveSnapshot {
	t.Helper()
	snapshot, err := health.Snapshot(context.Background(), deploymentID)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	return snapshot
}

func durationPointer(value time.Duration) *time.Duration {
	return &value
}
