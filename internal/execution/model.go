// Package execution records client requests and physical upstream attempts.
package execution

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
)

const (
	// RequestAuthorized means trusted virtual-key authentication succeeded.
	RequestAuthorized RequestStatus = "authorized"
	// RequestRouting means the gateway is choosing a deployment.
	RequestRouting RequestStatus = "routing"
	// RequestRunning means at least one physical attempt has started.
	RequestRunning RequestStatus = "running"
	// RequestSucceeded means a complete response is ready for the client.
	RequestSucceeded RequestStatus = "succeeded"
	// RequestPartialFailed means client-visible output preceded a failure.
	RequestPartialFailed RequestStatus = "partial_failed"
	// RequestFailed means no valid model output was delivered.
	RequestFailed RequestStatus = "failed"
	// RequestCancelled means client or platform cancellation ended execution.
	RequestCancelled RequestStatus = "cancelled"

	// AttemptCreated is the immutable attempt identity before dialing.
	AttemptCreated AttemptStatus = "created"
	// AttemptConnecting means adapter construction or provider I/O is active.
	AttemptConnecting AttemptStatus = "connecting"
	// AttemptHeadersReceived means a provider HTTP response exists.
	AttemptHeadersReceived AttemptStatus = "headers_received"
	// AttemptStreaming means client-visible stream output has started.
	AttemptStreaming AttemptStatus = "streaming"
	// AttemptSucceeded means the attempt produced a valid complete response.
	AttemptSucceeded AttemptStatus = "succeeded"
	// AttemptRetryableFailed means policy may safely consider another attempt.
	AttemptRetryableFailed AttemptStatus = "retryable_failed"
	// AttemptFailed means the attempt failed without client-visible output.
	AttemptFailed AttemptStatus = "failed"
	// AttemptPartialFailed means output was visible before terminal failure.
	AttemptPartialFailed AttemptStatus = "partial_failed"
	// AttemptCancelled means cancellation ended the physical call.
	AttemptCancelled AttemptStatus = "cancelled"
)

