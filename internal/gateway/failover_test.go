package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/correlation"
	"github.com/zse04152005-del/ai-gateway-platform/internal/execution"
	"github.com/zse04152005-del/ai-gateway-platform/internal/proxy"
	"github.com/zse04152005-del/ai-gateway-platform/internal/retry"
	"github.com/zse04152005-del/ai-gateway-platform/internal/routing"
)

const (
	failoverDeploymentA = "61000000-0000-4000-8000-000000000001"
	failoverDeploymentB = "61000000-0000-4000-8000-000000000002"
	failoverDeploymentC = "61000000-0000-4000-8000-000000000003"
)

func TestNonStreamFailoverSwitchesDeploymentAndRecordsEveryAttempt(t *testing.T) {
	selector := &sequenceFailoverSelector{steps: []selectorStep{
		{selection: failoverSelection(failoverDeploymentA)},
		{selection: failoverSelection(failoverDeploymentB)},
	}}
	executor := &sequenceFailoverExecutor{steps: []executorStep{
		{err: proxy.ErrTransport},
		{response: gatewayNormalizedResponse(t)},
	}}
	recorder := &stubExecutionRecorder{}
	waits := make([]time.Duration, 0, 1)
	coordinator := testFailoverCoordinator(t, selector, executor, recorder, func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	})

	response, err := coordinator.Execute(
		context.Background(), failoverSelectionRequest(), failoverRecordedRequest(),
		failoverProviderRequest(), "req_failover_switch",
	)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if response.Gateway.AttemptCount != 2 || !reflect.DeepEqual(
		recorder.attemptDeployments, []string{failoverDeploymentA, failoverDeploymentB},
	) {
		t.Fatalf("response/attempt deployments = %+v/%v", response.Gateway, recorder.attemptDeployments)
	}
	if !reflect.DeepEqual(recorder.events, []string{
		"start_attempt", "complete_attempt_for_retry", "start_attempt", "complete_attempt",
	}) || recorder.retryCompletionCalls != 1 || recorder.completionCalls != 1 {
		t.Fatalf("recording events = %v; recorder=%+v", recorder.events, recorder)
	}
	if len(recorder.retryOutcomes) != 1 || recorder.retryOutcomes[0].RequestStatus != execution.RequestRunning ||
		recorder.retryOutcomes[0].AttemptStatus != execution.AttemptRetryableFailed ||
		len(recorder.outcomes) != 1 || recorder.outcomes[0].AttemptStatus != execution.AttemptSucceeded {
		t.Fatalf("attempt outcomes = retry=%+v terminal=%+v", recorder.retryOutcomes, recorder.outcomes)
	}
	if len(selector.requests) != 2 || selector.requests[0].ExcludedDeploymentIDs != nil ||
		!reflect.DeepEqual(selector.requests[1].ExcludedDeploymentIDs, []string{failoverDeploymentA}) ||
		!reflect.DeepEqual(waits, []time.Duration{0}) {
		t.Fatalf("selection/wait evidence = %+v/%v", selector.requests, waits)
	}
}

func TestNonStreamFailoverDoesNotRetryDeterministicProviderRejection(t *testing.T) {
	failure := mustProviderError(t, adapter.NormalizedError{
		Code: "PROVIDER_AUTH_REJECTED", Category: adapter.ErrorAuth,
		ProviderStatus: http.StatusUnauthorized, SafeMessage: "Provider authentication failed",
	})
	selector := &sequenceFailoverSelector{steps: []selectorStep{{selection: failoverSelection(failoverDeploymentA)}}}
	executor := &sequenceFailoverExecutor{steps: []executorStep{{err: failure}}}
	recorder := &stubExecutionRecorder{}
	coordinator := testFailoverCoordinator(t, selector, executor, recorder, noWait)

	_, err := coordinator.Execute(
		context.Background(), failoverSelectionRequest(), failoverRecordedRequest(),
		failoverProviderRequest(), "req_failover_auth",
	)
	if !errors.Is(err, failure) || len(selector.requests) != 1 || len(executor.selections) != 1 ||
		recorder.retryCompletionCalls != 0 || recorder.completionCalls != 1 {
		t.Fatalf("error/calls = %v selector=%d executor=%d recorder=%+v", err, len(selector.requests), len(executor.selections), recorder)
	}
}

