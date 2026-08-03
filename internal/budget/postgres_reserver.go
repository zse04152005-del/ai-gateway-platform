package budget

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/lib/pq"
)

const budgetAccountColumns = `
	id, tenant_id, scope_kind, project_id, virtual_key_id, principal_ref, session_ref,
	currency, period_start, period_end, soft_limit_micros, hard_limit_micros,
	committed_amount_micros, reserved_amount_micros, status, version,
	created_at, created_by, updated_at, updated_by, closed_at`

const budgetReservationColumns = `
	id, tenant_id, account_id, request_id, idempotency_key, status,
	reserved_amount_micros, actual_amount_micros, released_amount_micros,
	overage_amount_micros, expires_at, version, created_at, created_by,
	updated_at, updated_by, terminal_at`

const budgetLedgerColumns = `
	id, tenant_id, account_id, reservation_id, entry_kind, idempotency_key,
	committed_delta_micros, reserved_delta_micros,
	result_committed_micros, result_reserved_micros, occurred_at, created_by`

// Reserver atomically admits one account hold and appends its evidence.
type Reserver interface {
	Reserve(context.Context, ReserveInput) (ReserveResult, error)
}

// PostgresReserver uses bounded optimistic account-version retries.
type PostgresReserver struct {
	database *sql.DB
	now      func() time.Time
	random   io.Reader
	options  ReserveOptions
}

// NewPostgresReserver validates process-scoped reservation dependencies.
func NewPostgresReserver(
	database *sql.DB,
	now func() time.Time,
	random io.Reader,
	options ReserveOptions,
) (*PostgresReserver, error) {
	if database == nil {
		return nil, errors.New("budget reservation database must not be nil")
	}
	if now == nil || now().IsZero() {
		return nil, errors.New("budget reservation clock must return a non-zero time")
	}
	if random == nil {
		return nil, errors.New("budget reservation random source must not be nil")
	}
	normalized, err := options.normalized()
	if err != nil {
		return nil, err
	}
	return &PostgresReserver{database: database, now: now, random: random, options: normalized}, nil
}

// Reserve creates an account hold, updates its balance and appends one ledger
// entry in the same transaction. Version conflicts retry only up to MaxAttempts.
func (reserver *PostgresReserver) Reserve(ctx context.Context, input ReserveInput) (ReserveResult, error) {
	if reserver == nil || reserver.database == nil || reserver.now == nil || reserver.random == nil ||
		ctx == nil || input.Validate() != nil {
		return ReserveResult{}, ErrInvalid
	}
	input.ExpiresAt = postgresTime(input.ExpiresAt)
	if input.ExpiresAt.IsZero() {
		return ReserveResult{}, ErrInvalid
	}
	reservationID := ""
	for attempt := 1; attempt <= reserver.options.MaxAttempts; attempt++ {
		now := postgresTime(reserver.now())
		if now.IsZero() || !input.ExpiresAt.After(now) {
			return ReserveResult{}, ErrInvalid
		}
		result, conflict, err := reserver.reserveOnce(ctx, input, now, &reservationID, attempt)
		if err != nil {
			return ReserveResult{}, err
		}
		if !conflict {
			return result, nil
		}
		if attempt == reserver.options.MaxAttempts {
			return ReserveResult{}, ErrRetryExhausted
		}
		if err = waitForReserveRetry(ctx, reserver.options.RetryDelay); err != nil {
			return ReserveResult{}, newReserveError(ErrUnavailable, err)
		}
	}
	return ReserveResult{}, ErrRetryExhausted
}

