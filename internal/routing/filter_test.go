package routing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
)

func TestCandidateFilterRecordsEveryFiniteReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		wantReason FilterReason
		mutate     func(*SelectionRequest, *catalog.RouteCandidate, *filterHealthReader, *filterEligibilityReader, *filterEligibilityReader)
	}{
		{
			name: "tenant allowlist", wantReason: FilterTenantNotAllowed,
			mutate: func(request *SelectionRequest, _ *catalog.RouteCandidate, _ *filterHealthReader, _ *filterEligibilityReader, _ *filterEligibilityReader) {
				models := []string{"another-model"}
				request.Access.KeyAllowedModels = &models
			},
		},
		{
			name: "capability", wantReason: FilterCapabilityMissing,
			mutate: func(request *SelectionRequest, _ *catalog.RouteCandidate, _ *filterHealthReader, _ *filterEligibilityReader, _ *filterEligibilityReader) {
				request.Request.Tools = []adapter.ToolDefinition{{Name: "lookup", InputSchema: []byte(`{"type":"object"}`)}}
			},
		},
		{
			name: "region", wantReason: FilterRegionNotAllowed,
			mutate: func(_ *SelectionRequest, candidate *catalog.RouteCandidate, _ *filterHealthReader, _ *filterEligibilityReader, _ *filterEligibilityReader) {
				regions := []string{"us-east-1"}
				candidate.LogicalModel.AllowedRegions = &regions
			},
		},
		{
			name: "status", wantReason: FilterInactive,
			mutate: func(_ *SelectionRequest, candidate *catalog.RouteCandidate, _ *filterHealthReader, _ *filterEligibilityReader, _ *filterEligibilityReader) {
				candidate.Deployment.Status = catalog.StatusDisabled
			},
		},
		{
			name: "previous attempt", wantReason: FilterPreviouslyAttempted,
			mutate: func(request *SelectionRequest, candidate *catalog.RouteCandidate, _ *filterHealthReader, _ *filterEligibilityReader, _ *filterEligibilityReader) {
				request.ExcludedDeploymentIDs = []string{candidate.Deployment.ID}
			},
		},
		{
			name: "health", wantReason: FilterUnhealthy,
			mutate: func(_ *SelectionRequest, _ *catalog.RouteCandidate, health *filterHealthReader, _ *filterEligibilityReader, _ *filterEligibilityReader) {
				health.allowed = false
			},
		},
		{
			name: "budget", wantReason: FilterBudgetDenied,
			mutate: func(_ *SelectionRequest, _ *catalog.RouteCandidate, _ *filterHealthReader, budget *filterEligibilityReader, _ *filterEligibilityReader) {
				budget.allowed = false
			},
		},
		{
			name: "capacity", wantReason: FilterCapacityUnavailable,
			mutate: func(_ *SelectionRequest, _ *catalog.RouteCandidate, _ *filterHealthReader, _ *filterEligibilityReader, capacity *filterEligibilityReader) {
				capacity.allowed = false
			},
		},
		{name: "eligible", wantReason: FilterEligible, mutate: func(*SelectionRequest, *catalog.RouteCandidate, *filterHealthReader, *filterEligibilityReader, *filterEligibilityReader) {
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := routeSelectionRequest()
			candidate := routeCandidate(1, "provider-a", "deployment-a", 20)
			health := &filterHealthReader{allowed: true}
			budget := &filterEligibilityReader{allowed: true}
			capacity := &filterEligibilityReader{allowed: true}
			test.mutate(&request, &candidate, health, budget, capacity)
			filter := mustCandidateFilter(t, &stubCandidateSource{candidates: []catalog.RouteCandidate{candidate}}, health, budget, capacity)

			result, err := filter.Filter(context.Background(), request)
			if err != nil {
				t.Fatalf("Filter() error = %v", err)
			}
			if result.PolicyVersion != candidateFilterPolicyVersion || len(result.Decisions) != 1 {
				t.Fatalf("result metadata = %+v", result)
			}
			decision, found := result.DecisionFor(candidate.Deployment.ID)
			if !found || decision.Reason != test.wantReason || decision.Eligible != (test.wantReason == FilterEligible) {
				t.Fatalf("decision = %+v/%v, want reason %q", decision, found, test.wantReason)
			}
			if got := len(result.eligible); got != boolInt(test.wantReason == FilterEligible) {
				t.Fatalf("eligible count = %d", got)
			}
		})
	}
}

