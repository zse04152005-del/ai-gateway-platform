package openaiadapter_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/openaiadapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/provideradapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/providersecret"
)

const fixtureCredential = "fixture-token"

type staticResolver struct {
	value []byte
	err   error
}

func (resolver staticResolver) Resolve(context.Context, providersecret.Locator) ([]byte, error) {
	return append([]byte(nil), resolver.value...), resolver.err
}

func TestBuildRequestMapsOfficialFieldsAndResolvesCredentialLate(t *testing.T) {
	t.Parallel()
	built := newOpenAIAdapter(t, "https://api.openai.com/v1", staticResolver{value: []byte(fixtureCredential)})
	temperature := 0.2
	topP := 0.9
	maxOutput := int64(64)
	strict := true
	request := normalizedRequest(true)
	request.EndUserReference = "application-user-1"
	request.Messages[0].Parts = append(request.Messages[0].Parts, adapter.ContentPart{
		Kind: adapter.ContentImageReference, Reference: "https://example.invalid/image.png", Detail: "low",
	})
	request.Temperature = &temperature
	request.TopP = &topP
	request.MaxOutputTokens = &maxOutput
	request.Stop = []string{"END"}
	request.Messages = append(request.Messages, adapter.Message{
		Role:      adapter.RoleAssistant,
		ToolCalls: []adapter.ToolCall{{ID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{"id":1}`)}},
	})
	request.Tools = []adapter.ToolDefinition{{Name: "lookup", Description: "Lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	request.ToolChoice = &adapter.ToolChoice{Mode: adapter.ToolChoiceNamed, Name: "lookup"}
	request.ResponseFormat = &adapter.ResponseFormat{
		Type: adapter.ResponseFormatJSONSchema, Name: "answer", Schema: json.RawMessage(`{"type":"object"}`), Strict: &strict,
	}

	httpRequest, err := built.BuildRequest(context.Background(), request)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if httpRequest.URL.String() != "https://api.openai.com/v1/chat/completions" || httpRequest.Method != http.MethodPost {
		t.Fatalf("request target = %s %s", httpRequest.Method, httpRequest.URL)
	}
	if httpRequest.Header.Get("Authorization") != "Bearer "+fixtureCredential ||
		httpRequest.Header.Get("X-Client-Request-Id") != request.RequestID {
		t.Fatalf("request headers were not mapped")
	}
	body, err := io.ReadAll(httpRequest.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	for _, name := range []string{"model", "user", "messages", "stream", "stream_options", "temperature", "top_p", "max_completion_tokens", "stop", "tools", "tool_choice", "response_format"} {
		if _, exists := payload[name]; !exists {
			t.Fatalf("request field %q is missing: %s", name, body)
		}
	}
	if _, exists := payload["max_tokens"]; exists {
		t.Fatalf("deprecated max_tokens was emitted: %s", body)
	}
	if !bytes.Contains(body, []byte(`"detail":"low"`)) || !bytes.Contains(body, []byte(`"user":"application-user-1"`)) {
		t.Fatalf("end-user or image detail was not preserved: %s", body)
	}
	if strings.Contains(string(body), fixtureCredential) {
		t.Fatal("credential leaked into request JSON")
	}
}

func TestFactoryAndCredentialFailuresAreSafe(t *testing.T) {
	t.Parallel()
	if _, err := openaiadapter.NewFactory(openaiadapter.FactoryOptions{}); err == nil {
		t.Fatal("nil resolver was accepted")
	}
	provider := openAIProvider()
	deployment := openAIDeployment(provider.ID, "http://api.openai.com/v1")
	factory, err := openaiadapter.NewFactory(openaiadapter.FactoryOptions{Secrets: staticResolver{value: []byte(fixtureCredential)}})
	if err != nil {
		t.Fatalf("new factory: %v", err)
	}
	if _, err := factory.New(context.Background(), provider, deployment); err == nil {
		t.Fatal("remote HTTP endpoint was accepted")
	}

	privateMarker := "credential-private-marker"
	built := newOpenAIAdapter(t, "https://api.openai.com/v1", staticResolver{err: errors.New(privateMarker)})
	_, err = built.BuildRequest(context.Background(), normalizedRequest(false))
	if !errors.Is(err, openaiadapter.ErrCredentialUnavailable) || strings.Contains(err.Error(), privateMarker) {
		t.Fatalf("credential error = %v", err)
	}
}

func TestRequestRejectsUnsupportedContentAndProviderPassthrough(t *testing.T) {
	t.Parallel()
	built := newOpenAIAdapter(t, "https://api.openai.com/v1", staticResolver{value: []byte(fixtureCredential)})
	tests := []struct {
		name   string
		mutate func(*adapter.NormalizedRequest)
	}{
		{"provider options", func(request *adapter.NormalizedRequest) { request.ProviderOptions = json.RawMessage(`{}`) }},
		{"policy labels", func(request *adapter.NormalizedRequest) { request.PolicyLabels = []string{"restricted"} }},
		{"audio", func(request *adapter.NormalizedRequest) {
			request.Messages[0].Parts = []adapter.ContentPart{{Kind: adapter.ContentAudioReference, Reference: "Zml4dHVyZQ==", MediaType: "audio/wav"}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := normalizedRequest(false)
			test.mutate(&request)
			_, err := built.BuildRequest(context.Background(), request)
			if !errors.Is(err, openaiadapter.ErrUnsupportedParameter) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestUsageMapsBillingDimensionsAndPreservesUnknownEvidence(t *testing.T) {
	t.Parallel()
	body := `{"id":"chatcmpl-usage","object":"chat.completion","model":"gpt-fixture-001","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":5,"total_tokens":17,"prompt_tokens_details":{"cached_tokens":4,"cache_write_tokens":3,"audio_tokens":2,"future_prompt":1},"completion_tokens_details":{"reasoning_tokens":2,"audio_tokens":1,"accepted_prediction_tokens":1}}}`
	response := &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)),
	}
	built := newOpenAIAdapter(t, "https://api.openai.com/v1", staticResolver{value: []byte(fixtureCredential)})
	normalized, err := built.ParseResponse(context.Background(), response)
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	usage := normalized.Usage
	if usage == nil || usage.CacheReadTokens.Value != 4 || usage.CacheWriteTokens.Value != 3 ||
		usage.ReasoningTokens.Value != 2 || usage.AudioInputTokens.Value != 2 || usage.AudioOutputTokens.Value != 1 {
		t.Fatalf("usage = %#v", usage)
	}
	wantUnknown := []string{
		"/completion_tokens_details/accepted_prediction_tokens",
		"/prompt_tokens_details/future_prompt",
	}
	if strings.Join(usage.UnmappedFields, ",") != strings.Join(wantUnknown, ",") || !usage.RawEvidence.Present() {
		t.Fatalf("unknown usage evidence = %#v", usage)
	}
}

func TestProtocolErrorsDoNotEchoProviderPayload(t *testing.T) {
	t.Parallel()
	privateMarker := "private-response-marker"
	response := &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"id":"x","object":"chat.completion","model":"gpt-fixture-001","choices":[],"future":"` + privateMarker + `"}`)),
	}
	built := newOpenAIAdapter(t, "https://api.openai.com/v1", staticResolver{value: []byte(fixtureCredential)})
	_, err := built.ParseResponse(context.Background(), response)
	var violation provideradapter.ProtocolViolation
	if !errors.Is(err, openaiadapter.ErrProtocol) || !errors.As(err, &violation) || strings.Contains(err.Error(), privateMarker) {
		t.Fatalf("protocol error = %v", err)
	}
}