var (
	// ErrInvalid means trusted execution facts violate the recording contract.
	ErrInvalid = errors.New("execution record input is invalid")
	// ErrConflict means a stale version, duplicate identity, or illegal transition lost a compare-and-swap.
	ErrConflict = errors.New("execution record transition conflicts with current state")
	// ErrUnavailable means PostgreSQL could not durably record execution facts.
	ErrUnavailable = errors.New("execution record store is unavailable")

	requestIDPattern         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`)
	uuidPattern              = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	modelPattern             = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
	traceIDPattern           = regexp.MustCompile(`^[0-9a-f]{32}$`)
	spanIDPattern            = regexp.MustCompile(`^[0-9a-f]{16}$`)
	reasonPattern            = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	errorCodePattern         = regexp.MustCompile(`^[A-Z][A-Z0-9_]{2,127}$`)
	categoryPattern          = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	providerRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)
)

// RequestStatus is the finite client-execution state.
type RequestStatus string

// AttemptStatus is the finite physical-call state.
type AttemptStatus string

// GatewayRequest is the durable current request state and optimistic version.
type GatewayRequest struct {
	ID           string
	TenantID     string
	ProjectID    string
	VirtualKeyID string
	LogicalModel string
	TraceID      string
	SpanID       string
	Status       RequestStatus
	AttemptCount int
	StartedAt    time.Time
	EndedAt      *time.Time
	EndReason    string
	Version      int64
	UpdatedAt    time.Time
}

// RouteAttempt is one durable physical deployment call.
type RouteAttempt struct {
	ID                string
	RequestID         string
	AttemptNo         int
	DeploymentID      string
	Status            AttemptStatus
	StartedAt         time.Time
	HeadersReceivedAt *time.Time
	FirstByteAt       *time.Time
	EndedAt           *time.Time
	EndReason         string
	ProviderRequestID string
	ErrorCategory     string
	ErrorCode         string
	UsageSummary      json.RawMessage
	Version           int64
	UpdatedAt         time.Time
}

// StartRequest contains trusted identity and correlation facts only.
type StartRequest struct {
	ID           string
	TenantID     string
	ProjectID    string
	VirtualKeyID string
	LogicalModel string
	TraceID      string
	SpanID       string
}

// AttemptOutcome atomically terminates one Attempt and its parent Request.
type AttemptOutcome struct {
	AttemptStatus     AttemptStatus
	RequestStatus     RequestStatus
	HeadersReceived   bool
	EndReason         string
	ProviderRequestID string
	ErrorCategory     string
	ErrorCode         string
	Usage             *adapter.NormalizedUsage
}

// Validate checks immutable trusted start facts without content.
func (start StartRequest) Validate() error {
	if !requestIDPattern.MatchString(start.ID) || !uuidPattern.MatchString(start.TenantID) ||
		!uuidPattern.MatchString(start.ProjectID) || !uuidPattern.MatchString(start.VirtualKeyID) ||
		!modelPattern.MatchString(start.LogicalModel) || !traceIDPattern.MatchString(start.TraceID) ||
		!spanIDPattern.MatchString(start.SpanID) {
		return ErrInvalid
	}
	return nil
}

// Validate checks a terminal attempt/request outcome.
func (outcome AttemptOutcome) Validate() error {
	if !reasonPattern.MatchString(outcome.EndReason) {
		return ErrInvalid
	}
	if outcome.ProviderRequestID != "" &&
		(!outcome.HeadersReceived || !providerRequestIDPattern.MatchString(outcome.ProviderRequestID)) {
		return ErrInvalid
	}
	switch outcome.AttemptStatus {
	case AttemptSucceeded:
		if outcome.RequestStatus != RequestSucceeded || !outcome.HeadersReceived ||
			outcome.ErrorCategory != "" || outcome.ErrorCode != "" {
			return ErrInvalid
		}
	case AttemptRetryableFailed, AttemptFailed:
		validRequestStatus := outcome.RequestStatus == RequestFailed ||
			(outcome.AttemptStatus == AttemptRetryableFailed && outcome.RequestStatus == RequestRunning)
		if !validRequestStatus || !categoryPattern.MatchString(outcome.ErrorCategory) ||
			!errorCodePattern.MatchString(outcome.ErrorCode) {
			return ErrInvalid
		}
	case AttemptPartialFailed:
		if outcome.RequestStatus != RequestPartialFailed || !outcome.HeadersReceived ||
			!categoryPattern.MatchString(outcome.ErrorCategory) || !errorCodePattern.MatchString(outcome.ErrorCode) {
			return ErrInvalid
		}
	case AttemptCancelled:
		if outcome.RequestStatus != RequestCancelled || outcome.ErrorCategory != string(adapter.ErrorCancelled) ||
			!errorCodePattern.MatchString(outcome.ErrorCode) {
			return ErrInvalid
		}
	case AttemptCreated, AttemptConnecting, AttemptHeadersReceived, AttemptStreaming:
		return ErrInvalid
	default:
		return ErrInvalid
	}
	if outcome.Usage != nil {
		if err := outcome.Usage.Validate(); err != nil {
			return fmt.Errorf("%w: usage", ErrInvalid)
		}
	}
	return nil
}

func validateRequestHandle(request GatewayRequest, allowed ...RequestStatus) error {
	if !requestIDPattern.MatchString(request.ID) || request.Version < 1 || request.AttemptCount < 0 {
		return ErrInvalid
	}
	for _, status := range allowed {
		if request.Status == status {
			return nil
		}
	}
	return ErrInvalid
}

func validateAttemptHandle(attempt RouteAttempt, status AttemptStatus) error {
	if !uuidPattern.MatchString(attempt.ID) || !requestIDPattern.MatchString(attempt.RequestID) ||
		!uuidPattern.MatchString(attempt.DeploymentID) || attempt.AttemptNo < 1 ||
		attempt.Version < 1 || attempt.Status != status {
		return ErrInvalid
	}
	return nil
}
