//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/execution"
	"github.com/zse04152005-del/ai-gateway-platform/internal/metering"
	"github.com/zse04152005-del/ai-gateway-platform/internal/meteringoutbox"
)

const (
	usageOutboxRequestID       = "integration-execution-usage-outbox"
	usageOutboxAtomicRequestID = "integration-execution-usage-outbox-atomic"
	usageOutboxConflictEventID = "7d000000-0000-4000-8000-000000000101"
	usageOutboxExpiredLeaseID  = "7d000000-0000-4000-8000-000000000102"
)

func TestUsageEventOutboxAtomicHandoffAndBoundedRelay(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set")
	}
	database, err := sql.Open("postgres", databaseURL)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	database.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		t.Fatalf("database.PingContext() error = %v", err)
	}

	cleanupGatewayExecutionFixtures(t, database)
	cleanupModelListFixtures(t, database)
	t.Cleanup(func() {
		cleanupGatewayExecutionFixtures(t, database)
		cleanupModelListFixtures(t, database)
	})
	seedModelListCatalog(ctx, t, database)
	seedExecutionVirtualKey(ctx, t, database)

	now := time.Now().UTC().Truncate(time.Microsecond)
	recorder, err := execution.NewPostgresRecorder(database, func() time.Time {
		now = now.Add(time.Microsecond)
		return now
	}, rand.Reader)
	if err != nil {
		t.Fatalf("execution.NewPostgresRecorder() error = %v", err)
	}

	request := startExecutionRequest(ctx, t, recorder, usageOutboxRequestID)
	request, err = recorder.MarkRouting(ctx, request)
	if err != nil {
		t.Fatalf("MarkRouting(outbox) error = %v", err)
	}
	request, attempt, err := recorder.StartAttempt(ctx, request, modelListDeploymentAID)
	if err != nil {
		t.Fatalf("StartAttempt(outbox) error = %v", err)
	}
	usage := adapter.NormalizedUsage{
		InputTokens: adapter.Tokens(13), OutputTokens: adapter.Tokens(3),
		CacheReadTokens: adapter.Tokens(0), Source: adapter.UsageSourceEstimated, Complete: true,
	}
	completedRequest, completedAttempt, err := recorder.CompleteAttempt(ctx, request, attempt, execution.AttemptOutcome{
		AttemptStatus: execution.AttemptSucceeded, RequestStatus: execution.RequestSucceeded,
		HeadersReceived: true, EndReason: "completed", Usage: &usage,
	})
	if err != nil || completedRequest.Status != execution.RequestSucceeded ||
		completedAttempt.Status != execution.AttemptSucceeded {
		t.Fatalf("CompleteAttempt(outbox) = %+v/%+v/%v", completedRequest, completedAttempt, err)
	}
	var pendingCount, distinctEventCount, positiveCount int
	err = database.QueryRowContext(ctx, `
		SELECT count(*), count(DISTINCT event_id),
			count(*) FILTER (WHERE quantity > 0 AND status = 'pending' AND publish_attempts = 0)
		FROM app.usage_event_outbox
		WHERE request_id = $1`, usageOutboxRequestID,
	).Scan(&pendingCount, &distinctEventCount, &positiveCount)
	if err != nil || pendingCount != 2 || distinctEventCount != 2 || positiveCount != 2 {
		t.Fatalf("pending/distinct/positive events = %d/%d/%d, error = %v",
			pendingCount, distinctEventCount, positiveCount, err)
	}

	relayNow := now.Add(time.Second)
	sink := &recordingUsageEventSink{failure: errors.New("private broker failure")}
	options := meteringoutbox.DefaultOptions()
	options.BatchSize = 2
	options.Now = func() time.Time { return relayNow }
	options.Random = bytes.NewReader(make([]byte, 64))
	options.PublishTimeout = 100 * time.Millisecond
	options.LeaseDuration = time.Second
	options.MinimumRetry = time.Second
	options.MaximumRetry = 4 * time.Second
	relay, err := meteringoutbox.New(database, sink, options)
	if err != nil {
		t.Fatalf("meteringoutbox.New() error = %v", err)
	}
	failedBatch, err := relay.RelayOnce(ctx)
	if err != nil || failedBatch.Claimed != 2 || failedBatch.Retried != 2 || failedBatch.Published != 0 {
		t.Fatalf("failed relay batch = %+v, %v", failedBatch, err)
	}
	var retriedCount int
	err = database.QueryRowContext(ctx, `
		SELECT count(*) FROM app.usage_event_outbox
		WHERE request_id = $1 AND status = 'pending' AND publish_attempts = 1
			AND last_error_code = 'EVENT_BUS_UNAVAILABLE'`, usageOutboxRequestID,
	).Scan(&retriedCount)
	if err != nil || retriedCount != 2 {
		t.Fatalf("durable retry count = %d, error = %v", retriedCount, err)
	}

	relayNow = relayNow.Add(time.Second)
	sink.failure = nil
	successfulBatch, err := relay.RelayOnce(ctx)
	if err != nil || successfulBatch.Claimed != 2 || successfulBatch.Published != 2 ||
		successfulBatch.Retried != 0 || len(sink.payloads) != 2 {
		t.Fatalf("successful relay batch = %+v, payloads = %d, error = %v",
			successfulBatch, len(sink.payloads), err)
	}
	for index, payload := range sink.payloads {
		var event metering.UsageEvent
		if err := json.Unmarshal(payload, &event); err != nil || event.Validate() != nil ||
			sink.keys[index] != event.EventID || event.RequestID != usageOutboxRequestID ||
			event.AttemptID != attempt.ID || event.DeploymentID != modelListDeploymentAID {
			t.Fatalf("published event[%d] = %+v, key = %q, decode error = %v",
				index, event, sink.keys[index], err)
		}
		for _, forbidden := range []string{"prompt", "response", "credential", "private", "broker"} {
			if strings.Contains(strings.ToLower(string(payload)), forbidden) {
				t.Fatalf("published event[%d] leaked %q: %s", index, forbidden, payload)
			}
		}
	}
	var publishedCount int
	err = database.QueryRowContext(ctx, `
		SELECT count(*) FROM app.usage_event_outbox
		WHERE request_id = $1 AND status = 'published' AND published_at IS NOT NULL
			AND lease_id IS NULL AND lease_expires_at IS NULL AND last_error_code IS NULL`, usageOutboxRequestID,
	).Scan(&publishedCount)
	if err != nil || publishedCount != 2 {
		t.Fatalf("published outbox count = %d, error = %v", publishedCount, err)
	}

	atomicRequest := startExecutionRequest(ctx, t, recorder, usageOutboxAtomicRequestID)
	atomicRequest, err = recorder.MarkRouting(ctx, atomicRequest)
	if err != nil {
		t.Fatalf("MarkRouting(atomic) error = %v", err)
	}
	atomicRequest, atomicAttempt, err := recorder.StartAttempt(ctx, atomicRequest, modelListDeploymentAID)
	if err != nil {
		t.Fatalf("StartAttempt(atomic) error = %v", err)
	}
	_, err = database.ExecContext(ctx, `
		INSERT INTO app.usage_event_outbox (
			event_id, schema_version, kind, tenant_id, request_id, attempt_id,
			deployment_id, token_type, quantity, source, usage_complete,
			observed_at, trace_id, span_id, available_at, created_at, created_by, updated_at
		) VALUES ($1, 1, 'usage.estimated', $2, $3, $4, $5, 'input', 1,
			'estimated', false, $6, $7, $8, $6, $6, 'integration:usage-outbox', $6)`,
		usageOutboxConflictEventID, atomicRequest.TenantID, atomicRequest.ID,
		atomicAttempt.ID, atomicAttempt.DeploymentID, now,
		atomicRequest.TraceID, atomicRequest.SpanID)
	if err != nil {
		t.Fatalf("seed conflicting outbox event: %v", err)
	}
	atomicUsage := adapter.NormalizedUsage{
		InputTokens: adapter.Tokens(1), Source: adapter.UsageSourceEstimated,
	}
	_, _, err = recorder.CompleteAttempt(ctx, atomicRequest, atomicAttempt, execution.AttemptOutcome{
		AttemptStatus: execution.AttemptSucceeded, RequestStatus: execution.RequestSucceeded,
		HeadersReceived: true, EndReason: "completed", Usage: &atomicUsage,
	})
	if !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("conflicting atomic completion error = %v, want ErrConflict", err)
	}
	var requestStatus, attemptStatus string
	err = database.QueryRowContext(ctx, `
		SELECT request.status, attempt.status
		FROM app.gateway_requests AS request
		JOIN app.route_attempts AS attempt ON attempt.request_id = request.id
		WHERE request.id = $1 AND attempt.id = $2`,
		atomicRequest.ID, atomicAttempt.ID,
	).Scan(&requestStatus, &attemptStatus)
	if err != nil || requestStatus != "running" || attemptStatus != "connecting" {
		t.Fatalf("rolled-back request/attempt = %s/%s, error = %v", requestStatus, attemptStatus, err)
	}
	_, err = database.ExecContext(ctx, `
		UPDATE app.usage_event_outbox SET quantity = 2, updated_at = $2 WHERE event_id = $1`,
		usageOutboxConflictEventID, now.Add(time.Second))
	expectConstraint(t, err, "usage_event_outbox_facts_immutable")
	_, err = database.ExecContext(ctx, `DELETE FROM app.usage_event_outbox WHERE event_id = $1`, usageOutboxConflictEventID)
	expectConstraint(t, err, "usage_event_outbox_delete_published_only")

	_, err = database.ExecContext(ctx, `
		UPDATE app.usage_event_outbox
		SET status = 'publishing', lease_id = $2, lease_expires_at = $3, updated_at = $4
		WHERE event_id = $1`, usageOutboxConflictEventID, usageOutboxExpiredLeaseID,
		relayNow, relayNow.Add(-time.Millisecond))
	if err != nil {
		t.Fatalf("seed expired publishing lease: %v", err)
	}
	recoveredBatch, err := relay.RelayOnce(ctx)
	if err != nil || recoveredBatch.Reclaimed != 1 || recoveredBatch.Claimed != 1 ||
		recoveredBatch.Published != 1 || recoveredBatch.Retried != 0 {
		t.Fatalf("expired lease recovery batch = %+v, error = %v", recoveredBatch, err)
	}
	var recoveredAttempts int
	err = database.QueryRowContext(ctx, `
		SELECT publish_attempts FROM app.usage_event_outbox
		WHERE event_id = $1 AND status = 'published' AND last_error_code IS NULL`,
		usageOutboxConflictEventID,
	).Scan(&recoveredAttempts)
	if err != nil || recoveredAttempts != 1 {
		t.Fatalf("recovered lease publish attempts = %d, error = %v", recoveredAttempts, err)
	}
}

