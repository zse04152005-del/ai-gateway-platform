// Package retry makes one fail-closed, content-free retry decision for a
// completed physical provider attempt. It does not select or start attempts.
package retry

import (
	"context"
	"errors"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/proxy"
	"github.com/zse04152005-del/ai-gateway-platform/internal/streaming"
	"github.com/zse04152005-del/ai-gateway-platform/internal/upstreamhttp"
)

const (
	policyVersion        = "retry-classifier/v1"
	maximumAttempts      = 32
	maximumRetryBudget   = 24 * time.Hour
	maximumAttemptWindow = 10 * time.Minute
)

// ErrInvalid means a retry decision cannot be made safely from the supplied facts.
var ErrInvalid = errors.New("retry classification input is invalid")

// Action is the finite result consumed by the P08-T07 attempt orchestrator.
type Action string

// Supported retry actions. The orchestrator must treat unknown values as NoRetry.
const (
	NoRetry                 Action = "no_retry"
	RetryAllowed            Action = "retry_allowed"
	DifferentDeploymentOnly Action = "different_deployment_only"
)

// Reason explains the final policy result without carrying an error string.
type Reason string

// Stable final-decision reasons. They are safe to persist and use as metric labels.
const (
	ReasonModelOutputStarted      Reason = "model_output_started"
	ReasonCallerCancelled         Reason = "caller_cancelled"
	ReasonCallerDeadline          Reason = "caller_deadline_exceeded"
	ReasonAuthentication          Reason = "authentication_rejected"
	ReasonPermission              Reason = "permission_rejected"
	ReasonInvalidRequest          Reason = "invalid_request"
	ReasonContextLength           Reason = "context_length_rejected"
	ReasonContentPolicy           Reason = "content_policy_rejected"
	ReasonProviderNotRetryable    Reason = "provider_not_retryable"
	ReasonUnknownFailure          Reason = "unknown_failure"
	ReasonLocalAdapter            Reason = "local_adapter_unavailable"
	ReasonStreamTimeoutIneligible Reason = "stream_timeout_not_eligible"
	ReasonRateLimited             Reason = "provider_rate_limited"
	ReasonCapacity                Reason = "provider_capacity"
	ReasonProviderTimeout         Reason = "provider_timeout"
	ReasonProviderTemporary       Reason = "provider_temporary_failure"
	ReasonTransport               Reason = "transport_failure"
	ReasonProtocol                Reason = "provider_protocol_failure"
	ReasonFirstTokenTimeout       Reason = "first_token_timeout"
	ReasonAttemptLimit            Reason = "attempt_limit_reached"
	ReasonCostBudget              Reason = "additional_cost_not_allowed"
	ReasonDeadlineExhausted       Reason = "total_deadline_exhausted"
	ReasonRetryAfterExceeds       Reason = "retry_after_exceeds_deadline"
	ReasonAttemptWindow           Reason = "minimum_attempt_window_unavailable"
)

// FailureClass is a stable, provider-neutral class suitable for persistence.
type FailureClass string

// Stable provider-neutral failure classes.
const (
	FailureAuthentication FailureClass = "authentication"
	FailurePermission     FailureClass = "permission"
	FailureInvalidRequest FailureClass = "invalid_request"
	FailureContextLength  FailureClass = "context_length"
	FailureContentPolicy  FailureClass = "content_policy"
	FailureRateLimit      FailureClass = "rate_limit"
	FailureCapacity       FailureClass = "capacity"
	FailureTimeout        FailureClass = "timeout"
	FailureProvider5xx    FailureClass = "provider_5xx"
	FailureProtocol       FailureClass = "protocol"
	FailureTransport      FailureClass = "transport"
	FailureCancelled      FailureClass = "cancelled"
	FailureLocalAdapter   FailureClass = "local_adapter"
	FailureUnknown        FailureClass = "unknown"
)

// SubmissionState records whether the failed attempt may already be billable.
type SubmissionState string

// Supported upstream submission facts.
const (
	NotSubmitted SubmissionState = "not_submitted"
	Submitted    SubmissionState = "submitted"
	Unknown      SubmissionState = "unknown"
)

// CostPermission forces callers to make the additional-cost decision explicit.
type CostPermission string

// Supported explicit additional-cost decisions.
const (
	CostAllowed CostPermission = "allowed"
	CostDenied  CostPermission = "denied"
)

