package openaiadapter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/providersecret"
)

func TestProviderErrorClassificationMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status    int
		category  adapter.ErrorCategory
		retryable bool
	}{
		{http.StatusUnauthorized, adapter.ErrorAuth, false},
		{http.StatusForbidden, adapter.ErrorPermission, false},
		{http.StatusRequestTimeout, adapter.ErrorTimeout, true},
		{http.StatusGatewayTimeout, adapter.ErrorTimeout, true},
		{http.StatusTooManyRequests, adapter.ErrorRateLimit, true},
		{http.StatusServiceUnavailable, adapter.ErrorCapacity, true},
		{http.StatusBadRequest, adapter.ErrorInvalidRequest, false},
		{http.StatusNotFound, adapter.ErrorInvalidRequest, false},
		{http.StatusMethodNotAllowed, adapter.ErrorInvalidRequest, false},
		{http.StatusConflict, adapter.ErrorInvalidRequest, false},
		{http.StatusRequestEntityTooLarge, adapter.ErrorInvalidRequest, false},
		{http.StatusUnsupportedMediaType, adapter.ErrorInvalidRequest, false},
		{http.StatusUnprocessableEntity, adapter.ErrorInvalidRequest, false},
		{http.StatusInternalServerError, adapter.ErrorProvider5xx, true},
		{http.StatusTeapot, adapter.ErrorUnknown, false},
	}
	for _, test := range tests {
		normalized := classifyProviderError(test.status)
		if normalized.Category != test.category || normalized.Retryable != test.retryable {
			t.Fatalf("status %d = %#v", test.status, normalized)
		}
	}
}

func TestRetryAfterAndRequestIDBoundaries(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	for _, value := range []string{"", "0", "-1", "90000", "invalid", now.Add(-time.Second).Format(http.TimeFormat)} {
		if parsed := parseRetryAfter(value, now); parsed != nil {
			t.Fatalf("retry-after %q = %v", value, *parsed)
		}
	}
	if parsed := parseRetryAfter("2", now); parsed == nil || *parsed != 2*time.Second {
		t.Fatalf("seconds retry-after = %v", parsed)
	}
	if parsed := parseRetryAfter(now.Add(time.Minute).Format(http.TimeFormat), now); parsed == nil || *parsed != time.Minute {
		t.Fatalf("date retry-after = %v", parsed)
	}
	if safeProviderRequestID(" invalid value ") != "" || safeProviderRequestID("req-valid") != "req-valid" {
		t.Fatal("request ID safety boundary failed")
	}
	if validProviderStatus(99) != 0 || validProviderStatus(600) != 0 || validProviderStatus(429) != 429 {
		t.Fatal("provider status safety boundary failed")
	}
}

func TestProtocolAndCapabilityUtilityBranches(t *testing.T) {
	t.Parallel()
	var nilUnsupported *UnsupportedParameterError
	if nilUnsupported.Error() != "<nil>" {
		t.Fatal("nil unsupported error is not safe")
	}
	privateCause := errors.New("private marker")
	violation := &ProtocolError{Operation: "parse", Code: "bad", cause: privateCause}
	if !errors.Is(violation.Unwrap(), privateCause) || violation.ProtocolOperation() != "parse" || violation.ProtocolCode() != "bad" ||
		!errors.Is(violation, ErrProtocol) || errors.Is(violation, ErrResponseTooLarge) {
		t.Fatalf("protocol violation = %#v", violation)
	}
	sizeViolation := &ProtocolError{Operation: "read", Code: "large", cause: ErrResponseTooLarge}
	if !errors.Is(sizeViolation, ErrResponseTooLarge) {
		t.Fatal("size sentinel was not preserved")
	}
	var nilViolation *ProtocolError
	if nilViolation.Error() != "<nil>" || nilViolation.Unwrap() != nil || nilViolation.ProtocolOperation() != "" || nilViolation.ProtocolCode() != "" {
		t.Fatal("nil protocol error is not safe")
	}
	var nilAdapter *openAIAdapter
	if nilAdapter.Capabilities(context.Background()).Chat {
		t.Fatal("nil adapter returned capabilities")
	}
	if _, err := nilAdapter.EstimateUsage(context.Background(), adapter.NormalizedRequest{}); !errors.Is(err, ErrUsageEstimationUnavailable) {
		t.Fatalf("estimate error = %v", err)
	}
}

