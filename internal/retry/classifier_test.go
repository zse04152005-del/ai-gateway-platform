package retry_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/proxy"
	"github.com/zse04152005-del/ai-gateway-platform/internal/retry"
	"github.com/zse04152005-del/ai-gateway-platform/internal/streaming"
	"github.com/zse04152005-del/ai-gateway-platform/internal/upstreamhttp"
)

var testNow = time.Date(2026, time.July, 31, 6, 0, 0, 0, time.UTC)

func TestProviderClassificationMatrix(t *testing.T) {
	t.Parallel()
	retryAfter := time.Second
	tests := []struct {
		name       string
		category   adapter.ErrorCategory
		retryable  bool
		submission retry.SubmissionState
		wantAction retry.Action
		wantReason retry.Reason
		wantClass  retry.FailureClass
	}{
		{"authentication", adapter.ErrorAuth, false, retry.Submitted, retry.NoRetry, retry.ReasonAuthentication, retry.FailureAuthentication},
		{"permission", adapter.ErrorPermission, false, retry.Submitted, retry.NoRetry, retry.ReasonPermission, retry.FailurePermission},
		{"invalid request", adapter.ErrorInvalidRequest, false, retry.Submitted, retry.NoRetry, retry.ReasonInvalidRequest, retry.FailureInvalidRequest},
		{"context length", adapter.ErrorContextLength, false, retry.Submitted, retry.NoRetry, retry.ReasonContextLength, retry.FailureContextLength},
		{"content policy", adapter.ErrorContentPolicy, false, retry.Submitted, retry.NoRetry, retry.ReasonContentPolicy, retry.FailureContentPolicy},
		{"cancelled", adapter.ErrorCancelled, false, retry.Submitted, retry.NoRetry, retry.ReasonCallerCancelled, retry.FailureCancelled},
		{"unknown", adapter.ErrorUnknown, false, retry.Unknown, retry.NoRetry, retry.ReasonUnknownFailure, retry.FailureUnknown},
		{"rate limited", adapter.ErrorRateLimit, true, retry.Submitted, retry.RetryAllowed, retry.ReasonRateLimited, retry.FailureRateLimit},
		{"capacity", adapter.ErrorCapacity, true, retry.Unknown, retry.RetryAllowed, retry.ReasonCapacity, retry.FailureCapacity},
		{"timeout before submit", adapter.ErrorTimeout, true, retry.NotSubmitted, retry.RetryAllowed, retry.ReasonProviderTimeout, retry.FailureTimeout},
		{"timeout after submit", adapter.ErrorTimeout, true, retry.Submitted, retry.DifferentDeploymentOnly, retry.ReasonProviderTimeout, retry.FailureTimeout},
		{"temporary server before submit", adapter.ErrorProvider5xx, true, retry.NotSubmitted, retry.RetryAllowed, retry.ReasonProviderTemporary, retry.FailureProvider5xx},
		{"temporary server unknown submit", adapter.ErrorProvider5xx, true, retry.Unknown, retry.DifferentDeploymentOnly, retry.ReasonProviderTemporary, retry.FailureProvider5xx},
		{"protocol", adapter.ErrorProtocol, false, retry.Submitted, retry.DifferentDeploymentOnly, retry.ReasonProtocol, retry.FailureProtocol},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var hint *time.Duration
			if test.retryable {
				hint = &retryAfter
			}
			failure := newProviderFailure(t, test.category, test.retryable, hint)
			input := validInput(failure)
			input.Submission = test.submission
			decision, err := retry.Classify(input)
			if err != nil {
				t.Fatalf("Classify() error = %v", err)
			}
			if decision.Action != test.wantAction || decision.Reason != test.wantReason ||
				decision.FailureClass != test.wantClass {
				t.Fatalf("decision = %#v", decision)
			}
			if decision.PolicyVersion != "retry-classifier/v1" || decision.AttemptNumber != 1 ||
				decision.MaximumAttempts != 3 || decision.RemainingBudgetMS != 5000 {
				t.Fatalf("decision evidence = %#v", decision)
			}
			if test.retryable && decision.RequiredDelayMS != 1000 {
				t.Fatalf("required delay = %d, want 1000", decision.RequiredDelayMS)
			}
		})
	}
}

func TestNonRetryableProviderCandidatesStayClosed(t *testing.T) {
	t.Parallel()
	for _, category := range []adapter.ErrorCategory{
		adapter.ErrorRateLimit, adapter.ErrorCapacity, adapter.ErrorTimeout, adapter.ErrorProvider5xx,
	} {
		failure := newProviderFailure(t, category, false, nil)
		decision, err := retry.Classify(validInput(failure))
		if err != nil || decision.Action != retry.NoRetry || decision.Reason != retry.ReasonProviderNotRetryable {
			t.Fatalf("category %q decision = %#v, error = %v", category, decision, err)
		}
	}
}

