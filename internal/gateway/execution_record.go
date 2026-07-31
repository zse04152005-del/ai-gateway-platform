package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/apierror"
	"github.com/zse04152005-del/ai-gateway-platform/internal/correlation"
	"github.com/zse04152005-del/ai-gateway-platform/internal/execution"
	"github.com/zse04152005-del/ai-gateway-platform/internal/keyauth"
	"github.com/zse04152005-del/ai-gateway-platform/internal/proxy"
	"github.com/zse04152005-del/ai-gateway-platform/internal/routing"
	"github.com/zse04152005-del/ai-gateway-platform/internal/upstreamhttp"
)

const terminalRecordTimeout = 2 * time.Second

func startExecutionRecord(
	request *http.Request,
	normalized normalizedChatRequest,
	recorder execution.Recorder,
) (execution.GatewayRequest, error) {
	if request == nil || recorder == nil {
		return execution.GatewayRequest{}, execution.ErrInvalid
	}
	principal, principalOK := keyauth.PrincipalFromContext(request.Context())
	fields, correlationOK := correlation.FromContext(request.Context())
	if !principalOK || !correlationOK {
		return execution.GatewayRequest{}, execution.ErrInvalid
	}
	return recorder.StartRequest(request.Context(), execution.StartRequest{
		ID: fields.RequestID, TenantID: principal.TenantID, ProjectID: principal.ProjectID,
		VirtualKeyID: principal.VirtualKeyID, LogicalModel: normalized.ProviderRequest.LogicalModel,
		TraceID: fields.TraceID, SpanID: fields.SpanID,
	})
}

func finalizeRequestFailure(
	ctx context.Context,
	recorder execution.Recorder,
	request execution.GatewayRequest,
	publicError error,
) error {
	status := execution.RequestFailed
	reason := "routing_failed"
	if errors.Is(publicError, routing.ErrNoCandidate) {
		reason = "model_unavailable"
	}
	if errors.Is(publicError, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		status = execution.RequestCancelled
		reason = "client_cancelled"
	}
	if err := failRecordedRequest(ctx, recorder, request, status, reason); err != nil {
		return executionRecordPublicError(err)
	}
	return publicError
}

func failRecordedRequest(
	ctx context.Context,
	recorder execution.Recorder,
	request execution.GatewayRequest,
	status execution.RequestStatus,
	reason string,
) error {
	recordCtx, cancel := terminalRecordContext(ctx)
	defer cancel()
	_, err := recorder.FailRequest(recordCtx, request, status, reason)
	return err
}

func completeRecordedAttempt(
	ctx context.Context,
	recorder execution.Recorder,
	request execution.GatewayRequest,
	attempt execution.RouteAttempt,
	outcome execution.AttemptOutcome,
) error {
	recordCtx, cancel := terminalRecordContext(ctx)
	defer cancel()
	_, _, err := recorder.CompleteAttempt(recordCtx, request, attempt, outcome)
	return err
}

func completeRecordedAttemptForRetry(
	ctx context.Context,
	recorder execution.Recorder,
	request execution.GatewayRequest,
	attempt execution.RouteAttempt,
	outcome execution.AttemptOutcome,
) error {
	recordCtx, cancel := terminalRecordContext(ctx)
	defer cancel()
	_, err := recorder.CompleteAttemptForRetry(recordCtx, request, attempt, outcome)
	return err
}

func terminalRecordContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), terminalRecordTimeout)
}

func attemptOutcomeForError(err error) execution.AttemptOutcome {
	if errors.Is(err, context.Canceled) {
		return cancelledAttemptOutcome()
	}
	var providerFailure *proxy.ProviderError
	if errors.As(err, &providerFailure) {
		detail := providerFailure.Detail()
		if detail.Category == adapter.ErrorCancelled {
			outcome := cancelledAttemptOutcome()
			outcome.HeadersReceived = true
			outcome.ProviderRequestID = detail.ProviderRequestID
			return outcome
		}
		status := execution.AttemptFailed
		if detail.Retryable {
			status = execution.AttemptRetryableFailed
		}
		return execution.AttemptOutcome{
			AttemptStatus: status, RequestStatus: execution.RequestFailed, HeadersReceived: true,
			EndReason: "provider_" + string(detail.Category), ProviderRequestID: detail.ProviderRequestID,
			ErrorCategory: string(detail.Category), ErrorCode: executionErrorCode(detail.Code, "PROVIDER_ERROR"),
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, upstreamhttp.ErrTimeout) {
		return failedAttemptOutcome(execution.AttemptRetryableFailed, false, "provider_timeout", string(adapter.ErrorTimeout), "PROVIDER_TIMEOUT")
	}
	switch {
	case errors.Is(err, proxy.ErrAdapterUnavailable):
		return failedAttemptOutcome(execution.AttemptFailed, false, "adapter_unavailable", "configuration", "ADAPTER_UNAVAILABLE")
	case errors.Is(err, proxy.ErrTransport):
		return failedAttemptOutcome(execution.AttemptRetryableFailed, false, "provider_transport", "transport", "PROVIDER_TRANSPORT")
	case errors.Is(err, proxy.ErrProtocol):
		return failedAttemptOutcome(execution.AttemptFailed, true, "provider_protocol", string(adapter.ErrorProtocol), "PROVIDER_PROTOCOL")
	default:
		return failedAttemptOutcome(execution.AttemptFailed, false, "gateway_execution", "gateway", "GATEWAY_EXECUTION_FAILED")
	}
}

func failedAttemptOutcome(
	status execution.AttemptStatus,
	headers bool,
	reason, category, code string,
) execution.AttemptOutcome {
	return execution.AttemptOutcome{
		AttemptStatus: status, RequestStatus: execution.RequestFailed, HeadersReceived: headers,
		EndReason: reason, ErrorCategory: category, ErrorCode: code,
	}
}

func cancelledAttemptOutcome() execution.AttemptOutcome {
	return execution.AttemptOutcome{
		AttemptStatus: execution.AttemptCancelled, RequestStatus: execution.RequestCancelled,
		EndReason: "client_cancelled", ErrorCategory: string(adapter.ErrorCancelled), ErrorCode: "CLIENT_CANCELLED",
	}
}

func executionErrorCode(value, fallback string) string {
	if len(value) >= 3 {
		return value
	}
	return fallback
}

func executionRecordPublicError(cause error) *apierror.Error {
	return apierror.MustNew(apierror.Definition{
		Status: http.StatusServiceUnavailable, Code: "EXECUTION_RECORD_UNAVAILABLE",
		Message: "Execution recording is temporarily unavailable", Type: "gateway_error",
		Retryable: true, RetryAfter: time.Second,
	}, cause)
}
