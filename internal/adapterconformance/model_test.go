package adapterconformance_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/adapterconformance"
	"github.com/zse04152005-del/ai-gateway-platform/internal/provideradapter"
)

func TestRegistrationValidationMatrix(t *testing.T) {
	t.Parallel()

	valid := validRegistration(t)
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid registration: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*adapterconformance.Registration)
	}{
		{name: "invalid name", mutate: func(value *adapterconformance.Registration) { value.Name = "Invalid Name" }},
		{name: "missing builder", mutate: func(value *adapterconformance.Registration) { value.NewAdapter = nil }},
		{name: "missing fixture", mutate: func(value *adapterconformance.Registration) {
			value.Fixtures.CachedUsage.NewHandler = nil
		}},
		{name: "duplicate fixture name", mutate: func(value *adapterconformance.Registration) {
			value.Fixtures.CachedUsage.Name = value.Fixtures.Ordinary.Name
		}},
		{name: "invalid request", mutate: func(value *adapterconformance.Registration) {
			value.Fixtures.Ordinary.Request.Messages = nil
		}},
		{name: "invalid expected response", mutate: func(value *adapterconformance.Registration) {
			value.Fixtures.Ordinary.Want.ObservedAt = time.Time{}
		}},
		{name: "missing finish reason", mutate: func(value *adapterconformance.Registration) {
			value.Fixtures.FinishReasons = value.Fixtures.FinishReasons[:2]
		}},
		{name: "multiple finish choices", mutate: func(value *adapterconformance.Registration) {
			value.Fixtures.FinishReasons[0].Want.Choices = append(
				value.Fixtures.FinishReasons[0].Want.Choices,
				value.Fixtures.FinishReasons[0].Want.Choices[0],
			)
		}},
		{name: "empty stream", mutate: func(value *adapterconformance.Registration) {
			value.Fixtures.Stream.Want = nil
		}},
		{name: "non monotonic stream", mutate: func(value *adapterconformance.Registration) {
			value.Fixtures.Stream.Want[1].Sequence = 7
		}},
		{name: "invalid expected chunk", mutate: func(value *adapterconformance.Registration) {
			value.Fixtures.Stream.Want[1].ContentDelta = ""
		}},
		{name: "unknown stream not isolated", mutate: func(value *adapterconformance.Registration) {
			value.Fixtures.UnknownStream.Want = value.Fixtures.Stream.Want
		}},
		{name: "invalid normalized error", mutate: func(value *adapterconformance.Registration) {
			value.Fixtures.RateLimit.Want.Code = "lowercase"
		}},
		{name: "unsafe retry category", mutate: func(value *adapterconformance.Registration) {
			value.Fixtures.RateLimit.Want.Category = adapter.ErrorUnknown
		}},
		{name: "provider failure not retryable", mutate: func(value *adapterconformance.Registration) {
			value.Fixtures.ProviderFailure.Want.Retryable = false
		}},
		{name: "missing cancellation handler", mutate: func(value *adapterconformance.Registration) {
			value.Fixtures.Cancellation.NewHandler = nil
		}},
		{name: "non stream cancellation", mutate: func(value *adapterconformance.Registration) {
			value.Fixtures.Cancellation.Request.Stream = false
		}},
		{name: "missing protocol sentinel", mutate: func(value *adapterconformance.Registration) {
			value.Fixtures.UnknownOrdinary.Want = nil
		}},
		{name: "blank forbidden marker", mutate: func(value *adapterconformance.Registration) {
			value.Fixtures.UnknownOrdinary.ForbiddenText = []string{" "}
		}},
		{name: "ordinary usage omitted", mutate: func(value *adapterconformance.Registration) {
			value.Fixtures.Ordinary.Want.Usage = nil
		}},
		{name: "cache usage omitted", mutate: func(value *adapterconformance.Registration) {
			value.Fixtures.CachedUsage.Want.Usage.CacheReadTokens = adapter.TokenCount{}
		}},
		{name: "tool call omitted", mutate: func(value *adapterconformance.Registration) {
			value.Fixtures.ToolCall.Want.Choices[0].Message.ToolCalls = nil
		}},
		{name: "stream content omitted", mutate: func(value *adapterconformance.Registration) {
			value.Fixtures.Stream.Want = []adapter.NormalizedChunk{
				value.Fixtures.Stream.Want[0], value.Fixtures.Stream.Want[2],
			}
			value.Fixtures.Stream.Want[1].Sequence = 1
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			registration := validRegistration(t)
			test.mutate(&registration)
			if err := registration.Validate(); !errors.Is(err, adapterconformance.ErrInvalidRegistration) {
				t.Fatalf("validation error = %v", err)
			}
		})
	}
}

