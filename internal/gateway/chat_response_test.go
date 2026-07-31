package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/apierror"
	"github.com/zse04152005-del/ai-gateway-platform/internal/correlation"
	"github.com/zse04152005-del/ai-gateway-platform/internal/execution"
	"github.com/zse04152005-del/ai-gateway-platform/internal/proxy"
	"github.com/zse04152005-del/ai-gateway-platform/internal/routing"
	"github.com/zse04152005-del/ai-gateway-platform/internal/upstreamhttp"
)

func TestExecutableChatHandlerReturnsUnifiedSuccess(t *testing.T) {
	executor := &stubChatExecutor{response: gatewayNormalizedResponse(t)}
	selector := &stubRouteSelector{selection: routing.Selection{}}
	executionRecorder := &stubExecutionRecorder{}
	handler, err := NewExecutableHandler(
		&stubAuthenticator{principal: validGatewayPrincipal()},
		&stubModelCatalog{},
		selector,
		executor,
		executionRecorder,
	)
	if err != nil {
		t.Fatalf("NewExecutableHandler() error = %v", err)
	}
	manager, err := correlation.New(correlation.Options{})
	if err != nil {
		t.Fatalf("correlation.New() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"general-chat","messages":[{"role":"user","content":"client prompt marker"}]}`,
	))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	manager.Middleware(handler).ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("status/content-type = %d/%q; body = %s", response.Code, response.Header().Get("Content-Type"), response.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["object"] != "chat.completion" || body["model"] != "general-chat" || body["provider_model"] != nil {
		t.Fatalf("response identity = %#v", body)
	}
	choices := body["choices"].([]any)
	choice := choices[0].(map[string]any)
	message := choice["message"].(map[string]any)
	toolCalls := message["tool_calls"].([]any)
	function := toolCalls[0].(map[string]any)["function"].(map[string]any)
	if choice["finish_reason"] != "tool_calls" || message["content"] != nil ||
		function["name"] != "get_weather" || function["arguments"] != `{"city":"Shanghai"}` {
		t.Fatalf("choice projection = %#v", choice)
	}
	usage := body["usage"].(map[string]any)
	promptDetails := usage["prompt_tokens_details"].(map[string]any)
	completionDetails := usage["completion_tokens_details"].(map[string]any)
	if usage["prompt_tokens"] != float64(13) || usage["completion_tokens"] != float64(3) ||
		usage["total_tokens"] != float64(16) || promptDetails["cached_tokens"] != float64(5) ||
		completionDetails["reasoning_tokens"] != float64(2) {
		t.Fatalf("usage projection = %#v", usage)
	}
	gateway := body["gateway"].(map[string]any)
	if gateway["request_id"] != response.Header().Get("X-Request-Id") ||
		gateway["attempt_count"] != float64(1) || gateway["usage_complete"] != true {
		t.Fatalf("gateway metadata = %#v", gateway)
	}
	if executor.calls != 1 || executor.request.LogicalModel != "general-chat" || strings.Contains(response.Body.String(), "client prompt marker") {
		t.Fatalf("executor calls/request or response leak = %d/%#v/%s", executor.calls, executor.request, response.Body)
	}
	if strings.Join(executionRecorder.events, ",") != "start_request,mark_routing,start_attempt,complete_attempt" ||
		executionRecorder.start.ID != response.Header().Get("X-Request-Id") ||
		executionRecorder.start.TenantID != gatewayTenantID || executionRecorder.start.ProjectID != gatewayProjectID ||
		executionRecorder.outcome.AttemptStatus != execution.AttemptSucceeded || executionRecorder.outcome.Usage == nil {
		t.Fatalf("execution recording = events=%v start=%+v outcome=%+v", executionRecorder.events, executionRecorder.start, executionRecorder.outcome)
	}
}

func TestExecutableChatHandlerDefersStreamingToP07(t *testing.T) {
	executor := &stubChatExecutor{response: gatewayNormalizedResponse(t)}
	handler, err := NewExecutableHandler(
		&stubAuthenticator{principal: validGatewayPrincipal()},
		&stubModelCatalog{},
		&stubRouteSelector{},
		executor,
		&stubExecutionRecorder{},
	)
	if err != nil {
		t.Fatalf("NewExecutableHandler() error = %v", err)
	}
	manager, err := correlation.New(correlation.Options{})
	if err != nil {
		t.Fatalf("correlation.New() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"general-chat","messages":[{"role":"user","content":"hello"}],"stream":true}`,
	))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	manager.Middleware(handler).ServeHTTP(response, request)
	if response.Code != http.StatusNotImplemented || executor.calls != 0 {
		t.Fatalf("status/executor calls = %d/%d; body = %s", response.Code, executor.calls, response.Body)
	}
	var envelope apierror.Envelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Error.Code != "CHAT_STREAMING_NOT_IMPLEMENTED" {
		t.Fatalf("stream envelope/error = %#v/%v", envelope, err)
	}
}

func TestExecutionPublicErrorMapsStableProviderCategories(t *testing.T) {
	retryAfter := 2 * time.Second
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name: "rate limit",
			err: mustProviderError(t, adapter.NormalizedError{
				Code: "FIXTURE_RATE_LIMIT", Category: adapter.ErrorRateLimit, Retryable: true,
				RetryAfter: &retryAfter, ProviderStatus: http.StatusTooManyRequests,
				SafeMessage: "Provider rate limited the request",
			}),
			wantStatus: http.StatusTooManyRequests, wantCode: "PROVIDER_RATE_LIMITED",
		},
		{
			name: "capacity",
			err: mustProviderError(t, adapter.NormalizedError{
				Code: "FIXTURE_CAPACITY", Category: adapter.ErrorCapacity, Retryable: true,
				ProviderStatus: http.StatusServiceUnavailable, SafeMessage: "Provider capacity is unavailable",
			}),
			wantStatus: http.StatusServiceUnavailable, wantCode: "PROVIDER_UNAVAILABLE",
		},
		{
			name: "provider credentials",
			err: mustProviderError(t, adapter.NormalizedError{
				Code: "FIXTURE_AUTH", Category: adapter.ErrorAuth,
				ProviderStatus: http.StatusUnauthorized, SafeMessage: "Provider authentication failed",
			}),
			wantStatus: http.StatusBadGateway, wantCode: "PROVIDER_CREDENTIAL_ERROR",
		},
		{name: "transport timeout", err: errors.Join(proxy.ErrTransport, upstreamhttp.ErrTimeout), wantStatus: http.StatusGatewayTimeout, wantCode: "PROVIDER_TIMEOUT"},
		{name: "protocol", err: errors.Join(proxy.ErrProtocol, errors.New("private response body marker")), wantStatus: http.StatusBadGateway, wantCode: "PROVIDER_PROTOCOL_ERROR"},
		{name: "cancelled", err: errors.Join(proxy.ErrTransport, context.Canceled), wantStatus: clientClosedRequestStatus, wantCode: "REQUEST_CANCELLED"},
		{name: "circuit open", err: routing.ErrCircuitOpen, wantStatus: http.StatusServiceUnavailable, wantCode: "MODEL_UNAVAILABLE"},
		{name: "half-open saturated", err: routing.ErrHalfOpenSaturated, wantStatus: http.StatusServiceUnavailable, wantCode: "MODEL_UNAVAILABLE"},
		{name: "circuit capacity", err: routing.ErrCircuitCapacity, wantStatus: http.StatusServiceUnavailable, wantCode: "MODEL_UNAVAILABLE"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, envelope := apierror.Render(executionPublicError(test.err), "req_public_error", "gateway_error")
			encoded, err := json.Marshal(envelope)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			if status != test.wantStatus || envelope.Error.Code != test.wantCode ||
				strings.Contains(string(encoded), "private response body marker") ||
				strings.Contains(string(encoded), "Provider authentication failed") {
				t.Fatalf("status/envelope = %d/%s", status, encoded)
			}
		})
	}
}

