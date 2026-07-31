package routing

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
)

const (
	candidateFilterPolicyVersion = "candidate-filter/v1"
	maximumExcludedDeployments   = 32
)

var (
	// ErrBudgetUnavailable means the budget dependency could not decide safely.
	ErrBudgetUnavailable = errors.New("route budget unavailable")
	// ErrCapacityUnavailable means the capacity dependency could not decide safely.
	ErrCapacityUnavailable = errors.New("route capacity unavailable")
)

// FilterReason is the finite, safe explanation for one candidate decision.
type FilterReason string

const (
	// FilterEligible means the candidate passed every filter.
	FilterEligible FilterReason = "eligible"
	// FilterTenantNotAllowed means the trusted key scope excludes the logical model.
	FilterTenantNotAllowed FilterReason = "tenant_not_allowed"
	// FilterCapabilityMissing means the deployment cannot satisfy the model or request contract.
	FilterCapabilityMissing FilterReason = "capability_missing"
	// FilterRegionNotAllowed means the deployment is outside the model's allowed regions.
	FilterRegionNotAllowed FilterReason = "region_not_allowed"
	// FilterInactive means at least one catalog record is disabled.
	FilterInactive FilterReason = "inactive"
	// FilterPreviouslyAttempted means request-scoped failover excludes this deployment.
	FilterPreviouslyAttempted FilterReason = "previously_attempted"
	// FilterUnhealthy means the current health view rejects the deployment.
	FilterUnhealthy FilterReason = "unhealthy"
	// FilterBudgetDenied means the request is outside its current budget envelope.
	FilterBudgetDenied FilterReason = "budget_denied"
	// FilterCapacityUnavailable means the deployment has no admissible capacity.
	FilterCapacityUnavailable FilterReason = "capacity_unavailable"
)

// EligibilityRequest is the minimal, secret-free input used by budget and
// capacity policies. It intentionally excludes endpoints, credentials, and content.
type EligibilityRequest struct {
	TenantID        string
	ProjectID       string
	LogicalModel    string
	DeploymentID    string
	ProviderID      string
	Stream          bool
	MaxOutputTokens *int64
}

// BudgetReader evaluates the current budget envelope without reserving funds.
type BudgetReader interface {
	Eligible(context.Context, EligibilityRequest) (bool, error)
}

// CapacityReader evaluates current admission capacity without starting an attempt.
type CapacityReader interface {
	Eligible(context.Context, EligibilityRequest) (bool, error)
}

// CandidateDecision is safe to record or expose to an authorized diagnostics API.
type CandidateDecision struct {
	DeploymentID string       `json:"deployment_id"`
	Eligible     bool         `json:"eligible"`
	Reason       FilterReason `json:"reason"`
}

// FilterResult is one deterministic policy evaluation. Eligible catalog facts
// remain private to routing; callers query only the safe Decisions projection.
type FilterResult struct {
	PolicyVersion string              `json:"policy_version"`
	Decisions     []CandidateDecision `json:"decisions"`
	eligible      []catalog.RouteCandidate
}

// DecisionFor returns the recorded decision for one deployment.
func (result FilterResult) DecisionFor(deploymentID string) (CandidateDecision, bool) {
	for _, decision := range result.Decisions {
		if decision.DeploymentID == deploymentID {
			return decision, true
		}
	}
	return CandidateDecision{}, false
}

// Clone returns an alias-free result for storage or asynchronous observation.
func (result FilterResult) Clone() FilterResult {
	cloned := FilterResult{
		PolicyVersion: result.PolicyVersion,
		Decisions:     append([]CandidateDecision(nil), result.Decisions...),
		eligible:      make([]catalog.RouteCandidate, 0, len(result.eligible)),
	}
	for _, candidate := range result.eligible {
		cloned.eligible = append(cloned.eligible, candidate.Clone())
	}
	return cloned
}

// ValidateExplanation checks the bounded, content-free projection used by the
// durable route-decision store. Eligible catalog facts remain intentionally
// outside this validation boundary.
func (result FilterResult) ValidateExplanation() error {
	if !routePolicyVersionPattern.MatchString(result.PolicyVersion) || len(result.Decisions) > maximumCandidates {
		return errors.New("route filter explanation is invalid")
	}
	seen := make(map[string]struct{}, len(result.Decisions))
	for _, decision := range result.Decisions {
		if !routeDeploymentIDPattern.MatchString(decision.DeploymentID) || !validFilterReason(decision.Reason) ||
			(decision.Eligible != (decision.Reason == FilterEligible)) {
			return errors.New("route candidate explanation is invalid")
		}
		if _, duplicate := seen[decision.DeploymentID]; duplicate {
			return errors.New("route candidate explanation contains a duplicate deployment")
		}
		seen[decision.DeploymentID] = struct{}{}
	}
	return nil
}

