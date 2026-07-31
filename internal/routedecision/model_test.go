package routedecision

import (
	"errors"
	"testing"

	"github.com/zse04152005-del/ai-gateway-platform/internal/retry"
	"github.com/zse04152005-del/ai-gateway-platform/internal/routing"
)

const (
	testRequestID    = "route-decision-request-0001"
	testDeploymentID = "65000000-0000-4000-8000-000000000001"
)

func TestInputValidationAndClone(t *testing.T) {
	draw := uint64(0)
	input := Input{
		RequestID: testRequestID, NextAttemptNo: 2, Outcome: OutcomeSelected,
		Filter: routing.FilterResult{
			PolicyVersion: "candidate-filter/v1",
			Decisions: []routing.CandidateDecision{{
				DeploymentID: testDeploymentID, Eligible: true, Reason: routing.FilterEligible,
			}},
		},
		Policy: &routing.PolicyDecision{
			PolicyVersion: "route-policy/v1", Mode: routing.RouteWeighted,
			SelectedDeploymentID: testDeploymentID, Priority: 1, Weight: 10,
			EligibleCount: 1, TotalWeight: 10, RandomDraw: &draw,
		},
		Retry: &retry.Decision{
			PolicyVersion: "retry-classifier/v1", Action: retry.DifferentDeploymentOnly,
			Reason: retry.ReasonCapacity, FailureClass: retry.FailureCapacity,
			Submission: retry.Submitted, AttemptNumber: 1, MaximumAttempts: 3,
			RemainingBudgetMS: 1000,
		},
	}
	if err := input.Validate(); err != nil {
		t.Fatalf("Input.Validate() error = %v", err)
	}
	cloned := cloneInput(input)
	cloned.Filter.Decisions[0].Reason = routing.FilterUnhealthy
	*cloned.Policy.RandomDraw = 4
	cloned.Retry.Action = retry.NoRetry
	if input.Filter.Decisions[0].Reason != routing.FilterEligible || *input.Policy.RandomDraw != 0 ||
		input.Retry.Action != retry.DifferentDeploymentOnly {
		t.Fatal("cloneInput() retained aliases")
	}
}

func TestInputRejectsAmbiguousOrUnsafeExplanations(t *testing.T) {
	validFilter := routing.FilterResult{
		PolicyVersion: "candidate-filter/v1",
		Decisions: []routing.CandidateDecision{{
			DeploymentID: testDeploymentID, Eligible: true, Reason: routing.FilterEligible,
		}},
	}
	validPolicy := routing.PolicyDecision{
		PolicyVersion: "route-policy/v1", Mode: routing.RoutePriority,
		SelectedDeploymentID: testDeploymentID, Priority: 1, Weight: 10, EligibleCount: 1,
	}
	tests := []struct {
		name  string
		input Input
	}{
		{name: "bad request", input: Input{RequestID: "short", NextAttemptNo: 1, Outcome: OutcomeNoCandidate, Filter: validFilter}},
		{name: "selected without policy", input: Input{RequestID: testRequestID, NextAttemptNo: 1, Outcome: OutcomeSelected, Filter: validFilter}},
		{name: "no candidate with eligible", input: Input{RequestID: testRequestID, NextAttemptNo: 1, Outcome: OutcomeNoCandidate, Filter: validFilter}},
		{name: "selection failed with policy", input: Input{RequestID: testRequestID, NextAttemptNo: 1, Outcome: OutcomeSelectionFailed, Filter: validFilter, Policy: &validPolicy}},
		{name: "unknown outcome", input: Input{RequestID: testRequestID, NextAttemptNo: 1, Outcome: "private failure", Filter: validFilter}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.input.Validate(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Input.Validate() error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestNoCandidateExplanationIsValid(t *testing.T) {
	input := Input{
		RequestID: testRequestID, NextAttemptNo: 1, Outcome: OutcomeNoCandidate,
		Filter: routing.FilterResult{
			PolicyVersion: "candidate-filter/v1",
			Decisions: []routing.CandidateDecision{{
				DeploymentID: testDeploymentID, Reason: routing.FilterUnhealthy,
			}},
		},
	}
	if err := input.Validate(); err != nil {
		t.Fatalf("no-candidate Input.Validate() error = %v", err)
	}
}
