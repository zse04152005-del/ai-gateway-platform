package openaiadapter_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/adapterconformance"
	"github.com/zse04152005-del/ai-gateway-platform/internal/openaiadapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/provideradapter"
)

func TestOpenAIAdapterConformance(t *testing.T) {
	adapterconformance.Run(t, openAIConformanceRegistration(t))
}

func openAIConformanceRegistration(t *testing.T) adapterconformance.Registration {
	t.Helper()
	normalUsage := conformanceUsage(t, `{"prompt_tokens":6,"completion_tokens":4,"total_tokens":10}`, 6, 4, 0)
	cachedUsage := conformanceUsage(t, `{"prompt_tokens":13,"completion_tokens":3,"total_tokens":16,"prompt_tokens_details":{"cached_tokens":5}}`, 13, 3, 5)
	toolUsage := conformanceUsage(t, `{"prompt_tokens":9,"completion_tokens":5,"total_tokens":14}`, 9, 5, 0)
	unknownUsage := conformanceUsage(t, `{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`, 1, 1, 0)
	return adapterconformance.Registration{
		Name: "openai", NewAdapter: newOpenAIConformanceAdapter,
		Fixtures: adapterconformance.FixtureSet{
			Ordinary: adapterconformance.ResponseFixture{
				Name: "ordinary", Request: normalizedRequest(false),
				NewHandler: responseHandler(ordinaryBody), Want: textResponse("chatcmpl-openai-normal", adapter.FinishStop, "stop", &normalUsage),
			},
			Stream: adapterconformance.StreamFixture{
				Name: "stream", Request: normalizedRequest(true), NewHandler: validStreamHandler,
				Want: streamChunks(normalUsage),
			},
			Cancellation: adapterconformance.CancellationFixture{
				Name: "cancellation", Request: normalizedRequest(true), NewHandler: blockingStreamHandler,
			},
			RateLimit: adapterconformance.ErrorFixture{
				Name: "rate_limit", Request: normalizedRequest(false), NewHandler: errorHandler(http.StatusTooManyRequests, "1"),
				Want: adapter.NormalizedError{
					Code: "OPENAI_RATE_LIMITED", Category: adapter.ErrorRateLimit, Retryable: true,
					RetryAfter: durationPointer(time.Second), ProviderStatus: http.StatusTooManyRequests,
					SafeMessage: "OpenAI rate limited the request", ProviderRequestID: "req_openai_provider",
				}, ForbiddenText: []string{"private-rate-limit-marker"},
			},
			ProviderFailure: adapterconformance.ErrorFixture{
				Name: "provider_failure", Request: normalizedRequest(false), NewHandler: errorHandler(http.StatusServiceUnavailable, ""),
				Want: adapter.NormalizedError{
					Code: "OPENAI_CAPACITY_UNAVAILABLE", Category: adapter.ErrorCapacity, Retryable: true,
					ProviderStatus: http.StatusServiceUnavailable, SafeMessage: "OpenAI is temporarily unavailable",
					ProviderRequestID: "req_openai_provider",
				}, ForbiddenText: []string{"private-provider-marker"},
			},
			CachedUsage: adapterconformance.ResponseFixture{
				Name: "cached_usage", Request: normalizedRequest(false), NewHandler: responseHandler(cachedBody),
				Want: textResponse("chatcmpl-openai-cached", adapter.FinishStop, "stop", &cachedUsage),
			},
			ToolCall: adapterconformance.ResponseFixture{
				Name: "tool_call", Request: normalizedRequest(false), NewHandler: responseHandler(toolBody),
				Want: toolResponse(toolUsage),
			},
			FinishReasons: []adapterconformance.ResponseFixture{
				finishFixture("finish_length", "length", adapter.FinishLength),
				finishFixture("finish_content_policy", "content_filter", adapter.FinishContentPolicy),
				finishFixture("finish_unknown", "future_finish", adapter.FinishUnknown),
			},
			UnknownOrdinary: adapterconformance.ProtocolErrorFixture{
				Name: "unknown_ordinary", Request: normalizedRequest(false), NewHandler: responseHandler(unknownOrdinaryBody),
				Want: openaiadapter.ErrProtocol, ForbiddenText: []string{"private-protocol-marker"},
			},
			UnknownStream: adapterconformance.StreamFixture{
				Name: "unknown_stream", Request: normalizedRequest(true), NewHandler: unknownStreamHandler,
				Want: unknownStreamChunks(unknownUsage),
			},
		},
	}
}

