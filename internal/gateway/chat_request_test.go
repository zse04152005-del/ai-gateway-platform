package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zse04152005-del/ai-gateway-platform/internal/apierror"
	"github.com/zse04152005-del/ai-gateway-platform/internal/correlation"
)

func TestParseChatCompletionRequestAcceptsSupportedProtocol(t *testing.T) {
	t.Parallel()
	body := `{
        "model":"general-chat",
        "messages":[
          {"role":"system","content":"Be concise","name":"policy"},
          {"role":"user","content":[
            {"type":"text","text":"What is shown?"},
            {"type":"image_url","image_url":{"url":"https://example.invalid/image.png","detail":"low"}}
          ]},
          {"role":"assistant","content":null,"tool_calls":[{
            "id":"call_weather","type":"function",
            "function":{"name":"get_weather","arguments":"{\"city\":\"Shanghai\"}"}
          }]},
          {"role":"tool","tool_call_id":"call_weather","content":"sunny"}
        ],
        "stream":true,
        "temperature":0.2,
        "top_p":0.8,
        "max_completion_tokens":512,
        "stop":["END","DONE"],
        "tools":[{"type":"function","function":{
          "name":"get_weather","description":"Get weather",
          "parameters":{"type":"object","properties":{"city":{"type":"string"}}}
        }}],
        "tool_choice":{"type":"function","function":{"name":"get_weather"}},
        "response_format":{"type":"json_schema","json_schema":{
          "name":"weather_result","description":"Weather result",
          "schema":{"type":"object"},"strict":true
        }},
        "user":"application-user-1"
      }`

	parsed, problem := parseRequestForTest(body)
	if problem != nil {
		t.Fatalf("parse problem = %+v", problem)
	}
	if parsed.Model != "general-chat" || len(parsed.Messages) != 4 || !parsed.Stream {
		t.Fatalf("basic parsed request = %+v", parsed)
	}
	if parsed.Temperature == nil || *parsed.Temperature != 0.2 || parsed.TopP == nil || *parsed.TopP != 0.8 {
		t.Fatalf("sampling = %v/%v", parsed.Temperature, parsed.TopP)
	}
	if parsed.MaxCompletionTokens == nil || *parsed.MaxCompletionTokens != 512 || len(parsed.Stop) != 2 {
		t.Fatalf("limits = %v/%v", parsed.MaxCompletionTokens, parsed.Stop)
	}
	image := parsed.Messages[1].Content[1]
	if image.Kind != "image_url" || image.ImageDetail != "low" || image.ImageURL == "" {
		t.Fatalf("image = %+v", image)
	}
	call := parsed.Messages[2].ToolCalls[0]
	if call.ID != "call_weather" || call.Name != "get_weather" || string(call.Arguments) != `{"city":"Shanghai"}` {
		t.Fatalf("tool call = %+v", call)
	}
	if len(parsed.Tools) != 1 || parsed.ToolChoice == nil || parsed.ToolChoice.Mode != "named" {
		t.Fatalf("tools/choice = %+v/%+v", parsed.Tools, parsed.ToolChoice)
	}
	if parsed.ResponseFormat == nil || parsed.ResponseFormat.Type != "json_schema" || parsed.ResponseFormat.Strict == nil || !*parsed.ResponseFormat.Strict {
		t.Fatalf("response format = %+v", parsed.ResponseFormat)
	}
}

func TestParseChatCompletionRequestSupportsLegacyMaximumAndDefaults(t *testing.T) {
	t.Parallel()
	parsed, problem := parseRequestForTest(`{"model":"general-chat","messages":[{"role":"user","content":"hello"}],"max_tokens":7}`)
	if problem != nil {
		t.Fatalf("parse problem = %+v", problem)
	}
	if parsed.MaxCompletionTokens == nil || *parsed.MaxCompletionTokens != 7 || parsed.Stream {
		t.Fatalf("parsed request = %+v", parsed)
	}
}

