//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"testing"
	"time"
)

const (
	budgetTenantOneID  = "78000000-0000-4000-8000-000000000001"
	budgetTenantTwoID  = "78000000-0000-4000-8000-000000000002"
	budgetProjectOneID = "78000000-0000-4000-8000-000000000011"
	budgetProjectTwoID = "78000000-0000-4000-8000-000000000012"
	budgetKeyOneID     = "78000000-0000-4000-8000-000000000021"
	budgetKeyTwoID     = "78000000-0000-4000-8000-000000000022"
	budgetModelOneID   = "78000000-0000-4000-8000-000000000031"
	budgetModelTwoID   = "78000000-0000-4000-8000-000000000032"

	budgetTenantAccountID  = "78000000-0000-4000-8000-000000000101"
	budgetProjectAccountID = "78000000-0000-4000-8000-000000000102"
	budgetKeyAccountID     = "78000000-0000-4000-8000-000000000103"
	budgetUserAccountID    = "78000000-0000-4000-8000-000000000104"
	budgetSessionAccountID = "78000000-0000-4000-8000-000000000105"
	budgetOtherAccountID   = "78000000-0000-4000-8000-000000000106"
	budgetInvalidID        = "78000000-0000-4000-8000-000000000199"
	budgetReservationID    = "78000000-0000-4000-8000-000000000201"
	budgetRequestID        = "integration-budget-request"
)

