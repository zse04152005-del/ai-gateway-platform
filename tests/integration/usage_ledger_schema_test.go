//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/execution"
)

const (
	usageRequestOneID = "integration-usage-request-one"
	usageRequestTwoID = "integration-usage-request-two"
	usageEventOneID   = "79000000-0000-4000-8000-000000000101"
	usageEventTwoID   = "79000000-0000-4000-8000-000000000102"
	usageEventNextID  = "79000000-0000-4000-8000-000000000103"
)

func TestUsageLedgerSchemaConstraints(t *testing.T) {
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

	cleanupUsageLedgerFixtures(t, database)
	cleanupModelListFixtures(t, database)
	t.Cleanup(func() {
		cleanupUsageLedgerFixtures(t, database)
		cleanupModelListFixtures(t, database)
	})
	seedModelListCatalog(ctx, t, database)
	seedExecutionVirtualKey(ctx, t, database)

	now := time.Now().UTC().Truncate(time.Microsecond)
	seedUsagePriceVersion(ctx, t, database, usagePriceVersionID, modelListDeploymentAID, now.Add(-time.Hour))
	recorder, err := execution.NewPostgresRecorder(database, func() time.Time {
		now = now.Add(time.Microsecond)
		return now
	}, rand.Reader)
	if err != nil {
		t.Fatalf("execution.NewPostgresRecorder() error = %v", err)
	}
	requestOne := startExecutionRequest(ctx, t, recorder, usageRequestOneID)
	requestOne, err = recorder.MarkRouting(ctx, requestOne)
	if err != nil {
		t.Fatalf("MarkRouting(first request) error = %v", err)
	}
	_, attemptOne, err := recorder.StartAttempt(ctx, requestOne, modelListDeploymentAID)
	if err != nil {
		t.Fatalf("StartAttempt(first request) error = %v", err)
	}
	_ = startExecutionRequest(ctx, t, recorder, usageRequestTwoID)

	observedAt := now.Add(time.Second)
	insertUsageLedgerEntry(ctx, t, database, usageEventOneID, modelListTenantOneID,
		usageRequestOneID, nil, "cache_read", 7, "provider", observedAt)
	insertUsageLedgerEntry(ctx, t, database, usageEventTwoID, modelListTenantOneID,
		usageRequestOneID, attemptOne.ID, "input", 11, "estimated", observedAt.Add(time.Microsecond))

	_, err = database.ExecContext(ctx, insertUsageLedgerSQL,
		usageEventOneID, modelListTenantOneID, usageRequestTwoID, nil,
		"output", int64(13), "provider", usagePriceVersionID, int64(13), observedAt, "integration:usage")
	expectConstraint(t, err, "usage_ledger_entries_event_id_unique")

	_, err = database.ExecContext(ctx, insertUsageLedgerSQL,
		usageEventNextID, modelListTenantOneID, usageRequestTwoID, attemptOne.ID,
		"output", int64(13), "provider", usagePriceVersionID, int64(13), observedAt, "integration:usage")
	expectConstraint(t, err, "usage_ledger_entries_attempt_fk")

	_, err = database.ExecContext(ctx, insertUsageLedgerSQL,
		usageEventNextID, modelListTenantTwoID, usageRequestOneID, nil,
		"output", int64(13), "provider", usagePriceVersionID, int64(13), observedAt, "integration:usage")
	expectConstraint(t, err, "usage_ledger_entries_request_fk")

	invalidEntries := []struct {
		name       string
		eventID    string
		tokenType  string
		quantity   int64
		source     string
		constraint string
	}{
		{name: "zero quantity", eventID: "79000000-0000-4000-8000-000000000201", tokenType: "output", quantity: 0, source: "provider", constraint: "usage_ledger_entries_quantity_valid"},
		{name: "unsafe token type", eventID: "79000000-0000-4000-8000-000000000202", tokenType: "Input", quantity: 1, source: "provider", constraint: "usage_ledger_entries_token_type_valid"},
		{name: "unsafe source", eventID: "79000000-0000-4000-8000-000000000203", tokenType: "output", quantity: 1, source: "provider/raw", constraint: "usage_ledger_entries_source_valid"},
	}
	for _, test := range invalidEntries {
		t.Run(test.name, func(t *testing.T) {
			_, insertErr := database.ExecContext(ctx, insertUsageLedgerSQL,
				test.eventID, modelListTenantOneID, usageRequestOneID, attemptOne.ID,
				test.tokenType, test.quantity, test.source, usagePriceVersionID, int64(0),
				observedAt, "integration:usage")
			expectConstraint(t, insertErr, test.constraint)
		})
	}
	_, err = database.ExecContext(ctx, insertUsageLedgerSQL,
		"79000000-0000-4000-8000-000000000204", modelListTenantOneID, usageRequestOneID, attemptOne.ID,
		"output", int64(9007199254740992), "provider", usagePriceVersionID, int64(0),
		observedAt, "integration:usage")
	expectConstraint(t, err, "usage_ledger_entries_quantity_valid")

	_, err = database.ExecContext(ctx, `
		UPDATE app.usage_ledger_entries SET quantity = quantity + 1 WHERE event_id = $1`, usageEventOneID)
	expectExecutionSQLState(t, err, "23514")
	_, err = database.ExecContext(ctx, `
		DELETE FROM app.usage_ledger_entries WHERE event_id = $1`, usageEventTwoID)
	expectExecutionSQLState(t, err, "23514")

	var entryCount, attemptCount, unsafeColumnCount int
	var totalQuantity int64
	err = database.QueryRowContext(ctx, `
		SELECT count(*), sum(quantity), count(attempt_id)
		FROM app.usage_ledger_entries
		WHERE tenant_id = $1 AND request_id = $2`,
		modelListTenantOneID, usageRequestOneID,
	).Scan(&entryCount, &totalQuantity, &attemptCount)
	if err != nil || entryCount != 2 || totalQuantity != 18 || attemptCount != 1 {
		t.Fatalf("stored usage facts = entries:%d quantity:%d attempts:%d error:%v",
			entryCount, totalQuantity, attemptCount, err)
	}
	err = database.QueryRowContext(ctx, `
		SELECT count(*)
		FROM information_schema.columns
		WHERE table_schema = 'app' AND table_name = 'usage_ledger_entries'
		  AND lower(column_name) ~ '(prompt|response|credential|secret|payload|body|content)'`,
	).Scan(&unsafeColumnCount)
	if err != nil || unsafeColumnCount != 0 {
		t.Fatalf("unsafe usage ledger columns = %d, want 0; error = %v", unsafeColumnCount, err)
	}
}

