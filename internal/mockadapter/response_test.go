package mockadapter_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/mockadapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/mockprovider"
)

func TestParseResponseUsageFixtures(t *testing.T) {
	t.Parallel()

	built, client, _ := newAdapterRuntime(t, mockprovider.NewHandler())
	tests := []struct {
		name      string
		scenario  string
		input     int64
		output    int64
		cache     adapter.TokenCount
		reasoning adapter.TokenCount
	}{
		{"normal", mockadapter.ScenarioNormal, 6, 4, adapter.TokenCount{}, adapter.TokenCount{}},
		{"fixed", mockadapter.ScenarioFixedUsage, 11, 7, adapter.Tokens(0), adapter.Tokens(2)},
		{"cached", mockadapter.ScenarioCachedUsage, 13, 3, adapter.Tokens(5), adapter.TokenCount{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			response, ctx := execute(t, client, built, normalizedRequest(false, test.scenario))
			defer func() { _ = response.Body.Close() }()
			normalized, err := built.ParseResponse(ctx, response)
			if err != nil {
				t.Fatalf("parse response: %v", err)
			}
			if normalized.ObservedAt != fixedClock() || normalized.Model != "mock-chat-v1" || len(normalized.Choices) != 1 {
				t.Fatalf("unexpected normalized response: %#v", normalized)
			}
			if normalized.Choices[0].FinishReason != adapter.FinishStop || normalized.Choices[0].Message.Parts[0].Text != "deterministic mock response" {
				t.Fatalf("unexpected choice: %#v", normalized.Choices[0])
			}
			usage := normalized.Usage
			if usage == nil || !usage.Complete || usage.Source != adapter.UsageSourceProvider {
				t.Fatalf("unexpected usage: %#v", usage)
			}
			if usage.InputTokens != adapter.Tokens(test.input) || usage.OutputTokens != adapter.Tokens(test.output) ||
				usage.CacheReadTokens != test.cache || usage.ReasoningTokens != test.reasoning {
				t.Fatalf("usage counts = %#v", *usage)
			}
			if usage.RawEvidenceHash() == "" || !strings.Contains(string(usage.RawEvidence.Bytes()), `"total_tokens"`) {
				t.Fatal("usage evidence missing")
			}
		})
	}
}

func TestParseResponseToolCallFixture(t *testing.T) {
	t.Parallel()

	built, client, _ := newAdapterRuntime(t, mockprovider.NewHandler())
	response, ctx := execute(t, client, built, normalizedRequest(false, mockadapter.ScenarioToolCall))
	defer func() { _ = response.Body.Close() }()
	normalized, err := built.ParseResponse(ctx, response)
	if err != nil {
		t.Fatalf("parse tool response: %v", err)
	}
	choice := normalized.Choices[0]
	if choice.FinishReason != adapter.FinishToolCalls || len(choice.Message.Parts) != 0 || len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("unexpected tool choice: %#v", choice)
	}
	call := choice.Message.ToolCalls[0]
	if call.ID != "call_mock_weather" || call.Name != "get_weather" || string(call.Arguments) != `{"city":"Shanghai"}` {
		t.Fatalf("unexpected tool call: %#v", call)
	}
}

