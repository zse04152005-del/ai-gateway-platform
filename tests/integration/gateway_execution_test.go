//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/execution"
)

const (
	executionVirtualKeyID      = "74000000-0000-4000-8000-000000000001"
	executionSuccessRequestID  = "integration-execution-success"
	executionFailureRequestID  = "integration-execution-failure"
	executionConflictRequestID = "integration-execution-conflict"
)

func TestGatewayExecutionLifecycle(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set")
	}
	database, err := sql.Open("postgres", databaseURL)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	database.SetMaxOpenConns(4)
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

	now := time.Now().UTC().Truncate(time.Microsecond)
	recorder, err := execution.NewPostgresRecorder(database, func() time.Time {
		now = now.Add(time.Microsecond)
		return now
	}, rand.Reader)
	if err != nil {
		t.Fatalf("execution.NewPostgresRecorder() error = %v", err)
	}

	t.Run("successful attempt records complete versioned evidence", func(t *testing.T) {
		request := startExecutionRequest(ctx, t, recorder, executionSuccessRequestID)
		routed, err := recorder.MarkRouting(ctx, request)
		if err != nil {
			t.Fatalf("MarkRouting() error = %v", err)
		}
		running, attempt, err := recorder.StartAttempt(ctx, routed, modelListDeploymentAID)
		if err != nil {
			t.Fatalf("StartAttempt() error = %v", err)
		}
		usage := adapter.NormalizedUsage{
			InputTokens: adapter.Tokens(0), OutputTokens: adapter.Tokens(9),
			Source: adapter.UsageSourceEstimated, Complete: false,
		}
		completedRequest, completedAttempt, err := recorder.CompleteAttempt(ctx, running, attempt, execution.AttemptOutcome{
			AttemptStatus: execution.AttemptSucceeded, RequestStatus: execution.RequestSucceeded,
			HeadersReceived: true, EndReason: "completed", ProviderRequestID: "provider/request-1", Usage: &usage,
		})
		if err != nil {
			t.Fatalf("CompleteAttempt() error = %v", err)
		}
		if completedRequest.Status != execution.RequestSucceeded || completedRequest.Version != 4 ||
			completedRequest.AttemptCount != 1 || completedRequest.EndedAt == nil {
			t.Fatalf("completed request = %+v", completedRequest)
		}
		if completedAttempt.Status != execution.AttemptSucceeded || completedAttempt.Version != 4 ||
			completedAttempt.HeadersReceivedAt == nil || completedAttempt.EndedAt == nil ||
			completedAttempt.ProviderRequestID != "provider/request-1" {
			t.Fatalf("completed attempt = %+v", completedAttempt)
		}
		var summary map[string]any
		if err := json.Unmarshal(completedAttempt.UsageSummary, &summary); err != nil {
			t.Fatalf("decode UsageSummary: %v", err)
		}
		if summary["input_tokens"] != float64(0) || summary["output_tokens"] != float64(9) ||
			summary["complete"] != false {
			t.Fatalf("usage summary = %#v", summary)
		}
		assertExecutionEvents(ctx, t, database, executionSuccessRequestID, completedAttempt.ID,
			[]string{"authorized", "routing", "running", "succeeded"},
			[]string{"created", "connecting", "headers_received", "succeeded"},
		)

		_, err = database.ExecContext(ctx, `
			UPDATE app.route_attempts
			SET status = 'failed', ended_at = CURRENT_TIMESTAMP, end_reason = 'overwritten',
				error_category = 'protocol', error_code = 'OVERWRITTEN',
				version = version + 1, updated_at = CURRENT_TIMESTAMP
			WHERE id = $1`, completedAttempt.ID)
		expectExecutionSQLState(t, err, "23514")
	})

	t.Run("provider failure and database conflicts stay explicit", func(t *testing.T) {
		request := startExecutionRequest(ctx, t, recorder, executionFailureRequestID)
		routed, err := recorder.MarkRouting(ctx, request)
		if err != nil {
			t.Fatalf("MarkRouting() error = %v", err)
		}
		running, attempt, err := recorder.StartAttempt(ctx, routed, modelListDeploymentAID)
		if err != nil {
			t.Fatalf("StartAttempt() error = %v", err)
		}

		_, err = database.ExecContext(ctx, `
			INSERT INTO app.route_attempts (
				id, request_id, attempt_no, deployment_id, status, started_at, version, updated_at
			) VALUES ('64000000-0000-4000-8000-000000000099', $1, 1, $2, 'created', CURRENT_TIMESTAMP, 1, CURRENT_TIMESTAMP)`,
			executionFailureRequestID, modelListDeploymentAID)
		expectConstraint(t, err, "route_attempts_request_number_unique")

		_, err = database.ExecContext(ctx, `
			UPDATE app.route_attempts
			SET status = 'succeeded', ended_at = CURRENT_TIMESTAMP, end_reason = 'completed',
				version = version + 1, updated_at = CURRENT_TIMESTAMP
			WHERE id = $1`, attempt.ID)
		expectExecutionSQLState(t, err, "23514")

		completedRequest, completedAttempt, err := recorder.CompleteAttempt(ctx, running, attempt, execution.AttemptOutcome{
			AttemptStatus: execution.AttemptRetryableFailed, RequestStatus: execution.RequestFailed,
			HeadersReceived: true, EndReason: "provider_capacity", ProviderRequestID: "provider-request-429",
			ErrorCategory: string(adapter.ErrorCapacity), ErrorCode: "PROVIDER_CAPACITY",
		})
		if err != nil {
			t.Fatalf("CompleteAttempt(provider error) error = %v", err)
		}
		if completedRequest.Status != execution.RequestFailed || completedAttempt.Status != execution.AttemptRetryableFailed ||
			completedAttempt.HeadersReceivedAt == nil || len(completedAttempt.UsageSummary) != 0 {
			t.Fatalf("provider failure result = %+v/%+v", completedRequest, completedAttempt)
		}
		assertExecutionEvents(ctx, t, database, executionFailureRequestID, completedAttempt.ID,
			[]string{"authorized", "routing", "running", "failed"},
			[]string{"created", "connecting", "headers_received", "retryable_failed"},
		)
	})

	t.Run("CAS and trusted scope constraints reject stale or mixed facts", func(t *testing.T) {
		request := startExecutionRequest(ctx, t, recorder, executionConflictRequestID)
		if _, err := recorder.MarkRouting(ctx, request); err != nil {
			t.Fatalf("first MarkRouting() error = %v", err)
		}
		if _, err := recorder.MarkRouting(ctx, request); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("stale MarkRouting() error = %v, want ErrConflict", err)
		}
		_, err := recorder.StartRequest(ctx, execution.StartRequest{
			ID: "integration-execution-cross-scope", TenantID: modelListTenantOneID,
			ProjectID: modelListProjectTwoID, VirtualKeyID: executionVirtualKeyID,
			LogicalModel: "model-a", TraceID: "11111111111111111111111111111111", SpanID: "2222222222222222",
		})
		if !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("cross-scope StartRequest() error = %v, want ErrConflict", err)
		}
	})

	t.Run("pre-attempt and no-header failures are durably terminal", func(t *testing.T) {
		noCandidate := startExecutionRequest(ctx, t, recorder, "integration-execution-no-candidate")
		routed, err := recorder.MarkRouting(ctx, noCandidate)
		if err != nil {
			t.Fatalf("MarkRouting(no candidate) error = %v", err)
		}
		failed, err := recorder.FailRequest(ctx, routed, execution.RequestFailed, "model_unavailable")
		if err != nil || failed.Status != execution.RequestFailed || failed.Version != 3 || failed.AttemptCount != 0 {
			t.Fatalf("FailRequest(no candidate) = %+v/%v", failed, err)
		}
		if _, err := recorder.FailRequest(ctx, routed, execution.RequestFailed, "model_unavailable"); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("stale FailRequest() error = %v, want ErrConflict", err)
		}

		cancelledStart := startExecutionRequest(ctx, t, recorder, "integration-execution-cancel-before-routing")
		cancelled, err := recorder.FailRequest(ctx, cancelledStart, execution.RequestCancelled, "client_cancelled")
		if err != nil || cancelled.Status != execution.RequestCancelled || cancelled.Version != 2 {
			t.Fatalf("FailRequest(cancelled) = %+v/%v", cancelled, err)
		}

		transport := startExecutionRequest(ctx, t, recorder, "integration-execution-transport")
		transport, err = recorder.MarkRouting(ctx, transport)
		if err != nil {
			t.Fatalf("MarkRouting(transport) error = %v", err)
		}
		transport, attempt, err := recorder.StartAttempt(ctx, transport, modelListDeploymentAID)
		if err != nil {
			t.Fatalf("StartAttempt(transport) error = %v", err)
		}
		transport, completedAttempt, err := recorder.CompleteAttempt(ctx, transport, attempt, execution.AttemptOutcome{
			AttemptStatus: execution.AttemptRetryableFailed, RequestStatus: execution.RequestFailed,
			EndReason: "provider_transport", ErrorCategory: "transport", ErrorCode: "PROVIDER_TRANSPORT",
		})
		if err != nil || transport.Status != execution.RequestFailed ||
			completedAttempt.Status != execution.AttemptRetryableFailed || completedAttempt.HeadersReceivedAt != nil {
			t.Fatalf("CompleteAttempt(transport) = %+v/%+v/%v", transport, completedAttempt, err)
		}

		cancelledAttemptRequest := startExecutionRequest(ctx, t, recorder, "integration-execution-cancel-active")
		cancelledAttemptRequest, err = recorder.MarkRouting(ctx, cancelledAttemptRequest)
		if err != nil {
			t.Fatalf("MarkRouting(active cancellation) error = %v", err)
		}
		cancelledAttemptRequest, activeAttempt, err := recorder.StartAttempt(ctx, cancelledAttemptRequest, modelListDeploymentAID)
		if err != nil {
			t.Fatalf("StartAttempt(active cancellation) error = %v", err)
		}
		cancelledAttemptRequest, activeAttempt, err = recorder.CompleteAttempt(ctx, cancelledAttemptRequest, activeAttempt, execution.AttemptOutcome{
			AttemptStatus: execution.AttemptCancelled, RequestStatus: execution.RequestCancelled,
			EndReason: "client_cancelled", ErrorCategory: string(adapter.ErrorCancelled), ErrorCode: "CLIENT_CANCELLED",
		})
		if err != nil || cancelledAttemptRequest.Status != execution.RequestCancelled ||
			cancelledAttemptRequest.EndReason != "client_cancelled" || activeAttempt.Status != execution.AttemptCancelled ||
			activeAttempt.EndReason != "client_cancelled" || activeAttempt.HeadersReceivedAt != nil {
			t.Fatalf("CompleteAttempt(active cancellation) = %+v/%+v/%v", cancelledAttemptRequest, activeAttempt, err)
		}
	})
}

