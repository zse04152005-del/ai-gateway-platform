package routing

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

const (
	passiveHealthPolicyVersion = "passive-health/v1"
	maximumPassiveLatency      = 24 * time.Hour
	passiveLatencyBucketCount  = 13
)

var (
	// ErrPassiveHealthInvalid means a passive-health fact or option is malformed.
	ErrPassiveHealthInvalid = errors.New("passive health input is invalid")
	passiveLatencyBounds    = [...]time.Duration{
		10 * time.Millisecond,
		25 * time.Millisecond,
		50 * time.Millisecond,
		100 * time.Millisecond,
		250 * time.Millisecond,
		500 * time.Millisecond,
		time.Second,
		2500 * time.Millisecond,
		5 * time.Second,
		10 * time.Second,
		30 * time.Second,
		60 * time.Second,
	}
)

// PassiveOutcome is one finite terminal category for a physical attempt.
type PassiveOutcome string

const (
	// PassiveSucceeded means the provider produced a complete valid response.
	PassiveSucceeded PassiveOutcome = "succeeded"
	// PassiveRateLimited means the provider returned HTTP 429.
	PassiveRateLimited PassiveOutcome = "rate_limited"
	// PassiveServerError means the provider returned an HTTP 5xx status.
	PassiveServerError PassiveOutcome = "server_error"
	// PassiveTimedOut means a provider-facing deadline elapsed.
	PassiveTimedOut PassiveOutcome = "timed_out"
	// PassiveCancelled means the caller cancelled the attempt.
	PassiveCancelled PassiveOutcome = "cancelled"
	// PassiveOtherFailure means a non-health failure ended the attempt.
	PassiveOtherFailure PassiveOutcome = "other_failure"
)

// PassiveState is the sample-aware health assessment for one deployment.
type PassiveState string

const (
	// PassiveStateWarmup means the current window has too few samples to judge.
	PassiveStateWarmup PassiveState = "warmup"
	// PassiveStateHealthy means a sufficient sample remains below the failure threshold.
	PassiveStateHealthy PassiveState = "healthy"
	// PassiveStateDegraded means a sufficient sample reached the failure threshold.
	PassiveStateDegraded PassiveState = "degraded"
)

// PassiveHealthOptions bounds memory, time resolution, and anomaly sensitivity.
type PassiveHealthOptions struct {
	Window                time.Duration
	BucketWidth           time.Duration
	MinimumSamples        uint64
	FailureRatioThreshold float64
	MaximumDeployments    int
}

// DefaultPassiveHealthOptions returns the process bootstrap policy.
func DefaultPassiveHealthOptions() PassiveHealthOptions {
	return PassiveHealthOptions{
		Window: 2 * time.Minute, BucketWidth: 5 * time.Second,
		MinimumSamples: 20, FailureRatioThreshold: 0.5, MaximumDeployments: 10_000,
	}
}

// Validate checks bounded sliding-window policy values.
func (options PassiveHealthOptions) Validate() error {
	if options.Window < time.Second || options.Window > time.Hour {
		return fmt.Errorf("%w: window must be between 1s and 1h", ErrPassiveHealthInvalid)
	}
	if options.BucketWidth < 100*time.Millisecond || options.BucketWidth > options.Window ||
		options.Window%options.BucketWidth != 0 {
		return fmt.Errorf("%w: bucket width must divide the window and be between 100ms and the window", ErrPassiveHealthInvalid)
	}
	bucketCount := options.Window / options.BucketWidth
	if bucketCount < 2 || bucketCount > 720 {
		return fmt.Errorf("%w: window must contain between 2 and 720 buckets", ErrPassiveHealthInvalid)
	}
	if options.MinimumSamples < 2 || options.MinimumSamples > 1_000_000 {
		return fmt.Errorf("%w: minimum samples must be between 2 and 1000000", ErrPassiveHealthInvalid)
	}
	if math.IsNaN(options.FailureRatioThreshold) || math.IsInf(options.FailureRatioThreshold, 0) ||
		options.FailureRatioThreshold <= 0 || options.FailureRatioThreshold > 1 {
		return fmt.Errorf("%w: failure ratio threshold must be in (0,1]", ErrPassiveHealthInvalid)
	}
	if options.MaximumDeployments < 1 || options.MaximumDeployments > 100_000 {
		return fmt.Errorf("%w: maximum deployments must be between 1 and 100000", ErrPassiveHealthInvalid)
	}
	return nil
}

