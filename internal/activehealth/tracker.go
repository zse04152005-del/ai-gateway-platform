// Package activehealth runs low-cost deployment probes outside production
// request accounting and exposes a sample-aware routing health signal.
package activehealth

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sync"
	"time"
)

const activeHealthPolicyVersion = "active-health/v1"

var (
	// ErrInvalid means active-health configuration or input is malformed.
	ErrInvalid          = errors.New("active health input is invalid")
	deploymentIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

// State is the finite routing assessment for one active probe target.
type State string

const (
	// StateUnknown has no completed probe and therefore allows traffic.
	StateUnknown State = "unknown"
	// StateHealthy has a recent success or remains below the failure threshold.
	StateHealthy State = "healthy"
	// StateUnhealthy reached the consecutive-failure threshold.
	StateUnhealthy State = "unhealthy"
	// StateStale has expired evidence and therefore fails open.
	StateStale State = "stale"
)

// ResultCode is a bounded, content-free probe outcome.
type ResultCode string

const (
	// ResultSucceeded means a complete valid response was received.
	ResultSucceeded ResultCode = "succeeded"
	// ResultTimedOut means the isolated probe deadline elapsed.
	ResultTimedOut ResultCode = "timed_out"
	// ResultCancelled means process cancellation stopped the probe.
	ResultCancelled ResultCode = "cancelled"
	// ResultAdapterUnavailable means request construction could not start safely.
	ResultAdapterUnavailable ResultCode = "adapter_unavailable"
	// ResultTransportFailure means no usable HTTP response was received.
	ResultTransportFailure ResultCode = "transport_failure"
	// ResultProviderFailure means the provider returned a non-success status.
	ResultProviderFailure ResultCode = "provider_failure"
	// ResultProtocolFailure means a success response could not be normalized.
	ResultProtocolFailure ResultCode = "protocol_failure"
)

// Options bounds probe frequency, resource use, and health-state sensitivity.
type Options struct {
	ProbeInterval      time.Duration
	RefreshInterval    time.Duration
	DispatchInterval   time.Duration
	ProbeTimeout       time.Duration
	StateTTL           time.Duration
	FailureThreshold   uint32
	RecoveryThreshold  uint32
	MaximumTargets     int
	MaximumConcurrency int
	MaximumBatchSize   int
	JitterPercent      uint8
}

// DefaultOptions is intentionally conservative: passive health handles fast
// traffic failures while active probes provide a low-frequency backstop.
func DefaultOptions() Options {
	return Options{
		ProbeInterval: 5 * time.Minute, RefreshInterval: 30 * time.Second,
		DispatchInterval: time.Second, ProbeTimeout: 5 * time.Second,
		StateTTL: 20 * time.Minute, FailureThreshold: 3, RecoveryThreshold: 2,
		MaximumTargets: 10_000, MaximumConcurrency: 4, MaximumBatchSize: 16,
		JitterPercent: 20,
	}
}

// Validate checks operational bounds and relationships.
func (options Options) Validate() error {
	if options.ProbeInterval < 30*time.Second || options.ProbeInterval > time.Hour {
		return fmt.Errorf("%w: probe interval must be between 30s and 1h", ErrInvalid)
	}
	if options.RefreshInterval < time.Second || options.RefreshInterval > options.ProbeInterval {
		return fmt.Errorf("%w: refresh interval must be between 1s and the probe interval", ErrInvalid)
	}
	if options.DispatchInterval < 100*time.Millisecond || options.DispatchInterval > options.RefreshInterval {
		return fmt.Errorf("%w: dispatch interval must be between 100ms and the refresh interval", ErrInvalid)
	}
	if options.ProbeTimeout < 100*time.Millisecond || options.ProbeTimeout > 30*time.Second ||
		options.ProbeTimeout >= options.ProbeInterval {
		return fmt.Errorf("%w: probe timeout must be between 100ms and 30s and below the probe interval", ErrInvalid)
	}
	if options.StateTTL < 2*options.ProbeInterval || options.StateTTL > 24*time.Hour {
		return fmt.Errorf("%w: state TTL must be at least two probe intervals and at most 24h", ErrInvalid)
	}
	if options.FailureThreshold < 2 || options.FailureThreshold > 100 ||
		options.RecoveryThreshold < 1 || options.RecoveryThreshold > 100 {
		return fmt.Errorf("%w: failure/recovery thresholds are outside safe bounds", ErrInvalid)
	}
	if options.MaximumTargets < 1 || options.MaximumTargets > 100_000 {
		return fmt.Errorf("%w: maximum targets must be between 1 and 100000", ErrInvalid)
	}
	if options.MaximumConcurrency < 1 || options.MaximumConcurrency > 256 {
		return fmt.Errorf("%w: maximum concurrency must be between 1 and 256", ErrInvalid)
	}
	if options.MaximumBatchSize < options.MaximumConcurrency || options.MaximumBatchSize > 4096 {
		return fmt.Errorf("%w: maximum batch size must cover concurrency and not exceed 4096", ErrInvalid)
	}
	if options.JitterPercent > 50 {
		return fmt.Errorf("%w: jitter percent must not exceed 50", ErrInvalid)
	}
	return nil
}

// Observation records one terminal internal probe without content or secrets.
type Observation struct {
	DeploymentID string
	Code         ResultCode
	Latency      time.Duration
}

// Validate checks the bounded observation contract.
func (observation Observation) Validate() error {
	if !deploymentIDPattern.MatchString(observation.DeploymentID) {
		return fmt.Errorf("%w: deployment ID", ErrInvalid)
	}
	switch observation.Code {
	case ResultSucceeded, ResultTimedOut, ResultCancelled, ResultAdapterUnavailable,
		ResultTransportFailure, ResultProviderFailure, ResultProtocolFailure:
	default:
		return fmt.Errorf("%w: result code", ErrInvalid)
	}
	if observation.Latency < 0 || observation.Latency > time.Minute {
		return fmt.Errorf("%w: latency", ErrInvalid)
	}
	return nil
}

// Snapshot is one safe point-in-time routing assessment.
type Snapshot struct {
	PolicyVersion        string        `json:"policy_version"`
	DeploymentID         string        `json:"deployment_id"`
	State                State         `json:"state"`
	Healthy              bool          `json:"healthy"`
	Stale                bool          `json:"stale"`
	LastResult           ResultCode    `json:"last_result"`
	LastObservedAt       time.Time     `json:"last_observed_at"`
	LastSuccessAt        time.Time     `json:"last_success_at"`
	LastFailureAt        time.Time     `json:"last_failure_at"`
	LastLatency          time.Duration `json:"last_latency"`
	TotalProbes          uint64        `json:"total_probes"`
	TotalSuccesses       uint64        `json:"total_successes"`
	TotalFailures        uint64        `json:"total_failures"`
	ConsecutiveFailures  uint32        `json:"consecutive_failures"`
	ConsecutiveSuccesses uint32        `json:"consecutive_successes"`
	EvictedTargets       uint64        `json:"evicted_targets"`
}

type targetState struct {
	state                State
	lastResult           ResultCode
	lastObservedAt       time.Time
	lastSuccessAt        time.Time
	lastFailureAt        time.Time
	lastLatency          time.Duration
	totalProbes          uint64
	totalSuccesses       uint64
	totalFailures        uint64
	consecutiveFailures  uint32
	consecutiveSuccesses uint32
}

// Tracker owns bounded hysteresis state and implements routing.HealthReader.
type Tracker struct {
	mutex     sync.RWMutex
	options   Options
	now       func() time.Time
	targets   map[string]*targetState
	evictions uint64
}

// NewTracker validates and creates a process-scoped tracker.
func NewTracker(options Options, now func() time.Time) (*Tracker, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	if now == nil {
		return nil, fmt.Errorf("%w: clock must not be nil", ErrInvalid)
	}
	return &Tracker{options: options, now: now, targets: make(map[string]*targetState)}, nil
}

// Observe applies failure and recovery hysteresis to one terminal probe.
func (tracker *Tracker) Observe(ctx context.Context, observation Observation) error {
	if tracker == nil || tracker.now == nil || tracker.targets == nil {
		return fmt.Errorf("%w: tracker is not initialized", ErrInvalid)
	}
	if ctx == nil {
		return fmt.Errorf("%w: context must not be nil", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := observation.Validate(); err != nil {
		return err
	}
	now := tracker.now().UTC()
	if now.IsZero() {
		return fmt.Errorf("%w: clock returned zero", ErrInvalid)
	}

	tracker.mutex.Lock()
	state := tracker.targets[observation.DeploymentID]
	if state == nil {
		if len(tracker.targets) >= tracker.options.MaximumTargets {
			tracker.evictOldestLocked()
		}
		state = &targetState{state: StateUnknown}
		tracker.targets[observation.DeploymentID] = state
	}
	state.lastResult = observation.Code
	state.lastObservedAt = now
	state.lastLatency = observation.Latency
	state.totalProbes = saturatingIncrement(state.totalProbes)
	if observation.Code == ResultSucceeded {
		state.totalSuccesses = saturatingIncrement(state.totalSuccesses)
		state.consecutiveFailures = 0
		state.consecutiveSuccesses = saturatingIncrement32(state.consecutiveSuccesses)
		state.lastSuccessAt = now
		if state.state != StateUnhealthy || state.consecutiveSuccesses >= tracker.options.RecoveryThreshold {
			state.state = StateHealthy
		}
	} else {
		state.totalFailures = saturatingIncrement(state.totalFailures)
		state.consecutiveSuccesses = 0
		state.consecutiveFailures = saturatingIncrement32(state.consecutiveFailures)
		state.lastFailureAt = now
		if state.consecutiveFailures >= tracker.options.FailureThreshold {
			state.state = StateUnhealthy
		} else if state.state == StateUnknown {
			state.state = StateHealthy
		}
	}
	tracker.mutex.Unlock()
	return nil
}

// Snapshot returns unknown/healthy for unseen targets and stale/healthy for
// expired observations so a broken monitoring path cannot blackhole traffic.
func (tracker *Tracker) Snapshot(ctx context.Context, deploymentID string) (Snapshot, error) {
	if tracker == nil || tracker.now == nil || tracker.targets == nil {
		return Snapshot{}, fmt.Errorf("%w: tracker is not initialized", ErrInvalid)
	}
	if ctx == nil {
		return Snapshot{}, fmt.Errorf("%w: context must not be nil", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if !deploymentIDPattern.MatchString(deploymentID) {
		return Snapshot{}, fmt.Errorf("%w: deployment ID", ErrInvalid)
	}
	now := tracker.now().UTC()
	if now.IsZero() {
		return Snapshot{}, fmt.Errorf("%w: clock returned zero", ErrInvalid)
	}

	tracker.mutex.RLock()
	state := tracker.targets[deploymentID]
	evictions := tracker.evictions
	if state == nil {
		tracker.mutex.RUnlock()
		return Snapshot{PolicyVersion: activeHealthPolicyVersion, DeploymentID: deploymentID,
			State: StateUnknown, Healthy: true, EvictedTargets: evictions}, nil
	}
	stateCopy := *state
	tracker.mutex.RUnlock()

	result := Snapshot{
		PolicyVersion: activeHealthPolicyVersion, DeploymentID: deploymentID,
		State: stateCopy.state, Healthy: stateCopy.state != StateUnhealthy,
		LastResult: stateCopy.lastResult, LastObservedAt: stateCopy.lastObservedAt,
		LastSuccessAt: stateCopy.lastSuccessAt, LastFailureAt: stateCopy.lastFailureAt,
		LastLatency: stateCopy.lastLatency, TotalProbes: stateCopy.totalProbes,
		TotalSuccesses: stateCopy.totalSuccesses, TotalFailures: stateCopy.totalFailures,
		ConsecutiveFailures:  stateCopy.consecutiveFailures,
		ConsecutiveSuccesses: stateCopy.consecutiveSuccesses, EvictedTargets: evictions,
	}
	if now.Sub(stateCopy.lastObservedAt) > tracker.options.StateTTL {
		result.State = StateStale
		result.Healthy = true
		result.Stale = true
	}
	return result, nil
}

// Healthy implements the routing health-reader contract.
func (tracker *Tracker) Healthy(ctx context.Context, deploymentID string) (bool, error) {
	snapshot, err := tracker.Snapshot(ctx, deploymentID)
	if err != nil {
		return false, err
	}
	return snapshot.Healthy, nil
}

func (tracker *Tracker) evictOldestLocked() {
	oldestID := ""
	var oldest time.Time
	for deploymentID, state := range tracker.targets {
		if oldestID == "" || state.lastObservedAt.Before(oldest) ||
			(state.lastObservedAt.Equal(oldest) && deploymentID < oldestID) {
			oldestID = deploymentID
			oldest = state.lastObservedAt
		}
	}
	if oldestID != "" {
		delete(tracker.targets, oldestID)
		tracker.evictions = saturatingIncrement(tracker.evictions)
	}
}

func saturatingIncrement(value uint64) uint64 {
	if value == math.MaxUint64 {
		return value
	}
	return value + 1
}

func saturatingIncrement32(value uint32) uint32 {
	if value == math.MaxUint32 {
		return value
	}
	return value + 1
}
