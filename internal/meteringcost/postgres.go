package meteringcost

import (
	"context"
	"database/sql"
	"errors"

	"github.com/zse04152005-del/ai-gateway-platform/internal/execution"
	"github.com/zse04152005-del/ai-gateway-platform/internal/meteringadjustment"
)

// Aggregator returns a complete request projection or ErrPending; it never
// returns a partial monetary total while durable Outbox facts are unpriced.
type Aggregator interface {
	Aggregate(context.Context, Scope, string) (RequestCost, error)
}

// PostgresAggregator rebuilds cost from append-only Ledger rows in one repeatable snapshot.
type PostgresAggregator struct {
	database *sql.DB
}

// NewPostgresAggregator validates the durable read dependency.
func NewPostgresAggregator(database *sql.DB) (*PostgresAggregator, error) {
	if database == nil {
		return nil, errors.New("metering cost database must not be nil")
	}
	return &PostgresAggregator{database: database}, nil
}

// Aggregate verifies terminal execution and Outbox completeness before adding
// every Attempt and request-level Ledger row, separated by currency.
func (aggregator *PostgresAggregator) Aggregate(
	ctx context.Context,
	scope Scope,
	requestID string,
) (RequestCost, error) {
	if aggregator == nil || aggregator.database == nil || ctx == nil ||
		scope.Validate() != nil || !requestIDPattern.MatchString(requestID) {
		return RequestCost{}, ErrInvalid
	}
	transaction, err := aggregator.database.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return RequestCost{}, newCostError(ErrUnavailable, err)
	}
	defer func() { _ = transaction.Rollback() }()

	status, attemptCount, err := loadRequest(ctx, transaction, scope, requestID)
	if err != nil {
		return RequestCost{}, err
	}
	if !terminalRequestStatus(status) {
		return RequestCost{}, ErrNotTerminal
	}
	attempts, err := loadAttempts(ctx, transaction, requestID)
	if err != nil {
		return RequestCost{}, err
	}
	complete, err := outboxComplete(ctx, transaction, scope.TenantID, requestID)
	if err != nil {
		return RequestCost{}, err
	}
	if !complete {
		return RequestCost{}, ErrPending
	}
	entries, err := loadLedger(ctx, transaction, scope.TenantID, requestID)
	if err != nil {
		return RequestCost{}, err
	}
	result, err := buildRequestCost(scope, requestID, status, attemptCount, attempts, entries)
	if err != nil {
		return RequestCost{}, err
	}
	if err := transaction.Commit(); err != nil {
		return RequestCost{}, newCostError(ErrUnavailable, err)
	}
	return result, nil
}

func loadRequest(
	ctx context.Context,
	transaction *sql.Tx,
	scope Scope,
	requestID string,
) (execution.RequestStatus, int, error) {
	var status string
	var attemptCount int
	err := transaction.QueryRowContext(ctx, `
		SELECT status, attempt_count
		FROM app.gateway_requests
		WHERE id = $1 AND tenant_id = $2::uuid AND project_id = $3::uuid`,
		requestID, scope.TenantID, scope.ProjectID,
	).Scan(&status, &attemptCount)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, ErrNotFound
	}
	if err != nil {
		return "", 0, newCostError(ErrUnavailable, err)
	}
	return execution.RequestStatus(status), attemptCount, nil
}

func loadAttempts(ctx context.Context, transaction *sql.Tx, requestID string) ([]attemptFact, error) {
	rows, err := transaction.QueryContext(ctx, `
		SELECT id::text, attempt_no, deployment_id::text, status
		FROM app.route_attempts
		WHERE request_id = $1
		ORDER BY attempt_no`, requestID)
	if err != nil {
		return nil, newCostError(ErrUnavailable, err)
	}
	defer func() { _ = rows.Close() }()
	attempts := make([]attemptFact, 0)
	for rows.Next() {
		var fact attemptFact
		var status string
		if err = rows.Scan(&fact.id, &fact.number, &fact.deploymentID, &status); err != nil {
			return nil, newCostError(ErrUnavailable, err)
		}
		fact.status = execution.AttemptStatus(status)
		attempts = append(attempts, fact)
	}
	if err = rows.Err(); err != nil {
		return nil, newCostError(ErrUnavailable, err)
	}
	return attempts, nil
}

