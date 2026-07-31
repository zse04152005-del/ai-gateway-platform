package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/proxy"
	"github.com/zse04152005-del/ai-gateway-platform/internal/routing"
)

func TestCircuitChatExecutorTripsBeforeAnotherProviderCall(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 31, 6, 0, 0, 0, time.UTC)
	options := routing.DefaultCircuitOptions()
	options.FailureThreshold = 2
	breaker, err := routing.NewCircuitBreaker(options, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewCircuitBreaker() error = %v", err)
	}
	next := &sequenceChatExecutor{errors: []error{proxy.ErrTransport, proxy.ErrTransport}}
	executor, err := NewCircuitChatExecutor(next, breaker)
	if err != nil {
		t.Fatalf("NewCircuitChatExecutor() error = %v", err)
	}
	selection := circuitSelection(1)
	for attempt := 0; attempt < 2; attempt++ {
		if _, executeErr := executor.Execute(context.Background(), selection, adapter.NormalizedRequest{}); !errors.Is(executeErr, proxy.ErrTransport) {
			t.Fatalf("Execute(%d) error = %v", attempt, executeErr)
		}
	}
	if _, executeErr := executor.Execute(context.Background(), selection, adapter.NormalizedRequest{}); !errors.Is(executeErr, routing.ErrCircuitOpen) {
		t.Fatalf("Execute(open) error = %v", executeErr)
	}
	if next.callCount() != 2 {
		t.Fatalf("provider calls = %d, want 2", next.callCount())
	}
}

func TestCircuitChatExecutorHalfOpenPermitIsAuthoritative(t *testing.T) {
	t.Parallel()
	var clockMutex sync.Mutex
	now := time.Date(2026, 7, 31, 6, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		clockMutex.Lock()
		defer clockMutex.Unlock()
		return now
	}
	options := routing.DefaultCircuitOptions()
	options.FailureThreshold = 2
	options.HalfOpenMaximumProbes = 1
	options.HalfOpenSuccessThreshold = 1
	breaker, err := routing.NewCircuitBreaker(options, clock)
	if err != nil {
		t.Fatalf("NewCircuitBreaker() error = %v", err)
	}
	tripper := &sequenceChatExecutor{errors: []error{proxy.ErrTransport, proxy.ErrTransport}}
	trippingExecutor, _ := NewCircuitChatExecutor(tripper, breaker)
	selection := circuitSelection(2)
	for range 2 {
		_, _ = trippingExecutor.Execute(context.Background(), selection, adapter.NormalizedRequest{})
	}
	clockMutex.Lock()
	now = now.Add(options.OpenDuration)
	clockMutex.Unlock()

	blocking := &blockingChatExecutor{started: make(chan struct{}), release: make(chan struct{})}
	executor, _ := NewCircuitChatExecutor(blocking, breaker)
	done := make(chan error, 1)
	go func() {
		_, executeErr := executor.Execute(context.Background(), selection, adapter.NormalizedRequest{})
		done <- executeErr
	}()
	select {
	case <-blocking.started:
	case <-time.After(time.Second):
		t.Fatal("half-open provider attempt did not start")
	}
	if _, executeErr := executor.Execute(context.Background(), selection, adapter.NormalizedRequest{}); !errors.Is(executeErr, routing.ErrHalfOpenSaturated) {
		t.Fatalf("concurrent Execute() error = %v", executeErr)
	}
	close(blocking.release)
	if executeErr := <-done; executeErr != nil {
		t.Fatalf("half-open Execute() error = %v", executeErr)
	}
	if blocking.callCount() != 1 {
		t.Fatalf("half-open provider calls = %d", blocking.callCount())
	}
}

func TestCircuitChatExecutorIgnoresCallerAndConfigurationFailures(t *testing.T) {
	t.Parallel()
	options := routing.DefaultCircuitOptions()
	options.FailureThreshold = 2
	breaker, _ := routing.NewCircuitBreaker(options, time.Now)
	next := &sequenceChatExecutor{errors: []error{context.Canceled, proxy.ErrAdapterUnavailable, nil}}
	executor, _ := NewCircuitChatExecutor(next, breaker)
	selection := circuitSelection(3)
	for range 3 {
		_, _ = executor.Execute(context.Background(), selection, adapter.NormalizedRequest{})
	}
	snapshot, err := breaker.Snapshot(context.Background(), selection.Candidate.Deployment.ID)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if snapshot.State != routing.CircuitClosed || snapshot.TotalIgnored != 2 || snapshot.TotalFailures != 0 || snapshot.TotalSuccesses != 1 {
		t.Fatalf("ignored snapshot = %+v", snapshot)
	}
}

