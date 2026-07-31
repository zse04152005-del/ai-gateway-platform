package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/lib/pq"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
)

const requestColumns = `
	id, tenant_id, project_id, virtual_key_id, logical_model, trace_id, span_id,
	status, attempt_count, started_at, ended_at, end_reason, version, updated_at`

const attemptColumns = `
	id, request_id, attempt_no, deployment_id, status, started_at,
	headers_received_at, first_byte_at, ended_at, end_reason, provider_request_id,
	error_category, error_code, usage_summary, version, updated_at`

// Recorder is the gateway application port for durable request/attempt facts.
type Recorder interface {
	StartRequest(context.Context, StartRequest) (GatewayRequest, error)
	MarkRouting(context.Context, GatewayRequest) (GatewayRequest, error)
	FailRequest(context.Context, GatewayRequest, RequestStatus, string) (GatewayRequest, error)
	StartAttempt(context.Context, GatewayRequest, string) (GatewayRequest, RouteAttempt, error)
	MarkAttemptStreaming(context.Context, GatewayRequest, RouteAttempt, string) (RouteAttempt, error)
	CompleteAttemptForRetry(context.Context, GatewayRequest, RouteAttempt, AttemptOutcome) (RouteAttempt, error)
	CompleteAttempt(context.Context, GatewayRequest, RouteAttempt, AttemptOutcome) (GatewayRequest, RouteAttempt, error)
}

// PostgresRecorder uses optimistic state/version conditions and transactional attempt boundaries.
type PostgresRecorder struct {
	database *sql.DB
	now      func() time.Time
	random   io.Reader
}

// NewPostgresRecorder validates process-scoped dependencies.
func NewPostgresRecorder(database *sql.DB, now func() time.Time, random io.Reader) (*PostgresRecorder, error) {
	if database == nil {
		return nil, errors.New("execution recorder database must not be nil")
	}
	if now == nil || now().IsZero() {
		return nil, errors.New("execution recorder clock must return a non-zero time")
	}
	if random == nil {
		return nil, errors.New("execution recorder random source must not be nil")
	}
	return &PostgresRecorder{database: database, now: now, random: random}, nil
}

// StartRequest durably records the authenticated request before routing.
func (recorder *PostgresRecorder) StartRequest(ctx context.Context, start StartRequest) (GatewayRequest, error) {
	if recorder == nil || recorder.database == nil || recorder.now == nil || ctx == nil || start.Validate() != nil {
		return GatewayRequest{}, ErrInvalid
	}
	now := recorder.now().UTC()
	request, err := scanRequest(recorder.database.QueryRowContext(ctx, `
		INSERT INTO app.gateway_requests (
			id, tenant_id, project_id, virtual_key_id, logical_model, trace_id, span_id,
			status, attempt_count, started_at, version, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, 'authorized', 0, $8, 1, $8)
		RETURNING `+requestColumns,
		start.ID, start.TenantID, start.ProjectID, start.VirtualKeyID,
		start.LogicalModel, start.TraceID, start.SpanID, now,
	))
	if err != nil {
		return GatewayRequest{}, mapDatabaseError(err)
	}
	return request, nil
}

// MarkRouting performs AUTHORIZED -> ROUTING with a compare-and-swap.
func (recorder *PostgresRecorder) MarkRouting(ctx context.Context, request GatewayRequest) (GatewayRequest, error) {
	if recorder == nil || recorder.database == nil || recorder.now == nil || ctx == nil ||
		validateRequestHandle(request, RequestAuthorized) != nil {
		return GatewayRequest{}, ErrInvalid
	}
	updated, err := scanRequest(recorder.database.QueryRowContext(ctx, `
		UPDATE app.gateway_requests
		SET status = 'routing', version = version + 1, updated_at = $3
		WHERE id = $1 AND status = 'authorized' AND version = $2
		RETURNING `+requestColumns,
		request.ID, request.Version, recorder.now().UTC(),
	))
	if err != nil {
		return GatewayRequest{}, mapDatabaseError(err)
	}
	return updated, nil
}