func TestFilterResultValidateExplanationBoundaries(t *testing.T) {
	t.Parallel()
	deploymentID := routeUUID(6, 230)
	valid := FilterResult{
		PolicyVersion: candidateFilterPolicyVersion,
		Decisions: []CandidateDecision{{
			DeploymentID: deploymentID, Eligible: true, Reason: FilterEligible,
		}},
	}
	if err := valid.ValidateExplanation(); err != nil {
		t.Fatalf("ValidateExplanation(valid) error = %v", err)
	}

	invalid := []FilterResult{
		{},
		{PolicyVersion: candidateFilterPolicyVersion, Decisions: []CandidateDecision{{
			DeploymentID: "not-a-uuid", Eligible: true, Reason: FilterEligible,
		}}},
		{PolicyVersion: candidateFilterPolicyVersion, Decisions: []CandidateDecision{{
			DeploymentID: deploymentID, Eligible: true, Reason: FilterUnhealthy,
		}}},
		{PolicyVersion: candidateFilterPolicyVersion, Decisions: []CandidateDecision{{
			DeploymentID: deploymentID, Reason: "private_reason",
		}}},
		{PolicyVersion: candidateFilterPolicyVersion, Decisions: []CandidateDecision{
			{DeploymentID: deploymentID, Eligible: true, Reason: FilterEligible},
			{DeploymentID: deploymentID, Eligible: true, Reason: FilterEligible},
		}},
	}
	for index, result := range invalid {
		if err := result.ValidateExplanation(); err == nil {
			t.Fatalf("ValidateExplanation(invalid[%d]) error = nil", index)
		}
	}
}

