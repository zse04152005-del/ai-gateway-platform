// Package limits provides process-local admission control primitives.
package limits

import (
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/limitpolicy"
)

const (
	defaultMaximumBindings = 100_000
	requiredScopeCount     = 4
)

var (
	// ErrInvalid means limiter configuration or admission input is unsafe.
	ErrInvalid = errors.New("local limit input is invalid")
	// ErrStaleSnapshot means a configuration version did not advance.
	ErrStaleSnapshot = errors.New("local limit snapshot is stale")
	// ErrPolicyUnavailable means admission cannot find every required scope.
	ErrPolicyUnavailable = errors.New("local limit policy is unavailable")

	localUUIDPattern = regexp.MustCompile(
		`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`,
	)
)

// ScopeKind identifies one aggregate independently protected in this process.
type ScopeKind string

const (
	// ScopePlatform aggregates all admitted traffic in the process.
	ScopePlatform ScopeKind = "platform"
	// ScopeTenant aggregates traffic for one Tenant.
	ScopeTenant ScopeKind = "tenant"
	// ScopeProject aggregates traffic for one Project within its Tenant.
	ScopeProject ScopeKind = "project"
	// ScopeKey aggregates traffic for one VirtualKey within its Project.
	ScopeKey ScopeKind = "key"
)

// Resource identifies the boundary responsible for a soft event or rejection.
type Resource string

const (
	// ResourceRPM counts admitted requests in the current minute window.
	ResourceRPM Resource = "rpm"
	// ResourceTPM counts reserved tokens in the current minute window.
	ResourceTPM Resource = "tpm"
	// ResourceConcurrency counts currently held admission leases.
	ResourceConcurrency Resource = "concurrency"
)

// Scope is a comparable, tenant-qualified counter identity.
type Scope struct {
	Kind      ScopeKind
	TenantID  string
	ProjectID string
	KeyID     string
}

// Binding assigns one fully resolved policy to one counter scope.
type Binding struct {
	Scope  Scope
	Policy limitpolicy.Effective
}

// Options bounds process-local configuration memory.
type Options struct {
	MaximumBindings int
}

// DefaultOptions returns production-safe local limiter bounds.
func DefaultOptions() Options {
	return Options{MaximumBindings: defaultMaximumBindings}
}

// Request is one atomic admission across Platform, Tenant, Project and Key.
type Request struct {
	Scopes          []Scope
	EstimatedTokens uint64
}

// SoftThreshold reports admitted usage beyond a configured soft boundary.
type SoftThreshold struct {
	Scope     Scope
	Resource  Resource
	Usage     uint64
	Threshold uint64
}

// Rejection explains the first deterministic hard boundary that denied work.
type Rejection struct {
	Scope      Scope
	Resource   Resource
	Usage      uint64
	Requested  uint64
	Hard       uint64
	RetryAfter time.Duration
	ResetAt    time.Time
}

// Admission contains either a lease or one hard rejection. Rate counters are
// consumed only when all four scopes admit atomically.
type Admission struct {
	SnapshotVersion uint64
	Lease           *Lease
	SoftThresholds  []SoftThreshold
	Rejection       *Rejection
}

// Allowed reports whether the caller owns a concurrency lease.
func (admission Admission) Allowed() bool {
	return admission.Lease != nil && admission.Rejection == nil
}

// UsageSnapshot is a content-free view of one process-local counter.
type UsageSnapshot struct {
	SnapshotVersion uint64
	Scope           Scope
	RPM             uint64
	TPM             uint64
	Concurrency     uint64
	WindowStart     time.Time
	ResetAt         time.Time
	Policy          limitpolicy.Effective
}

// LocalLimiter atomically protects one process. Replace hot-swaps a complete
// immutable policy map while retaining live counters for unchanged scopes.
type LocalLimiter struct {
	mu              sync.Mutex
	now             func() time.Time
	maximumBindings int
	version         uint64
	policies        map[Scope]limitpolicy.Effective
	states          map[Scope]*localState
}

type localState struct {
	windowStart time.Time
	rpm         uint64
	tpm         uint64
	concurrency uint64
}

// Lease owns one concurrency slot in every admitted scope. Release is
// idempotent and safe to call concurrently.
type Lease struct {
	once    sync.Once
	limiter *LocalLimiter
	scopes  [requiredScopeCount]Scope
}

// NewLocalLimiter creates an unconfigured process-local limiter.
func NewLocalLimiter(options Options, now func() time.Time) (*LocalLimiter, error) {
	if options.MaximumBindings < requiredScopeCount || now == nil {
		return nil, ErrInvalid
	}
	return &LocalLimiter{
		now:             now,
		maximumBindings: options.MaximumBindings,
		policies:        make(map[Scope]limitpolicy.Effective),
		states:          make(map[Scope]*localState),
	}, nil
}