// FailRequest terminates a request before or without a completed attempt.
func (recorder *PostgresRecorder) FailRequest(
	ctx context.Context,
	request GatewayRequest,
	status RequestStatus,
	reason string,
) (GatewayRequest, error) {
	if recorder == nil || recorder.database == nil || recorder.now == nil || ctx == nil ||
		validateRequestHandle(request, RequestAuthorized, RequestRouting, RequestRunning) != nil ||
		(status != RequestFailed && status != RequestCancelled) || !reasonPattern.MatchString(reason) {
		return GatewayRequest{}, ErrInvalid
	}
	now := recorder.now().UTC()
	updated, err := scanRequest(recorder.database.QueryRowContext(ctx, `
		UPDATE app.gateway_requests
		SET status = $3, ended_at = $4, end_reason = $5,
			version = version + 1, updated_at = $4
		WHERE id = $1 AND status = $2 AND version = $6
		RETURNING `+requestColumns,
		request.ID, request.Status, status, now, reason, request.Version,
	))
	if err != nil {
		return GatewayRequest{}, mapDatabaseError(err)
	}
	return updated, nil
}

// StartAttempt atomically moves the Request to RUNNING and creates CREATED -> CONNECTING evidence.
func (recorder *PostgresRecorder) StartAttempt(
	ctx context.Context,
	request GatewayRequest,
	deploymentID string,
) (GatewayRequest, RouteAttempt, error) {
	if recorder == nil || recorder.database == nil || recorder.now == nil || recorder.random == nil || ctx == nil ||
		validateRequestHandle(request, RequestRouting, RequestRunning) != nil || !uuidPattern.MatchString(deploymentID) {
		return GatewayRequest{}, RouteAttempt{}, ErrInvalid
	}
	attemptID, err := newUUID(recorder.random)
	if err != nil {
		return GatewayRequest{}, RouteAttempt{}, newRecordError(ErrUnavailable, err)
	}
	now := recorder.now().UTC()
	transaction, err := recorder.database.BeginTx(ctx, nil)
	if err != nil {
		return GatewayRequest{}, RouteAttempt{}, newRecordError(ErrUnavailable, err)
	}
	defer func() { _ = transaction.Rollback() }()

	updatedRequest, err := scanRequest(transaction.QueryRowContext(ctx, `
		UPDATE app.gateway_requests
		SET status = 'running', attempt_count = attempt_count + 1,
			version = version + 1, updated_at = $4
		WHERE id = $1 AND status = $2 AND version = $3 AND attempt_count = $5
		RETURNING `+requestColumns,
		request.ID, request.Status, request.Version, now, request.AttemptCount,
	))
	if err != nil {
		return GatewayRequest{}, RouteAttempt{}, mapDatabaseError(err)
	}
	attemptNo := request.AttemptCount + 1
	if _, err = transaction.ExecContext(ctx, `
		INSERT INTO app.route_attempts (
			id, request_id, attempt_no, deployment_id, status,
			started_at, version, updated_at
		) VALUES ($1, $2, $3, $4, 'created', $5, 1, $5)`,
		attemptID, request.ID, attemptNo, deploymentID, now,
	); err != nil {
		return GatewayRequest{}, RouteAttempt{}, mapDatabaseError(err)
	}
	attempt, err := scanAttempt(transaction.QueryRowContext(ctx, `
		UPDATE app.route_attempts
		SET status = 'connecting', version = 2, updated_at = $2
		WHERE id = $1 AND status = 'created' AND version = 1
		RETURNING `+attemptColumns,
		attemptID, now,
	))
	if err != nil {
		return GatewayRequest{}, RouteAttempt{}, mapDatabaseError(err)
	}
	if err := transaction.Commit(); err != nil {
		return GatewayRequest{}, RouteAttempt{}, newRecordError(ErrUnavailable, err)
	}
	return updatedRequest, attempt, nil
}

