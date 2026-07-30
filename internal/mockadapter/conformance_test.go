package mockadapter_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/adapterconformance"
	"github.com/zse04152005-del/ai-gateway-platform/internal/mockadapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/mockprovider"
	"github.com/zse04152005-del/ai-gateway-platform/internal/provideradapter"
)

func TestMockAdapterConformance(t *testing.T) {
	adapterconformance.Run(t, mockConformanceRegistration(t))
}

func TestMockConformanceRegistrationFailsWhenFixtureIsOmitted(t *testing.T) {
	t.Parallel()

	registration := mockConformanceRegistration(t)
	registration.Fixtures.ToolCall = adapterconformance.ResponseFixture{}
	if err := registration.Validate(); !errors.Is(err, adapterconformance.ErrInvalidRegistration) {
		t.Fatalf("registration error = %v", err)
	}
}

func mockConformanceRegistration(t *testing.T) adapterconformance.Registration {
	t.Helper()
	normalUsage := conformanceUsage(t, `{"prompt_tokens":6,"completion_tokens":4,"total_tokens":10}`, 6, 4, 0)
	cachedUsage := conformanceUsage(
		t,
		`{"prompt_tokens":13,"completion_tokens":3,"total_tokens":16,"prompt_tokens_details":{"cached_tokens":5}}`,
		13,
		3,
		5,
	)
	toolUsage := conformanceUsage(t, `{"prompt_tokens":9,"completion_tokens":5,"total_tokens":14}`, 9, 5, 0)
	unknownStreamUsage := conformanceUsage(t, `{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`, 1, 1, 0)
	return adapterconformance.Registration{
		Name:       "mock",
		NewAdapter: newMockConformanceAdapter,
		Fixtures: adapterconformance.FixtureSet{
			Ordinary: adapterconformance.ResponseFixture{
				Name: "ordinary", Request: normalizedRequest(false, mockadapter.ScenarioNormal),
				NewHandler: newMockProviderHandler,
				Want:       conformanceTextResponse("chatcmpl-mock-normal", adapter.FinishStop, "stop", &normalUsage),
			},
			Stream: adapterconformance.StreamFixture{
				Name: "stream", Request: normalizedRequest(true, mockadapter.ScenarioSSE),
				NewHandler: newMockProviderHandler,
				Want:       conformanceStreamChunks(normalUsage),
			},
			Cancellation: adapterconformance.CancellationFixture{
				Name: "cancellation", Request: normalizedRequest(true, mockadapter.ScenarioSSE),
				NewHandler: newBlockingSSEHandler,
			},
			RateLimit: adapterconformance.ErrorFixture{
				Name: "rate_limit", Request: normalizedRequest(false, mockadapter.ScenarioRateLimit),
				NewHandler: newMockProviderHandler,
				Want: adapter.NormalizedError{
					Code: "MOCK_RATE_LIMITED", Category: adapter.ErrorRateLimit, Retryable: true,
					RetryAfter: durationPointer(time.Second), ProviderStatus: http.StatusTooManyRequests,
					SafeMessage: "Mock provider rate limited the request", ProviderRequestID: "req_mock_adapter",
				},
				ForbiddenText: []string{"Mock rate limit exceeded"},
			},
			ProviderFailure: adapterconformance.ErrorFixture{
				Name: "provider_failure", Request: normalizedRequest(false, mockadapter.ScenarioServerError),
				NewHandler: newMockProviderHandler,
				Want: adapter.NormalizedError{
					Code: "MOCK_PROVIDER_UNAVAILABLE", Category: adapter.ErrorCapacity, Retryable: true,
					ProviderStatus: http.StatusServiceUnavailable,
					SafeMessage:    "Mock provider is temporarily unavailable", ProviderRequestID: "req_mock_adapter",
				},
				ForbiddenText: []string{"Mock provider is unavailable"},
			},
			CachedUsage: adapterconformance.ResponseFixture{
				Name: "cached_usage", Request: normalizedRequest(false, mockadapter.ScenarioCachedUsage),
				NewHandler: newMockProviderHandler,
				Want:       conformanceTextResponse("chatcmpl-mock-cached-usage", adapter.FinishStop, "stop", &cachedUsage),
			},
			ToolCall: adapterconformance.ResponseFixture{
				Name: "tool_call", Request: normalizedRequest(false, mockadapter.ScenarioToolCall),
				NewHandler: newMockProviderHandler,
				Want:       conformanceToolResponse(toolUsage),
			},
			FinishReasons: []adapterconformance.ResponseFixture{
				finishReasonFixture("finish_length", "length", adapter.FinishLength),
				finishReasonFixture("finish_content_policy", "content_filter", adapter.FinishContentPolicy),
				finishReasonFixture("finish_unknown", "future_finish", adapter.FinishUnknown),
			},
			UnknownOrdinary: adapterconformance.ProtocolErrorFixture{
				Name: "unknown_ordinary", Request: normalizedRequest(false, ""),
				NewHandler: newUnknownOrdinaryHandler, Want: mockadapter.ErrProtocol,
				ForbiddenText: []string{"private-protocol-marker"},
			},
			UnknownStream: adapterconformance.StreamFixture{
				Name: "unknown_stream", Request: normalizedRequest(true, ""),
				NewHandler: newUnknownStreamHandler,
				Want:       conformanceUnknownStreamChunks(unknownStreamUsage),
			},
		},
	}
}