func TestCandidateFilterUsesStrictOrderAndShortCircuit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		wantEvents []string
		mutate     func(*SelectionRequest, *catalog.RouteCandidate, *filterHealthReader, *filterEligibilityReader, *filterEligibilityReader)
	}{
		{
			name: "tenant", wantEvents: nil,
			mutate: func(request *SelectionRequest, _ *catalog.RouteCandidate, _ *filterHealthReader, _ *filterEligibilityReader, _ *filterEligibilityReader) {
				empty := []string{}
				request.Access.KeyAllowedModels = &empty
			},
		},
		{
			name: "capability", wantEvents: nil,
			mutate: func(request *SelectionRequest, _ *catalog.RouteCandidate, _ *filterHealthReader, _ *filterEligibilityReader, _ *filterEligibilityReader) {
				request.Request.Stream = true
			},
		},
		{
			name: "region", wantEvents: nil,
			mutate: func(_ *SelectionRequest, candidate *catalog.RouteCandidate, _ *filterHealthReader, _ *filterEligibilityReader, _ *filterEligibilityReader) {
				regions := []string{"eu-west-1"}
				candidate.LogicalModel.AllowedRegions = &regions
			},
		},
		{
			name: "status", wantEvents: nil,
			mutate: func(_ *SelectionRequest, candidate *catalog.RouteCandidate, _ *filterHealthReader, _ *filterEligibilityReader, _ *filterEligibilityReader) {
				candidate.Provider.Status = catalog.StatusDisabled
			},
		},
		{
			name: "health", wantEvents: []string{"health"},
			mutate: func(_ *SelectionRequest, _ *catalog.RouteCandidate, health *filterHealthReader, _ *filterEligibilityReader, _ *filterEligibilityReader) {
				health.allowed = false
			},
		},
		{
			name: "previous attempt", wantEvents: nil,
			mutate: func(request *SelectionRequest, candidate *catalog.RouteCandidate, _ *filterHealthReader, _ *filterEligibilityReader, _ *filterEligibilityReader) {
				request.ExcludedDeploymentIDs = []string{candidate.Deployment.ID}
			},
		},
		{
			name: "budget", wantEvents: []string{"health", "budget"},
			mutate: func(_ *SelectionRequest, _ *catalog.RouteCandidate, _ *filterHealthReader, budget *filterEligibilityReader, _ *filterEligibilityReader) {
				budget.allowed = false
			},
		},
		{
			name: "capacity", wantEvents: []string{"health", "budget", "capacity"},
			mutate: func(_ *SelectionRequest, _ *catalog.RouteCandidate, _ *filterHealthReader, _ *filterEligibilityReader, capacity *filterEligibilityReader) {
				capacity.allowed = false
			},
		},
		{
			name: "eligible", wantEvents: []string{"health", "budget", "capacity"},
			mutate: func(*SelectionRequest, *catalog.RouteCandidate, *filterHealthReader, *filterEligibilityReader, *filterEligibilityReader) {
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var events []string
			request := routeSelectionRequest()
			candidate := routeCandidate(1, "provider-a", "deployment-a", 30)
			health := &filterHealthReader{allowed: true, record: func() { events = append(events, "health") }}
			budget := &filterEligibilityReader{allowed: true, record: func() { events = append(events, "budget") }}
			capacity := &filterEligibilityReader{allowed: true, record: func() { events = append(events, "capacity") }}
			test.mutate(&request, &candidate, health, budget, capacity)
			filter := mustCandidateFilter(t, &stubCandidateSource{candidates: []catalog.RouteCandidate{candidate}}, health, budget, capacity)
			if _, err := filter.Filter(context.Background(), request); err != nil {
				t.Fatalf("Filter() error = %v", err)
			}
			if !reflect.DeepEqual(events, test.wantEvents) {
				t.Fatalf("events = %#v, want %#v", events, test.wantEvents)
			}
		})
	}
}

func TestCandidateFilterAllowlistAndRegionSemantics(t *testing.T) {
	t.Parallel()
	candidate := routeCandidate(1, "provider-a", "deployment-a", 40)
	health := &filterHealthReader{allowed: true}
	budget := &filterEligibilityReader{allowed: true}
	capacity := &filterEligibilityReader{allowed: true}
	filter := mustCandidateFilter(t, &stubCandidateSource{candidates: []catalog.RouteCandidate{candidate}}, health, budget, capacity)

	request := routeSelectionRequest()
	result, err := filter.Filter(context.Background(), request)
	if err != nil || result.Decisions[0].Reason != FilterEligible {
		t.Fatalf("nil allowlists = %+v/%v", result, err)
	}
	emptyModels := []string{}
	request.Access.KeyAllowedModels = &emptyModels
	result, err = filter.Filter(context.Background(), request)
	if err != nil || result.Decisions[0].Reason != FilterTenantNotAllowed {
		t.Fatalf("empty model allowlist = %+v/%v", result, err)
	}
	models := []string{"general-chat"}
	request.Access.KeyAllowedModels = &models
	regions := []string{"cn-north-1"}
	candidate.LogicalModel.AllowedRegions = &regions
	filter = mustCandidateFilter(t, &stubCandidateSource{candidates: []catalog.RouteCandidate{candidate}}, health, budget, capacity)
	result, err = filter.Filter(context.Background(), request)
	if err != nil || result.Decisions[0].Reason != FilterEligible {
		t.Fatalf("explicit matching allowlists = %+v/%v", result, err)
	}
}

