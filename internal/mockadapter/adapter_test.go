package mockadapter_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/httpserver"
	"github.com/zse04152005-del/ai-gateway-platform/internal/mockadapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/provideradapter"
)

func newAdapterRuntime(
	t *testing.T,
	handler http.Handler,
) (provideradapter.Adapter, *http.Client, *httptest.Server) {
	t.Helper()
	shared, err := httpserver.NewServer(httpserver.Options{
		ServiceName: "mock-provider-test", Version: "test",
		NotReadyCode: "MOCK_NOT_READY", NotReadyMessage: "Mock is not ready",
		ErrorType: "mock_error", ReadHeaderTimeout: time.Second, ShutdownTimeout: time.Second,
		ApplicationHandler: handler,
	})
	if err != nil {
		t.Fatalf("new shared test server: %v", err)
	}
	server := httptest.NewServer(shared.Handler())
	t.Cleanup(server.Close)

	factory, err := mockadapter.NewFactory(mockadapter.FactoryOptions{Now: fixedClock})
	if err != nil {
		t.Fatalf("new mock factory: %v", err)
	}
	registry, err := provideradapter.NewRegistry(factory)
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	provider := mockProvider()
	deployment := mockDeployment(provider.ID, server.URL)
	if err := registry.ValidateStartup([]catalog.Provider{provider}); err != nil {
		t.Fatalf("validate startup: %v", err)
	}
	built, err := registry.Build(context.Background(), provider, deployment)
	if err != nil {
		t.Fatalf("build mock adapter: %v", err)
	}
	return built, server.Client(), server
}

func mockProvider() catalog.Provider {
	now := fixedClock()
	return catalog.Provider{
		ID: "11111111-1111-4111-8111-111111111111", Code: "local-mock", Name: "Local Mock",
		AdapterType: string(mockadapter.Type), Status: catalog.StatusActive, Version: 1,
		CreatedAt: now, CreatedBy: "test", UpdatedAt: now, UpdatedBy: "test",
	}
}

func mockDeployment(providerID, endpoint string) catalog.Deployment {
	now := fixedClock()
	return catalog.Deployment{
		ID: "22222222-2222-4222-8222-222222222222", ProviderID: providerID,
		Code: "local", PhysicalModel: "mock-chat-v1", EndpointURL: endpoint, Region: "local",
		Capabilities: catalog.CapabilitySet{
			Chat: true, Stream: true, Tools: true, StructuredOutput: true, UsageInStream: true,
			CacheUsage: true, ReasoningUsage: true, MaxContextTokens: 8192, MaxOutputTokens: 2048,
			DataRetentionMode: catalog.RetentionSelfHosted, ProviderProtocolVersion: "mock-v1",
		},
		Status: catalog.StatusActive, Version: 1,
		CreatedAt: now, CreatedBy: "test", UpdatedAt: now, UpdatedBy: "test",
	}
}

func normalizedRequest(stream bool, scenario string) adapter.NormalizedRequest {
	request := adapter.NormalizedRequest{
		RequestID: "req_mock_adapter", LogicalModel: "logical-chat", Stream: stream,
		Messages: []adapter.Message{{
			Role:  adapter.RoleUser,
			Parts: []adapter.ContentPart{{Kind: adapter.ContentText, Text: "fixture prompt"}},
		}},
	}
	if scenario != "" {
		request.ProviderOptions = json.RawMessage(`{"mock_scenario":"` + scenario + `"}`)
	}
	return request
}

func execute(
	t *testing.T,
	client *http.Client,
	built provideradapter.Adapter,
	request adapter.NormalizedRequest,
) (*http.Response, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	httpRequest, err := built.BuildRequest(ctx, request)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	response, err := client.Do(httpRequest)
	if err != nil {
		t.Fatalf("execute request: %v", err)
	}
	return response, ctx
}

func fixedClock() time.Time {
	return time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
}