func outboxComplete(
	ctx context.Context,
	transaction *sql.Tx,
	tenantID string,
	requestID string,
) (bool, error) {
	var expected, priced int64
	err := transaction.QueryRowContext(ctx, `
		SELECT count(*), count(ledger.event_id)
		FROM app.usage_event_outbox AS outbox
		LEFT JOIN app.usage_ledger_entries AS ledger
		  ON ledger.event_id = outbox.event_id
		 AND ledger.tenant_id = outbox.tenant_id
		 AND ledger.request_id = outbox.request_id
		 AND ledger.attempt_id = outbox.attempt_id
		WHERE outbox.tenant_id = $1::uuid AND outbox.request_id = $2`,
		tenantID, requestID,
	).Scan(&expected, &priced)
	if err != nil {
		return false, newCostError(ErrUnavailable, err)
	}
	if expected < 0 || priced < 0 || priced > expected {
		return false, ErrUnavailable
	}
	return expected == priced, nil
}

func loadLedger(
	ctx context.Context,
	transaction *sql.Tx,
	tenantID string,
	requestID string,
) ([]ledgerFact, error) {
	rows, err := transaction.QueryContext(ctx, `
		SELECT ledger.event_id::text, ledger.attempt_id::text,
			ledger.token_type, ledger.quantity, ledger.source,
			ledger.observed_at, ledger.created_at,
			ledger.price_version_id::text, price.currency,
			rate.billing_unit, rate.unit_quantity, rate.unit_price_micros,
			ledger.amount_micros, ledger.adjusts_event_id::text,
			ledger.adjustment_origin, ledger.adjustment_reason,
			ledger.adjustment_reference, ledger.adjustment_actor,
			ledger.adjustment_result_quantity, ledger.adjustment_result_amount_micros
		FROM app.usage_ledger_entries AS ledger
		JOIN app.price_versions AS price ON price.id = ledger.price_version_id
		JOIN app.price_version_rates AS rate
		  ON rate.price_version_id = ledger.price_version_id
		 AND rate.token_type = ledger.token_type
		WHERE ledger.tenant_id = $1::uuid AND ledger.request_id = $2
		ORDER BY ledger.attempt_id NULLS FIRST, ledger.id`, tenantID, requestID)
	if err != nil {
		return nil, newCostError(ErrUnavailable, err)
	}
	defer func() { _ = rows.Close() }()
	entries := make([]ledgerFact, 0)
	for rows.Next() {
		var fact ledgerFact
		var attemptID sql.NullString
		var targetEventID, origin, reason, reference, actor sql.NullString
		var correctedQuantity, correctedAmount sql.NullInt64
		if err = rows.Scan(
			&fact.eventID, &attemptID, &fact.tokenType, &fact.quantity, &fact.source,
			&fact.observedAt, &fact.createdAt, &fact.priceVersionID, &fact.currency,
			&fact.billingUnit, &fact.unitQuantity, &fact.unitPriceMicros, &fact.amountMicros,
			&targetEventID, &origin, &reason, &reference, &actor,
			&correctedQuantity, &correctedAmount,
		); err != nil {
			return nil, newCostError(ErrUnavailable, err)
		}
		if attemptID.Valid {
			fact.attemptID = attemptID.String
		}
		if targetEventID.Valid || origin.Valid || reason.Valid || reference.Valid || actor.Valid ||
			correctedQuantity.Valid || correctedAmount.Valid {
			fact.adjustment = &LedgerAdjustment{
				TargetEventID: targetEventID.String,
				Origin:        meteringadjustment.Origin(origin.String),
				Reason:        reason.String, Reference: reference.String, Actor: actor.String,
				CorrectedQuantity:     correctedQuantity.Int64,
				CorrectedAmountMicros: correctedAmount.Int64,
			}
		}
		entries = append(entries, fact)
	}
	if err = rows.Err(); err != nil {
		return nil, newCostError(ErrUnavailable, err)
	}
	return entries, nil
}

type costError struct {
	kind  error
	cause error
}

func newCostError(kind, cause error) error {
	if cause == nil {
		return kind
	}
	return &costError{kind: kind, cause: cause}
}

func (failure *costError) Error() string {
	if failure == nil || failure.kind == nil {
		return "metering cost aggregation failed"
	}
	return failure.kind.Error()
}

func (failure *costError) Unwrap() []error {
	if failure == nil {
		return nil
	}
	return []error{failure.kind, failure.cause}
}

var _ Aggregator = (*PostgresAggregator)(nil)
