package mockprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

const validChatBody = `{"model":"mock-chat","messages":[{"role":"user","content":"hello"}]}`

func TestHandlerDeterministicCompletionScenarios(t *testing.T) {
	t.Parallel()
	server := newMockProviderTestServer(t, newHandler(waitForContext, noOpMarkStreaming))
	tests := []struct {
		name         string
		scenario     string
		promptTokens int
		completion   int
		cached       int
		finishReason string
		content      *string
		wantTool     bool
	}{
		{name: "normal", scenario: "", promptTokens: 6, completion: 4, finishReason: "stop", content: pointerTo("deterministic mock response")},
		{name: "fixed usage", scenario: string(scenarioFixedUsage), promptTokens: 11, completion: 7, finishReason: "stop", content: pointerTo("deterministic mock response")},
		{name: "cached usage", scenario: string(scenarioCachedUsage), promptTokens: 13, completion: 3, cached: 5, finishReason: "stop", content: pointerTo("deterministic mock response")},
		{name: "tool call", scenario: string(scenarioToolCall), promptTokens: 9, completion: 5, finishReason: "tool_calls", wantTool: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			body := validChatBody
			if test.scenario != "" {
				body = strings.TrimSuffix(validChatBody, "}") + `,"mock_scenario":"` + test.scenario + `"}`
			}
			response := postChat(t, server.Client(), server.URL, body, nil)
			defer closeTestBody(t, response.Body)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", response.StatusCode)
			}
			var completion completionResponse
			if err := json.NewDecoder(response.Body).Decode(&completion); err != nil {
				t.Fatalf("decode completion: %v", err)
			}
			if completion.ID == "" || completion.Model != "mock-chat" || completion.Created != 0 || len(completion.Choices) != 1 {
				t.Fatalf("completion identity = %+v", completion)
			}
			choice := completion.Choices[0]
			if choice.FinishReason != test.finishReason || !reflect.DeepEqual(choice.Message.Content, test.content) {
				t.Fatalf("choice = %+v", choice)
			}
			if completion.Usage.PromptTokens != test.promptTokens || completion.Usage.CompletionTokens != test.completion ||
				completion.Usage.TotalTokens != test.promptTokens+test.completion {
				t.Fatalf("usage = %+v", completion.Usage)
			}
			gotCached := 0
			if completion.Usage.PromptTokenDetails != nil {
				gotCached = completion.Usage.PromptTokenDetails.CachedTokens
			}
			if gotCached != test.cached {
				t.Fatalf("cached tokens = %d, want %d", gotCached, test.cached)
			}
			if test.wantTool {
				if len(choice.Message.ToolCalls) != 1 || choice.Message.ToolCalls[0].Function.Name != "get_weather" ||
					choice.Message.ToolCalls[0].Function.Arguments != `{"city":"Shanghai"}` {
					t.Fatalf("tool calls = %+v", choice.Message.ToolCalls)
				}
			}
		})
	}
}

func TestHandlerSSEAndMalformedChunkScenarios(t *testing.T) {
	t.Parallel()
	server := newMockProviderTestServer(t, newHandler(waitForContext, noOpMarkStreaming))

	t.Run("stream selected by request parameter", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions?scenario=sse", strings.NewReader(validChatBody))
		if err != nil {
			t.Fatalf("NewRequest() error = %v", err)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatalf("Do() error = %v", err)
		}
		defer closeTestBody(t, response.Body)
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		raw := string(body)
		for _, fragment := range []string{
			"text/event-stream", `"role":"assistant"`, `"content":"deterministic "`,
			`"finish_reason":"stop"`, `"prompt_tokens":6`, "data: [DONE]",
		} {
			if fragment == "text/event-stream" {
				if !strings.Contains(response.Header.Get("Content-Type"), fragment) {
					t.Fatalf("Content-Type = %q", response.Header.Get("Content-Type"))
				}
			} else if !strings.Contains(raw, fragment) {
				t.Fatalf("SSE body missing %q: %s", fragment, raw)
			}
		}
	})

	t.Run("malformed chunk is deterministic and has no done sentinel", func(t *testing.T) {
		response := postChat(t, server.Client(), server.URL, validChatBody, map[string]string{
			ScenarioHeader: string(scenarioMalformedChunk),
		})
		defer closeTestBody(t, response.Body)
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		raw := string(body)
		if !strings.Contains(raw, `data: {"id":"chatcmpl-mock-malformed","choices":[`) || strings.Contains(raw, "[DONE]") {
			t.Fatalf("malformed SSE body = %s", raw)
		}
	})
}

func TestHandlerDelayAndHTTPErrorScenarios(t *testing.T) {
	t.Parallel()
	var waited time.Duration
	handler := newHandler(func(_ context.Context, duration time.Duration) error {
		waited = duration
		return nil
	}, noOpMarkStreaming)
	server := newMockProviderTestServer(t, handler)

	delayBody := strings.TrimSuffix(validChatBody, "}") + `,"mock_scenario":"delay","mock_delay_ms":321}`
	delayResponse := postChat(t, server.Client(), server.URL, delayBody, nil)
	defer closeTestBody(t, delayResponse.Body)
	if delayResponse.StatusCode != http.StatusOK || waited != 321*time.Millisecond {
		t.Fatalf("delay status/wait = %d/%v", delayResponse.StatusCode, waited)
	}

	tests := []struct {
		scenario string
		status   int
		code     string
	}{
		{scenario: string(scenarioRateLimit), status: http.StatusTooManyRequests, code: "rate_limit_exceeded"},
		{scenario: string(scenarioServerError), status: http.StatusServiceUnavailable, code: "mock_provider_unavailable"},
	}
	for _, test := range tests {
		t.Run(test.scenario, func(t *testing.T) {
			response := postChat(t, server.Client(), server.URL, validChatBody, map[string]string{ScenarioHeader: test.scenario})
			defer closeTestBody(t, response.Body)
			if response.StatusCode != test.status || response.Header.Get(ScenarioHeader) != test.scenario {
				t.Fatalf("status/scenario = %d/%q", response.StatusCode, response.Header.Get(ScenarioHeader))
			}
			if test.status == http.StatusTooManyRequests && response.Header.Get("Retry-After") != "1" {
				t.Fatalf("Retry-After = %q", response.Header.Get("Retry-After"))
			}
			var envelope providerErrorEnvelope
			if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
				t.Fatalf("decode error: %v", err)
			}
			if envelope.Error.Code != test.code {
				t.Fatalf("error = %+v", envelope.Error)
			}
		})
	}
}

