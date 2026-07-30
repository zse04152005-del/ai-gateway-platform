package mockadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sort"
	"strings"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
)

const maximumJSONResponseBytes = 1 << 20

type completionEnvelope struct {
	ID                string            `json:"id"`
	Object            string            `json:"object"`
	Created           int64             `json:"created"`
	Model             string            `json:"model"`
	Choices           []json.RawMessage `json:"choices"`
	Usage             json.RawMessage   `json:"usage"`
	SystemFingerprint string            `json:"system_fingerprint"`
}

type completionChoice struct {
	Index        int             `json:"index"`
	Message      json.RawMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
}

type completionMessage struct {
	Role      string            `json:"role"`
	Content   json.RawMessage   `json:"content"`
	ToolCalls []json.RawMessage `json:"tool_calls"`
}

type completionToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function json.RawMessage `json:"function"`
}

type completionToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ParseResponse consumes and closes one bounded non-streaming response.
func (mock *mockAdapter) ParseResponse(
	ctx context.Context,
	response *http.Response,
) (adapter.NormalizedResponse, error) {
	if mock == nil || mock.now == nil {
		return adapter.NormalizedResponse{}, errors.New("mock adapter is not initialized")
	}
	if response == nil || response.Body == nil {
		return adapter.NormalizedResponse{}, protocolError("parse_response", "missing_response", nil)
	}
	if ctx == nil {
		_ = response.Body.Close()
		return adapter.NormalizedResponse{}, errors.New("mock adapter response context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		_ = response.Body.Close()
		return adapter.NormalizedResponse{}, fmt.Errorf("mock adapter response cancelled: %w", err)
	}
	body, err := readBoundedBody(response.Body, maximumJSONResponseBytes)
	if err != nil {
		return adapter.NormalizedResponse{}, err
	}
	if response.StatusCode != http.StatusOK {
		return adapter.NormalizedResponse{}, mock.NormalizeError(ctx, response, body)
	}
	if err := requireMediaType(response.Header.Get("Content-Type"), "application/json"); err != nil {
		return adapter.NormalizedResponse{}, protocolError("parse_response", "invalid_content_type", err)
	}
	if !onlyObjectFields(body, "id", "object", "created", "model", "choices", "usage", "system_fingerprint") {
		return adapter.NormalizedResponse{}, protocolError("parse_response", "unknown_response_field", nil)
	}
	var envelope completionEnvelope
	if err := decodeOneJSON(body, &envelope); err != nil {
		return adapter.NormalizedResponse{}, protocolError("parse_response", "invalid_json", err)
	}
	if envelope.Object != "chat.completion" || envelope.Model != mock.physicalModel {
		return adapter.NormalizedResponse{}, protocolError("parse_response", "unexpected_response_identity", nil)
	}
	choices := make([]adapter.NormalizedChoice, len(envelope.Choices))
	for index := range envelope.Choices {
		choice, parseErr := parseChoice(envelope.Choices[index])
		if parseErr != nil {
			return adapter.NormalizedResponse{}, parseErr
		}
		choices[index] = choice
	}
	var usage *adapter.NormalizedUsage
	if len(envelope.Usage) > 0 && !bytes.Equal(bytes.TrimSpace(envelope.Usage), []byte("null")) {
		parsedUsage, parseErr := parseUsage(envelope.Usage, true)
		if parseErr != nil {
			return adapter.NormalizedResponse{}, parseErr
		}
		usage = &parsedUsage
	}
	normalized := adapter.NormalizedResponse{
		ResponseID: envelope.ID, Model: envelope.Model, Choices: choices, Usage: usage,
		ProviderRequestID: safeProviderRequestID(response.Header.Get("X-Request-ID")),
		ObservedAt:        mock.now().UTC(),
	}
	if err := normalized.Validate(); err != nil {
		return adapter.NormalizedResponse{}, protocolError("parse_response", "invalid_normalized_response", err)
	}
	return normalized, nil
}

func parseChoice(raw json.RawMessage) (adapter.NormalizedChoice, error) {
	if !onlyObjectFields(raw, "index", "message", "finish_reason") {
		return adapter.NormalizedChoice{}, protocolError("parse_choice", "unknown_choice_field", nil)
	}
	var choice completionChoice
	if err := decodeOneJSON(raw, &choice); err != nil {
		return adapter.NormalizedChoice{}, protocolError("parse_choice", "invalid_choice", err)
	}
	message, err := parseMessage(choice.Message)
	if err != nil {
		return adapter.NormalizedChoice{}, err
	}
	finishReason, providerReason := normalizeFinishReason(choice.FinishReason)
	return adapter.NormalizedChoice{
		Index: choice.Index, Message: message, FinishReason: finishReason, ProviderFinishReason: providerReason,
	}, nil
}

func parseMessage(raw json.RawMessage) (adapter.Message, error) {
	if !onlyObjectFields(raw, "role", "content", "tool_calls") {
		return adapter.Message{}, protocolError("parse_message", "unknown_message_field", nil)
	}
	var message completionMessage
	if err := decodeOneJSON(raw, &message); err != nil {
		return adapter.Message{}, protocolError("parse_message", "invalid_message", err)
	}
	normalized := adapter.Message{Role: adapter.MessageRole(message.Role)}
	trimmedContent := bytes.TrimSpace(message.Content)
	if len(trimmedContent) > 0 && !bytes.Equal(trimmedContent, []byte("null")) {
		var content string
		if err := json.Unmarshal(trimmedContent, &content); err != nil {
			return adapter.Message{}, protocolError("parse_message", "invalid_content", err)
		}
		if content != "" {
			normalized.Parts = []adapter.ContentPart{{Kind: adapter.ContentText, Text: content}}
		}
	}
	normalized.ToolCalls = make([]adapter.ToolCall, len(message.ToolCalls))
	for index := range message.ToolCalls {
		call, err := parseToolCall(message.ToolCalls[index])
		if err != nil {
			return adapter.Message{}, err
		}
		normalized.ToolCalls[index] = call
	}
	return normalized, nil
}

func parseToolCall(raw json.RawMessage) (adapter.ToolCall, error) {
	if !onlyObjectFields(raw, "id", "type", "function") {
		return adapter.ToolCall{}, protocolError("parse_tool_call", "unknown_tool_call_field", nil)
	}
	var call completionToolCall
	if err := decodeOneJSON(raw, &call); err != nil {
		return adapter.ToolCall{}, protocolError("parse_tool_call", "invalid_tool_call", err)
	}
	if call.Type != "function" || !onlyObjectFields(call.Function, "name", "arguments") {
		return adapter.ToolCall{}, protocolError("parse_tool_call", "invalid_tool_call_type", nil)
	}
	var function completionToolFunction
	if err := decodeOneJSON(call.Function, &function); err != nil {
		return adapter.ToolCall{}, protocolError("parse_tool_call", "invalid_tool_function", err)
	}
	arguments := json.RawMessage(function.Arguments)
	if !onlyObjectFields(arguments) {
		return adapter.ToolCall{}, protocolError("parse_tool_call", "invalid_tool_arguments", nil)
	}
	return adapter.ToolCall{ID: call.ID, Name: function.Name, Arguments: append(json.RawMessage(nil), arguments...)}, nil
}

func parseUsage(raw json.RawMessage, complete bool) (adapter.NormalizedUsage, error) {
	var fields map[string]json.RawMessage
	if err := decodeOneJSON(raw, &fields); err != nil || fields == nil {
		return adapter.NormalizedUsage{}, protocolError("parse_usage", "invalid_usage", err)
	}
	input, err := parseTokenCount(fields, "prompt_tokens")
	if err != nil {
		return adapter.NormalizedUsage{}, err
	}
	output, err := parseTokenCount(fields, "completion_tokens")
	if err != nil {
		return adapter.NormalizedUsage{}, err
	}
	total, err := parseTokenCount(fields, "total_tokens")
	if err != nil {
		return adapter.NormalizedUsage{}, err
	}
	if total.Present && input.Present && output.Present && total.Value != input.Value+output.Value {
		return adapter.NormalizedUsage{}, protocolError("parse_usage", "inconsistent_total_tokens", nil)
	}
	unmapped := unknownPointers(fields, "prompt_tokens", "completion_tokens", "total_tokens", "prompt_tokens_details", "completion_tokens_details")
	cacheRead, nestedUnknown, err := parseUsageDetails(fields["prompt_tokens_details"], "cached_tokens", "/prompt_tokens_details")
	if err != nil {
		return adapter.NormalizedUsage{}, err
	}
	unmapped = append(unmapped, nestedUnknown...)
	reasoning, nestedUnknown, err := parseUsageDetails(fields["completion_tokens_details"], "reasoning_tokens", "/completion_tokens_details")
	if err != nil {
		return adapter.NormalizedUsage{}, err
	}
	unmapped = append(unmapped, nestedUnknown...)
	sort.Strings(unmapped)
	evidence, err := adapter.NewUsageEvidence(raw)
	if err != nil {
		return adapter.NormalizedUsage{}, protocolError("parse_usage", "invalid_usage_evidence", err)
	}
	usage := adapter.NormalizedUsage{
		InputTokens: input, OutputTokens: output, CacheReadTokens: cacheRead, ReasoningTokens: reasoning,
		Source: adapter.UsageSourceProvider, Complete: complete,
		RawEvidence: evidence, UnmappedFields: unmapped,
	}
	if err := usage.Validate(); err != nil {
		return adapter.NormalizedUsage{}, protocolError("parse_usage", "invalid_normalized_usage", err)
	}
	return usage, nil
}

func parseTokenCount(fields map[string]json.RawMessage, name string) (adapter.TokenCount, error) {
	raw, exists := fields[name]
	if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return adapter.TokenCount{}, nil
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil || value < 0 {
		return adapter.TokenCount{}, protocolError("parse_usage", "invalid_"+name, err)
	}
	return adapter.Tokens(value), nil
}

func parseUsageDetails(raw json.RawMessage, known, prefix string) (adapter.TokenCount, []string, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return adapter.TokenCount{}, nil, nil
	}
	var fields map[string]json.RawMessage
	if err := decodeOneJSON(raw, &fields); err != nil || fields == nil {
		return adapter.TokenCount{}, nil, protocolError("parse_usage", "invalid_usage_details", err)
	}
	count, err := parseTokenCount(fields, known)
	if err != nil {
		return adapter.TokenCount{}, nil, err
	}
	unknown := make([]string, 0)
	for name := range fields {
		if name != known {
			unknown = append(unknown, prefix+"/"+escapeJSONPointer(name))
		}
	}
	return count, unknown, nil
}

