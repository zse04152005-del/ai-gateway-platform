package activehealth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
)

func TestSchedulerSpreadsTargetsAndBoundsBatchConcurrency(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 31, 5, 0, 0, 0, time.UTC)
	options := DefaultOptions()
	options.ProbeInterval = 30 * time.Second
	options.RefreshInterval = time.Second
	options.DispatchInterval = 100 * time.Millisecond
	options.ProbeTimeout = time.Second
	options.StateTTL = time.Minute
	options.MaximumConcurrency = 2
	options.MaximumBatchSize = 2
	source := &stubTargetSource{targets: []catalog.HealthProbeTarget{
		activeTarget(1, "http://127.0.0.1:18090"), activeTarget(2, "http://127.0.0.1:18091"),
		activeTarget(3, "http://127.0.0.1:18092"),
	}}
	prober := &countingProber{delay: 10 * time.Millisecond}
	observer := &recordingObserver{}
	scheduler, err := NewScheduler(options, source, prober, observer, &stubProbeGate{needs: true}, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewScheduler() error = %v", err)
	}
	if err := scheduler.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if err := scheduler.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh(unchanged) error = %v", err)
	}
	if err := scheduler.Dispatch(context.Background()); err != nil {
		t.Fatalf("early Dispatch() error = %v", err)
	}
	if prober.calls.Load() != 0 {
		t.Fatalf("startup probes = %d, want 0 before spread elapses", prober.calls.Load())
	}
	now = now.Add(options.ProbeInterval + time.Second)
	if err := scheduler.Dispatch(context.Background()); err != nil {
		t.Fatalf("Dispatch(first batch) error = %v", err)
	}
	if prober.calls.Load() != 2 || prober.maximum.Load() > int64(options.MaximumConcurrency) {
		t.Fatalf("first batch calls/maximum = %d/%d", prober.calls.Load(), prober.maximum.Load())
	}
	if err := scheduler.Dispatch(context.Background()); err != nil {
		t.Fatalf("Dispatch(second batch) error = %v", err)
	}
	if prober.calls.Load() != 3 || observer.count() != 3 {
		t.Fatalf("final calls/observations = %d/%d", prober.calls.Load(), observer.count())
	}
	stats := scheduler.Stats()
	if stats.TargetCount != 3 || stats.Refreshes != 2 || stats.Probes != 3 || stats.ProbeSuccesses != 3 {
		t.Fatalf("Stats() = %+v", stats)
	}
}

func TestSchedulerRefreshIsAtomicAndRunStopsCleanly(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 31, 5, 0, 0, 0, time.UTC)
	options := DefaultOptions()
	source := &stubTargetSource{targets: []catalog.HealthProbeTarget{activeTarget(1, "http://127.0.0.1:18090")}}
	scheduler, err := NewScheduler(options, source, &countingProber{}, &recordingObserver{}, &stubProbeGate{needs: true}, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewScheduler() error = %v", err)
	}
	if err := scheduler.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	duplicate := activeTarget(1, "http://127.0.0.1:18090")
	source.targets = []catalog.HealthProbeTarget{duplicate, duplicate}
	if err := scheduler.Refresh(context.Background()); !errors.Is(err, ErrDuplicateTarget) {
		t.Fatalf("duplicate Refresh() error = %v", err)
	}
	if stats := scheduler.Stats(); stats.TargetCount != 1 || stats.RefreshFailures != 1 || stats.Refreshes != 1 {
		t.Fatalf("Stats() after rejected refresh = %+v", stats)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := scheduler.Run(cancelled); err != nil {
		t.Fatalf("Run(cancelled) error = %v", err)
	}
}

func TestSchedulerRunOwnsOneLoopAndStopsOnCancellation(t *testing.T) {
	t.Parallel()
	now := time.Now
	options := DefaultOptions()
	options.RefreshInterval = time.Second
	options.DispatchInterval = 100 * time.Millisecond
	source := &stubTargetSource{called: make(chan struct{}, 1)}
	scheduler, err := NewScheduler(options, source, &countingProber{}, &recordingObserver{}, &stubProbeGate{needs: true}, now)
	if err != nil {
		t.Fatalf("NewScheduler() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()
	select {
	case <-source.called:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not refresh on startup")
	}
	if err := scheduler.Run(ctx); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Run() error = %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop after cancellation")
	}
}

func TestSchedulerSuppressesHotTargetsAndSkipsOnGateFailure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 31, 5, 0, 0, 0, time.UTC)
	options := DefaultOptions()
	options.ProbeInterval = 30 * time.Second
	options.RefreshInterval = time.Second
	options.DispatchInterval = 100 * time.Millisecond
	options.ProbeTimeout = time.Second
	options.StateTTL = time.Minute
	source := &stubTargetSource{targets: []catalog.HealthProbeTarget{activeTarget(7, "http://127.0.0.1:18097")}}
	prober := &countingProber{}
	gate := &stubProbeGate{}
	scheduler, err := NewScheduler(options, source, prober, &recordingObserver{}, gate, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewScheduler() error = %v", err)
	}
	if err := scheduler.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	now = now.Add(options.ProbeInterval + time.Second)
	if err := scheduler.Dispatch(context.Background()); err != nil {
		t.Fatalf("Dispatch(suppressed) error = %v", err)
	}
	if prober.calls.Load() != 0 || scheduler.Stats().SuppressedProbes != 1 {
		t.Fatalf("suppressed Stats() = %+v, calls = %d", scheduler.Stats(), prober.calls.Load())
	}
	now = now.Add(options.ProbeInterval + time.Second)
	gate.err = errors.New("synthetic passive health failure")
	if err := scheduler.Dispatch(context.Background()); err != nil {
		t.Fatalf("Dispatch(gate failure) error = %v", err)
	}
	if prober.calls.Load() != 0 || scheduler.Stats().GateFailures != 1 {
		t.Fatalf("gate failure Stats() = %+v, calls = %d", scheduler.Stats(), prober.calls.Load())
	}
}

