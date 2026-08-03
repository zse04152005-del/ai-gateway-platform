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
	"github.com/zse04152005-del/ai-gateway-platform/internal/metering"
)

const (
	usagePriceVersionID       = "7b000000-0000-4000-8000-000000000101"
	priceCurrentVersionID     = "7b000000-0000-4000-8000-000000000102"
	priceFutureVersionID      = "7b000000-0000-4000-8000-000000000103"
	priceOtherDeploymentID    = "7b000000-0000-4000-8000-000000000104"
	priceDraftVersionID       = "7b000000-0000-4000-8000-000000000105"
	priceEmptyVersionID       = "7b000000-0000-4000-8000-000000000106"
	priceInvalidVersionID     = "7b000000-0000-4000-8000-000000000107"
	priceConcurrentVersionID  = "7b000000-0000-4000-8000-000000000108"
	priceHistoricalRequestID  = "integration-usage-price-history"
	priceHistoricalEventID    = "7b000000-0000-4000-8000-000000000201"
	priceInvalidEventIDPrefix = "7b000000-0000-4000-8000-0000000002"
)

func TestPriceVersionSchemaAndHistoricalLock(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set")
	}
	database, err := sql.Open("postgres", databaseURL)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	database.SetMaxOpenConns(2)
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
	seedUsagePriceVersion(ctx, t, database, usagePriceVersionID, modelListDeploymentAID, now.Add(-3*time.Hour))
	recorder, err := execution.NewPostgresRecorder(database, func() time.Time {
		now = now.Add(time.Microsecond)
		return now
	}, rand.Reader)
	if err != nil {
		t.Fatalf("execution.NewPostgresRecorder() error = %v", err)
	}
	request := startExecutionRequest(ctx, t, recorder, priceHistoricalRequestID)
	request, err = recorder.MarkRouting(ctx, request)
	if err != nil {
		t.Fatalf("MarkRouting(price request) error = %v", err)
	}
	_, attempt, err := recorder.StartAttempt(ctx, request, modelListDeploymentAID)
	if err != nil {
		t.Fatalf("StartAttempt(price request) error = %v", err)
	}

	observedAt := now.Add(time.Second)
	insertUsageLedgerEntryWithPrice(ctx, t, database, priceHistoricalEventID,
		modelListTenantOneID, priceHistoricalRequestID, attempt.ID, string(metering.TokenTypeInput),
		1_000_000, string(metering.SourceProvider), usagePriceVersionID, 2_500_000, observedAt)
	seedUsagePriceVersion(ctx, t, database, priceCurrentVersionID, modelListDeploymentAID, now.Add(-2*time.Hour))

	var lockedPriceVersion string
	var lockedAmount int64
	err = database.QueryRowContext(ctx, `
		SELECT price_version_id, amount_micros
		FROM app.usage_ledger_entries WHERE event_id = $1`, priceHistoricalEventID,
	).Scan(&lockedPriceVersion, &lockedAmount)
	if err != nil || lockedPriceVersion != usagePriceVersionID || lockedAmount != 2_500_000 {
		t.Fatalf("historical price lock = %s/%d, want %s/2500000; error:%v",
			lockedPriceVersion, lockedAmount, usagePriceVersionID, err)
	}
	_, err = database.ExecContext(ctx, `UPDATE app.price_versions SET currency = 'EUR' WHERE id = $1`, usagePriceVersionID)
	expectConstraint(t, err, "price_versions_identity_immutable")
	_, err = database.ExecContext(ctx, `UPDATE app.price_version_rates SET unit_price_micros = 1 WHERE price_version_id = $1`, usagePriceVersionID)
	expectExecutionSQLState(t, err, "23514")
	_, err = database.ExecContext(ctx, `UPDATE app.usage_ledger_entries SET price_version_id = $1 WHERE event_id = $2`,
		priceCurrentVersionID, priceHistoricalEventID)
	expectExecutionSQLState(t, err, "23514")

	seedUsagePriceVersion(ctx, t, database, priceFutureVersionID, modelListDeploymentAID, now.Add(time.Hour))
	_, err = database.ExecContext(ctx, insertUsageLedgerSQL,
		priceInvalidEventIDPrefix+"02", modelListTenantOneID, priceHistoricalRequestID, attempt.ID,
		string(metering.TokenTypeInput), int64(1), string(metering.SourceProvider),
		priceFutureVersionID, int64(1), observedAt, "integration:price")
	expectConstraint(t, err, "usage_ledger_entries_price_effective")

	seedUsagePriceVersion(ctx, t, database, priceOtherDeploymentID, modelListDeploymentBID, now.Add(-2*time.Hour))
	_, err = database.ExecContext(ctx, insertUsageLedgerSQL,
		priceInvalidEventIDPrefix+"03", modelListTenantOneID, priceHistoricalRequestID, attempt.ID,
		string(metering.TokenTypeInput), int64(1), string(metering.SourceProvider),
		priceOtherDeploymentID, int64(1), observedAt, "integration:price")
	expectConstraint(t, err, "usage_ledger_entries_price_deployment")

	insertPriceVersionDraft(ctx, t, database, priceDraftVersionID, modelListDeploymentAID,
		"cn-north-1", "USD", now.Add(-time.Hour))
	insertPriceRate(ctx, t, database, priceDraftVersionID, metering.TokenTypeInput,
		metering.BillingUnitToken, 1_000_000, 2_750_000)
	_, err = database.ExecContext(ctx, insertUsageLedgerSQL,
		priceInvalidEventIDPrefix+"04", modelListTenantOneID, priceHistoricalRequestID, attempt.ID,
		string(metering.TokenTypeInput), int64(1), string(metering.SourceProvider),
		priceDraftVersionID, int64(1), observedAt, "integration:price")
	expectConstraint(t, err, "usage_ledger_entries_price_published")
	publishPriceVersion(ctx, t, database, priceDraftVersionID)
	_, err = database.ExecContext(ctx, insertUsageLedgerSQL,
		priceInvalidEventIDPrefix+"05", modelListTenantOneID, priceHistoricalRequestID, attempt.ID,
		string(metering.TokenTypeOutput), int64(1), string(metering.SourceProvider),
		priceDraftVersionID, int64(1), observedAt, "integration:price")
	expectConstraint(t, err, "usage_ledger_entries_price_rate_fk")
	_, err = database.ExecContext(ctx, `
		INSERT INTO app.price_version_rates (
			price_version_id, token_type, billing_unit, unit_quantity, unit_price_micros, created_by
		) VALUES ($1, 'output', 'token', 1000000, 1, 'integration:price')`, priceDraftVersionID)
	expectConstraint(t, err, "price_version_rates_parent_draft")

	insertPriceVersionDraft(ctx, t, database, priceEmptyVersionID, modelListDeploymentAID,
		"cn-north-1", "USD", now.Add(-30*time.Minute))
	_, err = database.ExecContext(ctx, `
		UPDATE app.price_versions
		SET status = 'published', version = 2, published_at = CURRENT_TIMESTAMP,
		    updated_at = CURRENT_TIMESTAMP, updated_by = 'integration:price'
		WHERE id = $1`, priceEmptyVersionID)
	expectConstraint(t, err, "price_versions_rates_required")

	insertPriceVersionDraft(ctx, t, database, priceConcurrentVersionID, modelListDeploymentAID,
		"cn-north-1", "USD", now.Add(-45*time.Minute))
	insertPriceRate(ctx, t, database, priceConcurrentVersionID, metering.TokenTypeInput,
		metering.BillingUnitToken, 1_000_000, 2_750_000)
	rateTransaction, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin concurrent rate transaction: %v", err)
	}
	_, err = rateTransaction.ExecContext(ctx, `
		INSERT INTO app.price_version_rates (
			price_version_id, token_type, billing_unit, unit_quantity, unit_price_micros, created_by
		) VALUES ($1, 'output', 'token', 1000000, 7500000, 'integration:price')`, priceConcurrentVersionID)
	if err != nil {
		_ = rateTransaction.Rollback()
		t.Fatalf("insert concurrent price rate: %v", err)
	}
	publishResult := make(chan error, 1)
	go func() {
		_, publishErr := database.ExecContext(ctx, `
			UPDATE app.price_versions
			SET status = 'published', version = 2, published_at = CURRENT_TIMESTAMP,
			    updated_at = CURRENT_TIMESTAMP, updated_by = 'integration:price'
			WHERE id = $1`, priceConcurrentVersionID)
		publishResult <- publishErr
	}()
	select {
	case publishErr := <-publishResult:
		_ = rateTransaction.Rollback()
		t.Fatalf("publication was not serialized behind the draft rate: %v", publishErr)
	case <-time.After(250 * time.Millisecond):
	}
	if err := rateTransaction.Commit(); err != nil {
		t.Fatalf("commit concurrent price rate: %v", err)
	}
	select {
	case publishErr := <-publishResult:
		if publishErr != nil {
			t.Fatalf("publish after concurrent price rate: %v", publishErr)
		}
	case <-ctx.Done():
		t.Fatalf("wait for serialized price publication: %v", ctx.Err())
	}

	_, err = database.ExecContext(ctx, `
		INSERT INTO app.price_versions (
			id, deployment_id, region, currency, effective_at, created_by, updated_by
		) VALUES ($1, $2, 'us-east-1', 'USD', $3, 'integration:price', 'integration:price')`,
		priceInvalidVersionID, modelListDeploymentAID, now.Add(2*time.Hour))
	expectConstraint(t, err, "price_versions_deployment_region_fk")
	_, err = database.ExecContext(ctx, `
		INSERT INTO app.price_versions (
			id, deployment_id, region, currency, effective_at, created_by, updated_by
		) VALUES ($1, $2, 'cn-north-1', 'usd', $3, 'integration:price', 'integration:price')`,
		priceInvalidVersionID, modelListDeploymentAID, now.Add(2*time.Hour))
	expectConstraint(t, err, "price_versions_currency_format")
	_, err = database.ExecContext(ctx, `
		INSERT INTO app.price_versions (
			id, deployment_id, region, currency, effective_at, status, version,
			created_by, updated_by, published_at
		) VALUES ($1, $2, 'cn-north-1', 'USD', $3, 'published', 2,
			'integration:price', 'integration:price', CURRENT_TIMESTAMP)`,
		priceInvalidVersionID, modelListDeploymentAID, now.Add(2*time.Hour))
	expectConstraint(t, err, "price_versions_initial_draft")

	insertPriceVersionDraft(ctx, t, database, priceInvalidVersionID, modelListDeploymentAID,
		"cn-north-1", "USD", now.Add(2*time.Hour))
	_, err = database.ExecContext(ctx, `
		INSERT INTO app.price_version_rates (
			price_version_id, token_type, billing_unit, unit_quantity, unit_price_micros, created_by
		) VALUES ($1, 'input', 'image', 1, 1, 'integration:price')`, priceInvalidVersionID)
	expectConstraint(t, err, "price_version_rates_unit_compatible")
	_, err = database.ExecContext(ctx, `
		INSERT INTO app.price_version_rates (
			price_version_id, token_type, billing_unit, unit_quantity, unit_price_micros, created_by
		) VALUES ($1, 'input', 'token', 0, 1, 'integration:price')`, priceInvalidVersionID)
	expectConstraint(t, err, "price_version_rates_values_valid")
	_, err = database.ExecContext(ctx, `DELETE FROM app.price_versions WHERE id = $1`, priceInvalidVersionID)
	expectExecutionSQLState(t, err, "23514")
}

