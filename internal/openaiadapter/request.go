package openaiadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
)

const maximumUpstreamRequestBytes = 1 << 20

type upstreamRequest struct {
	Model               string            `json:"model"`
	Messages            []upstreamMessage `json:"messages"`
	Stream              bool              `json:"stream,omitempty"`
	StreamOptions       *streamOptions    `json:"stream_options,omitempty"`
	Temperature         *float64          `json:"temperature,omitempty"`
	TopP                *float64          `json:"top_p,omitempty"`
	MaxCompletionTokens *int64            `json:"max_completion_tokens,omitempty"`
	Stop                []string          `json:"stop,omitempty"`
	Tools               []upstreamTool    `json:"tools,omitempty"`
	ToolChoice          any               `json:"tool_choice,omitempty"`
	ResponseFormat      any               `json:"response_format,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type upstreamMessage struct {
	Role       string             `json:"role"`
	Name       string             `json:"name,omitempty"`
	Content    any                `json:"content,omitempty"`
	ToolCalls  []upstreamToolCall `json:"tool_calls,omitempty"`
	ToolCallID string             `json:"tool_call_id,omitempty"`
}

type upstreamContentPart struct {
	Type     string              `json:"type"`
	Text     string              `json:"text,omitempty"`
	ImageURL *upstreamImageValue `json:"image_url,omitempty"`
}

type upstreamImageValue struct {
	URL string `json:"url"`
}

type upstreamTool struct {
	Type     string               `json:"type"`
	Function upstreamToolFunction `json:"function"`
}

type upstreamToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type upstreamToolCall struct {
	ID       string                   `json:"id"`
	Type     string                   `json:"type"`
	Function upstreamToolCallFunction `json:"function"`
}

type upstreamToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type responseSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema"`
	Strict      *bool           `json:"strict,omitempty"`
}