func TestDeterministicCadenceStaysWithinJitterBound(t *testing.T) {
	t.Parallel()
	interval := 5 * time.Minute
	first := cadence(activeUUID(1), interval, 20)
	second := cadence(activeUUID(1), interval, 20)
	if first != second || first < 4*time.Minute || first > 6*time.Minute {
		t.Fatalf("cadence = %v/%v", first, second)
	}
	spread := startupSpread(activeUUID(1), interval)
	if spread < 0 || spread >= interval {
		t.Fatalf("startup spread = %v", spread)
	}
}

type stubTargetSource struct {
	targets []catalog.HealthProbeTarget
	err     error
	called  chan struct{}
}

func (source *stubTargetSource) ListHealthProbeTargets(context.Context) ([]catalog.HealthProbeTarget, error) {
	if source.called != nil {
		select {
		case source.called <- struct{}{}:
		default:
		}
	}
	if source.err != nil {
		return nil, source.err
	}
	return append([]catalog.HealthProbeTarget(nil), source.targets...), nil
}

type countingProber struct {
	delay   time.Duration
	calls   atomic.Uint64
	current atomic.Int64
	maximum atomic.Int64
}

func (prober *countingProber) Probe(ctx context.Context, _ catalog.HealthProbeTarget) ProbeResult {
	prober.calls.Add(1)
	current := prober.current.Add(1)
	for {
		maximum := prober.maximum.Load()
		if current <= maximum || prober.maximum.CompareAndSwap(maximum, current) {
			break
		}
	}
	if prober.delay > 0 {
		timer := time.NewTimer(prober.delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			prober.current.Add(-1)
			return ProbeResult{Code: classifyContext(ctx)}
		}
	}
	prober.current.Add(-1)
	return ProbeResult{Code: ResultSucceeded, Latency: prober.delay}
}

type recordingObserver struct {
	mutex        sync.Mutex
	observations []Observation
}

type stubProbeGate struct {
	needs bool
	err   error
}

func (gate *stubProbeGate) NeedsActiveProbe(context.Context, string) (bool, error) {
	return gate.needs, gate.err
}

func (observer *recordingObserver) Observe(_ context.Context, observation Observation) error {
	observer.mutex.Lock()
	observer.observations = append(observer.observations, observation)
	observer.mutex.Unlock()
	return nil
}

func (observer *recordingObserver) count() int {
	observer.mutex.Lock()
	defer observer.mutex.Unlock()
	return len(observer.observations)
}

func activeTarget(value int, endpoint string) catalog.HealthProbeTarget {
	now := time.Date(2026, 7, 31, 4, 0, 0, 0, time.UTC)
	providerID := "80000000-0000-4000-8000-000000000001"
	return catalog.HealthProbeTarget{
		Provider: catalog.Provider{
			ID: providerID, Code: "mock-provider", Name: "Mock Provider", AdapterType: "mock",
			Status: catalog.StatusActive, Version: 1, CreatedAt: now, CreatedBy: "test:active-health",
			UpdatedAt: now, UpdatedBy: "test:active-health",
		},
		Deployment: catalog.Deployment{
			ID: activeUUID(value), ProviderID: providerID, Code: "deployment-" + leftPad12(value),
			PhysicalModel: "mock-model", EndpointURL: endpoint, Region: "local",
			Capabilities: catalog.CapabilitySet{
				Chat: true, MaxContextTokens: 4096, MaxOutputTokens: 1024,
				DataRetentionMode: catalog.RetentionZero, ProviderProtocolVersion: "mock-v1",
			},
			Status: catalog.StatusActive, Version: 1, CreatedAt: now, CreatedBy: "test:active-health",
			UpdatedAt: now, UpdatedBy: "test:active-health",
		},
	}
}