func TestCandidateFilterTreatsEveryCatalogLayerStatusAsInactive(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*catalog.RouteCandidate)
	}{
		{name: "logical model", mutate: func(candidate *catalog.RouteCandidate) { candidate.LogicalModel.Status = catalog.StatusDisabled }},
		{name: "binding", mutate: func(candidate *catalog.RouteCandidate) { candidate.Binding.Status = catalog.StatusDisabled }},
		{name: "deployment", mutate: func(candidate *catalog.RouteCandidate) { candidate.Deployment.Status = catalog.StatusDisabled }},
		{name: "provider", mutate: func(candidate *catalog.RouteCandidate) { candidate.Provider.Status = catalog.StatusDisabled }},
	}
	for index, test := range tests {
		index, test := index, test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			candidate := routeCandidate(1, "provider-a", "deployment-a", 50+index)
			test.mutate(&candidate)
			filter := mustCandidateFilter(t, &stubCandidateSource{candidates: []catalog.RouteCandidate{candidate}}, &filterHealthReader{allowed: true}, &filterEligibilityReader{allowed: true}, &filterEligibilityReader{allowed: true})
			result, err := filter.Filter(context.Background(), routeSelectionRequest())
			if err != nil || result.Decisions[0].Reason != FilterInactive {
				t.Fatalf("decision = %+v/%v", result, err)
			}
		})
	}
}

func TestCandidateFilterFailsClosedOnReaderErrors(t *testing.T) {
	t.Parallel()
	privateMarker := "private-dependency-marker"
	tests := []struct {
		name        string
		wantError   error
		healthErr   error
		budgetErr   error
		capacityErr error
		wantCalls   []int
	}{
		{name: "health", wantError: ErrHealthUnavailable, healthErr: errors.New(privateMarker), wantCalls: []int{1, 0, 0}},
		{name: "budget", wantError: ErrBudgetUnavailable, budgetErr: errors.New(privateMarker), wantCalls: []int{1, 1, 0}},
		{name: "capacity", wantError: ErrCapacityUnavailable, capacityErr: errors.New(privateMarker), wantCalls: []int{1, 1, 1}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			health := &filterHealthReader{allowed: true, err: test.healthErr}
			budget := &filterEligibilityReader{allowed: true, err: test.budgetErr}
			capacity := &filterEligibilityReader{allowed: true, err: test.capacityErr}
			candidate := routeCandidate(1, "provider-a", "deployment-a", 60)
			filter := mustCandidateFilter(t, &stubCandidateSource{candidates: []catalog.RouteCandidate{candidate}}, health, budget, capacity)
			result, err := filter.Filter(context.Background(), routeSelectionRequest())
			if !errors.Is(err, test.wantError) || !strings.Contains(err.Error(), privateMarker) {
				t.Fatalf("Filter() error = %v, want errors.Is(%v)", err, test.wantError)
			}
			if len(result.Decisions) != 0 || !reflect.DeepEqual([]int{len(health.calls), len(budget.calls), len(capacity.calls)}, test.wantCalls) {
				t.Fatalf("partial result/calls = %+v/%v/%v/%v", result, health.calls, budget.calls, capacity.calls)
			}
		})
	}
}

func TestCandidateFilterRejectsUntrustedFactsDuplicatesAndOverflow(t *testing.T) {
	t.Parallel()
	health := &filterHealthReader{allowed: true}
	budget := &filterEligibilityReader{allowed: true}
	capacity := &filterEligibilityReader{allowed: true}
	tests := []struct {
		name       string
		candidates []catalog.RouteCandidate
	}{
		{
			name: "cross tenant",
			candidates: func() []catalog.RouteCandidate {
				candidate := routeCandidate(1, "provider-a", "deployment-a", 70)
				candidate.LogicalModel.TenantID = "30000000-0000-4000-8000-000000000001"
				return []catalog.RouteCandidate{candidate}
			}(),
		},
		{
			name: "broken relationship",
			candidates: func() []catalog.RouteCandidate {
				candidate := routeCandidate(1, "provider-a", "deployment-a", 71)
				candidate.Binding.DeploymentID = routeUUID(6, 72)
				return []catalog.RouteCandidate{candidate}
			}(),
		},
		{
			name: "duplicate deployment",
			candidates: func() []catalog.RouteCandidate {
				candidate := routeCandidate(1, "provider-a", "deployment-a", 73)
				return []catalog.RouteCandidate{candidate, candidate}
			}(),
		},
		{name: "overflow", candidates: make([]catalog.RouteCandidate, maximumCandidates+1)},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			filter := mustCandidateFilter(t, &stubCandidateSource{candidates: test.candidates}, health, budget, capacity)
			if _, err := filter.Filter(context.Background(), routeSelectionRequest()); !errors.Is(err, ErrCandidateSource) {
				t.Fatalf("Filter() error = %v", err)
			}
		})
	}
}

