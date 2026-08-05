//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/execution"
	"github.com/zse04152005-del/ai-gateway-platform/internal/metering"
	"github.com/zse04152005-del/ai-gateway-platform/internal/meteringconsumer"
	"github.com/zse04152005-del/ai-gateway-platform/internal/meteringcost"
)

const (
	meteringCostMultiRequestID   = "integration-metering-cost-multi-attempt"
	meteringCostPartialRequestID = "integration-metering-cost-partial-stream"
	meteringCostFailedRequestID  = "integration-metering-cost-failed-attempt"
	meteringCostActiveRequestID  = "integration-metering-cost-active-request"
	meteringCostPriceAID         = "7f000000-0000-4000-8000-000000000101"
	meteringCostPriceBID         = "7f000000-0000-4000-8000-000000000102"
	meteringCostConsumerGroup    = "ai-gateway-metering-cost-integration-v1"
)

func TestMeteringCostAggregationIncludesFailedPartialAndSuccessfulAttempts(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		t.Fatalf("database.PingContext() error = %v", err)
	}

	cleanupMeteringConsumerFixtures(t, database)
	cleanupModelListFixtures(t, database)
	t.Cleanup(func() {
		cleanupMeteringConsumerFixtures(t, database)
		cleanupModelListFixtures(t, database)
	})
	seedModelListCatalog(ctx, t, database)
	seedExecutionVirtualKey(ctx, t, database)

	now := time.Now().UTC().Truncate(time.Microsecond)
	seedUsagePriceVersion(ctx, t, database, meteringCostPriceAID,
		modelListDeploymentAID, now.Add(-time.Hour))
	seedUsagePriceVersion(ctx, t, database, meteringCostPriceBID,
		modelListDeploymentBID, now.Add(-time.Hour))
	recorder, err := execution.NewPostgresRecorder(database, func() time.Time {
		now = now.Add(time.Microsecond)
		return now
	}, rand.Reader)
	if err != nil {
		t.Fatalf("execution.NewPostgresRecorder() error = %v", err)
	}
	processor, err := meteringconsumer.NewProcessor(database, meteringCostConsumerGroup, time.Now)
	if err != nil {
		t.Fatalf("meteringconsumer.NewProcessor() error = %v", err)
	}
	aggregator, err := meteringcost.NewPostgresAggregator(database)
	if err != nil {
		t.Fatalf("meteringcost.NewPostgresAggregator() error = %v", err)
	}
	scope := meteringcost.Scope{TenantID: modelListTenantOneID, ProjectID: modelListProjectOneID}

	active := startExecutionRequest(ctx, t, recorder, meteringCostActiveRequestID)
	if _, err := aggregator.Aggregate(ctx, scope, active.ID); !errors.Is(err, meteringcost.ErrNotTerminal) {
		t.Fatalf("active request aggregate error = %v, want ErrNotTerminal", err)
	}
	forgedScope := meteringcost.Scope{
		TenantID:  "7f000000-0000-4000-8000-000000000201",
		ProjectID: "7f000000-0000-4000-8000-000000000202",
	}
	if _, err := aggregator.Aggregate(ctx, forgedScope, active.ID); !errors.Is(err, meteringcost.ErrNotFound) {
		t.Fatalf("cross-scope aggregate error = %v, want ErrNotFound", err)
	}

	multi := startExecutionRequest(ctx, t, recorder, meteringCostMultiRequestID)
	multi, err = recorder.MarkRouting(ctx, multi)
	if err != nil {
		t.Fatalf("MarkRouting(multi) error = %v", err)
	}
	multi, failedAttempt, err := recorder.StartAttempt(ctx, multi, modelListDeploymentAID)
	if err != nil {
		t.Fatalf("StartAttempt(failed) error = %v", err)
	}
	failedUsage := adapter.NormalizedUsage{
		InputTokens: adapter.Tokens(1_000_000),
		Source:      adapter.UsageSourceEstimated, Estimate: integrationEstimateMetadata(),
	}
	failedAttempt, err = recorder.CompleteAttemptForRetry(ctx, multi, failedAttempt, execution.AttemptOutcome{
		AttemptStatus: execution.AttemptRetryableFailed, RequestStatus: execution.RequestRunning,
		HeadersReceived: true, EndReason: "provider_capacity",
		ErrorCategory: string(adapter.ErrorCapacity), ErrorCode: "PROVIDER_CAPACITY",
		Usage: &failedUsage,
	})
	if err != nil {
		t.Fatalf("CompleteAttemptForRetry() error = %v", err)
	}
	multi, successfulAttempt, err := recorder.StartAttempt(ctx, multi, modelListDeploymentBID)
	if err != nil {
		t.Fatalf("StartAttempt(successful) error = %v", err)
	}
	successUsage := adapter.NormalizedUsage{
		OutputTokens: adapter.Tokens(2_000_000),
		Source:       adapter.UsageSourceEstimated,
		Complete:     true,
		Estimate:     integrationEstimateMetadataFor("model-b-physical"),
	}
	multi, successfulAttempt, err = recorder.CompleteAttempt(ctx, multi, successfulAttempt, execution.AttemptOutcome{
		AttemptStatus: execution.AttemptSucceeded, RequestStatus: execution.RequestSucceeded,
		HeadersReceived: true, EndReason: "completed", Usage: &successUsage,
	})
	if err != nil {
		t.Fatalf("CompleteAttempt(successful) error = %v", err)
	}
	multiEvents := loadMeteringCostEvents(ctx, t, database, multi.ID)
	if len(multiEvents) != 2 {
		t.Fatalf("multi-attempt event count = %d, want 2", len(multiEvents))
	}
	processMeteringCostEvent(ctx, t, processor, multiEvents[0])
	if _, err := aggregator.Aggregate(ctx, scope, multi.ID); !errors.Is(err, meteringcost.ErrPending) {
		t.Fatalf("partially priced aggregate error = %v, want ErrPending", err)
	}
	processMeteringCostEvent(ctx, t, processor, multiEvents[1])
	multiCost, err := aggregator.Aggregate(ctx, scope, multi.ID)
	if err != nil {
		t.Fatalf("Aggregate(multi) error = %v", err)
	}
	assertMeteringCost(t, multiCost, execution.RequestSucceeded, 7_500_000,
		[]expectedAttemptCost{
			{id: failedAttempt.ID, status: execution.AttemptRetryableFailed, amount: 2_500_000},
			{id: successfulAttempt.ID, status: execution.AttemptSucceeded, amount: 5_000_000},
		})
	var ledgerSchemaVersion int
	var ledgerTokenizer, ledgerPhysicalModel string
	var ledgerDeploymentVersion int64
	err = database.QueryRowContext(ctx, `
		SELECT event_schema_version, tokenizer, physical_model, deployment_version
		FROM app.usage_ledger_entries
		WHERE attempt_id = $1`, successfulAttempt.ID,
	).Scan(&ledgerSchemaVersion, &ledgerTokenizer, &ledgerPhysicalModel, &ledgerDeploymentVersion)
	if err != nil || ledgerSchemaVersion != metering.UsageEventSchemaVersion ||
		ledgerTokenizer != integrationEstimationAlgorithm || ledgerPhysicalModel != "model-b-physical" ||
		ledgerDeploymentVersion != 1 {
		t.Fatalf("estimated ledger evidence = %d/%q/%q/%d, error = %v",
			ledgerSchemaVersion, ledgerTokenizer, ledgerPhysicalModel, ledgerDeploymentVersion, err)
	}

	partial := startExecutionRequest(ctx, t, recorder, meteringCostPartialRequestID)
	partial, err = recorder.MarkRouting(ctx, partial)
	if err != nil {
		t.Fatalf("MarkRouting(partial) error = %v", err)
	}
	partial, partialAttempt, err := recorder.StartAttempt(ctx, partial, modelListDeploymentAID)
	if err != nil {
		t.Fatalf("StartAttempt(partial) error = %v", err)
	}
	partialAttempt, err = recorder.MarkAttemptStreaming(ctx, partial, partialAttempt, "provider/partial-stream")
	if err != nil {
		t.Fatalf("MarkAttemptStreaming(partial) error = %v", err)
	}
	partialUsage := adapter.NormalizedUsage{
		OutputTokens: adapter.Tokens(400_000), Source: adapter.UsageSourceEstimated,
		Estimate: integrationEstimateMetadata(),
	}
	partial, partialAttempt, err = recorder.CompleteAttempt(ctx, partial, partialAttempt, execution.AttemptOutcome{
		AttemptStatus: execution.AttemptPartialFailed, RequestStatus: execution.RequestPartialFailed,
		HeadersReceived: true, EndReason: "stream_interrupted",
		ErrorCategory: "transport", ErrorCode: "STREAM_INTERRUPTED",
		Usage: &partialUsage,
	})
	if err != nil {
		t.Fatalf("CompleteAttempt(partial) error = %v", err)
	}
	processAllMeteringCostEvents(ctx, t, database, processor, partial.ID)
	partialCost, err := aggregator.Aggregate(ctx, scope, partial.ID)
	if err != nil {
		t.Fatalf("Aggregate(partial) error = %v", err)
	}
	assertMeteringCost(t, partialCost, execution.RequestPartialFailed, 1_000_000,
		[]expectedAttemptCost{{
			id: partialAttempt.ID, status: execution.AttemptPartialFailed, amount: 1_000_000,
		}})

	failed := startExecutionRequest(ctx, t, recorder, meteringCostFailedRequestID)
	failed, err = recorder.MarkRouting(ctx, failed)
	if err != nil {
		t.Fatalf("MarkRouting(failed) error = %v", err)
	}
	failed, terminalFailedAttempt, err := recorder.StartAttempt(ctx, failed, modelListDeploymentAID)
	if err != nil {
		t.Fatalf("StartAttempt(terminal failed) error = %v", err)
	}
	terminalFailedUsage := adapter.NormalizedUsage{
		InputTokens: adapter.Tokens(200_000), Source: adapter.UsageSourceEstimated,
		Estimate: integrationEstimateMetadata(),
	}
	failed, terminalFailedAttempt, err = recorder.CompleteAttempt(
		ctx, failed, terminalFailedAttempt, execution.AttemptOutcome{
			AttemptStatus: execution.AttemptFailed, RequestStatus: execution.RequestFailed,
			HeadersReceived: true, EndReason: "provider_protocol",
			ErrorCategory: string(adapter.ErrorProtocol), ErrorCode: "PROVIDER_PROTOCOL",
			Usage: &terminalFailedUsage,
		},
	)
	if err != nil {
		t.Fatalf("CompleteAttempt(terminal failed) error = %v", err)
	}
	processAllMeteringCostEvents(ctx, t, database, processor, failed.ID)
	failedCost, err := aggregator.Aggregate(ctx, scope, failed.ID)
	if err != nil {
		t.Fatalf("Aggregate(failed) error = %v", err)
	}
	assertMeteringCost(t, failedCost, execution.RequestFailed, 500_000,
		[]expectedAttemptCost{{
			id: terminalFailedAttempt.ID, status: execution.AttemptFailed, amount: 500_000,
		}})
}

