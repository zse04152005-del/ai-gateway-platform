package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/execution"
	"github.com/zse04152005-del/ai-gateway-platform/internal/proxy"
	"github.com/zse04152005-del/ai-gateway-platform/internal/retry"
	"github.com/zse04152005-del/ai-gateway-platform/internal/routedecision"
	"github.com/zse04152005-del/ai-gateway-platform/internal/routing"
)

func TestConcurrentFailureStormAmplificationIsStrictlyLinear(t *testing.T) {
	const (
		requestCount   = 64
		maximumAttempt = 3
	)
	selector := &stormSelector{deployments: []string{
		failoverDeploymentA, failoverDeploymentB, failoverDeploymentC,
	}}
	executor := &stormExecutor{failure: proxy.ErrTransport, hold: time.Millisecond}
	executionRecorder := &stormExecutionRecorder{}
	decisionRecorder := &stormDecisionRecorder{}
	coordinator, err := newNonStreamFailover(
		selector, executor, executionRecorder, decisionRecorder,
		FailoverOptions{
			MaximumAttempts: maximumAttempt, TotalTimeout: 5 * time.Second,
			MinimumAttemptWindow: 10 * time.Millisecond, AdditionalCost: retry.CostAllowed,
		},
		time.Now, noWait,
	)
	if err != nil {
		t.Fatalf("newNonStreamFailover() error = %v", err)
	}

	start := make(chan struct{})
	errorsChannel := make(chan error, requestCount)
	var wait sync.WaitGroup
	for index := range requestCount {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			requestID := fmt.Sprintf("req_failure_storm_%03d", index)
			providerRequest := failoverProviderRequest()
			providerRequest.RequestID = requestID
			selectionRequest := failoverSelectionRequest()
			selectionRequest.Request = providerRequest.Clone()
			_, executeErr := coordinator.Execute(
				context.Background(), selectionRequest,
				execution.GatewayRequest{ID: requestID, Status: execution.RequestRouting, Version: 2},
				providerRequest, requestID,
			)
			if !errors.Is(executeErr, proxy.ErrTransport) {
				errorsChannel <- fmt.Errorf("request %s error = %w", requestID, executeErr)
			}
		}()
	}
	close(start)
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		t.Error(err)
	}

	wantPhysicalCalls := int64(requestCount * maximumAttempt)
	wantRetryCompletions := int64(requestCount * (maximumAttempt - 1))
	if selector.calls.Load() != wantPhysicalCalls || executor.calls.Load() != wantPhysicalCalls ||
		executionRecorder.attempts.Load() != wantPhysicalCalls ||
		decisionRecorder.routes.Load() != wantPhysicalCalls ||
		decisionRecorder.retries.Load() != wantPhysicalCalls {
		t.Fatalf("storm amplification selector/provider/attempt/route/retry = %d/%d/%d/%d/%d, want %d each",
			selector.calls.Load(), executor.calls.Load(), executionRecorder.attempts.Load(),
			decisionRecorder.routes.Load(), decisionRecorder.retries.Load(), wantPhysicalCalls)
	}
	if executionRecorder.retryCompletions.Load() != wantRetryCompletions ||
		executionRecorder.terminalCompletions.Load() != requestCount ||
		executionRecorder.failedRequests.Load() != 0 {
		t.Fatalf("storm completion counts retry/terminal/request-failed = %d/%d/%d",
			executionRecorder.retryCompletions.Load(), executionRecorder.terminalCompletions.Load(),
			executionRecorder.failedRequests.Load())
	}
	if executor.sameRequestOverlap.Load() != 0 || executor.maximumInFlight.Load() > requestCount {
		t.Fatalf("storm concurrency same-request-overlap/max-in-flight = %d/%d",
			executor.sameRequestOverlap.Load(), executor.maximumInFlight.Load())
	}
}