func startExecutionRequest(
	ctx context.Context,
	t *testing.T,
	recorder execution.Recorder,
	requestID string,
) execution.GatewayRequest {
	t.Helper()
	request, err := recorder.StartRequest(ctx, execution.StartRequest{
		ID: requestID, TenantID: modelListTenantOneID, ProjectID: modelListProjectOneID,
		VirtualKeyID: executionVirtualKeyID, LogicalModel: "model-a",
		TraceID: "11111111111111111111111111111111", SpanID: "2222222222222222",
	})
	if err != nil {
		t.Fatalf("StartRequest(%s) error = %v", requestID, err)
	}
	return request
}

func seedExecutionVirtualKey(ctx context.Context, t *testing.T, database *sql.DB) {
	t.Helper()
	_, err := database.ExecContext(ctx, `
		INSERT INTO app.virtual_api_keys (
			id, tenant_id, project_id, key_prefix, secret_hash, hash_key_version,
			status, allowed_models, created_by, updated_by
		) VALUES ($1, $2, $3, 'agw_test_exec0001', $4, 'integration-v1',
			'active', ARRAY['model-a'], 'integration:execution', 'integration:execution')`,
		executionVirtualKeyID, modelListTenantOneID, modelListProjectOneID, bytes.Repeat([]byte{0x6e}, 32),
	)
	if err != nil {
		t.Fatalf("seed execution virtual key: %v", err)
	}
}