func TestAttemptOutcomesPreserveProviderAndCancellationSemantics(t *testing.T) {
	providerFailure := mustProviderError(t, adapter.NormalizedError{
		Code: "FIXTURE_RATE_LIMIT", Category: adapter.ErrorRateLimit, Retryable: true,
		ProviderStatus: http.StatusTooManyRequests, ProviderRequestID: "provider-request-429",
		SafeMessage: "Provider rate limited the request",
	})
	provider := attemptOutcomeForError(providerFailure)
	if provider.AttemptStatus != execution.AttemptRetryableFailed || provider.RequestStatus != execution.RequestFailed ||
		!provider.HeadersReceived || provider.ProviderRequestID != "provider-request-429" ||
		provider.ErrorCategory != string(adapter.ErrorRateLimit) || provider.ErrorCode != "FIXTURE_RATE_LIMIT" ||
		provider.Validate() != nil {
		t.Fatalf("provider attempt outcome = %+v", provider)
	}
	cancelled := attemptOutcomeForError(errors.Join(proxy.ErrTransport, context.Canceled))
	if cancelled.AttemptStatus != execution.AttemptCancelled || cancelled.RequestStatus != execution.RequestCancelled ||
		cancelled.EndReason != "client_cancelled" || cancelled.Validate() != nil {
		t.Fatalf("cancelled attempt outcome = %+v", cancelled)
	}
	protocol := attemptOutcomeForError(proxy.ErrProtocol)
	if protocol.AttemptStatus != execution.AttemptFailed || !protocol.HeadersReceived ||
		protocol.ErrorCategory != string(adapter.ErrorProtocol) || protocol.Validate() != nil {
		t.Fatalf("protocol attempt outcome = %+v", protocol)
	}
}

