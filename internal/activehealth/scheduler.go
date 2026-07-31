package activehealth

import (
	"context"
	"errors"
	"hash/fnv"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
)

var (
	// ErrAlreadyRunning prevents duplicate process loops and duplicate probes.
	ErrAlreadyRunning = errors.New("active health scheduler is already running")
	// ErrDuplicateTarget means the catalog returned conflicting deployment rows.
	ErrDuplicateTarget = errors.New("active health catalog returned a duplicate target")
)

// TargetSource lists active physical deployments without credential material.
type TargetSource interface {
	ListHealthProbeTargets(context.Context) ([]catalog.HealthProbeTarget, error)
}

// Observer accepts safe terminal probe facts.
type Observer interface {
	Observe(context.Context, Observation) error
}

// ProbeGate suppresses paid active checks while recent production traffic
// already supplies a trustworthy passive health sample.
type ProbeGate interface {
	NeedsActiveProbe(context.Context, string) (bool, error)
}

type scheduledTarget struct {
	target  catalog.HealthProbeTarget
	nextDue time.Time
}

// SchedulerStats are monotonic process-local operational counters.
type SchedulerStats struct {
	TargetCount         int
	Refreshes           uint64
	RefreshFailures     uint64
	Probes              uint64
	ProbeSuccesses      uint64
	ProbeFailures       uint64
	SuppressedProbes    uint64
	GateFailures        uint64
	ObservationFailures uint64
}

// Scheduler refreshes target membership and dispatches bounded probe batches.
type Scheduler struct {
	mutex               sync.Mutex
	options             Options
	source              TargetSource
	prober              Prober
	observer            Observer
	gate                ProbeGate
	now                 func() time.Time
	targets             map[string]scheduledTarget
	running             atomic.Bool
	refreshes           atomic.Uint64
	refreshFailures     atomic.Uint64
	probes              atomic.Uint64
	probeSuccesses      atomic.Uint64
	probeFailures       atomic.Uint64
	suppressedProbes    atomic.Uint64
	gateFailures        atomic.Uint64
	observationFailures atomic.Uint64
}

// NewScheduler validates all process-scoped dependencies.
func NewScheduler(
	options Options,
	source TargetSource,
	prober Prober,
	observer Observer,
	gate ProbeGate,
	now func() time.Time,
) (*Scheduler, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	if source == nil || prober == nil || observer == nil || gate == nil || now == nil {
		return nil, errors.New("active health scheduler dependencies must not be nil")
	}
	return &Scheduler{options: options, source: source, prober: prober, observer: observer, gate: gate,
		now: now, targets: make(map[string]scheduledTarget)}, nil
}