// Replace atomically publishes a strictly newer complete configuration.
func (limiter *LocalLimiter) Replace(version uint64, bindings []Binding) error {
	if limiter == nil || version == 0 || len(bindings) == 0 || len(bindings) > limiter.maximumBindings {
		return ErrInvalid
	}
	next := make(map[Scope]limitpolicy.Effective, len(bindings))
	for _, binding := range bindings {
		if binding.Scope.Validate() != nil || binding.Policy.Validate() != nil {
			return ErrInvalid
		}
		if _, exists := next[binding.Scope]; exists {
			return ErrInvalid
		}
		next[binding.Scope] = binding.Policy
	}

	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if version <= limiter.version {
		return ErrStaleSnapshot
	}
	for scope, state := range limiter.states {
		if _, configured := next[scope]; !configured && state.concurrency == 0 {
			delete(limiter.states, scope)
		}
	}
	limiter.policies = next
	limiter.version = version
	return nil
}

// Version returns the currently published configuration version, or zero
// before the first successful Replace.
func (limiter *LocalLimiter) Version() uint64 {
	if limiter == nil {
		return 0
	}
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	return limiter.version
}

// Acquire performs one non-blocking, all-or-nothing admission decision.
func (limiter *LocalLimiter) Acquire(request Request) (Admission, error) {
	if limiter == nil || request.EstimatedTokens == 0 || request.EstimatedTokens > limitpolicy.MaximumValue {
		return Admission{}, ErrInvalid
	}
	scopes, err := normalizeScopes(request.Scopes)
	if err != nil {
		return Admission{}, err
	}

	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if limiter.version == 0 {
		return Admission{}, ErrPolicyUnavailable
	}

	var policies [requiredScopeCount]limitpolicy.Effective
	for index, scope := range scopes {
		policy, exists := limiter.policies[scope]
		if !exists {
			return Admission{}, fmt.Errorf("%w: missing %s scope", ErrPolicyUnavailable, scope.Kind)
		}
		policies[index] = policy
	}

	now := limiter.now()
	windowStart := now.UTC().Truncate(time.Minute)
	var states [requiredScopeCount]*localState
	for index, scope := range scopes {
		state := limiter.states[scope]
		if state == nil {
			state = &localState{windowStart: windowStart}
			limiter.states[scope] = state
		} else if windowStart.After(state.windowStart) {
			state.windowStart = windowStart
			state.rpm = 0
			state.tpm = 0
		}
		states[index] = state
	}

	for index, scope := range scopes {
		if rejection := checkHardLimits(
			now,
			scope,
			states[index],
			policies[index],
			request.EstimatedTokens,
		); rejection != nil {
			return Admission{SnapshotVersion: limiter.version, Rejection: rejection}, nil
		}
	}

	var softThresholds []SoftThreshold
	for index, scope := range scopes {
		state := states[index]
		policy := policies[index]
		state.rpm++
		state.tpm += request.EstimatedTokens
		state.concurrency++
		softThresholds = appendSoftThresholds(softThresholds, scope, state, policy)
	}
	return Admission{
		SnapshotVersion: limiter.version,
		Lease:           &Lease{limiter: limiter, scopes: scopes},
		SoftThresholds:  softThresholds,
	}, nil
}

// Snapshot returns current counters without consuming admission capacity.
func (limiter *LocalLimiter) Snapshot(scope Scope) (UsageSnapshot, error) {
	if limiter == nil || scope.Validate() != nil {
		return UsageSnapshot{}, ErrInvalid
	}
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	policy, configured := limiter.policies[scope]
	if !configured {
		return UsageSnapshot{}, ErrPolicyUnavailable
	}
	now := limiter.now()
	windowStart := now.UTC().Truncate(time.Minute)
	state := limiter.states[scope]
	if state == nil {
		return UsageSnapshot{
			SnapshotVersion: limiter.version,
			Scope:           scope,
			WindowStart:     windowStart,
			ResetAt:         windowStart.Add(time.Minute),
			Policy:          policy,
		}, nil
	}
	if windowStart.After(state.windowStart) {
		state.windowStart = windowStart
		state.rpm = 0
		state.tpm = 0
	}
	return UsageSnapshot{
		SnapshotVersion: limiter.version,
		Scope:           scope,
		RPM:             state.rpm,
		TPM:             state.tpm,
		Concurrency:     state.concurrency,
		WindowStart:     state.windowStart,
		ResetAt:         state.windowStart.Add(time.Minute),
		Policy:          policy,
	}, nil
}

