package routing

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

const circuitPolicyVersion = "circuit-breaker/v1"

var (
	// ErrCircuitInvalid means options, identity, or completion input is malformed.
	ErrCircuitInvalid = errors.New("circuit breaker input is invalid")
	// ErrCircuitOpen means the deployment remains inside its open cooldown.
	ErrCircuitOpen = errors.New("deployment circuit is open")
	// ErrHalfOpenSaturated means all bounded half-open probe permits are in use.
	ErrHalfOpenSaturated = errors.New("deployment half-open probes are saturated")
	// ErrCircuitCapacity means no safe state slot is available without evicting
	// an open, half-open, or in-flight deployment.
	ErrCircuitCapacity = errors.New("circuit breaker state capacity is exhausted")
	// ErrCircuitPermitCompleted means one permit was completed more than once.
	ErrCircuitPermitCompleted = errors.New("circuit breaker permit is already completed")
)

// CircuitState is the finite state of one deployment circuit.
type CircuitState string

const (
	// CircuitClosed allows ordinary attempts and counts attributable failures.
	CircuitClosed CircuitState = "closed"
	// CircuitOpen rejects attempts until its cooldown expires.
	CircuitOpen CircuitState = "open"
	// CircuitHalfOpen admits only a bounded number of recovery probes.
	CircuitHalfOpen CircuitState = "half_open"
)

// CircuitOutcome is one provider-health interpretation of a completed attempt.
type CircuitOutcome string

const (
	// CircuitSucceeded resets closed failures or advances half-open recovery.
	CircuitSucceeded CircuitOutcome = "succeeded"
	// CircuitFailed increments closed failures or immediately reopens half-open.
	CircuitFailed CircuitOutcome = "failed"
	// CircuitIgnored releases a permit without changing provider-health evidence.
	CircuitIgnored CircuitOutcome = "ignored"
)

// CircuitOptions bounds sensitivity, recovery traffic, and memory.
type CircuitOptions struct {
	FailureThreshold         uint32
	OpenDuration             time.Duration
	HalfOpenMaximumProbes    uint32
	HalfOpenSuccessThreshold uint32
	MaximumDeployments       int
}

// DefaultCircuitOptions returns conservative process bootstrap policy.
func DefaultCircuitOptions() CircuitOptions {
	return CircuitOptions{
		FailureThreshold: 5, OpenDuration: 30 * time.Second,
		HalfOpenMaximumProbes: 2, HalfOpenSuccessThreshold: 2,
		MaximumDeployments: 10_000,
	}
}

// Validate checks circuit policy bounds.
func (options CircuitOptions) Validate() error {
	if options.FailureThreshold < 2 || options.FailureThreshold > 1000 {
		return fmt.Errorf("%w: failure threshold must be between 2 and 1000", ErrCircuitInvalid)
	}
	if options.OpenDuration < time.Second || options.OpenDuration > time.Hour {
		return fmt.Errorf("%w: open duration must be between 1s and 1h", ErrCircuitInvalid)
	}
	if options.HalfOpenMaximumProbes < 1 || options.HalfOpenMaximumProbes > 1000 {
		return fmt.Errorf("%w: half-open maximum probes must be between 1 and 1000", ErrCircuitInvalid)
	}
	if options.HalfOpenSuccessThreshold < 1 || options.HalfOpenSuccessThreshold > 1000 {
		return fmt.Errorf("%w: half-open success threshold must be between 1 and 1000", ErrCircuitInvalid)
	}
	if options.MaximumDeployments < 1 || options.MaximumDeployments > 100_000 {
		return fmt.Errorf("%w: maximum deployments must be between 1 and 100000", ErrCircuitInvalid)
	}
	return nil
}

