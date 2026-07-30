package adapter_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
)

func TestNormalizedRequestValidateAndClone(t *testing.T) {
	t.Parallel()

	request := validRequest()
	if err := request.Validate(); err != nil {
		t.Fatalf("validate request: %v", err)
	}

	cloned := request.Clone()
	cloned.Messages[0].Parts[0].Text = "changed"
	cloned.Tools[0].InputSchema[2] = 'X'
	cloned.ProviderOptions[2] = 'X'
	*cloned.Temperature = 0.9
	*cloned.ResponseFormat.Strict = false
	cloned.PolicyLabels[0] = "changed"

	if request.Messages[0].Parts[0].Text != "system-secret-prompt" {
		t.Fatal("message content aliases clone")
	}
	if string(request.Tools[0].InputSchema) != `{"type":"object","properties":{"city":{"type":"string"}}}` {
		t.Fatal("tool schema aliases clone")
	}
	if string(request.ProviderOptions) != `{"tier":"standard"}` {
		t.Fatal("provider options alias clone")
	}
	if *request.Temperature != 0.2 || !*request.ResponseFormat.Strict {
		t.Fatal("optional scalar aliases clone")
	}
	if request.PolicyLabels[0] != "content.internal" {
		t.Fatal("policy labels alias clone")
	}
}

func TestNormalizedRequestValidationFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mutate    func(*adapter.NormalizedRequest)
		wantField string
	}{
		{"request id", func(request *adapter.NormalizedRequest) { request.RequestID = "bad id" }, "request_id"},
		{"end user", func(request *adapter.NormalizedRequest) { request.EndUserReference = "unsafe\nuser" }, "end_user_reference"},
		{"messages missing", func(request *adapter.NormalizedRequest) { request.Messages = nil }, "messages"},
		{"unknown role", func(request *adapter.NormalizedRequest) { request.Messages[0].Role = "provider_role" }, "messages[0].role"},
		{"mixed content part", func(request *adapter.NormalizedRequest) { request.Messages[0].Parts[0].Reference = "ref" }, "messages[0].parts[0]"},
		{"text image detail", func(request *adapter.NormalizedRequest) { request.Messages[0].Parts[0].Detail = "high" }, "messages[0].parts[0]"},
		{"invalid image detail", func(request *adapter.NormalizedRequest) {
			request.Messages[0].Parts[0] = adapter.ContentPart{Kind: adapter.ContentImageReference, Reference: "asset", Detail: "maximum"}
		}, "messages[0].parts[0].detail"},
		{"non finite temperature", func(request *adapter.NormalizedRequest) { value := math.NaN(); request.Temperature = &value }, "temperature"},
		{"top p range", func(request *adapter.NormalizedRequest) { value := 1.1; request.TopP = &value }, "top_p"},
		{"max tokens", func(request *adapter.NormalizedRequest) { value := int64(0); request.MaxOutputTokens = &value }, "max_output_tokens"},
		{"duplicate stop", func(request *adapter.NormalizedRequest) { request.Stop = []string{"END", "END"} }, "stop"},
		{"invalid tool schema", func(request *adapter.NormalizedRequest) { request.Tools[0].InputSchema = json.RawMessage(`[]`) }, "tools[0].input_schema"},
		{"unknown named tool", func(request *adapter.NormalizedRequest) { request.ToolChoice.Name = "missing_tool" }, "tool_choice.name"},
		{"invalid response schema", func(request *adapter.NormalizedRequest) { request.ResponseFormat.Schema = json.RawMessage(`null`) }, "response_format.schema"},
		{"unsorted labels", func(request *adapter.NormalizedRequest) {
			request.PolicyLabels = []string{"region.us", "content.internal"}
		}, "policy_labels"},
		{"provider options array", func(request *adapter.NormalizedRequest) { request.ProviderOptions = json.RawMessage(`[]`) }, "provider_options"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := validRequest()
			test.mutate(&request)
			assertValidationField(t, request.Validate(), test.wantField)
		})
	}
}

func TestNormalizedResponseValidateAndClone(t *testing.T) {
	t.Parallel()

	response := validResponse(t)
	if err := response.Validate(); err != nil {
		t.Fatalf("validate response: %v", err)
	}

	cloned := response.Clone()
	cloned.Choices[0].Message.Parts[0].Text = "changed"
	cloned.Choices[0].Message.ToolCalls[0].Arguments[2] = 'X'
	cloned.Usage.UnmappedFields[0] = "/changed"
	if response.Choices[0].Message.Parts[0].Text != "response-secret-content" {
		t.Fatal("response content aliases clone")
	}
	if string(response.Choices[0].Message.ToolCalls[0].Arguments) != `{"city":"Shanghai"}` {
		t.Fatal("response tool arguments alias clone")
	}
	if response.Usage.UnmappedFields[0] != "/service_tier_units" {
		t.Fatal("usage metadata aliases clone")
	}
}

func TestNormalizedResponseValidationFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mutate    func(*adapter.NormalizedResponse)
		wantField string
	}{
		{"missing choices", func(response *adapter.NormalizedResponse) { response.Choices = nil }, "choices"},
		{"duplicate index", func(response *adapter.NormalizedResponse) {
			response.Choices = append(response.Choices, response.Choices[0])
		}, "choices[1].index"},
		{"wrong role", func(response *adapter.NormalizedResponse) { response.Choices[0].Message.Role = adapter.RoleUser }, "choices[0].message.role"},
		{"unknown reason evidence", func(response *adapter.NormalizedResponse) {
			response.Choices[0].FinishReason = adapter.FinishUnknown
			response.Choices[0].ProviderFinishReason = ""
		}, "choices[0].provider_finish_reason"},
		{"missing observed time", func(response *adapter.NormalizedResponse) { response.ObservedAt = time.Time{} }, "observed_at"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			response := validResponse(t)
			test.mutate(&response)
			assertValidationField(t, response.Validate(), test.wantField)
		})
	}
}

func TestNormalizedLogValuesDoNotExposeContentOrRawEvidence(t *testing.T) {
	t.Parallel()

	request := validRequest()
	response := validResponse(t)
	chunk := adapter.NormalizedChunk{
		Sequence:          4,
		Kind:              adapter.ChunkContentDelta,
		ChoiceIndex:       0,
		ContentDelta:      "chunk-secret-content",
		ProviderEventType: "content.delta",
		ObservedAt:        fixedTime(),
	}
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	logger.Info("normalized facts",
		slog.Any("request", request),
		slog.Any("response", response),
		slog.Any("chunk", chunk),
	)

	logged := output.String()
	for _, secret := range []string{
		"system-secret-prompt",
		"user-secret-prompt",
		"response-secret-content",
		"Shanghai",
		"chunk-secret-content",
		"unknown-billing-marker",
		"standard",
	} {
		if strings.Contains(logged, secret) {
			t.Fatalf("log contains sensitive normalized value %q: %s", secret, logged)
		}
	}
	for _, safeFact := range []string{"req_p05_t02", "logical-chat", "messageCount", "choiceCount", "rawEvidenceHash"} {
		if !strings.Contains(logged, safeFact) {
			t.Fatalf("log missing safe fact %q: %s", safeFact, logged)
		}
	}
}

func validRequest() adapter.NormalizedRequest {
	temperature := 0.2
	topP := 0.8
	maxOutputTokens := int64(256)
	strict := true
	return adapter.NormalizedRequest{
		RequestID:        "req_p05_t02",
		LogicalModel:     "logical-chat",
		EndUserReference: "application-user-1",
		Messages: []adapter.Message{
			{Role: adapter.RoleSystem, Parts: []adapter.ContentPart{{Kind: adapter.ContentText, Text: "system-secret-prompt"}}},
			{Role: adapter.RoleUser, Parts: []adapter.ContentPart{{Kind: adapter.ContentText, Text: "user-secret-prompt"}}},
		},
		Stream:          true,
		Temperature:     &temperature,
		TopP:            &topP,
		MaxOutputTokens: &maxOutputTokens,
		Stop:            []string{"END"},
		Tools: []adapter.ToolDefinition{{
			Name:        "get_weather",
			Description: "Get weather",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
		ToolChoice: &adapter.ToolChoice{Mode: adapter.ToolChoiceNamed, Name: "get_weather"},
		ResponseFormat: &adapter.ResponseFormat{
			Type:        adapter.ResponseFormatJSONSchema,
			Name:        "weather_result",
			Description: "Structured weather",
			Schema:      json.RawMessage(`{"type":"object"}`),
			Strict:      &strict,
		},
		PolicyLabels:    []string{"content.internal", "region.cn"},
		ProviderOptions: json.RawMessage(`{"tier":"standard"}`),
	}
}

func validResponse(t *testing.T) adapter.NormalizedResponse {
	t.Helper()
	evidence, err := adapter.NewUsageEvidence([]byte(`{"input_tokens":13,"output_tokens":3,"service_tier_units":2,"marker":"unknown-billing-marker"}`))
	if err != nil {
		t.Fatalf("create usage evidence: %v", err)
	}
	usage := adapter.NormalizedUsage{
		InputTokens:     adapter.Tokens(13),
		OutputTokens:    adapter.Tokens(3),
		CacheReadTokens: adapter.Tokens(5),
		Source:          adapter.UsageSourceProvider,
		Complete:        true,
		RawEvidence:     evidence,
		UnmappedFields:  []string{"/service_tier_units"},
	}
	return adapter.NormalizedResponse{
		ResponseID: "response_fixture_1",
		Model:      "physical-model-v1",
		Choices: []adapter.NormalizedChoice{{
			Index: 0,
			Message: adapter.Message{
				Role:  adapter.RoleAssistant,
				Parts: []adapter.ContentPart{{Kind: adapter.ContentText, Text: "response-secret-content"}},
				ToolCalls: []adapter.ToolCall{{
					ID:        "call_fixture_1",
					Name:      "get_weather",
					Arguments: json.RawMessage(`{"city":"Shanghai"}`),
				}},
			},
			FinishReason:         adapter.FinishToolCalls,
			ProviderFinishReason: "tool_calls",
		}},
		Usage:             &usage,
		ProviderRequestID: "provider_request_fixture_1",
		ObservedAt:        fixedTime(),
	}
}

func fixedTime() time.Time {
	return time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
}

func assertValidationField(t *testing.T, err error, wantField string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected validation error for %s", wantField)
	}
	var validationError *adapter.ValidationError
	if !errors.As(err, &validationError) {
		t.Fatalf("expected ValidationError, got %T: %v", err, err)
	}
	if validationError.Field != wantField {
		t.Fatalf("validation field = %q, want %q (error: %v)", validationError.Field, wantField, err)
	}
}
