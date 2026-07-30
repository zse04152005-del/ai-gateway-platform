package mockadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
)

const maximumUpstreamRequestBytes = 1 << 20

type mockOptions struct {
	Scenario string `json:"mock_scenario"`
	DelayMS  *int   `json:"mock_delay_ms"`
}

type upstreamRequest struct {
	Model           string            `json:"model"`
	Messages        []upstreamMessage `json:"messages"`
	Stream          bool              `json:"stream,omitempty"`
	Temperature     *float64          `json:"temperature,omitempty"`
	TopP            *float64          `json:"top_p,omitempty"`
	MaxOutputTokens *int64            `json:"max_tokens,omitempty"`
	Stop            []string          `json:"stop,omitempty"`
	Tools           []upstreamTool    `json:"tools,omitempty"`
	ToolChoice      any               `json:"tool_choice,omitempty"`
	ResponseFormat  any               `json:"response_format,omitempty"`
	MockScenario    string            `json:"mock_scenario,omitempty"`
	MockDelayMS     *int              `json:"mock_delay_ms,omitempty"`
}

type upstreamMessage struct {
	Role       string             `json:"role"`
	Name       string             `json:"name,omitempty"`
	Content    any                `json:"content"`
	ToolCalls  []upstreamToolCall `json:"tool_calls,omitempty"`
	ToolCallID string             `json:"tool_call_id,omitempty"`
}

type upstreamContentPart struct {
	Type       string              `json:"type"`
	Text       string              `json:"text,omitempty"`
	ImageURL   *upstreamMediaValue `json:"image_url,omitempty"`
	InputAudio *upstreamMediaValue `json:"input_audio,omitempty"`
}

