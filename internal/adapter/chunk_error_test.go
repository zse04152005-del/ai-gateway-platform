package adapter_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
)

func TestNormalizedChunkKindsValidate(t *testing.T) {
	t.Parallel()

	providerUsage := providerUsage(t, true)
	partialUsage := adapter.NormalizedUsage{
		OutputTokens: adapter.Tokens(2),
		Source:       adapter.UsageSourceEstimated,
		Complete:     false,
	}
	tests := []struct {
		name  string
		chunk adapter.NormalizedChunk
	}{
		{"message start", baseChunk(adapter.ChunkMessageStart, func(chunk *adapter.NormalizedChunk) { chunk.Role = adapter.RoleAssistant })},
		{"content", baseChunk(adapter.ChunkContentDelta, func(chunk *adapter.NormalizedChunk) { chunk.ContentDelta = "visible" })},
		{"reasoning", baseChunk(adapter.ChunkReasoningDelta, func(chunk *adapter.NormalizedChunk) { chunk.ReasoningDelta = "classified" })},
		{"tool", baseChunk(adapter.ChunkToolDelta, func(chunk *adapter.NormalizedChunk) {
			chunk.ToolDelta = &adapter.ToolCallDelta{Index: 0, ID: "call_1", Name: "lookup", ArgumentsFragment: `{"q":"`}
		})},
		{"usage", baseChunk(adapter.ChunkUsageDelta, func(chunk *adapter.NormalizedChunk) {
			chunk.Usage = &partialUsage
			chunk.UsageStatus = adapter.UsageStatusPartial
		})},
		{"end present", baseChunk(adapter.ChunkMessageEnd, func(chunk *adapter.NormalizedChunk) {
			chunk.FinishReason = adapter.FinishStop
			chunk.ProviderFinishReason = "stop"
			chunk.Usage = &providerUsage
			chunk.UsageStatus = adapter.UsageStatusPresent
		})},
		{"end missing", baseChunk(adapter.ChunkMessageEnd, func(chunk *adapter.NormalizedChunk) {
			chunk.FinishReason = adapter.FinishError
			chunk.ProviderFinishReason = "stream_closed"
			chunk.UsageStatus = adapter.UsageStatusMissing
		})},
		{"heartbeat", baseChunk(adapter.ChunkHeartbeat, func(chunk *adapter.NormalizedChunk) { chunk.ProviderEventType = "ping" })},
		{"extension", baseChunk(adapter.ChunkProviderExtension, func(chunk *adapter.NormalizedChunk) {
			chunk.ProviderEventType = "future.event"
			chunk.ProviderExtension = []byte(`{"future":"bounded"}`)
		})},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := test.chunk.Validate(); err != nil {
				t.Fatalf("validate chunk: %v", err)
			}
		})
	}
}

func TestNormalizedChunkValidationFailures(t *testing.T) {
	t.Parallel()

	complete := providerUsage(t, true)
	partial := providerUsage(t, false)
	tests := []struct {
		name      string
		chunk     adapter.NormalizedChunk
		wantField string
	}{
		{"negative choice", baseChunk(adapter.ChunkHeartbeat, func(chunk *adapter.NormalizedChunk) { chunk.ChoiceIndex = -1 }), "choice_index"},
		{"missing timestamp", adapter.NormalizedChunk{Kind: adapter.ChunkHeartbeat}, "observed_at"},
		{"mixed content", baseChunk(adapter.ChunkContentDelta, func(chunk *adapter.NormalizedChunk) { chunk.ContentDelta = "a"; chunk.ReasoningDelta = "b" }), "chunk"},
		{"empty tool", baseChunk(adapter.ChunkToolDelta, func(chunk *adapter.NormalizedChunk) { chunk.ToolDelta = &adapter.ToolCallDelta{} }), "tool_delta"},
		{"complete delta", baseChunk(adapter.ChunkUsageDelta, func(chunk *adapter.NormalizedChunk) {
			chunk.Usage = &complete
			chunk.UsageStatus = adapter.UsageStatusPartial
		}), "usage.complete"},
		{"end missing with usage", baseChunk(adapter.ChunkMessageEnd, func(chunk *adapter.NormalizedChunk) {
			chunk.FinishReason = adapter.FinishStop
			chunk.UsageStatus = adapter.UsageStatusMissing
			chunk.Usage = &partial
		}), "usage"},
		{"extension array", baseChunk(adapter.ChunkProviderExtension, func(chunk *adapter.NormalizedChunk) {
			chunk.ProviderEventType = "future"
			chunk.ProviderExtension = []byte(`[]`)
		}), "provider_extension"},
		{"unknown kind", baseChunk("vendor_delta", func(*adapter.NormalizedChunk) {}), "kind"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assertValidationField(t, test.chunk.Validate(), test.wantField)
		})
	}
}

