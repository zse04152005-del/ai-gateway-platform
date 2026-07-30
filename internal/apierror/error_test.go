package apierror

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewValidatesPublicDefinition(t *testing.T) {
	valid := Definition{
		Status:  http.StatusBadRequest,
		Code:    "INVALID_REQUEST",
		Message: "The request is invalid",
		Type:    "invalid_request_error",
	}
	tests := []struct {
		name   string
		mutate func(*Definition)
	}{
		{name: "status", mutate: func(definition *Definition) { definition.Status = http.StatusOK }},
		{name: "code", mutate: func(definition *Definition) { definition.Code = "bad-code" }},
		{name: "message empty", mutate: func(definition *Definition) { definition.Message = "" }},
		{name: "message newline", mutate: func(definition *Definition) { definition.Message = "unsafe\nmessage" }},
		{name: "type", mutate: func(definition *Definition) { definition.Type = "Gateway Error" }},
		{name: "param", mutate: func(definition *Definition) { definition.Param = "messages/0/content" }},
		{name: "negative retry", mutate: func(definition *Definition) { definition.RetryAfter = -time.Second }},
		{name: "sub-millisecond retry", mutate: func(definition *Definition) { definition.Retryable = true; definition.RetryAfter = time.Nanosecond }},
		{name: "retry not retryable", mutate: func(definition *Definition) { definition.RetryAfter = time.Second }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			definition := valid
			test.mutate(&definition)
			if _, err := New(definition, nil); err == nil {
				t.Fatal("New() error = nil")
			}
		})
	}
}

func TestRenderSeparatesInternalCauseFromPublicResponse(t *testing.T) {
	cause := errors.New(`dial postgres://admin:secret@10.23.4.5:5432/private from C:\service\db.go:42`)
	applicationError, err := New(Definition{
		Status:     http.StatusServiceUnavailable,
		Code:       "MODEL_CAPACITY_EXHAUSTED",
		Message:    "No healthy deployment is currently available",
		Type:       "gateway_error",
		Param:      "model",
		Retryable:  true,
		RetryAfter: 1500 * time.Millisecond,
	}, cause)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	wrapped := fmt.Errorf("route request: %w", applicationError)
	if !errors.Is(wrapped, cause) || !strings.Contains(wrapped.Error(), "10.23.4.5") {
		t.Fatal("internal cause was not preserved for trusted control flow")
	}

	status, envelope := Render(wrapped, "req_safe", "ignored_error")
	if status != http.StatusServiceUnavailable || envelope.Error.Code != "MODEL_CAPACITY_EXHAUSTED" {
		t.Fatalf("Render() = %d, %+v", status, envelope)
	}
	if envelope.Error.RequestID != "req_safe" || envelope.Error.Param == nil || *envelope.Error.Param != "model" {
		t.Fatalf("public correlation fields = %+v", envelope.Error)
	}
	encoded, encodeErr := json.Marshal(envelope)
	if encodeErr != nil {
		t.Fatalf("json.Marshal() error = %v", encodeErr)
	}
	for _, forbidden := range []string{"postgres://", "admin:secret", "10.23.4.5", `C:\service`, "db.go:42"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("public response contains internal detail %q: %s", forbidden, encoded)
		}
	}
}

func TestRenderUnknownErrorUsesGenericFallback(t *testing.T) {
	status, envelope := Render(errors.New("provider-key=should-never-leak"), "req_unknown", "gateway_error")
	if status != http.StatusInternalServerError || envelope.Error.Code != defaultInternalCode ||
		envelope.Error.Message != defaultInternalMessage || envelope.Error.Type != "gateway_error" {
		t.Fatalf("Render() = %d, %+v", status, envelope)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if strings.Contains(string(encoded), "should-never-leak") {
		t.Fatalf("fallback leaked internal error: %s", encoded)
	}

	_, invalidType := Render(nil, "", "INVALID TYPE")
	if invalidType.Error.Type != defaultInternalType {
		t.Fatalf("invalid fallback type = %q", invalidType.Error.Type)
	}
}

func TestWriteHTTPSetsStableHeadersAndRetryMetadata(t *testing.T) {
	applicationError := MustNew(Definition{
		Status:     http.StatusTooManyRequests,
		Code:       "RATE_LIMITED",
		Message:    "Request rate limit exceeded",
		Type:       "gateway_error",
		Retryable:  true,
		RetryAfter: 1500 * time.Millisecond,
	}, errors.New("redis address 10.0.0.9"))
	recorder := httptest.NewRecorder()

	WriteHTTP(recorder, applicationError, "req_retry", "gateway_error")

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d", recorder.Code)
	}
	for name, want := range map[string]string{
		"Cache-Control":          "no-store",
		"Content-Type":           "application/json; charset=utf-8",
		"X-Content-Type-Options": "nosniff",
		"X-Request-Id":           "req_retry",
		"Retry-After":            "2",
	} {
		if got := recorder.Header().Get(name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}
	if strings.Contains(recorder.Body.String(), "10.0.0.9") {
		t.Fatalf("body leaked cause: %s", recorder.Body.String())
	}
}