func TestBudgetAndIrreversibleBoundaryPrecedence(t *testing.T) {
	t.Parallel()
	retryAfter := 4901 * time.Millisecond
	failure := newProviderFailure(t, adapter.ErrorRateLimit, true, &retryAfter)
	tests := []struct {
		name   string
		mutate func(*retry.Input)
		reason retry.Reason
	}{
		{"output wins over every budget", func(input *retry.Input) { input.ModelOutputStarted = true; input.AttemptNumber = input.MaximumAttempts }, retry.ReasonModelOutputStarted},
		{"attempt limit", func(input *retry.Input) { input.AttemptNumber = input.MaximumAttempts }, retry.ReasonAttemptLimit},
		{"cost denied", func(input *retry.Input) { input.AdditionalCost = retry.CostDenied }, retry.ReasonCostBudget},
		{"deadline exhausted", func(input *retry.Input) { input.Deadline = input.Now }, retry.ReasonDeadlineExhausted},
		{"deadline already elapsed", func(input *retry.Input) { input.Deadline = input.Now.Add(-time.Second) }, retry.ReasonDeadlineExhausted},
		{"minimum window", func(input *retry.Input) { input.Deadline = input.Now.Add(input.MinimumAttemptWindow) }, retry.ReasonAttemptWindow},
		{"retry after exceeds", func(_ *retry.Input) {}, retry.ReasonRetryAfterExceeds},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			input := validInput(failure)
			test.mutate(&input)
			decision, err := retry.Classify(input)
			if err != nil || decision.Action != retry.NoRetry || decision.Reason != test.reason {
				t.Fatalf("decision = %#v, error = %v", decision, err)
			}
		})
	}
}

func TestRetryAfterExactBoundaryAndMillisecondCeiling(t *testing.T) {
	t.Parallel()
	retryAfter := 4900 * time.Millisecond
	failure := newProviderFailure(t, adapter.ErrorCapacity, true, &retryAfter)
	input := validInput(failure)
	input.Deadline = input.Now.Add(5*time.Second + time.Nanosecond)
	decision, err := retry.Classify(input)
	if err != nil || decision.Action != retry.RetryAllowed {
		t.Fatalf("decision = %#v, error = %v", decision, err)
	}
	if decision.RemainingBudgetMS != 5001 || decision.RequiredDelayMS != 4900 {
		t.Fatalf("rounded decision = %#v", decision)
	}
}

func TestInfrastructureAndCallerFailureMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		failure    error
		submission retry.SubmissionState
		action     retry.Action
		reason     retry.Reason
	}{
		{"caller cancellation", errors.Join(proxy.ErrTransport, context.Canceled), retry.Unknown, retry.NoRetry, retry.ReasonCallerCancelled},
		{"caller deadline", errors.Join(proxy.ErrTransport, context.DeadlineExceeded), retry.Unknown, retry.NoRetry, retry.ReasonCallerDeadline},
		{"adapter unavailable", proxy.ErrAdapterUnavailable, retry.NotSubmitted, retry.NoRetry, retry.ReasonLocalAdapter},
		{"unsafe upstream request", upstreamhttp.ErrInvalidRequest, retry.NotSubmitted, retry.NoRetry, retry.ReasonLocalAdapter},
		{"protocol", proxy.ErrProtocol, retry.Submitted, retry.DifferentDeploymentOnly, retry.ReasonProtocol},
		{"transport before submit", proxy.ErrTransport, retry.NotSubmitted, retry.RetryAllowed, retry.ReasonTransport},
		{"transport after submit", proxy.ErrTransport, retry.Submitted, retry.DifferentDeploymentOnly, retry.ReasonTransport},
		{"transport submission unknown", upstreamhttp.ErrTimeout, retry.Unknown, retry.DifferentDeploymentOnly, retry.ReasonTransport},
		{"untrusted failure", errors.New("private provider body"), retry.Unknown, retry.NoRetry, retry.ReasonUnknownFailure},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			input := validInput(test.failure)
			input.Submission = test.submission
			decision, err := retry.Classify(input)
			if err != nil || decision.Action != test.action || decision.Reason != test.reason {
				t.Fatalf("decision = %#v, error = %v", decision, err)
			}
		})
	}
}

func TestFirstTokenAndPostOutputTimeouts(t *testing.T) {
	firstToken := timeoutFailure(t, false)
	input := validInput(firstToken)
	input.Submission = retry.Submitted
	decision, err := retry.Classify(input)
	if err != nil || decision.Action != retry.DifferentDeploymentOnly ||
		decision.Reason != retry.ReasonFirstTokenTimeout {
		t.Fatalf("first-token decision = %#v, error = %v", decision, err)
	}

	postOutput := timeoutFailure(t, true)
	input = validInput(postOutput)
	decision, err = retry.Classify(input)
	if err != nil || decision.Action != retry.NoRetry || decision.Reason != retry.ReasonModelOutputStarted ||
		!decision.ModelOutputStarted {
		t.Fatalf("post-output decision = %#v, error = %v", decision, err)
	}
}

