package gateway

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
	"github.com/zse04152005-del/ai-gateway-platform/internal/apierror"
	"github.com/zse04152005-del/ai-gateway-platform/internal/correlation"
	"github.com/zse04152005-del/ai-gateway-platform/internal/proxy"
	"github.com/zse04152005-del/ai-gateway-platform/internal/routing"
	"github.com/zse04152005-del/ai-gateway-platform/internal/upstreamhttp"
)

func TestExecutableChatHandlerReturnsUnifiedSuccess(t *testing.T) {
	executor := &stubChatExecutor{response: gatewayNormalizedResponse(t)}
	selector := &stubRouteSelector{selection: routing.Selection{}}
	handler, err := NewExecutableHandler(
		&stubAuthenticator{principal: validGatewayPrincipal()},
		&stubModelCatalog{},
		selector,
		executor,
	)
	if err != nil {
		t.Fatalf("NewExecutableHandler() error = %v", err)
	}
	manager, err := correlation.New(correlation.Options{})
	if err != nil {
		t.Fatalf("correlation.New() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"general-chat","messages":[{"role":"user","content":"client prompt marker"}]}`,
	))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	manager.Middleware(handler).ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("status/content-type = %d/%q; body = %s", response.Code, response.Header().Get("Content-Type"), response.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["object"] != "chat.completion" || body["model"] != "general-chat" || body["provider_model"] != nil {
		t.Fatalf("response identity = %#v", body)
	}
	choices := body["choices"].([]any)
	choice := choices[0].(map[string]any)
	message := choice["message"].(map[string]any)
	toolCalls := message["tool_calls"].([]any)
	function := toolCalls[0].(map[string]any)["function"].(map[string]any)
	if choice["finish_reason"] != "tool_calls" || message["content"] != nil ||
		function["name"] != "get_weather" || function["arguments"] != `{"city":"Shanghai"}` {
		t.Fatalf("choice projection = %#v", choice)
	}
	usage := body["usage"].(map[string]any)
	promptDetails := usage["prompt_tokens_details"].(map[string]any)
	completionDetails := usage["completion_tokens_details"].(map[string]any)
	if usage["prompt_tokens"] != float64(13) || usage["completion_tokens"] != float64(3) ||
		usage["total_tokens"] != float64(16) || promptDetails["cached_tokens"] != float64(5) ||
		completionDetails["reasoning_tokens"] != float64(2) {
		t.Fatalf("usage projection = %#v", usage)
	}
	gateway := body["gateway"].(map[string]any)
	if gateway["request_id"] != response.Header().Get("X-Request-Id") ||
		gateway["attempt_count"] != float64(1) || gateway["usage_complete"] != true {
		t.Fatalf("gateway metadata = %#v", gateway)
	}
	if executor.calls != 1 || executor.request.LogicalModel != "general-chat" || strings.Contains(response.Body.String(), "client prompt marker") {
		t.Fatalf("executor calls/request or response leak = %d/%#v/%s", executor.calls, executor.request, response.Body)
	}
}

func TestExecutableChatHandlerDefersStreamingToP07(t *testing.T) {
	executor := &stubChatExecutor{response: gatewayNormalizedResponse(t)}
	handler, err := NewExecutableHandler(
		&stubAuthenticator{principal: validGatewayPrincipal()},
		&stubModelCatalog{},
		&stubRouteSelector{},
		executor,
	)
	if err != nil {
		t.Fatalf("NewExecutableHandler() error = %v", err)
	}
	manager, err := correlation.New(correlation.Options{})
	if err != nil {
		t.Fatalf("correlation.New() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"general-chat","messages":[{"role":"user","content":"hello"}],"stream":true}`,
	))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	manager.Middleware(handler).ServeHTTP(response, request)
	if response.Code != http.StatusNotImplemented || executor.calls != 0 {
		t.Fatalf("status/executor calls = %d/%d; body = %s", response.Code, executor.calls, response.Body)
	}
	var envelope apierror.Envelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Error.Code != "CHAT_STREAMING_NOT_IMPLEMENTED" {
		t.Fatalf("stream envelope/error = %#v/%v", envelope, err)
	}
}

func TestExecutionPublicErrorMapsStableProviderCategories(t *testing.T) {
	retryAfter := 2 * time.Second
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name: "rate limit",
			err: mustProviderError(t, adapter.NormalizedError{
				Code: "FIXTURE_RATE_LIMIT", Category: adapter.ErrorRateLimit, Retryable: true,
				RetryAfter: &retryAfter, ProviderStatus: http.StatusTooManyRequests,
				SafeMessage: "Provider rate limited the request",
			}),
			wantStatus: http.StatusTooManyRequests, wantCode: "PROVIDER_RATE_LIMITED",
		},
		{
			name: "capacity",
			err: mustProviderError(t, adapter.NormalizedError{
				Code: "FIXTURE_CAPACITY", Category: adapter.ErrorCapacity, Retryable: true,
				ProviderStatus: http.StatusServiceUnavailable, SafeMessage: "Provider capacity is unavailable",
			}),
			wantStatus: http.StatusServiceUnavailable, wantCode: "PROVIDER_UNAVAILABLE",
		},
		{
			name: "provider credentials",
			err: mustProviderError(t, adapter.NormalizedError{
				Code: "FIXTURE_AUTH", Category: adapter.ErrorAuth,
				ProviderStatus: http.StatusUnauthorized, SafeMessage: "Provider authentication failed",
			}),
			wantStatus: http.StatusBadGateway, wantCode: "PROVIDER_CREDENTIAL_ERROR",
		},
		{name: "transport timeout", err: errors.Join(proxy.ErrTransport, upstreamhttp.ErrTimeout), wantStatus: http.StatusGatewayTimeout, wantCode: "PROVIDER_TIMEOUT"},
		{name: "protocol", err: errors.Join(proxy.ErrProtocol, errors.New("private response body marker")), wantStatus: http.StatusBadGateway, wantCode: "PROVIDER_PROTOCOL_ERROR"},
		{name: "cancelled", err: errors.Join(proxy.ErrTransport, context.Canceled), wantStatus: clientClosedRequestStatus, wantCode: "REQUEST_CANCELLED"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, envelope := apierror.Render(executionPublicError(test.err), "req_public_error", "gateway_error")
			encoded, err := json.Marshal(envelope)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			if status != test.wantStatus || envelope.Error.Code != test.wantCode ||
				strings.Contains(string(encoded), "private response body marker") ||
				strings.Contains(string(encoded), "Provider authentication failed") {
				t.Fatalf("status/envelope = %d/%s", status, encoded)
			}
		})
	}
}