type expectedAttemptCost struct {
	id     string
	status execution.AttemptStatus
	amount int64
}

func assertMeteringCost(
	t *testing.T,
	result meteringcost.RequestCost,
	status execution.RequestStatus,
	total int64,
	attempts []expectedAttemptCost,
) {
	t.Helper()
	if result.Status != status || result.AttemptCount != len(attempts) ||
		result.LedgerEntryCount != len(attempts) || len(result.Attempts) != len(attempts) ||
		result.RequestLevel.LedgerEntryCount != 0 || len(result.RequestLevel.Totals) != 0 ||
		len(result.Totals) != 1 || result.Totals[0].Currency != "USD" ||
		result.Totals[0].AmountMicros != total {
		t.Fatalf("request cost = %+v", result)
	}
	for index, expected := range attempts {
		attempt := result.Attempts[index]
		if attempt.AttemptID != expected.id || attempt.AttemptNo != index+1 ||
			attempt.Status != expected.status || attempt.LedgerEntryCount != 1 ||
			len(attempt.Totals) != 1 || attempt.Totals[0].Currency != "USD" ||
			attempt.Totals[0].AmountMicros != expected.amount {
			t.Fatalf("attempt cost[%d] = %+v, want %+v", index, attempt, expected)
		}
	}
}