func TestHandlerDisconnectReturnsTruncatedHTTPBody(t *testing.T) {
	t.Parallel()
	server := newMockProviderTestServer(t, newHandler(waitForContext, noOpMarkStreaming))
	response := postChat(t, server.Client(), server.URL, validChatBody, map[string]string{
		ScenarioHeader: string(scenarioDisconnect),
	})
	defer closeTestBody(t, response.Body)
	body, err := io.ReadAll(response.Body)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadAll(disconnect) error = %v, want unexpected EOF; body=%s", err, body)
	}
	if !bytes.Contains(body, []byte("chatcmpl-mock-disconnect")) || response.Header.Get(ScenarioHeader) != string(scenarioDisconnect) {
		t.Fatalf("disconnect response body/header = %q/%q", body, response.Header.Get(ScenarioHeader))
	}
}

func TestHandlerRejectsAmbiguousAndInvalidRequests(t *testing.T) {
	t.Parallel()
	server := newMockProviderTestServer(t, newHandler(waitForContext, noOpMarkStreaming))
	tests := []struct {
		name        string
		method      string
		path        string
		body        string
		contentType string
		headers     map[string][]string
		status      int
		code        string
	}{
		{name: "unknown route", method: http.MethodPost, path: "/v1/unknown", body: validChatBody, contentType: "application/json", status: http.StatusNotFound, code: "not_found"},
		{name: "method", method: http.MethodGet, path: "/v1/chat/completions", status: http.StatusMethodNotAllowed, code: "method_not_allowed"},
		{name: "content type", method: http.MethodPost, path: "/v1/chat/completions", body: validChatBody, contentType: "text/plain", status: http.StatusUnsupportedMediaType, code: "unsupported_media_type"},
		{name: "unknown scenario", method: http.MethodPost, path: "/v1/chat/completions", body: validChatBody, contentType: "application/json", headers: map[string][]string{ScenarioHeader: {"not-a-scenario"}}, status: http.StatusBadRequest, code: "unknown_mock_scenario"},
		{name: "multiple header values", method: http.MethodPost, path: "/v1/chat/completions", body: validChatBody, contentType: "application/json", headers: map[string][]string{ScenarioHeader: {"normal", "sse"}}, status: http.StatusBadRequest, code: "ambiguous_mock_scenario"},
		{name: "conflicting selectors", method: http.MethodPost, path: "/v1/chat/completions?scenario=sse", body: strings.TrimSuffix(validChatBody, "}") + `,"mock_scenario":"normal"}`, contentType: "application/json", status: http.StatusBadRequest, code: "ambiguous_mock_scenario"},
		{name: "delay outside scenario", method: http.MethodPost, path: "/v1/chat/completions", body: strings.TrimSuffix(validChatBody, "}") + `,"mock_delay_ms":5}`, contentType: "application/json", status: http.StatusBadRequest, code: "unexpected_mock_delay"},
		{name: "invalid model", method: http.MethodPost, path: "/v1/chat/completions", body: `{"model":" ","messages":[{"role":"user","content":"hello"}]}`, contentType: "application/json", status: http.StatusBadRequest, code: "invalid_model"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(test.method, server.URL+test.path, strings.NewReader(test.body))
			if err != nil {
				t.Fatalf("NewRequest() error = %v", err)
			}
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			for name, values := range test.headers {
				for _, value := range values {
					request.Header.Add(name, value)
				}
			}
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatalf("Do() error = %v", err)
			}
			defer closeTestBody(t, response.Body)
			if response.StatusCode != test.status {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.status)
			}
			var envelope providerErrorEnvelope
			if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if envelope.Error.Code != test.code {
				t.Fatalf("error code = %q, want %q", envelope.Error.Code, test.code)
			}
		})
	}
}

func TestHandlerRejectsOversizedBodyAndWaitHonorsCancellation(t *testing.T) {
	t.Parallel()
	server := newMockProviderTestServer(t, newHandler(waitForContext, noOpMarkStreaming))
	oversized := `{"model":"mock-chat","messages":[{"role":"user","content":"` + strings.Repeat("x", maximumRequestBody) + `"}]}`
	response := postChat(t, server.Client(), server.URL, oversized, nil)
	defer closeTestBody(t, response.Body)
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status = %d, want 413", response.StatusCode)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitForContext(ctx, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForContext(canceled) error = %v", err)
	}
}

func newMockProviderTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func postChat(
	t *testing.T,
	client *http.Client,
	baseURL string,
	body string,
	headers map[string]string,
) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	return response
}

func noOpMarkStreaming(context.Context) error {
	return nil
}

func pointerTo(value string) *string {
	return &value
}

func closeTestBody(t *testing.T, body io.Closer) {
	t.Helper()
	if err := body.Close(); err != nil {
		t.Errorf("response body close error = %v", err)
	}
}
