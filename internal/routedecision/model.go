// Package routedecision persists content-free routing explanations.
package routedecision

import (
	"errors"
	"regexp"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/retry"
	"github.com/zse04152005-del/ai-gateway-platform/internal/routing"
)

const maximumDecisionCandidates = 256

var (
	// ErrInvalid means the supplied explanation violates the safe persistence contract.
	ErrInvalid = errors.New("route decision input is invalid")
	// ErrUnavailable means the durable decision store could not be reached safely.
	ErrUnavailable = errors.New("route decision store unavailable")
	// ErrConflict means a request state or decision sequence changed concurrently.
	ErrConflict = errors.New("route decision record conflict")
	// ErrNotFound means the trusted request scope has no matching request.
	ErrNotFound = errors.New("route decision request not found")

	requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`)
	uuidPattern      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

// Outcome is the finite result of one selector evaluation.
type Outcome string

const (
	// OutcomeSelected means a policy selected one eligible deployment.
	OutcomeSelected Outcome = "selected"
	// OutcomeNoCandidate means all known candidates were safely filtered out.
	OutcomeNoCandidate Outcome = "no_candidate"
	// OutcomeSelectionFailed means a dependency or policy failed closed.
	OutcomeSelectionFailed Outcome = "selection_failed"
)

// Input is one selector evaluation about the next possible physical Attempt.
type Input struct {
	RequestID     string
	NextAttemptNo int
	Outcome       Outcome
	Filter        routing.FilterResult
	Policy        *routing.PolicyDecision
	Retry         *retry.Decision
}

// Validate enforces content-free, bounded and internally consistent facts.
func (input Input) Validate() error {
	if !requestIDPattern.MatchString(input.RequestID) || input.NextAttemptNo < 1 ||
		len(input.Filter.Decisions) > maximumDecisionCandidates || input.Filter.ValidateExplanation() != nil {
		return ErrInvalid
	}
	switch input.Outcome {
	case OutcomeSelected:
		if input.Policy == nil || input.Policy.Validate() != nil ||
			!selectedCandidateEligible(input.Filter, input.Policy.SelectedDeploymentID) {
			return ErrInvalid
		}
	case OutcomeNoCandidate:
		if input.Policy != nil || hasEligibleCandidate(input.Filter) {
			return ErrInvalid
		}
	case OutcomeSelectionFailed:
		if input.Policy != nil {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	if input.Retry != nil && input.Retry.Validate() != nil {
		return ErrInvalid
	}
	return nil
}

// Scope is the trusted tenant/project boundary required for replay queries.
type Scope struct {
	TenantID  string
	ProjectID string
}

// Validate rejects untrusted or cross-scope query keys.
func (scope Scope) Validate() error {
	if !uuidPattern.MatchString(scope.TenantID) || !uuidPattern.MatchString(scope.ProjectID) {
		return ErrInvalid
	}
	return nil
}

// Record is one immutable explanation ordered within a GatewayRequest.
type Record struct {
	RequestID     string                  `json:"request_id"`
	DecisionNo    int                     `json:"decision_no"`
	NextAttemptNo int                     `json:"next_attempt_no"`
	Outcome       Outcome                 `json:"outcome"`
	Filter        routing.FilterResult    `json:"filter"`
	Policy        *routing.PolicyDecision `json:"policy,omitempty"`
	Retry         *retry.Decision         `json:"retry,omitempty"`
	DecidedAt     time.Time               `json:"decided_at"`
}

// RetryInput is the safe classifier result for one already-created Attempt.
type RetryInput struct {
	RequestID string
	AttemptNo int
	Decision  retry.Decision
}

// Validate rejects mismatched or unbounded retry facts.
func (input RetryInput) Validate() error {
	if !requestIDPattern.MatchString(input.RequestID) || input.AttemptNo < 1 ||
		input.Decision.Validate() != nil || input.Decision.AttemptNumber != input.AttemptNo {
		return ErrInvalid
	}
	return nil
}

// RetryRecord is an immutable per-Attempt classifier result.
type RetryRecord struct {
	RequestID string         `json:"request_id"`
	AttemptNo int            `json:"attempt_no"`
	Decision  retry.Decision `json:"decision"`
	DecidedAt time.Time      `json:"decided_at"`
}

// Clone returns an alias-free record suitable for asynchronous diagnostics.
func (record Record) Clone() Record {
	record.Filter = record.Filter.Clone()
	if record.Policy != nil {
		policy := record.Policy.Clone()
		record.Policy = &policy
	}
	if record.Retry != nil {
		retryDecision := *record.Retry
		record.Retry = &retryDecision
	}
	return record
}

func cloneInput(input Input) Input {
	input.Filter = input.Filter.Clone()
	if input.Policy != nil {
		policy := input.Policy.Clone()
		input.Policy = &policy
	}
	if input.Retry != nil {
		retryDecision := *input.Retry
		input.Retry = &retryDecision
	}
	return input
}

func selectedCandidateEligible(filter routing.FilterResult, deploymentID string) bool {
	decision, ok := filter.DecisionFor(deploymentID)
	return ok && decision.Eligible && decision.Reason == routing.FilterEligible
}

func hasEligibleCandidate(filter routing.FilterResult) bool {
	for _, decision := range filter.Decisions {
		if decision.Eligible {
			return true
		}
	}
	return false
}