func TestBudgetLedgerSchemaConstraints(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set")
	}
	database, err := sql.Open("postgres", databaseURL)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	database.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		t.Fatalf("database.PingContext() error = %v", err)
	}
	cleanupBudgetLedgerFixtures(t, database)
	t.Cleanup(func() { cleanupBudgetLedgerFixtures(t, database) })
	seedBudgetLedgerParents(ctx, t, database)

	periodStart := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := periodStart.AddDate(0, 1, 0)
	insertBudgetAccount(ctx, t, database, budgetTenantAccountID, budgetTenantOneID, "tenant", nil, nil, nil, nil, periodStart, periodEnd)
	insertBudgetAccount(ctx, t, database, budgetProjectAccountID, budgetTenantOneID, "project", budgetProjectOneID, nil, nil, nil, periodStart, periodEnd)
	insertBudgetAccount(ctx, t, database, budgetKeyAccountID, budgetTenantOneID, "key", budgetProjectOneID, budgetKeyOneID, nil, nil, periodStart, periodEnd)
	insertBudgetAccount(ctx, t, database, budgetUserAccountID, budgetTenantOneID, "user", nil, nil, "user:sha256:abc", nil, periodStart, periodEnd)
	insertBudgetAccount(ctx, t, database, budgetSessionAccountID, budgetTenantOneID, "session", nil, nil, nil, "session:sha256:def", periodStart, periodEnd)
	insertBudgetAccount(ctx, t, database, budgetOtherAccountID, budgetTenantTwoID, "tenant", nil, nil, nil, nil, periodStart, periodEnd)

	rows, err := database.QueryContext(ctx, `
		SELECT scope_kind, project_id IS NOT NULL, virtual_key_id IS NOT NULL,
		       principal_ref IS NOT NULL, session_ref IS NOT NULL
		FROM app.budget_accounts
		WHERE tenant_id = $1
		ORDER BY scope_kind`, budgetTenantOneID)
	if err != nil {
		t.Fatalf("query budget scopes: %v", err)
	}
	defer func() { _ = rows.Close() }()
	seen := make(map[string][4]bool)
	for rows.Next() {
		var kind string
		var project, key, principal, session bool
		if err := rows.Scan(&kind, &project, &key, &principal, &session); err != nil {
			t.Fatalf("scan budget scope: %v", err)
		}
		seen[kind] = [4]bool{project, key, principal, session}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate budget scopes: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close budget scope rows: %v", err)
	}
	want := map[string][4]bool{
		"tenant": {}, "project": {true, false, false, false}, "key": {true, true, false, false},
		"user": {false, false, true, false}, "session": {false, false, false, true},
	}
	if len(seen) != len(want) {
		t.Fatalf("scope count = %d, want %d: %#v", len(seen), len(want), seen)
	}
	for kind, shape := range want {
		if seen[kind] != shape {
			t.Fatalf("scope %s shape = %v, want %v", kind, seen[kind], shape)
		}
	}

	invalidAccounts := []struct {
		name       string
		kind       string
		projectID  any
		keyID      any
		principal  any
		session    any
		soft       int64
		hard       int64
		committed  int64
		reserved   int64
		constraint string
	}{
		{"shape", "project", nil, nil, nil, nil, 800, 1000, 0, 0, "budget_accounts_scope_shape_valid"},
		{"principal format", "user", nil, nil, "raw user name", nil, 800, 1000, 0, 0, "budget_accounts_principal_ref_format"},
		{"limit order", "tenant", nil, nil, nil, nil, 1001, 1000, 0, 0, "budget_accounts_limits_valid"},
		{"balance overflow", "tenant", nil, nil, nil, nil, 800, 1000, 9007199254740991, 1, "budget_accounts_balances_valid"},
	}
	for _, test := range invalidAccounts {
		t.Run(test.name, func(t *testing.T) {
			_, insertErr := database.ExecContext(ctx, insertBudgetAccountSQL,
				budgetInvalidID, budgetTenantOneID, test.kind, test.projectID, test.keyID,
				test.principal, test.session, periodStart.AddDate(0, 2, 0), periodEnd.AddDate(0, 2, 0),
				test.soft, test.hard, test.committed, test.reserved,
			)
			expectConstraint(t, insertErr, test.constraint)
		})
	}
	_, err = database.ExecContext(ctx, insertBudgetAccountSQL,
		budgetInvalidID, budgetTenantOneID, "project", budgetProjectTwoID, nil, nil, nil,
		periodStart.AddDate(0, 2, 0), periodEnd.AddDate(0, 2, 0), 800, 1000, 0, 0,
	)
	expectConstraint(t, err, "budget_accounts_project_fk")
	_, err = database.ExecContext(ctx, insertBudgetAccountSQL,
		budgetInvalidID, budgetTenantOneID, "key", budgetProjectOneID, budgetKeyTwoID, nil, nil,
		periodStart.AddDate(0, 2, 0), periodEnd.AddDate(0, 2, 0), 800, 1000, 0, 0,
	)
	expectConstraint(t, err, "budget_accounts_virtual_key_fk")
	_, err = database.ExecContext(ctx, insertBudgetAccountSQL,
		budgetInvalidID, budgetTenantOneID, "tenant", nil, nil, nil, nil,
		periodStart, periodEnd, 800, 1000, 0, 0,
	)
	expectConstraint(t, err, "uq_budget_accounts_tenant_period")

	_, err = database.ExecContext(ctx, `
		INSERT INTO app.budget_reservations (
			id, tenant_id, account_id, request_id, idempotency_key,
			reserved_amount_micros, expires_at, created_by, updated_by
		) VALUES ($1, $2, $3, $4, 'reserve:tenant:1', 100,
			CURRENT_TIMESTAMP + INTERVAL '5 minutes', 'integration:budget', 'integration:budget')`,
		budgetReservationID, budgetTenantOneID, budgetTenantAccountID, budgetRequestID,
	)
	if err != nil {
		t.Fatalf("insert budget reservation: %v", err)
	}
	_, err = database.ExecContext(ctx, `
		INSERT INTO app.budget_reservations (
			id, tenant_id, account_id, request_id, idempotency_key,
			reserved_amount_micros, expires_at, created_by, updated_by
		) VALUES ($1, $2, $3, $4, 'reserve:tenant:1', 100,
			CURRENT_TIMESTAMP + INTERVAL '5 minutes', 'integration:budget', 'integration:budget')`,
		budgetInvalidID, budgetTenantOneID, budgetTenantAccountID, budgetRequestID,
	)
	expectConstraint(t, err, "budget_reservations_account_idempotency_unique")
	_, err = database.ExecContext(ctx, `
		INSERT INTO app.budget_reservations (
			id, tenant_id, account_id, request_id, idempotency_key, status,
			reserved_amount_micros, actual_amount_micros, expires_at, created_by, updated_by
		) VALUES ($1, $2, $3, $4, 'reserve:invalid-terminal', 'settled',
			100, 60, CURRENT_TIMESTAMP + INTERVAL '5 minutes', 'integration:budget', 'integration:budget')`,
		budgetInvalidID, budgetTenantOneID, budgetTenantAccountID, budgetRequestID,
	)
	expectConstraint(t, err, "budget_reservations_terminal_amounts_valid")
	_, err = database.ExecContext(ctx, `
		INSERT INTO app.budget_reservations (
			id, tenant_id, account_id, request_id, idempotency_key,
			reserved_amount_micros, expires_at, created_by, updated_by
		) VALUES ($1, $2, $3, $4, 'reserve:cross-tenant', 100,
			CURRENT_TIMESTAMP + INTERVAL '5 minutes', 'integration:budget', 'integration:budget')`,
		budgetInvalidID, budgetTenantOneID, budgetOtherAccountID, budgetRequestID,
	)
	expectConstraint(t, err, "budget_reservations_account_fk")

	_, err = database.ExecContext(ctx, `
		INSERT INTO app.budget_ledger_entries (
			tenant_id, account_id, reservation_id, entry_kind, idempotency_key,
			committed_delta_micros, reserved_delta_micros,
			result_committed_micros, result_reserved_micros, occurred_at, created_by
		) VALUES ($1, $2, $3, 'reserve', 'ledger:reserve:1', 0, 100, 0, 100,
			CURRENT_TIMESTAMP, 'integration:budget')`,
		budgetTenantOneID, budgetTenantAccountID, budgetReservationID,
	)
	if err != nil {
		t.Fatalf("insert reserve ledger entry: %v", err)
	}
	_, err = database.ExecContext(ctx, `
		INSERT INTO app.budget_ledger_entries (
			tenant_id, account_id, reservation_id, entry_kind, idempotency_key,
			committed_delta_micros, reserved_delta_micros,
			result_committed_micros, result_reserved_micros, occurred_at, created_by
		) VALUES ($1, $2, $3, 'release', 'ledger:wrong-account', 0, -100, 0, 0,
			CURRENT_TIMESTAMP, 'integration:budget')`,
		budgetTenantOneID, budgetProjectAccountID, budgetReservationID,
	)
	expectConstraint(t, err, "budget_ledger_entries_reservation_fk")
	_, err = database.ExecContext(ctx, `UPDATE app.budget_ledger_entries SET created_by = 'changed'`)
	expectExecutionSQLState(t, err, "23514")
	_, err = database.ExecContext(ctx, `DELETE FROM app.budget_ledger_entries`)
	expectExecutionSQLState(t, err, "23514")

	_, err = database.ExecContext(ctx, `
		UPDATE app.budget_accounts
		SET reserved_amount_micros = 100, version = version + 1,
		    updated_at = CURRENT_TIMESTAMP, updated_by = 'integration:budget'
		WHERE id = $1`, budgetTenantAccountID)
	if err != nil {
		t.Fatalf("advance budget account balance: %v", err)
	}
	_, err = database.ExecContext(ctx, `
		UPDATE app.budget_accounts
		SET hard_limit_micros = 2000, version = version + 1,
		    updated_at = CURRENT_TIMESTAMP, updated_by = 'integration:budget'
		WHERE id = $1`, budgetTenantAccountID)
	expectExecutionSQLState(t, err, "23514")
	_, err = database.ExecContext(ctx, `
		UPDATE app.budget_accounts
		SET committed_amount_micros = 1, updated_at = CURRENT_TIMESTAMP,
		    updated_by = 'integration:budget'
		WHERE id = $1`, budgetProjectAccountID)
	expectExecutionSQLState(t, err, "23514")

	_, err = database.ExecContext(ctx, `
		UPDATE app.budget_reservations
		SET status = 'settled', actual_amount_micros = 60,
		    released_amount_micros = 40, overage_amount_micros = 0,
		    terminal_at = CURRENT_TIMESTAMP, version = version + 1,
		    updated_at = CURRENT_TIMESTAMP, updated_by = 'integration:budget'
		WHERE id = $1`, budgetReservationID)
	if err != nil {
		t.Fatalf("settle budget reservation: %v", err)
	}
	_, err = database.ExecContext(ctx, `
		UPDATE app.budget_reservations
		SET status = 'expired', actual_amount_micros = 0,
		    released_amount_micros = 100, overage_amount_micros = 0,
		    terminal_at = CURRENT_TIMESTAMP, version = version + 1,
		    updated_at = CURRENT_TIMESTAMP, updated_by = 'integration:budget'
		WHERE id = $1`, budgetReservationID)
	expectExecutionSQLState(t, err, "23514")

	var accountCount, reservationCount, ledgerCount int
	err = database.QueryRowContext(ctx, `
		SELECT
			(SELECT count(*) FROM app.budget_accounts WHERE tenant_id = $1),
			(SELECT count(*) FROM app.budget_reservations WHERE tenant_id = $1),
			(SELECT count(*) FROM app.budget_ledger_entries WHERE tenant_id = $1)`,
		budgetTenantOneID,
	).Scan(&accountCount, &reservationCount, &ledgerCount)
	if err != nil || accountCount != 5 || reservationCount != 1 || ledgerCount != 1 {
		t.Fatalf("stored budget facts = accounts:%d reservations:%d ledger:%d error:%v", accountCount, reservationCount, ledgerCount, err)
	}
}

