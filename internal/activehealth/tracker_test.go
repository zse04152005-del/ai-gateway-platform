package activehealth

import (
	"context"
	"testing"
	"time"
)

func TestTrackerUsesFailureAndRecoveryHysteresis(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 31, 5, 0, 0, 0, time.UTC)
	options := DefaultOptions()
	tracker, err := NewTracker(options, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewTracker() error = %v", err)
	}
	deploymentID := activeUUID(1)
	snapshot, err := tracker.Snapshot(context.Background(), deploymentID)
	if err != nil || snapshot.State != StateUnknown || !snapshot.Healthy {
		t.Fatalf("initial Snapshot() = %+v, %v", snapshot, err)
	}
	if healthy, readErr := tracker.Healthy(context.Background(), deploymentID); readErr != nil || !healthy {
		t.Fatalf("initial Healthy() = %v, %v", healthy, readErr)
	}
	for attempt := uint32(1); attempt <= options.FailureThreshold; attempt++ {
		now = now.Add(time.Second)
		if err := tracker.Observe(context.Background(), Observation{
			DeploymentID: deploymentID, Code: ResultTransportFailure, Latency: 25 * time.Millisecond,
		}); err != nil {
			t.Fatalf("Observe(failure %d) error = %v", attempt, err)
		}
		snapshot, err = tracker.Snapshot(context.Background(), deploymentID)
		if err != nil {
			t.Fatalf("Snapshot(failure %d) error = %v", attempt, err)
		}
		wantHealthy := attempt < options.FailureThreshold
		if snapshot.Healthy != wantHealthy {
			t.Fatalf("failure %d healthy = %v, want %v", attempt, snapshot.Healthy, wantHealthy)
		}
	}
	if snapshot.State != StateUnhealthy || snapshot.TotalFailures != uint64(options.FailureThreshold) {
		t.Fatalf("unhealthy Snapshot() = %+v", snapshot)
	}
	for attempt := uint32(1); attempt <= options.RecoveryThreshold; attempt++ {
		now = now.Add(time.Second)
		if err := tracker.Observe(context.Background(), Observation{
			DeploymentID: deploymentID, Code: ResultSucceeded, Latency: 20 * time.Millisecond,
		}); err != nil {
			t.Fatalf("Observe(success %d) error = %v", attempt, err)
		}
		snapshot, _ = tracker.Snapshot(context.Background(), deploymentID)
		wantHealthy := attempt >= options.RecoveryThreshold
		if snapshot.Healthy != wantHealthy {
			t.Fatalf("recovery %d healthy = %v, want %v", attempt, snapshot.Healthy, wantHealthy)
		}
	}
	if snapshot.State != StateHealthy || snapshot.TotalSuccesses != uint64(options.RecoveryThreshold) {
		t.Fatalf("recovered Snapshot() = %+v", snapshot)
	}
}

func TestTrackerFailsOpenForStaleStateAndEvictsDeterministically(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 31, 5, 0, 0, 0, time.UTC)
	options := DefaultOptions()
	options.FailureThreshold = 2
	options.MaximumTargets = 1
	tracker, err := NewTracker(options, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewTracker() error = %v", err)
	}
	firstID := activeUUID(1)
	for attempt := 0; attempt < 2; attempt++ {
		if err := tracker.Observe(context.Background(), Observation{DeploymentID: firstID, Code: ResultTimedOut}); err != nil {
			t.Fatalf("Observe() error = %v", err)
		}
	}
	now = now.Add(options.StateTTL + time.Second)
	snapshot, err := tracker.Snapshot(context.Background(), firstID)
	if err != nil || !snapshot.Healthy || !snapshot.Stale || snapshot.State != StateStale {
		t.Fatalf("stale Snapshot() = %+v, %v", snapshot, err)
	}
	secondID := activeUUID(2)
	if err := tracker.Observe(context.Background(), Observation{DeploymentID: secondID, Code: ResultSucceeded}); err != nil {
		t.Fatalf("Observe(second) error = %v", err)
	}
	first, _ := tracker.Snapshot(context.Background(), firstID)
	second, _ := tracker.Snapshot(context.Background(), secondID)
	if first.State != StateUnknown || second.State != StateHealthy || second.EvictedTargets != 1 {
		t.Fatalf("eviction snapshots = first:%+v second:%+v", first, second)
	}
}

func TestActiveHealthValidationRejectsUnsafeInputs(t *testing.T) {
	t.Parallel()
	tests := []func(*Options){
		func(options *Options) { options.ProbeInterval = 0 },
		func(options *Options) { options.RefreshInterval = 0 },
		func(options *Options) { options.DispatchInterval = 0 },
		func(options *Options) { options.ProbeTimeout = 0 },
		func(options *Options) { options.StateTTL = options.ProbeInterval },
		func(options *Options) { options.FailureThreshold = 1 },
		func(options *Options) { options.MaximumTargets = 0 },
		func(options *Options) { options.MaximumConcurrency = 0 },
		func(options *Options) { options.MaximumBatchSize = 1 },
		func(options *Options) { options.JitterPercent = 51 },
	}
	for index, mutate := range tests {
		options := DefaultOptions()
		mutate(&options)
		if err := options.Validate(); err == nil {
			t.Errorf("Options.Validate(case %d) error = nil", index)
		}
	}
	if _, err := NewTracker(DefaultOptions(), nil); err == nil {
		t.Fatal("NewTracker(nil clock) error = nil")
	}
	tracker, err := NewTracker(DefaultOptions(), time.Now)
	if err != nil {
		t.Fatalf("NewTracker() error = %v", err)
	}
	if err := tracker.Observe(context.Background(), Observation{DeploymentID: "unsafe", Code: ResultSucceeded}); err == nil {
		t.Fatal("Observe(invalid ID) error = nil")
	}
	if err := tracker.Observe(context.Background(), Observation{DeploymentID: activeUUID(1), Code: "unsafe"}); err == nil {
		t.Fatal("Observe(invalid code) error = nil")
	}
	if err := tracker.Observe(context.Background(), Observation{
		DeploymentID: activeUUID(1), Code: ResultSucceeded, Latency: 2 * time.Minute,
	}); err == nil {
		t.Fatal("Observe(invalid latency) error = nil")
	}
	if _, err := tracker.Snapshot(context.Background(), "unsafe"); err == nil {
		t.Fatal("Snapshot(invalid ID) error = nil")
	}
}

func activeUUID(value int) string {
	return "90000000-0000-4000-8000-" + leftPad12(value)
}

func leftPad12(value int) string {
	const digits = "000000000000"
	raw := []byte(digits)
	for index := len(raw) - 1; value > 0 && index >= 0; index-- {
		raw[index] = byte('0' + value%10)
		value /= 10
	}
	return string(raw)
}