// CircuitSnapshot is a content-free, consistent deployment state.
type CircuitSnapshot struct {
	PolicyVersion         string       `json:"policy_version"`
	DeploymentID          string       `json:"deployment_id"`
	Tracked               bool         `json:"tracked"`
	State                 CircuitState `json:"state"`
	Healthy               bool         `json:"healthy"`
	Generation            uint64       `json:"generation"`
	ConsecutiveFailures   uint32       `json:"consecutive_failures"`
	HalfOpenSuccesses     uint32       `json:"half_open_successes"`
	InFlight              uint32       `json:"in_flight"`
	OpenedAt              time.Time    `json:"opened_at"`
	RetryAt               time.Time    `json:"retry_at"`
	LastTransitionAt      time.Time    `json:"last_transition_at"`
	TotalSuccesses        uint64       `json:"total_successes"`
	TotalFailures         uint64       `json:"total_failures"`
	TotalIgnored          uint64       `json:"total_ignored"`
	RejectedOpen          uint64       `json:"rejected_open"`
	RejectedHalfOpen      uint64       `json:"rejected_half_open"`
	RejectedCapacity      uint64       `json:"rejected_capacity"`
	EvictedClosedCircuits uint64       `json:"evicted_closed_circuits"`
}

type circuitRecord struct {
	state               CircuitState
	generation          uint64
	consecutiveFailures uint32
	halfOpenSuccesses   uint32
	inFlight            uint32
	openedAt            time.Time
	retryAt             time.Time
	lastTransitionAt    time.Time
	lastTouchedAt       time.Time
	totalSuccesses      uint64
	totalFailures       uint64
	totalIgnored        uint64
	rejectedOpen        uint64
	rejectedHalfOpen    uint64
}

// CircuitBreaker owns bounded per-deployment state and is safe for concurrent use.
type CircuitBreaker struct {
	mutex            sync.Mutex
	options          CircuitOptions
	now              func() time.Time
	deployments      map[string]*circuitRecord
	evictions        uint64
	rejectedCapacity uint64
}

// CircuitPermit is an exactly-once completion token for one real attempt.
// A token is bound to the deployment state generation at acquisition time.
type CircuitPermit struct {
	breaker      *CircuitBreaker
	deploymentID string
	generation   uint64
	completed    atomic.Bool
}

// NewCircuitBreaker validates and creates one process-scoped breaker.
func NewCircuitBreaker(options CircuitOptions, now func() time.Time) (*CircuitBreaker, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	if now == nil {
		return nil, fmt.Errorf("%w: clock must not be nil", ErrCircuitInvalid)
	}
	return &CircuitBreaker{options: options, now: now, deployments: make(map[string]*circuitRecord)}, nil
}

// Healthy implements HealthReader. Open circuits are denied; expired circuits
// transition to half-open and advertise eligibility only while a probe slot is
// potentially available. Acquire performs the authoritative reservation.
func (breaker *CircuitBreaker) Healthy(ctx context.Context, deploymentID string) (bool, error) {
	if err := breaker.validateRead(ctx, deploymentID); err != nil {
		return false, err
	}
	now, err := breaker.currentTime()
	if err != nil {
		return false, err
	}
	breaker.mutex.Lock()
	record := breaker.deployments[deploymentID]
	if record == nil {
		breaker.mutex.Unlock()
		return true, nil
	}
	breaker.advanceOpenLocked(record, now)
	healthy := record.state == CircuitClosed ||
		(record.state == CircuitHalfOpen && record.inFlight < breaker.options.HalfOpenMaximumProbes)
	breaker.mutex.Unlock()
	return healthy, nil
}

// Acquire reserves one real Attempt. Closed attempts are tracked so an entry
// with outstanding completions cannot be evicted. Half-open reservations are
// strictly capped under the same mutex as state transitions.
func (breaker *CircuitBreaker) Acquire(ctx context.Context, deploymentID string) (*CircuitPermit, error) {
	if err := breaker.validateRead(ctx, deploymentID); err != nil {
		return nil, err
	}
	now, err := breaker.currentTime()
	if err != nil {
		return nil, err
	}
	breaker.mutex.Lock()
	record := breaker.deployments[deploymentID]
	if record == nil {
		record = breaker.allocateLocked(deploymentID, now)
		if record == nil {
			breaker.rejectedCapacity = saturatingIncrement(breaker.rejectedCapacity)
			breaker.mutex.Unlock()
			return nil, ErrCircuitCapacity
		}
	}
	breaker.advanceOpenLocked(record, now)
	switch record.state {
	case CircuitOpen:
		record.rejectedOpen = saturatingIncrement(record.rejectedOpen)
		record.lastTouchedAt = now
		breaker.mutex.Unlock()
		return nil, ErrCircuitOpen
	case CircuitHalfOpen:
		if record.inFlight >= breaker.options.HalfOpenMaximumProbes {
			record.rejectedHalfOpen = saturatingIncrement(record.rejectedHalfOpen)
			record.lastTouchedAt = now
			breaker.mutex.Unlock()
			return nil, ErrHalfOpenSaturated
		}
	case CircuitClosed:
	default:
		breaker.mutex.Unlock()
		return nil, ErrCircuitInvalid
	}
	if record.inFlight == math.MaxUint32 {
		breaker.mutex.Unlock()
		return nil, ErrCircuitCapacity
	}
	record.inFlight++
	record.lastTouchedAt = now
	permit := &CircuitPermit{breaker: breaker, deploymentID: deploymentID, generation: record.generation}
	breaker.mutex.Unlock()
	return permit, nil
}

