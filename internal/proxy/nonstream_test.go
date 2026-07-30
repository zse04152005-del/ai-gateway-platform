package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/mockadapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/mockprovider"
	"github.com/zse04152005-del/ai-gateway-platform/internal/provideradapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/routing"
	"github.com/zse04152005-del/ai-gateway-platform/internal/upstreamhttp"
)

func TestNonStreamExecutorRunsRealMockAdapterResponses(t *testing.T) {
	executor, selection := newProxyRuntime(t)
	tests := []struct {
		name          string
		scenario      string
		finish        adapter.FinishReason
		cacheRead     int64
		toolCallCount int
	}{
		{name: "normal", scenario: mockadapter.ScenarioNormal, finish: adapter.FinishStop},
		{name: "cached usage", scenario: mockadapter.ScenarioCachedUsage, finish: adapter.FinishStop, cacheRead: 5},
		{name: "tool call", scenario: mockadapter.ScenarioToolCall, finish: adapter.FinishToolCalls, toolCallCount: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := proxyRequest(test.scenario)
			response, err := executor.Execute(context.Background(), selection, request)
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if response.Model != selection.Candidate.Deployment.PhysicalModel || response.Choices[0].FinishReason != test.finish {
				t.Fatalf("response identity/finish = %q/%q", response.Model, response.Choices[0].FinishReason)
			}
			if response.Usage == nil || response.Usage.InputTokens.Value+response.Usage.OutputTokens.Value <= 0 {
				t.Fatalf("usage = %#v", response.Usage)
			}
			if response.Usage.CacheReadTokens.Value != test.cacheRead || len(response.Choices[0].Message.ToolCalls) != test.toolCallCount {
				t.Fatalf("cache/tool count = %d/%d", response.Usage.CacheReadTokens.Value, len(response.Choices[0].Message.ToolCalls))
			}
			if test.toolCallCount > 0 {
				call := response.Choices[0].Message.ToolCalls[0]
				if call.Name != "get_weather" || !json.Valid(call.Arguments) {
					t.Fatalf("tool call = %#v", call)
				}
			}
		})
	}
}

func TestNonStreamExecutorReturnsSafeNormalizedProviderFailures(t *testing.T) {
	executor, selection := newProxyRuntime(t)
	tests := []struct {
		scenario  string
		category  adapter.ErrorCategory
		retryable bool
	}{
		{scenario: mockadapter.ScenarioRateLimit, category: adapter.ErrorRateLimit, retryable: true},
		{scenario: mockadapter.ScenarioServerError, category: adapter.ErrorCapacity, retryable: true},
	}
	for _, test := range tests {
		t.Run(test.scenario, func(t *testing.T) {
			_, err := executor.Execute(context.Background(), selection, proxyRequest(test.scenario))
			var providerFailure *ProviderError
			if !errors.As(err, &providerFailure) {
				t.Fatalf("Execute() error = %v, want ProviderError", err)
			}
			detail := providerFailure.Detail()
			if detail.Category != test.category || detail.Retryable != test.retryable || detail.ProviderStatus < 400 {
				t.Fatalf("provider detail = %#v", detail)
			}
			if strings.Contains(strings.ToLower(err.Error()), "mock provider") || strings.Contains(strings.ToLower(err.Error()), "rate limit exceeded") {
				t.Fatalf("public execution error contains provider message: %v", err)
			}
		})
	}
}

func TestNonStreamExecutorClassifiesProtocolTransportAndCancellation(t *testing.T) {
	executor, selection := newProxyRuntime(t)
	for _, scenario := range []string{mockadapter.ScenarioDisconnect} {
		_, err := executor.Execute(context.Background(), selection, proxyRequest(scenario))
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("scenario %q error = %v, want ErrProtocol", scenario, err)
		}
	}

	privateCause := errors.New("private-upstream-origin-fixture")
	transportExecutor := &NonStreamExecutor{
		registry: executor.registry,
		client:   failingHTTPClient{err: privateCause},
	}
	_, err := transportExecutor.Execute(context.Background(), selection, proxyRequest(mockadapter.ScenarioNormal))
	if !errors.Is(err, ErrTransport) || !errors.Is(err, privateCause) || strings.Contains(err.Error(), privateCause.Error()) {
		t.Fatalf("transport error = %v", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = executor.Execute(cancelled, selection, proxyRequest(mockadapter.ScenarioNormal))
	if !errors.Is(err, ErrTransport) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled error = %v", err)
	}
}