func newOpenAIAdapter(t *testing.T, endpoint string, resolver openaiadapter.SecretResolver) provideradapter.Adapter {
	t.Helper()
	factory, err := openaiadapter.NewFactory(openaiadapter.FactoryOptions{
		Secrets: resolver, Now: fixedClock, AllowInsecureLoopback: strings.HasPrefix(endpoint, "http://"),
	})
	if err != nil {
		t.Fatalf("new openai factory: %v", err)
	}
	registry, err := provideradapter.NewRegistry(factory)
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	provider := openAIProvider()
	built, err := registry.Build(context.Background(), provider, openAIDeployment(provider.ID, endpoint))
	if err != nil {
		t.Fatalf("build adapter: %v", err)
	}
	return built
}

func openAIProvider() catalog.Provider {
	now := fixedClock()
	return catalog.Provider{
		ID: "33333333-3333-4333-8333-333333333333", Code: "openai-official", Name: "OpenAI Official",
		AdapterType: string(openaiadapter.Type), Status: catalog.StatusActive, Version: 1,
		CreatedAt: now, CreatedBy: "test", UpdatedAt: now, UpdatedBy: "test",
	}
}

func openAIDeployment(providerID, endpoint string) catalog.Deployment {
	now := fixedClock()
	secretID := "44444444-4444-4444-8444-444444444444"
	return catalog.Deployment{
		ID: "55555555-5555-4555-8555-555555555555", ProviderID: providerID,
		Code: "fixture", PhysicalModel: "gpt-fixture-001", EndpointURL: endpoint, Region: "global",
		Capabilities: catalog.CapabilitySet{
			Chat: true, Stream: true, Tools: true, StructuredOutput: true, Vision: true,
			UsageInStream: true, CacheUsage: true, ReasoningUsage: true,
			MaxContextTokens: 128000, MaxOutputTokens: 16384,
			DataRetentionMode: catalog.RetentionNoTraining, ProviderProtocolVersion: "chat-completions-v1",
		},
		SecretReferenceID: &secretID, Status: catalog.StatusActive, Version: 1,
		CreatedAt: now, CreatedBy: "test", UpdatedAt: now, UpdatedBy: "test",
	}
}

func normalizedRequest(stream bool) adapter.NormalizedRequest {
	return adapter.NormalizedRequest{
		RequestID: "req_openai_adapter", LogicalModel: "logical-chat", Stream: stream,
		Messages: []adapter.Message{{
			Role:  adapter.RoleUser,
			Parts: []adapter.ContentPart{{Kind: adapter.ContentText, Text: "fixture prompt"}},
		}},
	}
}

func fixedClock() time.Time {
	return time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
}

func writeJSON(writer http.ResponseWriter, status int, body string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("X-Request-ID", "req_openai_provider")
	writer.WriteHeader(status)
	_, _ = io.WriteString(writer, body)
}
