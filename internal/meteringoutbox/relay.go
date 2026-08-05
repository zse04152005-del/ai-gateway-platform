// Package meteringoutbox relays transactional usage facts to the event bus.
package meteringoutbox

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/metering"
)

const maximumBatchSize = 1000

var (
	// ErrInvalid means relay dependencies or bounds are unsafe.
	ErrInvalid = errors.New("usage event outbox relay is invalid")
	// ErrStoreUnavailable means PostgreSQL could not claim or advance durable outbox state.
	ErrStoreUnavailable = errors.New("usage event outbox store is unavailable")
)

// Sink synchronously acknowledges one event-bus record or returns an error.
type Sink interface {
	Publish(context.Context, string, []byte) error
}

// Options bounds database claims, broker calls, retry delay, and shutdown latency.
type Options struct {
	BatchSize        int
	PollInterval     time.Duration
	LeaseDuration    time.Duration
	PublishTimeout   time.Duration
	MinimumRetry     time.Duration
	MaximumRetry     time.Duration
	Now              func() time.Time
	Random           io.Reader
	OnTransientError func(error)
}

// DefaultOptions returns production-safe finite relay bounds.
func DefaultOptions() Options {
	return Options{
		BatchSize: 100, PollInterval: 250 * time.Millisecond,
		LeaseDuration: 10 * time.Second, PublishTimeout: 2 * time.Second,
		MinimumRetry: time.Second, MaximumRetry: time.Minute,
		Now: time.Now, Random: rand.Reader,
	}
}

// BatchResult reports durable state transitions from one bounded relay pass.
type BatchResult struct {
	Reclaimed int
	Claimed   int
	Published int
	Retried   int
}

// Relay performs short database claims and publishes outside PostgreSQL transactions.
type Relay struct {
	database         *sql.DB
	sink             Sink
	batchSize        int
	pollInterval     time.Duration
	leaseDuration    time.Duration
	publishTimeout   time.Duration
	minimumRetry     time.Duration
	maximumRetry     time.Duration
	now              func() time.Time
	random           io.Reader
	onTransientError func(error)
}

// New validates process-scoped relay dependencies and finite bounds.
func New(database *sql.DB, sink Sink, options Options) (*Relay, error) {
	if database == nil || sink == nil || options.BatchSize < 1 || options.BatchSize > maximumBatchSize ||
		options.PollInterval <= 0 || options.LeaseDuration <= options.PublishTimeout ||
		options.PublishTimeout <= 0 || options.MinimumRetry <= 0 ||
		options.MaximumRetry < options.MinimumRetry || options.Now == nil ||
		options.Now().IsZero() || options.Random == nil {
		return nil, ErrInvalid
	}
	return &Relay{
		database: database, sink: sink, batchSize: options.BatchSize,
		pollInterval: options.PollInterval, leaseDuration: options.LeaseDuration,
		publishTimeout: options.PublishTimeout, minimumRetry: options.MinimumRetry,
		maximumRetry: options.MaximumRetry, now: options.Now, random: options.Random,
		onTransientError: options.OnTransientError,
	}, nil
}

// Run continually retries finite batches until cancellation. Transient database
// or broker failures never discard events or terminate the gateway process.
func (relay *Relay) Run(ctx context.Context) error {
	if relay == nil || relay.database == nil || relay.sink == nil || ctx == nil {
		return ErrInvalid
	}
	for ctx.Err() == nil {
		_, err := relay.RelayOnce(ctx)
		if ctx.Err() != nil {
			continue
		}
		if err != nil && !errors.Is(err, context.Canceled) && relay.onTransientError != nil {
			relay.onTransientError(err)
		}
		timer := time.NewTimer(relay.pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case <-timer.C:
		}
	}
	return nil
}