const insertBudgetAccountSQL = `
	INSERT INTO app.budget_accounts (
		id, tenant_id, scope_kind, project_id, virtual_key_id, principal_ref, session_ref,
		currency, period_start, period_end, soft_limit_micros, hard_limit_micros,
		committed_amount_micros, reserved_amount_micros, created_by, updated_by
	) VALUES ($1, $2, $3, $4, $5, $6, $7, 'USD', $8, $9, $10, $11, $12, $13,
		'integration:budget', 'integration:budget')`

func insertBudgetAccount(
	ctx context.Context,
	t *testing.T,
	database *sql.DB,
	id, tenantID, kind string,
	projectID, keyID, principalRef, sessionRef any,
	periodStart, periodEnd time.Time,
) {
	t.Helper()
	_, err := database.ExecContext(ctx, insertBudgetAccountSQL,
		id, tenantID, kind, projectID, keyID, principalRef, sessionRef,
		periodStart, periodEnd, 800, 1000, 0, 0,
	)
	if err != nil {
		t.Fatalf("insert %s budget account: %v", kind, err)
	}
}

func seedBudgetLedgerParents(ctx context.Context, t *testing.T, database *sql.DB) {
	t.Helper()
	insertTenant(ctx, t, database, budgetTenantOneID, "budget-tenant-one", "Budget Tenant One", "")
	insertTenant(ctx, t, database, budgetTenantTwoID, "budget-tenant-two", "Budget Tenant Two", "")
	insertProject(ctx, t, database, budgetProjectOneID, budgetTenantOneID, "budget-project-one", "Budget Project One", "")
	insertProject(ctx, t, database, budgetProjectTwoID, budgetTenantTwoID, "budget-project-two", "Budget Project Two", "")
	for _, model := range []struct{ id, tenantID, name string }{
		{budgetModelOneID, budgetTenantOneID, "budget-model"},
		{budgetModelTwoID, budgetTenantTwoID, "budget-model"},
	} {
		_, err := database.ExecContext(ctx, `
			INSERT INTO app.logical_models (
				id, tenant_id, name, display_name, required_capabilities, created_by, updated_by
			) VALUES ($1, $2, $3, $3, '{"chat":true}', 'integration:budget', 'integration:budget')`,
			model.id, model.tenantID, model.name,
		)
		if err != nil {
			t.Fatalf("insert budget logical model: %v", err)
		}
	}
	for _, key := range []struct {
		id, tenantID, projectID, prefix string
		fill                            byte
	}{
		{budgetKeyOneID, budgetTenantOneID, budgetProjectOneID, "agw_test_budget001", 0x81},
		{budgetKeyTwoID, budgetTenantTwoID, budgetProjectTwoID, "agw_test_budget002", 0x82},
	} {
		_, err := database.ExecContext(ctx, `
			INSERT INTO app.virtual_api_keys (
				id, tenant_id, project_id, key_prefix, secret_hash, hash_key_version,
				created_by, updated_by
			) VALUES ($1, $2, $3, $4, $5, 'budget-v1', 'integration:budget', 'integration:budget')`,
			key.id, key.tenantID, key.projectID, key.prefix, bytes.Repeat([]byte{key.fill}, 32),
		)
		if err != nil {
			t.Fatalf("insert budget virtual key: %v", err)
		}
	}
	_, err := database.ExecContext(ctx, `
		INSERT INTO app.gateway_requests (
			id, tenant_id, project_id, virtual_key_id, logical_model,
			trace_id, span_id, status, started_at, updated_at
		) VALUES ($1, $2, $3, $4, 'budget-model',
			'88888888888888888888888888888888', '9999999999999999', 'authorized',
			CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		budgetRequestID, budgetTenantOneID, budgetProjectOneID, budgetKeyOneID,
	)
	if err != nil {
		t.Fatalf("insert budget gateway request: %v", err)
	}
}

func cleanupBudgetLedgerFixtures(t *testing.T, database *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	statements := []struct {
		name  string
		query string
		args  []any
	}{
		{"usage event outbox", `TRUNCATE app.usage_event_outbox`, nil},
		{"budget tables", `TRUNCATE app.budget_ledger_entries, app.budget_reservations, app.budget_accounts RESTART IDENTITY`, nil},
		{"route decisions", `DELETE FROM app.route_decisions WHERE request_id LIKE 'integration-budget-%'`, nil},
		{"request events", `DELETE FROM app.gateway_request_status_events WHERE request_id LIKE 'integration-budget-%'`, nil},
		{"route attempts", `DELETE FROM app.route_attempts WHERE request_id LIKE 'integration-budget-%'`, nil},
		{"gateway request", `DELETE FROM app.gateway_requests WHERE id LIKE 'integration-budget-%'`, nil},
		{"keys", `DELETE FROM app.virtual_api_keys WHERE tenant_id IN ($1, $2)`, []any{budgetTenantOneID, budgetTenantTwoID}},
		{"logical models", `DELETE FROM app.logical_models WHERE tenant_id IN ($1, $2)`, []any{budgetTenantOneID, budgetTenantTwoID}},
		{"projects", `DELETE FROM app.projects WHERE tenant_id IN ($1, $2)`, []any{budgetTenantOneID, budgetTenantTwoID}},
		{"tenants", `DELETE FROM app.tenants WHERE id IN ($1, $2)`, []any{budgetTenantOneID, budgetTenantTwoID}},
	}
	for _, statement := range statements {
		if _, err := database.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Errorf("cleanup %s: %v", statement.name, err)
		}
	}
}
