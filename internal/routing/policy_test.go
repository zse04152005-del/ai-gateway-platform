package routing

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
)

func TestSelectorFixedPolicyChoosesOnlyConfiguredEligibleDeployment(t *testing.T) {
	t.Parallel()
	primary := routeCandidate(1, "provider-a", "primary", 201)
	fixed := routeCandidate(100, "provider-b", "fixed", 202)
	source := &stubCandidateSource{candidates: []catalog.RouteCandidate{primary, fixed}}
	health := &stubHealthReader{healthy: map[string]bool{primary.Deployment.ID: true, fixed.Deployment.ID: true}}
	policy := RoutePolicy{Version: "fixed-release/17", Mode: RouteFixed, FixedDeploymentID: fixed.Deployment.ID}
	resolver := mustStaticPolicyResolver(t, policy)
	random := &stubRandomSource{}
	selector := mustPolicySelector(t, source, health, resolver, random)

	selection, err := selector.Select(context.Background(), routeSelectionRequest())
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if selection.Candidate.Deployment.ID != fixed.Deployment.ID || selection.Decision.SelectedDeploymentID != fixed.Deployment.ID {
		t.Fatalf("selection = %+v", selection)
	}
	if selection.Decision.PolicyVersion != policy.Version || selection.Decision.Mode != RouteFixed ||
		selection.Decision.Priority != fixed.Binding.Priority || selection.Decision.Weight != fixed.Binding.Weight ||
		selection.Decision.EligibleCount != 2 || selection.Decision.TotalWeight != 0 || selection.Decision.RandomDraw != nil {
		t.Fatalf("decision = %+v", selection.Decision)
	}
	if len(random.bounds) != 0 {
		t.Fatalf("fixed policy random calls = %#v", random.bounds)
	}

	fixed.LogicalModel.RequiredCapabilities.DataRetentionModes[0] = catalog.RetentionProviderDefault
	if selection.Candidate.LogicalModel.RequiredCapabilities.DataRetentionModes[0] != catalog.RetentionZero {
		t.Fatal("selection aliases source candidate")
	}
}

func TestSelectorFixedPolicyDoesNotSubstituteFilteredOrUnknownTarget(t *testing.T) {
	t.Parallel()
	primary := routeCandidate(1, "provider-a", "primary", 211)
	fixed := routeCandidate(2, "provider-b", "fixed", 212)
	source := &stubCandidateSource{candidates: []catalog.RouteCandidate{primary, fixed}}
	tests := []struct {
		name string
		id   string
	}{
		{name: "filtered", id: fixed.Deployment.ID},
		{name: "unknown", id: routeUUID(6, 999999)},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			health := &stubHealthReader{healthy: map[string]bool{primary.Deployment.ID: true, fixed.Deployment.ID: false}}
			policy := RoutePolicy{Version: "fixed-release/18", Mode: RouteFixed, FixedDeploymentID: test.id}
			selector := mustPolicySelector(t, source, health, mustStaticPolicyResolver(t, policy), &stubRandomSource{})
			if _, err := selector.Select(context.Background(), routeSelectionRequest()); !errors.Is(err, ErrNoCandidate) {
				t.Fatalf("Select() error = %v", err)
			}
		})
	}
}

func TestSelectorWeightedPolicyUsesStableExactIntervals(t *testing.T) {
	t.Parallel()
	first := routeCandidate(10, "provider-a", "deployment-a", 221)
	second := routeCandidate(10, "provider-a", "deployment-b", 222)
	third := routeCandidate(10, "provider-a", "deployment-c", 223)
	first.Binding.Weight = 1
	second.Binding.Weight = 3
	third.Binding.Weight = 6
	candidates := []catalog.RouteCandidate{third, first, second}
	healthy := map[string]bool{
		first.Deployment.ID: true, second.Deployment.ID: true, third.Deployment.ID: true,
	}
	tests := []struct {
		draw uint64
		want string
	}{
		{draw: 0, want: first.Deployment.ID},
		{draw: 1, want: second.Deployment.ID},
		{draw: 3, want: second.Deployment.ID},
		{draw: 4, want: third.Deployment.ID},
		{draw: 9, want: third.Deployment.ID},
	}
	for _, test := range tests {
		test := test
		t.Run(test.want+"/draw", func(t *testing.T) {
			random := &stubRandomSource{draw: test.draw}
			selector := mustPolicySelector(
				t,
				&stubCandidateSource{candidates: candidates},
				&stubHealthReader{healthy: healthy},
				mustStaticPolicyResolver(t, RoutePolicy{Version: "weighted-release/3", Mode: RouteWeighted}),
				random,
			)
			selection, err := selector.Select(context.Background(), routeSelectionRequest())
			if err != nil {
				t.Fatalf("Select() error = %v", err)
			}
			if selection.Candidate.Deployment.ID != test.want || selection.Decision.SelectedDeploymentID != test.want {
				t.Fatalf("selected = %s/%s, want %s", selection.Candidate.Deployment.ID, selection.Decision.SelectedDeploymentID, test.want)
			}
			if !reflect.DeepEqual(random.bounds, []uint64{10}) || selection.Decision.TotalWeight != 10 ||
				selection.Decision.RandomDraw == nil || *selection.Decision.RandomDraw != test.draw ||
				selection.Decision.EligibleCount != 3 || selection.Decision.Mode != RouteWeighted {
				t.Fatalf("random/decision = %#v/%+v", random.bounds, selection.Decision)
			}
		})
	}
}