func TestNormalizedChunkCloneDoesNotAliasMutablePayloads(t *testing.T) {
	t.Parallel()

	usage := providerUsage(t, false)
	chunk := baseChunk(adapter.ChunkProviderExtension, func(chunk *adapter.NormalizedChunk) {
		chunk.ProviderEventType = "future.event"
		chunk.ProviderExtension = []byte(`{"value":"original"}`)
	})
	chunk.ToolDelta = &adapter.ToolCallDelta{Index: 0, ArgumentsFragment: "original"}
	chunk.Usage = &usage

	cloned := chunk.Clone()
	cloned.ProviderExtension[2] = 'X'
	cloned.ToolDelta.ArgumentsFragment = "changed"
	cloned.Usage.UnmappedFields[0] = "/changed"
	if string(chunk.ProviderExtension) != `{"value":"original"}` {
		t.Fatal("provider extension aliases clone")
	}
	if chunk.ToolDelta.ArgumentsFragment != "original" {
		t.Fatal("tool delta aliases clone")
	}
	if chunk.Usage.UnmappedFields[0] != "/future_meter" {
		t.Fatal("usage aliases clone")
	}
}

func TestNormalizedErrorValidateCloneAndSafeString(t *testing.T) {
	t.Parallel()

	retryAfter := time.Second
	normalizedError := adapter.NormalizedError{
		Code:              "PROVIDER_RATE_LIMITED",
		Category:          adapter.ErrorRateLimit,
		Retryable:         true,
		RetryAfter:        &retryAfter,
		ProviderStatus:    429,
		SafeMessage:       "Provider temporarily rate limited the request",
		ProviderRequestID: "provider_request_1",
	}
	if err := normalizedError.Validate(); err != nil {
		t.Fatalf("validate error: %v", err)
	}
	if normalizedError.Error() != "PROVIDER_RATE_LIMITED: Provider temporarily rate limited the request" {
		t.Fatalf("unexpected safe error string: %s", normalizedError.Error())
	}
	cloned := normalizedError.Clone()
	*cloned.RetryAfter = 2 * time.Second
	if *normalizedError.RetryAfter != time.Second {
		t.Fatal("retry duration aliases clone")
	}
}

func TestNormalizedErrorValidationFailures(t *testing.T) {
	t.Parallel()

	valid := func() adapter.NormalizedError {
		return adapter.NormalizedError{
			Code:           "PROVIDER_FAILED",
			Category:       adapter.ErrorProvider5xx,
			Retryable:      true,
			ProviderStatus: 503,
			SafeMessage:    "Provider request failed safely",
		}
	}
	retryAfter := time.Second
	tests := []struct {
		name      string
		mutate    func(*adapter.NormalizedError)
		wantField string
	}{
		{"code", func(value *adapter.NormalizedError) { value.Code = "provider_failed" }, "error.code"},
		{"category", func(value *adapter.NormalizedError) { value.Category = "vendor" }, "error.category"},
		{"unknown retry", func(value *adapter.NormalizedError) { value.Category = adapter.ErrorUnknown }, "error.retryable"},
		{"retry metadata", func(value *adapter.NormalizedError) { value.Retryable = false; value.RetryAfter = &retryAfter }, "error.retry_after"},
		{"status", func(value *adapter.NormalizedError) { value.ProviderStatus = 99 }, "error.provider_status"},
		{"safe message", func(value *adapter.NormalizedError) { value.SafeMessage = "unsafe\nheader" }, "error.safe_message"},
		{"request id", func(value *adapter.NormalizedError) { value.ProviderRequestID = "bad id" }, "error.provider_request_id"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			value := valid()
			test.mutate(&value)
			assertValidationField(t, value.Validate(), test.wantField)
		})
	}
}

func TestNormalizedErrorHasNoRawBodyOrCauseField(t *testing.T) {
	t.Parallel()

	typeOfError := reflect.TypeOf(adapter.NormalizedError{})
	for _, forbidden := range []string{"Body", "Raw", "Cause", "Credential", "Prompt", "Response"} {
		for index := 0; index < typeOfError.NumField(); index++ {
			if strings.Contains(typeOfError.Field(index).Name, forbidden) {
				t.Fatalf("NormalizedError field %q contains forbidden concept %q", typeOfError.Field(index).Name, forbidden)
			}
		}
	}
}

func baseChunk(kind adapter.ChunkKind, mutate func(*adapter.NormalizedChunk)) adapter.NormalizedChunk {
	chunk := adapter.NormalizedChunk{
		Sequence:    1,
		Kind:        kind,
		ChoiceIndex: 0,
		ObservedAt:  fixedTime(),
	}
	mutate(&chunk)
	return chunk
}

func providerUsage(t *testing.T, complete bool) adapter.NormalizedUsage {
	t.Helper()
	evidence, err := adapter.NewUsageEvidence([]byte(`{"input_tokens":4,"output_tokens":2,"future_meter":1}`))
	if err != nil {
		t.Fatalf("new usage evidence: %v", err)
	}
	return adapter.NormalizedUsage{
		InputTokens:    adapter.Tokens(4),
		OutputTokens:   adapter.Tokens(2),
		Source:         adapter.UsageSourceProvider,
		Complete:       complete,
		RawEvidence:    evidence,
		UnmappedFields: []string{"/future_meter"},
	}
}