func TestNonStreamFailoverDifferentDeploymentExhaustionReturnsOriginalFailure(t *testing.T) {
	private := errors.New("private transport detail")
	failure := errors.Join(proxy.ErrTransport, private)
	selector := &sequenceFailoverSelector{steps: []selectorStep{
		{selection: failoverSelection(failoverDeploymentA)}, {err: routing.ErrNoCandidate},
	}}
	executor := &sequenceFailoverExecutor{steps: []executorStep{{err: failure}}}
	recorder := &stubExecutionRecorder{}
	coordinator := testFailoverCoordinator(t, selector, executor, recorder, noWait)

	_, err := coordinator.Execute(
		context.Background(), failoverSelectionRequest(), failoverRecordedRequest(),
		failoverProviderRequest(), "req_failover_exhausted",
	)
	if !errors.Is(err, proxy.ErrTransport) || !errors.Is(err, private) || recorder.failureCalls != 1 ||
		recorder.failedReason != "failover_exhausted" || recorder.completionCalls != 0 ||
		recorder.retryCompletionCalls != 1 {
		t.Fatalf("exhaustion result = error=%v recorder=%+v", err, recorder)
	}
}

func TestRetryAllowedPrefersAnotherDeploymentThenMayReuseFixedTarget(t *testing.T) {
	failure := mustProviderError(t, adapter.NormalizedError{
		Code: "PROVIDER_RATE_LIMITED", Category: adapter.ErrorRateLimit, Retryable: true,
		ProviderStatus: http.StatusTooManyRequests, SafeMessage: "Provider rate limited request",
	})
	selector := &sequenceFailoverSelector{steps: []selectorStep{
		{selection: failoverSelection(failoverDeploymentA)},
		{err: routing.ErrNoCandidate},
		{selection: failoverSelection(failoverDeploymentA)},
	}}
	executor := &sequenceFailoverExecutor{steps: []executorStep{
		{err: failure}, {response: gatewayNormalizedResponse(t)},
	}}
	recorder := &stubExecutionRecorder{}
	coordinator := testFailoverCoordinator(t, selector, executor, recorder, noWait)

	response, err := coordinator.Execute(
		context.Background(), failoverSelectionRequest(), failoverRecordedRequest(),
		failoverProviderRequest(), "req_failover_rate_limit",
	)
	if err != nil || response.Gateway.AttemptCount != 2 || len(selector.requests) != 3 ||
		!reflect.DeepEqual(selector.requests[1].ExcludedDeploymentIDs, []string{failoverDeploymentA}) ||
		selector.requests[2].ExcludedDeploymentIDs != nil ||
		!reflect.DeepEqual(recorder.attemptDeployments, []string{failoverDeploymentA, failoverDeploymentA}) {
		t.Fatalf("same-target retry result = response=%+v error=%v requests=%+v attempts=%v", response.Gateway, err, selector.requests, recorder.attemptDeployments)
	}
}

func TestRetryAllowedSameTargetFailuresRemainBoundedWithoutDuplicateExclusions(t *testing.T) {
	failure := mustProviderError(t, adapter.NormalizedError{
		Code: "PROVIDER_RATE_LIMITED", Category: adapter.ErrorRateLimit, Retryable: true,
		ProviderStatus: http.StatusTooManyRequests, SafeMessage: "Provider rate limited request",
	})
	selector := &sequenceFailoverSelector{steps: []selectorStep{
		{selection: failoverSelection(failoverDeploymentA)},
		{err: routing.ErrNoCandidate},
		{selection: failoverSelection(failoverDeploymentA)},
		{err: routing.ErrNoCandidate},
		{selection: failoverSelection(failoverDeploymentA)},
	}}
	executor := &sequenceFailoverExecutor{steps: []executorStep{
		{err: failure}, {err: failure}, {err: failure},
	}}
	recorder := &stubExecutionRecorder{}
	coordinator := testFailoverCoordinator(t, selector, executor, recorder, noWait)

	_, err := coordinator.Execute(
		context.Background(), failoverSelectionRequest(), failoverRecordedRequest(),
		failoverProviderRequest(), "req_failover_same_target_bound",
	)
	wantExclusions := [][]string{
		nil, {failoverDeploymentA}, nil, {failoverDeploymentA}, nil,
	}
	if !errors.Is(err, failure) || len(selector.requests) != len(wantExclusions) ||
		len(executor.selections) != 3 || recorder.retryCompletionCalls != 2 || recorder.completionCalls != 1 {
		t.Fatalf("same-target exhaustion = error=%v selector=%d executor=%d recorder=%+v",
			err, len(selector.requests), len(executor.selections), recorder)
	}
	for index, request := range selector.requests {
		if !reflect.DeepEqual(request.ExcludedDeploymentIDs, wantExclusions[index]) {
			t.Fatalf("selector request %d exclusions = %v, want %v",
				index+1, request.ExcludedDeploymentIDs, wantExclusions[index])
		}
	}
}