func TestNonStreamExecutorRejectsInvalidDependenciesAndInput(t *testing.T) {
	executor, selection := newProxyRuntime(t)
	if _, err := NewNonStreamExecutor(nil, executor.client); err == nil {
		t.Fatal("NewNonStreamExecutor(nil registry) error = nil")
	}
	if _, err := NewNonStreamExecutor(executor.registry, nil); err == nil {
		t.Fatal("NewNonStreamExecutor(nil client) error = nil")
	}
	streaming := proxyRequest(mockadapter.ScenarioSSE)
	streaming.Stream = true
	if _, err := executor.Execute(context.Background(), selection, streaming); !errors.Is(err, ErrInvalidExecution) {
		t.Fatalf("Execute(streaming) error = %v", err)
	}
	wrongModel := proxyRequest(mockadapter.ScenarioNormal)
	wrongModel.LogicalModel = "another-model"
	if _, err := executor.Execute(context.Background(), selection, wrongModel); !errors.Is(err, ErrInvalidExecution) {
		t.Fatalf("Execute(wrong model) error = %v", err)
	}
	var nilContext context.Context
	if _, err := executor.Execute(nilContext, selection, proxyRequest(mockadapter.ScenarioNormal)); !errors.Is(err, ErrInvalidExecution) {
		t.Fatalf("Execute(nil context) error = %v", err)
	}
}

type failingHTTPClient struct {
	err error
}

func (client failingHTTPClient) Do(*http.Request) (*http.Response, error) {
	return nil, client.err
}

func newProxyRuntime(t *testing.T) (*NonStreamExecutor, routing.Selection) {
	t.Helper()
	server := httptest.NewServer(mockprovider.NewHandler())
	t.Cleanup(server.Close)
	factory, err := mockadapter.NewFactory(mockadapter.FactoryOptions{Now: proxyClock})
	if err != nil {
		t.Fatalf("NewFactory() error = %v", err)
	}
	registry, err := provideradapter.NewRegistry(factory)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	client, err := upstreamhttp.NewClient(upstreamhttp.Options{
		ConnectTimeout: time.Second, KeepAlive: time.Second,
		TLSHandshakeTimeout: time.Second, ResponseHeaderTimeout: time.Second,
		TotalTimeout: 3 * time.Second, IdleConnTimeout: time.Second,
		ExpectContinueTimeout: time.Second, MaxIdleConns: 10,
		MaxIdleConnsPerHost: 5, MaxConnsPerHost: 10, MaxResponseHeaderBytes: 64 << 10,
	})
	if err != nil {
		t.Fatalf("upstreamhttp.NewClient() error = %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)
	executor, err := NewNonStreamExecutor(registry, client)
	if err != nil {
		t.Fatalf("NewNonStreamExecutor() error = %v", err)
	}
	return executor, routing.Selection{Candidate: proxyCandidate(server.URL)}
}

func proxyCandidate(endpoint string) catalog.RouteCandidate {
	now := proxyClock()
	provider := catalog.Provider{
		ID: "11111111-1111-4111-8111-111111111111", Code: "local-mock", Name: "Local Mock",
		AdapterType: string(mockadapter.Type), Status: catalog.StatusActive, Version: 1,
		CreatedAt: now, CreatedBy: "test", UpdatedAt: now, UpdatedBy: "test",
	}
	logical := catalog.LogicalModel{
		ID:       "22222222-2222-4222-8222-222222222222",
		TenantID: "33333333-3333-4333-8333-333333333333",
		Name:     "logical-chat", DisplayName: "Logical Chat",
		RequiredCapabilities: catalog.CapabilityRequirements{Chat: true},
		Status:               catalog.StatusActive, Version: 1,
		CreatedAt: now, CreatedBy: "test", UpdatedAt: now, UpdatedBy: "test",
	}
	deployment := catalog.Deployment{
		ID: "44444444-4444-4444-8444-444444444444", ProviderID: provider.ID,
		Code: "local", PhysicalModel: "mock-chat-v1", EndpointURL: endpoint, Region: "local",
		Capabilities: catalog.CapabilitySet{
			Chat: true, Stream: true, Tools: true, StructuredOutput: true,
			UsageInStream: true, CacheUsage: true, ReasoningUsage: true,
			MaxContextTokens: 8192, MaxOutputTokens: 2048,
			DataRetentionMode: catalog.RetentionSelfHosted, ProviderProtocolVersion: "mock-v1",
		},
		Status: catalog.StatusActive, Version: 1,
		CreatedAt: now, CreatedBy: "test", UpdatedAt: now, UpdatedBy: "test",
	}
	return catalog.RouteCandidate{
		LogicalModel: logical, Provider: provider, Deployment: deployment,
		Binding: catalog.Binding{
			LogicalModelID: logical.ID, DeploymentID: deployment.ID, Priority: 10, Weight: 100,
			Status: catalog.StatusActive, Version: 1,
			CreatedAt: now, CreatedBy: "test", UpdatedAt: now, UpdatedBy: "test",
		},
	}
}

func proxyRequest(scenario string) adapter.NormalizedRequest {
	request := adapter.NormalizedRequest{
		RequestID: "req_proxy_fixture", LogicalModel: "logical-chat",
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

func proxyClock() time.Time {
	return time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
}