func TestParseChatCompletionRequestRejectsUnsafeInputs(t *testing.T) {
	t.Parallel()
	validPrefix := `{"model":"general-chat","messages":[{"role":"user","content":"hello"}]`
	tests := []struct {
		name        string
		body        string
		contentType string
		encoding    string
		wantStatus  int
		wantCode    string
		wantParam   string
	}{
		{name: "missing content type", body: validPrefix + `}`, wantStatus: http.StatusUnsupportedMediaType, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "wrong content type", body: validPrefix + `}`, contentType: "text/plain", wantStatus: http.StatusUnsupportedMediaType, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "compressed", body: validPrefix + `}`, contentType: "application/json", encoding: "gzip", wantStatus: http.StatusUnsupportedMediaType, wantCode: "UNSUPPORTED_CONTENT_ENCODING"},
		{name: "empty", body: "", contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_JSON"},
		{name: "truncated", body: `{"model":`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_JSON"},
		{name: "trailing value", body: validPrefix + `} {}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_JSON"},
		{name: "root array", body: `[]`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_JSON"},
		{name: "duplicate root field", body: `{"model":"one","model":"two","messages":[]}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "DUPLICATE_FIELD", wantParam: "model"},
		{name: "duplicate nested field", body: `{"model":"general-chat","messages":[{"role":"user","role":"assistant","content":"hello"}]}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "DUPLICATE_FIELD", wantParam: "messages[0].role"},
		{name: "unknown root", body: validPrefix + `,"provider":"secret"}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "UNSUPPORTED_FIELD", wantParam: "provider"},
		{name: "unknown nested", body: `{"model":"general-chat","messages":[{"role":"user","content":"hello","metadata":{}}]}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "UNSUPPORTED_FIELD", wantParam: "messages[0].metadata"},
		{name: "missing model", body: `{"messages":[{"role":"user","content":"hello"}]}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "MISSING_REQUIRED_FIELD", wantParam: "model"},
		{name: "model type", body: `{"model":7,"messages":[{"role":"user","content":"hello"}]}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_PARAMETER_TYPE", wantParam: "model"},
		{name: "model value", body: `{"model":"bad model","messages":[{"role":"user","content":"hello"}]}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_PARAMETER", wantParam: "model"},
		{name: "missing messages", body: `{"model":"general-chat"}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "MISSING_REQUIRED_FIELD", wantParam: "messages"},
		{name: "messages type", body: `{"model":"general-chat","messages":{}}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_PARAMETER_TYPE", wantParam: "messages"},
		{name: "empty messages", body: `{"model":"general-chat","messages":[]}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_PARAMETER", wantParam: "messages"},
		{name: "invalid role", body: `{"model":"general-chat","messages":[{"role":"admin","content":"hello"}]}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_PARAMETER", wantParam: "messages[0].role"},
		{name: "missing content", body: `{"model":"general-chat","messages":[{"role":"user"}]}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "MISSING_REQUIRED_FIELD", wantParam: "messages[0].content"},
		{name: "assistant empty", body: `{"model":"general-chat","messages":[{"role":"assistant","content":null}]}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "MISSING_REQUIRED_FIELD", wantParam: "messages[0].content"},
		{name: "tool id missing", body: `{"model":"general-chat","messages":[{"role":"tool","content":"result"}]}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "MISSING_REQUIRED_FIELD", wantParam: "messages[0].tool_call_id"},
		{name: "tool calls on user", body: `{"model":"general-chat","messages":[{"role":"user","content":"hello","tool_calls":[{"id":"call_one","type":"function","function":{"name":"lookup","arguments":"{}"}}]}]}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_PARAMETER", wantParam: "messages[0].tool_calls"},
		{name: "bad content part", body: `{"model":"general-chat","messages":[{"role":"user","content":[{"type":"text","text":""}]}]}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_PARAMETER", wantParam: "messages[0].content[0].text"},
		{name: "bad image detail", body: `{"model":"general-chat","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.invalid/a","detail":"maximum"}}]}]}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_PARAMETER", wantParam: "messages[0].content[0].image_url.detail"},
		{name: "bad tool arguments", body: `{"model":"general-chat","messages":[{"role":"assistant","tool_calls":[{"id":"call_one","type":"function","function":{"name":"lookup","arguments":"[]"}}]}]}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_PARAMETER_TYPE", wantParam: "messages[0].tool_calls[0].function.arguments"},
		{name: "temperature type", body: validPrefix + `,"temperature":"hot"}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_PARAMETER_TYPE", wantParam: "temperature"},
		{name: "temperature range", body: validPrefix + `,"temperature":3}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_PARAMETER", wantParam: "temperature"},
		{name: "top p range", body: validPrefix + `,"top_p":0}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_PARAMETER", wantParam: "top_p"},
		{name: "unsafe user whitespace", body: validPrefix + `,"user":" padded "}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_PARAMETER", wantParam: "user"},
		{name: "unsafe user control", body: validPrefix + `,"user":"line\nbreak"}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_PARAMETER", wantParam: "user"},
		{name: "fractional max", body: validPrefix + `,"max_completion_tokens":1.5}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_PARAMETER_TYPE", wantParam: "max_completion_tokens"},
		{name: "conflicting max", body: validPrefix + `,"max_tokens":2,"max_completion_tokens":3}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "CONFLICTING_PARAMETERS", wantParam: "max_completion_tokens"},
		{name: "duplicate stop", body: validPrefix + `,"stop":["END","END"]}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_PARAMETER", wantParam: "stop"},
		{name: "duplicate tools", body: validPrefix + `,"tools":[{"type":"function","function":{"name":"lookup","parameters":{}}},{"type":"function","function":{"name":"lookup","parameters":{}}}]}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_PARAMETER", wantParam: "tools[1].function.name"},
		{name: "required choice without tools", body: validPrefix + `,"tool_choice":"required"}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_PARAMETER", wantParam: "tool_choice"},
		{name: "unknown named tool", body: validPrefix + `,"tools":[{"type":"function","function":{"name":"lookup","parameters":{}}}],"tool_choice":{"type":"function","function":{"name":"missing"}}}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "INVALID_PARAMETER", wantParam: "tool_choice.function.name"},
		{name: "response schema missing", body: validPrefix + `,"response_format":{"type":"json_schema","json_schema":{"name":"result"}}}`, contentType: "application/json", wantStatus: http.StatusBadRequest, wantCode: "MISSING_REQUIRED_FIELD", wantParam: "response_format.json_schema.schema"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(test.body))
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			if test.encoding != "" {
				request.Header.Set("Content-Encoding", test.encoding)
			}
			_, problem := parseChatCompletionRequest(httptest.NewRecorder(), request)
			if problem == nil {
				t.Fatal("problem = nil")
			}
			if problem.status != test.wantStatus || problem.code != test.wantCode || problem.param != test.wantParam {
				t.Fatalf("problem = %+v; want status/code/param = %d/%s/%s", problem, test.wantStatus, test.wantCode, test.wantParam)
			}
		})
	}
}

