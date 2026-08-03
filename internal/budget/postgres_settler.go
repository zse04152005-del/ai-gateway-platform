package budget

import (
	"context"
	"database/sql"
	"errors"
)

// Settler atomically reconciles one pending hold into terminal cost facts.
type Settler interface {
	Settle(context.Context, SettlementInput) (SettlementResult, error)
}

// PostgresSettler uses bounded account-version CAS transactions.
type PostgresSettler struct {
	database *sql.DB
	options  SettlementOptions
}

// NewPostgresSettler validates process-scoped settlement dependencies.
func NewPostgresSettler(database *sql.DB, options SettlementOptions) (*PostgresSettler, error) {
	if database == nil {
		return nil, errors.New("budget settlement database must not be nil")
	}
	normalized, err := options.normalized()
	if err != nil {
		return nil, err
	}
	return &PostgresSettler{database: database, options: normalized}, nil
}

// Settle updates Account, Reservation and Ledger in one transaction. Terminal
// replays return the original facts only when outcome and actual cost agree.
func (settler *PostgresSettler) Settle(ctx context.Context, input SettlementInput) (SettlementResult, error) {
	if settler == nil || settler.database == nil || ctx == nil || input.Validate() != nil {
		return SettlementResult{}, ErrInvalid
	}
	actual, err := input.ActualMicros()
	if err != nil {
		return SettlementResult{}, err
	}
	for attempt := 1; attempt <= settler.options.MaxAttempts; attempt++ {
		result, conflict, settleErr := settler.settleOnce(ctx, input, actual, attempt)
		if settleErr != nil {
			return SettlementResult{}, settleErr
		}
		if !conflict {
			return result, nil
		}
		if attempt == settler.options.MaxAttempts {
			return SettlementResult{}, ErrRetryExhausted
		}
		if settleErr = waitForReserveRetry(ctx, settler.options.RetryDelay); settleErr != nil {
			return SettlementResult{}, newReserveError(ErrUnavailable, settleErr)
		}
	}
	return SettlementResult{}, ErrRetryExhausted
}

func (settler *PostgresSettler) settleOnce(
	ctx context.Context,
	input SettlementInput,
	actual uint64,
	attempt int,
) (SettlementResult, bool, error) {
	transaction, err := settler.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return SettlementResult{}, false, newReserveError(ErrUnavailable, err)
	}
	defer func() { _ = transaction.Rollback() }()

	reservation, err := loadBudgetReservation(ctx, transaction, input.TenantID, input.AccountID, input.ReservationID)
	if errors.Is(err, sql.ErrNoRows) {
		return SettlementResult{}, false, ErrReservationNotFound
	}
	if err != nil {
		return SettlementResult{}, false, newReserveError(ErrUnavailable, err)
	}
	if reservation.RequestID != input.RequestID {
		return SettlementResult{}, false, ErrSettlementConflict
	}
	requestStatus, err := loadBudgetRequestStatus(ctx, transaction, input.TenantID, input.RequestID)
	if err != nil {
		return SettlementResult{}, false, newReserveError(ErrUnavailable, err)
	}
	if !requestStatusMatchesOutcome(requestStatus, input.Outcome) {
		return SettlementResult{}, false, ErrSettlementConflict
	}
	if reservation.Status != ReservationPending {
		result, existingErr := loadTerminalSettlement(ctx, transaction, input, reservation, actual, attempt)
		return result, false, existingErr
	}

	account, err := loadBudgetAccount(ctx, transaction, input.TenantID, input.AccountID)
	if err != nil {
		return SettlementResult{}, false, newReserveError(ErrUnavailable, err)
	}
	// A concurrent settlement can commit after the pending Reservation read but
	// before this Account read. Restart so the next transaction observes the
	// terminal Reservation and returns its original facts idempotently.
	if account.ReservedMicros < reservation.ReservedMicros {
		return SettlementResult{}, true, nil
	}
	if actual > MaximumAmount-account.CommittedMicros {
		return SettlementResult{}, false, ErrConflict
	}
	remainingReserved := account.ReservedMicros - reservation.ReservedMicros
	resultCommitted := account.CommittedMicros + actual
	if resultCommitted > MaximumAmount-remainingReserved {
		return SettlementResult{}, false, ErrConflict
	}
	actualSigned, ok := signedExactAmount(actual)
	if !ok {
		return SettlementResult{}, false, ErrInvalid
	}
	reservedSigned := signedStoredAmount(reservation.ReservedMicros)
	maximumSigned := signedStoredAmount(MaximumAmount)
	updated, err := scanBudgetAccount(transaction.QueryRowContext(ctx, `
		UPDATE app.budget_accounts
		SET committed_amount_micros = committed_amount_micros + $3,
			reserved_amount_micros = reserved_amount_micros - $4,
			version = version + 1,
			updated_at = GREATEST(updated_at, clock_timestamp()), updated_by = $5
		WHERE tenant_id = $1 AND id = $2 AND version = $6
			AND reserved_amount_micros >= $4::bigint
			AND committed_amount_micros <= $7::bigint - $3::bigint
			AND committed_amount_micros + $3::bigint <=
				$7::bigint - (reserved_amount_micros - $4::bigint)
		RETURNING `+budgetAccountColumns,
		input.TenantID, input.AccountID, actualSigned, reservedSigned,
		input.Actor, account.Version, maximumSigned,
	))
	if errors.Is(err, sql.ErrNoRows) || isRetryableReserveDatabaseError(err) {
		return SettlementResult{}, true, nil
	}
	if err != nil {
		return SettlementResult{}, false, mapReserveDatabaseError(err)
	}

	released, overage := reservationDifference(reservation.ReservedMicros, actual)
	status := settlementReservationStatus(input.Outcome)
	eventTime := updated.UpdatedAt
	terminal, err := scanBudgetReservation(transaction.QueryRowContext(ctx, `
		UPDATE app.budget_reservations
		SET status = $4, actual_amount_micros = $5,
			released_amount_micros = $6, overage_amount_micros = $7,
			terminal_at = $8, version = version + 1, updated_at = $8, updated_by = $9
		WHERE tenant_id = $1 AND account_id = $2 AND id = $3
			AND status = 'pending' AND version = $10
		RETURNING `+budgetReservationColumns,
		input.TenantID, input.AccountID, input.ReservationID, status,
		actualSigned, signedStoredAmount(released), signedStoredAmount(overage),
		eventTime, input.Actor, reservation.Version,
	))
	if errors.Is(err, sql.ErrNoRows) || isRetryableReserveDatabaseError(err) {
		return SettlementResult{}, true, nil
	}
	if err != nil {
		return SettlementResult{}, false, mapReserveDatabaseError(err)
	}

	entryKind := settlementEntryKind(input.Outcome, actual)
	entry, err := scanBudgetLedger(transaction.QueryRowContext(ctx, `
		INSERT INTO app.budget_ledger_entries (
			tenant_id, account_id, reservation_id, entry_kind, idempotency_key,
			committed_delta_micros, reserved_delta_micros,
			result_committed_micros, result_reserved_micros, occurred_at, created_by
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING `+budgetLedgerColumns,
		input.TenantID, input.AccountID, input.ReservationID, entryKind,
		settlementLedgerKey(entryKind, input.ReservationID), actualSigned, -reservedSigned,
		signedStoredAmount(updated.CommittedMicros), signedStoredAmount(updated.ReservedMicros),
		eventTime, input.Actor,
	))
	if isRetryableReserveDatabaseError(err) {
		return SettlementResult{}, true, nil
	}
	if err != nil {
		return SettlementResult{}, false, mapReserveDatabaseError(err)
	}
	if err = transaction.Commit(); isRetryableReserveDatabaseError(err) {
		return SettlementResult{}, true, nil
	}
	if err != nil {
		return SettlementResult{}, false, newReserveError(ErrUnavailable, err)
	}
	result, err := buildSettlementResult(updated, terminal, entry, input.Outcome, false, attempt)
	return result, false, err
}

