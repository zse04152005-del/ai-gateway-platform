//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/execution"
	"github.com/zse04152005-del/ai-gateway-platform/internal/retry"
	"github.com/zse04152005-del/ai-gateway-platform/internal/routedecision"
	"github.com/zse04152005-del/ai-gateway-platform/internal/routing"
)

func TestRouteDecisionReplayEnforcesScopeAndSequence(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set")
	}
	database, err := sql.Open("postgres", databaseURL)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		t.Fatalf("database.PingContext() error = %v", err)
	}

	cleanupGatewayExecutionFixtures(t, database)
	cleanupModelListFixtures(t, database)
	t.Cleanup(func() {
		cleanupGatewayExecutionFixtures(t, database)
		cleanupModelListFixtures(t, database)
	})
	seedModelListCatalog(ctx, t, database)
	seedExecutionVirtualKey(ctx, t, database)

	executionRecorder, err := execution.NewPostgresRecorder(database, time.Now, rand.Reader)
	if err != nil {
		t.Fatalf("execution.NewPostgresRecorder() error = %v", err)
	}
	decisionStore, err := routedecision.NewPostgresStore(database, time.Now)
	if err != nil {
		t.Fatalf("routedecision.NewPostgresStore() error = %v", err)
	}
	requestID := "integration-execution-route-decision"
	request := startExecutionRequest(ctx, t, executionRecorder, requestID)
	request, err = executionRecorder.MarkRouting(ctx, request)
	if err != nil {
		t.Fatalf("MarkRouting() error = %v", err)
	}

	firstFilter := routing.FilterResult{
		PolicyVersion: "candidate-filter/v1",
		Decisions: []routing.CandidateDecision{
			{DeploymentID: modelListDeploymentAID, Eligible: true, Reason: routing.FilterEligible},
			{DeploymentID: modelListDeploymentBID, Eligible: true, Reason: routing.FilterEligible},
		},
	}
	firstPolicy := routing.PolicyDecision{
		PolicyVersion: "bootstrap-priority/v1", Mode: routing.RoutePriority,
		SelectedDeploymentID: modelListDeploymentAID, Priority: 10, Weight: 20, EligibleCount: 2,
	}
	first, err := decisionStore.Record(ctx, routedecision.Input{
		RequestID: requestID, NextAttemptNo: 1, Outcome: routedecision.OutcomeSelected,
		Filter: firstFilter, Policy: &firstPolicy,
	})
	if err != nil || first.DecisionNo != 1 || first.Policy == nil || first.Retry != nil {
		t.Fatalf("Record(first) = %+v/%v", first, err)
	}

	request, attempt, err := executionRecorder.StartAttempt(ctx, request, modelListDeploymentAID)
	if err != nil {
		t.Fatalf("StartAttempt() error = %v", err)
	}
	retryDecision := retry.Decision{
		PolicyVersion: "retry-classifier/v1", Action: retry.DifferentDeploymentOnly,
		Reason: retry.ReasonCapacity, FailureClass: retry.FailureCapacity,
		Submission: retry.Submitted, AttemptNumber: 1, MaximumAttempts: 3,
		RemainingBudgetMS: 25000,
	}
	retryRecord, err := decisionStore.RecordRetry(ctx, routedecision.RetryInput{
		RequestID: requestID, AttemptNo: 1, Decision: retryDecision,
	})
	if err != nil || retryRecord.AttemptNo != 1 || retryRecord.Decision.Action != retry.DifferentDeploymentOnly {
		t.Fatalf("RecordRetry() = %+v/%v", retryRecord, err)
	}
	_, err = executionRecorder.CompleteAttemptForRetry(ctx, request, attempt, execution.AttemptOutcome{
		AttemptStatus: execution.AttemptRetryableFailed, RequestStatus: execution.RequestRunning,
		HeadersReceived: true, EndReason: "provider_capacity", ErrorCategory: string(adapter.ErrorCapacity),
		ErrorCode: "PROVIDER_CAPACITY",
	})
	if err != nil {
		t.Fatalf("CompleteAttemptForRetry() error = %v", err)
	}

	secondFilter := routing.FilterResult{
		PolicyVersion: "candidate-filter/v1",
		Decisions: []routing.CandidateDecision{
			{DeploymentID: modelListDeploymentAID, Reason: routing.FilterPreviouslyAttempted},
			{DeploymentID: modelListDeploymentBID, Eligible: true, Reason: routing.FilterEligible},
		},
	}
	secondPolicy := routing.PolicyDecision{
		PolicyVersion: "bootstrap-priority/v1", Mode: routing.RoutePriority,
		SelectedDeploymentID: modelListDeploymentBID, Priority: 20, Weight: 10, EligibleCount: 1,
	}
	second, err := decisionStore.Record(ctx, routedecision.Input{
		RequestID: requestID, NextAttemptNo: 2, Outcome: routedecision.OutcomeSelected,
		Filter: secondFilter, Policy: &secondPolicy, Retry: &retryDecision,
	})
	if err != nil || second.DecisionNo != 2 || second.Retry == nil {
		t.Fatalf("Record(second) = %+v/%v", second, err)
	}

	scope := routedecision.Scope{TenantID: modelListTenantOneID, ProjectID: modelListProjectOneID}
	replayed, err := decisionStore.ListByRequestID(ctx, scope, requestID)
	if err != nil || len(replayed) != 2 || replayed[0].DecisionNo != 1 || replayed[1].DecisionNo != 2 ||
		replayed[1].NextAttemptNo != 2 || replayed[1].Policy == nil ||
		replayed[1].Policy.SelectedDeploymentID != modelListDeploymentBID || replayed[1].Retry == nil ||
		replayed[1].Retry.Action != retry.DifferentDeploymentOnly ||
		replayed[1].Filter.Decisions[0].Reason != routing.FilterPreviouslyAttempted {
		t.Fatalf("ListByRequestID() = %+v/%v", replayed, err)
	}
	replayed[1].Filter.Decisions[0].Reason = routing.FilterEligible
	again, err := decisionStore.ListByRequestID(ctx, scope, requestID)
	if err != nil || again[1].Filter.Decisions[0].Reason != routing.FilterPreviouslyAttempted {
		t.Fatalf("stored decision was aliased: %+v/%v", again, err)
	}
	retries, err := decisionStore.ListRetriesByRequestID(ctx, scope, requestID)
	if err != nil || len(retries) != 1 || retries[0].AttemptNo != 1 ||
		retries[0].Decision.Reason != retry.ReasonCapacity {
		t.Fatalf("ListRetriesByRequestID() = %+v/%v", retries, err)
	}

	_, err = decisionStore.ListByRequestID(ctx, routedecision.Scope{
		TenantID: modelListTenantTwoID, ProjectID: modelListProjectTwoID,
	}, requestID)
	if !errors.Is(err, routedecision.ErrNotFound) {
		t.Fatalf("cross-scope ListByRequestID() error = %v, want ErrNotFound", err)
	}

	noCandidateID := "integration-execution-route-no-candidate"
	noCandidateRequest := startExecutionRequest(ctx, t, executionRecorder, noCandidateID)
	_, err = executionRecorder.MarkRouting(ctx, noCandidateRequest)
	if err != nil {
		t.Fatalf("MarkRouting(no candidate) error = %v", err)
	}
	noCandidate, err := decisionStore.Record(ctx, routedecision.Input{
		RequestID: noCandidateID, NextAttemptNo: 1, Outcome: routedecision.OutcomeNoCandidate,
		Filter: routing.FilterResult{PolicyVersion: "candidate-filter/v1"},
	})
	if err != nil || noCandidate.Outcome != routedecision.OutcomeNoCandidate || noCandidate.Policy != nil {
		t.Fatalf("Record(no candidate) = %+v/%v", noCandidate, err)
	}

	_, err = database.ExecContext(ctx, `
		INSERT INTO app.route_decisions (
			request_id, decision_no, next_attempt_no, outcome,
			filter_policy_version, candidate_decisions, decided_at
		) VALUES ($1, 3, 2, 'no_candidate', 'candidate-filter/v1', '{}'::jsonb, CURRENT_TIMESTAMP)`, requestID)
	expectConstraint(t, err, "route_decisions_candidate_decisions_valid")
}