func TestCandidateFilterAcceptsBoundAndKeepsStableSafeAliasFreeReport(t *testing.T) {
	t.Parallel()
	candidates := make([]catalog.RouteCandidate, 0, maximumCandidates)
	for index := maximumCandidates; index > 0; index-- {
		candidates = append(candidates, routeCandidate(1, "provider-a", fmt.Sprintf("deployment-%03d", index), 1000+index))
	}
	filter := mustCandidateFilter(
		t,
		&stubCandidateSource{candidates: candidates},
		&filterHealthReader{allowed: true},
		&filterEligibilityReader{allowed: true},
		&filterEligibilityReader{allowed: true},
	)
	result, err := filter.Filter(context.Background(), routeSelectionRequest())
	if err != nil {
		t.Fatalf("Filter() error = %v", err)
	}
	if len(result.Decisions) != maximumCandidates || len(result.eligible) != maximumCandidates {
		t.Fatalf("result sizes = %d/%d", len(result.Decisions), len(result.eligible))
	}
	for index := 1; index < len(result.eligible); index++ {
		if !candidateLess(result.eligible[index-1], result.eligible[index]) {
			t.Fatalf("candidate order not stable at %d", index)
		}
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, forbidden := range []string{"endpoint", "secret", "physical_model", "provider_id"} {
		if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
			t.Fatalf("safe report contains %q: %s", forbidden, encoded)
		}
	}
	cloned := result.Clone()
	cloned.Decisions[0].Reason = FilterInactive
	cloned.eligible[0].LogicalModel.RequiredCapabilities.DataRetentionModes[0] = catalog.RetentionProviderDefault
	if result.Decisions[0].Reason != FilterEligible || result.eligible[0].LogicalModel.RequiredCapabilities.DataRetentionModes[0] != catalog.RetentionZero {
		t.Fatal("Clone() aliases original result")
	}
}

func TestCandidateFilterConstructorContextAndEligibilityProjection(t *testing.T) {
	t.Parallel()
	source := &stubCandidateSource{}
	health := &filterHealthReader{allowed: true}
	budget := &filterEligibilityReader{allowed: true}
	capacity := &filterEligibilityReader{allowed: true}
	if _, err := NewCandidateFilter(nil, health, budget, capacity); err == nil {
		t.Fatal("NewCandidateFilter(nil source) error = nil")
	}
	if _, err := NewCandidateFilter(source, nil, budget, capacity); err == nil {
		t.Fatal("NewCandidateFilter(nil health) error = nil")
	}
	if _, err := NewCandidateFilter(source, health, nil, capacity); err == nil {
		t.Fatal("NewCandidateFilter(nil budget) error = nil")
	}
	if _, err := NewCandidateFilter(source, health, budget, nil); err == nil {
		t.Fatal("NewCandidateFilter(nil capacity) error = nil")
	}
	filter := mustCandidateFilter(t, source, health, budget, capacity)
	if _, err := filter.Filter(nil, routeSelectionRequest()); err == nil { //nolint:staticcheck // explicit nil boundary
		t.Fatal("Filter(nil context) error = nil")
	}
	var nilFilter *CandidateFilter
	if _, err := nilFilter.Filter(context.Background(), routeSelectionRequest()); err == nil {
		t.Fatal("nil CandidateFilter.Filter() error = nil")
	}

	maxTokens := int64(1024)
	request := routeSelectionRequest()
	request.Request.Stream = true
	request.Request.MaxOutputTokens = &maxTokens
	candidate := routeCandidate(1, "provider-a", "deployment-a", 80)
	candidate.Deployment.Capabilities.Stream = true
	filter = mustCandidateFilter(t, &stubCandidateSource{candidates: []catalog.RouteCandidate{candidate}}, health, budget, capacity)
	if _, err := filter.Filter(context.Background(), request); err != nil {
		t.Fatalf("Filter() error = %v", err)
	}
	if len(budget.calls) != 1 || len(capacity.calls) != 1 || !reflect.DeepEqual(budget.calls[0], capacity.calls[0]) {
		t.Fatalf("eligibility calls = %+v/%+v", budget.calls, capacity.calls)
	}
	projection := budget.calls[0]
	if projection.TenantID != routeTenantID || projection.ProjectID != routeProjectID || projection.LogicalModel != "general-chat" ||
		projection.DeploymentID != candidate.Deployment.ID || projection.ProviderID != candidate.Provider.ID || !projection.Stream ||
		projection.MaxOutputTokens == nil || *projection.MaxOutputTokens != maxTokens {
		t.Fatalf("eligibility projection = %+v", projection)
	}
	*projection.MaxOutputTokens = 1
	if *request.Request.MaxOutputTokens != maxTokens || *capacity.calls[0].MaxOutputTokens != maxTokens {
		t.Fatal("eligibility projection aliases another reader or the normalized request")
	}
}

