package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/apierror"
	"github.com/zse04152005-del/ai-gateway-platform/internal/proxy"
	"github.com/zse04152005-del/ai-gateway-platform/internal/upstreamhttp"
)

const clientClosedRequestStatus = 499

type chatCompletionResponse struct {
	ID      string               `json:"id"`
	Object  string               `json:"object"`
	Created int64                `json:"created"`
	Model   string               `json:"model"`
	Choices []chatResponseChoice `json:"choices"`
	Usage   *chatUsageResponse   `json:"usage,omitempty"`
	Gateway chatGatewayMetadata  `json:"gateway"`
}

type chatResponseChoice struct {
	Index        int                  `json:"index"`
	Message      chatAssistantMessage `json:"message"`
	FinishReason string               `json:"finish_reason"`
}

type chatAssistantMessage struct {
	Role      string                 `json:"role"`
	Content   *string                `json:"content"`
	ToolCalls []chatResponseToolCall `json:"tool_calls,omitempty"`
}

type chatResponseToolCall struct {
	ID       string                   `json:"id"`
	Type     string                   `json:"type"`
	Function chatResponseToolFunction `json:"function"`
}

type chatResponseToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatUsageResponse struct {
	PromptTokens           int64                       `json:"prompt_tokens"`
	CompletionTokens       int64                       `json:"completion_tokens"`
	TotalTokens            int64                       `json:"total_tokens"`
	PromptTokensDetails    *chatPromptTokenDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokenDetails *chatCompletionTokenDetails `json:"completion_tokens_details,omitempty"`
	Source                 adapter.UsageSource         `json:"source"`
}

type chatPromptTokenDetails struct {
	CachedTokens     *int64 `json:"cached_tokens,omitempty"`
	CacheWriteTokens *int64 `json:"cache_write_tokens,omitempty"`
	AudioTokens      *int64 `json:"audio_tokens,omitempty"`
}

type chatCompletionTokenDetails struct {
	ReasoningTokens *int64 `json:"reasoning_tokens,omitempty"`
	AudioTokens     *int64 `json:"audio_tokens,omitempty"`
}

type chatGatewayMetadata struct {
	RequestID     string `json:"request_id"`
	AttemptCount  int    `json:"attempt_count"`
	UsageComplete bool   `json:"usage_complete"`
}

func projectChatCompletion(
	result adapter.NormalizedResponse,
	logicalModel string,
	requestID string,
) (chatCompletionResponse, error) {
	if result.Validate() != nil || logicalModel == "" || requestID == "" {
		return chatCompletionResponse{}, proxy.ErrProtocol
	}
	choices := make([]chatResponseChoice, len(result.Choices))
	for index, choice := range result.Choices {
		message, err := projectAssistantMessage(choice.Message)
		if err != nil {
			return chatCompletionResponse{}, err
		}
		finishReason, err := projectFinishReason(choice.FinishReason)
		if err != nil {
			return chatCompletionResponse{}, err
		}
		choices[index] = chatResponseChoice{
			Index: choice.Index, Message: message, FinishReason: finishReason,
		}
	}
	usage, usageComplete := projectChatUsage(result.Usage)
	return chatCompletionResponse{
		ID: result.ResponseID, Object: "chat.completion", Created: result.ObservedAt.Unix(),
		Model: logicalModel, Choices: choices, Usage: usage,
		Gateway: chatGatewayMetadata{RequestID: requestID, AttemptCount: 1, UsageComplete: usageComplete},
	}, nil
}

func projectAssistantMessage(message adapter.Message) (chatAssistantMessage, error) {
	if message.Role != adapter.RoleAssistant {
		return chatAssistantMessage{}, proxy.ErrProtocol
	}
	var contentBuilder strings.Builder
	for _, part := range message.Parts {
		if part.Kind != adapter.ContentText {
			return chatAssistantMessage{}, proxy.ErrProtocol
		}
		contentBuilder.WriteString(part.Text)
	}
	var content *string
	if contentBuilder.Len() > 0 {
		value := contentBuilder.String()
		content = &value
	}
	toolCalls := make([]chatResponseToolCall, len(message.ToolCalls))
	for index, call := range message.ToolCalls {
		toolCalls[index] = chatResponseToolCall{
			ID: call.ID, Type: "function",
			Function: chatResponseToolFunction{Name: call.Name, Arguments: string(call.Arguments)},
		}
	}
	return chatAssistantMessage{Role: "assistant", Content: content, ToolCalls: toolCalls}, nil
}

func projectFinishReason(reason adapter.FinishReason) (string, error) {
	switch reason {
	case adapter.FinishStop:
		return "stop", nil
	case adapter.FinishLength:
		return "length", nil
	case adapter.FinishToolCalls:
		return "tool_calls", nil
	case adapter.FinishContentPolicy:
		return "content_filter", nil
	case adapter.FinishCancelled:
		return "cancelled", nil
	case adapter.FinishError:
		return "error", nil
	case adapter.FinishUnknown:
		return "unknown", nil
	default:
		return "", proxy.ErrProtocol
	}
}

