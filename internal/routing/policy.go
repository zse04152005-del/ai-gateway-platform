package routing

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"sync"

	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
)

const (
	bootstrapPriorityPolicyVersion = "bootstrap-priority/v1"
	seedDiversifier                = uint64(0x9e3779b97f4a7c15)
)

var (
	// ErrPolicyUnavailable means no trustworthy routing policy could be resolved.
	ErrPolicyUnavailable = errors.New("route policy unavailable")
	// ErrRandomUnavailable means weighted selection could not obtain a valid draw.
	ErrRandomUnavailable      = errors.New("route random source unavailable")
	routePolicyVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
	routeDeploymentIDPattern  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

// RouteMode identifies one finite first-attempt selection algorithm.
type RouteMode string

const (
	// RouteFixed selects one exact eligible deployment and never substitutes another.
	RouteFixed RouteMode = "fixed"
	// RoutePriority selects the first eligible candidate in stable priority order.
	RoutePriority RouteMode = "priority"
	// RouteWeighted selects across all eligible candidates by binding weight.
	RouteWeighted RouteMode = "weighted"
)

// RoutePolicy is one immutable published selection-policy fact.
type RoutePolicy struct {
	Version           string
	Mode              RouteMode
	FixedDeploymentID string
}

// Validate rejects ambiguous or malformed policy facts.
func (policy RoutePolicy) Validate() error {
	if !routePolicyVersionPattern.MatchString(policy.Version) {
		return errors.New("route policy version must be a canonical 1-128 character identifier")
	}
	switch policy.Mode {
	case RouteFixed:
		if !routeDeploymentIDPattern.MatchString(policy.FixedDeploymentID) {
			return errors.New("fixed route policy requires a canonical deployment UUID")
		}
	case RoutePriority, RouteWeighted:
		if policy.FixedDeploymentID != "" {
			return errors.New("non-fixed route policy must not contain a fixed deployment")
		}
	default:
		return errors.New("route policy mode is unsupported")
	}
	return nil
}

// PolicyRequest is the content-free scope used to resolve a published policy.
type PolicyRequest struct {
	TenantID     string
	ProjectID    string
	LogicalModel string
}

// PolicyResolver returns the immutable policy for one trusted request scope.
type PolicyResolver interface {
	Resolve(context.Context, PolicyRequest) (RoutePolicy, error)
}

// StaticPolicyResolver supplies one validated policy for bootstrap and tests.
type StaticPolicyResolver struct {
	policy RoutePolicy
}

// NewStaticPolicyResolver validates the policy before publishing it to callers.
func NewStaticPolicyResolver(policy RoutePolicy) (*StaticPolicyResolver, error) {
	if err := policy.Validate(); err != nil {
		return nil, fmt.Errorf("validate static route policy: %w", err)
	}
	return &StaticPolicyResolver{policy: policy}, nil
}

// Resolve returns the immutable static policy unless the request is cancelled.
func (resolver *StaticPolicyResolver) Resolve(ctx context.Context, _ PolicyRequest) (RoutePolicy, error) {
	if resolver == nil {
		return RoutePolicy{}, errors.New("static route policy resolver is not initialized")
	}
	if ctx == nil {
		return RoutePolicy{}, errors.New("route policy context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return RoutePolicy{}, err
	}
	return resolver.policy, nil
}

// RandomSource returns an unbiased draw in [0, upperBound).
type RandomSource interface {
	Uint64N(upperBound uint64) (uint64, error)
}

// SeededRandom is a deterministic, concurrency-safe injectable random source.
type SeededRandom struct {
	mutex  sync.Mutex
	random *rand.Rand
}

// NewSeededRandom derives a reproducible PCG stream from one caller seed.
func NewSeededRandom(seed uint64) *SeededRandom {
	// This deterministic stream is for traffic distribution and repeatable tests,
	// never credentials, tokens, nonces, or another security decision.
	return &SeededRandom{random: rand.New(rand.NewPCG(seed, seed^seedDiversifier))} //nolint:gosec // non-security load distribution
}

// Uint64N returns the next deterministic draw.
func (source *SeededRandom) Uint64N(upperBound uint64) (uint64, error) {
	if source == nil || source.random == nil {
		return 0, errors.New("seeded random source is not initialized")
	}
	if upperBound == 0 {
		return 0, errors.New("random upper bound must be positive")
	}
	source.mutex.Lock()
	draw := source.random.Uint64N(upperBound)
	source.mutex.Unlock()
	return draw, nil
}

// PolicyDecision is the safe, content-free explanation for the chosen candidate.
type PolicyDecision struct {
	PolicyVersion        string    `json:"policy_version"`
	Mode                 RouteMode `json:"mode"`
	SelectedDeploymentID string    `json:"selected_deployment_id"`
	Priority             int16     `json:"priority"`
	Weight               int16     `json:"weight"`
	EligibleCount        int       `json:"eligible_count"`
	TotalWeight          uint64    `json:"total_weight,omitempty"`
	RandomDraw           *uint64   `json:"random_draw,omitempty"`
}

// Clone returns an alias-free decision.
func (decision PolicyDecision) Clone() PolicyDecision {
	cloned := decision
	if decision.RandomDraw != nil {
		draw := *decision.RandomDraw
		cloned.RandomDraw = &draw
	}
	return cloned
}

type bootstrapPolicyResolver struct{}

func (bootstrapPolicyResolver) Resolve(ctx context.Context, _ PolicyRequest) (RoutePolicy, error) {
	if ctx == nil {
		return RoutePolicy{}, errors.New("route policy context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return RoutePolicy{}, err
	}
	return RoutePolicy{Version: bootstrapPriorityPolicyVersion, Mode: RoutePriority}, nil
}

type systemRandom struct{}

func (systemRandom) Uint64N(upperBound uint64) (uint64, error) {
	if upperBound == 0 {
		return 0, errors.New("random upper bound must be positive")
	}
	// This draw distributes traffic and is not used for a security decision.
	return rand.Uint64N(upperBound), nil //nolint:gosec // non-security load distribution
}

func selectByPolicy(candidates []catalog.RouteCandidate, policy RoutePolicy, random RandomSource) (catalog.RouteCandidate, PolicyDecision, error) {
	if len(candidates) == 0 {
		return catalog.RouteCandidate{}, PolicyDecision{}, ErrNoCandidate
	}
	switch policy.Mode {
	case RouteFixed:
		return selectFixed(candidates, policy)
	case RoutePriority:
		candidate, decision := selectedCandidate(candidates[0], policy, len(candidates), 0, nil)
		return candidate, decision, nil
	case RouteWeighted:
		return selectWeighted(candidates, policy, random)
	default:
		return catalog.RouteCandidate{}, PolicyDecision{}, fmt.Errorf("%w: unsupported mode", ErrPolicyUnavailable)
	}
}

func selectFixed(candidates []catalog.RouteCandidate, policy RoutePolicy) (catalog.RouteCandidate, PolicyDecision, error) {
	for _, candidate := range candidates {
		if candidate.Deployment.ID == policy.FixedDeploymentID {
			selected, decision := selectedCandidate(candidate, policy, len(candidates), 0, nil)
			return selected, decision, nil
		}
	}
	return catalog.RouteCandidate{}, PolicyDecision{}, ErrNoCandidate
}

func selectWeighted(candidates []catalog.RouteCandidate, policy RoutePolicy, random RandomSource) (catalog.RouteCandidate, PolicyDecision, error) {
	if random == nil {
		return catalog.RouteCandidate{}, PolicyDecision{}, errors.New("route random source must not be nil")
	}
	var totalWeight uint64
	weights := make([]uint64, len(candidates))
	for index, candidate := range candidates {
		if candidate.Binding.Weight <= 0 {
			return catalog.RouteCandidate{}, PolicyDecision{}, fmt.Errorf("%w: binding weight must be positive", ErrPolicyUnavailable)
		}
		weight := uint64(candidate.Binding.Weight) //nolint:gosec // positive int16 was checked immediately above
		weights[index] = weight
		if ^uint64(0)-totalWeight < weight {
			return catalog.RouteCandidate{}, PolicyDecision{}, fmt.Errorf("%w: weight sum overflow", ErrPolicyUnavailable)
		}
		totalWeight += weight
	}
	if totalWeight == 0 {
		return catalog.RouteCandidate{}, PolicyDecision{}, fmt.Errorf("%w: weight sum is zero", ErrPolicyUnavailable)
	}
	if len(candidates) == 1 {
		candidate, decision := selectedCandidate(candidates[0], policy, 1, totalWeight, nil)
		return candidate, decision, nil
	}
	draw, err := random.Uint64N(totalWeight)
	if err != nil {
		return catalog.RouteCandidate{}, PolicyDecision{}, fmt.Errorf("%w: %w", ErrRandomUnavailable, err)
	}
	if draw >= totalWeight {
		return catalog.RouteCandidate{}, PolicyDecision{}, fmt.Errorf("%w: draw outside requested bound", ErrRandomUnavailable)
	}
	cumulative := uint64(0)
	for index, candidate := range candidates {
		cumulative += weights[index]
		if draw < cumulative {
			selected, decision := selectedCandidate(candidate, policy, len(candidates), totalWeight, &draw)
			return selected, decision, nil
		}
	}
	return catalog.RouteCandidate{}, PolicyDecision{}, fmt.Errorf("%w: no weighted interval matched", ErrPolicyUnavailable)
}

func selectedCandidate(
	candidate catalog.RouteCandidate,
	policy RoutePolicy,
	eligibleCount int,
	totalWeight uint64,
	randomDraw *uint64,
) (catalog.RouteCandidate, PolicyDecision) {
	return candidate.Clone(), PolicyDecision{
		PolicyVersion: policy.Version, Mode: policy.Mode,
		SelectedDeploymentID: candidate.Deployment.ID,
		Priority:             candidate.Binding.Priority, Weight: candidate.Binding.Weight,
		EligibleCount: eligibleCount, TotalWeight: totalWeight, RandomDraw: randomDraw,
	}
}