func TestEndpointAndFactoryOptionBoundaries(t *testing.T) {
	t.Parallel()
	valid := []string{"https://api.openai.com", "https://api.openai.com/v1/", "https://api.openai.com/v1/chat/completions"}
	for _, raw := range valid {
		if _, err := parseEndpoint(raw, false); err != nil {
			t.Fatalf("endpoint %q: %v", raw, err)
		}
	}
	invalid := []string{
		"ftp://api.openai.com/v1", "https://user@api.openai.com/v1", "https://api.openai.com/v2",
		"https://api.openai.com/v1?q=1", "https://api.openai.com/v1#fragment", "http://127.0.0.1/v1",
	}
	for _, raw := range invalid {
		if _, err := parseEndpoint(raw, false); err == nil {
			t.Fatalf("endpoint %q was accepted", raw)
		}
	}
	if _, err := parseEndpoint("http://127.0.0.1/v1", true); err != nil {
		t.Fatalf("loopback fixture endpoint: %v", err)
	}
	if _, err := NewFactory(FactoryOptions{Secrets: branchResolver{}, Now: func() time.Time { return time.Time{} }}); err == nil {
		t.Fatal("zero clock was accepted")
	}
	if _, err := NewFactory(FactoryOptions{Secrets: branchResolver{}, UserAgent: "invalid\ragent"}); err == nil {
		t.Fatal("invalid user agent was accepted")
	}
}

