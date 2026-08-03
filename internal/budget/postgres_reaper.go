package budget

import (
	"context"
	"database/sql"
	"errors"
)

// ReservationReaper recovers expired pending holds in bounded batches.
type ReservationReaper interface {
	Reap(context.Context, string) (ReapResult, error)
}

// PostgresReaper claims expired Reservations without blocking peer reapers and
// reconciles each one in an independent account CAS transaction.
type PostgresReaper struct {
	database *sql.DB
	options  ReaperOptions
}

// NewPostgresReaper validates process-scoped expiration dependencies.
func NewPostgresReaper(database *sql.DB, options ReaperOptions) (*PostgresReaper, error) {
	if database == nil {
		return nil, errors.New("budget reaper database must not be nil")
	}
	normalized, err := options.normalized()
	if err != nil {
		return nil, err
	}
	return &PostgresReaper{database: database, options: normalized}, nil
}

// Reap expires at most one configured batch. Earlier events remain committed
// and are returned if a later independent item fails.
func (reaper *PostgresReaper) Reap(ctx context.Context, actor string) (ReapResult, error) {
	if reaper == nil || reaper.database == nil || ctx == nil || !validActor(actor) {
		return ReapResult{}, ErrInvalid
	}
	result := ReapResult{Events: make([]ExpirationEvent, 0, reaper.options.BatchSize)}
	for len(result.Events) < reaper.options.BatchSize {
		event, found, err := reaper.reapNext(ctx, actor)
		if err != nil {
			return result, err
		}
		if !found {
			return result, nil
		}
		result.Events = append(result.Events, event)
	}
	result.AtCapacity = true
	return result, nil
}

func (reaper *PostgresReaper) reapNext(
	ctx context.Context,
	actor string,
) (ExpirationEvent, bool, error) {
	for attempt := 1; attempt <= reaper.options.MaxAttempts; attempt++ {
		event, found, conflict, err := reaper.reapOnce(ctx, actor, attempt)
		if err != nil || !found || !conflict {
			return event, found, err
		}
		if attempt == reaper.options.MaxAttempts {
			return ExpirationEvent{}, true, ErrRetryExhausted
		}
		if err = waitForReserveRetry(ctx, reaper.options.RetryDelay); err != nil {
			return ExpirationEvent{}, true, newReserveError(ErrUnavailable, err)
		}
	}
	return ExpirationEvent{}, true, ErrRetryExhausted
}

func (reaper *PostgresReaper) reapOnce(
	ctx context.Context,
	actor string,
	attempt int,
) (ExpirationEvent, bool, bool, error) {
	transaction, err := reaper.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return ExpirationEvent{}, false, false, newReserveError(ErrUnavailable, err)
	}
	defer func() { _ = transaction.Rollback() }()

	reservation, err := loadNextExpiredReservation(ctx, transaction)
	if errors.Is(err, sql.ErrNoRows) {
		return ExpirationEvent{}, false, false, nil
	}
	if err != nil {
		return ExpirationEvent{}, false, false, newReserveError(ErrUnavailable, err)
	}
	account, err := loadBudgetAccount(ctx, transaction, reservation.TenantID, reservation.AccountID)
	if err != nil {
		return ExpirationEvent{}, true, false, newReserveError(ErrUnavailable, err)
	}
	if account.ReservedMicros < reservation.ReservedMicros {
		return ExpirationEvent{}, true, false, ErrConflict
	}

	reservedSigned := signedStoredAmount(reservation.ReservedMicros)
	updated, err := scanBudgetAccount(transaction.QueryRowContext(ctx, `
		UPDATE app.budget_accounts
		SET reserved_amount_micros = reserved_amount_micros - $3::bigint,
			version = version + 1,
			updated_at = GREATEST(updated_at, clock_timestamp()), updated_by = $4
		WHERE tenant_id = $1 AND id = $2 AND version = $5
			AND reserved_amount_micros >= $3::bigint
		RETURNING `+budgetAccountColumns,
		reservation.TenantID, reservation.AccountID, reservedSigned, actor, account.Version,
	))
	if errors.Is(err, sql.ErrNoRows) || isRetryableReserveDatabaseError(err) {
		return ExpirationEvent{}, true, true, nil
	}
	if err != nil {
		return ExpirationEvent{}, true, false, mapReserveDatabaseError(err)
	}

	eventTime := updated.UpdatedAt
	zero := int64(0)
	terminal, err := scanBudgetReservation(transaction.QueryRowContext(ctx, `
		UPDATE app.budget_reservations
		SET status = 'expired', actual_amount_micros = $4,
			released_amount_micros = $5, overage_amount_micros = $4,
			terminal_at = $6, version = version + 1, updated_at = $6, updated_by = $7
		WHERE tenant_id = $1 AND account_id = $2 AND id = $3
			AND status = 'pending' AND version = $8 AND expires_at <= $6
		RETURNING `+budgetReservationColumns,
		reservation.TenantID, reservation.AccountID, reservation.ID, zero,
		reservedSigned, eventTime, actor, reservation.Version,
	))
	if errors.Is(err, sql.ErrNoRows) || isRetryableReserveDatabaseError(err) {
		return ExpirationEvent{}, true, true, nil
	}
	if err != nil {
		return ExpirationEvent{}, true, false, mapReserveDatabaseError(err)
	}

	entry, err := scanBudgetLedger(transaction.QueryRowContext(ctx, `
		INSERT INTO app.budget_ledger_entries (
			tenant_id, account_id, reservation_id, entry_kind, idempotency_key,
			committed_delta_micros, reserved_delta_micros,
			result_committed_micros, result_reserved_micros, occurred_at, created_by
		) VALUES ($1, $2, $3, 'expire', $4, 0, $5, $6, $7, $8, $9)
		RETURNING `+budgetLedgerColumns,
		reservation.TenantID, reservation.AccountID, reservation.ID,
		expirationLedgerKey(reservation.ID), -reservedSigned,
		signedStoredAmount(updated.CommittedMicros), signedStoredAmount(updated.ReservedMicros),
		eventTime, actor,
	))
	if isRetryableReserveDatabaseError(err) {
		return ExpirationEvent{}, true, true, nil
	}
	if err != nil {
		return ExpirationEvent{}, true, false, mapReserveDatabaseError(err)
	}
	event, err := buildExpirationEvent(updated, terminal, entry, attempt)
	if err != nil {
		return ExpirationEvent{}, true, false, err
	}
	if err = transaction.Commit(); isRetryableReserveDatabaseError(err) {
		return ExpirationEvent{}, true, true, nil
	}
	if err != nil {
		return ExpirationEvent{}, true, false, newReserveError(ErrUnavailable, err)
	}
	return event, true, false, nil
}

func loadNextExpiredReservation(ctx context.Context, transaction *sql.Tx) (Reservation, error) {
	return scanBudgetReservation(transaction.QueryRowContext(ctx, `
		SELECT `+budgetReservationColumns+`
		FROM app.budget_reservations
		WHERE status = 'pending' AND expires_at <= clock_timestamp()
		ORDER BY expires_at, id
		FOR UPDATE SKIP LOCKED
		LIMIT 1`))
}

var _ ReservationReaper = (*PostgresReaper)(nil)
