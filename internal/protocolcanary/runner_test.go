package protocolcanary_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/httpserver"
	"github.com/zse04152005-del/ai-gateway-platform/internal/mockadapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/mockprovider"
	"github.com/zse04152005-del/ai-gateway-platform/internal/protocolcanary"
	"github.com/zse04152005-del/ai-gateway-platform/internal/provideradapter"
)

func TestRunnerReportsStableOrdinaryAndStreamMockProbes(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		stream   bool
		scenario string
	}{
		{name: "ordinary", scenario: mockadapter.ScenarioNormal},
		{name: "stream", stream: true, scenario: mockadapter.ScenarioSSE},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			runner, probe := newCanaryRuntime(t, mockprovider.NewHandler(), protocolcanary.Options{})
			probe.Request.Stream = test.stream
			probe.Request.ProviderOptions = scenarioOptions(test.scenario)
			result, err := runner.Run(context.Background(), probe)
			if err != nil {
				t.Fatalf("run canary: %v", err)
			}
			if result.Outcome != protocolcanary.OutcomeStable || len(result.Findings) != 0 || result.Failure != nil {
				t.Fatalf("result = %#v", result)
			}
			if err := result.Validate(); err != nil {
				t.Fatalf("validate result: %v", err)
			}
		})
	}
}

func TestRunnerDetectsOrdinaryProtocolUsageAndFinishDrift(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		handler http.Handler
		codes   []protocolcanary.FindingCode
	}{
		{
			name:    "unknown response field",
			handler: jsonHandler(`{"id":"canary-unknown","object":"chat.completion","model":"mock-chat-v1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2},"future_field":"private-protocol-marker"}`),
			codes:   []protocolcanary.FindingCode{protocolcanary.FindingProtocolViolation},
		},
		{
			name:    "usage and finish",
			handler: jsonHandler(`{"id":"canary-drift","object":"chat.completion","model":"mock-chat-v1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"future_finish"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"future_meter":7}}`),
			codes: []protocolcanary.FindingCode{
				protocolcanary.FindingUnexpectedFinishReason,
				protocolcanary.FindingUnmappedUsageField,
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			runner, probe := newCanaryRuntime(t, test.handler, protocolcanary.Options{})
			result, err := runner.Run(context.Background(), probe)
			if err != nil {
				t.Fatalf("run canary: %v", err)
			}
			if result.Outcome != protocolcanary.OutcomeDrift || !hasFindingCodes(result.Findings, test.codes...) {
				t.Fatalf("result = %#v", result)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatalf("marshal result: %v", err)
			}
			var logBuffer bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logBuffer, nil))
			logger.Info("canary", "result", result)
			for _, secret := range []string{"synthetic-canary-prompt", "private-protocol-marker", "future_finish"} {
				if bytes.Contains(encoded, []byte(secret)) || strings.Contains(logBuffer.String(), secret) {
					t.Fatalf("safe result exposed %q: %s / %s", secret, encoded, logBuffer.String())
				}
			}
		})
	}
}

func TestRunnerDetectsStreamExtensionMissingUsageAndChunkLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		handler    http.Handler
		chunkLimit int
		want       protocolcanary.FindingCode
	}{
		{name: "provider extension", handler: unknownStreamHandler(), want: protocolcanary.FindingProviderExtension},
		{name: "missing usage", handler: missingUsageStreamHandler(), want: protocolcanary.FindingMissingUsage},
		{name: "chunk limit", handler: longStreamHandler(), chunkLimit: 2, want: protocolcanary.FindingChunkLimit},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			runner, probe := newCanaryRuntime(t, test.handler, protocolcanary.Options{MaximumChunks: test.chunkLimit})
			probe.Request.Stream = true
			result, err := runner.Run(context.Background(), probe)
			if err != nil {
				t.Fatalf("run canary: %v", err)
			}
			if result.Outcome != protocolcanary.OutcomeDrift || !hasFindingCodes(result.Findings, test.want) {
				t.Fatalf("result = %#v", result)
			}
			if test.want == protocolcanary.FindingProviderExtension && result.Findings[0].Fingerprint == "" {
				t.Fatal("provider extension omitted its safe fingerprint")
			}
		})
	}
}