func TestExecutionRecordingFailuresFailClosedAroundProviderBoundary(t *testing.T) {
	privateFailure := errors.New("postgres://private-user:private-password@private-host/execution")
	tests := []struct {
		name           string
		recorder       *stubExecutionRecorder
		wantSelector   int
		wantExecutor   int
		wantFailure    int
		wantCompletion int
	}{
		{name: "start", recorder: &stubExecutionRecorder{startErr: privateFailure}},
		{name: "mark routing", recorder: &stubExecutionRecorder{routingErr: privateFailure}},
		{name: "start attempt", recorder: &stubExecutionRecorder{attemptErr: privateFailure}, wantSelector: 1, wantFailure: 1},
		{name: "complete attempt", recorder: &stubExecutionRecorder{completionErr: privateFailure}, wantSelector: 1, wantExecutor: 1, wantCompletion: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selector := &stubRouteSelector{}
			executor := &stubChatExecutor{response: gatewayNormalizedResponse(t)}
			handler, err := NewExecutableHandler(
				&stubAuthenticator{principal: validGatewayPrincipal()}, &stubModelCatalog{},
				selector, executor, test.recorder,
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
			var envelope apierror.Envelope
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode record failure: %v; body=%s", err, response.Body)
			}
			if response.Code != http.StatusServiceUnavailable || envelope.Error.Code != "EXECUTION_RECORD_UNAVAILABLE" ||
				strings.Contains(response.Body.String(), "private") || selector.calls != test.wantSelector ||
				executor.calls != test.wantExecutor || test.recorder.failureCalls != test.wantFailure ||
				test.recorder.completionCalls != test.wantCompletion {
				t.Fatalf("failure boundary = status=%d envelope=%+v selector=%d executor=%d recorder=%+v", response.Code, envelope, selector.calls, executor.calls, test.recorder)
			}
		})
	}
}

