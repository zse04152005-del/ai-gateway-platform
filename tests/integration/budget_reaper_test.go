//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/budget"
)

const (
	budgetReaperOpenAccountID   = "78000000-0000-4000-8000-000000000501"
	budgetReaperClosedAccountID = "78000000-0000-4000-8000-000000000502"
	budgetReaperRaceAccountID   = "78000000-0000-4000-8000-000000000503"
	budgetReaperRaceRequestID   = "integration-budget-reaper-race"
	budgetReaperRaceReserveID   = "78000000-0000-4000-8000-000000000599"
)

func TestPostgresBudgetExpiredReservationReaper(t *testing.T) {
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

	now := time.Now().UTC().Truncate(time.Microsecond)
	insertReaperAccount(ctx, t, database, budgetReaperOpenAccountID, 140, "open", now.Add(-time.Hour))
	insertReaperAccount(ctx, t, database, budgetReaperClosedAccountID, 20, "closed", now.Add(-2*time.Hour))
	for index := range 12 {
		requestID := fmt.Sprintf("integration-budget-reaper-open-%02d", index)
		reservationID := fmt.Sprintf("78000000-0000-4000-8000-%012d", 510+index)
		insertReaperHold(ctx, t, database, budgetReaperOpenAccountID, requestID, reservationID,
			10, uint64((index+1)*10), now.Add(-5*time.Minute), now.Add(-time.Minute))
	}
	insertReaperHold(ctx, t, database, budgetReaperOpenAccountID,
		"integration-budget-reaper-live", "78000000-0000-4000-8000-000000000530",
		20, 140, now.Add(-time.Minute), now.Add(10*time.Minute))
	for index := range 4 {
		requestID := fmt.Sprintf("integration-budget-reaper-closed-%02d", index)
		reservationID := fmt.Sprintf("78000000-0000-4000-8000-%012d", 540+index)
		insertReaperHold(ctx, t, database, budgetReaperClosedAccountID, requestID, reservationID,
			5, uint64((index+1)*5), now.Add(-5*time.Minute), now.Add(-time.Minute))
	}

	reaper, err := budget.NewPostgresReaper(database, budget.ReaperOptions{BatchSize: 3})
	if err != nil {
		t.Fatalf("budget.NewPostgresReaper() error = %v", err)
	}
	first, err := reaper.Reap(ctx, "integration:budget-reaper")
	if err != nil || len(first.Events) != 3 || !first.AtCapacity {
		t.Fatalf("first Reap() = %+v/%v", first, err)
	}

	events := append([]budget.ExpirationEvent(nil), first.Events...)
	const workers = 8
	start := make(chan struct{})
	results := make(chan budget.ReapResult, workers)
	errorsFound := make(chan error, workers)
	var wait sync.WaitGroup
	wait.Add(workers)
	for range workers {
		go func() {
			defer wait.Done()
			<-start
			result, reapErr := reaper.Reap(ctx, "integration:budget-reaper")
			if reapErr != nil {
				errorsFound <- reapErr
				return
			}
			results <- result
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errorsFound)
	for reapErr := range errorsFound {
		t.Errorf("concurrent Reap() error = %v", reapErr)
	}
	for result := range results {
		events = append(events, result.Events...)
	}
	for len(events) < 16 {
		result, reapErr := reaper.Reap(ctx, "integration:budget-reaper")
		if reapErr != nil {
			t.Fatalf("drain Reap() error = %v", reapErr)
		}
		if len(result.Events) == 0 {
			break
		}
		events = append(events, result.Events...)
	}
	assertReaperEvents(t, events, 16)
	empty, err := reaper.Reap(ctx, "integration:budget-reaper")
	if err != nil || len(empty.Events) != 0 || empty.AtCapacity {
		t.Fatalf("empty Reap() = %+v/%v", empty, err)
	}
	assertReaperStoredFacts(ctx, t, database)

	testSettlementExpirationRace(ctx, t, database, now)
}

func insertReaperAccount(
	ctx context.Context,
	t *testing.T,
	database *sql.DB,
	accountID string,
	reserved int64,
	status string,
	periodStart time.Time,
) {
	t.Helper()
	createdAt := periodStart.Add(-time.Hour)
	var closedAt any
	if status == "closed" {
		closedAt = periodStart.Add(30 * time.Minute)
	}
	_, err := database.ExecContext(ctx, `
		INSERT INTO app.budget_accounts (
			id, tenant_id, scope_kind, currency, period_start, period_end,
			soft_limit_micros, hard_limit_micros,
			committed_amount_micros, reserved_amount_micros,
			status, version, created_at, created_by, updated_at, updated_by, closed_at
		) VALUES ($1, $2, 'tenant', 'USD', $3, $4, 800, 1000, 0, $5,
			$6, 1, $7, 'integration:budget-reaper', $7, 'integration:budget-reaper', $8)`,
		accountID, budgetTenantOneID, periodStart, periodStart.Add(4*time.Hour),
		reserved, status, createdAt, closedAt,
	)
	if err != nil {
		t.Fatalf("insert reaper account %s: %v", accountID, err)
	}
}

func insertReaperHold(
	ctx context.Context,
	t *testing.T,
	database *sql.DB,
	accountID, requestID, reservationID string,
	amount, resultReserved uint64,
	createdAt, expiresAt time.Time,
) {
	t.Helper()
	insertSettlementRequest(ctx, t, database, requestID)
	idempotencyKey := "reserve:" + requestID
	_, err := database.ExecContext(ctx, `
		INSERT INTO app.budget_reservations (
			id, tenant_id, account_id, request_id, idempotency_key, status,
			reserved_amount_micros, expires_at, version,
			created_at, created_by, updated_at, updated_by
		) VALUES ($1, $2, $3, $4, $5, 'pending', $6, $7, 1,
			$8, 'integration:budget-reaper', $8, 'integration:budget-reaper')`,
		reservationID, budgetTenantOneID, accountID, requestID, idempotencyKey,
		amount, expiresAt, createdAt,
	)
	if err != nil {
		t.Fatalf("insert reaper reservation %s: %v", reservationID, err)
	}
	_, err = database.ExecContext(ctx, `
		INSERT INTO app.budget_ledger_entries (
			tenant_id, account_id, reservation_id, entry_kind, idempotency_key,
			committed_delta_micros, reserved_delta_micros,
			result_committed_micros, result_reserved_micros, occurred_at, created_by
		) VALUES ($1, $2, $3, 'reserve', $4, 0, $5, 0, $6, $7, 'integration:budget-reaper')`,
		budgetTenantOneID, accountID, reservationID, idempotencyKey,
		amount, resultReserved, createdAt,
	)
	if err != nil {
		t.Fatalf("insert reaper reserve ledger %s: %v", reservationID, err)
	}
}

func assertReaperEvents(t *testing.T, events []budget.ExpirationEvent, want int) {
	t.Helper()
	if len(events) != want {
		t.Fatalf("expiration event count = %d, want %d", len(events), want)
	}
	eventIDs := make(map[string]struct{}, want)
	ledgerIDs := make(map[int64]struct{}, want)
	for _, event := range events {
		if event.EventID != "expire:"+event.Reservation.ID || event.Reservation.Status != budget.ReservationExpired ||
			event.LedgerEntry.Kind != budget.EntryExpire || event.ReleasedMicros != event.Reservation.ReservedMicros ||
			event.ResultCommittedMicros != 0 || event.Attempts < 1 || event.OccurredAt.Before(event.Reservation.ExpiresAt) {
			t.Errorf("expiration event = %+v", event)
		}
		if _, duplicate := eventIDs[event.EventID]; duplicate {
			t.Errorf("duplicate expiration event %s", event.EventID)
		}
		if _, duplicate := ledgerIDs[event.LedgerEntry.ID]; duplicate {
			t.Errorf("duplicate expiration ledger %d", event.LedgerEntry.ID)
		}
		eventIDs[event.EventID] = struct{}{}
		ledgerIDs[event.LedgerEntry.ID] = struct{}{}
	}
}

func assertReaperStoredFacts(ctx context.Context, t *testing.T, database *sql.DB) {
	t.Helper()
	var expired, pending, expireEntries int
	err := database.QueryRowContext(ctx, `
		SELECT
			count(*) FILTER (WHERE status = 'expired'),
			count(*) FILTER (WHERE status = 'pending'),
			(SELECT count(*) FROM app.budget_ledger_entries WHERE entry_kind = 'expire')
		FROM app.budget_reservations`).Scan(&expired, &pending, &expireEntries)
	if err != nil || expired != 16 || pending != 1 || expireEntries != 16 {
		t.Fatalf("stored reaper counts = expired:%d pending:%d ledger:%d error:%v",
			expired, pending, expireEntries, err)
	}
	for _, expected := range []struct {
		accountID         string
		status            string
		reserved, version int
	}{
		{budgetReaperOpenAccountID, "open", 20, 13},
		{budgetReaperClosedAccountID, "closed", 0, 5},
	} {
		var status string
		var committed, reserved, version int
		err := database.QueryRowContext(ctx, `
			SELECT status, committed_amount_micros, reserved_amount_micros, version
			FROM app.budget_accounts WHERE id = $1`, expected.accountID,
		).Scan(&status, &committed, &reserved, &version)
		if err != nil || status != expected.status || committed != 0 ||
			reserved != expected.reserved || version != expected.version {
			t.Fatalf("reaped account %s = %s/%d/%d/%d error:%v",
				expected.accountID, status, committed, reserved, version, err)
		}
	}
}

func testSettlementExpirationRace(
	ctx context.Context,
	t *testing.T,
	database *sql.DB,
	now time.Time,
) {
	t.Helper()
	insertReaperAccount(ctx, t, database, budgetReaperRaceAccountID, 100, "open", now.Add(-3*time.Hour))
	insertReaperHold(ctx, t, database, budgetReaperRaceAccountID,
		budgetReaperRaceRequestID, budgetReaperRaceReserveID,
		100, 100, now.Add(-5*time.Minute), now.Add(-time.Minute))
	completeSettlementRequest(ctx, t, database, budgetReaperRaceRequestID, "succeeded", 1)
	reaper, err := budget.NewPostgresReaper(database, budget.ReaperOptions{BatchSize: 1})
	if err != nil {
		t.Fatalf("budget.NewPostgresReaper(race) error = %v", err)
	}
	settler, err := budget.NewPostgresSettler(database, budget.SettlementOptions{})
	if err != nil {
		t.Fatalf("budget.NewPostgresSettler(race) error = %v", err)
	}
	settlementInput := budget.SettlementInput{
		TenantID: budgetTenantOneID, AccountID: budgetReaperRaceAccountID,
		ReservationID: budgetReaperRaceReserveID, RequestID: budgetReaperRaceRequestID,
		Outcome: budget.SettlementSucceeded,
		Charges: []budget.SettlementCharge{{
			Kind: budget.ChargeAttempt, ReferenceID: "78000000-0000-4000-8000-000000000598", AmountMicros: 60,
		}},
		Actor: "integration:budget-reaper-race",
	}
	start := make(chan struct{})
	type settlementResult struct {
		value budget.SettlementResult
		err   error
	}
	settled := make(chan settlementResult, 1)
	reaped := make(chan struct {
		value budget.ReapResult
		err   error
	}, 1)
	go func() {
		<-start
		value, settleErr := settler.Settle(ctx, settlementInput)
		settled <- settlementResult{value: value, err: settleErr}
	}()
	go func() {
		<-start
		value, reapErr := reaper.Reap(ctx, "integration:budget-reaper-race")
		reaped <- struct {
			value budget.ReapResult
			err   error
		}{value: value, err: reapErr}
	}()
	close(start)
	settlement := <-settled
	expiration := <-reaped
	if expiration.err != nil {
		t.Fatalf("race Reap() error = %v", expiration.err)
	}
	if settlement.err == nil {
		if settlement.value.Idempotent || len(expiration.value.Events) != 0 {
			t.Fatalf("settlement-won race = settlement:%+v expiration:%+v", settlement.value, expiration.value)
		}
	} else if !errors.Is(settlement.err, budget.ErrSettlementConflict) || len(expiration.value.Events) != 1 {
		t.Fatalf("expiration-won race = settlement:%v expiration:%+v", settlement.err, expiration.value)
	}
	var reservationStatus, entryKind string
	var committed, reserved, ledgerCount int
	err = database.QueryRowContext(ctx, `
		SELECT r.status, terminal.entry_kind,
			a.committed_amount_micros, a.reserved_amount_micros,
			(SELECT count(*) FROM app.budget_ledger_entries WHERE account_id = a.id)
		FROM app.budget_accounts a
		JOIN app.budget_reservations r ON r.account_id = a.id
		JOIN app.budget_ledger_entries terminal
			ON terminal.reservation_id = r.id AND terminal.entry_kind <> 'reserve'
		WHERE a.id = $1`, budgetReaperRaceAccountID,
	).Scan(&reservationStatus, &entryKind, &committed, &reserved, &ledgerCount)
	if err != nil || reserved != 0 || ledgerCount != 2 ||
		(reservationStatus == "settled" && (entryKind != "settle" || committed != 60)) ||
		(reservationStatus == "expired" && (entryKind != "expire" || committed != 0)) ||
		(reservationStatus != "settled" && reservationStatus != "expired") {
		t.Fatalf("stored settlement/expiration race = %s/%s/%d/%d/%d error:%v",
			reservationStatus, entryKind, committed, reserved, ledgerCount, err)
	}
}
