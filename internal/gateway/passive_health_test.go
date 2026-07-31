package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/proxy"
	"github.com/zse04152005-del/ai-gateway-platform/internal/routing"
	"github.com/zse04152005-del/ai-gateway-platform/internal/upstreamhttp"
)

func TestPassiveOutcomeClassifiesOnlyHealthSignals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		err        error
		want       routing.PassiveOutcome
		wantStatus int
	}{
		{name: "success", want: routing.PassiveSucceeded, wantStatus: 200},
		{
			name: "429", err: mustProviderError(t, adapter.NormalizedError{
				Code: "RATE_LIMITED", Category: adapter.ErrorRateLimit, Retryable: true,
				ProviderStatus: 429, SafeMessage: "Provider rate limited the request",
			}), want: routing.PassiveRateLimited, wantStatus: 429,
		},
		{
			name: "5xx", err: mustProviderError(t, adapter.NormalizedError{
				Code: "PROVIDER_FAILED", Category: adapter.ErrorProvider5xx, Retryable: true,
				ProviderStatus: 503, SafeMessage: "Provider temporarily failed",
			}), want: routing.PassiveServerError, wantStatus: 503,
		},
		{
			name: "provider timeout", err: mustProviderError(t, adapter.NormalizedError{
				Code: "PROVIDER_TIMEOUT", Category: adapter.ErrorTimeout, Retryable: true,
				ProviderStatus: 408, SafeMessage: "Provider request timed out",
			}), want: routing.PassiveTimedOut, wantStatus: 408,
		},
		{
			name: "provider cancellation", err: mustProviderError(t, adapter.NormalizedError{
				Code: "PROVIDER_CANCELLED", Category: adapter.ErrorCancelled,
				ProviderStatus: 499, SafeMessage: "Provider request was cancelled",
			}), want: routing.PassiveCancelled, wantStatus: 499,
		},
		{name: "context cancellation", err: context.Canceled, want: routing.PassiveCancelled},
		{name: "context timeout", err: context.DeadlineExceeded, want: routing.PassiveTimedOut},
		{name: "transport timeout", err: errors.Join(proxy.ErrTransport, upstreamhttp.ErrTimeout), want: routing.PassiveTimedOut},
		{name: "protocol", err: proxy.ErrProtocol, want: routing.PassiveOtherFailure},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			outcome, status := passiveOutcome(test.err)
			if outcome != test.want || status != test.wantStatus {
				t.Fatalf("passiveOutcome() = %q/%d, want %q/%d", outcome, status, test.want, test.wantStatus)
			}
		})
	}
}

func TestObservedChatExecutorPreservesResultAndRecordsAfterCancellation(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	clock := &gatewaySequenceClock{values: []time.Time{startedAt, startedAt.Add(125 * time.Millisecond)}}
	response := adapter.NormalizedResponse{ResponseID: "response_fixture"}
	next := &stubChatExecutor{response: response}
	observer := &stubPassiveObserver{}
	executor, err := NewObservedChatExecutor(next, observer, clock.Now)
	if err != nil {
		t.Fatalf("NewObservedChatExecutor() error = %v", err)
	}
	selection := routing.Selection{Candidate: catalog.RouteCandidate{
		Deployment: catalog.Deployment{ID: "60000000-0000-4000-8000-000000000401"},
	}}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	got, executeErr := executor.Execute(cancelled, selection, adapter.NormalizedRequest{})
	if executeErr != nil || got.ResponseID != response.ResponseID {
		t.Fatalf("Execute() = %+v/%v", got, executeErr)
	}
	if observer.calls != 1 || observer.contextErr != nil || observer.observation.DeploymentID != selection.Candidate.Deployment.ID ||
		observer.observation.Outcome != routing.PassiveSucceeded || observer.observation.ProviderStatus != 200 ||
		observer.observation.TotalLatency != 125*time.Millisecond || observer.observation.FirstTokenLatency != nil {
		t.Fatalf("observer = %+v", observer)
	}
	if executor.ObservationFailures() != 0 {
		t.Fatalf("observation failures = %d", executor.ObservationFailures())
	}
}