func TestFailureStormIsStrictlyLinearAndAttemptBounded(t *testing.T) {
	selector := &sequenceFailoverSelector{steps: []selectorStep{
		{selection: failoverSelection(failoverDeploymentA)},
		{selection: failoverSelection(failoverDeploymentB)},
		{selection: failoverSelection(failoverDeploymentC)},
	}}
	executor := &sequenceFailoverExecutor{steps: []executorStep{
		{err: proxy.ErrTransport}, {err: proxy.ErrTransport}, {err: proxy.ErrTransport},
	}}
	recorder := &stubExecutionRecorder{}
	coordinator := testFailoverCoordinator(t, selector, executor, recorder, noWait)

	_, err := coordinator.Execute(
		context.Background(), failoverSelectionRequest(), failoverRecordedRequest(),
		failoverProviderRequest(), "req_failover_storm",
	)
	if !errors.Is(err, proxy.ErrTransport) || len(selector.requests) != 3 || len(executor.selections) != 3 ||
		recorder.attemptCalls != 3 || recorder.retryCompletionCalls != 2 || recorder.completionCalls != 1 {
		t.Fatalf("storm amplification = error=%v selector=%d executor=%d recorder=%+v", err, len(selector.requests), len(executor.selections), recorder)
	}
	for index, request := range selector.requests[1:] {
		if len(request.ExcludedDeploymentIDs) != index+1 {
			t.Fatalf("selector request %d exclusions = %v", index+2, request.ExcludedDeploymentIDs)
		}
	}
}

func TestProjectionFailurePreservesKnownAttemptUsageForBilling(t *testing.T) {
	result := gatewayNormalizedResponse(t)
	outcome := failedOutcomeWithKnownUsage(proxy.ErrProtocol, result)
	if outcome.AttemptStatus != execution.AttemptFailed || !outcome.HeadersReceived ||
		outcome.ProviderRequestID != result.ProviderRequestID || outcome.Usage == nil ||
		outcome.Usage.OutputTokens != adapter.Tokens(3) || outcome.Validate() != nil {
		t.Fatalf("known failed-attempt usage = %+v", outcome)
	}
	result.Usage.OutputTokens.Value = 999
	if outcome.Usage.OutputTokens.Value == 999 {
		t.Fatal("failed attempt outcome aliases response usage")
	}
}

func TestExecutableHandlerReturnsAttemptCountAfterTransparentFailover(t *testing.T) {
	private := "private-upstream-failure-marker"
	selector := &sequenceFailoverSelector{steps: []selectorStep{
		{selection: failoverSelection(failoverDeploymentA)},
		{selection: failoverSelection(failoverDeploymentB)},
	}}
	executor := &sequenceFailoverExecutor{steps: []executorStep{
		{err: errors.Join(proxy.ErrTransport, errors.New(private))},
		{response: gatewayNormalizedResponse(t)},
	}}
	recorder := &stubExecutionRecorder{}
	handler, err := NewExecutableHandler(
		&stubAuthenticator{principal: validGatewayPrincipal()}, &stubModelCatalog{},
		selector, executor, recorder,
	)
	if err != nil {
		t.Fatalf("NewExecutableHandler() error = %v", err)
	}
	manager, err := correlation.New(correlation.Options{})
	if err != nil {
		t.Fatalf("correlation.New() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"general-chat","messages":[{"role":"user","content":"hello"}]}`,
	))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	manager.Middleware(handler).ServeHTTP(response, request)

	var payload chatCompletionResponse
	if decodeErr := json.Unmarshal(response.Body.Bytes(), &payload); decodeErr != nil {
		t.Fatalf("decode response: %v; body=%s", decodeErr, response.Body)
	}
	if response.Code != http.StatusOK || payload.Gateway.AttemptCount != 2 ||
		strings.Contains(response.Body.String(), private) || recorder.startCalls != 1 ||
		recorder.routingCalls != 1 || recorder.retryCompletionCalls != 1 || recorder.completionCalls != 1 {
		t.Fatalf("handler failover = status=%d payload=%+v recorder=%+v body=%s", response.Code, payload.Gateway, recorder, response.Body)
	}
}