// Refresh atomically replaces target membership after validating the complete
// catalog snapshot. Existing unchanged targets retain their cadence.
func (scheduler *Scheduler) Refresh(ctx context.Context) error {
	if scheduler == nil || scheduler.source == nil || scheduler.now == nil || scheduler.targets == nil {
		return ErrInvalid
	}
	if ctx == nil {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	targets, err := scheduler.source.ListHealthProbeTargets(ctx)
	if err != nil {
		scheduler.refreshFailures.Add(1)
		return err
	}
	if len(targets) > scheduler.options.MaximumTargets {
		scheduler.refreshFailures.Add(1)
		return ErrInvalid
	}
	sort.Slice(targets, func(left, right int) bool {
		return targets[left].Deployment.ID < targets[right].Deployment.ID
	})
	for index := range targets {
		if err := targets[index].Validate(); err != nil {
			scheduler.refreshFailures.Add(1)
			return ErrInvalid
		}
		if index > 0 && targets[index-1].Deployment.ID == targets[index].Deployment.ID {
			scheduler.refreshFailures.Add(1)
			return ErrDuplicateTarget
		}
	}
	now := scheduler.now().UTC()
	if now.IsZero() {
		scheduler.refreshFailures.Add(1)
		return ErrInvalid
	}

	scheduler.mutex.Lock()
	replacement := make(map[string]scheduledTarget, len(targets))
	for _, target := range targets {
		deploymentID := target.Deployment.ID
		existing, exists := scheduler.targets[deploymentID]
		if exists && sameTargetVersion(existing.target, target) {
			existing.target = target.Clone()
			replacement[deploymentID] = existing
			continue
		}
		replacement[deploymentID] = scheduledTarget{
			target: target.Clone(), nextDue: now.Add(startupSpread(deploymentID, scheduler.options.ProbeInterval)),
		}
	}
	scheduler.targets = replacement
	scheduler.mutex.Unlock()
	scheduler.refreshes.Add(1)
	return nil
}

// Dispatch probes at most one bounded due batch with fixed worker concurrency.
func (scheduler *Scheduler) Dispatch(ctx context.Context) error {
	if scheduler == nil || scheduler.prober == nil || scheduler.observer == nil || scheduler.gate == nil || scheduler.now == nil {
		return ErrInvalid
	}
	if ctx == nil {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now := scheduler.now().UTC()
	if now.IsZero() {
		return ErrInvalid
	}

	scheduler.mutex.Lock()
	due := make([]scheduledTarget, 0)
	for _, target := range scheduler.targets {
		if !target.nextDue.After(now) {
			due = append(due, target)
		}
	}
	sort.Slice(due, func(left, right int) bool {
		if !due[left].nextDue.Equal(due[right].nextDue) {
			return due[left].nextDue.Before(due[right].nextDue)
		}
		return due[left].target.Deployment.ID < due[right].target.Deployment.ID
	})
	if len(due) > scheduler.options.MaximumBatchSize {
		due = due[:scheduler.options.MaximumBatchSize]
	}
	for _, item := range due {
		deploymentID := item.target.Deployment.ID
		current := scheduler.targets[deploymentID]
		current.nextDue = now.Add(cadence(deploymentID, scheduler.options.ProbeInterval, scheduler.options.JitterPercent))
		scheduler.targets[deploymentID] = current
	}
	scheduler.mutex.Unlock()
	if len(due) == 0 {
		return nil
	}

	workerCount := scheduler.options.MaximumConcurrency
	if workerCount > len(due) {
		workerCount = len(due)
	}
	jobs := make(chan scheduledTarget)
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for index := 0; index < workerCount; index++ {
		go func() {
			defer workers.Done()
			for item := range jobs {
				needsProbe, gateErr := scheduler.gate.NeedsActiveProbe(ctx, item.target.Deployment.ID)
				if gateErr != nil {
					scheduler.gateFailures.Add(1)
					continue
				}
				if !needsProbe {
					scheduler.suppressedProbes.Add(1)
					continue
				}
				probeCtx, cancel := context.WithTimeout(ctx, scheduler.options.ProbeTimeout)
				result := scheduler.prober.Probe(probeCtx, item.target.Clone())
				cancel()
				if ctx.Err() != nil && result.Code == ResultCancelled {
					continue
				}
				scheduler.probes.Add(1)
				if result.Code == ResultSucceeded {
					scheduler.probeSuccesses.Add(1)
				} else {
					scheduler.probeFailures.Add(1)
				}
				observation := Observation{DeploymentID: item.target.Deployment.ID, Code: result.Code, Latency: result.Latency}
				if err := scheduler.observer.Observe(context.WithoutCancel(ctx), observation); err != nil {
					scheduler.observationFailures.Add(1)
				}
			}
		}()
	}
	for _, item := range due {
		select {
		case jobs <- item:
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return ctx.Err()
		}
	}
	close(jobs)
	workers.Wait()
	return nil
}

// Run owns exactly one refresh/dispatch loop until process cancellation.
func (scheduler *Scheduler) Run(ctx context.Context) error {
	if scheduler == nil || ctx == nil {
		return ErrInvalid
	}
	if !scheduler.running.CompareAndSwap(false, true) {
		return ErrAlreadyRunning
	}
	defer scheduler.running.Store(false)
	if err := ctx.Err(); err != nil {
		return nil
	}
	_ = scheduler.Refresh(ctx)
	_ = scheduler.Dispatch(ctx)
	refreshTicker := time.NewTicker(scheduler.options.RefreshInterval)
	dispatchTicker := time.NewTicker(scheduler.options.DispatchInterval)
	defer refreshTicker.Stop()
	defer dispatchTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-refreshTicker.C:
			_ = scheduler.Refresh(ctx)
		case <-dispatchTicker.C:
			_ = scheduler.Dispatch(ctx)
		}
	}
}

// Stats returns one race-free operational snapshot.
func (scheduler *Scheduler) Stats() SchedulerStats {
	if scheduler == nil {
		return SchedulerStats{}
	}
	scheduler.mutex.Lock()
	targetCount := len(scheduler.targets)
	scheduler.mutex.Unlock()
	return SchedulerStats{TargetCount: targetCount, Refreshes: scheduler.refreshes.Load(),
		RefreshFailures: scheduler.refreshFailures.Load(), Probes: scheduler.probes.Load(),
		ProbeSuccesses: scheduler.probeSuccesses.Load(), ProbeFailures: scheduler.probeFailures.Load(),
		SuppressedProbes: scheduler.suppressedProbes.Load(), GateFailures: scheduler.gateFailures.Load(),
		ObservationFailures: scheduler.observationFailures.Load()}
}

func sameTargetVersion(left, right catalog.HealthProbeTarget) bool {
	return left.Provider.ID == right.Provider.ID && left.Provider.Version == right.Provider.Version &&
		left.Deployment.ID == right.Deployment.ID && left.Deployment.Version == right.Deployment.Version
}

func startupSpread(deploymentID string, interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	return time.Duration(stableHash(deploymentID) % uint64(interval)) //nolint:gosec // bounded positive duration
}

func cadence(deploymentID string, interval time.Duration, jitterPercent uint8) time.Duration {
	if jitterPercent == 0 {
		return interval
	}
	unit := float64(stableHash(deploymentID+"/cadence")) / float64(^uint64(0))
	factor := 1 - float64(jitterPercent)/100 + unit*(2*float64(jitterPercent)/100)
	return time.Duration(float64(interval) * factor)
}

func stableHash(value string) uint64 {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(value))
	return hash.Sum64()
}

var _ Observer = (*Tracker)(nil)