func (reserver *PostgresReserver) reserveOnce(
	ctx context.Context,
	input ReserveInput,
	now time.Time,
	reservationID *string,
	attempt int,
) (ReserveResult, bool, error) {
	transaction, err := reserver.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return ReserveResult{}, false, newReserveError(ErrUnavailable, err)
	}
	defer func() { _ = transaction.Rollback() }()

	existing, found, err := loadExistingReserve(ctx, transaction, input, attempt)
	if err != nil || found {
		return existing, false, err
	}
	account, err := loadBudgetAccount(ctx, transaction, input.TenantID, input.AccountID)
	if errors.Is(err, sql.ErrNoRows) {
		return ReserveResult{}, false, ErrAccountNotFound
	}
	if err != nil {
		return ReserveResult{}, false, newReserveError(ErrUnavailable, err)
	}
	if account.Status != AccountOpen || now.Before(account.PeriodStart) || !now.Before(account.PeriodEnd) {
		return ReserveResult{}, false, ErrAccountInactive
	}
	spent := account.CommittedMicros + account.ReservedMicros
	if input.AmountMicros > account.HardLimitMicros || spent > account.HardLimitMicros-input.AmountMicros {
		return ReserveResult{}, false, ErrBudgetExceeded
	}
	amount, ok := signedExactAmount(input.AmountMicros)
	if !ok {
		return ReserveResult{}, false, ErrInvalid
	}
	if *reservationID == "" {
		*reservationID, err = newBudgetUUID(reserver.random)
		if err != nil {
			return ReserveResult{}, false, newReserveError(ErrUnavailable, err)
		}
	}

	updated, err := scanBudgetAccount(transaction.QueryRowContext(ctx, `
		UPDATE app.budget_accounts
		SET reserved_amount_micros = reserved_amount_micros + $3,
			version = version + 1,
			updated_at = GREATEST(updated_at, clock_timestamp()), updated_by = $4
		WHERE tenant_id = $1 AND id = $2 AND version = $5
			AND status = 'open'
			AND period_start <= clock_timestamp() AND period_end > clock_timestamp()
			AND committed_amount_micros + reserved_amount_micros <= hard_limit_micros - $3
		RETURNING `+budgetAccountColumns,
		input.TenantID, input.AccountID, amount, input.Actor, account.Version,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return ReserveResult{}, true, nil
	}
	if isRetryableReserveDatabaseError(err) {
		return ReserveResult{}, true, nil
	}
	if err != nil {
		return ReserveResult{}, false, mapReserveDatabaseError(err)
	}
	eventTime := updated.UpdatedAt
	if !input.ExpiresAt.After(eventTime) {
		return ReserveResult{}, false, ErrInvalid
	}

	reservation, err := scanBudgetReservation(transaction.QueryRowContext(ctx, `
		INSERT INTO app.budget_reservations (
			id, tenant_id, account_id, request_id, idempotency_key,
			status, reserved_amount_micros, expires_at, version,
			created_at, created_by, updated_at, updated_by
		) VALUES ($1, $2, $3, $4, $5, 'pending', $6, $7, 1, $8, $9, $8, $9)
		RETURNING `+budgetReservationColumns,
		*reservationID, input.TenantID, input.AccountID, input.RequestID,
		input.IdempotencyKey, amount, input.ExpiresAt, eventTime, input.Actor,
	))
	if isIdempotencyUniqueViolation(err) || isRetryableReserveDatabaseError(err) {
		return ReserveResult{}, true, nil
	}
	if err != nil {
		return ReserveResult{}, false, mapReserveDatabaseError(err)
	}

	entry, err := scanBudgetLedger(transaction.QueryRowContext(ctx, `
		INSERT INTO app.budget_ledger_entries (
			tenant_id, account_id, reservation_id, entry_kind, idempotency_key,
			committed_delta_micros, reserved_delta_micros,
			result_committed_micros, result_reserved_micros, occurred_at, created_by
		) VALUES ($1, $2, $3, 'reserve', $4, 0, $5, $6, $7, $8, $9)
		RETURNING `+budgetLedgerColumns,
		input.TenantID, input.AccountID, *reservationID, input.IdempotencyKey,
		amount, signedStoredAmount(updated.CommittedMicros), signedStoredAmount(updated.ReservedMicros), eventTime, input.Actor,
	))
	if isRetryableReserveDatabaseError(err) {
		return ReserveResult{}, true, nil
	}
	if err != nil {
		return ReserveResult{}, false, mapReserveDatabaseError(err)
	}
	if err = transaction.Commit(); isRetryableReserveDatabaseError(err) {
		return ReserveResult{}, true, nil
	}
	if err != nil {
		return ReserveResult{}, false, newReserveError(ErrUnavailable, err)
	}
	result, err := buildReserveResult(updated, reservation, entry, false, attempt)
	return result, false, err
}