func validFilterReason(reason FilterReason) bool {
	switch reason {
	case FilterEligible, FilterTenantNotAllowed, FilterCapabilityMissing, FilterRegionNotAllowed,
		FilterInactive, FilterPreviouslyAttempted, FilterUnhealthy, FilterBudgetDenied, FilterCapacityUnavailable:
		return true
	default:
		return false
	}
}

// CandidateFilter applies the mandatory filters in a fixed, first-failure order.
// It is immutable after construction and safe for concurrent use when its readers are.
type CandidateFilter struct {
	source   CandidateSource
	health   HealthReader
	budget   BudgetReader
	capacity CapacityReader
}

// NewCandidateFilter validates every mandatory dependency.
func NewCandidateFilter(source CandidateSource, health HealthReader, budget BudgetReader, capacity CapacityReader) (*CandidateFilter, error) {
	if source == nil {
		return nil, errors.New("route candidate source must not be nil")
	}
	if health == nil {
		return nil, errors.New("route health reader must not be nil")
	}
	if budget == nil {
		return nil, errors.New("route budget reader must not be nil")
	}
	if capacity == nil {
		return nil, errors.New("route capacity reader must not be nil")
	}
	return &CandidateFilter{source: source, health: health, budget: budget, capacity: capacity}, nil
}

// Filter queries and evaluates candidates. Only the first failed rule is
// recorded for each candidate, so explanations cannot vary with later readers.
func (filter *CandidateFilter) Filter(ctx context.Context, request SelectionRequest) (FilterResult, error) {
	result := FilterResult{PolicyVersion: candidateFilterPolicyVersion}
	if filter == nil || filter.source == nil || filter.health == nil || filter.budget == nil || filter.capacity == nil {
		return result, errors.New("route candidate filter is not initialized")
	}
	if ctx == nil {
		return result, errors.New("route filtering context must not be nil")
	}
	if err := request.Request.Validate(); err != nil {
		return result, fmt.Errorf("validate route request: %w", err)
	}
	excluded, err := excludedDeploymentSet(request.ExcludedDeploymentIDs)
	if err != nil {
		return result, err
	}
	request.Access.KeyAllowedModels = cloneStrings(request.Access.KeyAllowedModels)
	request.ExcludedDeploymentIDs = append([]string(nil), request.ExcludedDeploymentIDs...)
	candidates, err := filter.source.ListRouteCandidates(ctx, catalog.RouteQuery{
		Access: request.Access, LogicalModel: request.Request.LogicalModel,
	})
	if err != nil {
		return result, fmt.Errorf("%w: %w", ErrCandidateSource, err)
	}
	if len(candidates) > maximumCandidates {
		return result, fmt.Errorf("%w: candidate limit exceeded", ErrCandidateSource)
	}

	sort.SliceStable(candidates, func(left, right int) bool {
		return candidateLess(candidates[left], candidates[right])
	})
	result.Decisions = make([]CandidateDecision, 0, len(candidates))
	result.eligible = make([]catalog.RouteCandidate, 0, len(candidates))
	requiredCapabilities := requiredRequestCapabilities(request.Request)
	seenDeployments := make(map[string]struct{}, len(candidates))
	for index := range candidates {
		candidate := candidates[index]
		if err := validateCandidateFacts(request, candidate); err != nil {
			return FilterResult{PolicyVersion: candidateFilterPolicyVersion}, fmt.Errorf("%w: invalid catalog candidate", ErrCandidateSource)
		}
		if _, exists := seenDeployments[candidate.Deployment.ID]; exists {
			return FilterResult{PolicyVersion: candidateFilterPolicyVersion}, fmt.Errorf("%w: duplicate deployment candidate", ErrCandidateSource)
		}
		seenDeployments[candidate.Deployment.ID] = struct{}{}

		decision, dependencyErr := filter.evaluateCandidate(ctx, request, requiredCapabilities, candidate, excluded)
		if dependencyErr != nil {
			return result.Clone(), dependencyErr
		}
		result.Decisions = append(result.Decisions, decision)
		if decision.Eligible {
			result.eligible = append(result.eligible, candidate.Clone())
		}
	}
	return result.Clone(), nil
}