func seedUsagePriceVersion(
	ctx context.Context,
	t *testing.T,
	database *sql.DB,
	priceVersionID, deploymentID string,
	effectiveAt time.Time,
) {
	t.Helper()
	insertPriceVersionDraft(ctx, t, database, priceVersionID, deploymentID, "cn-north-1", "USD", effectiveAt)
	for _, tokenType := range metering.TokenTypes() {
		unit := metering.BillingUnitToken
		unitQuantity := int64(1_000_000)
		if tokenType == metering.TokenTypeAudioInput || tokenType == metering.TokenTypeAudioOutput {
			unit = metering.BillingUnitSecond
			unitQuantity = 1
		}
		if tokenType == metering.TokenTypeImageInput || tokenType == metering.TokenTypeImageOutput {
			unit = metering.BillingUnitImage
			unitQuantity = 1
		}
		insertPriceRate(ctx, t, database, priceVersionID, tokenType, unit, unitQuantity, 2_500_000)
	}
	publishPriceVersion(ctx, t, database, priceVersionID)
}

func insertPriceVersionDraft(
	ctx context.Context,
	t *testing.T,
	database *sql.DB,
	priceVersionID, deploymentID, region, currency string,
	effectiveAt time.Time,
) {
	t.Helper()
	_, err := database.ExecContext(ctx, `
		INSERT INTO app.price_versions (
			id, deployment_id, region, currency, effective_at, created_by, updated_by
		) VALUES ($1, $2, $3, $4, $5, 'integration:price', 'integration:price')`,
		priceVersionID, deploymentID, region, currency, effectiveAt)
	if err != nil {
		t.Fatalf("insert price version %s: %v", priceVersionID, err)
	}
}