// BuildRequest validates and converts one normalized request to OpenAI Chat Completions.
func (openAI *openAIAdapter) BuildRequest(ctx context.Context, input adapter.NormalizedRequest) (*http.Request, error) {
	if openAI == nil || openAI.endpoint == nil || openAI.secrets == nil {
		return nil, errors.New("openai adapter is not initialized")
	}
	if ctx == nil {
		return nil, errors.New("openai adapter request context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("openai adapter request cancelled: %w", err)
	}
	if err := input.Validate(); err != nil {
		return nil, fmt.Errorf("validate normalized openai request: %w", err)
	}
	if err := openAI.validateCapabilities(input); err != nil {
		return nil, err
	}
	messages, err := buildMessages(input.Messages)
	if err != nil {
		return nil, err
	}
	requestBody := upstreamRequest{
		Model: openAI.physicalModel, Messages: messages, Stream: input.Stream,
		Temperature: input.Temperature, TopP: input.TopP, MaxCompletionTokens: input.MaxOutputTokens,
		Stop: append([]string(nil), input.Stop...), Tools: buildTools(input.Tools),
		ToolChoice: buildToolChoice(input.ToolChoice), ResponseFormat: buildResponseFormat(input.ResponseFormat),
	}
	if input.Stream {
		requestBody.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	encoded, err := json.Marshal(requestBody)
	if err != nil {
		return nil, errors.New("encode openai provider request failed")
	}
	if len(encoded) > maximumUpstreamRequestBytes {
		return nil, ErrRequestTooLarge
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, openAI.completionURL(), bytes.NewReader(encoded))
	if err != nil {
		return nil, errors.New("create openai provider request failed")
	}
	credential, err := openAI.secrets.Resolve(ctx, openAI.secretLocator)
	if err != nil || !validCredential(credential) {
		clear(credential)
		return nil, ErrCredentialUnavailable
	}
	httpRequest.Header.Set("Authorization", "Bearer "+string(credential))
	clear(credential)
	httpRequest.Header.Set("Content-Type", "application/json")
	if input.Stream {
		httpRequest.Header.Set("Accept", "text/event-stream")
	} else {
		httpRequest.Header.Set("Accept", "application/json")
	}
	httpRequest.Header.Set("User-Agent", openAI.userAgent)
	httpRequest.Header.Set("X-Client-Request-Id", input.RequestID)
	return httpRequest, nil
}

func (openAI *openAIAdapter) validateCapabilities(input adapter.NormalizedRequest) error {
	if len(input.ProviderOptions) > 0 {
		return unsupported("provider_options", "no provider-specific passthrough options are allowed")
	}
	if len(input.PolicyLabels) > 0 {
		return unsupported("policy_labels", "the Chat Completions protocol has no policy-label mapping")
	}
	if input.Stream && !openAI.capabilities.Stream {
		return unsupported("stream", "the selected deployment does not declare streaming")
	}
	if len(input.Tools) > 0 && !openAI.capabilities.Tools {
		return unsupported("tools", "the selected deployment does not declare tool calling")
	}
	if input.ResponseFormat != nil && input.ResponseFormat.Type != adapter.ResponseFormatText && !openAI.capabilities.StructuredOutput {
		return unsupported("response_format", "the selected deployment does not declare structured output")
	}
	if input.MaxOutputTokens != nil && openAI.capabilities.MaxOutputTokens > 0 && *input.MaxOutputTokens > openAI.capabilities.MaxOutputTokens {
		return unsupported("max_output_tokens", "exceeds the selected deployment capability")
	}
	return nil
}

func buildMessages(messages []adapter.Message) ([]upstreamMessage, error) {
	result := make([]upstreamMessage, len(messages))
	for index := range messages {
		message := messages[index]
		content, err := buildContent(message.Parts)
		if err != nil {
			return nil, fmt.Errorf("message[%d]: %w", index, err)
		}
		toolCalls := make([]upstreamToolCall, len(message.ToolCalls))
		for toolIndex := range message.ToolCalls {
			call := message.ToolCalls[toolIndex]
			toolCalls[toolIndex] = upstreamToolCall{
				ID: call.ID, Type: "function",
				Function: upstreamToolCallFunction{Name: call.Name, Arguments: string(call.Arguments)},
			}
		}
		result[index] = upstreamMessage{
			Role: string(message.Role), Name: message.Name, Content: content,
			ToolCalls: toolCalls, ToolCallID: message.ToolCallID,
		}
	}
	return result, nil
}

func buildContent(parts []adapter.ContentPart) (any, error) {
	if len(parts) == 0 {
		return nil, nil
	}
	if len(parts) == 1 && parts[0].Kind == adapter.ContentText {
		return parts[0].Text, nil
	}
	content := make([]upstreamContentPart, len(parts))
	for index, part := range parts {
		switch part.Kind {
		case adapter.ContentText:
			content[index] = upstreamContentPart{Type: "text", Text: part.Text}
		case adapter.ContentImageReference:
			content[index] = upstreamContentPart{Type: "image_url", ImageURL: &upstreamImageValue{URL: part.Reference}}
		case adapter.ContentAudioReference:
			return nil, unsupported("messages.parts.audio_reference", "audio input mapping is not implemented")
		default:
			return nil, unsupported("messages.parts.kind", "content kind is not implemented")
		}
	}
	return content, nil
}

func buildTools(tools []adapter.ToolDefinition) []upstreamTool {
	result := make([]upstreamTool, len(tools))
	for index, tool := range tools {
		result[index] = upstreamTool{Type: "function", Function: upstreamToolFunction{
			Name: tool.Name, Description: tool.Description,
			Parameters: append(json.RawMessage(nil), tool.InputSchema...),
		}}
	}
	return result
}

func buildToolChoice(choice *adapter.ToolChoice) any {
	if choice == nil {
		return nil
	}
	if choice.Mode != adapter.ToolChoiceNamed {
		return string(choice.Mode)
	}
	return map[string]any{"type": "function", "function": map[string]string{"name": choice.Name}}
}

func buildResponseFormat(format *adapter.ResponseFormat) any {
	if format == nil {
		return nil
	}
	if format.Type != adapter.ResponseFormatJSONSchema {
		return map[string]string{"type": string(format.Type)}
	}
	return map[string]any{
		"type": "json_schema",
		"json_schema": responseSchema{
			Name: format.Name, Description: format.Description,
			Schema: append(json.RawMessage(nil), format.Schema...), Strict: format.Strict,
		},
	}
}

func validCredential(value []byte) bool {
	if len(value) == 0 || len(value) > 16*1024 || !bytes.Equal(value, bytes.TrimSpace(value)) {
		return false
	}
	return !bytes.ContainsAny(value, "\x00\r\n")
}

func (openAI *openAIAdapter) completionURL() string {
	target := *openAI.endpoint
	switch strings.TrimSuffix(target.Path, "/") {
	case "", "/", "/v1":
		target.Path = "/v1/chat/completions"
	case "/v1/chat/completions":
		target.Path = "/v1/chat/completions"
	}
	target.RawPath = ""
	return target.String()
}
