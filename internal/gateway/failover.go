package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/execution"
	"github.com/zse04152005-del/ai-gateway-platform/internal/proxy"
	"github.com/zse04152005-del/ai-gateway-platform/internal/retry"
	"github.com/zse04152005-del/ai-gateway-platform/internal/routing"
)

const maximumFailoverAttempts = 32

// ErrFailoverInvalid means request-scoped orchestration cannot proceed safely.
var ErrFailoverInvalid = errors.New("chat failover input is invalid")

// FailoverOptions bounds all physical Attempts contributing to one response.
type FailoverOptions struct {
	MaximumAttempts      int
	TotalTimeout         time.Duration
	MinimumAttemptWindow time.Duration
	AdditionalCost       retry.CostPermission
}

// DefaultFailoverOptions returns the production bootstrap retry envelope.
func DefaultFailoverOptions() FailoverOptions {
	return FailoverOptions{
		MaximumAttempts: 3, TotalTimeout: 30 * time.Second,
		MinimumAttemptWindow: 250 * time.Millisecond, AdditionalCost: retry.CostAllowed,
	}
}

// Validate rejects unbounded or ambiguous retry policy.
func (options FailoverOptions) Validate() error {
	if options.MaximumAttempts < 1 || options.MaximumAttempts > maximumFailoverAttempts {
		return fmt.Errorf("%w: maximum attempts must be between 1 and 32", ErrFailoverInvalid)
	}
	if options.TotalTimeout <= 0 || options.TotalTimeout > 24*time.Hour {
		return fmt.Errorf("%w: total timeout must be positive and at most 24 hours", ErrFailoverInvalid)
	}
	if options.MinimumAttemptWindow <= 0 || options.MinimumAttemptWindow > 10*time.Minute ||
		options.MinimumAttemptWindow >= options.TotalTimeout {
		return fmt.Errorf("%w: minimum attempt window is outside the total timeout", ErrFailoverInvalid)
	}
	if options.AdditionalCost != retry.CostAllowed && options.AdditionalCost != retry.CostDenied {
		return fmt.Errorf("%w: additional cost permission must be explicit", ErrFailoverInvalid)
	}
	return nil
}

type retryWaiter func(context.Context, time.Duration) error

type nonStreamFailover struct {
	selector RouteSelector
	executor ChatExecutor
	recorder execution.Recorder
	options  FailoverOptions
	now      func() time.Time
	wait     retryWaiter
}

func newNonStreamFailover(
	selector RouteSelector,
	executor ChatExecutor,
	recorder execution.Recorder,
	options FailoverOptions,
	now func() time.Time,
	wait retryWaiter,
) (*nonStreamFailover, error) {
	if selector == nil || executor == nil || recorder == nil || now == nil || wait == nil {
		return nil, ErrFailoverInvalid
	}
	if err := options.Validate(); err != nil {
		return nil, err
	}
	if now().IsZero() {
		return nil, ErrFailoverInvalid
	}
	return &nonStreamFailover{
		selector: selector, executor: executor, recorder: recorder,
		options: options, now: now, wait: wait,
	}, nil
}