type upstreamMediaValue struct {
	URL       string `json:"url,omitempty"`
	Data      string `json:"data,omitempty"`
	MediaType string `json:"media_type,omitempty"`
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

// BuildRequest validates and converts one normalized request to the real local
// Mock Provider HTTP surface. It never adds Authorization or a Provider secret.
func (mock *mockAdapter) BuildRequest(ctx context.Context, input adapter.NormalizedRequest) (*http.Request, error) {
	if mock == nil || mock.endpoint == nil {
		return nil, errors.New("mock adapter is not initialized")
	}
	if ctx == nil {
		return nil, errors.New("mock adapter request context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("mock adapter request cancelled: %w", err)
	}
	if err := input.Validate(); err != nil {
		return nil, fmt.Errorf("validate normalized mock request: %w", err)
	}
	if len(input.PolicyLabels) > 0 {
		return nil, unsupported("policy_labels", "the Mock Provider has no policy-label protocol mapping")
	}
	options, err := parseMockOptions(input.ProviderOptions)
	if err != nil {
		return nil, err
	}
	if err := validateScenarioForRequest(options, input.Stream); err != nil {
		return nil, err
	}
	messages, err := buildMessages(input.Messages)
	if err != nil {
		return nil, err
	}
	requestBody := upstreamRequest{
		Model: mock.physicalModel, Messages: messages, Stream: input.Stream,
		Temperature: input.Temperature, TopP: input.TopP, MaxOutputTokens: input.MaxOutputTokens,
		Stop: append([]string(nil), input.Stop...), Tools: buildTools(input.Tools),
		ToolChoice: buildToolChoice(input.ToolChoice), ResponseFormat: buildResponseFormat(input.ResponseFormat),
		MockScenario: options.Scenario, MockDelayMS: options.DelayMS,
	}
	encoded, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("encode mock provider request: %w", err)
	}
	if len(encoded) > maximumUpstreamRequestBytes {
		return nil, ErrRequestTooLarge
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, mock.completionURL(), bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("create mock provider request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	if input.Stream {
		httpRequest.Header.Set("Accept", "text/event-stream")
	} else {
		httpRequest.Header.Set("Accept", "application/json")
	}
	httpRequest.Header.Set("X-Request-ID", input.RequestID)
	return httpRequest, nil
}

func parseMockOptions(raw json.RawMessage) (mockOptions, error) {
	if len(raw) == 0 {
		return mockOptions{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var options mockOptions
	if err := decoder.Decode(&options); err != nil {
		return mockOptions{}, unsupported("provider_options", "must match the Mock Adapter option schema")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return mockOptions{}, unsupported("provider_options", "must contain one JSON object")
	}
	if options.Scenario == "" && options.DelayMS != nil {
		return mockOptions{}, unsupported("provider_options.mock_delay_ms", "requires the delay scenario")
	}
	return options, nil
}

func validateScenarioForRequest(options mockOptions, stream bool) error {
	if options.Scenario == "" {
		return nil
	}
	supported := map[string]struct{}{
		ScenarioNormal: {}, ScenarioSSE: {}, ScenarioFixedUsage: {}, ScenarioCachedUsage: {},
		ScenarioToolCall: {}, ScenarioDelay: {}, ScenarioRateLimit: {}, ScenarioServerError: {},
		ScenarioDisconnect: {}, ScenarioMalformedChunk: {},
	}
	if _, exists := supported[options.Scenario]; !exists {
		return unsupported("provider_options.mock_scenario", "must be a supported deterministic scenario")
	}
	if (options.Scenario == ScenarioSSE || options.Scenario == ScenarioMalformedChunk) && !stream {
		return unsupported("stream", "must be true for an SSE scenario")
	}
	if stream && (options.Scenario == ScenarioNormal || options.Scenario == ScenarioFixedUsage ||
		options.Scenario == ScenarioCachedUsage || options.Scenario == ScenarioToolCall) {
		return unsupported("provider_options.mock_scenario", "selected scenario is non-streaming")
	}
	if options.Scenario == ScenarioDelay {
		if options.DelayMS != nil && (*options.DelayMS < 1 || *options.DelayMS > 5000) {
			return unsupported("provider_options.mock_delay_ms", "must be between 1 and 5000")
		}
	} else if options.DelayMS != nil {
		return unsupported("provider_options.mock_delay_ms", "is only valid for the delay scenario")
	}
	return nil
}

func buildMessages(messages []adapter.Message) ([]upstreamMessage, error) {
	upstream := make([]upstreamMessage, len(messages))
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
		upstream[index] = upstreamMessage{
			Role: string(message.Role), Name: message.Name, Content: content,
			ToolCalls: toolCalls, ToolCallID: message.ToolCallID,
		}
	}
	return upstream, nil
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
			content[index] = upstreamContentPart{
				Type: "image_url", ImageURL: &upstreamMediaValue{URL: part.Reference, MediaType: part.MediaType},
			}
		case adapter.ContentAudioReference:
			content[index] = upstreamContentPart{
				Type: "input_audio", InputAudio: &upstreamMediaValue{Data: part.Reference, MediaType: part.MediaType},
			}
		default:
			return nil, unsupported("messages.parts.kind", "is not supported by the Mock Adapter")
		}
	}
	return content, nil
}

func buildTools(tools []adapter.ToolDefinition) []upstreamTool {
	upstream := make([]upstreamTool, len(tools))
	for index, tool := range tools {
		upstream[index] = upstreamTool{
			Type: "function",
			Function: upstreamToolFunction{
				Name: tool.Name, Description: tool.Description, Parameters: append(json.RawMessage(nil), tool.InputSchema...),
			},
		}
	}
	return upstream
}

func buildToolChoice(choice *adapter.ToolChoice) any {
	if choice == nil {
		return nil
	}
	if choice.Mode != adapter.ToolChoiceNamed {
		return string(choice.Mode)
	}
	return map[string]any{
		"type":     "function",
		"function": map[string]string{"name": choice.Name},
	}
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
		"json_schema": map[string]any{
			"name": format.Name, "description": format.Description,
			"schema": append(json.RawMessage(nil), format.Schema...), "strict": format.Strict,
		},
	}
}

func (mock *mockAdapter) completionURL() string {
	target := *mock.endpoint
	switch strings.TrimSuffix(target.Path, "/") {
	case "", "/":
		target.Path = "/v1/chat/completions"
	case "/v1":
		target.Path = "/v1/chat/completions"
	case "/v1/chat/completions":
		target.Path = "/v1/chat/completions"
	}
	target.RawPath = ""
	return target.String()
}