// MarkAttemptStreaming atomically records the first client-visible model output boundary.
func (recorder *PostgresRecorder) MarkAttemptStreaming(
	ctx context.Context,
	request GatewayRequest,
	attempt RouteAttempt,
	providerRequestID string,
) (RouteAttempt, error) {
	if recorder == nil || recorder.database == nil || recorder.now == nil || ctx == nil ||
		validateRequestHandle(request, RequestRunning) != nil ||
		validateAttemptHandle(attempt, AttemptConnecting) != nil || attempt.RequestID != request.ID ||
		(providerRequestID != "" && !providerRequestIDPattern.MatchString(providerRequestID)) {
		return RouteAttempt{}, ErrInvalid
	}
	headerObservedAt := recorder.now().UTC()
	firstByteAt := recorder.now().UTC()
	if firstByteAt.Before(headerObservedAt) {
		firstByteAt = headerObservedAt
	}
	transaction, err := recorder.database.BeginTx(ctx, nil)
	if err != nil {
		return RouteAttempt{}, newRecordError(ErrUnavailable, err)
	}
	defer func() { _ = transaction.Rollback() }()

	headersReceived, err := scanAttempt(transaction.QueryRowContext(ctx, `
		UPDATE app.route_attempts
		SET status = 'headers_received', headers_received_at = $4,
			provider_request_id = NULLIF($5, ''), version = version + 1, updated_at = $4
		WHERE id = $1 AND request_id = $2 AND status = 'connecting' AND version = $3
		RETURNING `+attemptColumns,
		attempt.ID, request.ID, attempt.Version, headerObservedAt, providerRequestID,
	))
	if err != nil {
		return RouteAttempt{}, mapDatabaseError(err)
	}
	streaming, err := scanAttempt(transaction.QueryRowContext(ctx, `
		UPDATE app.route_attempts
		SET status = 'streaming', first_byte_at = $4,
			version = version + 1, updated_at = $4
		WHERE id = $1 AND request_id = $2 AND status = 'headers_received' AND version = $3
		RETURNING `+attemptColumns,
		headersReceived.ID, request.ID, headersReceived.Version, firstByteAt,
	))
	if err != nil {
		return RouteAttempt{}, mapDatabaseError(err)
	}
	if err := transaction.Commit(); err != nil {
		return RouteAttempt{}, newRecordError(ErrUnavailable, err)
	}
	return streaming, nil
}

// CompleteAttempt atomically records a terminal attempt state and its parent terminal state.
func (recorder *PostgresRecorder) CompleteAttempt(
	ctx context.Context,
	request GatewayRequest,
	attempt RouteAttempt,
	outcome AttemptOutcome,
) (GatewayRequest, RouteAttempt, error) {
	if recorder == nil || recorder.database == nil || recorder.now == nil || ctx == nil ||
		validateRequestHandle(request, RequestRunning) != nil ||
		validateAttemptCompletion(attempt, outcome) != nil || attempt.RequestID != request.ID {
		return GatewayRequest{}, RouteAttempt{}, ErrInvalid
	}
	usageSummary, err := marshalUsageSummary(outcome.Usage)
	if err != nil {
		return GatewayRequest{}, RouteAttempt{}, err
	}
	now := recorder.now().UTC()
	transaction, err := recorder.database.BeginTx(ctx, nil)
	if err != nil {
		return GatewayRequest{}, RouteAttempt{}, newRecordError(ErrUnavailable, err)
	}
	defer func() { _ = transaction.Rollback() }()

	completedAttempt, err := completeAttemptInTransaction(
		ctx, transaction, request, attempt, outcome, usageSummary, now,
	)
	if err != nil {
		return GatewayRequest{}, RouteAttempt{}, err
	}
	completedRequest, err := scanRequest(transaction.QueryRowContext(ctx, `
		UPDATE app.gateway_requests
		SET status = $3, ended_at = $4, end_reason = $5,
			version = version + 1, updated_at = $4
		WHERE id = $1 AND status = 'running' AND version = $2
		RETURNING `+requestColumns,
		request.ID, request.Version, outcome.RequestStatus, now, outcome.EndReason,
	))
	if err != nil {
		return GatewayRequest{}, RouteAttempt{}, mapDatabaseError(err)
	}
	if err := transaction.Commit(); err != nil {
		return GatewayRequest{}, RouteAttempt{}, newRecordError(ErrUnavailable, err)
	}
	return completedRequest, completedAttempt, nil
}

