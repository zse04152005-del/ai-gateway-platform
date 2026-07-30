package adapter_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
)

func TestMessageAndRequestSupportedVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		message adapter.Message
	}{
		{
			"media user",
			adapter.Message{Role: adapter.RoleUser, Parts: []adapter.ContentPart{{
				Kind: adapter.ContentImageReference, Reference: "asset_123", MediaType: "image/png",
			}}},
		},
		{
			"tool result",
			adapter.Message{Role: adapter.RoleTool, ToolCallID: "call_1", Parts: []adapter.ContentPart{{
				Kind: adapter.ContentText, Text: "result",
			}}},
		},
		{
			"assistant tool only",
			adapter.Message{Role: adapter.RoleAssistant, ToolCalls: []adapter.ToolCall{{
				ID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{"q":"test"}`),
			}}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := adapter.NormalizedRequest{
				RequestID:    "req_variant",
				LogicalModel: "logical-chat",
				Messages:     []adapter.Message{test.message},
			}
			if err := request.Validate(); err != nil {
				t.Fatalf("validate variant: %v", err)
			}
			cloned := request.Clone()
			if len(cloned.Messages) != 1 {
				t.Fatal("clone lost message")
			}
		})
	}
}

func TestToolChoiceAndResponseFormatSupportedVariants(t *testing.T) {
	t.Parallel()

	for _, choice := range []*adapter.ToolChoice{
		nil,
		{Mode: adapter.ToolChoiceAuto},
		{Mode: adapter.ToolChoiceNone},
		{Mode: adapter.ToolChoiceRequired},
		{Mode: adapter.ToolChoiceNamed, Name: "get_weather"},
	} {
		request := validRequest()
		request.ToolChoice = choice
		if err := request.Validate(); err != nil {
			t.Fatalf("validate tool choice %#v: %v", choice, err)
		}
	}

	for _, format := range []*adapter.ResponseFormat{
		nil,
		{Type: adapter.ResponseFormatText},
		{Type: adapter.ResponseFormatJSONObject},
	} {
		request := validRequest()
		request.ResponseFormat = format
		if err := request.Validate(); err != nil {
			t.Fatalf("validate response format %#v: %v", format, err)
		}
	}
}

func TestMessageAndRequestAdditionalFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mutate    func(*adapter.NormalizedRequest)
		wantField string
	}{
		{"message name", func(request *adapter.NormalizedRequest) { request.Messages[0].Name = "bad name" }, "messages[0].name"},
		{"tool result id", func(request *adapter.NormalizedRequest) {
			request.Messages = []adapter.Message{{Role: adapter.RoleTool, Parts: []adapter.ContentPart{{Kind: adapter.ContentText, Text: "result"}}}}
		}, "messages[0].tool_call_id"},
		{"tool result nested call", func(request *adapter.NormalizedRequest) {
			request.Messages = []adapter.Message{{Role: adapter.RoleTool, ToolCallID: "call_1", Parts: []adapter.ContentPart{{Kind: adapter.ContentText, Text: "result"}}, ToolCalls: []adapter.ToolCall{{ID: "call_2", Name: "lookup", Arguments: json.RawMessage(`{}`)}}}}
		}, "messages[0].tool_calls"},
		{"tool id on user", func(request *adapter.NormalizedRequest) { request.Messages[0].ToolCallID = "call_1" }, "messages[0].tool_call_id"},
		{"tool calls on user", func(request *adapter.NormalizedRequest) {
			request.Messages[0].ToolCalls = []adapter.ToolCall{{ID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{}`)}}
		}, "messages[0].tool_calls"},
		{"empty assistant", func(request *adapter.NormalizedRequest) {
			request.Messages = []adapter.Message{{Role: adapter.RoleAssistant}}
		}, "messages[0]"},
		{"empty text", func(request *adapter.NormalizedRequest) { request.Messages[0].Parts[0].Text = "" }, "messages[0].parts[0].text"},
		{"media text", func(request *adapter.NormalizedRequest) {
			request.Messages[0].Parts[0] = adapter.ContentPart{Kind: adapter.ContentImageReference, Text: "mixed", Reference: "asset"}
		}, "messages[0].parts[0].text"},
		{"media reference", func(request *adapter.NormalizedRequest) {
			request.Messages[0].Parts[0] = adapter.ContentPart{Kind: adapter.ContentAudioReference}
		}, "messages[0].parts[0].reference"},
		{"content kind", func(request *adapter.NormalizedRequest) { request.Messages[0].Parts[0].Kind = "vendor_part" }, "messages[0].parts[0].kind"},
		{"tool id", func(request *adapter.NormalizedRequest) {
			request.Messages = []adapter.Message{{Role: adapter.RoleAssistant, ToolCalls: []adapter.ToolCall{{Name: "lookup", Arguments: json.RawMessage(`{}`)}}}}
		}, "messages[0].tool_calls[0].id"},
		{"tool name", func(request *adapter.NormalizedRequest) { request.Tools[0].Name = "bad name" }, "tools[0].name"},
		{"duplicate tools", func(request *adapter.NormalizedRequest) { request.Tools = append(request.Tools, request.Tools[0]) }, "tools[1].name"},
		{"tool description", func(request *adapter.NormalizedRequest) { request.Tools[0].Description = " padded " }, "tools[0].description"},
		{"required name", func(request *adapter.NormalizedRequest) {
			request.ToolChoice = &adapter.ToolChoice{Mode: adapter.ToolChoiceRequired, Name: "lookup"}
		}, "tool_choice.name"},
		{"required no tools", func(request *adapter.NormalizedRequest) {
			request.Tools = nil
			request.ToolChoice = &adapter.ToolChoice{Mode: adapter.ToolChoiceRequired}
		}, "tool_choice"},
		{"unknown choice", func(request *adapter.NormalizedRequest) { request.ToolChoice = &adapter.ToolChoice{Mode: "vendor"} }, "tool_choice.mode"},
		{"text schema", func(request *adapter.NormalizedRequest) {
			request.ResponseFormat = &adapter.ResponseFormat{Type: adapter.ResponseFormatText, Schema: json.RawMessage(`{}`)}
		}, "response_format"},
		{"schema name", func(request *adapter.NormalizedRequest) { request.ResponseFormat.Name = "bad name" }, "response_format.name"},
		{"unknown format", func(request *adapter.NormalizedRequest) { request.ResponseFormat.Type = "vendor_json" }, "response_format.type"},
		{"invalid json trailing", func(request *adapter.NormalizedRequest) { request.ProviderOptions = json.RawMessage(`{"a":1} {"b":2}`) }, "provider_options"},
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

