package meteringadjustment

import (
	"context"
	"database/sql"
	"errors"
	"math/big"
	"time"

	"github.com/lib/pq"

	"github.com/zse04152005-del/ai-gateway-platform/internal/metering"
)

// Writer appends one signed correction while preserving the referenced fact.
type Writer interface {
	Apply(context.Context, Command) (Result, error)
}

// PostgresWriter serializes corrections by locking their immutable target row.
type PostgresWriter struct {
	database *sql.DB
	now      func() time.Time
}

// NewPostgresWriter validates the durable store and authoritative clock.
func NewPostgresWriter(database *sql.DB, now func() time.Time) (*PostgresWriter, error) {
	if database == nil || now == nil || now().IsZero() {
		return nil, ErrInvalid
	}
	return &PostgresWriter{database: database, now: now}, nil
}

// Apply derives signed deltas from the current effective fact and appends one
// Adjustment. The original and every earlier correction remain immutable.
func (writer *PostgresWriter) Apply(ctx context.Context, command Command) (Result, error) {
	if writer == nil || writer.database == nil || writer.now == nil || ctx == nil || command.Validate() != nil {
		return Result{}, ErrInvalid
	}
	createdAt := writer.now().UTC()
	if createdAt.IsZero() {
		return Result{}, ErrStoreUnavailable
	}
	transaction, err := writer.database.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, newAdjustmentError(ErrStoreUnavailable, err)
	}
	defer func() { _ = transaction.Rollback() }()

	if existing, found, err := loadExisting(ctx, transaction, command); err != nil {
		return Result{}, err
	} else if found {
		if err := transaction.Commit(); err != nil {
			return Result{}, newAdjustmentError(ErrStoreUnavailable, err)
		}
		return existing, nil
	}

	target, err := lockTarget(ctx, transaction, command)
	if err != nil {
		return Result{}, err
	}
	// A concurrent replay blocks on the same target. Recheck after acquiring
	// the lock so only one row is appended for a shared idempotency key.
	if existing, found, err := loadExisting(ctx, transaction, command); err != nil {
		return Result{}, err
	} else if found {
		if err := transaction.Commit(); err != nil {
			return Result{}, newAdjustmentError(ErrStoreUnavailable, err)
		}
		return existing, nil
	}

	currentQuantity, currentAmount, err := loadCurrentResult(
		ctx, transaction, command.TargetEventID, target,
	)
	if err != nil {
		return Result{}, err
	}
	if currentQuantity == command.CorrectedQuantity && currentAmount == command.CorrectedAmountMicros {
		return Result{}, ErrNoChange
	}
	quantityDelta := command.CorrectedQuantity - currentQuantity
	amountDelta := command.CorrectedAmountMicros - currentAmount
	attemptID := nullableString(target.attemptID)
	_, err = transaction.ExecContext(ctx, `
		INSERT INTO app.usage_ledger_entries (
			event_id, tenant_id, request_id, attempt_id, token_type,
			quantity, source, observed_at, created_at, created_by,
			price_version_id, amount_micros, event_schema_version,
			adjusts_event_id, adjustment_idempotency_key, adjustment_origin,
			adjustment_reason, adjustment_reference, adjustment_actor,
			adjustment_result_quantity, adjustment_result_amount_micros
		) VALUES (
			$1, $2, $3, $4, $5, $6, 'adjustment', $7, $7, $8, $9, $10, NULL,
			$11, $12, $13, $14, $15, $16, $17, $18
		)`,
		command.EventID, target.tenantID, target.requestID, attemptID, target.tokenType,
		quantityDelta, createdAt, command.Actor, target.priceVersionID, amountDelta,
		command.TargetEventID, command.IdempotencyKey, command.Origin, command.Reason,
		command.Reference, command.Actor, command.CorrectedQuantity, command.CorrectedAmountMicros,
	)
	if err != nil {
		return Result{}, mapDatabaseError(err)
	}
	if err := transaction.Commit(); err != nil {
		return Result{}, newAdjustmentError(ErrStoreUnavailable, err)
	}
	return resultFrom(command, target, quantityDelta, amountDelta, createdAt, true, false), nil
}