func newOpenAIConformanceAdapter(ctx context.Context, endpoint string) (provideradapter.Adapter, error) {
	factory, err := openaiadapter.NewFactory(openaiadapter.FactoryOptions{
		Secrets: staticResolver{value: []byte(fixtureCredential)}, Now: fixedClock, AllowInsecureLoopback: true,
	})
	if err != nil {
		return nil, err
	}
	registry, err := provideradapter.NewRegistry(factory)
	if err != nil {
		return nil, err
	}
	provider := openAIProvider()
	return registry.Build(ctx, provider, openAIDeployment(provider.ID, endpoint))
}

func responseHandler(body string) adapterconformance.HandlerFactory {
	return func() http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.Header.Get("Authorization") != "Bearer "+fixtureCredential {
				writeJSON(writer, http.StatusUnauthorized, `{"error":{"message":"private-auth-marker"}}`)
				return
			}
			writeJSON(writer, http.StatusOK, body)
		})
	}
}

func errorHandler(status int, retryAfter string) adapterconformance.HandlerFactory {
	return func() http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			if retryAfter != "" {
				writer.Header().Set("Retry-After", retryAfter)
			}
			writeJSON(writer, status, `{"error":{"message":"private-provider-marker","type":"fixture"}}`)
		})
	}
}

func validStreamHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: "+streamStart+"\n\n")
		_, _ = io.WriteString(writer, "data: "+streamContentOne+"\n\n")
		_, _ = io.WriteString(writer, "data: "+streamContentTwo+"\n\n")
		_, _ = io.WriteString(writer, "data: "+streamFinish+"\n\n")
		_, _ = io.WriteString(writer, "data: "+streamUsage+"\n\n")
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	})
}

func blockingStreamHandler(cancelled chan<- struct{}) http.Handler {
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

func unknownStreamHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: "+unknownStreamEvent+"\n\n")
		_, _ = io.WriteString(writer, "data: "+streamFinish+"\n\n")
		_, _ = io.WriteString(writer, `data: {"id":"chatcmpl-openai-stream","object":"chat.completion.chunk","model":"gpt-fixture-001","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`+"\n\n")
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	})
}

func finishFixture(name, providerReason string, reason adapter.FinishReason) adapterconformance.ResponseFixture {
	body := `{"id":"chatcmpl-` + name + `","object":"chat.completion","model":"gpt-fixture-001","choices":[{"index":0,"message":{"role":"assistant","content":"finish fixture"},"finish_reason":"` + providerReason + `"}]}`
	return adapterconformance.ResponseFixture{
		Name: name, Request: normalizedRequest(false), NewHandler: responseHandler(body),
		Want: textResponse("chatcmpl-"+name, reason, providerReason, nil),
	}
}

func textResponse(id string, reason adapter.FinishReason, providerReason string, usage *adapter.NormalizedUsage) adapter.NormalizedResponse {
	text := "deterministic openai response"
	if len(id) > len("chatcmpl-finish_") && id[:len("chatcmpl-finish_")] == "chatcmpl-finish_" {
		text = "finish fixture"
	}
	return adapter.NormalizedResponse{
		ResponseID: id, Model: "gpt-fixture-001",
		Choices: []adapter.NormalizedChoice{{
			Index: 0, Message: adapter.Message{Role: adapter.RoleAssistant, Parts: []adapter.ContentPart{{Kind: adapter.ContentText, Text: text}}},
			FinishReason: reason, ProviderFinishReason: providerReason,
		}},
		Usage: usage, ProviderRequestID: "req_openai_provider", ObservedAt: fixedClock(),
	}
}

func toolResponse(usage adapter.NormalizedUsage) adapter.NormalizedResponse {
	return adapter.NormalizedResponse{
		ResponseID: "chatcmpl-openai-tool", Model: "gpt-fixture-001",
		Choices: []adapter.NormalizedChoice{{
			Index: 0, Message: adapter.Message{Role: adapter.RoleAssistant, ToolCalls: []adapter.ToolCall{{
				ID: "call_openai_weather", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Shanghai"}`),
			}}}, FinishReason: adapter.FinishToolCalls, ProviderFinishReason: "tool_calls",
		}},
		Usage: &usage, ProviderRequestID: "req_openai_provider", ObservedAt: fixedClock(),
	}
}

