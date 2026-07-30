package mockprovider

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type completionResponse struct {
	ID                string             `json:"id"`
	Object            string             `json:"object"`
	Created           int64              `json:"created"`
	Model             string             `json:"model"`
	Choices           []completionChoice `json:"choices"`
	Usage             usage              `json:"usage"`
	SystemFingerprint string             `json:"system_fingerprint"`
}

type completionChoice struct {
	Index        int             `json:"index"`
	Message      responseMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
}

type responseMessage struct {
	Role      string     `json:"role"`
	Content   *string    `json:"content"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type usage struct {
	PromptTokens       int                 `json:"prompt_tokens"`
	CompletionTokens   int                 `json:"completion_tokens"`
	TotalTokens        int                 `json:"total_tokens"`
	PromptTokenDetails *promptTokenDetails `json:"prompt_tokens_details,omitempty"`
	CompletionDetails  *completionDetails  `json:"completion_tokens_details,omitempty"`
}

type promptTokenDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type completionDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

type streamResponse struct {
	ID                string         `json:"id"`
	Object            string         `json:"object"`
	Created           int64          `json:"created"`
	Model             string         `json:"model"`
	Choices           []streamChoice `json:"choices"`
	Usage             *usage         `json:"usage,omitempty"`
	SystemFingerprint string         `json:"system_fingerprint"`
}

type streamChoice struct {
	Index        int         `json:"index"`
	Delta        streamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type streamDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type providerErrorEnvelope struct {
	Error providerError `json:"error"`
}

type providerError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

func writeCompletion(writer http.ResponseWriter, request chatRequest, selected scenario) {
	content := "deterministic mock response"
	response := completionResponse{
		ID: "chatcmpl-mock-" + string(selected), Object: "chat.completion", Created: 0,
		Model: request.Model, SystemFingerprint: "fp_mock_v1",
		Choices: []completionChoice{{
			Index: 0, Message: responseMessage{Role: "assistant", Content: &content}, FinishReason: "stop",
		}},
		Usage: usage{PromptTokens: 6, CompletionTokens: 4, TotalTokens: 10},
	}
	switch selected {
	case scenarioFixedUsage:
		response.Usage = usage{
			PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18,
			PromptTokenDetails: &promptTokenDetails{CachedTokens: 0},
			CompletionDetails:  &completionDetails{ReasoningTokens: 2},
		}
	case scenarioCachedUsage:
		response.Usage = usage{
			PromptTokens: 13, CompletionTokens: 3, TotalTokens: 16,
			PromptTokenDetails: &promptTokenDetails{CachedTokens: 5},
		}
	case scenarioToolCall:
		response.Choices[0].Message.Content = nil
		response.Choices[0].Message.ToolCalls = []toolCall{{
			ID: "call_mock_weather", Type: "function",
			Function: toolFunction{Name: "get_weather", Arguments: `{"city":"Shanghai"}`},
		}}
		response.Choices[0].FinishReason = "tool_calls"
		response.Usage = usage{PromptTokens: 9, CompletionTokens: 5, TotalTokens: 14}
	case scenarioNormal, scenarioSSE, scenarioDelay, scenarioRateLimit, scenarioServerError,
		scenarioDisconnect, scenarioMalformedChunk:
		// These paths are normally rendered elsewhere; keep a deterministic fallback.
	}
	writeJSON(writer, http.StatusOK, response)
}

func (handler *Handler) writeStream(writer http.ResponseWriter, request *http.Request, chat chatRequest) {
	if err := handler.markStreaming(request.Context()); err != nil {
		writeProviderError(writer, http.StatusInternalServerError, "stream_tracking_failed", "Mock stream could not start", "server_error", "")
		return
	}
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeProviderError(writer, http.StatusInternalServerError, "streaming_unsupported", "Streaming is unavailable", "server_error", "")
		return
	}
	setSSEHeaders(writer)
	writer.WriteHeader(http.StatusOK)
	finishReason := "stop"
	events := []streamResponse{
		newStreamResponse(chat.Model, []streamChoice{{Index: 0, Delta: streamDelta{Role: "assistant"}}}, nil),
		newStreamResponse(chat.Model, []streamChoice{{Index: 0, Delta: streamDelta{Content: "deterministic "}}}, nil),
		newStreamResponse(chat.Model, []streamChoice{{Index: 0, Delta: streamDelta{Content: "mock response"}}}, nil),
		newStreamResponse(chat.Model, []streamChoice{{Index: 0, Delta: streamDelta{}, FinishReason: &finishReason}}, nil),
		newStreamResponse(chat.Model, []streamChoice{}, &usage{PromptTokens: 6, CompletionTokens: 4, TotalTokens: 10}),
	}
	for _, event := range events {
		if err := writeSSEJSON(writer, event); err != nil {
			return
		}
		flusher.Flush()
		if request.Context().Err() != nil {
			return
		}
	}
	if _, err := writer.Write([]byte("data: [DONE]\n\n")); err == nil {
		flusher.Flush()
	}
}

func (handler *Handler) writeMalformedStream(writer http.ResponseWriter, request *http.Request, chat chatRequest) {
	if err := handler.markStreaming(request.Context()); err != nil {
		writeProviderError(writer, http.StatusInternalServerError, "stream_tracking_failed", "Mock stream could not start", "server_error", "")
		return
	}
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeProviderError(writer, http.StatusInternalServerError, "streaming_unsupported", "Streaming is unavailable", "server_error", "")
		return
	}
	setSSEHeaders(writer)
	writer.WriteHeader(http.StatusOK)
	first := newStreamResponse(chat.Model, []streamChoice{{Index: 0, Delta: streamDelta{Role: "assistant"}}}, nil)
	if err := writeSSEJSON(writer, first); err != nil {
		return
	}
	flusher.Flush()
	if _, err := writer.Write([]byte(`data: {"id":"chatcmpl-mock-malformed","choices":[`)); err == nil {
		flusher.Flush()
	}
}

func newStreamResponse(model string, choices []streamChoice, tokenUsage *usage) streamResponse {
	return streamResponse{
		ID: "chatcmpl-mock-sse", Object: "chat.completion.chunk", Created: 0,
		Model: model, Choices: choices, Usage: tokenUsage, SystemFingerprint: "fp_mock_v1",
	}
}

func writeSSEJSON(writer http.ResponseWriter, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = writer.Write(append(append([]byte("data: "), encoded...), '\n', '\n'))
	return err
}

func writeDisconnect(writer http.ResponseWriter) {
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		writeProviderError(writer, http.StatusInternalServerError, "disconnect_unsupported", "Disconnect simulation is unavailable", "server_error", "")
		return
	}
	connection, buffer, err := hijacker.Hijack()
	if err != nil {
		writeProviderError(writer, http.StatusInternalServerError, "disconnect_failed", "Disconnect simulation failed", "server_error", "")
		return
	}
	defer func() { _ = connection.Close() }()
	partial := `{"id":"chatcmpl-mock-disconnect","object":"chat.completion","choices":[`
	_, _ = fmt.Fprintf(
		buffer,
		"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nCache-Control: no-store\r\nX-Mock-Scenario: disconnect\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		len(partial)+128,
		partial,
	)
	_ = buffer.Flush()
}

func writeRequestFailure(writer http.ResponseWriter, failure *requestFailure) {
	writeProviderError(writer, failure.status, failure.code, failure.message, "invalid_request_error", failure.param)
}

func writeProviderError(writer http.ResponseWriter, status int, code, message, errorType, param string) {
	var paramPointer *string
	if param != "" {
		paramPointer = &param
	}
	writeJSON(writer, status, providerErrorEnvelope{Error: providerError{
		Message: message, Type: errorType, Param: paramPointer, Code: code,
	}})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	setSafeHeaders(writer, "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func setSSEHeaders(writer http.ResponseWriter) {
	setSafeHeaders(writer, "text/event-stream; charset=utf-8")
	writer.Header().Set("Connection", "keep-alive")
	writer.Header().Set("X-Accel-Buffering", "no")
}

func setSafeHeaders(writer http.ResponseWriter, contentType string) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", contentType)
	writer.Header().Set("X-Content-Type-Options", "nosniff")
}