func (filter *CandidateFilter) evaluateCandidate(
	ctx context.Context,
	request SelectionRequest,
	requiredCapabilities catalog.CapabilityRequirements,
	candidate catalog.RouteCandidate,
	excluded map[string]struct{},
) (CandidateDecision, error) {
	decision := CandidateDecision{DeploymentID: candidate.Deployment.ID}
	if !modelAllowed(request.Access.KeyAllowedModels, request.Request.LogicalModel) {
		decision.Reason = FilterTenantNotAllowed
		return decision, nil
	}
	if !candidate.Deployment.Capabilities.Satisfies(candidate.LogicalModel.RequiredCapabilities) ||
		!candidate.Deployment.Capabilities.Satisfies(requiredCapabilities) {
		decision.Reason = FilterCapabilityMissing
		return decision, nil
	}
	if !regionAllowed(candidate.LogicalModel.AllowedRegions, candidate.Deployment.Region) {
		decision.Reason = FilterRegionNotAllowed
		return decision, nil
	}
	if candidate.LogicalModel.Status != catalog.StatusActive || candidate.Binding.Status != catalog.StatusActive ||
		candidate.Deployment.Status != catalog.StatusActive || candidate.Provider.Status != catalog.StatusActive {
		decision.Reason = FilterInactive
		return decision, nil
	}
	if _, attempted := excluded[candidate.Deployment.ID]; attempted {
		decision.Reason = FilterPreviouslyAttempted
		return decision, nil
	}

	healthy, err := filter.health.Healthy(ctx, candidate.Deployment.ID)
	if err != nil {
		return CandidateDecision{}, fmt.Errorf("%w: %w", ErrHealthUnavailable, err)
	}
	if !healthy {
		decision.Reason = FilterUnhealthy
		return decision, nil
	}
	eligibility := eligibilityRequest(request, candidate)
	withinBudget, err := filter.budget.Eligible(ctx, cloneEligibilityRequest(eligibility))
	if err != nil {
		return CandidateDecision{}, fmt.Errorf("%w: %w", ErrBudgetUnavailable, err)
	}
	if !withinBudget {
		decision.Reason = FilterBudgetDenied
		return decision, nil
	}
	hasCapacity, err := filter.capacity.Eligible(ctx, cloneEligibilityRequest(eligibility))
	if err != nil {
		return CandidateDecision{}, fmt.Errorf("%w: %w", ErrCapacityUnavailable, err)
	}
	if !hasCapacity {
		decision.Reason = FilterCapacityUnavailable
		return decision, nil
	}
	decision.Eligible = true
	decision.Reason = FilterEligible
	return decision, nil
}

func excludedDeploymentSet(deploymentIDs []string) (map[string]struct{}, error) {
	if len(deploymentIDs) > maximumExcludedDeployments {
		return nil, fmt.Errorf("route exclusion limit exceeded")
	}
	excluded := make(map[string]struct{}, len(deploymentIDs))
	for _, deploymentID := range deploymentIDs {
		if !routeDeploymentIDPattern.MatchString(deploymentID) {
			return nil, fmt.Errorf("route exclusion contains an invalid deployment ID")
		}
		if _, duplicate := excluded[deploymentID]; duplicate {
			return nil, fmt.Errorf("route exclusion contains a duplicate deployment ID")
		}
		excluded[deploymentID] = struct{}{}
	}
	return excluded, nil
}

func validateCandidateFacts(request SelectionRequest, candidate catalog.RouteCandidate) error {
	for _, validation := range []func() error{
		candidate.LogicalModel.Validate,
		candidate.Binding.Validate,
		candidate.Deployment.Validate,
		candidate.Provider.Validate,
	} {
		if err := validation(); err != nil {
			return err
		}
	}
	if candidate.Binding.LogicalModelID != candidate.LogicalModel.ID ||
		candidate.Binding.DeploymentID != candidate.Deployment.ID ||
		candidate.Deployment.ProviderID != candidate.Provider.ID {
		return errors.New("route candidate relationship mismatch")
	}
	if candidate.LogicalModel.TenantID != request.Access.TenantID {
		return errors.New("route candidate tenant mismatch")
	}
	if candidate.LogicalModel.Name != request.Request.LogicalModel {
		return errors.New("route candidate logical model mismatch")
	}
	return nil
}

func modelAllowed(allowedModels *[]string, logicalModel string) bool {
	if allowedModels == nil {
		return true
	}
	for _, allowed := range *allowedModels {
		if allowed == logicalModel {
			return true
		}
	}
	return false
}

func regionAllowed(allowedRegions *[]string, region string) bool {
	if allowedRegions == nil {
		return true
	}
	for _, allowed := range *allowedRegions {
		if allowed == region {
			return true
		}
	}
	return false
}

func eligibilityRequest(request SelectionRequest, candidate catalog.RouteCandidate) EligibilityRequest {
	return EligibilityRequest{
		TenantID: request.Access.TenantID, ProjectID: request.Access.ProjectID,
		LogicalModel: request.Request.LogicalModel, DeploymentID: candidate.Deployment.ID,
		ProviderID: candidate.Provider.ID, Stream: request.Request.Stream,
		MaxOutputTokens: cloneInt64(request.Request.MaxOutputTokens),
	}
}

func cloneEligibilityRequest(request EligibilityRequest) EligibilityRequest {
	request.MaxOutputTokens = cloneInt64(request.MaxOutputTokens)
	return request
}

func cloneStrings(values *[]string) *[]string {
	if values == nil {
		return nil
	}
	cloned := append([]string(nil), (*values)...)
	return &cloned
}

type allowAllEligibility struct{}

func (allowAllEligibility) Eligible(ctx context.Context, _ EligibilityRequest) (bool, error) {
	if ctx == nil {
		return false, errors.New("route eligibility context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return true, nil
}