func TestResponseUsageAndChunkAdditionalBranches(t *testing.T) {
	t.Parallel()

	response := validResponse(t)
	response.Usage = nil
	if err := response.Validate(); err != nil {
		t.Fatalf("response without usage must remain representable: %v", err)
	}
	if response.Clone().Usage != nil {
		t.Fatal("clone synthesized missing usage")
	}

	partial := providerUsage(t, false)
	partialEnd := baseChunk(adapter.ChunkMessageEnd, func(chunk *adapter.NormalizedChunk) {
		chunk.FinishReason = adapter.FinishError
		chunk.ProviderFinishReason = "connection_closed"
		chunk.Usage = &partial
		chunk.UsageStatus = adapter.UsageStatusPartial
	})
	if err := partialEnd.Validate(); err != nil {
		t.Fatalf("partial terminal usage: %v", err)
	}

	unknownFinish := validResponse(t)
	unknownFinish.Choices[0].FinishReason = adapter.FinishUnknown
	unknownFinish.Choices[0].ProviderFinishReason = "new_provider_reason"
	if err := unknownFinish.Validate(); err != nil {
		t.Fatalf("unknown finish reason with evidence: %v", err)
	}
}

func TestChunkAdditionalFailures(t *testing.T) {
	t.Parallel()

	partial := providerUsage(t, false)
	tests := []struct {
		name      string
		chunk     adapter.NormalizedChunk
		wantField string
	}{
		{"start role", baseChunk(adapter.ChunkMessageStart, func(chunk *adapter.NormalizedChunk) { chunk.Role = "vendor" }), "role"},
		{"content empty", baseChunk(adapter.ChunkContentDelta, func(*adapter.NormalizedChunk) {}), "content_delta"},
		{"reasoning empty", baseChunk(adapter.ChunkReasoningDelta, func(*adapter.NormalizedChunk) {}), "reasoning_delta"},
		{"usage missing", baseChunk(adapter.ChunkUsageDelta, func(chunk *adapter.NormalizedChunk) { chunk.UsageStatus = adapter.UsageStatusPartial }), "usage"},
		{"usage status", baseChunk(adapter.ChunkUsageDelta, func(chunk *adapter.NormalizedChunk) { chunk.Usage = &partial }), "usage_status"},
		{"end status", baseChunk(adapter.ChunkMessageEnd, func(chunk *adapter.NormalizedChunk) { chunk.FinishReason = adapter.FinishStop }), "usage_status"},
		{"end present partial", baseChunk(adapter.ChunkMessageEnd, func(chunk *adapter.NormalizedChunk) {
			chunk.FinishReason = adapter.FinishStop
			chunk.Usage = &partial
			chunk.UsageStatus = adapter.UsageStatusPresent
		}), "usage"},
		{"extension type", baseChunk(adapter.ChunkProviderExtension, func(chunk *adapter.NormalizedChunk) { chunk.ProviderExtension = json.RawMessage(`{}`) }), "provider_event_type"},
		{"extension payload", baseChunk(adapter.ChunkProviderExtension, func(chunk *adapter.NormalizedChunk) { chunk.ProviderEventType = "future" }), "provider_extension"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assertValidationField(t, test.chunk.Validate(), test.wantField)
		})
	}
}

func TestErrorAndValidationStringBranchesAndLogging(t *testing.T) {
	t.Parallel()

	if (*adapter.ValidationError)(nil).Error() != "<nil>" {
		t.Fatal("nil ValidationError string changed")
	}
	if (&adapter.ValidationError{Field: "field", Reason: "reason"}).Error() != "field: reason" {
		t.Fatal("ValidationError string changed")
	}
	if (adapter.NormalizedError{SafeMessage: "safe"}).Error() != "safe" {
		t.Fatal("message-only normalized error changed")
	}
	if (adapter.NormalizedError{Code: "SAFE_CODE"}).Error() != "SAFE_CODE" {
		t.Fatal("code-only normalized error changed")
	}
	if (adapter.UsageEvidence{}).Hash() != "" {
		t.Fatal("zero evidence hash must be empty")
	}

	retry := 2 * time.Second
	normalizedError := adapter.NormalizedError{
		Code: "SAFE_CODE", Category: adapter.ErrorCapacity, Retryable: true,
		RetryAfter: &retry, ProviderStatus: 503, SafeMessage: "Safe message",
	}
	var output bytes.Buffer
	slog.New(slog.NewJSONHandler(&output, nil)).Info("error", slog.Any("error", normalizedError))
	for _, want := range []string{"SAFE_CODE", "capacity", "retryAfter", "Safe message"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("safe error log missing %q: %s", want, output.String())
		}
	}
}