func assertExecutionEvents(
	ctx context.Context,
	t *testing.T,
	database *sql.DB,
	requestID, attemptID string,
	wantRequest, wantAttempt []string,
) {
	t.Helper()
	requestEvents := queryStatusEvents(ctx, t, database, `
		SELECT to_status FROM app.gateway_request_status_events
		WHERE request_id = $1 ORDER BY request_version`, requestID)
	attemptEvents := queryStatusEvents(ctx, t, database, `
		SELECT to_status FROM app.route_attempt_status_events
		WHERE attempt_id = $1 ORDER BY attempt_version`, attemptID)
	if !reflect.DeepEqual(requestEvents, wantRequest) || !reflect.DeepEqual(attemptEvents, wantAttempt) {
		t.Fatalf("status events request/attempt = %v/%v, want %v/%v", requestEvents, attemptEvents, wantRequest, wantAttempt)
	}
}

func queryStatusEvents(ctx context.Context, t *testing.T, database *sql.DB, query, id string) []string {
	t.Helper()
	rows, err := database.QueryContext(ctx, query, id)
	if err != nil {
		t.Fatalf("query status events: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var statuses []string
	for rows.Next() {
		var status string
		if err := rows.Scan(&status); err != nil {
			t.Fatalf("scan status event: %v", err)
		}
		statuses = append(statuses, status)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate status events: %v", err)
	}
	return statuses
}

func cleanupGatewayExecutionFixtures(t *testing.T, database *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	statements := []string{
		`DELETE FROM app.route_attempt_status_events WHERE attempt_id IN (
			SELECT id FROM app.route_attempts WHERE request_id LIKE 'integration-execution-%')`,
		`DELETE FROM app.gateway_request_status_events WHERE request_id LIKE 'integration-execution-%'`,
		`DELETE FROM app.route_attempts WHERE request_id LIKE 'integration-execution-%'`,
		`DELETE FROM app.gateway_requests WHERE id LIKE 'integration-execution-%'`,
	}
	for _, statement := range statements {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Errorf("cleanup gateway execution fixtures: %v", err)
		}
	}
}

func expectExecutionSQLState(t *testing.T, err error, code string) {
	t.Helper()
	var databaseError *pq.Error
	if !errors.As(err, &databaseError) || string(databaseError.Code) != code {
		t.Fatalf("error = %v, want PostgreSQL SQLSTATE %s", err, code)
	}
}