func TestWeightedPolicySeedIsInjectableAndDistributionTracksWeights(t *testing.T) {
	t.Parallel()
	left := NewSeededRandom(20260731)
	right := NewSeededRandom(20260731)
	different := NewSeededRandom(20260732)
	leftSequence := make([]uint64, 256)
	rightSequence := make([]uint64, 256)
	differentSequence := make([]uint64, 256)
	for index := range leftSequence {
		leftSequence[index] = mustRandomDraw(t, left, 10_000)
		rightSequence[index] = mustRandomDraw(t, right, 10_000)
		differentSequence[index] = mustRandomDraw(t, different, 10_000)
	}
	if !reflect.DeepEqual(leftSequence, rightSequence) {
		t.Fatal("equal seeds produced different sequences")
	}
	if reflect.DeepEqual(leftSequence, differentSequence) {
		t.Fatal("different seeds produced the same sequence")
	}

	candidates := []catalog.RouteCandidate{
		routeCandidate(1, "provider-a", "weight-1", 231),
		routeCandidate(1, "provider-a", "weight-3", 232),
		routeCandidate(1, "provider-a", "weight-6", 233),
	}
	candidates[0].Binding.Weight = 1
	candidates[1].Binding.Weight = 3
	candidates[2].Binding.Weight = 6
	policy := RoutePolicy{Version: "weighted-distribution/1", Mode: RouteWeighted}
	random := NewSeededRandom(20260731)
	counts := make(map[string]int, len(candidates))
	for range 20_000 {
		selected, _, err := selectWeighted(candidates, policy, random)
		if err != nil {
			t.Fatalf("selectWeighted() error = %v", err)
		}
		counts[selected.Deployment.ID]++
	}
	assertCountBetween(t, counts[candidates[0].Deployment.ID], 1_800, 2_200)
	assertCountBetween(t, counts[candidates[1].Deployment.ID], 5_700, 6_300)
	assertCountBetween(t, counts[candidates[2].Deployment.ID], 11_600, 12_400)
}

func TestSeededRandomIsConcurrentAndRejectsInvalidState(t *testing.T) {
	t.Parallel()
	source := NewSeededRandom(42)
	var wait sync.WaitGroup
	errorsChannel := make(chan error, 32)
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range 1_000 {
				draw, err := source.Uint64N(97)
				if err != nil || draw >= 97 {
					errorsChannel <- errors.New("concurrent seeded draw violated its bound")
					return
				}
			}
		}()
	}
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		t.Fatal(err)
	}
	if _, err := source.Uint64N(0); err == nil {
		t.Fatal("Uint64N(0) error = nil")
	}
	var nilSource *SeededRandom
	if _, err := nilSource.Uint64N(1); err == nil {
		t.Fatal("nil SeededRandom.Uint64N() error = nil")
	}
}

