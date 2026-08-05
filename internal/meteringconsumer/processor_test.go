package meteringconsumer

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/metering"
)

const testEstimationAlgorithm = "utf8-byte-bound"

func TestCanonicalUsageEventRejectsInvalidRecordsAndNormalizesJSON(t *testing.T) {
	event := validConsumerEvent()
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	decoded, fingerprint, err := canonicalUsageEvent(event.EventID, payload)
	if err != nil || decoded.EventID != event.EventID || decoded.BillingUnit != metering.BillingUnitToken ||
		fingerprint == [32]byte{} {
		t.Fatalf("canonicalUsageEvent(valid) = %+v/%x/%v", decoded, fingerprint, err)
	}
	pretty, err := json.MarshalIndent(event, "", "  ")
	if err != nil {
		t.Fatalf("json.MarshalIndent() error = %v", err)
	}
	_, prettyFingerprint, err := canonicalUsageEvent(event.EventID, pretty)
	if err != nil || prettyFingerprint != fingerprint {
		t.Fatalf("canonical pretty fingerprint = %x/%v, want %x", prettyFingerprint, err, fingerprint)
	}
	var legacyFields map[string]any
	if err := json.Unmarshal(payload, &legacyFields); err != nil {
		t.Fatalf("json.Unmarshal(legacy fields) error = %v", err)
	}
	delete(legacyFields, "billing_unit")
	legacyPayload, err := json.Marshal(legacyFields)
	if err != nil {
		t.Fatalf("json.Marshal(legacy fields) error = %v", err)
	}
	_, legacyFingerprint, err := canonicalUsageEvent(event.EventID, legacyPayload)
	if err != nil || legacyFingerprint != fingerprint {
		t.Fatalf("legacy unit fingerprint = %x/%v, want %x", legacyFingerprint, err, fingerprint)
	}

	unknown := append([]byte(nil), payload[:len(payload)-1]...)
	unknown = append(unknown, []byte(`,"prompt":"secret"}`)...)
	invalid := []struct {
		key     string
		payload []byte
	}{
		{key: "", payload: payload},
		{key: "7e000000-0000-4000-8000-000000000099", payload: payload},
		{key: event.EventID, payload: nil},
		{key: event.EventID, payload: unknown},
		{key: event.EventID, payload: append(append([]byte(nil), payload...), []byte(` {}`)...)},
		{key: event.EventID, payload: []byte(`{`)},
		{key: event.EventID, payload: []byte(strings.Repeat("x", maximumUsageEventBytes+1))},
	}
	for index, record := range invalid {
		if _, _, err := canonicalUsageEvent(record.key, record.payload); !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("invalid[%d] error = %v", index, err)
		}
	}
}

func TestCanonicalUsageEventAcceptsVersionTwoEstimateEvidence(t *testing.T) {
	event := validConsumerEvent()
	event.SchemaVersion = metering.UsageEventSchemaVersion
	event.UsageComplete = false
	event.Estimate = &adapter.UsageEstimateMetadata{
		Estimated: true, Tokenizer: testEstimationAlgorithm, TokenizerVersion: "v1",
		PhysicalModel: "model-fixture", DeploymentVersion: 4,
		ProviderProtocolVersion: "protocol-v1",
	}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	decoded, _, err := canonicalUsageEvent(event.EventID, payload)
	if err != nil || decoded.Estimate == nil || !decoded.Estimate.Estimated ||
		decoded.Estimate.Tokenizer != "utf8-byte-bound" || decoded.SchemaVersion != metering.UsageEventSchemaVersion {
		t.Fatalf("canonicalUsageEvent(v2) = %+v, %v", decoded, err)
	}
	event.Estimate = nil
	payload, err = json.Marshal(event)
	if err != nil {
		t.Fatalf("json.Marshal(missing evidence) error = %v", err)
	}
	if _, _, err := canonicalUsageEvent(event.EventID, payload); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("canonicalUsageEvent(missing v2 evidence) error = %v", err)
	}
}

func TestProcessorConstructorAndSafeErrors(t *testing.T) {
	if _, err := NewProcessor(nil, "ai-gateway-metering-v1", time.Now); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("NewProcessor(nil) error = %v", err)
	}
	private := errors.New("private database address")
	failure := newConsumerError(ErrStoreUnavailable, private)
	if failure.Error() != ErrStoreUnavailable.Error() || strings.Contains(failure.Error(), "private") ||
		!errors.Is(failure, ErrStoreUnavailable) || !errors.Is(failure, private) {
		t.Fatalf("consumer error safety = %q/%v", failure, failure)
	}
	var nilFailure *consumerError
	if nilFailure.Error() != "metering consumer failed" || nilFailure.Unwrap() != nil {
		t.Fatalf("nil consumer error = %q/%#v", nilFailure.Error(), nilFailure.Unwrap())
	}
}

func validConsumerEvent() metering.UsageEvent {
	return metering.UsageEvent{
		EventID: "7e000000-0000-4000-8000-000000000001", SchemaVersion: 1,
		Kind:         metering.UsageEventEstimated,
		TenantID:     "7e000000-0000-4000-8000-000000000002",
		RequestID:    "integration-metering-consumer-event",
		AttemptID:    "7e000000-0000-4000-8000-000000000003",
		DeploymentID: "7e000000-0000-4000-8000-000000000004",
		TokenType:    metering.TokenTypeInput, Quantity: 13, Source: metering.SourceEstimated,
		UsageComplete: true, ObservedAt: time.Date(2026, time.August, 4, 1, 2, 3, 0, time.UTC),
		TraceID: "7e000000000000000000000000000001", SpanID: "7e00000000000001",
	}
}