// PassiveObservation contains content-free terminal metrics for one attempt.
type PassiveObservation struct {
	DeploymentID      string
	Outcome           PassiveOutcome
	ProviderStatus    int
	FirstTokenLatency *time.Duration
	TotalLatency      time.Duration
}

// Validate checks observation identity, category, and latency invariants.
func (observation PassiveObservation) Validate() error {
	if !routeDeploymentIDPattern.MatchString(observation.DeploymentID) {
		return fmt.Errorf("%w: deployment ID", ErrPassiveHealthInvalid)
	}
	if observation.ProviderStatus != 0 && (observation.ProviderStatus < 100 || observation.ProviderStatus > 599) {
		return fmt.Errorf("%w: provider status", ErrPassiveHealthInvalid)
	}
	switch observation.Outcome {
	case PassiveSucceeded:
		if observation.ProviderStatus != 0 && (observation.ProviderStatus < 200 || observation.ProviderStatus > 299) {
			return fmt.Errorf("%w: successful provider status", ErrPassiveHealthInvalid)
		}
	case PassiveRateLimited:
		if observation.ProviderStatus != 429 {
			return fmt.Errorf("%w: rate limit status must be 429", ErrPassiveHealthInvalid)
		}
	case PassiveServerError:
		if observation.ProviderStatus < 500 || observation.ProviderStatus > 599 {
			return fmt.Errorf("%w: server-error status must be 5xx", ErrPassiveHealthInvalid)
		}
	case PassiveTimedOut, PassiveCancelled, PassiveOtherFailure:
	default:
		return fmt.Errorf("%w: outcome", ErrPassiveHealthInvalid)
	}
	if observation.TotalLatency < 0 || observation.TotalLatency > maximumPassiveLatency {
		return fmt.Errorf("%w: total latency", ErrPassiveHealthInvalid)
	}
	if observation.FirstTokenLatency != nil &&
		(*observation.FirstTokenLatency < 0 || *observation.FirstTokenLatency > observation.TotalLatency) {
		return fmt.Errorf("%w: first-token latency", ErrPassiveHealthInvalid)
	}
	return nil
}

// LatencyStatistics is a bounded aggregate without request or response content.
type LatencyStatistics struct {
	Count         uint64        `json:"count"`
	Average       time.Duration `json:"average"`
	Maximum       time.Duration `json:"maximum"`
	P50UpperBound time.Duration `json:"p50_upper_bound"`
	P95UpperBound time.Duration `json:"p95_upper_bound"`
	P99UpperBound time.Duration `json:"p99_upper_bound"`
}

// PassiveSnapshot is one consistent, queryable deployment window.
type PassiveSnapshot struct {
	PolicyVersion      string            `json:"policy_version"`
	DeploymentID       string            `json:"deployment_id"`
	State              PassiveState      `json:"state"`
	Healthy            bool              `json:"healthy"`
	SampleSufficient   bool              `json:"sample_sufficient"`
	WindowStart        time.Time         `json:"window_start"`
	EvaluatedAt        time.Time         `json:"evaluated_at"`
	RequestCount       uint64            `json:"request_count"`
	SuccessCount       uint64            `json:"success_count"`
	RateLimitCount     uint64            `json:"rate_limit_count"`
	ServerErrorCount   uint64            `json:"server_error_count"`
	TimeoutCount       uint64            `json:"timeout_count"`
	CancelledCount     uint64            `json:"cancelled_count"`
	OtherFailureCount  uint64            `json:"other_failure_count"`
	HealthSampleCount  uint64            `json:"health_sample_count"`
	FailureSignalCount uint64            `json:"failure_signal_count"`
	FailureRatio       float64           `json:"failure_ratio"`
	FirstTokenLatency  LatencyStatistics `json:"first_token_latency"`
	TotalLatency       LatencyStatistics `json:"total_latency"`
	EvictedDeployments uint64            `json:"evicted_deployments"`
}