func unknownPointers(fields map[string]json.RawMessage, known ...string) []string {
	allowed := make(map[string]struct{}, len(known))
	for _, name := range known {
		allowed[name] = struct{}{}
	}
	unknown := make([]string, 0)
	for name := range fields {
		if _, exists := allowed[name]; !exists {
			unknown = append(unknown, "/"+escapeJSONPointer(name))
		}
	}
	return unknown
}

func escapeJSONPointer(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}

func normalizeFinishReason(providerReason string) (adapter.FinishReason, string) {
	switch providerReason {
	case "stop":
		return adapter.FinishStop, providerReason
	case "length":
		return adapter.FinishLength, providerReason
	case "tool_calls":
		return adapter.FinishToolCalls, providerReason
	case "content_filter":
		return adapter.FinishContentPolicy, providerReason
	default:
		return adapter.FinishUnknown, providerReason
	}
}

func readBoundedBody(body io.ReadCloser, maximum int64) ([]byte, error) {
	defer func() { _ = body.Close() }()
	limited, err := io.ReadAll(io.LimitReader(body, maximum+1))
	if err != nil {
		return nil, protocolError("read_response", "read_failed", err)
	}
	if int64(len(limited)) > maximum {
		return nil, protocolError("read_response", "response_too_large", ErrResponseTooLarge)
	}
	return limited, nil
}

func requireMediaType(header, wanted string) error {
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil || mediaType != wanted {
		return fmt.Errorf("content type must be %s", wanted)
	}
	return nil
}

func decodeOneJSON(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("JSON must contain exactly one value")
	}
	return nil
}

func onlyObjectFields(raw []byte, allowed ...string) bool {
	var fields map[string]json.RawMessage
	if err := decodeOneJSON(raw, &fields); err != nil || fields == nil {
		return false
	}
	if len(allowed) == 0 {
		return true
	}
	allowlist := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allowlist[name] = struct{}{}
	}
	for name := range fields {
		if _, exists := allowlist[name]; !exists {
			return false
		}
	}
	return true
}
