package mockprovider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/httpserver"
)

type waitFunc func(context.Context, time.Duration) error
type markStreamingFunc func(context.Context) error

// Handler serves the deterministic OpenAI-compatible mock surface.
type Handler struct {
	wait          waitFunc
	markStreaming markStreamingFunc
}

// NewHandler creates a production handler using cancellable timers and the shared HTTP lifecycle tracker.
func NewHandler() *Handler {
	return newHandler(waitForContext, httpserver.MarkStreaming)
}

func newHandler(wait waitFunc, markStreaming markStreamingFunc) *Handler {
	return &Handler{wait: wait, markStreaming: markStreaming}
}

// ServeHTTP exposes POST /v1/chat/completions and safe errors for every other request.
func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/v1/chat/completions" {
		writeProviderError(writer, http.StatusNotFound, "not_found", "The requested resource was not found", "invalid_request_error", "")
		return
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeProviderError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "Only POST is allowed", "invalid_request_error", "")
		return
	}

	chat, failure := decodeChatRequest(writer, request)
	if failure != nil {
		writeRequestFailure(writer, failure)
		return
	}
	selected, delayMS, failure := resolveScenario(request, chat)
	if failure != nil {
		writeRequestFailure(writer, failure)
		return
	}
	writer.Header().Set(ScenarioHeader, string(selected))

	switch selected {
	case scenarioRateLimit:
		writer.Header().Set("Retry-After", "1")
		writeProviderError(writer, http.StatusTooManyRequests, "rate_limit_exceeded", "Mock rate limit exceeded", "rate_limit_error", "")
	case scenarioServerError:
		writeProviderError(writer, http.StatusServiceUnavailable, "mock_provider_unavailable", "Mock provider is unavailable", "server_error", "")
	case scenarioDisconnect:
		writeDisconnect(writer)
	case scenarioMalformedChunk:
		handler.writeMalformedStream(writer, request, chat)
	case scenarioSSE:
		handler.writeStream(writer, request, chat)
	case scenarioDelay:
		if err := handler.wait(request.Context(), time.Duration(delayMS)*time.Millisecond); err != nil {
			return
		}
		if chat.Stream {
			handler.writeStream(writer, request, chat)
		} else {
			writeCompletion(writer, chat, scenarioNormal)
		}
	case scenarioNormal, scenarioFixedUsage, scenarioCachedUsage, scenarioToolCall:
		writeCompletion(writer, chat, selected)
	default:
		writeProviderError(writer, http.StatusInternalServerError, "mock_internal_error", "Mock provider failed", "server_error", "")
	}
}

func decodeChatRequest(writer http.ResponseWriter, request *http.Request) (chatRequest, *requestFailure) {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return chatRequest{}, &requestFailure{
			status: http.StatusUnsupportedMediaType, code: "unsupported_media_type",
			message: "Content-Type must be application/json", param: "content_type",
		}
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maximumRequestBody)
	decoder := json.NewDecoder(request.Body)
	var chat chatRequest
	if err := decoder.Decode(&chat); err != nil {
		var maximum *http.MaxBytesError
		if errors.As(err, &maximum) {
			return chatRequest{}, &requestFailure{
				status: http.StatusRequestEntityTooLarge, code: "request_too_large",
				message: "Request body exceeds 1 MiB", param: "body",
			}
		}
		return chatRequest{}, invalidRequest("invalid_json", "Request body must be valid JSON", "body")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return chatRequest{}, invalidRequest("invalid_json", "Request body must contain one JSON value", "body")
	}
	if failure := validateChatRequest(chat); failure != nil {
		return chatRequest{}, failure
	}
	return chat, nil
}

func waitForContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}
