package mockadapter_test

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
	"github.com/zse04152005-del/ai-gateway-platform/internal/mockadapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/mockprovider"
	"github.com/zse04152005-del/ai-gateway-platform/internal/provideradapter"
)

func TestFactoryRegistersAndBuildsLocalAdapter(t *testing.T) {
	t.Parallel()

	built, _, _ := newAdapterRuntime(t, mockprovider.NewHandler())
	if built.Type() != mockadapter.Type {
		t.Fatalf("adapter type = %q", built.Type())
	}
	capabilities := built.Capabilities(context.Background())
	if !capabilities.Chat || !capabilities.Stream || !capabilities.CacheUsage {
		t.Fatalf("unexpected capabilities: %#v", capabilities)
	}
	if _, err := built.EstimateUsage(context.Background(), normalizedRequest(false, "")); !errors.Is(err, mockadapter.ErrUsageEstimationUnavailable) {
		t.Fatalf("estimate error = %v", err)
	}
}

func TestFactoryRejectsUnsafeOrInconsistentDeployment(t *testing.T) {
	t.Parallel()

	factory, err := mockadapter.NewFactory(mockadapter.FactoryOptions{Now: fixedClock})
	if err != nil {
		t.Fatalf("new factory: %v", err)
	}
	provider := mockProvider()
	base := mockDeployment(provider.ID, "http://127.0.0.1:18082")
	secretID := "33333333-3333-4333-8333-333333333333"
	tests := []struct {
		name   string
		change func(*catalog.Provider, *catalog.Deployment)
	}{
		{"remote", func(_ *catalog.Provider, deployment *catalog.Deployment) {
			deployment.EndpointURL = "https://provider.example/v1"
		}},
		{"https loopback", func(_ *catalog.Provider, deployment *catalog.Deployment) {
			deployment.EndpointURL = "https://127.0.0.1:18082"
		}},
		{"hostname loopback", func(_ *catalog.Provider, deployment *catalog.Deployment) {
			deployment.EndpointURL = "http://localhost:18082"
		}},
		{"path", func(_ *catalog.Provider, deployment *catalog.Deployment) {
			deployment.EndpointURL = "http://127.0.0.1:18082/other"
		}},
		{"secret", func(_ *catalog.Provider, deployment *catalog.Deployment) { deployment.SecretReferenceID = &secretID }},
		{"disabled", func(_ *catalog.Provider, deployment *catalog.Deployment) { deployment.Status = catalog.StatusDisabled }},
		{"no chat", func(_ *catalog.Provider, deployment *catalog.Deployment) { deployment.Capabilities.Chat = false }},
		{"wrong type", func(provider *catalog.Provider, _ *catalog.Deployment) { provider.AdapterType = "other" }},
		{"wrong provider", func(_ *catalog.Provider, deployment *catalog.Deployment) {
			deployment.ProviderID = "44444444-4444-4444-8444-444444444444"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			candidateProvider := provider
			candidateDeployment := base
			test.change(&candidateProvider, &candidateDeployment)
			if _, err := factory.New(context.Background(), candidateProvider, candidateDeployment); err == nil {
				t.Fatal("expected factory rejection")
			}
		})
	}

	if _, err := mockadapter.NewFactory(mockadapter.FactoryOptions{Now: func() time.Time { return time.Time{} }}); err == nil {
		t.Fatal("zero clock must fail")
	}
}

func TestFactoryRejectsContextAndCatalogValidationFailures(t *testing.T) {
	t.Parallel()

	factory, err := mockadapter.NewFactory(mockadapter.FactoryOptions{})
	if err != nil {
		t.Fatalf("new factory: %v", err)
	}
	provider := mockProvider()
	deployment := mockDeployment(provider.ID, "http://127.0.0.1:18082")
	var nilContext context.Context
	if _, err := factory.New(nilContext, provider, deployment); err == nil {
		t.Fatal("nil context must fail")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := factory.New(cancelled, provider, deployment); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled factory error = %v", err)
	}
	invalidProvider := provider
	invalidProvider.Name = " padded "
	if _, err := factory.New(context.Background(), invalidProvider, deployment); err == nil {
		t.Fatal("invalid provider must fail")
	}
	invalidDeployment := deployment
	invalidDeployment.EndpointURL = "not-a-url"
	if _, err := factory.New(context.Background(), provider, invalidDeployment); err == nil {
		t.Fatal("invalid deployment must fail")
	}
	disabledProvider := provider
	disabledProvider.Status = catalog.StatusDisabled
	if _, err := factory.New(context.Background(), disabledProvider, deployment); err == nil {
		t.Fatal("disabled provider must fail")
	}
	var nilFactory *mockadapter.Factory
	if _, err := nilFactory.New(context.Background(), provider, deployment); err == nil {
		t.Fatal("nil factory must fail")
	}
}