func insertPriceRate(
	ctx context.Context,
	t *testing.T,
	database *sql.DB,
	priceVersionID string,
	tokenType metering.TokenType,
	unit metering.BillingUnit,
	unitQuantity, unitPriceMicros int64,
) {
	t.Helper()
	_, err := database.ExecContext(ctx, `
		INSERT INTO app.price_version_rates (
			price_version_id, token_type, billing_unit,
			unit_quantity, unit_price_micros, created_by
		) VALUES ($1, $2, $3, $4, $5, 'integration:price')`,
		priceVersionID, tokenType, unit, unitQuantity, unitPriceMicros)
	if err != nil {
		t.Fatalf("insert price rate %s/%s: %v", priceVersionID, tokenType, err)
	}
}

func publishPriceVersion(ctx context.Context, t *testing.T, database *sql.DB, priceVersionID string) {
	t.Helper()
	_, err := database.ExecContext(ctx, `
		UPDATE app.price_versions
		SET status = 'published', version = 2, published_at = CURRENT_TIMESTAMP,
		    updated_at = CURRENT_TIMESTAMP, updated_by = 'integration:price'
		WHERE id = $1`, priceVersionID)
	if err != nil {
		t.Fatalf("publish price version %s: %v", priceVersionID, err)
	}
}