const insertUsageLedgerSQL = `
	INSERT INTO app.usage_ledger_entries (
		event_id, tenant_id, request_id, attempt_id, token_type,
		quantity, source, price_version_id, amount_micros, observed_at, created_by
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`

func insertUsageLedgerEntry(
	ctx context.Context,
	t *testing.T,
	database *sql.DB,
	eventID, tenantID, requestID string,
	attemptID any,
	tokenType string,
	quantity int64,
	source string,
	observedAt time.Time,
) {
	t.Helper()
	insertUsageLedgerEntryWithPrice(ctx, t, database, eventID, tenantID, requestID, attemptID,
		tokenType, quantity, source, usagePriceVersionID, quantity, observedAt)
}

func insertUsageLedgerEntryWithPrice(
	ctx context.Context,
	t *testing.T,
	database *sql.DB,
	eventID, tenantID, requestID string,
	attemptID any,
	tokenType string,
	quantity int64,
	source, priceVersionID string,
	amountMicros int64,
	observedAt time.Time,
) {
	t.Helper()
	_, err := database.ExecContext(ctx, insertUsageLedgerSQL,
		eventID, tenantID, requestID, attemptID, tokenType,
		quantity, source, priceVersionID, amountMicros, observedAt, "integration:usage")
	if err != nil {
		t.Fatalf("insert usage ledger entry %s: %v", eventID, err)
	}
}

func cleanupUsageLedgerFixtures(t *testing.T, database *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	statements := []string{
		`TRUNCATE app.usage_ledger_entries, app.price_version_rates, app.price_versions RESTART IDENTITY`,
		`DELETE FROM app.route_retry_decisions WHERE request_id LIKE 'integration-usage-%'`,
		`DELETE FROM app.route_decisions WHERE request_id LIKE 'integration-usage-%'`,
		`DELETE FROM app.route_attempt_status_events WHERE attempt_id IN (
			SELECT id FROM app.route_attempts WHERE request_id LIKE 'integration-usage-%')`,
		`DELETE FROM app.gateway_request_status_events WHERE request_id LIKE 'integration-usage-%'`,
		`DELETE FROM app.route_attempts WHERE request_id LIKE 'integration-usage-%'`,
		`DELETE FROM app.gateway_requests WHERE id LIKE 'integration-usage-%'`,
	}
	for _, statement := range statements {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Errorf("cleanup usage ledger fixtures: %v", err)
		}
	}
}