func (failover *nonStreamFailover) Execute(
	ctx context.Context,
	selectionRequest routing.SelectionRequest,
	recordedRequest execution.GatewayRequest,
	providerRequest adapter.NormalizedRequest,
	requestID string,
) (chatCompletionResponse, error) {
	if failover == nil || failover.selector == nil || failover.executor == nil || failover.recorder == nil ||
		ctx == nil || selectionRequest.Request.Validate() != nil || providerRequest.Validate() != nil ||
		providerRequest.Stream || requestID == "" || recordedRequest.Status != execution.RequestRouting {
		return chatCompletionResponse{}, ErrFailoverInvalid
	}
	executionContext, cancel := context.WithTimeout(ctx, failover.options.TotalTimeout)
	defer cancel()
	deadline, ok := executionContext.Deadline()
	if !ok {
		return chatCompletionResponse{}, ErrFailoverInvalid
	}

	selection, err := failover.selector.Select(executionContext, cloneSelectionRequest(selectionRequest))
	if err != nil {
		publicError := routeSelectionPublicError(err)
		if recordErr := finalizeRequestFailure(executionContext, failover.recorder, recordedRequest, publicError); recordErr != nil {
			return chatCompletionResponse{}, recordErr
		}
		return chatCompletionResponse{}, publicError
	}

	attemptedDeployments := make([]string, 0, failover.options.MaximumAttempts)
	for {
		updatedRequest, attempt, startErr := failover.recorder.StartAttempt(
			executionContext, recordedRequest, selection.Candidate.Deployment.ID,
		)
		if startErr != nil {
			_ = failRecordedRequest(executionContext, failover.recorder, recordedRequest, execution.RequestFailed, "attempt_record_unavailable")
			return chatCompletionResponse{}, executionRecordPublicError(startErr)
		}
		recordedRequest = updatedRequest

		result, attemptErr := failover.executor.Execute(
			executionContext, selection, providerRequest.Clone(),
		)
		var projected chatCompletionResponse
		if attemptErr == nil {
			projected, attemptErr = projectChatCompletion(
				result, providerRequest.LogicalModel, requestID, recordedRequest.AttemptCount,
			)
		}
		if attemptErr == nil {
			outcome := execution.AttemptOutcome{
				AttemptStatus: execution.AttemptSucceeded, RequestStatus: execution.RequestSucceeded,
				HeadersReceived: true, EndReason: "completed", ProviderRequestID: result.ProviderRequestID,
				Usage: result.Usage,
			}
			if recordErr := completeRecordedAttempt(executionContext, failover.recorder, recordedRequest, attempt, outcome); recordErr != nil {
				return chatCompletionResponse{}, executionRecordPublicError(recordErr)
			}
			return projected, nil
		}

		decision, decisionErr := retry.Classify(retry.Input{
			Failure: attemptErr, ModelOutputStarted: false,
			Submission: submissionState(attemptErr), AttemptNumber: recordedRequest.AttemptCount,
			MaximumAttempts: failover.options.MaximumAttempts, Now: failover.now().UTC(), Deadline: deadline,
			MinimumAttemptWindow: failover.options.MinimumAttemptWindow,
			AdditionalCost:       failover.options.AdditionalCost,
		})
		if decisionErr != nil || decision.Action == retry.NoRetry {
			outcome := failedOutcomeWithKnownUsage(attemptErr, result)
			if recordErr := completeRecordedAttempt(executionContext, failover.recorder, recordedRequest, attempt, outcome); recordErr != nil {
				return chatCompletionResponse{}, executionRecordPublicError(recordErr)
			}
			return chatCompletionResponse{}, attemptErr
		}

		retryOutcome := failedOutcomeWithKnownUsage(attemptErr, result)
		retryOutcome.AttemptStatus = execution.AttemptRetryableFailed
		retryOutcome.RequestStatus = execution.RequestRunning
		if recordErr := completeRecordedAttemptForRetry(
			executionContext, failover.recorder, recordedRequest, attempt, retryOutcome,
		); recordErr != nil {
			return chatCompletionResponse{}, executionRecordPublicError(recordErr)
		}
		if !containsDeployment(attemptedDeployments, selection.Candidate.Deployment.ID) {
			attemptedDeployments = append(attemptedDeployments, selection.Candidate.Deployment.ID)
		}

		if waitErr := failover.wait(
			executionContext, time.Duration(decision.RequiredDelayMS)*time.Millisecond,
		); waitErr != nil {
			return chatCompletionResponse{}, failRetryRequest(
				ctx, executionContext, failover.recorder, recordedRequest, waitErr,
			)
		}
		selection, err = failover.selectNext(
			executionContext, selectionRequest, attemptedDeployments, decision.Action,
		)
		if err != nil {
			if errors.Is(err, routing.ErrNoCandidate) {
				if recordErr := failRecordedRequest(
					executionContext, failover.recorder, recordedRequest,
					execution.RequestFailed, "failover_exhausted",
				); recordErr != nil {
					return chatCompletionResponse{}, executionRecordPublicError(recordErr)
				}
				return chatCompletionResponse{}, attemptErr
			}
			publicError := routeSelectionPublicError(err)
			if recordErr := failRecordedRequest(
				executionContext, failover.recorder, recordedRequest,
				execution.RequestFailed, "failover_routing_failed",
			); recordErr != nil {
				return chatCompletionResponse{}, executionRecordPublicError(recordErr)
			}
			return chatCompletionResponse{}, publicError
		}
	}
}

func (failover *nonStreamFailover) selectNext(
	ctx context.Context,
	request routing.SelectionRequest,
	attempted []string,
	action retry.Action,
) (routing.Selection, error) {
	request.ExcludedDeploymentIDs = append([]string(nil), attempted...)
	selection, err := failover.selector.Select(ctx, cloneSelectionRequest(request))
	if err == nil {
		if action == retry.DifferentDeploymentOnly && containsDeployment(attempted, selection.Candidate.Deployment.ID) {
			return routing.Selection{}, ErrFailoverInvalid
		}
		return selection, nil
	}
	if !errors.Is(err, routing.ErrNoCandidate) || action != retry.RetryAllowed {
		return routing.Selection{}, err
	}
	request.ExcludedDeploymentIDs = nil
	return failover.selector.Select(ctx, cloneSelectionRequest(request))
}

func failedOutcomeWithKnownUsage(err error, result adapter.NormalizedResponse) execution.AttemptOutcome {
	outcome := attemptOutcomeForError(err)
	if result.Validate() == nil {
		cloned := result.Clone()
		outcome.HeadersReceived = true
		outcome.ProviderRequestID = cloned.ProviderRequestID
		outcome.Usage = cloned.Usage
	}
	return outcome
}

func submissionState(err error) retry.SubmissionState {
	var providerFailure *proxy.ProviderError
	switch {
	case errors.Is(err, proxy.ErrAdapterUnavailable):
		return retry.NotSubmitted
	case errors.As(err, &providerFailure), errors.Is(err, proxy.ErrProtocol):
		return retry.Submitted
	default:
		return retry.Unknown
	}
}

func failRetryRequest(
	parent context.Context,
	executionContext context.Context,
	recorder execution.Recorder,
	request execution.GatewayRequest,
	failure error,
) error {
	status := execution.RequestFailed
	reason := "retry_deadline_exhausted"
	if parent != nil && parent.Err() != nil {
		status = execution.RequestCancelled
		reason = "client_cancelled"
	}
	if err := failRecordedRequest(executionContext, recorder, request, status, reason); err != nil {
		return executionRecordPublicError(err)
	}
	return failure
}

func defaultRetryWaiter(ctx context.Context, delay time.Duration) error {
	if ctx == nil || delay < 0 {
		return ErrFailoverInvalid
	}
	if delay == 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func cloneSelectionRequest(request routing.SelectionRequest) routing.SelectionRequest {
	request.Request = request.Request.Clone()
	request.ExcludedDeploymentIDs = append([]string(nil), request.ExcludedDeploymentIDs...)
	if request.Access.KeyAllowedModels != nil {
		models := append([]string(nil), (*request.Access.KeyAllowedModels)...)
		request.Access.KeyAllowedModels = &models
	}
	return request
}

func containsDeployment(deployments []string, target string) bool {
	for _, deploymentID := range deployments {
		if deploymentID == target {
			return true
		}
	}
	return false
}