func validRegistration(t *testing.T) adapterconformance.Registration {
	t.Helper()
	now := time.Date(2026, time.July, 30, 0, 0, 0, 0, time.UTC)
	usage := testUsage(t, 2, 1, 0)
	cached := testUsage(t, 3, 1, 1)
	request := func(stream bool) adapter.NormalizedRequest {
		return adapter.NormalizedRequest{
			RequestID: "req_conformance", LogicalModel: "logical-test", Stream: stream,
			Messages: []adapter.Message{{
				Role:  adapter.RoleUser,
				Parts: []adapter.ContentPart{{Kind: adapter.ContentText, Text: "synthetic fixture"}},
			}},
		}
	}
	response := func(id string, reason adapter.FinishReason, providerReason string, selected adapter.NormalizedUsage) adapter.NormalizedResponse {
		return adapter.NormalizedResponse{
			ResponseID: id, Model: "test-model", ObservedAt: now, Usage: &selected,
			Choices: []adapter.NormalizedChoice{{
				Index: 0,
				Message: adapter.Message{
					Role:  adapter.RoleAssistant,
					Parts: []adapter.ContentPart{{Kind: adapter.ContentText, Text: "fixture response"}},
				},
				FinishReason: reason, ProviderFinishReason: providerReason,
			}},
		}
	}
	handler := http.NotFoundHandler
	stream := []adapter.NormalizedChunk{
		{Sequence: 0, Kind: adapter.ChunkMessageStart, Role: adapter.RoleAssistant, ObservedAt: now},
		{Sequence: 1, Kind: adapter.ChunkContentDelta, ContentDelta: "fixture", ObservedAt: now},
		{
			Sequence: 2, Kind: adapter.ChunkMessageEnd, FinishReason: adapter.FinishStop,
			ProviderFinishReason: "stop", Usage: &usage, UsageStatus: adapter.UsageStatusPresent, ObservedAt: now,
		},
	}
	unknownStream := []adapter.NormalizedChunk{
		{Sequence: 0, Kind: adapter.ChunkMessageStart, Role: adapter.RoleAssistant, ObservedAt: now},
		{
			Sequence: 1, Kind: adapter.ChunkProviderExtension, ProviderEventType: "future.event",
			ProviderExtension: json.RawMessage(`{"future":true}`), ObservedAt: now,
		},
		{
			Sequence: 2, Kind: adapter.ChunkMessageEnd, FinishReason: adapter.FinishStop,
			ProviderFinishReason: "stop", Usage: &usage, UsageStatus: adapter.UsageStatusPresent, ObservedAt: now,
		},
	}
	toolResponse := response("response-tool", adapter.FinishToolCalls, "tool_calls", usage)
	toolResponse.Choices[0].Message.Parts = nil
	toolResponse.Choices[0].Message.ToolCalls = []adapter.ToolCall{{
		ID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{}`),
	}}
	return adapterconformance.Registration{
		Name: "test", NewAdapter: func(context.Context, string) (provideradapter.Adapter, error) { return nil, nil },
		Fixtures: adapterconformance.FixtureSet{
			Ordinary: adapterconformance.ResponseFixture{
				Name: "ordinary", Request: request(false), NewHandler: handler,
				Want: response("response-ordinary", adapter.FinishStop, "stop", usage),
			},
			Stream: adapterconformance.StreamFixture{Name: "stream", Request: request(true), NewHandler: handler, Want: stream},
			Cancellation: adapterconformance.CancellationFixture{
				Name: "cancellation", Request: request(true),
				NewHandler: func(chan<- struct{}) http.Handler { return http.NotFoundHandler() },
			},
			RateLimit: adapterconformance.ErrorFixture{
				Name: "rate_limit", Request: request(false), NewHandler: handler,
				Want: adapter.NormalizedError{
					Code: "RATE_LIMITED", Category: adapter.ErrorRateLimit, Retryable: true,
					ProviderStatus: http.StatusTooManyRequests, SafeMessage: "Provider rate limited the request",
				},
			},
			ProviderFailure: adapterconformance.ErrorFixture{
				Name: "provider_failure", Request: request(false), NewHandler: handler,
				Want: adapter.NormalizedError{
					Code: "PROVIDER_FAILED", Category: adapter.ErrorProvider5xx, Retryable: true,
					ProviderStatus: http.StatusInternalServerError, SafeMessage: "Provider request failed",
				},
			},
			CachedUsage: adapterconformance.ResponseFixture{
				Name: "cached_usage", Request: request(false), NewHandler: handler,
				Want: response("response-cached", adapter.FinishStop, "stop", cached),
			},
			ToolCall: adapterconformance.ResponseFixture{
				Name: "tool_call", Request: request(false), NewHandler: handler, Want: toolResponse,
			},
			FinishReasons: []adapterconformance.ResponseFixture{
				{Name: "finish_length", Request: request(false), NewHandler: handler, Want: response("response-length", adapter.FinishLength, "length", usage)},
				{Name: "finish_policy", Request: request(false), NewHandler: handler, Want: response("response-policy", adapter.FinishContentPolicy, "content_filter", usage)},
				{Name: "finish_unknown", Request: request(false), NewHandler: handler, Want: response("response-unknown", adapter.FinishUnknown, "future", usage)},
			},
			UnknownOrdinary: adapterconformance.ProtocolErrorFixture{
				Name: "unknown_ordinary", Request: request(false), NewHandler: handler, Want: errors.New("protocol fixture"),
			},
			UnknownStream: adapterconformance.StreamFixture{
				Name: "unknown_stream", Request: request(true), NewHandler: handler, Want: unknownStream,
			},
		},
	}
}

func testUsage(t *testing.T, input, output, cache int64) adapter.NormalizedUsage {
	t.Helper()
	evidence, err := adapter.NewUsageEvidence([]byte(`{"input":2,"output":1}`))
	if err != nil {
		t.Fatalf("create test usage evidence: %v", err)
	}
	usage := adapter.NormalizedUsage{
		InputTokens: adapter.Tokens(input), OutputTokens: adapter.Tokens(output),
		Source: adapter.UsageSourceProvider, Complete: true, RawEvidence: evidence,
	}
	if cache > 0 {
		usage.CacheReadTokens = adapter.Tokens(cache)
	}
	return usage
}
