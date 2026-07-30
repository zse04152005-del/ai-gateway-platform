package protocolcanary_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/protocolcanary"
	"github.com/zse04152005-del/ai-gateway-platform/internal/provideradapter"
)

func TestProbeValidationMatrix(t *testing.T) {
	t.Parallel()

	valid := canaryProbe("http://127.0.0.1:18082")
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid probe: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*protocolcanary.Probe)
	}{
		{name: "invalid id", mutate: func(value *protocolcanary.Probe) { value.ID = "Invalid ID" }},
		{name: "disabled provider", mutate: func(value *protocolcanary.Probe) {
			value.Provider.Status = "disabled"
		}},
		{name: "cross provider", mutate: func(value *protocolcanary.Probe) {
			value.Deployment.ProviderID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
		}},
		{name: "short timeout", mutate: func(value *protocolcanary.Probe) { value.Timeout = time.Millisecond }},
		{name: "multiple messages", mutate: func(value *protocolcanary.Probe) {
			value.Request.Messages = append(value.Request.Messages, value.Request.Messages[0])
		}},
		{name: "missing maximum", mutate: func(value *protocolcanary.Probe) { value.Request.MaxOutputTokens = nil }},
		{name: "tool enabled", mutate: func(value *protocolcanary.Probe) {
			value.Request.Tools = []adapter.ToolDefinition{{Name: "lookup", InputSchema: []byte(`{}`)}}
		}},
		{name: "stream capability missing", mutate: func(value *protocolcanary.Probe) {
			value.Request.Stream = true
			value.Deployment.Capabilities.Stream = false
			value.Deployment.Capabilities.UsageInStream = false
		}},
		{name: "unsorted baseline", mutate: func(value *protocolcanary.Probe) {
			value.Baseline.AllowedFinishReasons = []adapter.FinishReason{adapter.FinishStop, adapter.FinishLength}
		}},
		{name: "unknown baseline", mutate: func(value *protocolcanary.Probe) {
			value.Baseline.AllowedFinishReasons = []adapter.FinishReason{adapter.FinishUnknown}
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			probe := canaryProbe("http://127.0.0.1:18082")
			test.mutate(&probe)
			if err := probe.Validate(); err == nil {
				t.Fatal("invalid probe passed validation")
			}
		})
	}
}

func TestResultValidationMatrix(t *testing.T) {
	t.Parallel()

	valid := validResult()
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid result: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*protocolcanary.Result)
	}{
		{name: "invalid identity", mutate: func(value *protocolcanary.Result) { value.ProbeID = "" }},
		{name: "invalid outcome", mutate: func(value *protocolcanary.Result) { value.Outcome = "mystery" }},
		{name: "bad timestamps", mutate: func(value *protocolcanary.Result) { value.Duration = time.Second }},
		{name: "drift without finding", mutate: func(value *protocolcanary.Result) { value.Outcome = protocolcanary.OutcomeDrift }},
		{name: "finding on stable", mutate: func(value *protocolcanary.Result) {
			value.Findings = []protocolcanary.Finding{{Code: protocolcanary.FindingMissingUsage, Path: "/usage"}}
		}},
		{name: "failure missing facts", mutate: func(value *protocolcanary.Result) {
			value.Outcome = protocolcanary.OutcomeProviderFailure
		}},
		{name: "facts on stable", mutate: func(value *protocolcanary.Result) {
			value.Failure = &protocolcanary.ProviderFailure{
				Code: "FAILED", Category: adapter.ErrorProvider5xx, Retryable: true, ProviderStatus: 500,
			}
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			result := validResult()
			test.mutate(&result)
			if err := result.Validate(); err == nil {
				t.Fatal("invalid result passed validation")
			}
		})
	}
}

func TestNewRunnerRejectsMissingAndUnboundedDependencies(t *testing.T) {
	t.Parallel()

	var nilBuilder *fakeBuilder
	tests := []protocolcanary.Options{
		{},
		{Builder: nilBuilder, Client: http.DefaultClient},
		{Builder: &fakeBuilder{}, Client: nil},
		{Builder: &fakeBuilder{}, Client: http.DefaultClient, Now: func() time.Time { return time.Time{} }},
		{Builder: &fakeBuilder{}, Client: http.DefaultClient, DefaultTimeout: time.Hour},
		{Builder: &fakeBuilder{}, Client: http.DefaultClient, MaximumChunks: 5000},
	}
	for index, options := range tests {
		if _, err := protocolcanary.NewRunner(options); err == nil {
			t.Fatalf("options %d passed validation", index)
		}
	}
}

func TestRunnerRejectsInvalidAssemblyWithoutLeakingCause(t *testing.T) {
	t.Parallel()

	runner, err := protocolcanary.NewRunner(protocolcanary.Options{
		Builder: &fakeBuilder{err: errors.New("private-builder-secret")}, Client: http.DefaultClient,
	})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	_, err = runner.Run(context.Background(), canaryProbe("http://127.0.0.1:18082"))
	if !errors.Is(err, protocolcanary.ErrConfiguration) || err == nil ||
		containsAny(err.Error(), "private-builder-secret", "synthetic-canary-prompt") {
		t.Fatalf("safe configuration error = %v", err)
	}
}

func validResult() protocolcanary.Result {
	started := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	return protocolcanary.Result{
		ProbeID: "mock.minimal", ProviderID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		DeploymentID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", AdapterType: "mock",
		ProviderProtocolVersion: "mock-v1", Outcome: protocolcanary.OutcomeStable,
		StartedAt: started, FinishedAt: started.Add(time.Millisecond), Duration: time.Millisecond,
	}
}

type fakeBuilder struct {
	err error
}

func (builder *fakeBuilder) Build(
	_ context.Context,
	interfaceProvider catalog.Provider,
	interfaceDeployment catalog.Deployment,
) (provideradapter.Adapter, error) {
	_ = interfaceProvider
	_ = interfaceDeployment
	return nil, builder.err
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