// PassiveObserver records terminal physical-attempt metrics.
type PassiveObserver interface {
	Observe(context.Context, PassiveObservation) error
}

// PassiveHealth maintains bounded per-deployment sliding-window statistics.
type PassiveHealth struct {
	mutex       sync.RWMutex
	options     PassiveHealthOptions
	now         func() time.Time
	bucketCount int
	deployments map[string]*passiveDeployment
	evictions   uint64
}

type passiveDeployment struct {
	buckets      []passiveBucket
	lastObserved time.Time
}

type passiveBucket struct {
	startedAt    time.Time
	requests     uint64
	successes    uint64
	rateLimits   uint64
	serverErrors uint64
	timeouts     uint64
	cancelled    uint64
	otherFailure uint64
	firstToken   latencyAggregate
	total        latencyAggregate
}

type latencyAggregate struct {
	count      uint64
	totalNanos uint64
	maximum    time.Duration
	buckets    [passiveLatencyBucketCount]uint64
}

// NewPassiveHealth validates and creates one process-scoped tracker.
func NewPassiveHealth(options PassiveHealthOptions, now func() time.Time) (*PassiveHealth, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	if now == nil {
		return nil, fmt.Errorf("%w: clock must not be nil", ErrPassiveHealthInvalid)
	}
	return &PassiveHealth{
		options: options, now: now, bucketCount: int(options.Window / options.BucketWidth),
		deployments: make(map[string]*passiveDeployment),
	}, nil
}