func TestParseResponsePreservesUnknownUsageEvidence(t *testing.T) {
	t.Parallel()

	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{
            "id":"response_unknown_usage","object":"chat.completion","created":0,"model":"mock-chat-v1",
            "choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"future_reason"}],
            "usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3,
                "future_meter":7,"prompt_tokens_details":{"cached_tokens":1,"future_cache_class":2}}
        }`)
	})
	built, client, _ := newAdapterRuntime(t, handler)
	response, ctx := execute(t, client, built, normalizedRequest(false, ""))
	defer func() { _ = response.Body.Close() }()
	normalized, err := built.ParseResponse(ctx, response)
	if err != nil {
		t.Fatalf("parse unknown usage: %v", err)
	}
	if normalized.Choices[0].FinishReason != adapter.FinishUnknown || normalized.Choices[0].ProviderFinishReason != "future_reason" {
		t.Fatalf("unknown finish not preserved: %#v", normalized.Choices[0])
	}
	want := []string{"/future_meter", "/prompt_tokens_details/future_cache_class"}
	if fmt.Sprint(normalized.Usage.UnmappedFields) != fmt.Sprint(want) {
		t.Fatalf("unmapped = %v, want %v", normalized.Usage.UnmappedFields, want)
	}
	raw := string(normalized.Usage.RawEvidence.Bytes())
	if !strings.Contains(raw, "future_meter") || !strings.Contains(raw, "future_cache_class") {
		t.Fatalf("raw evidence lost unknown fields: %s", raw)
	}
}

func TestParseResponseNormalizesProviderErrors(t *testing.T) {
	t.Parallel()

	built, client, _ := newAdapterRuntime(t, mockprovider.NewHandler())
	tests := []struct {
		name       string
		scenario   string
		code       string
		category   adapter.ErrorCategory
		status     int
		retryAfter time.Duration
	}{
		{"rate", mockadapter.ScenarioRateLimit, "MOCK_RATE_LIMITED", adapter.ErrorRateLimit, 429, time.Second},
		{"server", mockadapter.ScenarioServerError, "MOCK_PROVIDER_UNAVAILABLE", adapter.ErrorCapacity, 503, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			response, ctx := execute(t, client, built, normalizedRequest(false, test.scenario))
			defer func() { _ = response.Body.Close() }()
			_, err := built.ParseResponse(ctx, response)
			if err == nil {
				t.Fatal("expected normalized provider error")
			}
			var normalized adapter.NormalizedError
			if !errors.As(err, &normalized) {
				t.Fatalf("error type = %T: %v", err, err)
			}
			if normalized.Code != test.code || normalized.Category != test.category || normalized.ProviderStatus != test.status || !normalized.Retryable {
				t.Fatalf("normalized error = %#v", normalized)
			}
			if test.retryAfter == 0 {
				if normalized.RetryAfter != nil {
					t.Fatalf("unexpected retry after: %v", *normalized.RetryAfter)
				}
			} else if normalized.RetryAfter == nil || *normalized.RetryAfter != test.retryAfter {
				t.Fatalf("retry after = %v, want %v", normalized.RetryAfter, test.retryAfter)
			}
		})
	}
}

func TestNormalizeErrorNeverCopiesProviderMessageOrInvalidRequestID(t *testing.T) {
	t.Parallel()

	built, _, _ := newAdapterRuntime(t, mockprovider.NewHandler())
	response := &http.Response{
		StatusCode: http.StatusTeapot,
		Header:     http.Header{"X-Request-ID": []string{"unsafe id"}},
	}
	normalized := built.NormalizeError(context.Background(), response, []byte(`{
        "error":{"code":"future_error","message":"prompt-secret provider-key-secret"}
    }`))
	if normalized.Category != adapter.ErrorUnknown || normalized.Retryable || normalized.ProviderRequestID != "" {
		t.Fatalf("unexpected unknown error: %#v", normalized)
	}
	if strings.Contains(normalized.Error(), "prompt-secret") || strings.Contains(normalized.Error(), "provider-key-secret") {
		t.Fatalf("provider message leaked: %s", normalized.Error())
	}
}

func TestParseResponseRejectsTruncationUnknownFieldsAndOversize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		handler http.Handler
		want    error
	}{
		{
			"unknown response field",
			http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(writer, `{"id":"x","object":"chat.completion","model":"mock-chat-v1","choices":[],"usage":null,"future":1}`)
			}),
			mockadapter.ErrProtocol,
		},
		{
			"oversize",
			http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(writer, strings.Repeat("x", (1<<20)+1))
			}),
			mockadapter.ErrResponseTooLarge,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			built, client, _ := newAdapterRuntime(t, test.handler)
			response, ctx := execute(t, client, built, normalizedRequest(false, ""))
			defer func() { _ = response.Body.Close() }()
			if _, err := built.ParseResponse(ctx, response); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want errors.Is %v", err, test.want)
			}
		})
	}

	built, client, _ := newAdapterRuntime(t, mockprovider.NewHandler())
	response, ctx := execute(t, client, built, normalizedRequest(false, mockadapter.ScenarioDisconnect))
	defer func() { _ = response.Body.Close() }()
	if _, err := built.ParseResponse(ctx, response); !errors.Is(err, mockadapter.ErrProtocol) {
		t.Fatalf("disconnect error = %v", err)
	}
}

func TestParseResponseRejectsInvalidContentTypeAndUsageTotal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		contentType string
		body        string
	}{
		{"content type", "text/plain", `{}`},
		{"usage total", "application/json", `{
            "id":"bad","object":"chat.completion","model":"mock-chat-v1",
            "choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
            "usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":99}
        }`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", test.contentType)
				_, _ = io.WriteString(writer, test.body)
			})
			built, client, _ := newAdapterRuntime(t, handler)
			response, ctx := execute(t, client, built, normalizedRequest(false, ""))
			defer func() { _ = response.Body.Close() }()
			if _, err := built.ParseResponse(ctx, response); !errors.Is(err, mockadapter.ErrProtocol) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestUsageEvidenceJSONIsStillMetadataOnlyAfterAdapterParse(t *testing.T) {
	t.Parallel()

	built, client, _ := newAdapterRuntime(t, mockprovider.NewHandler())
	response, ctx := execute(t, client, built, normalizedRequest(false, mockadapter.ScenarioNormal))
	defer func() { _ = response.Body.Close() }()
	normalized, err := built.ParseResponse(ctx, response)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	encoded, err := json.Marshal(normalized.Usage.RawEvidence)
	if err != nil {
		t.Fatalf("marshal evidence: %v", err)
	}
	if strings.Contains(string(encoded), "prompt_tokens") || !strings.Contains(string(encoded), "sha256") {
		t.Fatalf("unsafe evidence JSON: %s", encoded)
	}
}

func TestNormalizeErrorCategoryAndRetryMatrix(t *testing.T) {
	t.Parallel()

	built, _, _ := newAdapterRuntime(t, mockprovider.NewHandler())
	tests := []struct {
		name       string
		status     int
		header     string
		category   adapter.ErrorCategory
		code       string
		retryable  bool
		retryAfter time.Duration
	}{
		{"auth", 401, "", adapter.ErrorAuth, "MOCK_AUTH_FAILED", false, 0},
		{"permission", 403, "", adapter.ErrorPermission, "MOCK_PERMISSION_DENIED", false, 0},
		{"timeout", 408, "5", adapter.ErrorTimeout, "MOCK_PROVIDER_TIMEOUT", true, 5 * time.Second},
		{"invalid", 404, "", adapter.ErrorInvalidRequest, "MOCK_INVALID_REQUEST", false, 0},
		{"server", 500, "invalid", adapter.ErrorProvider5xx, "MOCK_PROVIDER_FAILED", true, 0},
		{"unknown", 418, "", adapter.ErrorUnknown, "MOCK_PROVIDER_ERROR", false, 0},
		{"http date", 429, fixedClock().Add(10 * time.Second).Format(http.TimeFormat), adapter.ErrorRateLimit, "MOCK_RATE_LIMITED", true, 10 * time.Second},
		{"invalid status fallback", 700, "", adapter.ErrorProtocol, "MOCK_PROTOCOL_ERROR", false, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			response := &http.Response{StatusCode: test.status, Header: make(http.Header)}
			response.Header.Set("Retry-After", test.header)
			normalized := built.NormalizeError(context.Background(), response, []byte(`{"error":{"code":"fixture","message":"private"}}`))
			if normalized.Category != test.category || normalized.Code != test.code || normalized.Retryable != test.retryable {
				t.Fatalf("normalized = %#v", normalized)
			}
			if test.retryAfter == 0 {
				if normalized.RetryAfter != nil {
					t.Fatalf("retry after = %v", *normalized.RetryAfter)
				}
			} else if normalized.RetryAfter == nil || *normalized.RetryAfter != test.retryAfter {
				t.Fatalf("retry after = %v, want %v", normalized.RetryAfter, test.retryAfter)
			}
		})
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	normalized := built.NormalizeError(cancelled, &http.Response{StatusCode: 503, Header: make(http.Header)}, nil)
	if normalized.Category != adapter.ErrorCancelled || normalized.Code != "UPSTREAM_CANCELLED" || normalized.Retryable {
		t.Fatalf("cancelled normalization = %#v", normalized)
	}
}

func TestProtocolAndUnsupportedErrorsRemainSafe(t *testing.T) {
	t.Parallel()

	var nilProtocol *mockadapter.ProtocolError
	if nilProtocol.Error() != "<nil>" || nilProtocol.Unwrap() != nil || nilProtocol.Is(mockadapter.ErrProtocol) {
		t.Fatal("nil ProtocolError behavior changed")
	}
	var nilParameter *mockadapter.UnsupportedParameterError
	if nilParameter.Error() != "<nil>" || nilParameter.Is(mockadapter.ErrUnsupportedParameter) {
		t.Fatal("nil UnsupportedParameterError behavior changed")
	}
	parameter := &mockadapter.UnsupportedParameterError{Field: "field", Reason: "not supported"}
	if !errors.Is(parameter, mockadapter.ErrUnsupportedParameter) || strings.Contains(parameter.Error(), "secret") {
		t.Fatalf("unsupported error = %v", parameter)
	}
}

func TestParseResponseClosesBodyWhenContextAlreadyCancelled(t *testing.T) {
	t.Parallel()

	built, _, _ := newAdapterRuntime(t, mockprovider.NewHandler())
	body := &trackedBody{Reader: strings.NewReader(`{}`)}
	response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: body}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := built.ParseResponse(ctx, response); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled parse error = %v", err)
	}
	if !body.closed {
		t.Fatal("cancelled ParseResponse did not close Body")
	}
}

func TestParseResponseProtocolValidationMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{
			"unknown choice field",
			`{"id":"x","object":"chat.completion","model":"mock-chat-v1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop","future":1}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		},
		{
			"unknown message field",
			`{"id":"x","object":"chat.completion","model":"mock-chat-v1","choices":[{"index":0,"message":{"role":"assistant","content":"ok","future":1},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		},
		{
			"invalid tool type",
			`{"id":"x","object":"chat.completion","model":"mock-chat-v1","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"future","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		},
		{
			"invalid tool arguments",
			`{"id":"x","object":"chat.completion","model":"mock-chat-v1","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"[]"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		},
		{
			"negative token",
			`{"id":"x","object":"chat.completion","model":"mock-chat-v1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":-1,"completion_tokens":1,"total_tokens":0}}`,
		},
		{
			"invalid usage details",
			`{"id":"x","object":"chat.completion","model":"mock-chat-v1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"prompt_tokens_details":[]}}`,
		},
		{
			"wrong response model",
			`{"id":"x","object":"chat.completion","model":"other-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":null}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(writer, test.body)
			})
			built, client, _ := newAdapterRuntime(t, handler)
			response, ctx := execute(t, client, built, normalizedRequest(false, ""))
			defer func() { _ = response.Body.Close() }()
			if _, err := built.ParseResponse(ctx, response); !errors.Is(err, mockadapter.ErrProtocol) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestParseResponseFinishReasonMappingsWithoutUsage(t *testing.T) {
	t.Parallel()

	for providerReason, want := range map[string]adapter.FinishReason{
		"length":         adapter.FinishLength,
		"content_filter": adapter.FinishContentPolicy,
	} {
		providerReason, want := providerReason, want
		t.Run(providerReason, func(t *testing.T) {
			t.Parallel()
			body := fmt.Sprintf(`{"id":"x","object":"chat.completion","model":"mock-chat-v1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":%q}]}`, providerReason)
			handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(writer, body)
			})
			built, client, _ := newAdapterRuntime(t, handler)
			response, ctx := execute(t, client, built, normalizedRequest(false, ""))
			defer func() { _ = response.Body.Close() }()
			normalized, err := built.ParseResponse(ctx, response)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if normalized.Usage != nil || normalized.Choices[0].FinishReason != want {
				t.Fatalf("normalized = %#v", normalized)
			}
		})
	}
}

type trackedBody struct {
	io.Reader
	closed bool
}

func (body *trackedBody) Close() error {
	body.closed = true
	return nil
}