func TestRoutePolicyValidationAndStaticResolverBoundaries(t *testing.T) {
	t.Parallel()
	validFixedID := routeUUID(6, 241)
	valid := []RoutePolicy{
		{Version: "release/1", Mode: RouteFixed, FixedDeploymentID: validFixedID},
		{Version: "release/2", Mode: RoutePriority},
		{Version: "release/3", Mode: RouteWeighted},
	}
	for _, policy := range valid {
		if err := policy.Validate(); err != nil {
			t.Fatalf("Validate(%+v) error = %v", policy, err)
		}
	}
	invalid := []RoutePolicy{
		{},
		{Version: " bad", Mode: RoutePriority},
		{Version: strings.Repeat("a", 129), Mode: RoutePriority},
		{Version: "release/1", Mode: "unknown"},
		{Version: "release/1", Mode: RouteFixed},
		{Version: "release/1", Mode: RouteFixed, FixedDeploymentID: "not-a-uuid"},
		{Version: "release/1", Mode: RoutePriority, FixedDeploymentID: validFixedID},
		{Version: "release/1", Mode: RouteWeighted, FixedDeploymentID: validFixedID},
	}
	for _, policy := range invalid {
		if err := policy.Validate(); err == nil {
			t.Fatalf("Validate(%+v) error = nil", policy)
		}
		if _, err := NewStaticPolicyResolver(policy); err == nil {
			t.Fatalf("NewStaticPolicyResolver(%+v) error = nil", policy)
		}
	}

	resolver := mustStaticPolicyResolver(t, valid[1])
	resolved, err := resolver.Resolve(context.Background(), PolicyRequest{})
	if err != nil || resolved != valid[1] {
		t.Fatalf("Resolve() = %+v/%v", resolved, err)
	}
	if _, err := resolver.Resolve(nil, PolicyRequest{}); err == nil { //nolint:staticcheck // explicit nil boundary
		t.Fatal("Resolve(nil) error = nil")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := resolver.Resolve(cancelled, PolicyRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Resolve(cancelled) error = %v", err)
	}
	var nilResolver *StaticPolicyResolver
	if _, err := nilResolver.Resolve(context.Background(), PolicyRequest{}); err == nil {
		t.Fatal("nil StaticPolicyResolver.Resolve() error = nil")
	}
}

func TestPolicyDecisionValidationBoundaries(t *testing.T) {
	t.Parallel()
	deploymentID := routeUUID(6, 242)
	draw := uint64(4)
	valid := []PolicyDecision{
		{
			PolicyVersion: "release/1", Mode: RouteFixed, SelectedDeploymentID: deploymentID,
			Priority: 1, Weight: 10, EligibleCount: 2,
		},
		{
			PolicyVersion: "release/2", Mode: RouteWeighted, SelectedDeploymentID: deploymentID,
			Priority: 1, Weight: 10, EligibleCount: 2, TotalWeight: 20, RandomDraw: &draw,
		},
		{
			PolicyVersion: "release/3", Mode: RouteWeighted, SelectedDeploymentID: deploymentID,
			Priority: 1, Weight: 10, EligibleCount: 1, TotalWeight: 10,
		},
	}
	for _, decision := range valid {
		if err := decision.Validate(); err != nil {
			t.Fatalf("PolicyDecision.Validate(%+v) error = %v", decision, err)
		}
	}

	outOfRangeDraw := uint64(20)
	invalid := []PolicyDecision{
		{},
		{
			PolicyVersion: "release/1", Mode: RoutePriority, SelectedDeploymentID: deploymentID,
			EligibleCount: 1, TotalWeight: 1,
		},
		{
			PolicyVersion: "release/1", Mode: RouteWeighted, SelectedDeploymentID: deploymentID,
			Weight: 10, EligibleCount: 2, TotalWeight: 20,
		},
		{
			PolicyVersion: "release/1", Mode: RouteWeighted, SelectedDeploymentID: deploymentID,
			Weight: 10, EligibleCount: 2, TotalWeight: 20, RandomDraw: &outOfRangeDraw,
		},
		{
			PolicyVersion: "release/1", Mode: "private_mode", SelectedDeploymentID: deploymentID,
			EligibleCount: 1,
		},
	}
	for index, decision := range invalid {
		if err := decision.Validate(); err == nil {
			t.Fatalf("PolicyDecision.Validate(invalid[%d]) error = nil", index)
		}
	}
}

func TestSelectorPolicyAndRandomFailuresFailClosed(t *testing.T) {
	t.Parallel()
	candidate := routeCandidate(1, "provider-a", "deployment-a", 251)
	health := &stubHealthReader{healthy: map[string]bool{candidate.Deployment.ID: true}}
	source := &stubCandidateSource{candidates: []catalog.RouteCandidate{candidate}}
	privateMarker := "private-policy-marker"
	tests := []struct {
		name      string
		resolver  PolicyResolver
		random    RandomSource
		wantError error
	}{
		{
			name: "resolver error", resolver: &stubPolicyResolver{err: errors.New(privateMarker)},
			random: &stubRandomSource{}, wantError: ErrPolicyUnavailable,
		},
		{
			name: "invalid resolved policy", resolver: &stubPolicyResolver{policy: RoutePolicy{Version: "bad", Mode: "unknown"}},
			random: &stubRandomSource{}, wantError: ErrPolicyUnavailable,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			selector := mustPolicySelector(t, source, health, test.resolver, test.random)
			if _, err := selector.Select(context.Background(), routeSelectionRequest()); !errors.Is(err, test.wantError) {
				t.Fatalf("Select() error = %v, want errors.Is(%v)", err, test.wantError)
			}
		})
	}

	weightedCandidate := routeCandidate(1, "provider-a", "deployment-b", 252)
	weightedSource := &stubCandidateSource{candidates: []catalog.RouteCandidate{candidate, weightedCandidate}}
	weightedHealth := &stubHealthReader{healthy: map[string]bool{candidate.Deployment.ID: true, weightedCandidate.Deployment.ID: true}}
	resolver := mustStaticPolicyResolver(t, RoutePolicy{Version: "weighted/failed-random", Mode: RouteWeighted})
	for _, random := range []*stubRandomSource{
		{err: errors.New(privateMarker)},
		{draw: 200},
	} {
		selector := mustPolicySelector(t, weightedSource, weightedHealth, resolver, random)
		if _, err := selector.Select(context.Background(), routeSelectionRequest()); !errors.Is(err, ErrRandomUnavailable) {
			t.Fatalf("Select() random error = %v", err)
		}
	}
}

