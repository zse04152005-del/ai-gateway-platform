package routing

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
)

const (
	routeTenantID  = "10000000-0000-4000-8000-000000000001"
	routeProjectID = "20000000-0000-4000-8000-000000000001"
)

func TestSelectorChoosesFirstHealthyCandidateByStablePriority(t *testing.T) {
	t.Parallel()
	high := routeCandidate(10, "provider-b", "deployment-b", 2)
	middle := routeCandidate(20, "provider-a", "deployment-a", 3)
	low := routeCandidate(100, "provider-a", "deployment-z", 4)
	source := &stubCandidateSource{candidates: []catalog.RouteCandidate{low, middle, high}}
	health := &stubHealthReader{healthy: map[string]bool{
		high.Deployment.ID: false, middle.Deployment.ID: true, low.Deployment.ID: true,
	}}
	selector := mustSelector(t, source, health)
	request := routeSelectionRequest()

	selection, err := selector.Select(context.Background(), request)
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if selection.Candidate.Deployment.ID != middle.Deployment.ID {
		t.Fatalf("selected deployment = %s, want %s", selection.Candidate.Deployment.ID, middle.Deployment.ID)
	}
	if !reflect.DeepEqual(health.calls, []string{high.Deployment.ID, middle.Deployment.ID, low.Deployment.ID}) {
		t.Fatalf("health calls = %#v", health.calls)
	}
	if source.query.Access.TenantID != routeTenantID || source.query.Access.ProjectID != routeProjectID || source.query.LogicalModel != "general-chat" {
		t.Fatalf("source query = %+v", source.query)
	}

	selection.Candidate.LogicalModel.RequiredCapabilities.DataRetentionModes[0] = catalog.RetentionProviderDefault
	if middle.LogicalModel.RequiredCapabilities.DataRetentionModes[0] != catalog.RetentionZero {
		t.Fatal("selection aliases source candidate")
	}
}

func TestSelectorFiltersDynamicRequestCapabilities(t *testing.T) {
	t.Parallel()
	incompatible := routeCandidate(1, "provider-a", "no-tools", 5)
	compatible := routeCandidate(2, "provider-a", "tools-vision", 6)
	compatible.Deployment.Capabilities.Tools = true
	compatible.Deployment.Capabilities.Vision = true
	compatible.Deployment.Capabilities.StructuredOutput = true
	source := &stubCandidateSource{candidates: []catalog.RouteCandidate{incompatible, compatible}}
	health := &stubHealthReader{healthy: map[string]bool{compatible.Deployment.ID: true}}
	selector := mustSelector(t, source, health)
	request := routeSelectionRequest()
	request.Request.Tools = []adapter.ToolDefinition{{Name: "lookup", InputSchema: []byte(`{"type":"object"}`)}}
	request.Request.Messages[0].Parts = append(request.Request.Messages[0].Parts, adapter.ContentPart{
		Kind: adapter.ContentImageReference, Reference: "https://example.invalid/image.png", Detail: "low",
	})
	request.Request.ResponseFormat = &adapter.ResponseFormat{Type: adapter.ResponseFormatJSONObject}

	selection, err := selector.Select(context.Background(), request)
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if selection.Candidate.Deployment.ID != compatible.Deployment.ID {
		t.Fatalf("selected deployment = %s", selection.Candidate.Deployment.ID)
	}
	if !reflect.DeepEqual(health.calls, []string{compatible.Deployment.ID}) {
		t.Fatalf("health calls = %#v; incompatible deployment should be filtered first", health.calls)
	}
}

func TestSelectorNoCandidateAndDependencyFailures(t *testing.T) {
	t.Parallel()
	request := routeSelectionRequest()
	privateMarker := "postgres-private-route-marker"
	healthMarker := "redis-private-health-marker"
	tests := []struct {
		name      string
		source    *stubCandidateSource
		health    *stubHealthReader
		wantError error
	}{
		{name: "empty", source: &stubCandidateSource{}, health: &stubHealthReader{}, wantError: ErrNoCandidate},
		{name: "all unhealthy", source: &stubCandidateSource{candidates: []catalog.RouteCandidate{routeCandidate(1, "provider-a", "deployment-a", 7)}}, health: &stubHealthReader{}, wantError: ErrNoCandidate},
		{name: "source", source: &stubCandidateSource{err: errors.New(privateMarker)}, health: &stubHealthReader{}, wantError: ErrCandidateSource},
		{name: "health", source: &stubCandidateSource{candidates: []catalog.RouteCandidate{routeCandidate(1, "provider-a", "deployment-a", 8)}}, health: &stubHealthReader{err: errors.New(healthMarker)}, wantError: ErrHealthUnavailable},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			selector := mustSelector(t, test.source, test.health)
			_, err := selector.Select(context.Background(), request)
			if !errors.Is(err, test.wantError) {
				t.Fatalf("Select() error = %v, want errors.Is(%v)", err, test.wantError)
			}
		})
	}
}