func TestInvalidInputFailsClosed(t *testing.T) {
	t.Parallel()
	valid := validInput(proxy.ErrTransport)
	tests := []struct {
		name   string
		mutate func(*retry.Input)
	}{
		{"missing failure", func(input *retry.Input) { input.Failure = nil }},
		{"zero attempt", func(input *retry.Input) { input.AttemptNumber = 0 }},
		{"attempt beyond maximum", func(input *retry.Input) { input.AttemptNumber = 4 }},
		{"unbounded maximum", func(input *retry.Input) { input.MaximumAttempts = 33 }},
		{"unknown submission", func(input *retry.Input) { input.Submission = "maybe" }},
		{"unknown cost", func(input *retry.Input) { input.AdditionalCost = "maybe" }},
		{"zero now", func(input *retry.Input) { input.Now = time.Time{} }},
		{"zero deadline", func(input *retry.Input) { input.Deadline = time.Time{} }},
		{"unbounded deadline", func(input *retry.Input) { input.Deadline = input.Now.Add(24*time.Hour + time.Nanosecond) }},
		{"zero attempt window", func(input *retry.Input) { input.MinimumAttemptWindow = 0 }},
		{"unbounded attempt window", func(input *retry.Input) { input.MinimumAttemptWindow = 10*time.Minute + time.Nanosecond }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			input := valid
			test.mutate(&input)
			decision, err := retry.Classify(input)
			if !errors.Is(err, retry.ErrInvalid) || decision != (retry.Decision{}) {
				t.Fatalf("decision = %#v, error = %v", decision, err)
			}
		})
	}
}

func TestDecisionSerializationContainsOnlyFiniteEvidence(t *testing.T) {
	t.Parallel()
	private := "private-provider-body-with-secret"
	decision, err := retry.Classify(validInput(errors.New(private)))
	if err != nil {
		t.Fatalf("Classify() error = %v", err)
	}
	payload, err := json.Marshal(decision)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	serialized := string(payload)
	if strings.Contains(serialized, private) || strings.Contains(serialized, "error") ||
		!strings.Contains(serialized, `"policy_version":"retry-classifier/v1"`) {
		t.Fatalf("unsafe or incomplete decision JSON: %s", serialized)
	}
}

func TestDecisionValidationBoundaries(t *testing.T) {
	t.Parallel()
	valid := retry.Decision{
		PolicyVersion: "retry-classifier/v1", Action: retry.NoRetry,
		Reason: retry.ReasonAuthentication, FailureClass: retry.FailureAuthentication,
		Submission: retry.Submitted, AttemptNumber: 1, MaximumAttempts: 3,
		RemainingBudgetMS: 1000,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Decision.Validate(valid) error = %v", err)
	}

	invalid := []retry.Decision{
		{},
		{
			PolicyVersion: "retry-classifier/v1", Action: "private_action",
			Reason: retry.ReasonAuthentication, FailureClass: retry.FailureAuthentication,
			Submission: retry.Submitted, AttemptNumber: 1, MaximumAttempts: 3,
		},
		{
			PolicyVersion: "retry-classifier/v1", Action: retry.NoRetry,
			Reason: "private_reason", FailureClass: retry.FailureAuthentication,
			Submission: retry.Submitted, AttemptNumber: 1, MaximumAttempts: 3,
		},
		{
			PolicyVersion: "retry-classifier/v1", Action: retry.NoRetry,
			Reason: retry.ReasonAuthentication, FailureClass: "private_class",
			Submission: retry.Submitted, AttemptNumber: 1, MaximumAttempts: 3,
		},
		{
			PolicyVersion: "retry-classifier/v1", Action: retry.RetryAllowed,
			Reason: retry.ReasonModelOutputStarted, FailureClass: retry.FailureTimeout,
			Submission: retry.Submitted, AttemptNumber: 1, MaximumAttempts: 3,
			ModelOutputStarted: true,
		},
		{
			PolicyVersion: "retry-classifier/v1", Action: retry.NoRetry,
			Reason: retry.ReasonAttemptLimit, FailureClass: retry.FailureCapacity,
			Submission: retry.Submitted, AttemptNumber: 4, MaximumAttempts: 3,
		},
		{
			PolicyVersion: "retry-classifier/v1", Action: retry.NoRetry,
			Reason: retry.ReasonDeadlineExhausted, FailureClass: retry.FailureTimeout,
			Submission: retry.Submitted, AttemptNumber: 1, MaximumAttempts: 3,
			RemainingBudgetMS: -1,
		},
	}
	for index, decision := range invalid {
		if err := decision.Validate(); !errors.Is(err, retry.ErrInvalid) {
			t.Fatalf("Decision.Validate(invalid[%d]) error = %v, want ErrInvalid", index, err)
		}
	}
}

