//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/zse04152005-del/ai-gateway-platform/internal/budget"
)

const (
	budgetAtomicAccountID       = "78000000-0000-4000-8000-000000000301"
	budgetRetryCapAccountID     = "78000000-0000-4000-8000-000000000302"
	budgetRetrySuccessAccountID = "78000000-0000-4000-8000-000000000303"
	budgetRollbackAccountID     = "78000000-0000-4000-8000-000000000304"
	budgetAtomicRequestCount    = 160
)

func TestPostgresBudgetAtomicReservation(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set")
	}
	database, err := sql.Open("postgres", databaseURL)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	database.SetMaxOpenConns(32)
	database.SetMaxIdleConns(32)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		t.Fatalf("database.PingContext() error = %v", err)
	}
	cleanupBudgetLedgerFixtures(t, database)
	t.Cleanup(func() { cleanupBudgetLedgerFixtures(t, database) })
	seedBudgetLedgerParents(ctx, t, database)

	periodStart := time.Now().UTC().Add(-time.Hour)
	periodEnd := periodStart.Add(2 * time.Hour)
	insertAtomicBudgetAccount(ctx, t, database, budgetAtomicAccountID, "tenant", nil, nil, nil, nil, 80, 100, periodStart, periodEnd)
	insertAtomicBudgetAccount(ctx, t, database, budgetRetryCapAccountID, "user", nil, nil, "user:retry-cap", nil, 80, 100, periodStart, periodEnd)
	insertAtomicBudgetAccount(ctx, t, database, budgetRetrySuccessAccountID, "session", nil, nil, nil, "session:retry-success", 80, 100, periodStart, periodEnd)
	insertAtomicBudgetAccount(ctx, t, database, budgetRollbackAccountID, "project", budgetProjectOneID, nil, nil, nil, 80, 100, periodStart, periodEnd)
	inputs := seedAtomicBudgetRequests(ctx, t, database)

	reserver, err := budget.NewPostgresReserver(database, time.Now, rand.Reader, budget.ReserveOptions{
		MaxAttempts: budget.MaximumReserveAttempts,
		RetryDelay:  250 * time.Microsecond,
	})
	if err != nil {
		t.Fatalf("budget.NewPostgresReserver() error = %v", err)
	}
	type outcome struct {
		input  budget.ReserveInput
		result budget.ReserveResult
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, len(inputs))
	var workers sync.WaitGroup
	workers.Add(len(inputs))
	for _, input := range inputs {
		go func(selected budget.ReserveInput) {
			defer workers.Done()
			<-start
			result, reserveErr := reserver.Reserve(ctx, selected)
			outcomes <- outcome{input: selected, result: result, err: reserveErr}
		}(input)
	}
	close(start)
	workers.Wait()
	close(outcomes)

	allowed := make([]outcome, 0, 100)
	exceeded := make([]outcome, 0, budgetAtomicRequestCount-100)
	softExceeded := 0
	seenReservations := make(map[string]struct{}, 100)
	seenBalances := make(map[uint64]struct{}, 100)
	for selected := range outcomes {
		switch {
		case selected.err == nil:
			allowed = append(allowed, selected)
			if selected.result.Idempotent || selected.result.Attempts < 1 ||
				selected.result.Attempts > budget.MaximumReserveAttempts ||
				selected.result.Reservation.Status != budget.ReservationPending ||
				selected.result.LedgerEntry.Kind != budget.EntryReserve {
				t.Errorf("allowed result = %+v", selected.result)
			}
			if _, exists := seenReservations[selected.result.Reservation.ID]; exists {
				t.Errorf("duplicate reservation ID %s", selected.result.Reservation.ID)
			}
			seenReservations[selected.result.Reservation.ID] = struct{}{}
			seenBalances[selected.result.ResultReservedMicros] = struct{}{}
			if selected.result.SoftLimitExceeded {
				softExceeded++
			}
		case errors.Is(selected.err, budget.ErrBudgetExceeded):
			exceeded = append(exceeded, selected)
		default:
			t.Errorf("Reserve(%s) unexpected error = %v (%s)",
				selected.input.IdempotencyKey, selected.err, budgetErrorDiagnostic(selected.err))
		}
	}
	if len(allowed) != 100 || len(exceeded) != budgetAtomicRequestCount-100 {
		t.Fatalf("atomic admission = allowed:%d exceeded:%d, want 100/%d", len(allowed), len(exceeded), budgetAtomicRequestCount-100)
	}
	if len(seenBalances) != 100 || softExceeded != 20 {
		t.Fatalf("result balances/soft exceeded = %d/%d, want 100/20", len(seenBalances), softExceeded)
	}
	assertAtomicBudgetCounts(ctx, t, database, 100, 100, 100, 101)

	duplicate, err := reserver.Reserve(ctx, allowed[0].input)
	if err != nil || !duplicate.Idempotent || duplicate.Attempts != 1 ||
		duplicate.Reservation.ID != allowed[0].result.Reservation.ID ||
		duplicate.LedgerEntry.ID != allowed[0].result.LedgerEntry.ID {
		t.Fatalf("duplicate Reserve() = %+v/%v", duplicate, err)
	}
	conflicting := allowed[0].input
	conflicting.AmountMicros = 2
	if _, err := reserver.Reserve(ctx, conflicting); !errors.Is(err, budget.ErrIdempotencyConflict) {
		t.Fatalf("conflicting idempotency Reserve() error = %v", err)
	}
	if _, err := reserver.Reserve(ctx, exceeded[0].input); !errors.Is(err, budget.ErrBudgetExceeded) {
		t.Fatalf("full account Reserve() error = %v", err)
	}
	crossTenant := exceeded[0].input
	crossTenant.TenantID = budgetTenantTwoID
	if _, err := reserver.Reserve(ctx, crossTenant); !errors.Is(err, budget.ErrAccountNotFound) {
		t.Fatalf("cross-tenant Reserve() error = %v", err)
	}

	testBudgetReservationRollback(ctx, t, database, inputs[0])
	testBudgetVersionRetryBound(ctx, t, database, inputs[1], budgetRetryCapAccountID, 1, true)
	testBudgetVersionRetryBound(ctx, t, database, inputs[2], budgetRetrySuccessAccountID, 2, false)
}