// CompleteAttemptForRetry durably terminates one failed physical Attempt while
// deliberately keeping its parent Request RUNNING for a subsequent Attempt.
func (recorder *PostgresRecorder) CompleteAttemptForRetry(
	ctx context.Context,
	request GatewayRequest,
	attempt RouteAttempt,
	outcome AttemptOutcome,
) (RouteAttempt, error) {
	if recorder == nil || recorder.database == nil || recorder.now == nil || ctx == nil ||
		validateRequestHandle(request, RequestRunning) != nil ||
		validateRetryAttemptCompletion(attempt, outcome) != nil || attempt.RequestID != request.ID {
		return RouteAttempt{}, ErrInvalid
	}
	usageSummary, err := marshalUsageSummary(outcome.Usage)
	if err != nil {
		return RouteAttempt{}, err
	}
	now := recorder.now().UTC()
	transaction, err := recorder.database.BeginTx(ctx, nil)
	if err != nil {
		return RouteAttempt{}, newRecordError(ErrUnavailable, err)
	}
	defer func() { _ = transaction.Rollback() }()

	completedAttempt, err := completeAttemptInTransaction(
		ctx, transaction, request, attempt, outcome, usageSummary, now,
	)
	if err != nil {
		return RouteAttempt{}, err
	}
	if err := transaction.Commit(); err != nil {
		return RouteAttempt{}, newRecordError(ErrUnavailable, err)
	}
	return completedAttempt, nil
}

func completeAttemptInTransaction(
	ctx context.Context,
	transaction *sql.Tx,
	request GatewayRequest,
	attempt RouteAttempt,
	outcome AttemptOutcome,
	usageSummary []byte,
	now time.Time,
) (RouteAttempt, error) {
	currentAttempt := attempt
	var err error
	if attempt.Status == AttemptConnecting && outcome.HeadersReceived {
		currentAttempt, err = scanAttempt(transaction.QueryRowContext(ctx, `
			UPDATE app.route_attempts
			SET status = 'headers_received', headers_received_at = $4,
				provider_request_id = NULLIF($5, ''), version = version + 1, updated_at = $4
			WHERE id = $1 AND request_id = $2 AND status = 'connecting' AND version = $3
			RETURNING `+attemptColumns,
			attempt.ID, request.ID, attempt.Version, now, outcome.ProviderRequestID,
		))
		if err != nil {
			return RouteAttempt{}, mapDatabaseError(err)
		}
	}
	completedAttempt, err := scanAttempt(transaction.QueryRowContext(ctx, `
		UPDATE app.route_attempts
		SET status = $4, ended_at = $5, end_reason = $6,
			error_category = NULLIF($7, ''), error_code = NULLIF($8, ''),
			usage_summary = $9::jsonb,
			provider_request_id = COALESCE(provider_request_id, NULLIF($10, '')),
			version = version + 1, updated_at = $5
		WHERE id = $1 AND request_id = $2 AND status = $3 AND version = $11
		RETURNING `+attemptColumns,
		currentAttempt.ID, request.ID, currentAttempt.Status, outcome.AttemptStatus,
		now, outcome.EndReason, outcome.ErrorCategory, outcome.ErrorCode,
		nullableJSON(usageSummary), outcome.ProviderRequestID, currentAttempt.Version,
	))
	if err != nil {
		return RouteAttempt{}, mapDatabaseError(err)
	}
	return completedAttempt, nil
}

