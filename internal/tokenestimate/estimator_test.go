package tokenestimate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/limits"
)

func TestEstimatorBindsTokenizerModelAndCachesContentDigests(t *testing.T) {
	estimator, err := New(Options{MaximumEntries: 8})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	request := estimateRequest("你好, cached world")
	response := estimateResponse("工具结果 ✓")
	deployment := estimateDeployment()

	first, err := estimator.EstimateUsage(context.Background(), deployment, request, &response)
	if err != nil {
		t.Fatalf("EstimateUsage(first) error = %v", err)
	}
	if first.Validate() != nil || first.Source != adapter.UsageSourceEstimated || first.Complete ||
		!first.InputTokens.Present || first.InputTokens.Value < 1 || !first.OutputTokens.Present ||
		first.OutputTokens.Value < 1 || first.RawEvidence.Present() || first.Estimate == nil ||
		!first.Estimate.Estimated || first.Estimate.Tokenizer != defaultAlgorithm ||
		first.Estimate.TokenizerVersion != defaultAlgorithmV1 ||
		first.Estimate.PhysicalModel != deployment.PhysicalModel ||
		first.Estimate.DeploymentVersion != deployment.Version ||
		first.Estimate.ProviderProtocolVersion != deployment.Capabilities.ProviderProtocolVersion {
		t.Fatalf("EstimateUsage(first) = %+v", first)
	}
	second, err := estimator.EstimateUsage(context.Background(), deployment, request, &response)
	if err != nil || second.InputTokens != first.InputTokens || second.OutputTokens != first.OutputTokens {
		t.Fatalf("EstimateUsage(second) = %+v, %v; want %+v", second, err, first)
	}
	stats := estimator.Stats()
	if stats.Entries != 2 || stats.Misses != 2 || stats.Hits != 2 {
		t.Fatalf("Stats() = %+v, want entries/misses/hits 2/2/2", stats)
	}

	inputOnly, err := estimator.EstimateUsage(context.Background(), deployment, request, nil)
	if err != nil || !inputOnly.InputTokens.Present || inputOnly.OutputTokens.Present || inputOnly.Estimate == nil {
		t.Fatalf("EstimateUsage(input only) = %+v, %v", inputOnly, err)
	}
}

func TestEstimatorCacheIsBoundedAndConcurrent(t *testing.T) {
	estimator, err := New(Options{MaximumEntries: 2})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	deployment := estimateDeployment()
	var wait sync.WaitGroup
	for worker := range 16 {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			for iteration := range 50 {
				request := estimateRequest(string(rune('a' + (index+iteration)%4)))
				if _, _, estimateErr := estimator.EstimateInput(context.Background(), deployment, request); estimateErr != nil {
					t.Errorf("EstimateInput() error = %v", estimateErr)
					return
				}
			}
		}(worker)
	}
	wait.Wait()
	stats := estimator.Stats()
	if stats.Entries < 1 || stats.Entries > 2 || stats.Hits+stats.Misses != 800 {
		t.Fatalf("bounded concurrent Stats() = %+v", stats)
	}
}

func TestEstimatorFailsClosedAndAdaptsToTPM(t *testing.T) {
	if _, err := New(Options{MaximumEntries: -1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("New(invalid) error = %v", err)
	}
	estimator, err := New(Options{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	deployment := estimateDeployment()
	request := estimateRequest("bounded")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := estimator.EstimateInput(cancelled, deployment, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("EstimateInput(cancelled) error = %v", err)
	}
	invalidDeployment := deployment
	invalidDeployment.Version = 0
	if _, _, err := estimator.EstimateInput(context.Background(), invalidDeployment, request); !errors.Is(err, ErrInvalid) {
		t.Fatalf("EstimateInput(invalid deployment) error = %v", err)
	}

	bound, err := estimator.BindInput(deployment)
	if err != nil {
		t.Fatalf("BindInput() error = %v", err)
	}
	plan, err := limits.PlanTPMReservation(
		context.Background(), bound, request, 100_000,
	)
	if err != nil || !plan.Estimated || plan.Tokenizer != defaultAlgorithm ||
		plan.PhysicalModel != deployment.PhysicalModel || plan.DeploymentVersion != deployment.Version ||
		plan.ReservedTokens != plan.InputTokens+plan.MaximumOutputTokens {
		t.Fatalf("PlanTPMReservation() = %+v, %v", plan, err)
	}
}

func estimateDeployment() catalog.Deployment {
	now := time.Date(2026, time.August, 5, 1, 2, 3, 0, time.UTC)
	return catalog.Deployment{
		ID: "7f000000-0000-4000-8000-000000000001", ProviderID: "7f000000-0000-4000-8000-000000000002",
		Code: "estimate-fixture", PhysicalModel: "gpt-fixture-2026-08-05",
		EndpointURL: "https://provider.invalid/v1", Region: "us-east-1",
		Capabilities: catalog.CapabilitySet{
			Chat: true, Tools: true, StructuredOutput: true,
			MaxContextTokens: 1_000_000, MaxOutputTokens: 100_000,
			DataRetentionMode: catalog.RetentionNoTraining, ProviderProtocolVersion: "openai-chat-v1",
		},
		Status: catalog.StatusActive, Version: 7, CreatedAt: now, CreatedBy: "test",
		UpdatedAt: now, UpdatedBy: "test",
	}
}

func estimateRequest(content string) adapter.NormalizedRequest {
	maximum := int64(256)
	return adapter.NormalizedRequest{
		RequestID: "token-estimate-request", LogicalModel: "support-chat", MaxOutputTokens: &maximum,
		Messages: []adapter.Message{{
			Role: adapter.RoleUser, Parts: []adapter.ContentPart{{Kind: adapter.ContentText, Text: content}},
		}},
		Tools: []adapter.ToolDefinition{{
			Name: "lookup", Description: "find a record",
			InputSchema: []byte(`{"type":"object","properties":{"id":{"type":"string"}}}`),
		}},
	}
}

func estimateResponse(content string) adapter.NormalizedResponse {
	return adapter.NormalizedResponse{
		ResponseID: "token-estimate-response", Model: "gpt-fixture-2026-08-05",
		Choices: []adapter.NormalizedChoice{{
			Index: 0, Message: adapter.Message{
				Role: adapter.RoleAssistant, Parts: []adapter.ContentPart{{Kind: adapter.ContentText, Text: content}},
			},
			FinishReason: adapter.FinishStop, ProviderFinishReason: "stop",
		}},
		ProviderRequestID: "provider-estimate-response",
		ObservedAt:        time.Date(2026, time.August, 5, 1, 2, 4, 0, time.UTC),
	}
}