func seedAtomicBudgetRequests(ctx context.Context, t *testing.T, database *sql.DB) []budget.ReserveInput {
	t.Helper()
	expiresAt := time.Now().UTC().Add(15 * time.Minute).Truncate(time.Microsecond)
	inputs := make([]budget.ReserveInput, 0, budgetAtomicRequestCount)
	for index := range budgetAtomicRequestCount {
		requestID := fmt.Sprintf("integration-budget-atomic-%03d", index)
		_, err := database.ExecContext(ctx, `
			INSERT INTO app.gateway_requests (
				id, tenant_id, project_id, virtual_key_id, logical_model,
				trace_id, span_id, status, started_at, updated_at
			) VALUES ($1, $2, $3, $4, 'budget-model',
				'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'bbbbbbbbbbbbbbbb', 'authorized',
				CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
			requestID, budgetTenantOneID, budgetProjectOneID, budgetKeyOneID,
		)
		if err != nil {
			t.Fatalf("insert atomic budget request %d: %v", index, err)
		}
		inputs = append(inputs, budget.ReserveInput{
			TenantID: budgetTenantOneID, AccountID: budgetAtomicAccountID,
			RequestID: requestID, IdempotencyKey: fmt.Sprintf("reserve:atomic:%03d", index),
			AmountMicros: 1, ExpiresAt: expiresAt, Actor: "integration:budget-atomic",
		})
	}
	return inputs
}

func insertAtomicBudgetAccount(
	ctx context.Context,
	t *testing.T,
	database *sql.DB,
	accountID string,
	kind string,
	projectID, keyID, principalRef, sessionRef any,
	soft, hard int64,
	periodStart, periodEnd time.Time,
) {
	t.Helper()
	_, err := database.ExecContext(ctx, insertBudgetAccountSQL,
		accountID, budgetTenantOneID, kind, projectID, keyID, principalRef, sessionRef,
		periodStart, periodEnd, soft, hard, 0, 0,
	)
	if err != nil {
		t.Fatalf("insert atomic budget account %s: %v", accountID, err)
	}
}

func assertAtomicBudgetCounts(
	ctx context.Context,
	t *testing.T,
	database *sql.DB,
	wantReserved, wantReservations, wantLedger, wantVersion int,
) {
	t.Helper()
	var reserved, reservations, ledger, version int
	err := database.QueryRowContext(ctx, `
		SELECT a.reserved_amount_micros, a.version,
			(SELECT count(*) FROM app.budget_reservations r WHERE r.account_id = a.id),
			(SELECT count(*) FROM app.budget_ledger_entries l WHERE l.account_id = a.id)
		FROM app.budget_accounts a WHERE a.id = $1`, budgetAtomicAccountID,
	).Scan(&reserved, &version, &reservations, &ledger)
	if err != nil || reserved != wantReserved || reservations != wantReservations ||
		ledger != wantLedger || version != wantVersion {
		t.Fatalf("atomic stored facts = reserved:%d reservations:%d ledger:%d version:%d error:%v",
			reserved, reservations, ledger, version, err)
	}
}

func testBudgetReservationRollback(
	ctx context.Context,
	t *testing.T,
	database *sql.DB,
	base budget.ReserveInput,
) {
	t.Helper()
	reserver, err := budget.NewPostgresReserver(database, time.Now, rand.Reader, budget.ReserveOptions{MaxAttempts: 2})
	if err != nil {
		t.Fatalf("new rollback reserver: %v", err)
	}
	base.AccountID = budgetRollbackAccountID
	base.RequestID = "integration-budget-missing-request"
	base.IdempotencyKey = "reserve:atomic:rollback"
	if _, err := reserver.Reserve(ctx, base); !errors.Is(err, budget.ErrConflict) {
		t.Fatalf("missing request Reserve() error = %v", err)
	}
	var reserved, version, reservations, ledger int
	err = database.QueryRowContext(ctx, `
		SELECT a.reserved_amount_micros, a.version,
			(SELECT count(*) FROM app.budget_reservations WHERE account_id = a.id),
			(SELECT count(*) FROM app.budget_ledger_entries WHERE account_id = a.id)
		FROM app.budget_accounts a WHERE a.id = $1`, budgetRollbackAccountID,
	).Scan(&reserved, &version, &reservations, &ledger)
	if err != nil || reserved != 0 || version != 1 || reservations != 0 || ledger != 0 {
		t.Fatalf("rolled-back facts = %d/%d/%d/%d error:%v", reserved, version, reservations, ledger, err)
	}
}

func testBudgetVersionRetryBound(
	ctx context.Context,
	t *testing.T,
	database *sql.DB,
	base budget.ReserveInput,
	accountID string,
	maxAttempts int,
	wantExhausted bool,
) {
	t.Helper()
	blocker, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin version blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback() }()
	_, err = blocker.ExecContext(ctx, `
		UPDATE app.budget_accounts
		SET version = version + 1, updated_at = CURRENT_TIMESTAMP,
			updated_by = 'integration:budget-blocker'
		WHERE id = $1`, accountID)
	if err != nil {
		t.Fatalf("update version blocker: %v", err)
	}
	reserver, err := budget.NewPostgresReserver(database, time.Now, rand.Reader, budget.ReserveOptions{
		MaxAttempts: maxAttempts,
		RetryDelay:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new retry reserver: %v", err)
	}
	base.AccountID = accountID
	base.IdempotencyKey = fmt.Sprintf("reserve:retry:%d", maxAttempts)
	type result struct {
		reservation budget.ReserveResult
		err         error
	}
	finished := make(chan result, 1)
	go func() {
		reservation, reserveErr := reserver.Reserve(ctx, base)
		finished <- result{reservation: reservation, err: reserveErr}
	}()
	waitForBudgetCASLock(ctx, t, database)
	if err := blocker.Commit(); err != nil {
		t.Fatalf("commit version blocker: %v", err)
	}
	selected := <-finished
	if wantExhausted {
		if !errors.Is(selected.err, budget.ErrRetryExhausted) {
			t.Fatalf("bounded Reserve() = %+v/%v", selected.reservation, selected.err)
		}
		return
	}
	if selected.err != nil || selected.reservation.Attempts != 2 || selected.reservation.Idempotent {
		t.Fatalf("retried Reserve() = %+v/%v", selected.reservation, selected.err)
	}
}

func waitForBudgetCASLock(ctx context.Context, t *testing.T, database *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		err := database.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE pid <> pg_backend_pid() AND datname = current_database()
					AND state = 'active' AND wait_event_type = 'Lock'
					AND query LIKE '%UPDATE app.budget_accounts%reserved_amount_micros%'
			)`).Scan(&waiting)
		if err != nil {
			t.Fatalf("query budget CAS lock: %v", err)
		}
		if waiting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("budget reserver did not reach the controlled CAS lock")
}

func budgetErrorDiagnostic(err error) string {
	if err == nil {
		return "nil"
	}
	var databaseError *pq.Error
	if errors.As(err, &databaseError) {
		return fmt.Sprintf("postgres code=%s constraint=%s", databaseError.Code, databaseError.Constraint)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "context deadline exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "context canceled"
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "sql no rows"
	}
	if unwrapped, ok := err.(interface{ Unwrap() []error }); ok {
		causes := unwrapped.Unwrap()
		for index := len(causes) - 1; index >= 0; index-- {
			if diagnostic := budgetErrorDiagnostic(causes[index]); diagnostic != "" {
				return diagnostic
			}
		}
	}
	return fmt.Sprintf("type=%T", err)
}