func TestObservedChatExecutorObservationFailureNeverChangesAttemptOutcome(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	clock := &gatewaySequenceClock{values: []time.Time{startedAt, startedAt.Add(-time.Second)}}
	executionErr := proxy.ErrProtocol
	next := &stubChatExecutor{err: executionErr}
	observer := &stubPassiveObserver{err: errors.New("local observation rejected")}
	executor, err := NewObservedChatExecutor(next, observer, clock.Now)
	if err != nil {
		t.Fatalf("NewObservedChatExecutor() error = %v", err)
	}
	selection := routing.Selection{Candidate: catalog.RouteCandidate{
		Deployment: catalog.Deployment{ID: "60000000-0000-4000-8000-000000000402"},
	}}
	_, gotErr := executor.Execute(context.Background(), selection, adapter.NormalizedRequest{})
	if !errors.Is(gotErr, executionErr) {
		t.Fatalf("Execute() error = %v, want %v", gotErr, executionErr)
	}
	if observer.observation.Outcome != routing.PassiveOtherFailure || observer.observation.TotalLatency != 0 ||
		executor.ObservationFailures() != 1 {
		t.Fatalf("observer/failures = %+v/%d", observer.observation, executor.ObservationFailures())
	}
}

func TestObservedChatExecutorFeedsPassiveHealthTracker(t *testing.T) {
	t.Parallel()
	observedAt := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	health, err := routing.NewPassiveHealth(routing.PassiveHealthOptions{
		Window: 10 * time.Second, BucketWidth: time.Second,
		MinimumSamples: 2, FailureRatioThreshold: 0.5, MaximumDeployments: 10,
	}, func() time.Time { return observedAt })
	if err != nil {
		t.Fatalf("NewPassiveHealth() error = %v", err)
	}
	providerFailure := mustProviderError(t, adapter.NormalizedError{
		Code: "RATE_LIMITED", Category: adapter.ErrorRateLimit, Retryable: true,
		ProviderStatus: 429, SafeMessage: "Provider rate limited the request",
	})
	clock := &gatewaySequenceClock{values: []time.Time{observedAt, observedAt.Add(30 * time.Millisecond)}}
	executor, err := NewObservedChatExecutor(&stubChatExecutor{err: providerFailure}, health, clock.Now)
	if err != nil {
		t.Fatalf("NewObservedChatExecutor() error = %v", err)
	}
	deploymentID := "60000000-0000-4000-8000-000000000403"
	_, gotErr := executor.Execute(context.Background(), routing.Selection{Candidate: catalog.RouteCandidate{
		Deployment: catalog.Deployment{ID: deploymentID},
	}}, adapter.NormalizedRequest{})
	if !errors.Is(gotErr, providerFailure) {
		t.Fatalf("Execute() error = %v", gotErr)
	}
	snapshot, err := health.Snapshot(context.Background(), deploymentID)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if snapshot.RequestCount != 1 || snapshot.RateLimitCount != 1 || snapshot.TotalLatency.Count != 1 ||
		snapshot.TotalLatency.Average != 30*time.Millisecond || snapshot.State != routing.PassiveStateWarmup || !snapshot.Healthy {
		t.Fatalf("passive snapshot = %+v", snapshot)
	}
}

func TestObservedChatExecutorConstructorAndNilBoundaries(t *testing.T) {
	t.Parallel()
	observer := &stubPassiveObserver{}
	next := &stubChatExecutor{}
	if _, err := NewObservedChatExecutor(nil, observer, time.Now); err == nil {
		t.Fatal("NewObservedChatExecutor(nil next) error = nil")
	}
	if _, err := NewObservedChatExecutor(next, nil, time.Now); err == nil {
		t.Fatal("NewObservedChatExecutor(nil observer) error = nil")
	}
	if _, err := NewObservedChatExecutor(next, observer, nil); err == nil {
		t.Fatal("NewObservedChatExecutor(nil clock) error = nil")
	}
	var nilExecutor *ObservedChatExecutor
	if _, err := nilExecutor.Execute(context.Background(), routing.Selection{}, adapter.NormalizedRequest{}); err == nil {
		t.Fatal("nil ObservedChatExecutor.Execute() error = nil")
	}
	if nilExecutor.ObservationFailures() != 0 {
		t.Fatal("nil ObservedChatExecutor.ObservationFailures() != 0")
	}
}

type stubPassiveObserver struct {
	observation routing.PassiveObservation
	contextErr  error
	err         error
	calls       int
}

func (observer *stubPassiveObserver) Observe(ctx context.Context, observation routing.PassiveObservation) error {
	observer.calls++
	observer.observation = observation
	observer.contextErr = ctx.Err()
	return observer.err
}

type gatewaySequenceClock struct {
	values []time.Time
	index  int
}

func (clock *gatewaySequenceClock) Now() time.Time {
	if clock.index >= len(clock.values) {
		return clock.values[len(clock.values)-1]
	}
	value := clock.values[clock.index]
	clock.index++
	return value
}
