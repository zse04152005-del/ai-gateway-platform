package routedecision

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
	"github.com/zse04152005-del/ai-gateway-platform/internal/retry"
	"github.com/zse04152005-del/ai-gateway-platform/internal/routing"
)

// Recorder durably appends one safe routing evaluation before provider I/O.
type Recorder interface {
	Record(context.Context, Input) (Record, error)
	RecordRetry(context.Context, RetryInput) (RetryRecord, error)
}

// Reader replays all evaluations within a trusted request scope.
type Reader interface {
	ListByRequestID(context.Context, Scope, string) ([]Record, error)
	ListRetriesByRequestID(context.Context, Scope, string) ([]RetryRecord, error)
}

// Store combines the append and replay ports.
type Store interface {
	Recorder
	Reader
}

// PostgresStore serializes decision numbers by locking the parent request row.
type PostgresStore struct {
	database *sql.DB
	now      func() time.Time
}

// NewPostgresStore validates process-scoped dependencies.
func NewPostgresStore(database *sql.DB, now func() time.Time) (*PostgresStore, error) {
	if database == nil {
		return nil, errors.New("route decision database must not be nil")
	}
	if now == nil || now().IsZero() {
		return nil, errors.New("route decision clock must return a non-zero time")
	}
	return &PostgresStore{database: database, now: now}, nil
}

// Record appends an explanation. The request row lock prevents duplicate or
// reordered decision numbers when the same request is accidentally concurrent.
func (store *PostgresStore) Record(ctx context.Context, input Input) (Record, error) {
	if store == nil || store.database == nil || store.now == nil || ctx == nil || input.Validate() != nil {
		return Record{}, ErrInvalid
	}
	input = cloneInput(input)
	candidateDecisions := input.Filter.Decisions
	if candidateDecisions == nil {
		candidateDecisions = make([]routing.CandidateDecision, 0)
	}
	candidates, err := json.Marshal(candidateDecisions)
	if err != nil {
		return Record{}, fmt.Errorf("%w: encode candidates", ErrInvalid)
	}
	policyJSON, routePolicyVersion, selectedDeploymentID, err := marshalPolicy(input.Policy)
	if err != nil {
		return Record{}, err
	}
	retryJSON, retryPolicyVersion, err := marshalRetry(input.Retry)
	if err != nil {
		return Record{}, err
	}

	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, wrapDatabaseError(err)
	}
	defer func() { _ = transaction.Rollback() }()

	var status string
	var attemptCount int
	if err = transaction.QueryRowContext(ctx, `
		SELECT status, attempt_count
		FROM app.gateway_requests
		WHERE id = $1
		FOR UPDATE`, input.RequestID).Scan(&status, &attemptCount); err != nil {
		return Record{}, wrapDatabaseError(err)
	}
	if (status != "routing" && status != "running") || input.NextAttemptNo != attemptCount+1 {
		return Record{}, ErrConflict
	}

	var decisionNo int
	if err = transaction.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(decision_no), 0) + 1
		FROM app.route_decisions
		WHERE request_id = $1`, input.RequestID).Scan(&decisionNo); err != nil {
		return Record{}, wrapDatabaseError(err)
	}
	decidedAt := store.now().UTC()
	if _, err = transaction.ExecContext(ctx, `
		INSERT INTO app.route_decisions (
			request_id, decision_no, next_attempt_no, outcome,
			filter_policy_version, candidate_decisions,
			route_policy_version, policy_decision,
			retry_policy_version, retry_decision,
			selected_deployment_id, decided_at
		) VALUES (
			$1, $2, $3, $4, $5, $6::jsonb,
			NULLIF($7, ''), $8::jsonb, NULLIF($9, ''), $10::jsonb,
			NULLIF($11::text, '')::uuid, $12
		)`,
		input.RequestID, decisionNo, input.NextAttemptNo, input.Outcome,
		input.Filter.PolicyVersion, string(candidates), routePolicyVersion, nullableJSON(policyJSON),
		retryPolicyVersion, nullableJSON(retryJSON), selectedDeploymentID, decidedAt,
	); err != nil {
		return Record{}, wrapDatabaseError(err)
	}
	if err = transaction.Commit(); err != nil {
		return Record{}, wrapDatabaseError(err)
	}
	return Record{
		RequestID: input.RequestID, DecisionNo: decisionNo, NextAttemptNo: input.NextAttemptNo,
		Outcome: input.Outcome, Filter: input.Filter.Clone(), Policy: input.Policy,
		Retry: input.Retry, DecidedAt: decidedAt,
	}.Clone(), nil
}

// RecordRetry appends the classifier result before the active Attempt is
// completed. A duplicate decision for the same Attempt is a conflict.
func (store *PostgresStore) RecordRetry(ctx context.Context, input RetryInput) (RetryRecord, error) {
	if store == nil || store.database == nil || store.now == nil || ctx == nil || input.Validate() != nil {
		return RetryRecord{}, ErrInvalid
	}
	encoded, policyVersion, err := marshalRetry(&input.Decision)
	if err != nil {
		return RetryRecord{}, err
	}
	decidedAt := store.now().UTC()
	result, err := store.database.ExecContext(ctx, `
		INSERT INTO app.route_retry_decisions (
			request_id, attempt_no, retry_policy_version, retry_decision, decided_at
		)
		SELECT $1, $2, $3, $4::jsonb, $5
		FROM app.route_attempts a
		JOIN app.gateway_requests r ON r.id = a.request_id
		WHERE a.request_id = $1 AND a.attempt_no = $2
			AND a.status IN ('connecting', 'headers_received', 'streaming')
			AND r.status = 'running'`,
		input.RequestID, input.AttemptNo, policyVersion, string(encoded), decidedAt,
	)
	if err != nil {
		return RetryRecord{}, wrapDatabaseError(err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return RetryRecord{}, wrapDatabaseError(err)
	}
	if rows != 1 {
		return RetryRecord{}, ErrConflict
	}
	return RetryRecord{
		RequestID: input.RequestID, AttemptNo: input.AttemptNo,
		Decision: input.Decision, DecidedAt: decidedAt,
	}, nil
}

// ListByRequestID returns ordered, alias-free decisions only when both trusted
// tenant and project match the parent request.
func (store *PostgresStore) ListByRequestID(ctx context.Context, scope Scope, requestID string) ([]Record, error) {
	if store == nil || store.database == nil || ctx == nil || scope.Validate() != nil ||
		!requestIDPattern.MatchString(requestID) {
		return nil, ErrInvalid
	}
	rows, err := store.database.QueryContext(ctx, `
		SELECT d.request_id, d.decision_no, d.next_attempt_no, d.outcome,
			d.filter_policy_version, d.candidate_decisions,
			d.route_policy_version, d.policy_decision,
			d.retry_policy_version, d.retry_decision,
			d.selected_deployment_id::text, d.decided_at
		FROM app.route_decisions d
		JOIN app.gateway_requests r ON r.id = d.request_id
		WHERE d.request_id = $1 AND r.tenant_id = $2::uuid AND r.project_id = $3::uuid
		ORDER BY d.decision_no`, requestID, scope.TenantID, scope.ProjectID)
	if err != nil {
		return nil, wrapDatabaseError(err)
	}
	defer func() { _ = rows.Close() }()
	records := make([]Record, 0)
	for rows.Next() {
		record, scanErr := scanRecord(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		records = append(records, record)
	}
	if err = rows.Err(); err != nil {
		return nil, wrapDatabaseError(err)
	}
	if len(records) == 0 {
		var exists bool
		if err = store.database.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM app.gateway_requests
				WHERE id = $1 AND tenant_id = $2::uuid AND project_id = $3::uuid
			)`, requestID, scope.TenantID, scope.ProjectID).Scan(&exists); err != nil {
			return nil, wrapDatabaseError(err)
		}
		if !exists {
			return nil, ErrNotFound
		}
	}
	return records, nil
}