func projectChatUsage(usage *adapter.NormalizedUsage) (*chatUsageResponse, bool) {
	if usage == nil || !usage.InputTokens.Present || !usage.OutputTokens.Present {
		return nil, false
	}
	projected := &chatUsageResponse{
		PromptTokens: usage.InputTokens.Value, CompletionTokens: usage.OutputTokens.Value,
		TotalTokens: usage.InputTokens.Value + usage.OutputTokens.Value, Source: usage.Source,
	}
	promptDetails := chatPromptTokenDetails{
		CachedTokens:     tokenPointer(usage.CacheReadTokens),
		CacheWriteTokens: tokenPointer(usage.CacheWriteTokens),
		AudioTokens:      tokenPointer(usage.AudioInputTokens),
	}
	if promptDetails.CachedTokens != nil || promptDetails.CacheWriteTokens != nil || promptDetails.AudioTokens != nil {
		projected.PromptTokensDetails = &promptDetails
	}
	completionDetails := chatCompletionTokenDetails{
		ReasoningTokens: tokenPointer(usage.ReasoningTokens), AudioTokens: tokenPointer(usage.AudioOutputTokens),
	}
	if completionDetails.ReasoningTokens != nil || completionDetails.AudioTokens != nil {
		projected.CompletionTokenDetails = &completionDetails
	}
	return projected, usage.Complete
}

func tokenPointer(count adapter.TokenCount) *int64 {
	if !count.Present {
		return nil
	}
	value := count.Value
	return &value
}

func executionPublicError(err error) *apierror.Error {
	if errors.Is(err, context.Canceled) {
		return newExecutionAPIError(clientClosedRequestStatus, "REQUEST_CANCELLED", "The request was cancelled", false, 0, err)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, upstreamhttp.ErrTimeout) {
		return newExecutionAPIError(http.StatusGatewayTimeout, "PROVIDER_TIMEOUT", "The upstream provider timed out", true, time.Second, err)
	}
	var providerFailure *proxy.ProviderError
	if errors.As(err, &providerFailure) {
		return providerPublicError(providerFailure.Detail(), err)
	}
	switch {
	case errors.Is(err, proxy.ErrAdapterUnavailable):
		return newExecutionAPIError(http.StatusServiceUnavailable, "PROVIDER_CONFIGURATION_UNAVAILABLE", "Provider configuration is temporarily unavailable", true, time.Second, err)
	case errors.Is(err, proxy.ErrTransport):
		return newExecutionAPIError(http.StatusBadGateway, "PROVIDER_CONNECTION_FAILED", "The upstream provider could not be reached", true, time.Second, err)
	case errors.Is(err, proxy.ErrProtocol):
		return newExecutionAPIError(http.StatusBadGateway, "PROVIDER_PROTOCOL_ERROR", "The upstream provider returned an invalid response", false, 0, err)
	default:
		return newExecutionAPIError(http.StatusInternalServerError, "CHAT_EXECUTION_FAILED", "Chat execution failed", false, 0, err)
	}
}

func providerPublicError(detail adapter.NormalizedError, cause error) *apierror.Error {
	retryAfter := time.Duration(0)
	if detail.RetryAfter != nil {
		retryAfter = *detail.RetryAfter
	}
	switch detail.Category {
	case adapter.ErrorRateLimit:
		return newExecutionAPIError(http.StatusTooManyRequests, "PROVIDER_RATE_LIMITED", "The upstream provider rate limited the request", detail.Retryable, retryAfter, cause)
	case adapter.ErrorTimeout:
		return newExecutionAPIError(http.StatusGatewayTimeout, "PROVIDER_TIMEOUT", "The upstream provider timed out", detail.Retryable, retryAfter, cause)
	case adapter.ErrorCapacity, adapter.ErrorProvider5xx:
		return newExecutionAPIError(http.StatusServiceUnavailable, "PROVIDER_UNAVAILABLE", "The upstream provider is temporarily unavailable", detail.Retryable, retryAfter, cause)
	case adapter.ErrorContentPolicy:
		return newExecutionAPIError(http.StatusForbidden, "CONTENT_POLICY_REJECTED", "The request was rejected by a content policy", false, 0, cause)
	case adapter.ErrorCancelled:
		return newExecutionAPIError(clientClosedRequestStatus, "REQUEST_CANCELLED", "The request was cancelled", false, 0, cause)
	case adapter.ErrorAuth, adapter.ErrorPermission:
		return newExecutionAPIError(http.StatusBadGateway, "PROVIDER_CREDENTIAL_ERROR", "The upstream provider rejected gateway credentials", false, 0, cause)
	case adapter.ErrorInvalidRequest, adapter.ErrorContextLength:
		return newExecutionAPIError(http.StatusBadGateway, "PROVIDER_REQUEST_REJECTED", "The upstream provider rejected the normalized request", false, 0, cause)
	case adapter.ErrorProtocol:
		return newExecutionAPIError(http.StatusBadGateway, "PROVIDER_PROTOCOL_ERROR", "The upstream provider returned an invalid response", false, 0, cause)
	case adapter.ErrorUnknown:
		return newExecutionAPIError(http.StatusBadGateway, "PROVIDER_ERROR", "The upstream provider returned an error", false, 0, cause)
	default:
		return newExecutionAPIError(http.StatusBadGateway, "PROVIDER_ERROR", "The upstream provider returned an error", false, 0, cause)
	}
}

func newExecutionAPIError(
	status int,
	code string,
	message string,
	retryable bool,
	retryAfter time.Duration,
	cause error,
) *apierror.Error {
	return apierror.MustNew(apierror.Definition{
		Status: status, Code: code, Message: message, Type: "provider_error",
		Retryable: retryable, RetryAfter: retryAfter,
	}, cause)
}

func writeChatCompletionJSON(writer http.ResponseWriter, response chatCompletionResponse, requestID string) {
	body, err := json.Marshal(response)
	if err != nil {
		apierror.WriteHTTP(writer, err, requestID, "gateway_error")
		return
	}
	body = append(body, '\n')
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("X-Request-Id", requestID)
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(body)
}