// Observe adds one terminal attempt to its current time bucket.
func (health *PassiveHealth) Observe(ctx context.Context, observation PassiveObservation) error {
	if health == nil || health.now == nil || health.bucketCount < 2 || health.deployments == nil {
		return fmt.Errorf("%w: tracker is not initialized", ErrPassiveHealthInvalid)
	}
	if ctx == nil {
		return fmt.Errorf("%w: context must not be nil", ErrPassiveHealthInvalid)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := observation.Validate(); err != nil {
		return err
	}
	now := health.now().UTC()
	if now.IsZero() {
		return fmt.Errorf("%w: clock returned zero", ErrPassiveHealthInvalid)
	}
	startedAt := now.Truncate(health.options.BucketWidth)

	health.mutex.Lock()
	deployment := health.deployments[observation.DeploymentID]
	if deployment == nil {
		if len(health.deployments) >= health.options.MaximumDeployments {
			health.evictOldestLocked()
		}
		deployment = &passiveDeployment{buckets: make([]passiveBucket, health.bucketCount)}
		health.deployments[observation.DeploymentID] = deployment
	}
	index := health.bucketIndex(startedAt)
	if !deployment.buckets[index].startedAt.Equal(startedAt) {
		deployment.buckets[index] = passiveBucket{startedAt: startedAt}
	}
	deployment.buckets[index].add(observation)
	deployment.lastObserved = now
	health.mutex.Unlock()
	return nil
}

// Snapshot returns one consistent current-window assessment. Unknown
// deployments are warmup/healthy with zero samples.
func (health *PassiveHealth) Snapshot(ctx context.Context, deploymentID string) (PassiveSnapshot, error) {
	if health == nil || health.now == nil || health.bucketCount < 2 || health.deployments == nil {
		return PassiveSnapshot{}, fmt.Errorf("%w: tracker is not initialized", ErrPassiveHealthInvalid)
	}
	if ctx == nil {
		return PassiveSnapshot{}, fmt.Errorf("%w: context must not be nil", ErrPassiveHealthInvalid)
	}
	if err := ctx.Err(); err != nil {
		return PassiveSnapshot{}, err
	}
	if !routeDeploymentIDPattern.MatchString(deploymentID) {
		return PassiveSnapshot{}, fmt.Errorf("%w: deployment ID", ErrPassiveHealthInvalid)
	}
	now := health.now().UTC()
	if now.IsZero() {
		return PassiveSnapshot{}, fmt.Errorf("%w: clock returned zero", ErrPassiveHealthInvalid)
	}
	currentStart := now.Truncate(health.options.BucketWidth)
	oldestStart := currentStart.Add(-time.Duration(health.bucketCount-1) * health.options.BucketWidth)

	health.mutex.RLock()
	var aggregate passiveBucket
	if deployment := health.deployments[deploymentID]; deployment != nil {
		for index := range deployment.buckets {
			bucket := deployment.buckets[index]
			if bucket.startedAt.Before(oldestStart) || bucket.startedAt.After(currentStart) {
				continue
			}
			aggregate.merge(bucket)
		}
	}
	evictions := health.evictions
	health.mutex.RUnlock()
	return health.snapshot(deploymentID, oldestStart, now, aggregate, evictions), nil
}

// Healthy implements HealthReader with explicit low-sample protection.
func (health *PassiveHealth) Healthy(ctx context.Context, deploymentID string) (bool, error) {
	snapshot, err := health.Snapshot(ctx, deploymentID)
	if err != nil {
		return false, err
	}
	return snapshot.Healthy, nil
}

func (health *PassiveHealth) bucketIndex(startedAt time.Time) int {
	slot := startedAt.UnixNano() / health.options.BucketWidth.Nanoseconds()
	index := int(slot % int64(health.bucketCount))
	if index < 0 {
		index += health.bucketCount
	}
	return index
}

func (health *PassiveHealth) evictOldestLocked() {
	oldestID := ""
	var oldestTime time.Time
	for deploymentID, deployment := range health.deployments {
		if oldestID == "" || deployment.lastObserved.Before(oldestTime) ||
			(deployment.lastObserved.Equal(oldestTime) && deploymentID < oldestID) {
			oldestID = deploymentID
			oldestTime = deployment.lastObserved
		}
	}
	if oldestID != "" {
		delete(health.deployments, oldestID)
		health.evictions = saturatingIncrement(health.evictions)
	}
}

func (health *PassiveHealth) snapshot(
	deploymentID string,
	windowStart time.Time,
	now time.Time,
	aggregate passiveBucket,
	evictions uint64,
) PassiveSnapshot {
	failureSignals := saturatingSum(aggregate.rateLimits, aggregate.serverErrors, aggregate.timeouts)
	healthSamples := saturatingSum(aggregate.successes, failureSignals)
	sufficient := healthSamples >= health.options.MinimumSamples
	state := PassiveStateWarmup
	healthy := true
	ratio := float64(0)
	if healthSamples > 0 {
		ratio = float64(failureSignals) / float64(healthSamples)
	}
	if sufficient {
		state = PassiveStateHealthy
		if ratio >= health.options.FailureRatioThreshold {
			state = PassiveStateDegraded
			healthy = false
		}
	}
	return PassiveSnapshot{
		PolicyVersion: passiveHealthPolicyVersion, DeploymentID: deploymentID,
		State: state, Healthy: healthy, SampleSufficient: sufficient,
		WindowStart: windowStart, EvaluatedAt: now,
		RequestCount: aggregate.requests, SuccessCount: aggregate.successes,
		RateLimitCount: aggregate.rateLimits, ServerErrorCount: aggregate.serverErrors,
		TimeoutCount: aggregate.timeouts, CancelledCount: aggregate.cancelled,
		OtherFailureCount: aggregate.otherFailure,
		HealthSampleCount: healthSamples, FailureSignalCount: failureSignals, FailureRatio: ratio,
		FirstTokenLatency: aggregate.firstToken.statistics(), TotalLatency: aggregate.total.statistics(),
		EvictedDeployments: evictions,
	}
}

func (bucket *passiveBucket) add(observation PassiveObservation) {
	bucket.requests = saturatingIncrement(bucket.requests)
	switch observation.Outcome {
	case PassiveSucceeded:
		bucket.successes = saturatingIncrement(bucket.successes)
	case PassiveRateLimited:
		bucket.rateLimits = saturatingIncrement(bucket.rateLimits)
	case PassiveServerError:
		bucket.serverErrors = saturatingIncrement(bucket.serverErrors)
	case PassiveTimedOut:
		bucket.timeouts = saturatingIncrement(bucket.timeouts)
	case PassiveCancelled:
		bucket.cancelled = saturatingIncrement(bucket.cancelled)
	case PassiveOtherFailure:
		bucket.otherFailure = saturatingIncrement(bucket.otherFailure)
	}
	if observation.FirstTokenLatency != nil {
		bucket.firstToken.add(*observation.FirstTokenLatency)
	}
	bucket.total.add(observation.TotalLatency)
}

func (bucket *passiveBucket) merge(other passiveBucket) {
	bucket.requests = saturatingSum(bucket.requests, other.requests)
	bucket.successes = saturatingSum(bucket.successes, other.successes)
	bucket.rateLimits = saturatingSum(bucket.rateLimits, other.rateLimits)
	bucket.serverErrors = saturatingSum(bucket.serverErrors, other.serverErrors)
	bucket.timeouts = saturatingSum(bucket.timeouts, other.timeouts)
	bucket.cancelled = saturatingSum(bucket.cancelled, other.cancelled)
	bucket.otherFailure = saturatingSum(bucket.otherFailure, other.otherFailure)
	bucket.firstToken.merge(other.firstToken)
	bucket.total.merge(other.total)
}

func (aggregate *latencyAggregate) add(value time.Duration) {
	aggregate.count = saturatingIncrement(aggregate.count)
	// PassiveObservation.Validate rejects negative durations before aggregation.
	aggregate.totalNanos = saturatingSum(aggregate.totalNanos, uint64(value)) //nolint:gosec // validated non-negative duration
	if value > aggregate.maximum {
		aggregate.maximum = value
	}
	index := len(passiveLatencyBounds)
	for candidate, upperBound := range passiveLatencyBounds {
		if value <= upperBound {
			index = candidate
			break
		}
	}
	aggregate.buckets[index] = saturatingIncrement(aggregate.buckets[index])
}

func (aggregate *latencyAggregate) merge(other latencyAggregate) {
	aggregate.count = saturatingSum(aggregate.count, other.count)
	aggregate.totalNanos = saturatingSum(aggregate.totalNanos, other.totalNanos)
	if other.maximum > aggregate.maximum {
		aggregate.maximum = other.maximum
	}
	for index := range aggregate.buckets {
		aggregate.buckets[index] = saturatingSum(aggregate.buckets[index], other.buckets[index])
	}
}

func (aggregate latencyAggregate) statistics() LatencyStatistics {
	statistics := LatencyStatistics{Count: aggregate.count, Maximum: aggregate.maximum}
	if aggregate.count == 0 {
		return statistics
	}
	averageNanos := aggregate.totalNanos / aggregate.count
	if averageNanos > math.MaxInt64 {
		averageNanos = math.MaxInt64
	}
	statistics.Average = time.Duration(averageNanos)
	statistics.P50UpperBound = aggregate.quantileUpperBound(50)
	statistics.P95UpperBound = aggregate.quantileUpperBound(95)
	statistics.P99UpperBound = aggregate.quantileUpperBound(99)
	return statistics
}

func (aggregate latencyAggregate) quantileUpperBound(percentile uint64) time.Duration {
	rank := uint64(math.Ceil(float64(aggregate.count) * float64(percentile) / 100))
	var observed uint64
	for index, count := range aggregate.buckets {
		observed = saturatingSum(observed, count)
		if observed < rank {
			continue
		}
		if index == len(passiveLatencyBounds) {
			return aggregate.maximum
		}
		return passiveLatencyBounds[index]
	}
	return aggregate.maximum
}

func saturatingIncrement(value uint64) uint64 {
	if value == math.MaxUint64 {
		return value
	}
	return value + 1
}

func saturatingSum(values ...uint64) uint64 {
	total := uint64(0)
	for _, value := range values {
		if math.MaxUint64-total < value {
			return math.MaxUint64
		}
		total += value
	}
	return total
}

var _ HealthReader = (*PassiveHealth)(nil)
var _ PassiveObserver = (*PassiveHealth)(nil)
