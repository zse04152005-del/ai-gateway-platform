// Package proxy executes selected provider attempts without owning routing policy.
package proxy

import (
	"context"
	"errors"
	"net/http"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/provideradapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/routing"
)

var (
	// ErrInvalidExecution means trusted routing/request input violated the executor contract.
	ErrInvalidExecution = errors.New("non-stream execution input is invalid")
	// ErrAdapterUnavailable means a deployment-scoped adapter could not be constructed.
	ErrAdapterUnavailable = errors.New("provider adapter is unavailable")
	// ErrTransport means no usable upstream HTTP response was received.
	ErrTransport = errors.New("provider transport failed")
	// ErrProtocol means an upstream response could not be normalized safely.
	ErrProtocol = errors.New("provider protocol failed")
)

// AdapterRegistry constructs the exact adapter declared by a selected Provider.
type AdapterRegistry interface {
	Build(context.Context, catalog.Provider, catalog.Deployment) (provideradapter.Adapter, error)
}

// HTTPClient is implemented by the shared upstreamhttp.Client.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// NonStreamExecutor performs exactly one selected non-streaming attempt.
type NonStreamExecutor struct {
	registry AdapterRegistry
	client   HTTPClient
}

// ProviderError carries a validated, provider-neutral error without a raw body.
type ProviderError struct {
	detail adapter.NormalizedError
}

// NewProviderError accepts only a validated provider-neutral failure.
func NewProviderError(detail adapter.NormalizedError) (*ProviderError, error) {
	if err := detail.Validate(); err != nil {
		return nil, ErrProtocol
	}
	cloned := detail
	if detail.RetryAfter != nil {
		retryAfter := *detail.RetryAfter
		cloned.RetryAfter = &retryAfter
	}
	return &ProviderError{detail: cloned}, nil
}

// NewNonStreamExecutor validates process-scoped dependencies.
func NewNonStreamExecutor(registry AdapterRegistry, client HTTPClient) (*NonStreamExecutor, error) {
	if registry == nil {
		return nil, errors.New("provider adapter registry must not be nil")
	}
	if client == nil {
		return nil, errors.New("upstream HTTP client must not be nil")
	}
	return &NonStreamExecutor{registry: registry, client: client}, nil
}

// Execute builds the selected Adapter, sends one request, and returns one
// validated provider-neutral response. ParseResponse owns response body reads;
// the executor still closes the body defensively if an Adapter returns early.
func (executor *NonStreamExecutor) Execute(
	ctx context.Context,
	selection routing.Selection,
	request adapter.NormalizedRequest,
) (adapter.NormalizedResponse, error) {
	if executor == nil || executor.registry == nil || executor.client == nil || ctx == nil {
		return adapter.NormalizedResponse{}, ErrInvalidExecution
	}
	if err := ctx.Err(); err != nil {
		return adapter.NormalizedResponse{}, cancellationExecutionError(ctx, err)
	}
	if request.Stream || request.Validate() != nil || selection.Candidate.Validate() != nil ||
		selection.Candidate.LogicalModel.Name != request.LogicalModel {
		return adapter.NormalizedResponse{}, ErrInvalidExecution
	}

	built, err := executor.registry.Build(
		ctx,
		selection.Candidate.Provider,
		selection.Candidate.Deployment,
	)
	if err != nil {
		if cancellation := cancellationExecutionError(ctx, err); cancellation != nil {
			return adapter.NormalizedResponse{}, cancellation
		}
		return adapter.NormalizedResponse{}, newExecutionError(ErrAdapterUnavailable, err)
	}
	upstreamRequest, err := built.BuildRequest(ctx, request.Clone())
	if err != nil || upstreamRequest == nil {
		if cancellation := cancellationExecutionError(ctx, err); cancellation != nil {
			return adapter.NormalizedResponse{}, cancellation
		}
		return adapter.NormalizedResponse{}, newExecutionError(ErrAdapterUnavailable, err)
	}
	response, err := executor.client.Do(upstreamRequest)
	if err != nil {
		if cancellation := cancellationExecutionError(ctx, err); cancellation != nil {
			return adapter.NormalizedResponse{}, cancellation
		}
		return adapter.NormalizedResponse{}, newExecutionError(ErrTransport, err)
	}
	if response == nil || response.Body == nil {
		return adapter.NormalizedResponse{}, ErrTransport
	}
	defer func() { _ = response.Body.Close() }()

	normalized, err := built.ParseResponse(ctx, response)
	if err != nil {
		if cancellation := cancellationExecutionError(ctx, err); cancellation != nil {
			return adapter.NormalizedResponse{}, cancellation
		}
		return adapter.NormalizedResponse{}, normalizeExecutionError(err)
	}
	if err := normalized.Validate(); err != nil || normalized.Model != selection.Candidate.Deployment.PhysicalModel {
		return adapter.NormalizedResponse{}, newExecutionError(ErrProtocol, err)
	}
	return normalized, nil
}

func cancellationExecutionError(ctx context.Context, observed error) error {
	if ctx == nil || ctx.Err() == nil {
		return nil
	}
	cause := context.Cause(ctx)
	if cause == nil {
		cause = ctx.Err()
	}
	return newExecutionError(ErrTransport, errors.Join(observed, ctx.Err(), cause))
}

// Error returns only a validated provider code and never a provider body.
func (failure *ProviderError) Error() string {
	if failure == nil {
		return "<nil>"
	}
	return failure.detail.Code
}

// Detail returns a defensive copy of the normalized provider failure.
func (failure *ProviderError) Detail() adapter.NormalizedError {
	if failure == nil {
		return adapter.NormalizedError{}
	}
	detail := failure.detail
	if detail.RetryAfter != nil {
		retryAfter := *detail.RetryAfter
		detail.RetryAfter = &retryAfter
	}
	return detail
}

type executionError struct {
	kind  error
	cause error
}

func newExecutionError(kind, cause error) error {
	if cause == nil {
		return kind
	}
	return &executionError{kind: kind, cause: cause}
}

func (failure *executionError) Error() string {
	if failure == nil || failure.kind == nil {
		return "provider execution failed"
	}
	return failure.kind.Error()
}

func (failure *executionError) Unwrap() []error {
	if failure == nil {
		return nil
	}
	return []error{failure.kind, failure.cause}
}

func normalizeExecutionError(err error) error {
	var providerFailure adapter.NormalizedError
	if errors.As(err, &providerFailure) && providerFailure.Validate() == nil {
		normalized, newErr := NewProviderError(providerFailure)
		if newErr == nil {
			return normalized
		}
	}
	var protocolFailure provideradapter.ProtocolViolation
	if errors.As(err, &protocolFailure) {
		return newExecutionError(ErrProtocol, err)
	}
	return newExecutionError(ErrProtocol, err)
}

var _ error = (*ProviderError)(nil)