func loadExistingReserve(
	ctx context.Context,
	transaction *sql.Tx,
	input ReserveInput,
	attempt int,
) (ReserveResult, bool, error) {
	reservation, err := scanBudgetReservation(transaction.QueryRowContext(ctx, `
		SELECT `+budgetReservationColumns+`
		FROM app.budget_reservations
		WHERE tenant_id = $1 AND account_id = $2 AND idempotency_key = $3`,
		input.TenantID, input.AccountID, input.IdempotencyKey,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return ReserveResult{}, false, nil
	}
	if err != nil {
		return ReserveResult{}, false, newReserveError(ErrUnavailable, err)
	}
	if reservation.RequestID != input.RequestID || reservation.ReservedMicros != input.AmountMicros ||
		!reservation.ExpiresAt.Equal(input.ExpiresAt) {
		return ReserveResult{}, false, ErrIdempotencyConflict
	}
	account, err := loadBudgetAccount(ctx, transaction, input.TenantID, input.AccountID)
	if err != nil {
		return ReserveResult{}, false, newReserveError(ErrUnavailable, err)
	}
	entry, err := scanBudgetLedger(transaction.QueryRowContext(ctx, `
		SELECT `+budgetLedgerColumns+`
		FROM app.budget_ledger_entries
		WHERE tenant_id = $1 AND account_id = $2 AND reservation_id = $3
			AND entry_kind = 'reserve' AND idempotency_key = $4`,
		input.TenantID, input.AccountID, reservation.ID, input.IdempotencyKey,
	))
	if err != nil {
		return ReserveResult{}, false, newReserveError(ErrUnavailable, err)
	}
	result, err := buildReserveResult(account, reservation, entry, true, attempt)
	return result, true, err
}

func loadBudgetAccount(
	ctx context.Context,
	queryer interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	},
	tenantID string,
	accountID string,
) (Account, error) {
	return scanBudgetAccount(queryer.QueryRowContext(ctx, `
		SELECT `+budgetAccountColumns+`
		FROM app.budget_accounts
		WHERE tenant_id = $1 AND id = $2`, tenantID, accountID))
}

func scanBudgetAccount(scanner interface{ Scan(...any) error }) (Account, error) {
	var (
		account                         Account
		kind, status                    string
		projectID, keyID                sql.NullString
		principalRef, sessionRef        sql.NullString
		closedAt                        sql.NullTime
		soft, hard, committed, reserved int64
	)
	err := scanner.Scan(
		&account.ID, &account.Scope.TenantID, &kind, &projectID, &keyID, &principalRef, &sessionRef,
		&account.Currency, &account.PeriodStart, &account.PeriodEnd, &soft, &hard, &committed, &reserved,
		&status, &account.Version, &account.CreatedAt, &account.CreatedBy,
		&account.UpdatedAt, &account.UpdatedBy, &closedAt,
	)
	if err != nil {
		return Account{}, err
	}
	account.Scope.Kind = ScopeKind(kind)
	account.Scope.ProjectID = projectID.String
	account.Scope.VirtualKeyID = keyID.String
	account.Scope.PrincipalRef = principalRef.String
	account.Scope.SessionRef = sessionRef.String
	var ok bool
	account.SoftLimitMicros, ok = unsignedStoredAmount(soft)
	if !ok {
		return Account{}, ErrUnavailable
	}
	account.HardLimitMicros, ok = unsignedStoredAmount(hard)
	if !ok {
		return Account{}, ErrUnavailable
	}
	account.CommittedMicros, ok = unsignedStoredAmount(committed)
	if !ok {
		return Account{}, ErrUnavailable
	}
	account.ReservedMicros, ok = unsignedStoredAmount(reserved)
	if !ok {
		return Account{}, ErrUnavailable
	}
	account.Status = AccountStatus(status)
	if closedAt.Valid {
		account.ClosedAt = &closedAt.Time
	}
	if account.Validate() != nil {
		return Account{}, ErrUnavailable
	}
	return account, nil
}

