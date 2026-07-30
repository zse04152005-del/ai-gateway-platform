package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/apierror"
)

const idempotencyKeyHeader = "Idempotency-Key"

var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`)

// normalizedChatRequest keeps execution metadata owned by the gateway outside
// the provider-neutral model request. The idempotency key is available to the
// later replay-protection stage but is never sent to a provider by an adapter.
type normalizedChatRequest struct {
	ProviderRequest adapter.NormalizedRequest
	IdempotencyKey  string
}

// Clone isolates later attempt-local mutation from the parsed request and from
// other attempts. Strings are immutable and the adapter clone owns all slices,
// pointers, and raw JSON values.
func (request normalizedChatRequest) Clone() normalizedChatRequest {
	request.ProviderRequest = request.ProviderRequest.Clone()
	return request
}

// LogValue exposes only safe shape facts. The opaque end-user reference and
// idempotency value are deliberately represented as presence booleans.
func (request normalizedChatRequest) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Any("providerRequest", request.ProviderRequest),
		slog.Bool("hasEndUserReference", request.ProviderRequest.EndUserReference != ""),
		slog.Bool("hasIdempotencyKey", request.IdempotencyKey != ""),
	)
}

func normalizeChatCompletionRequest(
	parsed parsedChatRequest,
	requestID string,
	idempotencyValues []string,
) (normalizedChatRequest, error) {
	if requestID == "" {
		return normalizedChatRequest{}, errors.New("trusted correlation request ID is missing")
	}
	idempotencyKey, err := normalizeIdempotencyKey(idempotencyValues)
	if err != nil {
		return normalizedChatRequest{}, err
	}
	messages := make([]adapter.Message, len(parsed.Messages))
	for index := range parsed.Messages {
		messages[index] = normalizeMessage(parsed.Messages[index])
	}
	normalized := adapter.NormalizedRequest{
		RequestID: requestID, LogicalModel: parsed.Model,
		EndUserReference: parsed.User, Messages: messages, Stream: parsed.Stream,
		Temperature: cloneFloatPointer(parsed.Temperature), TopP: cloneFloatPointer(parsed.TopP),
		MaxOutputTokens: cloneIntegerPointer(parsed.MaxCompletionTokens),
		Stop:            append([]string(nil), parsed.Stop...),
		Tools:           normalizeTools(parsed.Tools),
		ToolChoice:      normalizeToolChoice(parsed.ToolChoice),
		ResponseFormat:  normalizeResponseFormat(parsed.ResponseFormat),
	}
	if err := normalized.Validate(); err != nil {
		return normalizedChatRequest{}, fmt.Errorf("validate normalized chat request: %w", err)
	}
	return normalizedChatRequest{ProviderRequest: normalized, IdempotencyKey: idempotencyKey}, nil
}

func normalizeIdempotencyKey(values []string) (string, error) {
	if len(values) == 0 {
		return "", nil
	}
	if len(values) != 1 || values[0] != strings.TrimSpace(values[0]) || !idempotencyKeyPattern.MatchString(values[0]) {
		return "", apierror.MustNew(apierror.Definition{
			Status: http.StatusBadRequest, Code: "INVALID_IDEMPOTENCY_KEY",
			Message: "Idempotency-Key must be one safe 8-128 character value",
			Type:    "invalid_request_error", Param: idempotencyKeyHeader,
		}, nil)
	}
	return values[0], nil
}

func normalizeMessage(message parsedChatMessage) adapter.Message {
	parts := make([]adapter.ContentPart, len(message.Content))
	for index, part := range message.Content {
		switch part.Kind {
		case "text":
			parts[index] = adapter.ContentPart{Kind: adapter.ContentText, Text: part.Text}
		case "image_url":
			parts[index] = adapter.ContentPart{
				Kind: adapter.ContentImageReference, Reference: part.ImageURL, Detail: part.ImageDetail,
			}
		default:
			parts[index] = adapter.ContentPart{Kind: adapter.ContentKind(part.Kind)}
		}
	}
	toolCalls := make([]adapter.ToolCall, len(message.ToolCalls))
	for index, call := range message.ToolCalls {
		toolCalls[index] = adapter.ToolCall{
			ID: call.ID, Name: call.Name, Arguments: append(json.RawMessage(nil), call.Arguments...),
		}
	}
	return adapter.Message{
		Role: adapter.MessageRole(message.Role), Name: message.Name,
		Parts: parts, ToolCalls: toolCalls, ToolCallID: message.ToolCallID,
	}
}

func normalizeTools(tools []parsedChatTool) []adapter.ToolDefinition {
	normalized := make([]adapter.ToolDefinition, len(tools))
	for index, tool := range tools {
		normalized[index] = adapter.ToolDefinition{
			Name: tool.Name, Description: tool.Description,
			InputSchema: append(json.RawMessage(nil), tool.Parameters...),
		}
	}
	return normalized
}

func normalizeToolChoice(choice *parsedToolChoice) *adapter.ToolChoice {
	if choice == nil {
		return nil
	}
	mode := adapter.ToolChoiceMode(choice.Mode)
	if choice.Mode == "named" {
		mode = adapter.ToolChoiceNamed
	}
	return &adapter.ToolChoice{Mode: mode, Name: choice.Name}
}

func normalizeResponseFormat(format *parsedResponseFormat) *adapter.ResponseFormat {
	if format == nil {
		return nil
	}
	return &adapter.ResponseFormat{
		Type: adapter.ResponseFormatType(format.Type), Name: format.Name,
		Description: format.Description, Schema: append(json.RawMessage(nil), format.Schema...),
		Strict: cloneBoolPointer(format.Strict),
	}
}

func cloneFloatPointer(value *float64) *float64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneIntegerPointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneBoolPointer(value *bool) *bool {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

var _ slog.LogValuer = normalizedChatRequest{}