func TestRunnerSeparatesProviderFailureTimeoutAndCancellation(t *testing.T) {
	t.Parallel()

	t.Run("provider failure", func(t *testing.T) {
		t.Parallel()
		runner, probe := newCanaryRuntime(t, mockprovider.NewHandler(), protocolcanary.Options{})
		probe.Request.ProviderOptions = scenarioOptions(mockadapter.ScenarioRateLimit)
		result, err := runner.Run(context.Background(), probe)
		if err != nil {
			t.Fatalf("run rate limit canary: %v", err)
		}
		if result.Outcome != protocolcanary.OutcomeProviderFailure || result.Failure == nil ||
			result.Failure.Category != adapter.ErrorRateLimit || !result.Failure.Retryable ||
			result.Failure.ProviderStatus != http.StatusTooManyRequests {
			t.Fatalf("result = %#v", result)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		runner, probe := newCanaryRuntime(t, mockprovider.NewHandler(), protocolcanary.Options{})
		probe.Timeout = 20 * time.Millisecond
		probe.Request.ProviderOptions = json.RawMessage(`{"mock_scenario":"delay","mock_delay_ms":200}`)
		result, err := runner.Run(context.Background(), probe)
		if err != nil {
			t.Fatalf("run timeout canary: %v", err)
		}
		if result.Outcome != protocolcanary.OutcomeTimeout {
			t.Fatalf("result = %#v", result)
		}
	})

	t.Run("caller cancellation", func(t *testing.T) {
		t.Parallel()
		runner, probe := newCanaryRuntime(t, mockprovider.NewHandler(), protocolcanary.Options{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err := runner.Run(ctx, probe)
		if err != nil {
			t.Fatalf("run cancelled canary: %v", err)
		}
		if result.Outcome != protocolcanary.OutcomeCancelled {
			t.Fatalf("result = %#v", result)
		}
	})
}

func newCanaryRuntime(
	t *testing.T,
	handler http.Handler,
	overrides protocolcanary.Options,
) (*protocolcanary.Runner, protocolcanary.Probe) {
	t.Helper()
	shared, err := httpserver.NewServer(httpserver.Options{
		ServiceName: "protocol-canary-test", Version: "test",
		NotReadyCode: "CANARY_NOT_READY", NotReadyMessage: "Canary fixture is not ready",
		ErrorType: "canary_error", ReadHeaderTimeout: time.Second, ShutdownTimeout: time.Second,
		ApplicationHandler: handler,
	})
	if err != nil {
		t.Fatalf("new shared fixture server: %v", err)
	}
	server := httptest.NewServer(shared.Handler())
	t.Cleanup(server.Close)
	factory, err := mockadapter.NewFactory(mockadapter.FactoryOptions{Now: canaryAdapterClock})
	if err != nil {
		t.Fatalf("new mock adapter factory: %v", err)
	}
	registry, err := provideradapter.NewRegistry(factory)
	if err != nil {
		t.Fatalf("new adapter registry: %v", err)
	}
	clock := newIncrementingClock()
	options := protocolcanary.Options{
		Builder: registry, Client: server.Client(), Now: clock,
		DefaultTimeout: 2 * time.Second, MaximumChunks: overrides.MaximumChunks,
	}
	runner, err := protocolcanary.NewRunner(options)
	if err != nil {
		t.Fatalf("new protocol canary runner: %v", err)
	}
	return runner, canaryProbe(server.URL)
}

func canaryProbe(endpoint string) protocolcanary.Probe {
	now := canaryAdapterClock()
	provider := catalog.Provider{
		ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Code: "canary-mock", Name: "Canary Mock",
		AdapterType: string(mockadapter.Type), Status: catalog.StatusActive, Version: 1,
		CreatedAt: now, CreatedBy: "test", UpdatedAt: now, UpdatedBy: "test",
	}
	deployment := catalog.Deployment{
		ID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", ProviderID: provider.ID,
		Code: "canary", PhysicalModel: "mock-chat-v1", EndpointURL: endpoint, Region: "local",
		Capabilities: catalog.CapabilitySet{
			Chat: true, Stream: true, UsageInStream: true, CacheUsage: true,
			MaxContextTokens: 8192, MaxOutputTokens: 2048,
			DataRetentionMode: catalog.RetentionSelfHosted, ProviderProtocolVersion: "mock-v1",
		},
		Status: catalog.StatusActive, Version: 1,
		CreatedAt: now, CreatedBy: "test", UpdatedAt: now, UpdatedBy: "test",
	}
	maximum := int64(1)
	return protocolcanary.Probe{
		ID: "mock.minimal", Provider: provider, Deployment: deployment,
		Request: adapter.NormalizedRequest{
			RequestID: "req_protocol_canary", LogicalModel: "canary-model", MaxOutputTokens: &maximum,
			Messages: []adapter.Message{{
				Role:  adapter.RoleUser,
				Parts: []adapter.ContentPart{{Kind: adapter.ContentText, Text: "synthetic-canary-prompt"}},
			}},
		},
		Baseline: protocolcanary.Baseline{
			AllowedFinishReasons: []adapter.FinishReason{adapter.FinishStop}, RequireUsage: true,
		},
	}
}

func jsonHandler(body string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, body)
	})
}

func unknownStreamHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, `data: {"id":"canary-stream","object":"chat.completion.chunk","model":"mock-chat-v1","choices":[{"index":0,"delta":{"role":"assistant","future_delta":"private-stream-marker"},"finish_reason":null}],"future_top":true}`+"\n\n")
		_, _ = io.WriteString(writer, `data: {"id":"canary-stream","object":"chat.completion.chunk","model":"mock-chat-v1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`+"\n\n")
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	})
}

func missingUsageStreamHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, `data: {"id":"canary-missing","object":"chat.completion.chunk","model":"mock-chat-v1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`+"\n\n")
		_, _ = io.WriteString(writer, `data: {"id":"canary-missing","object":"chat.completion.chunk","model":"mock-chat-v1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	})
}

func longStreamHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, `data: {"id":"canary-long","object":"chat.completion.chunk","model":"mock-chat-v1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`+"\n\n")
		for _, content := range []string{"one", "two", "three"} {
			_, _ = io.WriteString(writer, `data: {"id":"canary-long","object":"chat.completion.chunk","model":"mock-chat-v1","choices":[{"index":0,"delta":{"content":"`+content+`"},"finish_reason":null}]}`+"\n\n")
		}
		_, _ = io.WriteString(writer, `data: {"id":"canary-long","object":"chat.completion.chunk","model":"mock-chat-v1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`+"\n\n")
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	})
}

func scenarioOptions(scenario string) json.RawMessage {
	return json.RawMessage(`{"mock_scenario":"` + scenario + `"}`)
}

func hasFindingCodes(findings []protocolcanary.Finding, wanted ...protocolcanary.FindingCode) bool {
	for _, code := range wanted {
		found := false
		for _, finding := range findings {
			found = found || finding.Code == code
		}
		if !found {
			return false
		}
	}
	return true
}

func newIncrementingClock() func() time.Time {
	var mutex sync.Mutex
	current := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	return func() time.Time {
		mutex.Lock()
		defer mutex.Unlock()
		current = current.Add(time.Millisecond)
		return current
	}
}

func canaryAdapterClock() time.Time {
	return time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
}
