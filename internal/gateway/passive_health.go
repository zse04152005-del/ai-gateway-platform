package gateway

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/proxy"
	"github.com/zse04152005-del/ai-gateway-platform/internal/routing"
	"github.com/zse04152005-del/ai-gateway-platform/internal/upstreamhttp"
)

// ObservedChatExecutor records passive health without changing attempt results.
type ObservedChatExecutor struct {
	next                ChatExecutor
	observer            routing.PassiveObserver
	now                 func() time.Time
	observationFailures atomic.Uint64
}

// NewObservedChatExecutor decorates one executor with terminal health observations.
func NewObservedChatExecutor(
	next ChatExecutor,
	observer routing.PassiveObserver,
	now func() time.Time,
) (*ObservedChatExecutor, error) {
	if next == nil {
		return nil, errors.New("observed chat executor requires a next executor")
	}
	if observer == nil {
		return nil, errors.New("observed chat executor requires a passive observer")
	}
	if now == nil {
		return nil, errors.New("observed chat executor requires a clock")
	}
	return &ObservedChatExecutor{next: next, observer: observer, now: now}, nil
}

// Execute preserves the wrapped result and records one best-effort observation.
func (executor *ObservedChatExecutor) Execute(
	ctx context.Context,
	selection routing.Selection,
	request adapter.NormalizedRequest,
) (adapter.NormalizedResponse, error) {
	if executor == nil || executor.next == nil || executor.observer == nil || executor.now == nil {
		return adapter.NormalizedResponse{}, errors.New("observed chat executor is not initialized")
	}
	startedAt := executor.now()
	response, executeErr := executor.next.Execute(ctx, selection, request)
	endedAt := executor.now()
	totalLatency := endedAt.Sub(startedAt)
	if totalLatency < 0 {
		totalLatency = 0
	}
	outcome, providerStatus := passiveOutcome(executeErr)
	observation := routing.PassiveObservation{
		DeploymentID: selection.Candidate.Deployment.ID,
		Outcome:      outcome, ProviderStatus: providerStatus, TotalLatency: totalLatency,
	}
	observeContext := context.Background()
	if ctx != nil {
		observeContext = context.WithoutCancel(ctx)
	}
	if err := executor.observer.Observe(observeContext, observation); err != nil {
		executor.observationFailures.Add(1)
	}
	return response, executeErr
}

// ObservationFailures returns the number of rejected local observations.
func (executor *ObservedChatExecutor) ObservationFailures() uint64 {
	if executor == nil {
		return 0
	}
	return executor.observationFailures.Load()
}

func passiveOutcome(err error) (routing.PassiveOutcome, int) {
	if err == nil {
		return routing.PassiveSucceeded, http.StatusOK
	}
	var providerFailure *proxy.ProviderError
	if errors.As(err, &providerFailure) {
		detail := providerFailure.Detail()
		switch {
		case detail.ProviderStatus == http.StatusTooManyRequests:
			return routing.PassiveRateLimited, detail.ProviderStatus
		case detail.ProviderStatus >= http.StatusInternalServerError:
			return routing.PassiveServerError, detail.ProviderStatus
		case detail.Category == adapter.ErrorTimeout:
			return routing.PassiveTimedOut, detail.ProviderStatus
		case detail.Category == adapter.ErrorCancelled:
			return routing.PassiveCancelled, detail.ProviderStatus
		default:
			return routing.PassiveOtherFailure, detail.ProviderStatus
		}
	}
	if errors.Is(err, context.Canceled) {
		return routing.PassiveCancelled, 0
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, upstreamhttp.ErrTimeout) {
		return routing.PassiveTimedOut, 0
	}
	return routing.PassiveOtherFailure, 0
}

var _ ChatExecutor = (*ObservedChatExecutor)(nil)