func newMockConformanceAdapter(ctx context.Context, endpoint string) (provideradapter.Adapter, error) {
	factory, err := mockadapter.NewFactory(mockadapter.FactoryOptions{Now: fixedClock})
	if err != nil {
		return nil, err
	}
	registry, err := provideradapter.NewRegistry(factory)
	if err != nil {
		return nil, err
	}
	provider := mockProvider()
	return registry.Build(ctx, provider, mockDeployment(provider.ID, endpoint))
}

func newMockProviderHandler() http.Handler {
	return mockprovider.NewHandler()
}

func newBlockingSSEHandler(cancelled chan<- struct{}) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		<-request.Context().Done()
		close(cancelled)
	})
}

func finishReasonFixture(name, providerReason string, reason adapter.FinishReason) adapterconformance.ResponseFixture {
	return adapterconformance.ResponseFixture{
		Name: name, Request: normalizedRequest(false, ""),
		NewHandler: func() http.Handler { return newFinishReasonHandler(providerReason) },
		Want:       conformanceTextResponse("chatcmpl-"+name, reason, providerReason, nil),
	}
}

func newFinishReasonHandler(providerReason string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		body, err := json.Marshal(map[string]any{
			"id": "chatcmpl-finish_" + finishFixtureSuffix(providerReason), "object": "chat.completion",
			"model": "mock-chat-v1", "choices": []any{map[string]any{
				"index": 0, "message": map[string]any{"role": "assistant", "content": "finish fixture"},
				"finish_reason": providerReason,
			}},
		})
		if err != nil {
			http.Error(writer, "encode fixture", http.StatusInternalServerError)
			return
		}
		_, _ = writer.Write(body)
	})
}

func finishFixtureSuffix(providerReason string) string {
	switch providerReason {
	case "length":
		return "length"
	case "content_filter":
		return "content_policy"
	default:
		return "unknown"
	}
}

func newUnknownOrdinaryHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":"chatcmpl-unknown","object":"chat.completion","model":"mock-chat-v1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2},"future_field":"private-protocol-marker"}`)
	})
}

func newUnknownStreamHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: "+unknownStreamEvent+"\n\n")
		_, _ = io.WriteString(writer, `data: {"id":"chatcmpl-unknown-stream","object":"chat.completion.chunk","model":"mock-chat-v1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`+"\n\n")
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	})
}

const unknownStreamEvent = `{"id":"chatcmpl-unknown-stream","object":"chat.completion.chunk","model":"mock-chat-v1","choices":[{"index":0,"delta":{"role":"assistant","future_delta":7},"finish_reason":null}],"future_top":true}`