type targetFact struct {
	tenantID       string
	requestID      string
	attemptID      string
	tokenType      metering.TokenType
	priceVersionID string
	currency       string
	source         string
	quantity       int64
	amountMicros   int64
}

func lockTarget(ctx context.Context, transaction *sql.Tx, command Command) (targetFact, error) {
	var target targetFact
	var attemptID sql.NullString
	err := transaction.QueryRowContext(ctx, `
		SELECT ledger.tenant_id::text, ledger.request_id, ledger.attempt_id::text,
			ledger.token_type, ledger.price_version_id::text, price.currency,
			ledger.source, ledger.quantity, ledger.amount_micros
		FROM app.usage_ledger_entries AS ledger
		JOIN app.gateway_requests AS request
		  ON request.tenant_id = ledger.tenant_id AND request.id = ledger.request_id
		JOIN app.price_versions AS price ON price.id = ledger.price_version_id
		WHERE ledger.event_id = $1 AND ledger.tenant_id = $2::uuid
		  AND request.project_id = $3::uuid
		FOR UPDATE OF ledger`, command.TargetEventID, command.Scope.TenantID, command.Scope.ProjectID,
	).Scan(
		&target.tenantID, &target.requestID, &attemptID, &target.tokenType,
		&target.priceVersionID, &target.currency, &target.source, &target.quantity, &target.amountMicros,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return targetFact{}, ErrNotFound
	}
	if err != nil {
		return targetFact{}, newAdjustmentError(ErrStoreUnavailable, err)
	}
	if attemptID.Valid {
		target.attemptID = attemptID.String
	}
	if target.source == string(metering.SourceAdjustment) || !target.tokenType.Valid() ||
		target.quantity < 1 || target.quantity > metering.MaximumExactInteger ||
		target.amountMicros < 0 || target.amountMicros > metering.MaximumExactInteger {
		return targetFact{}, ErrInvalidTarget
	}
	return target, nil
}

func loadCurrentResult(
	ctx context.Context,
	transaction *sql.Tx,
	targetEventID string,
	target targetFact,
) (int64, int64, error) {
	var quantityText, amountText string
	err := transaction.QueryRowContext(ctx, `
		SELECT ($2::numeric + COALESCE(sum(quantity), 0))::text,
			($3::numeric + COALESCE(sum(amount_micros), 0))::text
		FROM app.usage_ledger_entries
		WHERE adjusts_event_id = $1`, targetEventID, target.quantity, target.amountMicros,
	).Scan(&quantityText, &amountText)
	if err != nil {
		return 0, 0, newAdjustmentError(ErrStoreUnavailable, err)
	}
	quantity, validQuantity := exactNonnegative(quantityText)
	amount, validAmount := exactNonnegative(amountText)
	if !validQuantity || !validAmount {
		return 0, 0, ErrInvalidTarget
	}
	return quantity, amount, nil
}