func TestSelectorFailsClosedOnInvalidCatalogFactsAndBounds(t *testing.T) {
	t.Parallel()
	invalid := routeCandidate(1, "provider-a", "deployment-a", 9)
	invalid.LogicalModel.TenantID = "30000000-0000-4000-8000-000000000001"
	selector := mustSelector(t, &stubCandidateSource{candidates: []catalog.RouteCandidate{invalid}}, &stubHealthReader{})
	if _, err := selector.Select(context.Background(), routeSelectionRequest()); !errors.Is(err, ErrCandidateSource) {
		t.Fatalf("invalid candidate error = %v", err)
	}

	overflow := make([]catalog.RouteCandidate, maximumCandidates+1)
	selector = mustSelector(t, &stubCandidateSource{candidates: overflow}, &stubHealthReader{})
	if _, err := selector.Select(context.Background(), routeSelectionRequest()); !errors.Is(err, ErrCandidateSource) {
		t.Fatalf("overflow error = %v", err)
	}
}

func TestSelectorConstructorAndContextBoundaries(t *testing.T) {
	t.Parallel()
	if _, err := NewSelector(nil, &stubHealthReader{}); err == nil {
		t.Fatal("NewSelector(nil source) error = nil")
	}
	if _, err := NewSelector(&stubCandidateSource{}, nil); err == nil {
		t.Fatal("NewSelector(nil health) error = nil")
	}
	selector := mustSelector(t, &stubCandidateSource{}, &stubHealthReader{})
	if _, err := selector.Select(nil, routeSelectionRequest()); err == nil { //nolint:staticcheck // explicit nil boundary
		t.Fatal("Select(nil context) error = nil")
	}
	var nilSelector *Selector
	if _, err := nilSelector.Select(context.Background(), routeSelectionRequest()); err == nil {
		t.Fatal("nil Selector.Select() error = nil")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if healthy, err := (ActiveCatalogHealth{}).Healthy(cancelled, "deployment"); healthy || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled health = %v/%v", healthy, err)
	}
	if _, err := (ActiveCatalogHealth{}).Healthy(nil, "deployment"); err == nil { //nolint:staticcheck // explicit nil boundary
		t.Fatal("ActiveCatalogHealth.Healthy(nil) error = nil")
	}
}

type stubCandidateSource struct {
	candidates []catalog.RouteCandidate
	err        error
	query      catalog.RouteQuery
}

func (stub *stubCandidateSource) ListRouteCandidates(_ context.Context, query catalog.RouteQuery) ([]catalog.RouteCandidate, error) {
	stub.query = query
	return append([]catalog.RouteCandidate(nil), stub.candidates...), stub.err
}

type stubHealthReader struct {
	healthy map[string]bool
	err     error
	calls   []string
}

func (stub *stubHealthReader) Healthy(_ context.Context, deploymentID string) (bool, error) {
	stub.calls = append(stub.calls, deploymentID)
	return stub.healthy[deploymentID], stub.err
}

func mustSelector(t *testing.T, source CandidateSource, health HealthReader) *Selector {
	t.Helper()
	selector, err := NewSelector(source, health)
	if err != nil {
		t.Fatalf("NewSelector() error = %v", err)
	}
	return selector
}

func routeSelectionRequest() SelectionRequest {
	return SelectionRequest{
		Access: catalog.Access{TenantID: routeTenantID, ProjectID: routeProjectID},
		Request: adapter.NormalizedRequest{
			RequestID: "req_routing_fixture", LogicalModel: "general-chat",
			Messages: []adapter.Message{{
				Role: adapter.RoleUser, Parts: []adapter.ContentPart{{Kind: adapter.ContentText, Text: "fixture"}},
			}},
		},
	}
}

func routeCandidate(priority int16, providerCode, deploymentCode string, suffix int) catalog.RouteCandidate {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	providerID := routeUUID(4, suffix)
	modelID := routeUUID(5, 1)
	deploymentID := routeUUID(6, suffix)
	return catalog.RouteCandidate{
		LogicalModel: catalog.LogicalModel{
			ID: modelID, TenantID: routeTenantID, Name: "general-chat", DisplayName: "General Chat",
			RequiredCapabilities: catalog.CapabilityRequirements{
				Chat: true, DataRetentionModes: []catalog.DataRetentionMode{catalog.RetentionZero},
			},
			Status: catalog.StatusActive, Version: 1, CreatedAt: now, CreatedBy: "test:routing", UpdatedAt: now, UpdatedBy: "test:routing",
		},
		Binding: catalog.Binding{
			LogicalModelID: modelID, DeploymentID: deploymentID, Priority: priority, Weight: 100,
			Status: catalog.StatusActive, Version: 1, CreatedAt: now, CreatedBy: "test:routing", UpdatedAt: now, UpdatedBy: "test:routing",
		},
		Deployment: catalog.Deployment{
			ID: deploymentID, ProviderID: providerID, Code: deploymentCode,
			PhysicalModel: "physical-chat", EndpointURL: "https://models.example.test/v1", Region: "cn-north-1",
			Capabilities: catalog.CapabilitySet{
				Chat: true, MaxContextTokens: 128000, MaxOutputTokens: 8192,
				DataRetentionMode: catalog.RetentionZero, ProviderProtocolVersion: "v1",
			},
			Status: catalog.StatusActive, Version: 1, CreatedAt: now, CreatedBy: "test:routing", UpdatedAt: now, UpdatedBy: "test:routing",
		},
		Provider: catalog.Provider{
			ID: providerID, Code: providerCode, Name: providerCode, AdapterType: "mock",
			Status: catalog.StatusActive, Version: 1, CreatedAt: now, CreatedBy: "test:routing", UpdatedAt: now, UpdatedBy: "test:routing",
		},
	}
}

func routeUUID(prefix, suffix int) string {
	return fmt.Sprintf("%d0000000-0000-4000-8000-%012d", prefix, suffix)
}