func TestKafkaUsageEventSinkAcknowledgement(t *testing.T) {
	rawBrokers := strings.TrimSpace(os.Getenv("KAFKA_BROKERS"))
	if rawBrokers == "" {
		t.Skip("KAFKA_BROKERS is not set")
	}
	brokers := strings.Split(rawBrokers, ",")
	for index := range brokers {
		brokers[index] = strings.TrimSpace(brokers[index])
	}
	sink, err := meteringoutbox.NewKafkaSink(brokers)
	if err != nil {
		t.Fatalf("meteringoutbox.NewKafkaSink() error = %v", err)
	}
	t.Cleanup(sink.Close)
	event := metering.UsageEvent{
		EventID:       "7d000000-0000-4000-8000-000000000201",
		SchemaVersion: metering.UsageEventSchemaVersion,
		Kind:          metering.UsageEventEstimated,
		TenantID:      modelListTenantOneID,
		RequestID:     "integration-execution-kafka-usage",
		AttemptID:     "7d000000-0000-4000-8000-000000000202",
		DeploymentID:  modelListDeploymentAID,
		TokenType:     metering.TokenTypeInput,
		Quantity:      1,
		Source:        metering.SourceEstimated,
		ObservedAt:    time.Now().UTC(),
		TraceID:       "7d000000000000000000000000000001",
		SpanID:        "7d00000000000001",
	}
	payload, err := json.Marshal(event)
	if err != nil || event.Validate() != nil {
		t.Fatalf("marshal Kafka usage event = %s, %v", payload, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sink.Publish(ctx, event.EventID, payload); err != nil {
		t.Fatalf("KafkaSink.Publish() error = %v", err)
	}
}

type recordingUsageEventSink struct {
	failure  error
	keys     []string
	payloads [][]byte
}

func (sink *recordingUsageEventSink) Publish(_ context.Context, key string, payload []byte) error {
	if sink.failure != nil {
		return sink.failure
	}
	sink.keys = append(sink.keys, key)
	sink.payloads = append(sink.payloads, append([]byte(nil), payload...))
	return nil
}