func TestSelectorSkipsPolicyAndRandomWhenNoEligibleCandidate(t *testing.T) {
	t.Parallel()
	candidate := routeCandidate(1, "provider-a", "deployment-a", 261)
	resolver := &stubPolicyResolver{err: errors.New("must not be called")}
	random := &stubRandomSource{err: errors.New("must not be called")}
	selector := mustPolicySelector(
		t,
		&stubCandidateSource{candidates: []catalog.RouteCandidate{candidate}},
		&stubHealthReader{},
		resolver,
		random,
	)
	if _, err := selector.Select(context.Background(), routeSelectionRequest()); !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("Select() error = %v", err)
	}
	if len(resolver.requests) != 0 || len(random.bounds) != 0 {
		t.Fatalf("policy/random calls = %#v/%#v", resolver.requests, random.bounds)
	}
}

func TestSelectorPolicyQueryDecisionSafetyAndConstructor(t *testing.T) {
	t.Parallel()
	candidate := routeCandidate(1, "provider-a", "deployment-a", 271)
	health := &stubHealthReader{healthy: map[string]bool{candidate.Deployment.ID: true}}
	source := &stubCandidateSource{candidates: []catalog.RouteCandidate{candidate}}
	resolver := &stubPolicyResolver{policy: RoutePolicy{Version: "priority/observed", Mode: RoutePriority}}
	selector := mustPolicySelector(t, source, health, resolver, &stubRandomSource{})
	selection, err := selector.Select(context.Background(), routeSelectionRequest())
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if !reflect.DeepEqual(resolver.requests, []PolicyRequest{{
		TenantID: routeTenantID, ProjectID: routeProjectID, LogicalModel: "general-chat",
	}}) {
		t.Fatalf("policy requests = %+v", resolver.requests)
	}
	encoded, err := json.Marshal(selection.Decision)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, forbidden := range []string{"endpoint", "secret", "physical_model", "provider_id", "prompt"} {
		if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
			t.Fatalf("policy decision contains %q: %s", forbidden, encoded)
		}
	}
	draw := uint64(7)
	decision := PolicyDecision{RandomDraw: &draw}
	cloned := decision.Clone()
	*cloned.RandomDraw = 8
	if *decision.RandomDraw != 7 {
		t.Fatal("PolicyDecision.Clone() aliases original")
	}

	budget := &filterEligibilityReader{allowed: true}
	capacity := &filterEligibilityReader{allowed: true}
	if _, err := NewSelectorWithRoutingPolicy(source, health, budget, capacity, nil, &stubRandomSource{}); err == nil {
		t.Fatal("NewSelectorWithRoutingPolicy(nil resolver) error = nil")
	}
	if _, err := NewSelectorWithRoutingPolicy(source, health, budget, capacity, resolver, nil); err == nil {
		t.Fatal("NewSelectorWithRoutingPolicy(nil random) error = nil")
	}
}