func TestFailureStormSharedDeadlineStopsBeforeLargeAttemptLimit(t *testing.T) {
	failure := mustProviderError(t, adapter.NormalizedError{
		Code: "PROVIDER_RATE_LIMITED", Category: adapter.ErrorRateLimit, Retryable: true,
		ProviderStatus: 429, SafeMessage: "Provider rate limited request",
	})
	selector := &stormSelector{deployments: []string{failoverDeploymentA}}
	executor := &stormExecutor{failure: failure}
	executionRecorder := &stormExecutionRecorder{}
	decisionRecorder := &stormDecisionRecorder{}
	base := time.Now().UTC()
	var clockCalls atomic.Int64
	advancingClock := func() time.Time {
		step := clockCalls.Add(1) - 1
		return base.Add(time.Duration(step) * 1100 * time.Millisecond)
	}
	coordinator, err := newNonStreamFailover(
		selector, executor, executionRecorder, decisionRecorder,
		FailoverOptions{
			MaximumAttempts: 32, TotalTimeout: 3 * time.Second,
			MinimumAttemptWindow: 10 * time.Millisecond, AdditionalCost: retry.CostAllowed,
		},
		advancingClock, noWait,
	)
	if err != nil {
		t.Fatalf("newNonStreamFailover() error = %v", err)
	}

	startedAt := time.Now()
	_, executeErr := coordinator.Execute(
		context.Background(), failoverSelectionRequest(), failoverRecordedRequest(),
		failoverProviderRequest(), "req_failure_storm_deadline",
	)
	if !errors.Is(executeErr, failure) {
		t.Fatalf("Execute() error = %v, want provider failure", executeErr)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("deadline-bounded storm took %s", elapsed)
	}
	if executor.calls.Load() != 3 || executionRecorder.attempts.Load() != 3 ||
		executionRecorder.retryCompletions.Load() != 2 || executionRecorder.terminalCompletions.Load() != 1 ||
		selector.calls.Load() != 5 || decisionRecorder.routes.Load() != 5 || decisionRecorder.retries.Load() != 3 {
		t.Fatalf("deadline counts provider/attempt/retry-complete/terminal/selector/route/retry = %d/%d/%d/%d/%d/%d/%d",
			executor.calls.Load(), executionRecorder.attempts.Load(), executionRecorder.retryCompletions.Load(),
			executionRecorder.terminalCompletions.Load(), selector.calls.Load(), decisionRecorder.routes.Load(),
			decisionRecorder.retries.Load())
	}
	lastRetry, ok := decisionRecorder.lastRetry.Load().(retry.Decision)
	if !ok || lastRetry.Action != retry.NoRetry || lastRetry.Reason != retry.ReasonDeadlineExhausted ||
		lastRetry.AttemptNumber != 3 || lastRetry.MaximumAttempts != 32 {
		t.Fatalf("final deadline decision = %+v/%t", lastRetry, ok)
	}
}

func TestFailureStormCostDenialStopsBeforeSecondPhysicalCall(t *testing.T) {
	selector := &stormSelector{deployments: []string{
		failoverDeploymentA, failoverDeploymentB, failoverDeploymentC,
	}}
	executor := &stormExecutor{failure: proxy.ErrTransport}
	executionRecorder := &stormExecutionRecorder{}
	decisionRecorder := &stormDecisionRecorder{}
	coordinator, err := newNonStreamFailover(
		selector, executor, executionRecorder, decisionRecorder,
		FailoverOptions{
			MaximumAttempts: 32, TotalTimeout: 5 * time.Second,
			MinimumAttemptWindow: 10 * time.Millisecond, AdditionalCost: retry.CostDenied,
		},
		time.Now, noWait,
	)
	if err != nil {
		t.Fatalf("newNonStreamFailover() error = %v", err)
	}

	_, executeErr := coordinator.Execute(
		context.Background(), failoverSelectionRequest(), failoverRecordedRequest(),
		failoverProviderRequest(), "req_failure_storm_cost_denied",
	)
	if !errors.Is(executeErr, proxy.ErrTransport) || selector.calls.Load() != 1 ||
		executor.calls.Load() != 1 || executionRecorder.attempts.Load() != 1 ||
		executionRecorder.retryCompletions.Load() != 0 || executionRecorder.terminalCompletions.Load() != 1 ||
		decisionRecorder.routes.Load() != 1 || decisionRecorder.retries.Load() != 1 {
		t.Fatalf("cost-denied counts error/selector/provider/attempt/retry-complete/terminal/route/retry = %v/%d/%d/%d/%d/%d/%d/%d",
			executeErr, selector.calls.Load(), executor.calls.Load(), executionRecorder.attempts.Load(),
			executionRecorder.retryCompletions.Load(), executionRecorder.terminalCompletions.Load(),
			decisionRecorder.routes.Load(), decisionRecorder.retries.Load())
	}
	lastRetry, ok := decisionRecorder.lastRetry.Load().(retry.Decision)
	if !ok || lastRetry.Action != retry.NoRetry || lastRetry.Reason != retry.ReasonCostBudget ||
		lastRetry.AttemptNumber != 1 || lastRetry.MaximumAttempts != 32 {
		t.Fatalf("final cost decision = %+v/%t", lastRetry, ok)
	}
}