// Input contains only bounded policy facts. Failure is inspected but never
// copied to Decision, serialized, logged, or returned as an error cause.
type Input struct {
	Failure              error
	ModelOutputStarted   bool
	Submission           SubmissionState
	AttemptNumber        int
	MaximumAttempts      int
	Now                  time.Time
	Deadline             time.Time
	MinimumAttemptWindow time.Duration
	AdditionalCost       CostPermission
}

// Decision is safe to persist for later route explanation. Durations use
// rounded-up milliseconds so a sub-millisecond allowance is never overstated.
type Decision struct {
	PolicyVersion      string          `json:"policy_version"`
	Action             Action          `json:"action"`
	Reason             Reason          `json:"reason"`
	FailureClass       FailureClass    `json:"failure_class"`
	Submission         SubmissionState `json:"submission_state"`
	AttemptNumber      int             `json:"attempt_number"`
	MaximumAttempts    int             `json:"maximum_attempts"`
	RemainingBudgetMS  int64           `json:"remaining_budget_ms"`
	RequiredDelayMS    int64           `json:"required_delay_ms"`
	ModelOutputStarted bool            `json:"model_output_started"`
}

type candidate struct {
	action              Action
	reason              Reason
	failureClass        FailureClass
	retryAfter          time.Duration
	submissionSensitive bool
}

// Classify returns a deterministic decision. ErrInvalid always means callers
// must fail closed and not retry.
func Classify(input Input) (Decision, error) {
	if err := validateInput(input); err != nil {
		return Decision{}, err
	}
	candidate := classifyFailure(input.Failure)
	remaining := input.Deadline.Sub(input.Now)
	decision := Decision{
		PolicyVersion: policyVersion, Action: candidate.action, Reason: candidate.reason,
		FailureClass: candidate.failureClass, Submission: input.Submission,
		AttemptNumber: input.AttemptNumber, MaximumAttempts: input.MaximumAttempts,
		RemainingBudgetMS:  durationMillisecondsCeil(remaining),
		RequiredDelayMS:    durationMillisecondsCeil(candidate.retryAfter),
		ModelOutputStarted: input.ModelOutputStarted,
	}

	var timeoutFailure *streaming.TimeoutFailure
	if errors.As(input.Failure, &timeoutFailure) && timeoutFailure.ModelOutputStarted() {
		decision.ModelOutputStarted = true
	}
	if decision.ModelOutputStarted {
		decision.Action = NoRetry
		decision.Reason = ReasonModelOutputStarted
		return decision, nil
	}
	if candidate.action == NoRetry {
		return decision, nil
	}
	if input.AttemptNumber >= input.MaximumAttempts {
		decision.Action = NoRetry
		decision.Reason = ReasonAttemptLimit
		return decision, nil
	}
	if input.AdditionalCost != CostAllowed {
		decision.Action = NoRetry
		decision.Reason = ReasonCostBudget
		return decision, nil
	}
	if remaining <= 0 {
		decision.Action = NoRetry
		decision.Reason = ReasonDeadlineExhausted
		return decision, nil
	}
	if remaining <= input.MinimumAttemptWindow {
		decision.Action = NoRetry
		decision.Reason = ReasonAttemptWindow
		return decision, nil
	}
	if candidate.retryAfter > remaining-input.MinimumAttemptWindow {
		decision.Action = NoRetry
		decision.Reason = ReasonRetryAfterExceeds
		return decision, nil
	}
	if candidate.submissionSensitive && input.Submission != NotSubmitted {
		decision.Action = DifferentDeploymentOnly
	}
	return decision, nil
}

func validateInput(input Input) error {
	if input.Failure == nil || input.AttemptNumber < 1 || input.MaximumAttempts < 1 ||
		input.MaximumAttempts > maximumAttempts || input.AttemptNumber > input.MaximumAttempts {
		return ErrInvalid
	}
	if input.Submission != NotSubmitted && input.Submission != Submitted && input.Submission != Unknown {
		return ErrInvalid
	}
	if input.AdditionalCost != CostAllowed && input.AdditionalCost != CostDenied {
		return ErrInvalid
	}
	if input.Now.IsZero() || input.Deadline.IsZero() || input.Deadline.Sub(input.Now) > maximumRetryBudget {
		return ErrInvalid
	}
	if input.MinimumAttemptWindow <= 0 || input.MinimumAttemptWindow > maximumAttemptWindow {
		return ErrInvalid
	}
	return nil
}