func TestWeightedSingleCandidateAvoidsRandomDependency(t *testing.T) {
	t.Parallel()
	candidate := routeCandidate(1, "provider-a", "deployment-a", 281)
	candidate.Binding.Weight = 77
	selector := mustPolicySelector(
		t,
		&stubCandidateSource{candidates: []catalog.RouteCandidate{candidate}},
		&stubHealthReader{healthy: map[string]bool{candidate.Deployment.ID: true}},
		mustStaticPolicyResolver(t, RoutePolicy{Version: "weighted/single", Mode: RouteWeighted}),
		&stubRandomSource{err: errors.New("must not be called")},
	)
	selection, err := selector.Select(context.Background(), routeSelectionRequest())
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if selection.Candidate.Deployment.ID != candidate.Deployment.ID || selection.Decision.TotalWeight != 77 || selection.Decision.RandomDraw != nil {
		t.Fatalf("selection = %+v", selection)
	}
}

func TestPolicyPrimitiveFailureBoundaries(t *testing.T) {
	t.Parallel()
	if _, _, err := selectByPolicy(nil, RoutePolicy{Version: "priority/empty", Mode: RoutePriority}, &stubRandomSource{}); !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("selectByPolicy(empty) error = %v", err)
	}
	candidate := routeCandidate(1, "provider-a", "deployment-a", 291)
	if _, _, err := selectByPolicy([]catalog.RouteCandidate{candidate}, RoutePolicy{Version: "bad/mode", Mode: "unknown"}, &stubRandomSource{}); !errors.Is(err, ErrPolicyUnavailable) {
		t.Fatalf("selectByPolicy(unknown) error = %v", err)
	}
	invalidWeight := candidate
	invalidWeight.Binding.Weight = 0
	if _, _, err := selectWeighted([]catalog.RouteCandidate{invalidWeight}, RoutePolicy{Version: "weighted/invalid", Mode: RouteWeighted}, &stubRandomSource{}); !errors.Is(err, ErrPolicyUnavailable) {
		t.Fatalf("selectWeighted(invalid weight) error = %v", err)
	}
	if _, _, err := selectWeighted([]catalog.RouteCandidate{candidate, candidate}, RoutePolicy{Version: "weighted/nil", Mode: RouteWeighted}, nil); err == nil {
		t.Fatal("selectWeighted(nil random) error = nil")
	}

	draw, err := (systemRandom{}).Uint64N(17)
	if err != nil || draw >= 17 {
		t.Fatalf("system random draw = %d/%v", draw, err)
	}
	if _, err := (systemRandom{}).Uint64N(0); err == nil {
		t.Fatal("systemRandom.Uint64N(0) error = nil")
	}
	if _, err := (bootstrapPolicyResolver{}).Resolve(nil, PolicyRequest{}); err == nil { //nolint:staticcheck // explicit nil boundary
		t.Fatal("bootstrap resolver nil context error = nil")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (bootstrapPolicyResolver{}).Resolve(cancelled, PolicyRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("bootstrap resolver cancelled error = %v", err)
	}
}

type stubPolicyResolver struct {
	policy   RoutePolicy
	err      error
	requests []PolicyRequest
}

func (resolver *stubPolicyResolver) Resolve(_ context.Context, request PolicyRequest) (RoutePolicy, error) {
	resolver.requests = append(resolver.requests, request)
	return resolver.policy, resolver.err
}

type stubRandomSource struct {
	draw   uint64
	err    error
	bounds []uint64
}

func (source *stubRandomSource) Uint64N(upperBound uint64) (uint64, error) {
	source.bounds = append(source.bounds, upperBound)
	return source.draw, source.err
}

func mustStaticPolicyResolver(t *testing.T, policy RoutePolicy) *StaticPolicyResolver {
	t.Helper()
	resolver, err := NewStaticPolicyResolver(policy)
	if err != nil {
		t.Fatalf("NewStaticPolicyResolver() error = %v", err)
	}
	return resolver
}

func mustPolicySelector(t *testing.T, source CandidateSource, health HealthReader, policies PolicyResolver, random RandomSource) *Selector {
	t.Helper()
	selector, err := NewSelectorWithRoutingPolicy(
		source,
		health,
		&filterEligibilityReader{allowed: true},
		&filterEligibilityReader{allowed: true},
		policies,
		random,
	)
	if err != nil {
		t.Fatalf("NewSelectorWithRoutingPolicy() error = %v", err)
	}
	return selector
}

func mustRandomDraw(t *testing.T, source RandomSource, upperBound uint64) uint64 {
	t.Helper()
	draw, err := source.Uint64N(upperBound)
	if err != nil {
		t.Fatalf("Uint64N() error = %v", err)
	}
	return draw
}

func assertCountBetween(t *testing.T, value, minimum, maximum int) {
	t.Helper()
	if value < minimum || value > maximum {
		t.Fatalf("count = %d, want [%d, %d]", value, minimum, maximum)
	}
}