func TestFailoverOptionsConstructorAndWaiterFailClosed(t *testing.T) {
	valid := DefaultFailoverOptions()
	for _, options := range []FailoverOptions{
		{},
		{MaximumAttempts: 33, TotalTimeout: time.Second, MinimumAttemptWindow: time.Millisecond, AdditionalCost: retry.CostAllowed},
		{MaximumAttempts: 1, TotalTimeout: 0, MinimumAttemptWindow: time.Millisecond, AdditionalCost: retry.CostAllowed},
		{MaximumAttempts: 1, TotalTimeout: time.Second, MinimumAttemptWindow: time.Second, AdditionalCost: retry.CostAllowed},
		{MaximumAttempts: 1, TotalTimeout: time.Second, MinimumAttemptWindow: time.Millisecond},
	} {
		if err := options.Validate(); !errors.Is(err, ErrFailoverInvalid) {
			t.Fatalf("Validate(%+v) error = %v", options, err)
		}
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("DefaultFailoverOptions().Validate() error = %v", err)
	}
	if _, err := newNonStreamFailover(nil, nil, nil, valid, time.Now, defaultRetryWaiter); !errors.Is(err, ErrFailoverInvalid) {
		t.Fatalf("newNonStreamFailover(nil) error = %v", err)
	}
	if err := defaultRetryWaiter(nil, 0); !errors.Is(err, ErrFailoverInvalid) { //nolint:staticcheck // explicit nil boundary
		t.Fatalf("defaultRetryWaiter(nil) error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := defaultRetryWaiter(cancelled, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("defaultRetryWaiter(cancelled) error = %v", err)
	}
}

type selectorStep struct {
	selection routing.Selection
	err       error
}

type sequenceFailoverSelector struct {
	steps    []selectorStep
	requests []routing.SelectionRequest
}

func (selector *sequenceFailoverSelector) Select(
	_ context.Context,
	request routing.SelectionRequest,
) (routing.Selection, error) {
	selector.requests = append(selector.requests, cloneSelectionRequest(request))
	index := len(selector.requests) - 1
	if index >= len(selector.steps) {
		return routing.Selection{}, routing.ErrNoCandidate
	}
	return selector.steps[index].selection, selector.steps[index].err
}

type executorStep struct {
	response adapter.NormalizedResponse
	err      error
}

type sequenceFailoverExecutor struct {
	steps      []executorStep
	selections []routing.Selection
}

func (executor *sequenceFailoverExecutor) Execute(
	_ context.Context,
	selection routing.Selection,
	_ adapter.NormalizedRequest,
) (adapter.NormalizedResponse, error) {
	executor.selections = append(executor.selections, selection)
	index := len(executor.selections) - 1
	if index >= len(executor.steps) {
		return adapter.NormalizedResponse{}, errors.New("unexpected physical attempt")
	}
	return executor.steps[index].response.Clone(), executor.steps[index].err
}

func testFailoverCoordinator(
	t *testing.T,
	selector RouteSelector,
	executor ChatExecutor,
	recorder execution.Recorder,
	wait retryWaiter,
) *nonStreamFailover {
	t.Helper()
	coordinator, err := newNonStreamFailover(
		selector, executor, recorder,
		FailoverOptions{
			MaximumAttempts: 3, TotalTimeout: 5 * time.Second,
			MinimumAttemptWindow: 10 * time.Millisecond, AdditionalCost: retry.CostAllowed,
		},
		time.Now, wait,
	)
	if err != nil {
		t.Fatalf("newNonStreamFailover() error = %v", err)
	}
	return coordinator
}

func noWait(context.Context, time.Duration) error { return nil }

func failoverSelection(deploymentID string) routing.Selection {
	return routing.Selection{Candidate: catalog.RouteCandidate{
		Deployment: catalog.Deployment{ID: deploymentID},
	}}
}

func failoverSelectionRequest() routing.SelectionRequest {
	return routing.SelectionRequest{Request: failoverProviderRequest()}
}

func failoverProviderRequest() adapter.NormalizedRequest {
	return adapter.NormalizedRequest{
		RequestID: "req_failover_fixture", LogicalModel: "general-chat",
		Messages: []adapter.Message{{
			Role:  adapter.RoleUser,
			Parts: []adapter.ContentPart{{Kind: adapter.ContentText, Text: "fixture"}},
		}},
	}
}

func failoverRecordedRequest() execution.GatewayRequest {
	return execution.GatewayRequest{
		ID: "req_failover_fixture", Status: execution.RequestRouting, Version: 2,
	}
}