func TestCandidateFilterValidatesRequestScopedExclusions(t *testing.T) {
	t.Parallel()
	candidate := routeCandidate(1, "provider-a", "deployment-a", 90)
	filter := mustCandidateFilter(
		t, &stubCandidateSource{candidates: []catalog.RouteCandidate{candidate}},
		&filterHealthReader{allowed: true}, &filterEligibilityReader{allowed: true},
		&filterEligibilityReader{allowed: true},
	)

	request := routeSelectionRequest()
	request.ExcludedDeploymentIDs = []string{candidate.Deployment.ID}
	result, err := filter.Filter(context.Background(), request)
	if err != nil || len(result.Decisions) != 1 || result.Decisions[0].Reason != FilterPreviouslyAttempted ||
		result.Decisions[0].Eligible {
		t.Fatalf("excluded decision = %+v, error = %v", result, err)
	}

	for _, exclusions := range [][]string{
		{"not-a-deployment"},
		{candidate.Deployment.ID, candidate.Deployment.ID},
		func() []string {
			values := make([]string, maximumExcludedDeployments+1)
			for index := range values {
				values[index] = routeUUID(7, 1000+index)
			}
			return values
		}(),
	} {
		request.ExcludedDeploymentIDs = exclusions
		if _, err := filter.Filter(context.Background(), request); err == nil {
			t.Fatalf("Filter(exclusions=%d) error = nil", len(exclusions))
		}
	}
}

type filterHealthReader struct {
	allowed bool
	err     error
	calls   []string
	record  func()
}

func (reader *filterHealthReader) Healthy(_ context.Context, deploymentID string) (bool, error) {
	reader.calls = append(reader.calls, deploymentID)
	if reader.record != nil {
		reader.record()
	}
	return reader.allowed, reader.err
}

type filterEligibilityReader struct {
	allowed bool
	err     error
	calls   []EligibilityRequest
	record  func()
}

func (reader *filterEligibilityReader) Eligible(_ context.Context, request EligibilityRequest) (bool, error) {
	reader.calls = append(reader.calls, request)
	if reader.record != nil {
		reader.record()
	}
	return reader.allowed, reader.err
}

func mustCandidateFilter(t *testing.T, source CandidateSource, health HealthReader, budget BudgetReader, capacity CapacityReader) *CandidateFilter {
	t.Helper()
	filter, err := NewCandidateFilter(source, health, budget, capacity)
	if err != nil {
		t.Fatalf("NewCandidateFilter() error = %v", err)
	}
	return filter
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