func loadExisting(
	ctx context.Context,
	transaction *sql.Tx,
	command Command,
) (Result, bool, error) {
	var result Result
	var attemptID sql.NullString
	var idempotencyKey string
	err := transaction.QueryRowContext(ctx, `
		SELECT ledger.event_id::text, ledger.adjusts_event_id::text,
			ledger.tenant_id::text, ledger.request_id, ledger.attempt_id::text,
			ledger.token_type, ledger.price_version_id::text, price.currency,
			ledger.quantity, ledger.amount_micros,
			ledger.adjustment_result_quantity, ledger.adjustment_result_amount_micros,
			ledger.adjustment_origin, ledger.adjustment_reason,
			ledger.adjustment_reference, ledger.adjustment_actor,
			ledger.adjustment_idempotency_key, ledger.created_at
		FROM app.usage_ledger_entries AS ledger
		JOIN app.gateway_requests AS request
		  ON request.tenant_id = ledger.tenant_id AND request.id = ledger.request_id
		JOIN app.price_versions AS price ON price.id = ledger.price_version_id
		WHERE ledger.tenant_id = $1::uuid AND request.project_id = $2::uuid
		  AND ledger.adjustment_idempotency_key = $3`,
		command.Scope.TenantID, command.Scope.ProjectID, command.IdempotencyKey,
	).Scan(
		&result.EventID, &result.TargetEventID, &result.TenantID, &result.RequestID, &attemptID,
		&result.TokenType, &result.PriceVersionID, &result.Currency,
		&result.QuantityDelta, &result.AmountMicrosDelta,
		&result.CorrectedQuantity, &result.CorrectedAmountMicros,
		&result.Origin, &result.Reason, &result.Reference, &result.Actor,
		&idempotencyKey, &result.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, newAdjustmentError(ErrStoreUnavailable, err)
	}
	if attemptID.Valid {
		result.AttemptID = attemptID.String
	}
	if result.EventID != command.EventID || result.TargetEventID != command.TargetEventID ||
		idempotencyKey != command.IdempotencyKey || result.CorrectedQuantity != command.CorrectedQuantity ||
		result.CorrectedAmountMicros != command.CorrectedAmountMicros || result.Origin != command.Origin ||
		result.Reason != command.Reason || result.Reference != command.Reference || result.Actor != command.Actor {
		return Result{}, false, ErrConflict
	}
	result.Replayed = true
	return result, true, nil
}

func resultFrom(
	command Command,
	target targetFact,
	quantityDelta int64,
	amountDelta int64,
	createdAt time.Time,
	inserted bool,
	replayed bool,
) Result {
	return Result{
		EventID: command.EventID, TargetEventID: command.TargetEventID,
		TenantID: target.tenantID, RequestID: target.requestID, AttemptID: target.attemptID,
		TokenType: target.tokenType, PriceVersionID: target.priceVersionID, Currency: target.currency,
		QuantityDelta: quantityDelta, AmountMicrosDelta: amountDelta,
		CorrectedQuantity: command.CorrectedQuantity, CorrectedAmountMicros: command.CorrectedAmountMicros,
		Origin: command.Origin, Reason: command.Reason, Reference: command.Reference, Actor: command.Actor,
		CreatedAt: createdAt, Inserted: inserted, Replayed: replayed,
	}
}

func exactNonnegative(value string) (int64, bool) {
	parsed, ok := new(big.Int).SetString(value, 10)
	if !ok || parsed.Sign() < 0 || parsed.Cmp(big.NewInt(metering.MaximumExactInteger)) > 0 {
		return 0, false
	}
	return parsed.Int64(), true
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func mapDatabaseError(err error) error {
	var databaseError *pq.Error
	if errors.As(err, &databaseError) {
		switch databaseError.Code {
		case "23505":
			return newAdjustmentError(ErrConflict, err)
		case "23503", "23514":
			return newAdjustmentError(ErrInvalidTarget, err)
		}
	}
	return newAdjustmentError(ErrStoreUnavailable, err)
}

type adjustmentError struct {
	kind  error
	cause error
}

func newAdjustmentError(kind, cause error) error {
	if cause == nil {
		return kind
	}
	return &adjustmentError{kind: kind, cause: cause}
}

func (failure *adjustmentError) Error() string {
	if failure == nil || failure.kind == nil {
		return "metering adjustment failed"
	}
	return failure.kind.Error()
}

func (failure *adjustmentError) Unwrap() []error {
	if failure == nil {
		return nil
	}
	return []error{failure.kind, failure.cause}
}

var _ Writer = (*PostgresWriter)(nil)