func TestClientCancellationReachesExecutorAndRecordsClientCancelled(t *testing.T) {
	executor := &cancellableChatExecutor{started: make(chan struct{}), released: make(chan struct{})}
	recorder := &stubExecutionRecorder{}
	handler, err := NewExecutableHandler(
		&stubAuthenticator{principal: validGatewayPrincipal()}, &stubModelCatalog{},
		&stubRouteSelector{}, executor, recorder,
	)
	if err != nil {
		t.Fatalf("NewExecutableHandler() error = %v", err)
	}
	manager, err := correlation.New(correlation.Options{})
	if err != nil {
		t.Fatalf("correlation.New() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"general-chat","messages":[{"role":"user","content":"hello"}]}`,
	)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		manager.Middleware(handler).ServeHTTP(response, request)
		close(done)
	}()
	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("executor did not start")
	}
	cancelledAt := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("gateway did not return after client cancellation")
	}
	if elapsed := time.Since(cancelledAt); elapsed > time.Second {
		t.Fatalf("gateway cancellation propagation took %s", elapsed)
	}
	select {
	case <-executor.released:
	default:
		t.Fatal("executor context was not cancelled")
	}
	var envelope apierror.Envelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode cancellation response: %v", err)
	}
	if response.Code != clientClosedRequestStatus || envelope.Error.Code != "REQUEST_CANCELLED" ||
		recorder.completionCalls != 1 || recorder.outcome.AttemptStatus != execution.AttemptCancelled ||
		recorder.outcome.RequestStatus != execution.RequestCancelled || recorder.outcome.EndReason != "client_cancelled" ||
		recorder.completionContextErr != nil || !recorder.completionHasDeadline {
		t.Fatalf("cancellation result = status=%d envelope=%+v recorder=%+v", response.Code, envelope, recorder)
	}
}

