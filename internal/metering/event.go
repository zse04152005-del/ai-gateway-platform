package metering

import (
	"errors"
	"regexp"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
)

const (
	// UsageEventSchemaVersionV1 is the historical token-count contract accepted during upgrades.
	UsageEventSchemaVersionV1 = 1
	// UsageEventSchemaVersion is the current wire contract. Version 2 adds
	// mandatory content-free tokenizer/model evidence to estimated facts.
	UsageEventSchemaVersion = 2
	// UsageEventTopic is the Kafka-compatible topic for normalized usage facts.
	UsageEventTopic = "ai-gateway.usage.v1"
)

var (
	// ErrInvalidUsageEvent means an event could change tenant attribution or billing meaning.
	ErrInvalidUsageEvent = errors.New("metering usage event is invalid")
	// ErrUsageEventID means the caller could not allocate a globally unique event identity.
	ErrUsageEventID = errors.New("metering usage event id generation failed")

	eventRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`)
	eventTraceIDPattern   = regexp.MustCompile(`^[0-9a-f]{32}$`)
	eventSpanIDPattern    = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// UsageEventKind distinguishes provider observations from gateway-owned estimates.
type UsageEventKind string

// Supported UsageEventKind values are deliberately finite and source-compatible.
const (
	UsageEventObserved  UsageEventKind = "usage.observed"
	UsageEventEstimated UsageEventKind = "usage.estimated"
)

// UsageIdentity contains trusted, content-free execution and trace attribution.
type UsageIdentity struct {
	TenantID     string
	RequestID    string
	AttemptID    string
	DeploymentID string
	TraceID      string
	SpanID       string
	ObservedAt   time.Time
}

// UsageEvent is one independently priced positive usage fact on the versioned wire contract.
type UsageEvent struct {
	EventID       string                         `json:"event_id"`
	SchemaVersion int                            `json:"schema_version"`
	Kind          UsageEventKind                 `json:"kind"`
	TenantID      string                         `json:"tenant_id"`
	RequestID     string                         `json:"request_id"`
	AttemptID     string                         `json:"attempt_id"`
	DeploymentID  string                         `json:"deployment_id"`
	TokenType     TokenType                      `json:"token_type"`
	BillingUnit   BillingUnit                    `json:"billing_unit"`
	Quantity      int64                          `json:"quantity"`
	Source        Source                         `json:"source"`
	UsageComplete bool                           `json:"usage_complete"`
	Estimate      *adapter.UsageEstimateMetadata `json:"estimate,omitempty"`
	ObservedAt    time.Time                      `json:"observed_at"`
	TraceID       string                         `json:"trace_id"`
	SpanID        string                         `json:"span_id"`
}

// EventIDFactory allocates one stable UUID for each independently priced fact.
type EventIDFactory func() (string, error)

// NewUsageEvents converts one normalized Attempt summary into independent positive facts.
// Missing and explicitly reported zero dimensions do not create billable events.
func NewUsageEvents(
	identity UsageIdentity,
	usage *adapter.NormalizedUsage,
	newEventID EventIDFactory,
) ([]UsageEvent, error) {
	if usage == nil {
		return nil, nil
	}
	if validateUsageIdentity(identity) != nil || usage.Validate() != nil || newEventID == nil {
		return nil, ErrInvalidUsageEvent
	}
	kind, validSource := gatewayUsageKind(usage.Source)
	if !validSource {
		return nil, ErrInvalidUsageEvent
	}
	dimensions := []struct {
		tokenType TokenType
		count     adapter.TokenCount
	}{
		{TokenTypeInput, usage.InputTokens},
		{TokenTypeOutput, usage.OutputTokens},
		{TokenTypeCacheRead, usage.CacheReadTokens},
		{TokenTypeCacheWrite, usage.CacheWriteTokens},
		{TokenTypeReasoning, usage.ReasoningTokens},
		{TokenTypeAudioInput, usage.AudioInputTokens},
		{TokenTypeAudioOutput, usage.AudioOutputTokens},
	}
	events := make([]UsageEvent, 0, len(dimensions))
	for _, dimension := range dimensions {
		if !dimension.count.Present || dimension.count.Value == 0 {
			continue
		}
		if dimension.count.Value < 0 || dimension.count.Value > MaximumExactInteger {
			return nil, ErrInvalidUsageEvent
		}
		eventID, err := newEventID()
		if err != nil {
			return nil, ErrUsageEventID
		}
		event := UsageEvent{
			EventID: eventID, SchemaVersion: UsageEventSchemaVersion, Kind: kind,
			TenantID: identity.TenantID, RequestID: identity.RequestID,
			AttemptID: identity.AttemptID, DeploymentID: identity.DeploymentID,
			TokenType: dimension.tokenType, BillingUnit: BillingUnitToken,
			Quantity: dimension.count.Value,
			Source:   usage.Source, UsageComplete: usage.Complete,
			Estimate:   usage.Clone().Estimate,
			ObservedAt: identity.ObservedAt.UTC(), TraceID: identity.TraceID, SpanID: identity.SpanID,
		}
		if event.Validate() != nil {
			return nil, ErrInvalidUsageEvent
		}
		events = append(events, event)
	}
	return events, nil
}

// Validate checks the complete wire event without accepting implicit normalization.
func (event UsageEvent) Validate() error {
	identity := UsageIdentity{
		TenantID: event.TenantID, RequestID: event.RequestID,
		AttemptID: event.AttemptID, DeploymentID: event.DeploymentID,
		TraceID: event.TraceID, SpanID: event.SpanID, ObservedAt: event.ObservedAt,
	}
	kind, validSource := gatewayUsageKind(event.Source)
	if (event.SchemaVersion != UsageEventSchemaVersionV1 && event.SchemaVersion != UsageEventSchemaVersion) ||
		!priceUUIDPattern.MatchString(event.EventID) ||
		validateUsageIdentity(identity) != nil || !event.TokenType.Valid() ||
		event.BillingUnit != BillingUnitToken ||
		event.Quantity < 1 || event.Quantity > MaximumExactInteger || !validSource || event.Kind != kind {
		return ErrInvalidUsageEvent
	}
	if event.SchemaVersion == UsageEventSchemaVersionV1 {
		if event.Estimate != nil {
			return ErrInvalidUsageEvent
		}
		return nil
	}
	if event.Source == SourceEstimated {
		if event.Estimate == nil || event.Estimate.Validate() != nil {
			return ErrInvalidUsageEvent
		}
	} else if event.Estimate != nil {
		return ErrInvalidUsageEvent
	}
	return nil
}

func validateUsageIdentity(identity UsageIdentity) error {
	if !priceUUIDPattern.MatchString(identity.TenantID) ||
		!eventRequestIDPattern.MatchString(identity.RequestID) ||
		!priceUUIDPattern.MatchString(identity.AttemptID) ||
		!priceUUIDPattern.MatchString(identity.DeploymentID) ||
		!eventTraceIDPattern.MatchString(identity.TraceID) ||
		!eventSpanIDPattern.MatchString(identity.SpanID) || identity.ObservedAt.IsZero() {
		return ErrInvalidUsageEvent
	}
	return nil
}

func gatewayUsageKind(source Source) (UsageEventKind, bool) {
	switch source {
	case SourceProvider:
		return UsageEventObserved, true
	case SourceEstimated:
		return UsageEventEstimated, true
	case SourceReconciled, SourceAdjustment:
		return "", false
	default:
		return "", false
	}
}