// ListRetriesByRequestID returns every Attempt classifier result in Attempt order.
func (store *PostgresStore) ListRetriesByRequestID(ctx context.Context, scope Scope, requestID string) ([]RetryRecord, error) {
	if store == nil || store.database == nil || ctx == nil || scope.Validate() != nil ||
		!requestIDPattern.MatchString(requestID) {
		return nil, ErrInvalid
	}
	rows, err := store.database.QueryContext(ctx, `
		SELECT d.request_id, d.attempt_no, d.retry_policy_version, d.retry_decision, d.decided_at
		FROM app.route_retry_decisions d
		JOIN app.gateway_requests r ON r.id = d.request_id
		WHERE d.request_id = $1 AND r.tenant_id = $2::uuid AND r.project_id = $3::uuid
		ORDER BY d.attempt_no`, requestID, scope.TenantID, scope.ProjectID)
	if err != nil {
		return nil, wrapDatabaseError(err)
	}
	defer func() { _ = rows.Close() }()
	records := make([]RetryRecord, 0)
	for rows.Next() {
		var record RetryRecord
		var policyVersion string
		var encoded []byte
		if err = rows.Scan(&record.RequestID, &record.AttemptNo, &policyVersion, &encoded, &record.DecidedAt); err != nil {
			return nil, wrapDatabaseError(err)
		}
		if err = json.Unmarshal(encoded, &record.Decision); err != nil ||
			record.Decision.PolicyVersion != policyVersion ||
			(RetryInput{RequestID: record.RequestID, AttemptNo: record.AttemptNo, Decision: record.Decision}).Validate() != nil ||
			record.DecidedAt.IsZero() {
			return nil, fmt.Errorf("%w: invalid stored retry decision", ErrUnavailable)
		}
		records = append(records, record)
	}
	if err = rows.Err(); err != nil {
		return nil, wrapDatabaseError(err)
	}
	if len(records) == 0 {
		var exists bool
		if err = store.database.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM app.gateway_requests
				WHERE id = $1 AND tenant_id = $2::uuid AND project_id = $3::uuid
			)`, requestID, scope.TenantID, scope.ProjectID).Scan(&exists); err != nil {
			return nil, wrapDatabaseError(err)
		}
		if !exists {
			return nil, ErrNotFound
		}
	}
	return records, nil
}

func scanRecord(scanner interface{ Scan(...any) error }) (Record, error) {
	var (
		record               Record
		outcome              string
		candidateJSON        []byte
		routePolicyVersion   sql.NullString
		policyJSON           []byte
		retryPolicyVersion   sql.NullString
		retryJSON            []byte
		selectedDeploymentID sql.NullString
	)
	if err := scanner.Scan(
		&record.RequestID, &record.DecisionNo, &record.NextAttemptNo, &outcome,
		&record.Filter.PolicyVersion, &candidateJSON, &routePolicyVersion, &policyJSON,
		&retryPolicyVersion, &retryJSON, &selectedDeploymentID, &record.DecidedAt,
	); err != nil {
		return Record{}, wrapDatabaseError(err)
	}
	record.Outcome = Outcome(outcome)
	if err := json.Unmarshal(candidateJSON, &record.Filter.Decisions); err != nil {
		return Record{}, fmt.Errorf("%w: decode candidates", ErrUnavailable)
	}
	if len(policyJSON) > 0 {
		var policy routing.PolicyDecision
		if err := json.Unmarshal(policyJSON, &policy); err != nil {
			return Record{}, fmt.Errorf("%w: decode policy", ErrUnavailable)
		}
		if !routePolicyVersion.Valid || policy.PolicyVersion != routePolicyVersion.String ||
			!selectedDeploymentID.Valid || policy.SelectedDeploymentID != selectedDeploymentID.String {
			return Record{}, fmt.Errorf("%w: inconsistent policy", ErrUnavailable)
		}
		record.Policy = &policy
	}
	if len(retryJSON) > 0 {
		var retryDecision retry.Decision
		if err := json.Unmarshal(retryJSON, &retryDecision); err != nil {
			return Record{}, fmt.Errorf("%w: decode retry", ErrUnavailable)
		}
		if !retryPolicyVersion.Valid || retryDecision.PolicyVersion != retryPolicyVersion.String {
			return Record{}, fmt.Errorf("%w: inconsistent retry", ErrUnavailable)
		}
		record.Retry = &retryDecision
	}
	input := Input{
		RequestID: record.RequestID, NextAttemptNo: record.NextAttemptNo, Outcome: record.Outcome,
		Filter: record.Filter, Policy: record.Policy, Retry: record.Retry,
	}
	if record.DecisionNo < 1 || record.DecidedAt.IsZero() || input.Validate() != nil {
		return Record{}, fmt.Errorf("%w: invalid stored decision", ErrUnavailable)
	}
	return record.Clone(), nil
}

func marshalPolicy(policy *routing.PolicyDecision) ([]byte, string, string, error) {
	if policy == nil {
		return nil, "", "", nil
	}
	if policy.Validate() != nil {
		return nil, "", "", ErrInvalid
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		return nil, "", "", fmt.Errorf("%w: encode policy", ErrInvalid)
	}
	return encoded, policy.PolicyVersion, policy.SelectedDeploymentID, nil
}

func marshalRetry(decision *retry.Decision) ([]byte, string, error) {
	if decision == nil {
		return nil, "", nil
	}
	if decision.Validate() != nil {
		return nil, "", ErrInvalid
	}
	encoded, err := json.Marshal(decision)
	if err != nil {
		return nil, "", fmt.Errorf("%w: encode retry", ErrInvalid)
	}
	return encoded, decision.PolicyVersion, nil
}

func nullableJSON(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return string(value)
}

func wrapDatabaseError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %w", ErrConflict, err)
	}
	var databaseError *pq.Error
	if errors.As(err, &databaseError) {
		switch databaseError.Code {
		case "23503", "23505", "23514":
			return fmt.Errorf("%w: %w", ErrConflict, err)
		}
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}

var _ Store = (*PostgresStore)(nil)
