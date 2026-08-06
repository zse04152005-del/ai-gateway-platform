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

	"github.com/zse04152005-del/ai-gateway-platform/internal/execution"
	"github.com/zse04152005-del/ai-gateway-platform/internal/meteringadjustment"
	"github.com/zse04152005-del/ai-gateway-platform/internal/meteringcost"
)

const (
	adjustmentRequestID       = "integration-usage-adjustment-request"
	adjustmentTargetEventID   = "82000000-0000-4000-8000-000000000001"
	adjustmentFirstEventID    = "82000000-0000-4000-8000-000000000002"
	adjustmentSecondEventID   = "82000000-0000-4000-8000-000000000003"
	adjustmentConcurrentEvent = "82000000-0000-4000-8000-000000000004"
	adjustmentInvalidEventID  = "82000000-0000-4000-8000-000000000099"
)

func TestUsageLedgerAdjustmentIsAppendOnlyAuditableAndIdempotent(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set")
	}
	database, err := sql.Open("postgres", databaseURL)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	database.SetMaxOpenConns(24)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
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
	request := startExecutionRequest(ctx, t, recorder, adjustmentRequestID)
	request, err = recorder.MarkRouting(ctx, request)
	if err != nil {
		t.Fatalf("MarkRouting() error = %v", err)
	}
	request, attempt, err := recorder.StartAttempt(ctx, request, modelListDeploymentAID)
	if err != nil {
		t.Fatalf("StartAttempt() error = %v", err)
	}
	request, attempt, err = recorder.CompleteAttempt(ctx, request, attempt, execution.AttemptOutcome{
		AttemptStatus: execution.AttemptSucceeded, RequestStatus: execution.RequestSucceeded,
		HeadersReceived: true, EndReason: "completed",
	})
	if err != nil {
		t.Fatalf("CompleteAttempt() error = %v", err)
	}
	observedAt := now.Add(time.Second)
	insertUsageLedgerEntryWithPrice(
		ctx, t, database, adjustmentTargetEventID, modelListTenantOneID, request.ID, attempt.ID,
		"input", 100, "provider", usagePriceVersionID, 250, observedAt,
	)

	adjustmentTime := observedAt.Add(time.Minute)
	writer, err := meteringadjustment.NewPostgresWriter(database, func() time.Time { return adjustmentTime })
	if err != nil {
		t.Fatalf("meteringadjustment.NewPostgresWriter() error = %v", err)
	}
	scope := meteringadjustment.Scope{TenantID: modelListTenantOneID, ProjectID: modelListProjectOneID}
	first := meteringadjustment.Command{
		Scope: scope, EventID: adjustmentFirstEventID, IdempotencyKey: "ticket:billing-0001",
		TargetEventID: adjustmentTargetEventID, CorrectedQuantity: 60, CorrectedAmountMicros: 150,
		Origin: meteringadjustment.OriginManual, Reason: "provider_invoice_correction",
		Reference: "ticket:BILL-0001", Actor: "admin:user-001",
	}
	firstResult, err := writer.Apply(ctx, first)
	if err != nil || !firstResult.Inserted || firstResult.Replayed ||
		firstResult.QuantityDelta != -40 || firstResult.AmountMicrosDelta != -100 {
		t.Fatalf("first Apply() = %+v, error = %v", firstResult, err)
	}
	replay, err := writer.Apply(ctx, first)
	if err != nil || replay.Inserted || !replay.Replayed ||
		replay.QuantityDelta != -40 || replay.AmountMicrosDelta != -100 {
		t.Fatalf("replay Apply() = %+v, error = %v", replay, err)
	}
	conflict := first
	conflict.Actor = "admin:user-002"
	if _, err := writer.Apply(ctx, conflict); !errors.Is(err, meteringadjustment.ErrConflict) {
		t.Fatalf("conflicting replay error = %v", err)
	}
	noChange := first
	noChange.EventID = adjustmentInvalidEventID
	noChange.IdempotencyKey = "ticket:billing-no-change"
	if _, err := writer.Apply(ctx, noChange); !errors.Is(err, meteringadjustment.ErrNoChange) {
		t.Fatalf("no-change correction error = %v", err)
	}
	forgedScope := first
	forgedScope.EventID = adjustmentInvalidEventID
	forgedScope.IdempotencyKey = "ticket:billing-forged-scope"
	forgedScope.Scope.TenantID = modelListTenantTwoID
	if _, err := writer.Apply(ctx, forgedScope); !errors.Is(err, meteringadjustment.ErrNotFound) {
		t.Fatalf("cross-scope correction error = %v", err)
	}
	adjustmentTarget := first
	adjustmentTarget.EventID = adjustmentInvalidEventID
	adjustmentTarget.IdempotencyKey = "ticket:billing-adjustment-target"
	adjustmentTarget.TargetEventID = adjustmentFirstEventID
	adjustmentTarget.CorrectedQuantity = 50
	adjustmentTarget.CorrectedAmountMicros = 125
	if _, err := writer.Apply(ctx, adjustmentTarget); !errors.Is(err, meteringadjustment.ErrInvalidTarget) {
		t.Fatalf("adjustment-of-adjustment error = %v", err)
	}

	second := meteringadjustment.Command{
		Scope: scope, EventID: adjustmentSecondEventID, IdempotencyKey: "reconciliation:item-0002",
		TargetEventID: adjustmentTargetEventID, CorrectedQuantity: 80, CorrectedAmountMicros: 210,
		Origin: meteringadjustment.OriginProviderReconciliation,
		Reason: "provider_bill_reconciled", Reference: "batch:invoice-2026-08:item-2",
		Actor: "service:reconciliation-worker",
	}
	secondResult, err := writer.Apply(ctx, second)
	if err != nil || secondResult.QuantityDelta != 20 || secondResult.AmountMicrosDelta != 60 {
		t.Fatalf("second Apply() = %+v, error = %v", secondResult, err)
	}

	concurrent := meteringadjustment.Command{
		Scope: scope, EventID: adjustmentConcurrentEvent, IdempotencyKey: "repair:concurrent-0003",
		TargetEventID: adjustmentTargetEventID, CorrectedQuantity: 90, CorrectedAmountMicros: 225,
		Origin: meteringadjustment.OriginSystemRepair, Reason: "concurrent_idempotency_repair",
		Reference: "incident:INC-0003", Actor: "service:repair-worker",
	}
	const workers = 16
	results := make(chan meteringadjustment.Result, workers)
	errorsFound := make(chan error, workers)
	var waitGroup sync.WaitGroup
	for index := 0; index < workers; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			result, applyErr := writer.Apply(ctx, concurrent)
			results <- result
			errorsFound <- applyErr
		}()
	}
	waitGroup.Wait()
	close(results)
	close(errorsFound)
	inserted, replayed := 0, 0
	for applyErr := range errorsFound {
		if applyErr != nil {
			t.Errorf("concurrent Apply() error = %v", applyErr)
		}
	}
	for result := range results {
		if result.Inserted {
			inserted++
		}
		if result.Replayed {
			replayed++
		}
	}
	if inserted != 1 || replayed != workers-1 {
		t.Fatalf("concurrent results = inserted:%d replayed:%d, want 1/%d", inserted, replayed, workers-1)
	}

	var originalQuantity, originalAmount int64
	err = database.QueryRowContext(ctx, `
		SELECT quantity, amount_micros FROM app.usage_ledger_entries WHERE event_id = $1`,
		adjustmentTargetEventID,
	).Scan(&originalQuantity, &originalAmount)
	if err != nil || originalQuantity != 100 || originalAmount != 250 {
		t.Fatalf("original entry = %d/%d, error = %v", originalQuantity, originalAmount, err)
	}
	var adjustmentCount int
	var effectiveQuantity, effectiveAmount int64
	err = database.QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE source = 'adjustment'), sum(quantity), sum(amount_micros)
		FROM app.usage_ledger_entries WHERE request_id = $1`, request.ID,
	).Scan(&adjustmentCount, &effectiveQuantity, &effectiveAmount)
	if err != nil || adjustmentCount != 3 || effectiveQuantity != 90 || effectiveAmount != 225 {
		t.Fatalf("effective ledger = adjustments:%d quantity:%d amount:%d error:%v",
			adjustmentCount, effectiveQuantity, effectiveAmount, err)
	}

	aggregator, err := meteringcost.NewPostgresAggregator(database)
	if err != nil {
		t.Fatalf("meteringcost.NewPostgresAggregator() error = %v", err)
	}
	cost, err := aggregator.Aggregate(ctx, meteringcost.Scope{
		TenantID: modelListTenantOneID, ProjectID: modelListProjectOneID,
	}, request.ID)
	if err != nil || cost.LedgerEntryCount != 4 || len(cost.Totals) != 1 ||
		cost.Totals[0].Currency != "USD" || cost.Totals[0].AmountMicros != 225 {
		t.Fatalf("adjusted request cost = %+v, error = %v", cost, err)
	}

	_, err = database.ExecContext(ctx, `
		INSERT INTO app.usage_ledger_entries (
			event_id, tenant_id, request_id, attempt_id, token_type,
			quantity, source, price_version_id, amount_micros, observed_at, created_by,
			event_schema_version, adjusts_event_id
		) VALUES ($1, $2, $3, $4, 'input', -1, 'adjustment', $5, -1, $6, $7, NULL, $8)`,
		adjustmentInvalidEventID, modelListTenantOneID, request.ID, attempt.ID,
		usagePriceVersionID, adjustmentTime, "admin:user-001", adjustmentTargetEventID)
	expectConstraint(t, err, "usage_ledger_entries_adjustment_metadata_valid")
	_, err = database.ExecContext(ctx, `
		INSERT INTO app.usage_ledger_entries (
			event_id, tenant_id, request_id, attempt_id, token_type,
			quantity, source, observed_at, created_at, created_by,
			price_version_id, amount_micros, event_schema_version,
			adjusts_event_id, adjustment_idempotency_key, adjustment_origin,
			adjustment_reason, adjustment_reference, adjustment_actor,
			adjustment_result_quantity, adjustment_result_amount_micros
		) VALUES (
			$1, $2, $3, $4, 'input', 1, 'adjustment', $5, $5, $6,
			$7, 1, NULL, $8, 'ticket:forged-result', 'manual',
			'forged_result', 'ticket:FORGED-1', $6, 999, 999
		)`, adjustmentInvalidEventID, modelListTenantOneID, request.ID, attempt.ID,
		adjustmentTime, "admin:user-001", usagePriceVersionID, adjustmentTargetEventID)
	expectConstraint(t, err, "usage_ledger_entries_adjustment_result")
	_, err = database.ExecContext(ctx, `
		UPDATE app.usage_ledger_entries SET adjustment_reason = 'tampered'
		WHERE event_id = $1`, adjustmentFirstEventID)
	expectExecutionSQLState(t, err, "23514")
	_, err = database.ExecContext(ctx, `
		DELETE FROM app.usage_ledger_entries WHERE event_id = $1`, adjustmentFirstEventID)
	expectExecutionSQLState(t, err, "23514")
}