func classifyFailure(failure error) candidate {
	var timeoutFailure *streaming.TimeoutFailure
	if errors.As(failure, &timeoutFailure) {
		if timeoutFailure.RetryEligibleBeforeOutput() {
			return candidate{DifferentDeploymentOnly, ReasonFirstTokenTimeout, FailureTimeout, 0, false}
		}
		return noRetry(ReasonStreamTimeoutIneligible, FailureTimeout)
	}
	if errors.Is(failure, context.Canceled) {
		return noRetry(ReasonCallerCancelled, FailureCancelled)
	}
	if errors.Is(failure, context.DeadlineExceeded) {
		return noRetry(ReasonCallerDeadline, FailureCancelled)
	}
	var providerFailure *proxy.ProviderError
	if errors.As(failure, &providerFailure) {
		return classifyNormalized(providerFailure.Detail())
	}
	var normalized adapter.NormalizedError
	if errors.As(failure, &normalized) && normalized.Validate() == nil {
		return classifyNormalized(normalized)
	}
	switch {
	case errors.Is(failure, proxy.ErrAdapterUnavailable), errors.Is(failure, upstreamhttp.ErrInvalidRequest):
		return noRetry(ReasonLocalAdapter, FailureLocalAdapter)
	case errors.Is(failure, proxy.ErrProtocol):
		return candidate{DifferentDeploymentOnly, ReasonProtocol, FailureProtocol, 0, false}
	case errors.Is(failure, proxy.ErrTransport), errors.Is(failure, upstreamhttp.ErrTransport),
		errors.Is(failure, upstreamhttp.ErrTimeout):
		return candidate{RetryAllowed, ReasonTransport, FailureTransport, 0, true}
	default:
		return noRetry(ReasonUnknownFailure, FailureUnknown)
	}
}

func classifyNormalized(failure adapter.NormalizedError) candidate {
	retryAfter := time.Duration(0)
	if failure.RetryAfter != nil {
		retryAfter = *failure.RetryAfter
	}
	switch failure.Category {
	case adapter.ErrorAuth:
		return noRetry(ReasonAuthentication, FailureAuthentication)
	case adapter.ErrorPermission:
		return noRetry(ReasonPermission, FailurePermission)
	case adapter.ErrorInvalidRequest:
		return noRetry(ReasonInvalidRequest, FailureInvalidRequest)
	case adapter.ErrorContextLength:
		return noRetry(ReasonContextLength, FailureContextLength)
	case adapter.ErrorContentPolicy:
		return noRetry(ReasonContentPolicy, FailureContentPolicy)
	case adapter.ErrorCancelled:
		return noRetry(ReasonCallerCancelled, FailureCancelled)
	case adapter.ErrorUnknown:
		return noRetry(ReasonUnknownFailure, FailureUnknown)
	case adapter.ErrorProtocol:
		return candidate{DifferentDeploymentOnly, ReasonProtocol, FailureProtocol, retryAfter, false}
	case adapter.ErrorRateLimit:
		if failure.Retryable {
			return candidate{RetryAllowed, ReasonRateLimited, FailureRateLimit, retryAfter, false}
		}
		return noRetry(ReasonProviderNotRetryable, FailureRateLimit)
	case adapter.ErrorCapacity:
		if failure.Retryable {
			return candidate{RetryAllowed, ReasonCapacity, FailureCapacity, retryAfter, false}
		}
		return noRetry(ReasonProviderNotRetryable, FailureCapacity)
	case adapter.ErrorTimeout:
		if failure.Retryable {
			return candidate{RetryAllowed, ReasonProviderTimeout, FailureTimeout, retryAfter, true}
		}
		return noRetry(ReasonProviderNotRetryable, FailureTimeout)
	case adapter.ErrorProvider5xx:
		if failure.Retryable {
			return candidate{RetryAllowed, ReasonProviderTemporary, FailureProvider5xx, retryAfter, true}
		}
		return noRetry(ReasonProviderNotRetryable, FailureProvider5xx)
	default:
		return noRetry(ReasonUnknownFailure, FailureUnknown)
	}
}

func noRetry(reason Reason, failureClass FailureClass) candidate {
	return candidate{action: NoRetry, reason: reason, failureClass: failureClass}
}

func durationMillisecondsCeil(value time.Duration) int64 {
	if value <= 0 {
		return 0
	}
	return int64((value + time.Millisecond - 1) / time.Millisecond)
}
