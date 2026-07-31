package gateway

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/proxy"
	"github.com/zse04152005-del/ai-gateway-platform/internal/routing"
	"github.com/zse04152005-del/ai-gateway-platform/internal/upstreamhttp"
)

// CircuitChatExecutor atomically reserves the selected deployment circuit
// immediately before a real Attempt and completes the permit exactly once.
// It must wrap ObservedChatExecutor so a rejected permit does not contaminate
// passive health or execute Provider traffic.
type CircuitChatExecutor struct {
	next               ChatExecutor
	breaker            *routing.CircuitBreaker
	completionFailures atomic.Uint64
}

// NewCircuitChatExecutor validates process-scoped dependencies.
func NewCircuitChatExecutor(next ChatExecutor, breaker *routing.CircuitBreaker) (*CircuitChatExecutor, error) {
	if next == nil || breaker == nil {
		return nil, errors.New("circuit chat executor dependencies must not be nil")
	}
	return &CircuitChatExecutor{next: next, breaker: breaker}, nil
}

// Execute enforces the authoritative half-open concurrency reservation and
// preserves the wrapped response/error even if local completion recording fails.
func (executor *CircuitChatExecutor) Execute(
	ctx context.Context,
	selection routing.Selection,
	request adapter.NormalizedRequest,
) (adapter.NormalizedResponse, error) {
	if executor == nil || executor.next == nil || executor.breaker == nil {
		return adapter.NormalizedResponse{}, errors.New("circuit chat executor is not initialized")
	}
	permit, err := executor.breaker.Acquire(ctx, selection.Candidate.Deployment.ID)
	if err != nil {
		return adapter.NormalizedResponse{}, err
	}
	response, executeErr := executor.next.Execute(ctx, selection, request)
	completionContext := context.WithoutCancel(ctx)
	if err := permit.Complete(completionContext, classifyCircuitOutcome(executeErr)); err != nil {
		executor.completionFailures.Add(1)
	}
	return response, executeErr
}

// CompletionFailures returns rejected local completion updates.
func (executor *CircuitChatExecutor) CompletionFailures() uint64 {
	if executor == nil {
		return 0
	}
	return executor.completionFailures.Load()
}

func classifyCircuitOutcome(err error) routing.CircuitOutcome {
	if err == nil {
		return routing.CircuitSucceeded
	}
	if errors.Is(err, context.Canceled) {
		return routing.CircuitIgnored
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, upstreamhttp.ErrTimeout) {
		return routing.CircuitFailed
	}
	var providerFailure *proxy.ProviderError
	if errors.As(err, &providerFailure) {
		detail := providerFailure.Detail()
		switch detail.Category {
		case adapter.ErrorRateLimit, adapter.ErrorCapacity, adapter.ErrorTimeout,
			adapter.ErrorProvider5xx, adapter.ErrorProtocol:
			return routing.CircuitFailed
		case adapter.ErrorUnknown:
			if detail.Retryable {
				return routing.CircuitFailed
			}
		case adapter.ErrorAuth, adapter.ErrorPermission, adapter.ErrorInvalidRequest,
			adapter.ErrorContentPolicy, adapter.ErrorContextLength, adapter.ErrorCancelled:
		}
		return routing.CircuitIgnored
	}
	if errors.Is(err, proxy.ErrTransport) || errors.Is(err, proxy.ErrProtocol) {
		return routing.CircuitFailed
	}
	return routing.CircuitIgnored
}

var _ ChatExecutor = (*CircuitChatExecutor)(nil)