// RelayOnce reclaims expired leases, claims at most one bounded batch, and
// records either a broker acknowledgement or a durable retry for every event.
func (relay *Relay) RelayOnce(ctx context.Context) (BatchResult, error) {
	if relay == nil || relay.database == nil || relay.sink == nil || ctx == nil {
		return BatchResult{}, ErrInvalid
	}
	now := relay.now().UTC()
	leaseID, err := newUUID(relay.random)
	if err != nil {
		return BatchResult{}, newRelayError(ErrStoreUnavailable, err)
	}
	events, reclaimed, err := relay.claim(ctx, leaseID, now)
	if err != nil {
		return BatchResult{}, err
	}
	result := BatchResult{Reclaimed: reclaimed, Claimed: len(events)}
	for _, event := range events {
		payload, marshalErr := json.Marshal(event.UsageEvent)
		if marshalErr != nil || event.Validate() != nil {
			if marshalErr == nil {
				marshalErr = metering.ErrInvalidUsageEvent
			}
			return result, newRelayError(ErrStoreUnavailable, marshalErr)
		}
		publishCtx, cancel := context.WithTimeout(ctx, relay.publishTimeout)
		publishErr := relay.sink.Publish(publishCtx, event.EventID, payload)
		cancel()
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		transitionAt := relay.now().UTC()
		if publishErr == nil {
			if err := relay.markPublished(ctx, event.EventID, leaseID, transitionAt); err != nil {
				return result, err
			}
			result.Published++
			continue
		}
		availableAt := transitionAt.Add(relay.retryDelay(event.PublishAttempts))
		if err := relay.markRetry(ctx, event.EventID, leaseID, transitionAt, availableAt); err != nil {
			return result, err
		}
		result.Retried++
	}
	return result, nil
}

type claimedEvent struct {
	metering.UsageEvent
	PublishAttempts int
}

func (relay *Relay) claim(
	ctx context.Context,
	leaseID string,
	now time.Time,
) ([]claimedEvent, int, error) {
	transaction, err := relay.database.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, newRelayError(ErrStoreUnavailable, err)
	}
	defer func() { _ = transaction.Rollback() }()
	reclaimResult, err := transaction.ExecContext(ctx, `
		WITH expired AS (
			SELECT event_id
			FROM app.usage_event_outbox
			WHERE status = 'publishing' AND lease_expires_at <= $1
			ORDER BY lease_expires_at, event_id
			FOR UPDATE SKIP LOCKED
			LIMIT $2
		)
		UPDATE app.usage_event_outbox AS event
		SET status = 'pending', publish_attempts = event.publish_attempts + 1,
			available_at = $1, lease_id = NULL, lease_expires_at = NULL,
			last_error_code = 'LEASE_EXPIRED', updated_at = $1
		FROM expired
		WHERE event.event_id = expired.event_id`, now, relay.batchSize)
	if err != nil {
		return nil, 0, newRelayError(ErrStoreUnavailable, err)
	}
	reclaimedCount, err := reclaimResult.RowsAffected()
	if err != nil {
		return nil, 0, newRelayError(ErrStoreUnavailable, err)
	}
	rows, err := transaction.QueryContext(ctx, `
		WITH candidates AS (
			SELECT event_id
			FROM app.usage_event_outbox
			WHERE status = 'pending' AND available_at <= $1
			ORDER BY available_at, created_at, event_id
			FOR UPDATE SKIP LOCKED
			LIMIT $2
		)
		UPDATE app.usage_event_outbox AS event
		SET status = 'publishing', lease_id = $3,
			lease_expires_at = $4, updated_at = $1
		FROM candidates
		WHERE event.event_id = candidates.event_id
		RETURNING event.event_id, event.schema_version, event.kind,
			event.tenant_id, event.request_id, event.attempt_id, event.deployment_id,
			event.token_type, event.billing_unit, event.quantity, event.source, event.usage_complete,
			event.tokenizer, event.tokenizer_version, event.physical_model,
			event.deployment_version, event.provider_protocol_version,
			event.observed_at, event.trace_id, event.span_id, event.publish_attempts`,
		now, relay.batchSize, leaseID, now.Add(relay.leaseDuration))
	if err != nil {
		return nil, 0, newRelayError(ErrStoreUnavailable, err)
	}
	events := make([]claimedEvent, 0, relay.batchSize)
	for rows.Next() {
		var event claimedEvent
		var tokenizer, tokenizerVersion, physicalModel, providerProtocolVersion sql.NullString
		var deploymentVersion sql.NullInt64
		if err := rows.Scan(
			&event.EventID, &event.SchemaVersion, &event.Kind,
			&event.TenantID, &event.RequestID, &event.AttemptID, &event.DeploymentID,
			&event.TokenType, &event.BillingUnit, &event.Quantity, &event.Source, &event.UsageComplete,
			&tokenizer, &tokenizerVersion, &physicalModel, &deploymentVersion, &providerProtocolVersion,
			&event.ObservedAt, &event.TraceID, &event.SpanID, &event.PublishAttempts,
		); err != nil {
			_ = rows.Close()
			return nil, 0, newRelayError(ErrStoreUnavailable, err)
		}
		if event.SchemaVersion == metering.UsageEventSchemaVersion && event.Source == metering.SourceEstimated &&
			tokenizer.Valid && tokenizerVersion.Valid && physicalModel.Valid && deploymentVersion.Valid &&
			providerProtocolVersion.Valid {
			event.Estimate = &adapter.UsageEstimateMetadata{
				Estimated: true, Tokenizer: tokenizer.String, TokenizerVersion: tokenizerVersion.String,
				PhysicalModel: physicalModel.String, DeploymentVersion: deploymentVersion.Int64,
				ProviderProtocolVersion: providerProtocolVersion.String,
			}
		}
		events = append(events, event)
	}
	if err := rows.Close(); err != nil {
		return nil, 0, newRelayError(ErrStoreUnavailable, err)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, newRelayError(ErrStoreUnavailable, err)
	}
	if err := transaction.Commit(); err != nil {
		return nil, 0, newRelayError(ErrStoreUnavailable, err)
	}
	return events, int(reclaimedCount), nil
}