func scanBudgetReservation(scanner interface{ Scan(...any) error }) (Reservation, error) {
	var (
		reservation               Reservation
		status                    string
		reserved                  int64
		actual, released, overage sql.NullInt64
		terminalAt                sql.NullTime
	)
	err := scanner.Scan(
		&reservation.ID, &reservation.TenantID, &reservation.AccountID,
		&reservation.RequestID, &reservation.IdempotencyKey, &status,
		&reserved, &actual, &released, &overage, &reservation.ExpiresAt,
		&reservation.Version, &reservation.CreatedAt, &reservation.CreatedBy,
		&reservation.UpdatedAt, &reservation.UpdatedBy, &terminalAt,
	)
	if err != nil {
		return Reservation{}, err
	}
	reservation.Status = ReservationStatus(status)
	var ok bool
	reservation.ReservedMicros, ok = unsignedStoredAmount(reserved)
	if !ok {
		return Reservation{}, ErrUnavailable
	}
	if actual.Valid {
		value, valid := unsignedStoredAmount(actual.Int64)
		if !valid {
			return Reservation{}, ErrUnavailable
		}
		reservation.ActualMicros = &value
	}
	if released.Valid {
		value, valid := unsignedStoredAmount(released.Int64)
		if !valid {
			return Reservation{}, ErrUnavailable
		}
		reservation.ReleasedMicros = &value
	}
	if overage.Valid {
		value, valid := unsignedStoredAmount(overage.Int64)
		if !valid {
			return Reservation{}, ErrUnavailable
		}
		reservation.OverageMicros = &value
	}
	if terminalAt.Valid {
		reservation.TerminalAt = &terminalAt.Time
	}
	if reservation.Validate() != nil {
		return Reservation{}, ErrUnavailable
	}
	return reservation, nil
}

func scanBudgetLedger(scanner interface{ Scan(...any) error }) (LedgerEntry, error) {
	var (
		entry                           LedgerEntry
		kind                            string
		committedDelta, reservedDelta   int64
		resultCommitted, resultReserved int64
	)
	err := scanner.Scan(
		&entry.ID, &entry.TenantID, &entry.AccountID, &entry.ReservationID,
		&kind, &entry.IdempotencyKey, &committedDelta, &reservedDelta,
		&resultCommitted, &resultReserved, &entry.OccurredAt, &entry.CreatedBy,
	)
	if err != nil {
		return LedgerEntry{}, err
	}
	entry.Kind = EntryKind(kind)
	entry.CommittedDeltaMicros = committedDelta
	entry.ReservedDeltaMicros = reservedDelta
	var ok bool
	entry.ResultCommittedMicros, ok = unsignedStoredAmount(resultCommitted)
	if !ok {
		return LedgerEntry{}, ErrUnavailable
	}
	entry.ResultReservedMicros, ok = unsignedStoredAmount(resultReserved)
	if !ok || entry.Validate() != nil {
		return LedgerEntry{}, ErrUnavailable
	}
	return entry, nil
}

func newBudgetUUID(reader io.Reader) (string, error) {
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

func waitForReserveRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func postgresTime(value time.Time) time.Time { return value.UTC().Truncate(time.Microsecond) }

func signedExactAmount(value uint64) (int64, bool) {
	if value > MaximumAmount {
		return 0, false
	}
	return int64(value), true //nolint:gosec // MaximumAmount is below math.MaxInt64.
}

func signedStoredAmount(value uint64) int64 {
	converted, _ := signedExactAmount(value)
	return converted
}

func unsignedStoredAmount(value int64) (uint64, bool) {
	if value < 0 || value > int64(MaximumAmount) {
		return 0, false
	}
	return uint64(value), true //nolint:gosec // Non-negative and bounded above.
}

func isIdempotencyUniqueViolation(err error) bool {
	var databaseError *pq.Error
	return errors.As(err, &databaseError) && databaseError.Code == "23505" &&
		databaseError.Constraint == "budget_reservations_account_idempotency_unique"
}

func isRetryableReserveDatabaseError(err error) bool {
	var databaseError *pq.Error
	return errors.As(err, &databaseError) && (databaseError.Code == "40001" || databaseError.Code == "40P01")
}

func mapReserveDatabaseError(err error) error {
	var databaseError *pq.Error
	if errors.As(err, &databaseError) {
		switch databaseError.Code {
		case "23503", "23505", "23514":
			return newReserveError(ErrConflict, err)
		}
	}
	return newReserveError(ErrUnavailable, err)
}

type reserveError struct {
	kind  error
	cause error
}

func newReserveError(kind, cause error) error {
	if cause == nil {
		return kind
	}
	return &reserveError{kind: kind, cause: cause}
}

func (failure *reserveError) Error() string {
	if failure == nil || failure.kind == nil {
		return "budget reservation failed"
	}
	return failure.kind.Error()
}

func (failure *reserveError) Unwrap() []error {
	if failure == nil {
		return nil
	}
	return []error{failure.kind, failure.cause}
}

var _ Reserver = (*PostgresReserver)(nil)