func conformanceTextResponse(
	id string,
	reason adapter.FinishReason,
	providerReason string,
	usage *adapter.NormalizedUsage,
) adapter.NormalizedResponse {
	return adapter.NormalizedResponse{
		ResponseID: id, Model: "mock-chat-v1",
		Choices: []adapter.NormalizedChoice{{
			Index: 0, Message: adapter.Message{
				Role:  adapter.RoleAssistant,
				Parts: []adapter.ContentPart{{Kind: adapter.ContentText, Text: responseText(id)}},
			},
			FinishReason: reason, ProviderFinishReason: providerReason,
		}},
		Usage: usage, ProviderRequestID: "req_mock_adapter", ObservedAt: fixedClock(),
	}
}

func responseText(id string) string {
	if len(id) >= len("chatcmpl-finish_") && id[:len("chatcmpl-finish_")] == "chatcmpl-finish_" {
		return "finish fixture"
	}
	return "deterministic mock response"
}

func conformanceToolResponse(usage adapter.NormalizedUsage) adapter.NormalizedResponse {
	return adapter.NormalizedResponse{
		ResponseID: "chatcmpl-mock-tool-call", Model: "mock-chat-v1",
		Choices: []adapter.NormalizedChoice{{
			Index: 0, Message: adapter.Message{
				Role: adapter.RoleAssistant,
				ToolCalls: []adapter.ToolCall{{
					ID: "call_mock_weather", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Shanghai"}`),
				}},
			},
			FinishReason: adapter.FinishToolCalls, ProviderFinishReason: "tool_calls",
		}},
		Usage: &usage, ProviderRequestID: "req_mock_adapter", ObservedAt: fixedClock(),
	}
}

func conformanceStreamChunks(usage adapter.NormalizedUsage) []adapter.NormalizedChunk {
	return []adapter.NormalizedChunk{
		{Sequence: 0, Kind: adapter.ChunkMessageStart, Role: adapter.RoleAssistant, ProviderEventType: "message", ObservedAt: fixedClock()},
		{Sequence: 1, Kind: adapter.ChunkContentDelta, ContentDelta: "deterministic ", ProviderEventType: "message", ObservedAt: fixedClock()},
		{Sequence: 2, Kind: adapter.ChunkContentDelta, ContentDelta: "mock response", ProviderEventType: "message", ObservedAt: fixedClock()},
		{
			Sequence: 3, Kind: adapter.ChunkMessageEnd, FinishReason: adapter.FinishStop,
			ProviderFinishReason: "stop", Usage: &usage, UsageStatus: adapter.UsageStatusPresent,
			ProviderEventType: "message", ObservedAt: fixedClock(),
		},
	}
}

func conformanceUnknownStreamChunks(usage adapter.NormalizedUsage) []adapter.NormalizedChunk {
	return []adapter.NormalizedChunk{
		{Sequence: 0, Kind: adapter.ChunkMessageStart, Role: adapter.RoleAssistant, ProviderEventType: "message", ObservedAt: fixedClock()},
		{
			Sequence: 1, Kind: adapter.ChunkProviderExtension, ProviderEventType: "provider.unknown_fields",
			ProviderExtension: json.RawMessage(unknownStreamEvent), ObservedAt: fixedClock(),
		},
		{
			Sequence: 2, Kind: adapter.ChunkMessageEnd, FinishReason: adapter.FinishStop,
			ProviderFinishReason: "stop", Usage: &usage, UsageStatus: adapter.UsageStatusPresent,
			ProviderEventType: "message", ObservedAt: fixedClock(),
		},
	}
}

func conformanceUsage(t *testing.T, raw string, input, output, cacheRead int64) adapter.NormalizedUsage {
	t.Helper()
	evidence, err := adapter.NewUsageEvidence([]byte(raw))
	if err != nil {
		t.Fatalf("create conformance usage evidence: %v", err)
	}
	usage := adapter.NormalizedUsage{
		InputTokens: adapter.Tokens(input), OutputTokens: adapter.Tokens(output),
		Source: adapter.UsageSourceProvider, Complete: true, RawEvidence: evidence,
	}
	if cacheRead > 0 {
		usage.CacheReadTokens = adapter.Tokens(cacheRead)
	}
	return usage
}

func durationPointer(value time.Duration) *time.Duration {
	return &value
}