func validateAttemptCompletion(attempt RouteAttempt, outcome AttemptOutcome) error {
	if outcome.Validate() != nil {
		return ErrInvalid
	}
	if outcome.RequestStatus == RequestRunning {
		return ErrInvalid
	}
	return validateAttemptStateCompletion(attempt, outcome)
}

func validateRetryAttemptCompletion(attempt RouteAttempt, outcome AttemptOutcome) error {
	if outcome.Validate() != nil || outcome.AttemptStatus != AttemptRetryableFailed ||
		outcome.RequestStatus != RequestRunning {
		return ErrInvalid
	}
	return validateAttemptStateCompletion(attempt, outcome)
}

func validateAttemptStateCompletion(attempt RouteAttempt, outcome AttemptOutcome) error {
	switch attempt.Status {
	case AttemptConnecting:
		if validateAttemptHandle(attempt, AttemptConnecting) != nil || outcome.AttemptStatus == AttemptPartialFailed {
			return ErrInvalid
		}
	case AttemptStreaming:
		if validateAttemptHandle(attempt, AttemptStreaming) != nil || attempt.HeadersReceivedAt == nil ||
			attempt.FirstByteAt == nil || !outcome.HeadersReceived {
			return ErrInvalid
		}
		switch outcome.AttemptStatus {
		case AttemptSucceeded, AttemptPartialFailed, AttemptCancelled:
		case AttemptCreated, AttemptConnecting, AttemptHeadersReceived, AttemptStreaming,
			AttemptRetryableFailed, AttemptFailed:
			return ErrInvalid
		default:
			return ErrInvalid
		}
		if attempt.ProviderRequestID != "" && outcome.ProviderRequestID != "" &&
			attempt.ProviderRequestID != outcome.ProviderRequestID {
			return ErrInvalid
		}
	case AttemptCreated, AttemptHeadersReceived, AttemptSucceeded, AttemptRetryableFailed,
		AttemptFailed, AttemptPartialFailed, AttemptCancelled:
		return ErrInvalid
	default:
		return ErrInvalid
	}
	return nil
}

type rowScanner interface {
	Scan(...any) error
}

func scanRequest(scanner rowScanner) (GatewayRequest, error) {
	var (
		request   GatewayRequest
		status    string
		endedAt   sql.NullTime
		endReason sql.NullString
	)
	if err := scanner.Scan(
		&request.ID, &request.TenantID, &request.ProjectID, &request.VirtualKeyID,
		&request.LogicalModel, &request.TraceID, &request.SpanID, &status,
		&request.AttemptCount, &request.StartedAt, &endedAt, &endReason,
		&request.Version, &request.UpdatedAt,
	); err != nil {
		return GatewayRequest{}, err
	}
	request.Status = RequestStatus(status)
	if endedAt.Valid {
		request.EndedAt = &endedAt.Time
	}
	if endReason.Valid {
		request.EndReason = endReason.String
	}
	return request, nil
}