func processAllMeteringCostEvents(
	ctx context.Context,
	t *testing.T,
	database *sql.DB,
	processor *meteringconsumer.Processor,
	requestID string,
) {
	t.Helper()
	for _, event := range loadMeteringCostEvents(ctx, t, database, requestID) {
		processMeteringCostEvent(ctx, t, processor, event)
	}
}

func processMeteringCostEvent(
	ctx context.Context,
	t *testing.T,
	processor *meteringconsumer.Processor,
	event metering.UsageEvent,
) {
	t.Helper()
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("json.Marshal(event) error = %v", err)
	}
	result, err := processor.Process(ctx, event.EventID, payload)
	if err != nil || !result.Inserted || result.Replayed {
		t.Fatalf("Process(%s) = %+v, %v", event.EventID, result, err)
	}
}

func loadMeteringCostEvents(
	ctx context.Context,
	t *testing.T,
	database *sql.DB,
	requestID string,
) []metering.UsageEvent {
	t.Helper()
	rows, err := database.QueryContext(ctx, `
		SELECT outbox.event_id::text, outbox.schema_version, outbox.kind,
			outbox.tenant_id::text, outbox.request_id, outbox.attempt_id::text,
			outbox.deployment_id::text, outbox.token_type, outbox.billing_unit,
			outbox.quantity, outbox.source, outbox.usage_complete,
			outbox.tokenizer, outbox.tokenizer_version, outbox.physical_model,
			outbox.deployment_version, outbox.provider_protocol_version,
			outbox.observed_at, outbox.trace_id, outbox.span_id
		FROM app.usage_event_outbox AS outbox
		JOIN app.route_attempts AS attempt ON attempt.id = outbox.attempt_id
		WHERE outbox.request_id = $1
		ORDER BY attempt.attempt_no, outbox.token_type`, requestID)
	if err != nil {
		t.Fatalf("query metering cost events: %v", err)
	}
	defer func() { _ = rows.Close() }()
	events := make([]metering.UsageEvent, 0)
	for rows.Next() {
		var event metering.UsageEvent
		var kind, tokenType, billingUnit, source string
		var tokenizer, tokenizerVersion, physicalModel, providerProtocolVersion sql.NullString
		var deploymentVersion sql.NullInt64
		if err = rows.Scan(
			&event.EventID, &event.SchemaVersion, &kind,
			&event.TenantID, &event.RequestID, &event.AttemptID,
			&event.DeploymentID, &tokenType, &billingUnit,
			&event.Quantity, &source, &event.UsageComplete,
			&tokenizer, &tokenizerVersion, &physicalModel, &deploymentVersion, &providerProtocolVersion,
			&event.ObservedAt, &event.TraceID, &event.SpanID,
		); err != nil {
			t.Fatalf("scan metering cost event: %v", err)
		}
		event.Kind = metering.UsageEventKind(kind)
		event.TokenType = metering.TokenType(tokenType)
		event.BillingUnit = metering.BillingUnit(billingUnit)
		event.Source = metering.Source(source)
		if event.SchemaVersion == metering.UsageEventSchemaVersion && event.Source == metering.SourceEstimated &&
			tokenizer.Valid && tokenizerVersion.Valid && physicalModel.Valid && deploymentVersion.Valid &&
			providerProtocolVersion.Valid {
			event.Estimate = &adapter.UsageEstimateMetadata{
				Estimated: true, Tokenizer: tokenizer.String, TokenizerVersion: tokenizerVersion.String,
				PhysicalModel: physicalModel.String, DeploymentVersion: deploymentVersion.Int64,
				ProviderProtocolVersion: providerProtocolVersion.String,
			}
		}
		if event.Validate() != nil {
			t.Fatalf("stored metering cost event is invalid: %+v", event)
		}
		events = append(events, event)
	}
	if err = rows.Err(); err != nil {
		t.Fatalf("iterate metering cost events: %v", err)
	}
	return events
}