// Complete releases an acquired Attempt and applies exactly one outcome. Late
// completions from an earlier generation are ignored so they cannot close a
// newly reopened circuit or underflow current half-open accounting.
func (permit *CircuitPermit) Complete(ctx context.Context, outcome CircuitOutcome) error {
	if permit == nil || permit.breaker == nil || !routeDeploymentIDPattern.MatchString(permit.deploymentID) {
		return ErrCircuitInvalid
	}
	if ctx == nil {
		return ErrCircuitInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if outcome != CircuitSucceeded && outcome != CircuitFailed && outcome != CircuitIgnored {
		return ErrCircuitInvalid
	}
	if !permit.completed.CompareAndSwap(false, true) {
		return ErrCircuitPermitCompleted
	}
	now, err := permit.breaker.currentTime()
	if err != nil {
		return err
	}
	breaker := permit.breaker
	breaker.mutex.Lock()
	record := breaker.deployments[permit.deploymentID]
	if record == nil || record.generation != permit.generation {
		breaker.mutex.Unlock()
		return nil
	}
	if record.inFlight == 0 {
		breaker.mutex.Unlock()
		return ErrCircuitInvalid
	}
	record.inFlight--
	record.lastTouchedAt = now
	switch outcome {
	case CircuitSucceeded:
		record.totalSuccesses = saturatingIncrement(record.totalSuccesses)
		switch record.state {
		case CircuitClosed:
			record.consecutiveFailures = 0
		case CircuitHalfOpen:
			record.halfOpenSuccesses = circuitSaturatingIncrement32(record.halfOpenSuccesses)
			if record.halfOpenSuccesses >= breaker.options.HalfOpenSuccessThreshold {
				breaker.closeLocked(record, now)
			}
		case CircuitOpen:
			// A matching-generation permit cannot normally observe Open.
		}
	case CircuitFailed:
		record.totalFailures = saturatingIncrement(record.totalFailures)
		switch record.state {
		case CircuitClosed:
			record.consecutiveFailures = circuitSaturatingIncrement32(record.consecutiveFailures)
			if record.consecutiveFailures >= breaker.options.FailureThreshold {
				breaker.openLocked(record, now)
			}
		case CircuitHalfOpen:
			breaker.openLocked(record, now)
		case CircuitOpen:
			// A matching-generation permit cannot normally observe Open.
		}
	case CircuitIgnored:
		record.totalIgnored = saturatingIncrement(record.totalIgnored)
	}
	breaker.mutex.Unlock()
	return nil
}

// Snapshot returns a consistent current state. Reading an expired open circuit
// performs the same half-open transition as routing or acquisition.
func (breaker *CircuitBreaker) Snapshot(ctx context.Context, deploymentID string) (CircuitSnapshot, error) {
	if err := breaker.validateRead(ctx, deploymentID); err != nil {
		return CircuitSnapshot{}, err
	}
	now, err := breaker.currentTime()
	if err != nil {
		return CircuitSnapshot{}, err
	}
	breaker.mutex.Lock()
	record := breaker.deployments[deploymentID]
	if record == nil {
		snapshot := CircuitSnapshot{
			PolicyVersion: circuitPolicyVersion, DeploymentID: deploymentID,
			State: CircuitClosed, Healthy: true, RejectedCapacity: breaker.rejectedCapacity,
			EvictedClosedCircuits: breaker.evictions,
		}
		breaker.mutex.Unlock()
		return snapshot, nil
	}
	breaker.advanceOpenLocked(record, now)
	snapshot := breaker.snapshotLocked(deploymentID, record)
	breaker.mutex.Unlock()
	return snapshot, nil
}

func (breaker *CircuitBreaker) validateRead(ctx context.Context, deploymentID string) error {
	if breaker == nil || breaker.now == nil || breaker.deployments == nil {
		return ErrCircuitInvalid
	}
	if ctx == nil {
		return ErrCircuitInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !routeDeploymentIDPattern.MatchString(deploymentID) {
		return ErrCircuitInvalid
	}
	return nil
}

func (breaker *CircuitBreaker) currentTime() (time.Time, error) {
	now := breaker.now().UTC()
	if now.IsZero() {
		return time.Time{}, ErrCircuitInvalid
	}
	return now, nil
}

func (breaker *CircuitBreaker) allocateLocked(deploymentID string, now time.Time) *circuitRecord {
	if len(breaker.deployments) >= breaker.options.MaximumDeployments {
		oldestID := ""
		var oldestTime time.Time
		for candidateID, record := range breaker.deployments {
			if record.state != CircuitClosed || record.inFlight != 0 {
				continue
			}
			if oldestID == "" || record.lastTouchedAt.Before(oldestTime) ||
				(record.lastTouchedAt.Equal(oldestTime) && candidateID < oldestID) {
				oldestID = candidateID
				oldestTime = record.lastTouchedAt
			}
		}
		if oldestID == "" {
			return nil
		}
		delete(breaker.deployments, oldestID)
		breaker.evictions = saturatingIncrement(breaker.evictions)
	}
	record := &circuitRecord{state: CircuitClosed, generation: 1, lastTransitionAt: now, lastTouchedAt: now}
	breaker.deployments[deploymentID] = record
	return record
}

func (breaker *CircuitBreaker) advanceOpenLocked(record *circuitRecord, now time.Time) {
	if record.state == CircuitOpen && !now.Before(record.retryAt) {
		record.state = CircuitHalfOpen
		record.generation = saturatingIncrement(record.generation)
		record.halfOpenSuccesses = 0
		record.inFlight = 0
		record.lastTransitionAt = now
		record.lastTouchedAt = now
	}
}

func (breaker *CircuitBreaker) openLocked(record *circuitRecord, now time.Time) {
	record.state = CircuitOpen
	record.generation = saturatingIncrement(record.generation)
	record.openedAt = now
	record.retryAt = now.Add(breaker.options.OpenDuration)
	record.halfOpenSuccesses = 0
	record.inFlight = 0
	record.lastTransitionAt = now
	record.lastTouchedAt = now
}

func (*CircuitBreaker) closeLocked(record *circuitRecord, now time.Time) {
	record.state = CircuitClosed
	record.generation = saturatingIncrement(record.generation)
	record.consecutiveFailures = 0
	record.halfOpenSuccesses = 0
	record.inFlight = 0
	record.openedAt = time.Time{}
	record.retryAt = time.Time{}
	record.lastTransitionAt = now
	record.lastTouchedAt = now
}

func (breaker *CircuitBreaker) snapshotLocked(deploymentID string, record *circuitRecord) CircuitSnapshot {
	return CircuitSnapshot{
		PolicyVersion: circuitPolicyVersion, DeploymentID: deploymentID, Tracked: true,
		State: record.state,
		Healthy: record.state == CircuitClosed ||
			(record.state == CircuitHalfOpen && record.inFlight < breaker.options.HalfOpenMaximumProbes),
		Generation: record.generation, ConsecutiveFailures: record.consecutiveFailures,
		HalfOpenSuccesses: record.halfOpenSuccesses, InFlight: record.inFlight,
		OpenedAt: record.openedAt, RetryAt: record.retryAt, LastTransitionAt: record.lastTransitionAt,
		TotalSuccesses: record.totalSuccesses, TotalFailures: record.totalFailures,
		TotalIgnored: record.totalIgnored, RejectedOpen: record.rejectedOpen,
		RejectedHalfOpen: record.rejectedHalfOpen, RejectedCapacity: breaker.rejectedCapacity,
		EvictedClosedCircuits: breaker.evictions,
	}
}

func circuitSaturatingIncrement32(value uint32) uint32 {
	if value == math.MaxUint32 {
		return value
	}
	return value + 1
}

var _ HealthReader = (*CircuitBreaker)(nil)
