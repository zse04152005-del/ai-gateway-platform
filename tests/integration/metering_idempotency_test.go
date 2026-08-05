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

	"github.com/zse04152005-del/ai-gateway-platform/internal/execution"
	"github.com/zse04152005-del/ai-gateway-platform/internal/metering"
	"github.com/zse04152005-del/ai-gateway-platform/internal/meteringconsumer"
	"github.com/zse04152005-del/ai-gateway-platform/internal/meteringoutbox"
	"github.com/zse04152005-del/ai-gateway-platform/internal/meteringworker"
)

const (
	meteringConsumerRequestID    = "integration-metering-consumer-idempotency"
	meteringConsumerEventID      = "7e000000-0000-4000-8000-000000000101"
	meteringConsumerPriceID      = "7e000000-0000-4000-8000-000000000102"
	meteringConsumerGroup        = "ai-gateway-metering-integration-v1"
	meteringConsumerReplayCount  = 10
	meteringConsumerWaitDuration = 15 * time.Second
)

func TestMeteringConsumerTenReplaysCreateOnePricedLedgerEntry(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	rawBrokers := strings.TrimSpace(os.Getenv("KAFKA_BROKERS"))
	if databaseURL == "" || rawBrokers == "" {
		t.Skip("DATABASE_URL and KAFKA_BROKERS are required")
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
	seedUsagePriceVersion(ctx, t, database, meteringConsumerPriceID,
		modelListDeploymentAID, now.Add(-time.Hour))
	recorder, err := execution.NewPostgresRecorder(database, func() time.Time {
		now = now.Add(time.Microsecond)
		return now
	}, rand.Reader)
	if err != nil {
		t.Fatalf("execution.NewPostgresRecorder() error = %v", err)
	}
	request := startExecutionRequest(ctx, t, recorder, meteringConsumerRequestID)
	request, err = recorder.MarkRouting(ctx, request)
	if err != nil {
		t.Fatalf("MarkRouting() error = %v", err)
	}
	request, attempt, err := recorder.StartAttempt(ctx, request, modelListDeploymentAID)
	if err != nil {
		t.Fatalf("StartAttempt() error = %v", err)
	}
	_, _, err = recorder.CompleteAttempt(ctx, request, attempt, execution.AttemptOutcome{
		AttemptStatus: execution.AttemptSucceeded, RequestStatus: execution.RequestSucceeded,
		HeadersReceived: true, EndReason: "completed",
	})
	if err != nil {
		t.Fatalf("CompleteAttempt() error = %v", err)
	}

	event := metering.UsageEvent{
		EventID: meteringConsumerEventID, SchemaVersion: metering.UsageEventSchemaVersion,
		Kind:     metering.UsageEventEstimated,
		TenantID: request.TenantID, RequestID: request.ID, AttemptID: attempt.ID,
		DeploymentID: attempt.DeploymentID, TokenType: metering.TokenTypeInput,
		BillingUnit: metering.BillingUnitToken,
		Quantity:    13, Source: metering.SourceEstimated, UsageComplete: true,
		ObservedAt: now.Add(time.Second), TraceID: request.TraceID, SpanID: request.SpanID,
	}
	payload, err := json.Marshal(event)
	if err != nil || event.Validate() != nil {
		t.Fatalf("marshal metering event = %s, %v", payload, err)
	}
	processor, err := meteringconsumer.NewProcessor(database, meteringConsumerGroup, time.Now)
	if err != nil {
		t.Fatalf("meteringconsumer.NewProcessor() error = %v", err)
	}
	processed := make(chan meteringconsumer.Result, meteringConsumerReplayCount)
	committed := make(chan struct{}, meteringConsumerReplayCount)
	assigned := make(chan struct{})
	handler := &recordingMeteringHandler{processor: processor, processed: processed}
	connector, err := meteringconsumer.NewKafkaConnector(handler, meteringconsumer.KafkaOptions{
		ConsumerGroup: meteringConsumerGroup, StartAtEnd: true,
		OnPartitionsAssigned: func() { close(assigned) },
		OnRecordCommitted:    func() { committed <- struct{}{} },
	})
	if err != nil {
		t.Fatalf("meteringconsumer.NewKafkaConnector() error = %v", err)
	}
	worker, err := meteringworker.New(meteringworker.Options{
		Brokers: strings.Split(rawBrokers, ","), ConnectTimeout: 5 * time.Second,
		ShutdownTimeout: 5 * time.Second, Connector: connector,
	})
	if err != nil {
		t.Fatalf("meteringworker.New() error = %v", err)
	}
	workerContext, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(workerContext) }()
	waitForMeteringConsumerSignal(t, assigned, workerDone, "Kafka partition assignment")

	brokers := strings.Split(rawBrokers, ",")
	for index := range brokers {
		brokers[index] = strings.TrimSpace(brokers[index])
	}
	sink, err := meteringoutbox.NewKafkaSink(brokers)
	if err != nil {
		t.Fatalf("meteringoutbox.NewKafkaSink() error = %v", err)
	}
	t.Cleanup(sink.Close)
	for replay := 0; replay < meteringConsumerReplayCount; replay++ {
		if err := sink.Publish(ctx, event.EventID, payload); err != nil {
			t.Fatalf("Kafka Publish(replay=%d) error = %v", replay, err)
		}
	}

	inserted, replayed := 0, 0
	for received := 0; received < meteringConsumerReplayCount; received++ {
		select {
		case result := <-processed:
			if result.Inserted {
				inserted++
			}
			if result.Replayed {
				replayed++
			}
		case runErr := <-workerDone:
			t.Fatalf("worker stopped before processing all replays: %v", runErr)
		case <-time.After(meteringConsumerWaitDuration):
			t.Fatal("timed out waiting for metering replay processing")
		}
	}
	for offset := 0; offset < meteringConsumerReplayCount; offset++ {
		waitForMeteringConsumerSignal(t, committed, workerDone, "Kafka offset commit")
	}
	stopWorker()
	if err := <-workerDone; err != nil {
		t.Fatalf("metering worker shutdown error = %v", err)
	}
	if inserted != 1 || replayed != meteringConsumerReplayCount-1 {
		t.Fatalf("inserted/replayed results = %d/%d, want 1/%d",
			inserted, replayed, meteringConsumerReplayCount-1)
	}

	var ledgerCount, receiptCount int
	var quantity, amountMicros int64
	var priceVersionID, receiptGroup string
	err = database.QueryRowContext(ctx, `
		SELECT count(*), min(quantity), min(amount_micros), min(price_version_id::text)
		FROM app.usage_ledger_entries WHERE event_id = $1`, event.EventID,
	).Scan(&ledgerCount, &quantity, &amountMicros, &priceVersionID)
	if err != nil || ledgerCount != 1 || quantity != 13 || amountMicros != 33 ||
		priceVersionID != meteringConsumerPriceID {
		t.Fatalf("ledger fact = count:%d quantity:%d amount:%d price:%s error:%v",
			ledgerCount, quantity, amountMicros, priceVersionID, err)
	}
	err = database.QueryRowContext(ctx, `
		SELECT count(*), min(consumer_group) FROM app.usage_event_receipts WHERE event_id = $1`,
		event.EventID,
	).Scan(&receiptCount, &receiptGroup)
	if err != nil || receiptCount != 1 || receiptGroup != meteringConsumerGroup {
		t.Fatalf("receipt = count:%d group:%s error:%v", receiptCount, receiptGroup, err)
	}

	changed := event
	changed.Quantity++
	changedPayload, err := json.Marshal(changed)
	if err != nil {
		t.Fatalf("marshal conflicting event: %v", err)
	}
	if _, err := processor.Process(ctx, event.EventID, changedPayload); !errors.Is(err, meteringconsumer.ErrEventConflict) {
		t.Fatalf("conflicting event error = %v, want ErrEventConflict", err)
	}
	audioEvent := event
	audioEvent.EventID = "7e000000-0000-4000-8000-000000000103"
	audioEvent.TokenType = metering.TokenTypeAudioInput
	audioPayload, err := json.Marshal(audioEvent)
	if err != nil {
		t.Fatalf("marshal audio unit event: %v", err)
	}
	if _, err := processor.Process(ctx, audioEvent.EventID, audioPayload); !errors.Is(err, meteringconsumer.ErrPriceUnavailable) {
		t.Fatalf("audio token against second rate error = %v, want ErrPriceUnavailable", err)
	}
	var rejectedAudioFacts int
	err = database.QueryRowContext(ctx, `
		SELECT (SELECT count(*) FROM app.usage_event_receipts WHERE event_id = $1)
		     + (SELECT count(*) FROM app.usage_ledger_entries WHERE event_id = $1)`,
		audioEvent.EventID,
	).Scan(&rejectedAudioFacts)
	if err != nil || rejectedAudioFacts != 0 {
		t.Fatalf("rejected audio receipt+ledger count = %d, error = %v", rejectedAudioFacts, err)
	}
	_, err = database.ExecContext(ctx, `UPDATE app.usage_event_receipts SET consumer_group = 'changed' WHERE event_id = $1`, event.EventID)
	expectConstraint(t, err, "usage_event_receipts_append_only")
	_, err = database.ExecContext(ctx, `DELETE FROM app.usage_event_receipts WHERE event_id = $1`, event.EventID)
	expectConstraint(t, err, "usage_event_receipts_append_only")
}