func TestResponseFailureBranches(t *testing.T) {
	t.Parallel()
	openAI := branchAdapter(t)
	if _, err := openAI.ParseResponse(context.Background(), nil); !errors.Is(err, ErrProtocol) {
		t.Fatalf("nil response error = %v", err)
	}
	//nolint:bodyclose,staticcheck // Deliberately verifies nil-context rejection; ParseResponse closes the body.
	if _, err := openAI.ParseResponse(nil, trackedBranchResponse(t, http.StatusOK, "application/json", `{}`)); err == nil {
		t.Fatal("nil context was accepted")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	//nolint:bodyclose // ParseResponse owns and closes the supplied response body.
	if _, err := openAI.ParseResponse(cancelled, trackedBranchResponse(t, http.StatusOK, "application/json", `{}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled response error = %v", err)
	}
	tests := []struct {
		name        string
		contentType string
		body        string
	}{
		{"content type", "text/plain", `{}`},
		{"invalid json", "application/json", `{`},
		{"identity", "application/json", `{"object":"other","model":"gpt-fixture","choices":[]}`},
		{"moderation", "application/json", `{"id":"x","object":"chat.completion","model":"gpt-fixture","choices":[],"moderation":{"input":{}}}`},
		{"logprobs", "application/json", `{"id":"x","object":"chat.completion","model":"gpt-fixture","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop","logprobs":{}}]}`},
		{"role", "application/json", `{"id":"x","object":"chat.completion","model":"gpt-fixture","choices":[{"index":0,"message":{"role":"user","content":"ok"},"finish_reason":"stop"}]}`},
		{"refusal", "application/json", `{"id":"x","object":"chat.completion","model":"gpt-fixture","choices":[{"index":0,"message":{"role":"assistant","content":null,"refusal":"no"},"finish_reason":"stop"}]}`},
		{"tool type", "application/json", `{"id":"x","object":"chat.completion","model":"gpt-fixture","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"x","type":"custom","function":{}}]},"finish_reason":"tool_calls"}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			//nolint:bodyclose // ParseResponse owns and closes the supplied response body.
			_, err := openAI.ParseResponse(context.Background(), trackedBranchResponse(t, http.StatusOK, test.contentType, test.body))
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestNormalizeErrorCancellationAndRetryAfter(t *testing.T) {
	t.Parallel()
	openAI := branchAdapter(t)
	//nolint:bodyclose // trackedBranchResponse registers cleanup for this NormalizeError-only response.
	response := trackedBranchResponse(t, http.StatusInternalServerError, "application/json", `{}`)
	response.Header.Set("Retry-After", "2")
	response.Header.Set("X-Request-ID", "req-provider")
	normalized := openAI.NormalizeError(context.Background(), response, nil)
	if normalized.Category != adapter.ErrorProvider5xx || normalized.RetryAfter == nil || *normalized.RetryAfter != 2*time.Second {
		t.Fatalf("normalized error = %#v", normalized)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	normalized = openAI.NormalizeError(cancelled, response, nil)
	if normalized.Category != adapter.ErrorCancelled || normalized.Retryable {
		t.Fatalf("cancelled error = %#v", normalized)
	}
}

func TestStreamToolHeartbeatMissingUsageAndFailures(t *testing.T) {
	t.Parallel()
	openAI := branchAdapter(t)
	toolEvent := `{"id":"chatcmpl-stream","object":"chat.completion.chunk","model":"gpt-fixture","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"id\":"}}]},"finish_reason":null}]}`
	finishEvent := `{"id":"chatcmpl-stream","object":"chat.completion.chunk","model":"gpt-fixture","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`
	body := ": keepalive\n\n" + "data: " + toolEvent + "\n\n" + "data: " + finishEvent + "\n\n" + "data: [DONE]\n\n"
	//nolint:bodyclose // OpenStream transfers body ownership to the returned stream.
	stream, err := openAI.OpenStream(context.Background(), trackedBranchResponse(t, http.StatusOK, "text/event-stream", body))
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	wantKinds := []adapter.ChunkKind{adapter.ChunkHeartbeat, adapter.ChunkMessageStart, adapter.ChunkToolDelta, adapter.ChunkMessageEnd}
	for index, want := range wantKinds {
		chunk, nextErr := stream.Next(context.Background())
		if nextErr != nil || chunk.Kind != want {
			t.Fatalf("chunk %d = %#v / %v", index, chunk, nextErr)
		}
		if chunk.Kind == adapter.ChunkMessageEnd && chunk.UsageStatus != adapter.UsageStatusMissing {
			t.Fatalf("missing usage status = %s", chunk.UsageStatus)
		}
	}
	if _, err := stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal error = %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}

	if _, err := openAI.OpenStream(context.Background(), nil); !errors.Is(err, ErrProtocol) {
		t.Fatalf("nil stream response = %v", err)
	}
	//nolint:bodyclose // OpenStream consumes and closes non-success bodies.
	if _, err := openAI.OpenStream(context.Background(), trackedBranchResponse(t, http.StatusTooManyRequests, "application/json", `{}`)); err == nil {
		t.Fatal("provider stream error was accepted")
	}
	//nolint:bodyclose // OpenStream closes responses rejected before ownership transfer.
	if _, err := openAI.OpenStream(context.Background(), trackedBranchResponse(t, http.StatusOK, "application/json", `{}`)); !errors.Is(err, ErrProtocol) {
		t.Fatalf("stream content type error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	//nolint:bodyclose // OpenStream closes responses rejected for cancellation.
	if _, err := openAI.OpenStream(cancelled, trackedBranchResponse(t, http.StatusOK, "text/event-stream", "")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled open stream = %v", err)
	}
}

func TestUsageAndCredentialInvalidBranches(t *testing.T) {
	t.Parallel()
	invalidUsage := []string{
		`[]`,
		`{"prompt_tokens":-1}`,
		`{"prompt_tokens":1,"completion_tokens":1,"total_tokens":3}`,
		`{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"prompt_tokens_details":[]}`,
	}
	for _, raw := range invalidUsage {
		if _, err := parseUsage([]byte(raw), true); !errors.Is(err, ErrProtocol) {
			t.Fatalf("usage %s error = %v", raw, err)
		}
	}
	if validCredential(nil) || validCredential([]byte(" value")) || validCredential([]byte("bad\nvalue")) || !validCredential([]byte("fixture-token")) {
		t.Fatal("credential validation boundary failed")
	}
	if _, err := buildContent(nil); err != nil {
		t.Fatalf("empty content: %v", err)
	}
	if _, err := buildContent([]adapter.ContentPart{{Kind: adapter.ContentImageReference, Reference: "https://example.test/image.png"}}); err != nil {
		t.Fatalf("image content: %v", err)
	}
}

func branchAdapter(t *testing.T) *openAIAdapter {
	t.Helper()
	endpoint, err := url.Parse("https://api.openai.com/v1")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	return &openAIAdapter{endpoint: endpoint, physicalModel: "gpt-fixture", now: func() time.Time {
		return time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	}}
}

func branchResponse(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Header: http.Header{"Content-Type": []string{contentType}},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}

func trackedBranchResponse(t *testing.T, status int, contentType, body string) *http.Response {
	t.Helper()
	response := branchResponse(status, contentType, body)
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

type branchResolver struct{}

func (branchResolver) Resolve(context.Context, providersecret.Locator) ([]byte, error) {
	return []byte("fixture-token"), nil
}
