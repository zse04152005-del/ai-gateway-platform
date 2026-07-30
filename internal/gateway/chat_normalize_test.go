package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/apierror"
	"github.com/zse04152005-del/ai-gateway-platform/internal/correlation"
)

func TestNormalizeChatCompletionRequestPreservesSupportedSemantics(t *testing.T) {
	t.Parallel()
	body := `{
      "model":"general-chat","user":"application-user-1","stream":true,
      "messages":[
        {"role":"user","name":"customer","content":[
          {"type":"text","text":"describe"},
          {"type":"image_url","image_url":{"url":"https://example.invalid/a.png","detail":"high"}}
        ]},
        {"role":"assistant","tool_calls":[{"id":"call_one","type":"function","function":{"name":"lookup","arguments":"{\"id\":1}"}}]},
        {"role":"tool","tool_call_id":"call_one","content":"result"}
      ],
      "temperature":0.25,"top_p":0.9,"max_completion_tokens":128,"stop":"END",
      "tools":[{"type":"function","function":{"name":"lookup","description":"Lookup","parameters":{"type":"object"}}}],
      "tool_choice":{"type":"function","function":{"name":"lookup"}},
      "response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object"},"strict":true}}
    }`
	parsed, problem := parseRequestForTest(body)
	if problem != nil {
		t.Fatalf("parse problem = %+v", problem)
	}
	normalized, err := normalizeChatCompletionRequest(parsed, "req_normalized_1", []string{"idem_normalized_1"})
	if err != nil {
		t.Fatalf("normalize error = %v", err)
	}
	request := normalized.ProviderRequest
	if err := request.Validate(); err != nil {
		t.Fatalf("normalized validation error = %v", err)
	}
	if request.RequestID != "req_normalized_1" || request.LogicalModel != "general-chat" || request.EndUserReference != "application-user-1" || !request.Stream {
		t.Fatalf("identity fields = %+v", request)
	}
	if normalized.IdempotencyKey != "idem_normalized_1" || request.MaxOutputTokens == nil || *request.MaxOutputTokens != 128 {
		t.Fatalf("idempotency/output = %q/%v", normalized.IdempotencyKey, request.MaxOutputTokens)
	}
	image := request.Messages[0].Parts[1]
	if image.Kind != adapter.ContentImageReference || image.Reference == "" || image.Detail != "high" {
		t.Fatalf("image = %+v", image)
	}
	call := request.Messages[1].ToolCalls[0]
	if call.ID != "call_one" || call.Name != "lookup" || string(call.Arguments) != `{"id":1}` {
		t.Fatalf("tool call = %+v", call)
	}
	if request.Messages[2].ToolCallID != "call_one" || len(request.Tools) != 1 || request.ToolChoice == nil || request.ToolChoice.Mode != adapter.ToolChoiceNamed {
		t.Fatalf("tool semantics = %+v/%+v", request.Messages[2], request.ToolChoice)
	}
	if request.ResponseFormat == nil || request.ResponseFormat.Type != adapter.ResponseFormatJSONSchema || request.ResponseFormat.Strict == nil || !*request.ResponseFormat.Strict {
		t.Fatalf("response format = %+v", request.ResponseFormat)
	}

	cloned := normalized.Clone()
	cloned.ProviderRequest.Messages[1].ToolCalls[0].Arguments[2] = 'X'
	cloned.ProviderRequest.Tools[0].InputSchema[2] = 'X'
	*cloned.ProviderRequest.Temperature = 1.5
	if string(request.Messages[1].ToolCalls[0].Arguments) != `{"id":1}` || string(request.Tools[0].InputSchema) != `{"type":"object"}` || *request.Temperature != 0.25 {
		t.Fatal("normalized clone aliases mutable input")
	}

	var logBuffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuffer, nil))
	logger.Info("normalized", "request", normalized)
	for _, secret := range []string{"application-user-1", "idem_normalized_1", "describe", "https://example.invalid"} {
		if strings.Contains(logBuffer.String(), secret) {
			t.Fatalf("safe log leaked %q: %s", secret, logBuffer.String())
		}
	}
}

func TestNormalizeChatCompletionRequestValidatesIdempotencyBoundary(t *testing.T) {
	t.Parallel()
	parsed, problem := parseRequestForTest(`{"model":"general-chat","messages":[{"role":"user","content":"hello"}]}`)
	if problem != nil {
		t.Fatalf("parse problem = %+v", problem)
	}
	if normalized, err := normalizeChatCompletionRequest(parsed, "req_normalized_2", nil); err != nil || normalized.IdempotencyKey != "" {
		t.Fatalf("optional idempotency = %+v/%v", normalized, err)
	}
	for _, values := range [][]string{
		{"short"},
		{" idem_valid_1"},
		{"idem,invalid"},
		{"idem_valid_1", "idem_valid_2"},
	} {
		_, err := normalizeChatCompletionRequest(parsed, "req_normalized_2", values)
		if err == nil {
			t.Fatalf("idempotency values %#v were accepted", values)
		}
		status, envelope := apierror.Render(err, "req_normalized_2", "gateway_error")
		if status != http.StatusBadRequest || envelope.Error.Code != "INVALID_IDEMPOTENCY_KEY" || envelope.Error.Param == nil || *envelope.Error.Param != idempotencyKeyHeader {
			t.Fatalf("idempotency error = %d/%+v", status, envelope)
		}
	}
}

func TestNormalizeChatCompletionRequestFailsClosedOnMissingTrustedFacts(t *testing.T) {
	t.Parallel()
	parsed, problem := parseRequestForTest(`{"model":"general-chat","messages":[{"role":"user","content":"hello"}]}`)
	if problem != nil {
		t.Fatalf("parse problem = %+v", problem)
	}
	_, err := normalizeChatCompletionRequest(parsed, "", nil)
	if err == nil || !strings.Contains(err.Error(), "correlation") {
		t.Fatalf("missing request ID error = %v", err)
	}
	status, envelope := apierror.Render(err, "", "gateway_error")
	if status != http.StatusInternalServerError || envelope.Error.Code != "INTERNAL_ERROR" {
		t.Fatalf("public error = %d/%+v", status, envelope)
	}

	parsed.Messages[0].Content[0].Kind = "provider_private_kind"
	_, err = normalizeChatCompletionRequest(parsed, "req_normalized_3", nil)
	var validationError *adapter.ValidationError
	if err == nil || !errors.As(err, &validationError) || validationError.Field != "messages[0].parts[0].kind" {
		t.Fatalf("invalid normalized fact error = %v", err)
	}
}

func TestChatCompletionsHandlerRejectsInvalidIdempotencyKey(t *testing.T) {
	t.Parallel()
	request := httptestChatRequest(`{"model":"general-chat","messages":[{"role":"user","content":"hello"}]}`)
	request.Header.Set(idempotencyKeyHeader, "short")
	response := serveCorrelatedChatRequest(t, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; body = %s", response.Code, response.Body)
	}
	var envelope apierror.Envelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.Error.Code != "INVALID_IDEMPOTENCY_KEY" || envelope.Error.RequestID == "" {
		t.Fatalf("envelope = %+v", envelope)
	}
}

func httptestChatRequest(body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	return request
}

func serveCorrelatedChatRequest(t *testing.T, request *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	handler, err := NewHandler(&stubAuthenticator{principal: validGatewayPrincipal()}, &stubModelCatalog{})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	manager, err := correlation.New(correlation.Options{})
	if err != nil {
		t.Fatalf("correlation.New() error = %v", err)
	}
	response := httptest.NewRecorder()
	manager.Middleware(handler).ServeHTTP(response, request)
	return response
}
