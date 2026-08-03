//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/budget"
)

const (
	budgetSettlementSuccessAccountID = "78000000-0000-4000-8000-000000000401"
	budgetSettlementFailureAccountID = "78000000-0000-4000-8000-000000000402"
	budgetSettlementCacheAccountID   = "78000000-0000-4000-8000-000000000403"
	budgetSettlementCancelAccountID  = "78000000-0000-4000-8000-000000000404"
	budgetSettlementPartialAccountID = "78000000-0000-4000-8000-000000000405"

	budgetSettlementSuccessRequestID = "integration-budget-settlement-success"
	budgetSettlementFailureRequestID = "integration-budget-settlement-failure"
	budgetSettlementCacheRequestID   = "integration-budget-settlement-cache"
	budgetSettlementCancelRequestID  = "integration-budget-settlement-cancel"
	budgetSettlementPartialRequestID = "integration-budget-settlement-partial"
)

func TestPostgresBudgetSettlementAndDifferenceRelease(t *testing.T) {
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
	insertAtomicBudgetAccount(ctx, t, database, budgetSettlementSuccessAccountID,
		"tenant", nil, nil, nil, nil, 400, 500, periodStart, periodEnd)
	insertAtomicBudgetAccount(ctx, t, database, budgetSettlementFailureAccountID,
		"project", budgetProjectOneID, nil, nil, nil, 80, 100, periodStart, periodEnd)
	insertAtomicBudgetAccount(ctx, t, database, budgetSettlementCacheAccountID,
		"key", budgetProjectOneID, budgetKeyOneID, nil, nil, 400, 500, periodStart, periodEnd)
	insertAtomicBudgetAccount(ctx, t, database, budgetSettlementCancelAccountID,
		"user", nil, nil, "user:settlement-cancel", nil, 400, 500, periodStart, periodEnd)
	insertAtomicBudgetAccount(ctx, t, database, budgetSettlementPartialAccountID,
		"session", nil, nil, nil, "session:settlement-partial", 400, 500, periodStart, periodEnd)

	requestIDs := []string{
		budgetSettlementSuccessRequestID, budgetSettlementFailureRequestID, budgetSettlementCacheRequestID,
		budgetSettlementCancelRequestID, budgetSettlementPartialRequestID,
	}
	for _, requestID := range requestIDs {
		insertSettlementRequest(ctx, t, database, requestID)
	}
	reserver, err := budget.NewPostgresReserver(database, time.Now, rand.Reader, budget.ReserveOptions{})
	if err != nil {
		t.Fatalf("budget.NewPostgresReserver() error = %v", err)
	}
	expiresAt := time.Now().UTC().Add(15 * time.Minute).Truncate(time.Microsecond)
	success := reserveForSettlement(ctx, t, reserver, budgetSettlementSuccessAccountID,
		budgetSettlementSuccessRequestID, 100, expiresAt)
	failure := reserveForSettlement(ctx, t, reserver, budgetSettlementFailureAccountID,
		budgetSettlementFailureRequestID, 80, expiresAt)
	cache := reserveForSettlement(ctx, t, reserver, budgetSettlementCacheAccountID,
		budgetSettlementCacheRequestID, 100, expiresAt)
	cancelled := reserveForSettlement(ctx, t, reserver, budgetSettlementCancelAccountID,
		budgetSettlementCancelRequestID, 100, expiresAt)
	partial := reserveForSettlement(ctx, t, reserver, budgetSettlementPartialAccountID,
		budgetSettlementPartialRequestID, 100, expiresAt)

	closeBudgetAccount(ctx, t, database, budgetSettlementSuccessAccountID)
	completeSettlementRequest(ctx, t, database, budgetSettlementSuccessRequestID, "succeeded", 2)
	completeSettlementRequest(ctx, t, database, budgetSettlementFailureRequestID, "failed", 2)
	completeSettlementRequest(ctx, t, database, budgetSettlementCacheRequestID, "succeeded", 0)
	completeSettlementRequest(ctx, t, database, budgetSettlementCancelRequestID, "cancelled", 0)
	completeSettlementRequest(ctx, t, database, budgetSettlementPartialRequestID, "cancelled", 1)

	settler, err := budget.NewPostgresSettler(database, budget.SettlementOptions{})
	if err != nil {
		t.Fatalf("budget.NewPostgresSettler() error = %v", err)
	}
	successInput := budget.SettlementInput{
		TenantID: budgetTenantOneID, AccountID: budgetSettlementSuccessAccountID,
		ReservationID: success.Reservation.ID, RequestID: budgetSettlementSuccessRequestID,
		Outcome: budget.SettlementSucceeded,
		Charges: []budget.SettlementCharge{
			{Kind: budget.ChargeAttempt, ReferenceID: "78000000-0000-4000-8000-000000000411", AmountMicros: 40},
			{Kind: budget.ChargeAttempt, ReferenceID: "78000000-0000-4000-8000-000000000412", AmountMicros: 30},
		},
		Actor: "integration:budget-settlement",
	}
	settleSameReservationConcurrently(ctx, t, settler, successInput)
	assertSettlementStored(ctx, t, database, budgetSettlementSuccessAccountID,
		budget.ReservationSettled, budget.EntrySettle, 70, 30, 0, "closed")

	failureInput := budget.SettlementInput{
		TenantID: budgetTenantOneID, AccountID: budgetSettlementFailureAccountID,
		ReservationID: failure.Reservation.ID, RequestID: budgetSettlementFailureRequestID,
		Outcome: budget.SettlementFailed,
		Charges: []budget.SettlementCharge{
			{Kind: budget.ChargeAttempt, ReferenceID: "78000000-0000-4000-8000-000000000421", AmountMicros: 50},
			{Kind: budget.ChargeAttempt, ReferenceID: "78000000-0000-4000-8000-000000000422", AmountMicros: 70},
		},
		Actor: "integration:budget-settlement",
	}
	failureResult := settleBudgetScenario(ctx, t, settler, failureInput, 120, 0, 40, budget.EntrySettle)
	if failureResult.RemainingHardMicros != 0 || !failureResult.SoftLimitExceeded {
		t.Fatalf("failure overage limits = %+v", failureResult)
	}
	assertSettlementStored(ctx, t, database, budgetSettlementFailureAccountID,
		budget.ReservationSettled, budget.EntrySettle, 120, 0, 40, "open")

	cacheInput := budget.SettlementInput{
		TenantID: budgetTenantOneID, AccountID: budgetSettlementCacheAccountID,
		ReservationID: cache.Reservation.ID, RequestID: budgetSettlementCacheRequestID,
		Outcome: budget.SettlementCacheHit,
		Charges: []budget.SettlementCharge{{
			Kind: budget.ChargeCache, ReferenceID: "cache:sha256:settlement", AmountMicros: 5,
		}},
		Actor: "integration:budget-settlement",
	}
	cacheResult := settleBudgetScenario(ctx, t, settler, cacheInput, 5, 95, 0, budget.EntrySettle)
	cacheAgain, err := settler.Settle(ctx, cacheInput)
	if err != nil || !cacheAgain.Idempotent || cacheAgain.Reservation.ID != cacheResult.Reservation.ID ||
		cacheAgain.LedgerEntry.ID != cacheResult.LedgerEntry.ID {
		t.Fatalf("idempotent cache settlement = %+v/%v", cacheAgain, err)
	}
	conflicting := cacheInput
	conflicting.Charges = []budget.SettlementCharge{{
		Kind: budget.ChargeCache, ReferenceID: "cache:sha256:settlement", AmountMicros: 4,
	}}
	if _, err := settler.Settle(ctx, conflicting); !errors.Is(err, budget.ErrSettlementConflict) {
		t.Fatalf("conflicting terminal settlement error = %v", err)
	}
	assertSettlementStored(ctx, t, database, budgetSettlementCacheAccountID,
		budget.ReservationSettled, budget.EntrySettle, 5, 95, 0, "open")

	cancelInput := budget.SettlementInput{
		TenantID: budgetTenantOneID, AccountID: budgetSettlementCancelAccountID,
		ReservationID: cancelled.Reservation.ID, RequestID: budgetSettlementCancelRequestID,
		Outcome: budget.SettlementCancelled, Actor: "integration:budget-settlement",
	}
	settleBudgetScenario(ctx, t, settler, cancelInput, 0, 100, 0, budget.EntryRelease)
	assertSettlementStored(ctx, t, database, budgetSettlementCancelAccountID,
		budget.ReservationCancelled, budget.EntryRelease, 0, 100, 0, "open")

	partialInput := budget.SettlementInput{
		TenantID: budgetTenantOneID, AccountID: budgetSettlementPartialAccountID,
		ReservationID: partial.Reservation.ID, RequestID: budgetSettlementPartialRequestID,
		Outcome: budget.SettlementCancelled,
		Charges: []budget.SettlementCharge{{
			Kind: budget.ChargeAttempt, ReferenceID: "78000000-0000-4000-8000-000000000431", AmountMicros: 20,
		}},
		Actor: "integration:budget-settlement",
	}
	settleBudgetScenario(ctx, t, settler, partialInput, 20, 80, 0, budget.EntrySettle)
	assertSettlementStored(ctx, t, database, budgetSettlementPartialAccountID,
		budget.ReservationCancelled, budget.EntrySettle, 20, 80, 0, "open")

	unknown := partialInput
	unknown.ReservationID = "78000000-0000-4000-8000-000000000499"
	if _, err := settler.Settle(ctx, unknown); !errors.Is(err, budget.ErrReservationNotFound) {
		t.Fatalf("unknown reservation settlement error = %v", err)
	}
	mismatched := partialInput
	mismatched.Outcome = budget.SettlementFailed
	if _, err := settler.Settle(ctx, mismatched); !errors.Is(err, budget.ErrSettlementConflict) {
		t.Fatalf("request outcome mismatch settlement error = %v", err)
	}
}