func TestParseChatCompletionRequestEnforcesBodyLimitForKnownAndStreamingLengths(t *testing.T) {
	t.Parallel()
	large := strings.Repeat("x", maximumChatRequestBytes+1)
	for _, contentLength := range []int64{int64(len(large)), -1} {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(large))
		request.Header.Set("Content-Type", "application/json")
		request.ContentLength = contentLength
		_, problem := parseChatCompletionRequest(httptest.NewRecorder(), request)
		if problem == nil || problem.status != http.StatusRequestEntityTooLarge || problem.code != "REQUEST_TOO_LARGE" {
			t.Fatalf("content length %d problem = %+v", contentLength, problem)
		}
	}
}

func TestChatCompletionsHandlerIsProtectedAndFailsExplicitlyUntilExecutionExists(t *testing.T) {
	t.Parallel()
	authenticator := &stubAuthenticator{principal: validGatewayPrincipal()}
	handler, err := NewHandler(authenticator, &stubModelCatalog{})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	correlationManager, err := correlation.New(correlation.Options{})
	if err != nil {
		t.Fatalf("correlation.New() error = %v", err)
	}
	handler = correlationManager.Middleware(handler)

	tests := []struct {
		name       string
		method     string
		body       string
		wantStatus int
		wantCode   string
	}{
		{name: "valid parsed", method: http.MethodPost, body: `{"model":"general-chat","messages":[{"role":"user","content":"hello"}]}`, wantStatus: http.StatusNotImplemented, wantCode: "CHAT_EXECUTION_NOT_IMPLEMENTED"},
		{name: "invalid rejected", method: http.MethodPost, body: `{"model":"general-chat","messages":[]}`, wantStatus: http.StatusBadRequest, wantCode: "INVALID_PARAMETER"},
		{name: "method rejected", method: http.MethodGet, body: "", wantStatus: http.StatusMethodNotAllowed, wantCode: "METHOD_NOT_ALLOWED"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, "/v1/chat/completions", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d; body = %s", response.Code, response.Body)
			}
			var envelope apierror.Envelope
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode error envelope: %v", err)
			}
			if envelope.Error.Code != test.wantCode || strings.Contains(response.Body.String(), "hello") {
				t.Fatalf("envelope = %+v; body = %s", envelope, response.Body)
			}
			if test.wantStatus == http.StatusMethodNotAllowed && response.Header().Get("Allow") != http.MethodPost {
				t.Fatalf("Allow = %q", response.Header().Get("Allow"))
			}
		})
	}
	if authenticator.calls != len(tests) {
		t.Fatalf("authenticator calls = %d, want %d", authenticator.calls, len(tests))
	}
}

func parseRequestForTest(body string) (parsedChatRequest, *requestProblem) {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	request.Header.Set("Content-Encoding", "identity")
	return parseChatCompletionRequest(httptest.NewRecorder(), request)
}