func TestChatProjectionPreservesFinishReasonsAndMissingUsage(t *testing.T) {
	tests := map[adapter.FinishReason]string{
		adapter.FinishStop: "stop", adapter.FinishLength: "length",
		adapter.FinishToolCalls: "tool_calls", adapter.FinishContentPolicy: "content_filter",
		adapter.FinishCancelled: "cancelled", adapter.FinishError: "error", adapter.FinishUnknown: "unknown",
	}
	for input, want := range tests {
		got, err := projectFinishReason(input)
		if err != nil || got != want {
			t.Errorf("projectFinishReason(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if usage, complete := projectChatUsage(nil); usage != nil || complete {
		t.Fatalf("projectChatUsage(nil) = %#v/%v", usage, complete)
	}
	evidence, err := adapter.NewUsageEvidence(json.RawMessage(`{"prompt_tokens":4}`))
	if err != nil {
		t.Fatalf("NewUsageEvidence() error = %v", err)
	}
	partial := &adapter.NormalizedUsage{
		InputTokens: adapter.Tokens(4), Source: adapter.UsageSourceProvider,
		Complete: false, RawEvidence: evidence,
	}
	if usage, complete := projectChatUsage(partial); usage != nil || complete {
		t.Fatalf("projectChatUsage(partial) = %#v/%v", usage, complete)
	}
}

func TestNewExecutableHandlerRejectsMissingExecutionDependencies(t *testing.T) {
	authenticator := &stubAuthenticator{}
	catalog := &stubModelCatalog{}
	selector := &stubRouteSelector{}
	executor := &stubChatExecutor{}
	if _, err := NewExecutableHandler(nil, catalog, selector, executor); err == nil {
		t.Fatal("nil authenticator accepted")
	}
	if _, err := NewExecutableHandler(authenticator, nil, selector, executor); err == nil {
		t.Fatal("nil catalog accepted")
	}
	if _, err := NewExecutableHandler(authenticator, catalog, nil, executor); err == nil {
		t.Fatal("nil selector accepted")
	}
	if _, err := NewExecutableHandler(authenticator, catalog, selector, nil); err == nil {
		t.Fatal("nil executor accepted")
	}
}

type stubChatExecutor struct {
	response  adapter.NormalizedResponse
	err       error
	selection routing.Selection
	request   adapter.NormalizedRequest
	calls     int
}

func (stub *stubChatExecutor) Execute(
	_ context.Context,
	selection routing.Selection,
	request adapter.NormalizedRequest,
) (adapter.NormalizedResponse, error) {
	stub.calls++
	stub.selection = selection
	stub.request = request.Clone()
	return stub.response.Clone(), stub.err
}

func gatewayNormalizedResponse(t *testing.T) adapter.NormalizedResponse {
	t.Helper()
	evidence, err := adapter.NewUsageEvidence(json.RawMessage(`{
		"prompt_tokens":13,"completion_tokens":3,"total_tokens":16,
		"prompt_tokens_details":{"cached_tokens":5},
		"completion_tokens_details":{"reasoning_tokens":2}
	}`))
	if err != nil {
		t.Fatalf("NewUsageEvidence() error = %v", err)
	}
	return adapter.NormalizedResponse{
		ResponseID: "chatcmpl_gateway_fixture", Model: "provider-model-v1",
		Choices: []adapter.NormalizedChoice{{
			Index: 0,
			Message: adapter.Message{
				Role: adapter.RoleAssistant,
				ToolCalls: []adapter.ToolCall{{
					ID: "call_weather", Name: "get_weather",
					Arguments: json.RawMessage(`{"city":"Shanghai"}`),
				}},
			},
			FinishReason: adapter.FinishToolCalls, ProviderFinishReason: "tool_calls",
		}},
		Usage: &adapter.NormalizedUsage{
			InputTokens: adapter.Tokens(13), OutputTokens: adapter.Tokens(3),
			CacheReadTokens: adapter.Tokens(5), ReasoningTokens: adapter.Tokens(2),
			Source: adapter.UsageSourceProvider, Complete: true, RawEvidence: evidence,
		},
		ProviderRequestID: "provider_request_fixture",
		ObservedAt:        time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC),
	}
}

func mustProviderError(t *testing.T, detail adapter.NormalizedError) error {
	t.Helper()
	failure, err := proxy.NewProviderError(detail)
	if err != nil {
		t.Fatalf("proxy.NewProviderError() error = %v", err)
	}
	return failure
}