func TestClassifyIsDeterministicAndConcurrent(t *testing.T) {
	t.Parallel()
	failure := newProviderFailure(t, adapter.ErrorProvider5xx, true, nil)
	input := validInput(failure)
	want, err := retry.Classify(input)
	if err != nil {
		t.Fatalf("Classify() error = %v", err)
	}
	var wait sync.WaitGroup
	errorsFound := make(chan error, 64)
	for range 64 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range 1000 {
				got, classifyErr := retry.Classify(input)
				if classifyErr != nil || got != want {
					errorsFound <- errors.New("classification changed")
					return
				}
			}
		}()
	}
	wait.Wait()
	close(errorsFound)
	if err := <-errorsFound; err != nil {
		t.Fatal(err)
	}
}

func validInput(failure error) retry.Input {
	return retry.Input{
		Failure: failure, Submission: retry.Unknown, AttemptNumber: 1, MaximumAttempts: 3,
		Now: testNow, Deadline: testNow.Add(5 * time.Second),
		MinimumAttemptWindow: 100 * time.Millisecond, AdditionalCost: retry.CostAllowed,
	}
}

func newProviderFailure(
	t *testing.T,
	category adapter.ErrorCategory,
	retryable bool,
	retryAfter *time.Duration,
) error {
	t.Helper()
	status := 400
	switch category {
	case adapter.ErrorRateLimit:
		status = 429
	case adapter.ErrorCapacity, adapter.ErrorProvider5xx:
		status = 503
	case adapter.ErrorTimeout:
		status = 504
	case adapter.ErrorProtocol:
		status = 502
	case adapter.ErrorAuth, adapter.ErrorPermission, adapter.ErrorInvalidRequest,
		adapter.ErrorContentPolicy, adapter.ErrorContextLength, adapter.ErrorCancelled,
		adapter.ErrorUnknown:
	}
	detail := adapter.NormalizedError{
		Code: "SAFE_PROVIDER_FAILURE", Category: category, Retryable: retryable,
		RetryAfter: retryAfter, ProviderStatus: status, SafeMessage: "Provider request failed",
	}
	failure, err := proxy.NewProviderError(detail)
	if err != nil {
		t.Fatalf("NewProviderError() error = %v", err)
	}
	return failure
}

func timeoutFailure(t *testing.T, afterModelOutput bool) error {
	t.Helper()
	options := streaming.TimeoutOptions{
		FirstTokenTimeout: 10 * time.Millisecond, NoProgressTimeout: 10 * time.Millisecond,
		TotalTimeout: time.Second,
	}
	if afterModelOutput {
		options.FirstTokenTimeout = time.Second
	}
	controller, err := streaming.NewTimeoutController(context.Background(), options)
	if err != nil {
		t.Fatalf("NewTimeoutController() error = %v", err)
	}
	defer func() { _ = controller.Close() }()
	var upstream interface {
		Next(context.Context) (adapter.NormalizedChunk, error)
		Close() error
	} = &idleStream{}
	if afterModelOutput {
		upstream = &oneChunkStream{}
	}
	guarded, err := controller.Attach(upstream)
	if err != nil {
		t.Fatalf("Attach() error = %v", err)
	}
	if afterModelOutput {
		if _, nextErr := guarded.Next(context.Background()); nextErr != nil {
			t.Fatalf("Next() error = %v", nextErr)
		}
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if failure := controller.Failure(); failure != nil {
			return failure
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timeout failure was not produced")
	return nil
}

type idleStream struct{}

func (*idleStream) Next(context.Context) (adapter.NormalizedChunk, error) {
	return adapter.NormalizedChunk{}, errors.New("unused")
}

func (*idleStream) Close() error { return nil }

type oneChunkStream struct {
	once sync.Once
}

func (stream *oneChunkStream) Next(context.Context) (adapter.NormalizedChunk, error) {
	var chunk adapter.NormalizedChunk
	stream.once.Do(func() {
		chunk = adapter.NormalizedChunk{
			Sequence: 1, Kind: adapter.ChunkContentDelta, ChoiceIndex: 0,
			ContentDelta: "safe", ObservedAt: time.Now().UTC(),
		}
	})
	if chunk.Kind == "" {
		return adapter.NormalizedChunk{}, errors.New("unused")
	}
	return chunk, nil
}

func (*oneChunkStream) Close() error { return nil }