type stormSelector struct {
	deployments []string
	calls       atomic.Int64
}

func (selector *stormSelector) Select(
	ctx context.Context,
	request routing.SelectionRequest,
) (routing.Selection, error) {
	selector.calls.Add(1)
	if err := ctx.Err(); err != nil {
		return routing.Selection{Filter: selector.filter(request.ExcludedDeploymentIDs)}, err
	}
	filter := selector.filter(request.ExcludedDeploymentIDs)
	for _, decision := range filter.Decisions {
		if !decision.Eligible {
			continue
		}
		eligibleCount := 0
		for _, candidate := range filter.Decisions {
			if candidate.Eligible {
				eligibleCount++
			}
		}
		return routing.Selection{
			Candidate: catalog.RouteCandidate{Deployment: catalog.Deployment{ID: decision.DeploymentID}},
			Filter:    filter,
			Decision: routing.PolicyDecision{
				PolicyVersion: "failure-storm-priority/v1", Mode: routing.RoutePriority,
				SelectedDeploymentID: decision.DeploymentID, EligibleCount: eligibleCount,
			},
		}, nil
	}
	return routing.Selection{Filter: filter}, routing.ErrNoCandidate
}

func (selector *stormSelector) filter(excluded []string) routing.FilterResult {
	filter := routing.FilterResult{
		PolicyVersion: "candidate-filter/v1",
		Decisions:     make([]routing.CandidateDecision, 0, len(selector.deployments)),
	}
	for _, deploymentID := range selector.deployments {
		decision := routing.CandidateDecision{
			DeploymentID: deploymentID, Eligible: true, Reason: routing.FilterEligible,
		}
		if containsDeployment(excluded, deploymentID) {
			decision.Eligible = false
			decision.Reason = routing.FilterPreviouslyAttempted
		}
		filter.Decisions = append(filter.Decisions, decision)
	}
	return filter
}

type stormExecutor struct {
	failure            error
	hold               time.Duration
	calls              atomic.Int64
	inFlight           atomic.Int64
	maximumInFlight    atomic.Int64
	sameRequestOverlap atomic.Int64
	activeRequests     sync.Map
}

func (executor *stormExecutor) Execute(
	ctx context.Context,
	_ routing.Selection,
	request adapter.NormalizedRequest,
) (adapter.NormalizedResponse, error) {
	executor.calls.Add(1)
	if _, loaded := executor.activeRequests.LoadOrStore(request.RequestID, struct{}{}); loaded {
		executor.sameRequestOverlap.Add(1)
	}
	defer executor.activeRequests.Delete(request.RequestID)
	inFlight := executor.inFlight.Add(1)
	updateAtomicMaximum(&executor.maximumInFlight, inFlight)
	defer executor.inFlight.Add(-1)
	if executor.hold > 0 {
		timer := time.NewTimer(executor.hold)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return adapter.NormalizedResponse{}, ctx.Err()
		case <-timer.C:
		}
	}
	return adapter.NormalizedResponse{}, executor.failure
}

func updateAtomicMaximum(target *atomic.Int64, candidate int64) {
	for current := target.Load(); candidate > current; current = target.Load() {
		if target.CompareAndSwap(current, candidate) {
			return
		}
	}
}

type stormDecisionRecorder struct {
	routes    atomic.Int64
	retries   atomic.Int64
	lastRetry atomic.Value
}

func (recorder *stormDecisionRecorder) Record(
	_ context.Context,
	input routedecision.Input,
) (routedecision.Record, error) {
	if err := input.Validate(); err != nil {
		return routedecision.Record{}, err
	}
	decisionNo := int(recorder.routes.Add(1))
	return routedecision.Record{
		RequestID: input.RequestID, DecisionNo: decisionNo, NextAttemptNo: input.NextAttemptNo,
		Outcome: input.Outcome, Filter: input.Filter, Policy: input.Policy, Retry: input.Retry,
		DecidedAt: time.Now(),
	}, nil
}