func TestChatProjectionPreservesFinishReasonsAndMissingUsage(t *testing.T) {
	tests := map[adapter.FinishReason]string{
		adapter.FinishStop: "stop", adapter.FinishLength: "length",
		adapter.FinishToolCalls: "tool_calls", adapter.FinishContentPolicy: "content_filter",
		adapter.FinishCancelled: "cancelled", adapter.FinishError: "error", adapter.FinishUnknown: "unknown",
	}
	for input, want := range tests {
		got, err := projectFinishReason(input)
		if err != nil || got != want {
			t.Errorf("projectFinishReason(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if usage, complete := projectChatUsage(nil); usage != nil || complete {
		t.Fatalf("projectChatUsage(nil) = %#v/%v", usage, complete)
	}
	evidence, err := adapter.NewUsageEvidence(json.RawMessage(`{"prompt_tokens":4}`))
	if err != nil {
		t.Fatalf("NewUsageEvidence() error = %v", err)
	}
	partial := &adapter.NormalizedUsage{
		InputTokens: adapter.Tokens(4), Source: adapter.UsageSourceProvider,
		Complete: false, RawEvidence: evidence,
	}
	if usage, complete := projectChatUsage(partial); usage != nil || complete {
		t.Fatalf("projectChatUsage(partial) = %#v/%v", usage, complete)
	}
}

func TestNewExecutableHandlerRejectsMissingExecutionDependencies(t *testing.T) {
	authenticator := &stubAuthenticator{}
	catalog := &stubModelCatalog{}
	selector := &stubRouteSelector{}
	executor := &stubChatExecutor{}
	recorder := &stubExecutionRecorder{}
	if _, err := NewExecutableHandler(nil, catalog, selector, executor, recorder); err == nil {
		t.Fatal("nil authenticator accepted")
	}
	if _, err := NewExecutableHandler(authenticator, nil, selector, executor, recorder); err == nil {
		t.Fatal("nil catalog accepted")
	}
	if _, err := NewExecutableHandler(authenticator, catalog, nil, executor, recorder); err == nil {
		t.Fatal("nil selector accepted")
	}
	if _, err := NewExecutableHandler(authenticator, catalog, selector, nil, recorder); err == nil {
		t.Fatal("nil executor accepted")
	}
	if _, err := NewExecutableHandler(authenticator, catalog, selector, executor, nil); err == nil {
		t.Fatal("nil recorder accepted")
	}
}

type stubChatExecutor struct {
	response  adapter.NormalizedResponse
	err       error
	selection routing.Selection
	request   adapter.NormalizedRequest
	calls     int
}

type cancellableChatExecutor struct {
	started  chan struct{}
	released chan struct{}
}

func (executor *cancellableChatExecutor) Execute(
	ctx context.Context,
	_ routing.Selection,
	_ adapter.NormalizedRequest,
) (adapter.NormalizedResponse, error) {
	close(executor.started)
	<-ctx.Done()
	close(executor.released)
	return adapter.NormalizedResponse{}, ctx.Err()
}

type stubExecutionRecorder struct {
	start                 execution.StartRequest
	failedStatus          execution.RequestStatus
	failedReason          string
	outcome               execution.AttemptOutcome
	outcomes              []execution.AttemptOutcome
	retryOutcomes         []execution.AttemptOutcome
	attemptDeployments    []string
	events                []string
	startCalls            int
	routingCalls          int
	attemptCalls          int
	streamingCalls        int
	completionCalls       int
	retryCompletionCalls  int
	failureCalls          int
	startErr              error
	routingErr            error
	attemptErr            error
	completionErr         error
	retryCompletionErr    error
	failureErr            error
	completionContextErr  error
	completionHasDeadline bool
}

func (stub *stubExecutionRecorder) StartRequest(_ context.Context, start execution.StartRequest) (execution.GatewayRequest, error) {
	stub.startCalls++
	stub.events = append(stub.events, "start_request")
	stub.start = start
	if stub.startErr != nil {
		return execution.GatewayRequest{}, stub.startErr
	}
	return execution.GatewayRequest{ID: start.ID, Status: execution.RequestAuthorized, Version: 1}, nil
}

func (stub *stubExecutionRecorder) MarkRouting(_ context.Context, request execution.GatewayRequest) (execution.GatewayRequest, error) {
	stub.routingCalls++
	stub.events = append(stub.events, "mark_routing")
	if stub.routingErr != nil {
		return execution.GatewayRequest{}, stub.routingErr
	}
	request.Status = execution.RequestRouting
	request.Version++
	return request, nil
}

func (stub *stubExecutionRecorder) FailRequest(
	_ context.Context,
	request execution.GatewayRequest,
	status execution.RequestStatus,
	reason string,
) (execution.GatewayRequest, error) {
	stub.failureCalls++
	stub.events = append(stub.events, "fail_request")
	stub.failedStatus, stub.failedReason = status, reason
	if stub.failureErr != nil {
		return execution.GatewayRequest{}, stub.failureErr
	}
	request.Status = status
	request.Version++
	return request, nil
}

func (stub *stubExecutionRecorder) StartAttempt(
	_ context.Context,
	request execution.GatewayRequest,
	deploymentID string,
) (execution.GatewayRequest, execution.RouteAttempt, error) {
	stub.attemptCalls++
	stub.events = append(stub.events, "start_attempt")
	stub.attemptDeployments = append(stub.attemptDeployments, deploymentID)
	if stub.attemptErr != nil {
		return execution.GatewayRequest{}, execution.RouteAttempt{}, stub.attemptErr
	}
	request.Status = execution.RequestRunning
	request.Version++
	request.AttemptCount++
	return request, execution.RouteAttempt{
		ID: "60000000-0000-4000-8000-000000000099", RequestID: request.ID,
		AttemptNo: request.AttemptCount, DeploymentID: deploymentID,
		Status: execution.AttemptConnecting, Version: 2,
	}, nil
}

func (stub *stubExecutionRecorder) MarkAttemptStreaming(
	_ context.Context,
	_ execution.GatewayRequest,
	attempt execution.RouteAttempt,
	providerRequestID string,
) (execution.RouteAttempt, error) {
	stub.streamingCalls++
	stub.events = append(stub.events, "mark_attempt_streaming")
	attempt.Status = execution.AttemptStreaming
	attempt.ProviderRequestID = providerRequestID
	attempt.Version += 2
	return attempt, nil
}

func (stub *stubExecutionRecorder) CompleteAttempt(
	ctx context.Context,
	request execution.GatewayRequest,
	attempt execution.RouteAttempt,
	outcome execution.AttemptOutcome,
) (execution.GatewayRequest, execution.RouteAttempt, error) {
	stub.completionCalls++
	stub.events = append(stub.events, "complete_attempt")
	stub.outcome = outcome
	stub.outcomes = append(stub.outcomes, outcome)
	stub.completionContextErr = ctx.Err()
	_, stub.completionHasDeadline = ctx.Deadline()
	if stub.completionErr != nil {
		return execution.GatewayRequest{}, execution.RouteAttempt{}, stub.completionErr
	}
	request.Status = outcome.RequestStatus
	request.Version++
	attempt.Status = outcome.AttemptStatus
	attempt.Version++
	return request, attempt, nil
}

func (stub *stubExecutionRecorder) CompleteAttemptForRetry(
	ctx context.Context,
	_ execution.GatewayRequest,
	attempt execution.RouteAttempt,
	outcome execution.AttemptOutcome,
) (execution.RouteAttempt, error) {
	stub.retryCompletionCalls++
	stub.events = append(stub.events, "complete_attempt_for_retry")
	stub.outcome = outcome
	stub.retryOutcomes = append(stub.retryOutcomes, outcome)
	stub.completionContextErr = ctx.Err()
	_, stub.completionHasDeadline = ctx.Deadline()
	if stub.retryCompletionErr != nil {
		return execution.RouteAttempt{}, stub.retryCompletionErr
	}
	attempt.Status = outcome.AttemptStatus
	attempt.Version++
	return attempt, nil
}

func (stub *stubChatExecutor) Execute(
	_ context.Context,
	selection routing.Selection,
	request adapter.NormalizedRequest,
) (adapter.NormalizedResponse, error) {
	stub.calls++
	stub.selection = selection
	stub.request = request.Clone()
	return stub.response.Clone(), stub.err
}

func gatewayNormalizedResponse(t *testing.T) adapter.NormalizedResponse {
	t.Helper()
	evidence, err := adapter.NewUsageEvidence(json.RawMessage(`{
		"prompt_tokens":13,"completion_tokens":3,"total_tokens":16,
		"prompt_tokens_details":{"cached_tokens":5},
		"completion_tokens_details":{"reasoning_tokens":2}
	}`))
	if err != nil {
		t.Fatalf("NewUsageEvidence() error = %v", err)
	}
	return adapter.NormalizedResponse{
		ResponseID: "chatcmpl_gateway_fixture", Model: "provider-model-v1",
		Choices: []adapter.NormalizedChoice{{
			Index: 0,
			Message: adapter.Message{
				Role: adapter.RoleAssistant,
				ToolCalls: []adapter.ToolCall{{
					ID: "call_weather", Name: "get_weather",
					Arguments: json.RawMessage(`{"city":"Shanghai"}`),
				}},
			},
			FinishReason: adapter.FinishToolCalls, ProviderFinishReason: "tool_calls",
		}},
		Usage: &adapter.NormalizedUsage{
			InputTokens: adapter.Tokens(13), OutputTokens: adapter.Tokens(3),
			CacheReadTokens: adapter.Tokens(5), ReasoningTokens: adapter.Tokens(2),
			Source: adapter.UsageSourceProvider, Complete: true, RawEvidence: evidence,
		},
		ProviderRequestID: "provider_request_fixture",
		ObservedAt:        time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC),
	}
}

func mustProviderError(t *testing.T, detail adapter.NormalizedError) error {
	t.Helper()
	failure, err := proxy.NewProviderError(detail)
	if err != nil {
		t.Fatalf("proxy.NewProviderError() error = %v", err)
	}
	return failure
}