func (relay *Relay) markPublished(ctx context.Context, eventID, leaseID string, now time.Time) error {
	result, err := relay.database.ExecContext(ctx, `
		UPDATE app.usage_event_outbox
		SET status = 'published', lease_id = NULL, lease_expires_at = NULL,
			published_at = $3, last_error_code = NULL, updated_at = $3
		WHERE event_id = $1 AND status = 'publishing' AND lease_id = $2`,
		eventID, leaseID, now)
	return expectOneTransition(result, err)
}

func (relay *Relay) markRetry(
	ctx context.Context,
	eventID, leaseID string,
	now, availableAt time.Time,
) error {
	result, err := relay.database.ExecContext(ctx, `
		UPDATE app.usage_event_outbox
		SET status = 'pending', publish_attempts = publish_attempts + 1,
			available_at = $3, lease_id = NULL, lease_expires_at = NULL,
			last_error_code = 'EVENT_BUS_UNAVAILABLE', updated_at = $4
		WHERE event_id = $1 AND status = 'publishing' AND lease_id = $2`,
		eventID, leaseID, availableAt, now)
	return expectOneTransition(result, err)
}

func expectOneTransition(result sql.Result, err error) error {
	if err != nil {
		return newRelayError(ErrStoreUnavailable, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return newRelayError(ErrStoreUnavailable, err)
	}
	if updated != 1 {
		return newRelayError(ErrStoreUnavailable, fmt.Errorf("updated %d outbox rows", updated))
	}
	return nil
}

func (relay *Relay) retryDelay(publishAttempts int) time.Duration {
	delay := relay.minimumRetry
	for attempt := 0; attempt < publishAttempts && delay < relay.maximumRetry; attempt++ {
		if delay > relay.maximumRetry/2 {
			return relay.maximumRetry
		}
		delay *= 2
	}
	if delay > relay.maximumRetry {
		return relay.maximumRetry
	}
	return delay
}

func newUUID(reader io.Reader) (string, error) {
	raw := make([]byte, 16)
	if _, err := io.ReadFull(reader, raw); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf(
		"%08x-%04x-%04x-%04x-%012x",
		raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16],
	), nil
}

type relayError struct {
	kind  error
	cause error
}

func newRelayError(kind, cause error) error {
	if cause == nil {
		return kind
	}
	return &relayError{kind: kind, cause: cause}
}

func (failure *relayError) Error() string {
	if failure == nil || failure.kind == nil {
		return "usage event relay failed"
	}
	return failure.kind.Error()
}

func (failure *relayError) Unwrap() []error {
	if failure == nil {
		return nil
	}
	return []error{failure.kind, failure.cause}
}