type recordingMeteringHandler struct {
	processor *meteringconsumer.Processor
	processed chan<- meteringconsumer.Result
}

func (handler *recordingMeteringHandler) Process(
	ctx context.Context,
	key string,
	payload []byte,
) (meteringconsumer.Result, error) {
	result, err := handler.processor.Process(ctx, key, payload)
	if err == nil {
		handler.processed <- result
	}
	return result, err
}

func waitForMeteringConsumerSignal(
	t *testing.T,
	signal <-chan struct{},
	workerDone <-chan error,
	description string,
) {
	t.Helper()
	select {
	case <-signal:
	case err := <-workerDone:
		t.Fatalf("worker stopped before %s: %v", description, err)
	case <-time.After(meteringConsumerWaitDuration):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func cleanupMeteringConsumerFixtures(t *testing.T, database *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	statements := []string{
		`TRUNCATE app.usage_event_receipts, app.usage_event_outbox,
			app.usage_ledger_entries, app.price_version_rates, app.price_versions RESTART IDENTITY`,
		`DELETE FROM app.route_retry_decisions WHERE request_id LIKE 'integration-metering-%'`,
		`DELETE FROM app.route_decisions WHERE request_id LIKE 'integration-metering-%'`,
		`DELETE FROM app.route_attempt_status_events WHERE attempt_id IN (
			SELECT id FROM app.route_attempts WHERE request_id LIKE 'integration-metering-%')`,
		`DELETE FROM app.gateway_request_status_events WHERE request_id LIKE 'integration-metering-%'`,
		`DELETE FROM app.route_attempts WHERE request_id LIKE 'integration-metering-%'`,
		`DELETE FROM app.gateway_requests WHERE id LIKE 'integration-metering-%'`,
	}
	for _, statement := range statements {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Errorf("cleanup metering consumer fixtures: %v", err)
		}
	}
}