// Validate checks that a Scope contains exactly the identifiers required by
// its kind.
func (scope Scope) Validate() error {
	switch scope.Kind {
	case ScopePlatform:
		if scope.TenantID != "" || scope.ProjectID != "" || scope.KeyID != "" {
			return ErrInvalid
		}
	case ScopeTenant:
		if !validLocalUUID(scope.TenantID) || scope.ProjectID != "" || scope.KeyID != "" {
			return ErrInvalid
		}
	case ScopeProject:
		if !validLocalUUID(scope.TenantID) || !validLocalUUID(scope.ProjectID) || scope.KeyID != "" {
			return ErrInvalid
		}
	case ScopeKey:
		if !validLocalUUID(scope.TenantID) || !validLocalUUID(scope.ProjectID) || !validLocalUUID(scope.KeyID) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

// Release relinquishes the concurrency portion of an admission. RPM and TPM
// remain consumed for their minute window.
func (lease *Lease) Release() {
	if lease == nil {
		return
	}
	lease.once.Do(func() {
		if lease.limiter != nil {
			lease.limiter.release(lease.scopes)
		}
	})
}

func (limiter *LocalLimiter) release(scopes [requiredScopeCount]Scope) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	for _, scope := range scopes {
		state := limiter.states[scope]
		if state == nil || state.concurrency == 0 {
			continue
		}
		state.concurrency--
		if _, configured := limiter.policies[scope]; !configured && state.concurrency == 0 {
			delete(limiter.states, scope)
		}
	}
}

func normalizeScopes(input []Scope) ([requiredScopeCount]Scope, error) {
	var normalized [requiredScopeCount]Scope
	if len(input) != requiredScopeCount {
		return normalized, ErrInvalid
	}
	var seen [requiredScopeCount]bool
	for _, scope := range input {
		if scope.Validate() != nil {
			return [requiredScopeCount]Scope{}, ErrInvalid
		}
		rank := scopeRank(scope.Kind)
		if rank < 0 || rank >= requiredScopeCount || seen[rank] {
			return [requiredScopeCount]Scope{}, ErrInvalid
		}
		normalized[rank] = scope
		seen[rank] = true
	}
	if normalized[0].Kind != ScopePlatform ||
		normalized[1].Kind != ScopeTenant ||
		normalized[2].Kind != ScopeProject ||
		normalized[3].Kind != ScopeKey ||
		normalized[1].TenantID != normalized[2].TenantID ||
		normalized[1].TenantID != normalized[3].TenantID ||
		normalized[2].ProjectID != normalized[3].ProjectID {
		return [requiredScopeCount]Scope{}, ErrInvalid
	}
	return normalized, nil
}

func checkHardLimits(
	now time.Time,
	scope Scope,
	state *localState,
	policy limitpolicy.Effective,
	estimatedTokens uint64,
) *Rejection {
	if exceeds(state.rpm, 1, policy.RPM.Hard) {
		return rateRejection(now, scope, ResourceRPM, state.rpm, 1, policy.RPM.Hard, state.windowStart)
	}
	if exceeds(state.tpm, estimatedTokens, policy.TPM.Hard) {
		return rateRejection(
			now,
			scope,
			ResourceTPM,
			state.tpm,
			estimatedTokens,
			policy.TPM.Hard,
			state.windowStart,
		)
	}
	if exceeds(state.concurrency, 1, policy.Concurrency.Hard) {
		return &Rejection{
			Scope: scope, Resource: ResourceConcurrency,
			Usage: state.concurrency, Requested: 1, Hard: policy.Concurrency.Hard,
		}
	}
	return nil
}

func rateRejection(
	now time.Time,
	scope Scope,
	resource Resource,
	usage uint64,
	requested uint64,
	hard uint64,
	windowStart time.Time,
) *Rejection {
	resetAt := windowStart.Add(time.Minute)
	retryAfter := resetAt.Sub(now)
	if retryAfter < 0 {
		retryAfter = 0
	}
	return &Rejection{
		Scope: scope, Resource: resource, Usage: usage, Requested: requested, Hard: hard,
		RetryAfter: retryAfter, ResetAt: resetAt,
	}
}

func appendSoftThresholds(
	thresholds []SoftThreshold,
	scope Scope,
	state *localState,
	policy limitpolicy.Effective,
) []SoftThreshold {
	if state.rpm > policy.RPM.Soft {
		thresholds = append(thresholds, SoftThreshold{
			Scope: scope, Resource: ResourceRPM, Usage: state.rpm, Threshold: policy.RPM.Soft,
		})
	}
	if state.tpm > policy.TPM.Soft {
		thresholds = append(thresholds, SoftThreshold{
			Scope: scope, Resource: ResourceTPM, Usage: state.tpm, Threshold: policy.TPM.Soft,
		})
	}
	if state.concurrency > policy.Concurrency.Soft {
		thresholds = append(thresholds, SoftThreshold{
			Scope: scope, Resource: ResourceConcurrency,
			Usage: state.concurrency, Threshold: policy.Concurrency.Soft,
		})
	}
	return thresholds
}

func exceeds(current, requested, hard uint64) bool {
	return requested > hard || current > hard-requested
}

func scopeRank(kind ScopeKind) int {
	switch kind {
	case ScopePlatform:
		return 0
	case ScopeTenant:
		return 1
	case ScopeProject:
		return 2
	case ScopeKey:
		return 3
	default:
		return requiredScopeCount
	}
}

func validLocalUUID(value string) bool {
	return localUUIDPattern.MatchString(value)
}