func (recorder *stormDecisionRecorder) RecordRetry(
	_ context.Context,
	input routedecision.RetryInput,
) (routedecision.RetryRecord, error) {
	if err := input.Validate(); err != nil {
		return routedecision.RetryRecord{}, err
	}
	recorder.retries.Add(1)
	recorder.lastRetry.Store(input.Decision)
	return routedecision.RetryRecord{
		RequestID: input.RequestID, AttemptNo: input.AttemptNo,
		Decision: input.Decision, DecidedAt: time.Now(),
	}, nil
}

type stormExecutionRecorder struct {
	attempts            atomic.Int64
	retryCompletions    atomic.Int64
	terminalCompletions atomic.Int64
	failedRequests      atomic.Int64
}

func (*stormExecutionRecorder) StartRequest(
	_ context.Context,
	start execution.StartRequest,
) (execution.GatewayRequest, error) {
	return execution.GatewayRequest{ID: start.ID, Status: execution.RequestAuthorized, Version: 1}, nil
}

func (*stormExecutionRecorder) MarkRouting(
	_ context.Context,
	request execution.GatewayRequest,
) (execution.GatewayRequest, error) {
	request.Status = execution.RequestRouting
	request.Version++
	return request, nil
}

func (recorder *stormExecutionRecorder) FailRequest(
	_ context.Context,
	request execution.GatewayRequest,
	status execution.RequestStatus,
	_ string,
) (execution.GatewayRequest, error) {
	recorder.failedRequests.Add(1)
	request.Status = status
	request.Version++
	return request, nil
}

func (recorder *stormExecutionRecorder) StartAttempt(
	_ context.Context,
	request execution.GatewayRequest,
	deploymentID string,
) (execution.GatewayRequest, execution.RouteAttempt, error) {
	recorder.attempts.Add(1)
	request.Status = execution.RequestRunning
	request.Version++
	request.AttemptCount++
	return request, execution.RouteAttempt{
		ID: "69000000-0000-4000-8000-000000000001", RequestID: request.ID,
		AttemptNo: request.AttemptCount, DeploymentID: deploymentID,
		Status: execution.AttemptConnecting, Version: 2,
	}, nil
}

func (*stormExecutionRecorder) MarkAttemptStreaming(
	_ context.Context,
	_ execution.GatewayRequest,
	attempt execution.RouteAttempt,
	providerRequestID string,
) (execution.RouteAttempt, error) {
	attempt.Status = execution.AttemptStreaming
	attempt.ProviderRequestID = providerRequestID
	attempt.Version += 2
	return attempt, nil
}

func (recorder *stormExecutionRecorder) CompleteAttemptForRetry(
	_ context.Context,
	_ execution.GatewayRequest,
	attempt execution.RouteAttempt,
	outcome execution.AttemptOutcome,
) (execution.RouteAttempt, error) {
	if err := outcome.Validate(); err != nil {
		return execution.RouteAttempt{}, err
	}
	recorder.retryCompletions.Add(1)
	attempt.Status = outcome.AttemptStatus
	attempt.Version++
	return attempt, nil
}

func (recorder *stormExecutionRecorder) CompleteAttempt(
	_ context.Context,
	request execution.GatewayRequest,
	attempt execution.RouteAttempt,
	outcome execution.AttemptOutcome,
) (execution.GatewayRequest, execution.RouteAttempt, error) {
	if err := outcome.Validate(); err != nil {
		return execution.GatewayRequest{}, execution.RouteAttempt{}, err
	}
	recorder.terminalCompletions.Add(1)
	request.Status = outcome.RequestStatus
	request.Version++
	attempt.Status = outcome.AttemptStatus
	attempt.Version++
	return request, attempt, nil
}

var (
	_ RouteSelector          = (*stormSelector)(nil)
	_ ChatExecutor           = (*stormExecutor)(nil)
	_ routedecision.Recorder = (*stormDecisionRecorder)(nil)
	_ execution.Recorder     = (*stormExecutionRecorder)(nil)
)