func insertSettlementRequest(ctx context.Context, t *testing.T, database *sql.DB, requestID string) {
	t.Helper()
	_, err := database.ExecContext(ctx, `
		INSERT INTO app.gateway_requests (
			id, tenant_id, project_id, virtual_key_id, logical_model,
			trace_id, span_id, status, started_at, updated_at
		) VALUES ($1, $2, $3, $4, 'budget-model',
			'cccccccccccccccccccccccccccccccc', 'dddddddddddddddd', 'authorized',
			CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		requestID, budgetTenantOneID, budgetProjectOneID, budgetKeyOneID,
	)
	if err != nil {
		t.Fatalf("insert settlement request %s: %v", requestID, err)
	}
}

func reserveForSettlement(
	ctx context.Context,
	t *testing.T,
	reserver budget.Reserver,
	accountID, requestID string,
	amount uint64,
	expiresAt time.Time,
) budget.ReserveResult {
	t.Helper()
	result, err := reserver.Reserve(ctx, budget.ReserveInput{
		TenantID: budgetTenantOneID, AccountID: accountID, RequestID: requestID,
		IdempotencyKey: "reserve:" + requestID, AmountMicros: amount,
		ExpiresAt: expiresAt, Actor: "integration:budget-settlement",
	})
	if err != nil {
		t.Fatalf("reserve %s: %v", requestID, err)
	}
	return result
}

func closeBudgetAccount(ctx context.Context, t *testing.T, database *sql.DB, accountID string) {
	t.Helper()
	_, err := database.ExecContext(ctx, `
		UPDATE app.budget_accounts
		SET status = 'closed', closed_at = clock_timestamp(), version = version + 1,
			updated_at = GREATEST(updated_at, clock_timestamp()), updated_by = 'integration:budget-close'
		WHERE id = $1`, accountID)
	if err != nil {
		t.Fatalf("close budget account: %v", err)
	}
}

func completeSettlementRequest(
	ctx context.Context,
	t *testing.T,
	database *sql.DB,
	requestID, terminalStatus string,
	attemptCount int,
) {
	t.Helper()
	if terminalStatus == "cancelled" && attemptCount == 0 {
		updateSettlementRequest(ctx, t, database, requestID, "authorized", "cancelled", false, true)
		return
	}
	updateSettlementRequest(ctx, t, database, requestID, "authorized", "routing", false, false)
	updateSettlementRequest(ctx, t, database, requestID, "routing", "running", attemptCount > 0, false)
	for attempt := 1; attempt < attemptCount; attempt++ {
		updateSettlementRequest(ctx, t, database, requestID, "running", "running", true, false)
	}
	updateSettlementRequest(ctx, t, database, requestID, "running", terminalStatus, false, true)
}

func updateSettlementRequest(
	ctx context.Context,
	t *testing.T,
	database *sql.DB,
	requestID, fromStatus, toStatus string,
	incrementAttempt, terminal bool,
) {
	t.Helper()
	result, err := database.ExecContext(ctx, `
		UPDATE app.gateway_requests
		SET status = $3,
			attempt_count = attempt_count + CASE WHEN $4 THEN 1 ELSE 0 END,
			ended_at = CASE WHEN $5 THEN clock_timestamp() ELSE NULL END,
			end_reason = CASE WHEN $5 THEN 'budget_settlement_test' ELSE NULL END,
			version = version + 1, updated_at = GREATEST(updated_at, clock_timestamp())
		WHERE id = $1 AND status = $2`, requestID, fromStatus, toStatus, incrementAttempt, terminal)
	if err != nil {
		t.Fatalf("advance settlement request %s %s→%s: %v", requestID, fromStatus, toStatus, err)
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		t.Fatalf("advance settlement request %s %s→%s rows/error = %d/%v", requestID, fromStatus, toStatus, rows, err)
	}
}

func settleSameReservationConcurrently(
	ctx context.Context,
	t *testing.T,
	settler budget.Settler,
	input budget.SettlementInput,
) {
	t.Helper()
	const concurrency = 64
	start := make(chan struct{})
	results := make(chan budget.SettlementResult, concurrency)
	errorsFound := make(chan error, concurrency)
	var workers sync.WaitGroup
	workers.Add(concurrency)
	for range concurrency {
		go func() {
			defer workers.Done()
			<-start
			result, err := settler.Settle(ctx, input)
			if err != nil {
				errorsFound <- err
				return
			}
			results <- result
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	close(errorsFound)
	for err := range errorsFound {
		t.Errorf("concurrent settlement error = %v", err)
	}
	nonIdempotent := 0
	reservationID := ""
	ledgerID := int64(0)
	for result := range results {
		if !result.Idempotent {
			nonIdempotent++
		}
		if reservationID == "" {
			reservationID = result.Reservation.ID
			ledgerID = result.LedgerEntry.ID
		}
		if result.Reservation.ID != reservationID || result.LedgerEntry.ID != ledgerID ||
			result.ActualMicros != 70 || result.ReleasedMicros != 30 || result.OverageMicros != 0 {
			t.Errorf("concurrent settlement result = %+v", result)
		}
	}
	if nonIdempotent != 1 {
		t.Fatalf("non-idempotent settlement count = %d, want 1", nonIdempotent)
	}
}

func settleBudgetScenario(
	ctx context.Context,
	t *testing.T,
	settler budget.Settler,
	input budget.SettlementInput,
	wantActual, wantReleased, wantOverage uint64,
	wantKind budget.EntryKind,
) budget.SettlementResult {
	t.Helper()
	result, err := settler.Settle(ctx, input)
	if err != nil || result.Idempotent || result.ActualMicros != wantActual ||
		result.ReleasedMicros != wantReleased || result.OverageMicros != wantOverage ||
		result.LedgerEntry.Kind != wantKind || result.ResultReservedMicros != 0 {
		t.Fatalf("Settle(%s) = %+v/%v", input.Outcome, result, err)
	}
	return result
}

func assertSettlementStored(
	ctx context.Context,
	t *testing.T,
	database *sql.DB,
	accountID string,
	wantStatus budget.ReservationStatus,
	wantKind budget.EntryKind,
	wantActual, wantReleased, wantOverage int,
	wantAccountStatus string,
) {
	t.Helper()
	var accountStatus, reservationStatus, entryKind string
	var committed, reserved, actual, released, overage, ledgerCount int
	err := database.QueryRowContext(ctx, `
		SELECT a.status, a.committed_amount_micros, a.reserved_amount_micros,
			r.status, r.actual_amount_micros, r.released_amount_micros, r.overage_amount_micros,
			l.entry_kind,
			(SELECT count(*) FROM app.budget_ledger_entries WHERE account_id = a.id)
		FROM app.budget_accounts a
		JOIN app.budget_reservations r ON r.account_id = a.id
		JOIN app.budget_ledger_entries l ON l.reservation_id = r.id AND l.entry_kind <> 'reserve'
		WHERE a.id = $1`, accountID,
	).Scan(&accountStatus, &committed, &reserved, &reservationStatus,
		&actual, &released, &overage, &entryKind, &ledgerCount)
	if err != nil || accountStatus != wantAccountStatus || committed != wantActual || reserved != 0 ||
		reservationStatus != string(wantStatus) || actual != wantActual || released != wantReleased ||
		overage != wantOverage || entryKind != string(wantKind) || ledgerCount != 2 {
		t.Fatalf("stored settlement %s = account:%s/%d/%d reservation:%s/%d/%d/%d ledger:%s/%d error:%v",
			accountID, accountStatus, committed, reserved, reservationStatus,
			actual, released, overage, entryKind, ledgerCount, err)
	}
}