func TestCircuitChatExecutorPreservesResultWhenCompletionFails(t *testing.T) {
	t.Parallel()
	zeroClock := false
	breaker, err := routing.NewCircuitBreaker(routing.DefaultCircuitOptions(), func() time.Time {
		if zeroClock {
			return time.Time{}
		}
		return time.Date(2026, 7, 31, 6, 0, 0, 0, time.UTC)
	})
	if err != nil {
		t.Fatalf("NewCircuitBreaker() error = %v", err)
	}
	want := adapter.NormalizedResponse{ResponseID: "response-preserved"}
	next := &sequenceChatExecutor{responses: []adapter.NormalizedResponse{want}, after: func() { zeroClock = true }}
	executor, _ := NewCircuitChatExecutor(next, breaker)
	response, executeErr := executor.Execute(context.Background(), circuitSelection(4), adapter.NormalizedRequest{})
	if executeErr != nil || response.ResponseID != want.ResponseID || executor.CompletionFailures() != 1 {
		t.Fatalf("Execute() = %+v/%v, completion failures = %d", response, executeErr, executor.CompletionFailures())
	}
	if _, err := NewCircuitChatExecutor(nil, breaker); err == nil {
		t.Fatal("NewCircuitChatExecutor(nil next) error = nil")
	}
	if _, err := (*CircuitChatExecutor)(nil).Execute(context.Background(), circuitSelection(4), adapter.NormalizedRequest{}); err == nil {
		t.Fatal("nil CircuitChatExecutor error = nil")
	}
}

func TestCircuitOutcomeClassification(t *testing.T) {
	t.Parallel()
	retryableFailure, err := proxy.NewProviderError(adapter.NormalizedError{
		Code: "CAPACITY", Category: adapter.ErrorCapacity, Retryable: true, ProviderStatus: 503,
		SafeMessage: "Provider capacity is temporarily unavailable",
	})
	if err != nil {
		t.Fatalf("NewProviderError() error = %v", err)
	}
	nonRetryable, err := proxy.NewProviderError(adapter.NormalizedError{
		Code: "AUTH", Category: adapter.ErrorAuth, ProviderStatus: 401,
		SafeMessage: "Provider rejected gateway credentials",
	})
	if err != nil {
		t.Fatalf("NewProviderError(auth) error = %v", err)
	}
	tests := []struct {
		err  error
		want routing.CircuitOutcome
	}{
		{err: nil, want: routing.CircuitSucceeded},
		{err: context.Canceled, want: routing.CircuitIgnored},
		{err: context.DeadlineExceeded, want: routing.CircuitFailed},
		{err: proxy.ErrTransport, want: routing.CircuitFailed},
		{err: proxy.ErrProtocol, want: routing.CircuitFailed},
		{err: retryableFailure, want: routing.CircuitFailed},
		{err: nonRetryable, want: routing.CircuitIgnored},
		{err: errors.New("private local error"), want: routing.CircuitIgnored},
	}
	for index, test := range tests {
		if got := classifyCircuitOutcome(test.err); got != test.want {
			t.Errorf("classifyCircuitOutcome(case %d) = %s, want %s", index, got, test.want)
		}
	}
}

type sequenceChatExecutor struct {
	mutex     sync.Mutex
	errors    []error
	responses []adapter.NormalizedResponse
	after     func()
	calls     int
}

func (executor *sequenceChatExecutor) Execute(context.Context, routing.Selection, adapter.NormalizedRequest) (adapter.NormalizedResponse, error) {
	executor.mutex.Lock()
	index := executor.calls
	executor.calls++
	var response adapter.NormalizedResponse
	var err error
	if index < len(executor.responses) {
		response = executor.responses[index]
	}
	if index < len(executor.errors) {
		err = executor.errors[index]
	}
	after := executor.after
	executor.mutex.Unlock()
	if after != nil {
		after()
	}
	return response, err
}

func (executor *sequenceChatExecutor) callCount() int {
	executor.mutex.Lock()
	defer executor.mutex.Unlock()
	return executor.calls
}

type blockingChatExecutor struct {
	mutex   sync.Mutex
	started chan struct{}
	release chan struct{}
	calls   int
}

func (executor *blockingChatExecutor) Execute(context.Context, routing.Selection, adapter.NormalizedRequest) (adapter.NormalizedResponse, error) {
	executor.mutex.Lock()
	executor.calls++
	executor.mutex.Unlock()
	close(executor.started)
	<-executor.release
	return adapter.NormalizedResponse{}, nil
}

func (executor *blockingChatExecutor) callCount() int {
	executor.mutex.Lock()
	defer executor.mutex.Unlock()
	return executor.calls
}

func circuitSelection(value int) routing.Selection {
	return routing.Selection{Candidate: catalog.RouteCandidate{Deployment: catalog.Deployment{ID: gatewayUUID(value)}}}
}

func gatewayUUID(value int) string {
	return fmt.Sprintf("97000000-0000-4000-8000-%012d", value)
}