func TestBuildRequestPreservesNormalizedSemanticsAndNeverAddsCredential(t *testing.T) {
	t.Parallel()

	built, _, _ := newAdapterRuntime(t, mockprovider.NewHandler())
	temperature := 0.25
	topP := 0.9
	maxTokens := int64(128)
	strict := true
	request := adapter.NormalizedRequest{
		RequestID: "req_full_fixture", LogicalModel: "logical-chat",
		Messages: []adapter.Message{{
			Role: adapter.RoleUser,
			Parts: []adapter.ContentPart{
				{Kind: adapter.ContentText, Text: "describe"},
				{Kind: adapter.ContentImageReference, Reference: "asset_image_1", MediaType: "image/png"},
			},
		}},
		Temperature: &temperature, TopP: &topP, MaxOutputTokens: &maxTokens,
		Stop: []string{"END"},
		Tools: []adapter.ToolDefinition{{
			Name: "lookup", Description: "Lookup", InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
		ToolChoice: &adapter.ToolChoice{Mode: adapter.ToolChoiceNamed, Name: "lookup"},
		ResponseFormat: &adapter.ResponseFormat{
			Type: adapter.ResponseFormatJSONSchema, Name: "result", Schema: json.RawMessage(`{"type":"object"}`), Strict: &strict,
		},
		ProviderOptions: json.RawMessage(`{"mock_scenario":"fixed-usage"}`),
	}
	httpRequest, err := built.BuildRequest(context.Background(), request)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if httpRequest.Method != http.MethodPost || httpRequest.URL.Path != "/v1/chat/completions" {
		t.Fatalf("unexpected request target: %s %s", httpRequest.Method, httpRequest.URL)
	}
	if httpRequest.Header.Get("Authorization") != "" || httpRequest.Header.Get("X-Request-ID") != request.RequestID {
		t.Fatalf("unsafe or missing headers: %v", httpRequest.Header)
	}
	body, err := io.ReadAll(httpRequest.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	for _, wanted := range []string{
		`"model":"mock-chat-v1"`, `"temperature":0.25`, `"top_p":0.9`, `"max_tokens":128`,
		`"image_url"`, `"lookup"`, `"json_schema"`, `"mock_scenario":"fixed-usage"`,
	} {
		if !bytes.Contains(body, []byte(wanted)) {
			t.Fatalf("request body missing %s: %s", wanted, body)
		}
	}
}

func TestBuildRequestRejectsInvalidOptionsAndOversize(t *testing.T) {
	t.Parallel()

	built, _, _ := newAdapterRuntime(t, mockprovider.NewHandler())
	tests := []struct {
		name    string
		stream  bool
		options string
		labels  []string
	}{
		{"unknown field", false, `{"unknown":true}`, nil},
		{"unknown scenario", false, `{"mock_scenario":"future"}`, nil},
		{"sse without stream", false, `{"mock_scenario":"sse"}`, nil},
		{"normal with stream", true, `{"mock_scenario":"normal"}`, nil},
		{"delay range", false, `{"mock_scenario":"delay","mock_delay_ms":5001}`, nil},
		{"delay on normal", false, `{"mock_scenario":"normal","mock_delay_ms":1}`, nil},
		{"policy labels", false, `{}`, []string{"content.internal"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := normalizedRequest(test.stream, "")
			request.ProviderOptions = json.RawMessage(test.options)
			request.PolicyLabels = test.labels
			_, err := built.BuildRequest(context.Background(), request)
			if !errors.Is(err, mockadapter.ErrUnsupportedParameter) {
				t.Fatalf("error = %v", err)
			}
			var parameterError *mockadapter.UnsupportedParameterError
			if !errors.As(err, &parameterError) || strings.Contains(err.Error(), test.options) {
				t.Fatalf("unsafe parameter error: %v", err)
			}
		})
	}

	large := normalizedRequest(false, "")
	large.Messages[0].Parts[0].Text = strings.Repeat("x", 1<<20)
	if _, err := built.BuildRequest(context.Background(), large); !errors.Is(err, mockadapter.ErrRequestTooLarge) {
		t.Fatalf("oversize error = %v", err)
	}
}

func TestBuildRequestHonorsEndpointPathVariants(t *testing.T) {
	t.Parallel()

	factory, err := mockadapter.NewFactory(mockadapter.FactoryOptions{Now: fixedClock})
	if err != nil {
		t.Fatalf("new factory: %v", err)
	}
	provider := mockProvider()
	for _, suffix := range []string{"", "/v1", "/v1/chat/completions"} {
		deployment := mockDeployment(provider.ID, "http://127.0.0.1:18082"+suffix)
		built, err := factory.New(context.Background(), provider, deployment)
		if err != nil {
			t.Fatalf("new adapter for %q: %v", suffix, err)
		}
		request, err := built.BuildRequest(context.Background(), normalizedRequest(false, ""))
		if err != nil {
			t.Fatalf("build for %q: %v", suffix, err)
		}
		if request.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path for %q = %q", suffix, request.URL.Path)
		}
	}
}

func TestBuildRequestContextValidationAndAudioMapping(t *testing.T) {
	t.Parallel()

	built, _, _ := newAdapterRuntime(t, mockprovider.NewHandler())
	var nilContext context.Context
	if _, err := built.BuildRequest(nilContext, normalizedRequest(false, "")); err == nil {
		t.Fatal("nil context must fail")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := built.BuildRequest(cancelled, normalizedRequest(false, "")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled build error = %v", err)
	}
	invalid := normalizedRequest(false, "")
	invalid.RequestID = "bad id"
	if _, err := built.BuildRequest(context.Background(), invalid); err == nil {
		t.Fatal("invalid normalized request must fail")
	}

	audio := normalizedRequest(false, "")
	audio.Messages[0].Parts = []adapter.ContentPart{
		{Kind: adapter.ContentText, Text: "listen"},
		{Kind: adapter.ContentAudioReference, Reference: "audio_fixture", MediaType: "audio/wav"},
	}
	audio.ToolChoice = &adapter.ToolChoice{Mode: adapter.ToolChoiceAuto}
	audio.ResponseFormat = &adapter.ResponseFormat{Type: adapter.ResponseFormatText}
	request, err := built.BuildRequest(context.Background(), audio)
	if err != nil {
		t.Fatalf("build audio request: %v", err)
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	for _, wanted := range []string{"input_audio", "audio_fixture", `"tool_choice":"auto"`, `"response_format":{"type":"text"}`} {
		if !strings.Contains(string(body), wanted) {
			t.Fatalf("body missing %q: %s", wanted, body)
		}
	}
}

var _ provideradapter.Factory = (*mockadapter.Factory)(nil)