func streamChunks(usage adapter.NormalizedUsage) []adapter.NormalizedChunk {
	return []adapter.NormalizedChunk{
		{Sequence: 0, Kind: adapter.ChunkMessageStart, Role: adapter.RoleAssistant, ProviderEventType: "message", ObservedAt: fixedClock()},
		{Sequence: 1, Kind: adapter.ChunkContentDelta, ContentDelta: "deterministic ", ProviderEventType: "message", ObservedAt: fixedClock()},
		{Sequence: 2, Kind: adapter.ChunkContentDelta, ContentDelta: "openai response", ProviderEventType: "message", ObservedAt: fixedClock()},
		{Sequence: 3, Kind: adapter.ChunkMessageEnd, FinishReason: adapter.FinishStop, ProviderFinishReason: "stop", Usage: &usage, UsageStatus: adapter.UsageStatusPresent, ProviderEventType: "message", ObservedAt: fixedClock()},
	}
}

func unknownStreamChunks(usage adapter.NormalizedUsage) []adapter.NormalizedChunk {
	return []adapter.NormalizedChunk{
		{Sequence: 0, Kind: adapter.ChunkMessageStart, Role: adapter.RoleAssistant, ProviderEventType: "message", ObservedAt: fixedClock()},
		{Sequence: 1, Kind: adapter.ChunkProviderExtension, ProviderEventType: "provider.unknown_fields", ProviderExtension: json.RawMessage(unknownStreamEvent), ObservedAt: fixedClock()},
		{Sequence: 2, Kind: adapter.ChunkMessageEnd, FinishReason: adapter.FinishStop, ProviderFinishReason: "stop", Usage: &usage, UsageStatus: adapter.UsageStatusPresent, ProviderEventType: "message", ObservedAt: fixedClock()},
	}
}

func conformanceUsage(t *testing.T, raw string, input, output, cacheRead int64) adapter.NormalizedUsage {
	t.Helper()
	evidence, err := adapter.NewUsageEvidence([]byte(raw))
	if err != nil {
		t.Fatalf("create usage evidence: %v", err)
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

const (
	ordinaryBody        = `{"id":"chatcmpl-openai-normal","object":"chat.completion","model":"gpt-fixture-001","choices":[{"index":0,"message":{"role":"assistant","content":"deterministic openai response"},"finish_reason":"stop"}],"usage":{"prompt_tokens":6,"completion_tokens":4,"total_tokens":10}}`
	cachedBody          = `{"id":"chatcmpl-openai-cached","object":"chat.completion","model":"gpt-fixture-001","choices":[{"index":0,"message":{"role":"assistant","content":"deterministic openai response"},"finish_reason":"stop"}],"usage":{"prompt_tokens":13,"completion_tokens":3,"total_tokens":16,"prompt_tokens_details":{"cached_tokens":5}}}`
	toolBody            = `{"id":"chatcmpl-openai-tool","object":"chat.completion","model":"gpt-fixture-001","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_openai_weather","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Shanghai\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":9,"completion_tokens":5,"total_tokens":14}}`
	unknownOrdinaryBody = `{"id":"chatcmpl-unknown","object":"chat.completion","model":"gpt-fixture-001","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"future_field":"private-protocol-marker"}`
	streamStart         = `{"id":"chatcmpl-openai-stream","object":"chat.completion.chunk","model":"gpt-fixture-001","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}],"usage":null}`
	streamContentOne    = `{"id":"chatcmpl-openai-stream","object":"chat.completion.chunk","model":"gpt-fixture-001","choices":[{"index":0,"delta":{"content":"deterministic "},"finish_reason":null}],"usage":null}`
	streamContentTwo    = `{"id":"chatcmpl-openai-stream","object":"chat.completion.chunk","model":"gpt-fixture-001","choices":[{"index":0,"delta":{"content":"openai response"},"finish_reason":null}],"usage":null}`
	streamFinish        = `{"id":"chatcmpl-openai-stream","object":"chat.completion.chunk","model":"gpt-fixture-001","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}`
	streamUsage         = `{"id":"chatcmpl-openai-stream","object":"chat.completion.chunk","model":"gpt-fixture-001","choices":[],"usage":{"prompt_tokens":6,"completion_tokens":4,"total_tokens":10}}`
	unknownStreamEvent  = `{"id":"chatcmpl-openai-stream","object":"chat.completion.chunk","model":"gpt-fixture-001","choices":[{"index":0,"delta":{"role":"assistant","future_delta":7},"finish_reason":null}],"future_top":"private-stream-marker"}`
)
