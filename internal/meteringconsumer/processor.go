// Package meteringconsumer validates UsageEvents and writes idempotent priced ledger facts.
package meteringconsumer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"time"

	"github.com/lib/pq"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/metering"
)

const (
	maximumUsageEventBytes = 64 * 1024
	ledgerWriter           = "metering-worker:usage-v1"
)

var (
	// ErrInvalidEvent means the key or versioned payload is not a valid UsageEvent.
	ErrInvalidEvent = errors.New("metering consumer event is invalid")
	// ErrEventConflict means one event ID was reused for different semantic facts.
	ErrEventConflict = errors.New("metering consumer event id conflicts with an existing fact")
	// ErrPriceUnavailable means trusted attribution or an effective rate is not available.
	ErrPriceUnavailable = errors.New("metering consumer price is unavailable")
	// ErrStoreUnavailable means the durable idempotency transaction could not complete.
	ErrStoreUnavailable = errors.New("metering consumer store is unavailable")

	consumerGroupPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,127}$`)
)

// Result identifies whether processing created a ledger row or recognized a replay.
type Result struct {
	EventID        string
	PriceVersionID string
	AmountMicros   int64
	Inserted       bool
	Replayed       bool
}

// Processor atomically binds a canonical event receipt to one priced ledger entry.
type Processor struct {
	database      *sql.DB
	consumerGroup string
	now           func() time.Time
}

// NewProcessor validates the durable store and stable consumer identity.
func NewProcessor(database *sql.DB, consumerGroup string, now func() time.Time) (*Processor, error) {
	if database == nil || !consumerGroupPattern.MatchString(consumerGroup) || now == nil || now().IsZero() {
		return nil, ErrInvalidEvent
	}
	return &Processor{database: database, consumerGroup: consumerGroup, now: now}, nil
}

// Process validates one record and commits its receipt, selected price, and
// ledger entry in one transaction. A byte-different but semantically identical
// JSON replay is accepted after canonicalization.
func (processor *Processor) Process(ctx context.Context, key string, payload []byte) (Result, error) {
	if processor == nil || processor.database == nil || ctx == nil {
		return Result{}, ErrInvalidEvent
	}
	event, fingerprint, err := canonicalUsageEvent(key, payload)
	if err != nil {
		return Result{}, err
	}
	consumedAt := processor.now().UTC()
	if consumedAt.IsZero() {
		return Result{}, ErrStoreUnavailable
	}
	transaction, err := processor.database.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, newConsumerError(ErrStoreUnavailable, err)
	}
	defer func() { _ = transaction.Rollback() }()

	inserted, err := insertReceipt(ctx, transaction, event, fingerprint, processor.consumerGroup, consumedAt)
	if err != nil {
		return Result{}, err
	}
	if !inserted {
		result, err := loadReplay(ctx, transaction, event.EventID, fingerprint)
		if err != nil {
			return Result{}, err
		}
		if err := transaction.Commit(); err != nil {
			return Result{}, newConsumerError(ErrStoreUnavailable, err)
		}
		return result, nil
	}

	priceVersionID, rate, err := selectEffectiveRate(ctx, transaction, event)
	if err != nil {
		return Result{}, err
	}
	amountMicros, err := metering.CalculateAmountMicros(event.Quantity, rate)
	if err != nil {
		return Result{}, newConsumerError(ErrPriceUnavailable, err)
	}
	_, err = transaction.ExecContext(ctx, `
		INSERT INTO app.usage_ledger_entries (
			event_id, tenant_id, request_id, attempt_id, token_type,
			quantity, source, observed_at, created_at, created_by,
			price_version_id, amount_micros, event_schema_version,
			tokenizer, tokenizer_version, physical_model, deployment_version,
			provider_protocol_version
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
			$14, $15, $16, $17, $18
		)`,
		event.EventID, event.TenantID, event.RequestID, event.AttemptID,
		event.TokenType, event.Quantity, event.Source, event.ObservedAt,
		consumedAt, ledgerWriter, priceVersionID, amountMicros, event.SchemaVersion,
		estimateField(event.Estimate, func(value adapter.UsageEstimateMetadata) any { return value.Tokenizer }),
		estimateField(event.Estimate, func(value adapter.UsageEstimateMetadata) any { return value.TokenizerVersion }),
		estimateField(event.Estimate, func(value adapter.UsageEstimateMetadata) any { return value.PhysicalModel }),
		estimateField(event.Estimate, func(value adapter.UsageEstimateMetadata) any { return value.DeploymentVersion }),
		estimateField(event.Estimate, func(value adapter.UsageEstimateMetadata) any { return value.ProviderProtocolVersion }),
	)
	if err != nil {
		return Result{}, mapConsumerDatabaseError(err)
	}
	if err := transaction.Commit(); err != nil {
		return Result{}, newConsumerError(ErrStoreUnavailable, err)
	}
	return Result{
		EventID: event.EventID, PriceVersionID: priceVersionID,
		AmountMicros: amountMicros, Inserted: true,
	}, nil
}

func estimateField(
	metadata *adapter.UsageEstimateMetadata,
	value func(adapter.UsageEstimateMetadata) any,
) any {
	if metadata == nil {
		return nil
	}
	return value(*metadata)
}

func canonicalUsageEvent(key string, payload []byte) (metering.UsageEvent, [sha256.Size]byte, error) {
	if key == "" || len(payload) == 0 || len(payload) > maximumUsageEventBytes {
		return metering.UsageEvent{}, [sha256.Size]byte{}, ErrInvalidEvent
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var event metering.UsageEvent
	if err := decoder.Decode(&event); err != nil {
		return metering.UsageEvent{}, [sha256.Size]byte{}, newConsumerError(ErrInvalidEvent, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return metering.UsageEvent{}, [sha256.Size]byte{}, newConsumerError(ErrInvalidEvent, err)
	}
	// Version 1 originally carried only token-count dimensions. Missing unit is
	// therefore backward-compatible as token, while all new publishers emit it.
	if event.BillingUnit == "" {
		event.BillingUnit = metering.BillingUnitToken
	}
	event.ObservedAt = event.ObservedAt.UTC()
	if event.EventID != key || event.Validate() != nil {
		return metering.UsageEvent{}, [sha256.Size]byte{}, ErrInvalidEvent
	}
	canonical, err := json.Marshal(event)
	if err != nil {
		return metering.UsageEvent{}, [sha256.Size]byte{}, newConsumerError(ErrInvalidEvent, err)
	}
	return event, sha256.Sum256(canonical), nil
}

func insertReceipt(
	ctx context.Context,
	transaction *sql.Tx,
	event metering.UsageEvent,
	fingerprint [sha256.Size]byte,
	consumerGroup string,
	consumedAt time.Time,
) (bool, error) {
	result, err := transaction.ExecContext(ctx, `
		INSERT INTO app.usage_event_receipts (
			event_id, schema_version, payload_sha256, consumer_group, consumed_at, created_by
		) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (event_id) DO NOTHING`,
		event.EventID, event.SchemaVersion, fingerprint[:], consumerGroup, consumedAt, ledgerWriter,
	)
	if err != nil {
		return false, mapConsumerDatabaseError(err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, newConsumerError(ErrStoreUnavailable, err)
	}
	return rows == 1, nil
}

func loadReplay(
	ctx context.Context,
	transaction *sql.Tx,
	eventID string,
	fingerprint [sha256.Size]byte,
) (Result, error) {
	var storedFingerprint []byte
	var priceVersionID string
	var amountMicros int64
	err := transaction.QueryRowContext(ctx, `
		SELECT receipt.payload_sha256, ledger.price_version_id, ledger.amount_micros
		FROM app.usage_event_receipts AS receipt
		JOIN app.usage_ledger_entries AS ledger ON ledger.event_id = receipt.event_id
		WHERE receipt.event_id = $1`, eventID,
	).Scan(&storedFingerprint, &priceVersionID, &amountMicros)
	if err != nil {
		return Result{}, newConsumerError(ErrStoreUnavailable, err)
	}
	if !bytes.Equal(storedFingerprint, fingerprint[:]) {
		return Result{}, ErrEventConflict
	}
	return Result{
		EventID: eventID, PriceVersionID: priceVersionID,
		AmountMicros: amountMicros, Replayed: true,
	}, nil
}

func selectEffectiveRate(
	ctx context.Context,
	transaction *sql.Tx,
	event metering.UsageEvent,
) (string, metering.PriceRate, error) {
	var priceVersionID string
	var billingUnit metering.BillingUnit
	var unitQuantity, unitPriceMicros int64
	err := transaction.QueryRowContext(ctx, `
		SELECT price.id, rate.billing_unit, rate.unit_quantity, rate.unit_price_micros
		FROM app.gateway_requests AS request
		JOIN app.route_attempts AS attempt
		  ON attempt.request_id = request.id
		JOIN app.deployments AS deployment
		  ON deployment.id = attempt.deployment_id
		JOIN app.price_versions AS price
		  ON price.deployment_id = deployment.id
		 AND price.region = deployment.region
		 AND price.status = 'published'
		 AND price.effective_at <= $5
		JOIN app.price_version_rates AS rate
		  ON rate.price_version_id = price.id
		 AND rate.token_type = $6 AND rate.billing_unit = $7
		WHERE request.tenant_id = $1 AND request.id = $2
		  AND attempt.id = $3 AND attempt.deployment_id = $4
		ORDER BY price.effective_at DESC, price.id DESC
		LIMIT 1`,
		event.TenantID, event.RequestID, event.AttemptID,
		event.DeploymentID, event.ObservedAt, event.TokenType, event.BillingUnit,
	).Scan(&priceVersionID, &billingUnit, &unitQuantity, &unitPriceMicros)
	if errors.Is(err, sql.ErrNoRows) {
		return "", metering.PriceRate{}, ErrPriceUnavailable
	}
	if err != nil {
		return "", metering.PriceRate{}, newConsumerError(ErrStoreUnavailable, err)
	}
	rate := metering.PriceRate{
		TokenType: event.TokenType, BillingUnit: billingUnit,
		UnitQuantity: unitQuantity, UnitPriceMicros: unitPriceMicros,
	}
	if rate.Validate() != nil {
		return "", metering.PriceRate{}, ErrPriceUnavailable
	}
	return priceVersionID, rate, nil
}

func mapConsumerDatabaseError(err error) error {
	var databaseError *pq.Error
	if errors.As(err, &databaseError) && databaseError.Code == "23505" {
		return newConsumerError(ErrEventConflict, err)
	}
	return newConsumerError(ErrStoreUnavailable, err)
}

type consumerError struct {
	kind  error
	cause error
}

func newConsumerError(kind, cause error) error {
	if cause == nil {
		return kind
	}
	return &consumerError{kind: kind, cause: cause}
}

func (failure *consumerError) Error() string {
	if failure == nil || failure.kind == nil {
		return "metering consumer failed"
	}
	return failure.kind.Error()
}

func (failure *consumerError) Unwrap() []error {
	if failure == nil {
		return nil
	}
	return []error{failure.kind, failure.cause}
}