func loadBudgetReservation(
	ctx context.Context,
	queryer interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	},
	tenantID, accountID, reservationID string,
) (Reservation, error) {
	return scanBudgetReservation(queryer.QueryRowContext(ctx, `
		SELECT `+budgetReservationColumns+`
		FROM app.budget_reservations
		WHERE tenant_id = $1 AND account_id = $2 AND id = $3`, tenantID, accountID, reservationID))
}

func loadBudgetRequestStatus(
	ctx context.Context,
	queryer interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	},
	tenantID, requestID string,
) (string, error) {
	var status string
	err := queryer.QueryRowContext(ctx, `
		SELECT status FROM app.gateway_requests WHERE tenant_id = $1 AND id = $2`, tenantID, requestID).Scan(&status)
	return status, err
}

func loadTerminalSettlement(
	ctx context.Context,
	transaction *sql.Tx,
	input SettlementInput,
	reservation Reservation,
	actual uint64,
	attempt int,
) (SettlementResult, error) {
	expectedStatus := settlementReservationStatus(input.Outcome)
	if reservation.Status != expectedStatus || reservation.ActualMicros == nil || *reservation.ActualMicros != actual {
		return SettlementResult{}, ErrSettlementConflict
	}
	account, err := loadBudgetAccount(ctx, transaction, input.TenantID, input.AccountID)
	if err != nil {
		return SettlementResult{}, newReserveError(ErrUnavailable, err)
	}
	entryKind := settlementEntryKind(input.Outcome, actual)
	entry, err := scanBudgetLedger(transaction.QueryRowContext(ctx, `
		SELECT `+budgetLedgerColumns+`
		FROM app.budget_ledger_entries
		WHERE tenant_id = $1 AND account_id = $2 AND reservation_id = $3
			AND entry_kind = $4 AND idempotency_key = $5`,
		input.TenantID, input.AccountID, input.ReservationID,
		entryKind, settlementLedgerKey(entryKind, input.ReservationID),
	))
	if err != nil {
		return SettlementResult{}, newReserveError(ErrUnavailable, err)
	}
	return buildSettlementResult(account, reservation, entry, input.Outcome, true, attempt)
}

func requestStatusMatchesOutcome(status string, outcome SettlementOutcome) bool {
	switch outcome {
	case SettlementSucceeded, SettlementCacheHit:
		return status == "succeeded"
	case SettlementFailed:
		return status == "failed" || status == "partial_failed"
	case SettlementCancelled:
		return status == "cancelled"
	default:
		return false
	}
}

var _ Settler = (*PostgresSettler)(nil)