func scanAttempt(scanner rowScanner) (RouteAttempt, error) {
	var (
		attempt           RouteAttempt
		status            string
		headersReceivedAt sql.NullTime
		firstByteAt       sql.NullTime
		endedAt           sql.NullTime
		endReason         sql.NullString
		providerRequestID sql.NullString
		errorCategory     sql.NullString
		errorCode         sql.NullString
		usageSummary      []byte
	)
	if err := scanner.Scan(
		&attempt.ID, &attempt.RequestID, &attempt.AttemptNo, &attempt.DeploymentID,
		&status, &attempt.StartedAt, &headersReceivedAt, &firstByteAt, &endedAt,
		&endReason, &providerRequestID, &errorCategory, &errorCode, &usageSummary,
		&attempt.Version, &attempt.UpdatedAt,
	); err != nil {
		return RouteAttempt{}, err
	}
	attempt.Status = AttemptStatus(status)
	if headersReceivedAt.Valid {
		attempt.HeadersReceivedAt = &headersReceivedAt.Time
	}
	if firstByteAt.Valid {
		attempt.FirstByteAt = &firstByteAt.Time
	}
	if endedAt.Valid {
		attempt.EndedAt = &endedAt.Time
	}
	if endReason.Valid {
		attempt.EndReason = endReason.String
	}
	if providerRequestID.Valid {
		attempt.ProviderRequestID = providerRequestID.String
	}
	if errorCategory.Valid {
		attempt.ErrorCategory = errorCategory.String
	}
	if errorCode.Valid {
		attempt.ErrorCode = errorCode.String
	}
	attempt.UsageSummary = append(json.RawMessage(nil), usageSummary...)
	return attempt, nil
}

type usageSummary struct {
	InputTokens       *int64              `json:"input_tokens,omitempty"`
	OutputTokens      *int64              `json:"output_tokens,omitempty"`
	CacheReadTokens   *int64              `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens  *int64              `json:"cache_write_tokens,omitempty"`
	ReasoningTokens   *int64              `json:"reasoning_tokens,omitempty"`
	AudioInputTokens  *int64              `json:"audio_input_tokens,omitempty"`
	AudioOutputTokens *int64              `json:"audio_output_tokens,omitempty"`
	Source            adapter.UsageSource `json:"source"`
	Complete          bool                `json:"complete"`
}

func marshalUsageSummary(usage *adapter.NormalizedUsage) ([]byte, error) {
	if usage == nil {
		return nil, nil
	}
	if err := usage.Validate(); err != nil {
		return nil, ErrInvalid
	}
	summary := usageSummary{
		InputTokens: tokenValue(usage.InputTokens), OutputTokens: tokenValue(usage.OutputTokens),
		CacheReadTokens: tokenValue(usage.CacheReadTokens), CacheWriteTokens: tokenValue(usage.CacheWriteTokens),
		ReasoningTokens: tokenValue(usage.ReasoningTokens), AudioInputTokens: tokenValue(usage.AudioInputTokens),
		AudioOutputTokens: tokenValue(usage.AudioOutputTokens), Source: usage.Source, Complete: usage.Complete,
	}
	encoded, err := json.Marshal(summary)
	if err != nil {
		return nil, newRecordError(ErrInvalid, err)
	}
	return encoded, nil
}

func tokenValue(count adapter.TokenCount) *int64 {
	if !count.Present {
		return nil
	}
	value := count.Value
	return &value
}

func nullableJSON(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return string(value)
}

func newUUID(reader io.Reader) (string, error) {
	raw := make([]byte, 16)
	if _, err := io.ReadFull(reader, raw); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf(
		"%08x-%04x-%04x-%04x-%012x",
		raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16],
	), nil
}

type recordError struct {
	kind  error
	cause error
}

func newRecordError(kind, cause error) error {
	if cause == nil {
		return kind
	}
	return &recordError{kind: kind, cause: cause}
}

func (failure *recordError) Error() string {
	if failure == nil || failure.kind == nil {
		return "execution record failed"
	}
	return failure.kind.Error()
}

func (failure *recordError) Unwrap() []error {
	if failure == nil {
		return nil
	}
	return []error{failure.kind, failure.cause}
}

func mapDatabaseError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return newRecordError(ErrConflict, err)
	}
	var databaseError *pq.Error
	if errors.As(err, &databaseError) {
		switch databaseError.Code {
		case "23503", "23505", "23514":
			return newRecordError(ErrConflict, err)
		}
	}
	return newRecordError(ErrUnavailable, err)
}

var _ Recorder = (*PostgresRecorder)(nil)
