//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/budget"
)

const budgetLimitNoticeAccountID = "78000000-0000-4000-8000-000000000601"

func TestPostgresBudgetSoftNoticeAndHardErrorIsolation(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set")
	}
	database, err := sql.Open("postgres", databaseURL)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	database.SetMaxOpenConns(8)
	database.SetMaxIdleConns(8)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		t.Fatalf("database.PingContext() error = %v", err)
	}
	cleanupBudgetLedgerFixtures(t, database)
	t.Cleanup(func() { cleanupBudgetLedgerFixtures(t, database) })
	seedBudgetLedgerParents(ctx, t, database)

	periodStart := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	periodEnd := periodStart.Add(2 * time.Hour)
	insertAtomicBudgetAccount(ctx, t, database, budgetLimitNoticeAccountID,
		"user", nil, nil, "user:budget-limit-notice", nil, 60, 100, periodStart, periodEnd)
	requestIDs := []string{
		"integration-budget-limit-at-soft",
		"integration-budget-limit-over-soft",
		"integration-budget-limit-hard",
	}
	for _, requestID := range requestIDs {
		insertSettlementRequest(ctx, t, database, requestID)
	}
	reserver, err := budget.NewPostgresReserver(database, time.Now, rand.Reader, budget.ReserveOptions{})
	if err != nil {
		t.Fatalf("budget.NewPostgresReserver() error = %v", err)
	}
	expiresAt := time.Now().UTC().Add(10 * time.Minute).Truncate(time.Microsecond)
	atSoft, err := reserver.Reserve(ctx, budget.ReserveInput{
		TenantID: budgetTenantOneID, AccountID: budgetLimitNoticeAccountID,
		RequestID: requestIDs[0], IdempotencyKey: "reserve:budget-limit-at-soft",
		AmountMicros: 60, ExpiresAt: expiresAt, Actor: "integration:budget-limit",
		DegradationHint: budget.DegradeLowerCostModel,
	})
	if err != nil || atSoft.SoftLimitExceeded || atSoft.LimitNotice != nil || atSoft.RemainingHardMicros != 40 {
		t.Fatalf("at-soft Reserve() = %+v/%v", atSoft, err)
	}
	overSoft, err := reserver.Reserve(ctx, budget.ReserveInput{
		TenantID: budgetTenantOneID, AccountID: budgetLimitNoticeAccountID,
		RequestID: requestIDs[1], IdempotencyKey: "reserve:budget-limit-over-soft",
		AmountMicros: 10, ExpiresAt: expiresAt, Actor: "integration:budget-limit",
		DegradationHint: budget.DegradeReduceMaxOutput,
	})
	if err != nil || !overSoft.SoftLimitExceeded || overSoft.LimitNotice == nil ||
		overSoft.LimitNotice.Level != budget.LimitSoft ||
		overSoft.LimitNotice.RemainingMicros != 30 || !overSoft.LimitNotice.ResetAt.Equal(periodEnd) ||
		overSoft.LimitNotice.DegradationHint != budget.DegradeReduceMaxOutput ||
		overSoft.LimitNotice.Validate() != nil {
		t.Fatalf("over-soft Reserve() = %+v/%v", overSoft, err)
	}

	_, hardErr := reserver.Reserve(ctx, budget.ReserveInput{
		TenantID: budgetTenantOneID, AccountID: budgetLimitNoticeAccountID,
		RequestID: requestIDs[2], IdempotencyKey: "reserve:budget-limit-hard",
		AmountMicros: 31, ExpiresAt: expiresAt, Actor: "integration:budget-limit",
		DegradationHint: budget.DegradeWaitForReset,
	})
	var exceeded *budget.HardLimitError
	if !errors.Is(hardErr, budget.ErrBudgetExceeded) || !errors.As(hardErr, &exceeded) ||
		exceeded.Notice.Level != budget.LimitHard || exceeded.Notice.RemainingMicros != 30 ||
		!exceeded.Notice.ResetAt.Equal(periodEnd) ||
		exceeded.Notice.DegradationHint != budget.DegradeWaitForReset || exceeded.Notice.Validate() != nil {
		t.Fatalf("hard Reserve() error = %#v", hardErr)
	}
	assertBudgetLimitErrorSafe(t, exceeded, requestIDs[2])

	crossTenant := budget.ReserveInput{
		TenantID: budgetTenantTwoID, AccountID: budgetLimitNoticeAccountID,
		RequestID: requestIDs[2], IdempotencyKey: "reserve:budget-limit-cross-tenant",
		AmountMicros: 1, ExpiresAt: expiresAt, Actor: "integration:budget-limit",
		DegradationHint: budget.DegradeLowerCostModel,
	}
	_, crossErr := reserver.Reserve(ctx, crossTenant)
	var leaked *budget.HardLimitError
	if !errors.Is(crossErr, budget.ErrAccountNotFound) || errors.As(crossErr, &leaked) {
		t.Fatalf("cross-tenant Reserve() error = %#v", crossErr)
	}
	if strings.Contains(crossErr.Error(), budgetLimitNoticeAccountID) || strings.Contains(crossErr.Error(), budgetTenantOneID) {
		t.Fatalf("cross-tenant error leaked identity: %q", crossErr)
	}

	var reserved, version, reservations, ledger int
	err = database.QueryRowContext(ctx, `
		SELECT a.reserved_amount_micros, a.version,
			(SELECT count(*) FROM app.budget_reservations WHERE account_id = a.id),
			(SELECT count(*) FROM app.budget_ledger_entries WHERE account_id = a.id)
		FROM app.budget_accounts a WHERE a.id = $1`, budgetLimitNoticeAccountID,
	).Scan(&reserved, &version, &reservations, &ledger)
	if err != nil || reserved != 70 || version != 3 || reservations != 2 || ledger != 2 {
		t.Fatalf("budget limit stored facts = %d/%d/%d/%d error:%v",
			reserved, version, reservations, ledger, err)
	}
}

func assertBudgetLimitErrorSafe(t *testing.T, failure *budget.HardLimitError, requestID string) {
	t.Helper()
	encoded, err := json.Marshal(failure)
	if err != nil {
		t.Fatalf("json.Marshal(BudgetExceededError) error = %v", err)
	}
	text := string(encoded)
	for _, forbidden := range []string{
		budgetTenantOneID, budgetTenantTwoID, budgetLimitNoticeAccountID, requestID,
		"tenant_id", "account_id", "request_id", "project_id", "virtual_key_id",
	} {
		if strings.Contains(text, forbidden) || strings.Contains(failure.Error(), forbidden) {
			t.Fatalf("budget limit error leaked %q: error=%q json=%s", forbidden, failure.Error(), text)
		}
	}
	for _, required := range []string{
		`"level":"hard"`, `"remaining_micros":30`, `"reset_at":`, `"wait_for_reset"`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("budget limit error JSON %s missing %s", text, required)
		}
	}
}
