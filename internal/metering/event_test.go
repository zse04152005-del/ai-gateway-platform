package metering_test

import (
	"errors"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/metering"
)

func TestNewUsageEventsPreservesIndependentPositiveFacts(t *testing.T) {
	identity := validUsageIdentity()
	usage := adapter.NormalizedUsage{
		InputTokens: adapter.Tokens(13), OutputTokens: adapter.Tokens(3),
		CacheReadTokens: adapter.Tokens(5), CacheWriteTokens: adapter.Tokens(0),
		Source: adapter.UsageSourceEstimated, Complete: true,
	}
	ids := []string{
		"7c000000-0000-4000-8000-000000000001",
		"7c000000-0000-4000-8000-000000000002",
		"7c000000-0000-4000-8000-000000000003",
	}
	index := 0
	events, err := metering.NewUsageEvents(identity, &usage, func() (string, error) {
		id := ids[index]
		index++
		return id, nil
	})
	if err != nil {
		t.Fatalf("NewUsageEvents() error = %v", err)
	}
	if len(events) != 3 || index != 3 {
		t.Fatalf("event count/id allocations = %d/%d, want 3/3", len(events), index)
	}
	wantTypes := []metering.TokenType{
		metering.TokenTypeInput, metering.TokenTypeOutput, metering.TokenTypeCacheRead,
	}
	wantQuantities := []int64{13, 3, 5}
	for eventIndex, event := range events {
		if event.EventID != ids[eventIndex] || event.Kind != metering.UsageEventEstimated ||
			event.TokenType != wantTypes[eventIndex] || event.Quantity != wantQuantities[eventIndex] ||
			event.Source != metering.SourceEstimated || !event.UsageComplete ||
			event.ObservedAt.Location() != time.UTC || event.Validate() != nil {
			t.Fatalf("events[%d] = %+v", eventIndex, event)
		}
	}
	usage.CacheReadTokens = adapter.TokenCount{}
	usage.InputTokens = adapter.Tokens(0)
	usage.OutputTokens = adapter.Tokens(0)
	if events, err := metering.NewUsageEvents(identity, &usage, func() (string, error) {
		t.Fatal("zero-only usage allocated an event id")
		return "", nil
	}); err != nil || len(events) != 0 {
		t.Fatalf("zero-only events = %+v, %v", events, err)
	}
	if events, err := metering.NewUsageEvents(metering.UsageIdentity{}, nil, nil); err != nil || events != nil {
		t.Fatalf("nil usage events = %+v, %v", events, err)
	}
}

func TestNewUsageEventsRejectsUnsafeIdentitySourceQuantityAndIDs(t *testing.T) {
	identity := validUsageIdentity()
	usage := adapter.NormalizedUsage{InputTokens: adapter.Tokens(1), Source: adapter.UsageSourceEstimated}
	invalidIdentities := []metering.UsageIdentity{
		{},
		mutateUsageIdentity(identity, func(value *metering.UsageIdentity) { value.RequestID = "short" }),
		mutateUsageIdentity(identity, func(value *metering.UsageIdentity) { value.TraceID = "private-trace" }),
		mutateUsageIdentity(identity, func(value *metering.UsageIdentity) { value.ObservedAt = time.Time{} }),
	}
	for index, invalid := range invalidIdentities {
		if _, err := metering.NewUsageEvents(invalid, &usage, validEventIDFactory()); !errors.Is(err, metering.ErrInvalidUsageEvent) {
			t.Errorf("invalid identity[%d] error = %v", index, err)
		}
	}
	for _, source := range []adapter.UsageSource{
		adapter.UsageSourceReconciled, adapter.UsageSourceAdjustment, "vendor",
	} {
		invalid := usage
		invalid.Source = source
		if _, err := metering.NewUsageEvents(identity, &invalid, validEventIDFactory()); !errors.Is(err, metering.ErrInvalidUsageEvent) {
			t.Errorf("source %q error = %v", source, err)
		}
	}
	tooLarge := usage
	tooLarge.InputTokens = adapter.Tokens(metering.MaximumExactInteger + 1)
	if _, err := metering.NewUsageEvents(identity, &tooLarge, validEventIDFactory()); !errors.Is(err, metering.ErrInvalidUsageEvent) {
		t.Fatalf("too-large quantity error = %v", err)
	}
	if _, err := metering.NewUsageEvents(identity, &usage, nil); !errors.Is(err, metering.ErrInvalidUsageEvent) {
		t.Fatalf("nil ID factory error = %v", err)
	}
	if _, err := metering.NewUsageEvents(identity, &usage, func() (string, error) {
		return "", errors.New("private random failure")
	}); !errors.Is(err, metering.ErrUsageEventID) {
		t.Fatalf("ID generation error = %v", err)
	}
	if _, err := metering.NewUsageEvents(identity, &usage, func() (string, error) {
		return "bad-id", nil
	}); !errors.Is(err, metering.ErrInvalidUsageEvent) {
		t.Fatalf("invalid generated ID error = %v", err)
	}
}

func TestUsageEventValidationRequiresCompatibleKindAndCompleteContract(t *testing.T) {
	events, err := metering.NewUsageEvents(
		validUsageIdentity(),
		&adapter.NormalizedUsage{InputTokens: adapter.Tokens(1), Source: adapter.UsageSourceEstimated},
		validEventIDFactory(),
	)
	if err != nil || len(events) != 1 {
		t.Fatalf("valid event setup = %+v, %v", events, err)
	}
	valid := events[0]
	invalid := []metering.UsageEvent{
		{},
		mutateUsageEvent(valid, func(value *metering.UsageEvent) { value.SchemaVersion = 2 }),
		mutateUsageEvent(valid, func(value *metering.UsageEvent) { value.Kind = metering.UsageEventObserved }),
		mutateUsageEvent(valid, func(value *metering.UsageEvent) { value.TokenType = "total" }),
		mutateUsageEvent(valid, func(value *metering.UsageEvent) { value.Quantity = 0 }),
		mutateUsageEvent(valid, func(value *metering.UsageEvent) { value.Source = metering.SourceReconciled }),
	}
	for index, event := range invalid {
		if err := event.Validate(); !errors.Is(err, metering.ErrInvalidUsageEvent) {
			t.Errorf("invalid event[%d] error = %v", index, err)
		}
	}
}

func validUsageIdentity() metering.UsageIdentity {
	return metering.UsageIdentity{
		TenantID:     "7c000000-0000-4000-8000-000000000101",
		RequestID:    "usage-event-request-0001",
		AttemptID:    "7c000000-0000-4000-8000-000000000102",
		DeploymentID: "7c000000-0000-4000-8000-000000000103",
		TraceID:      "7c000000000000000000000000000001",
		SpanID:       "7c00000000000001",
		ObservedAt:   time.Date(2026, time.August, 4, 1, 2, 3, 456000000, time.FixedZone("test", 8*60*60)),
	}
}

func validEventIDFactory() metering.EventIDFactory {
	return func() (string, error) {
		return "7c000000-0000-4000-8000-000000000201", nil
	}
}

func mutateUsageIdentity(
	input metering.UsageIdentity,
	mutate func(*metering.UsageIdentity),
) metering.UsageIdentity {
	mutate(&input)
	return input
}

func mutateUsageEvent(input metering.UsageEvent, mutate func(*metering.UsageEvent)) metering.UsageEvent {
	mutate(&input)
	return input
}
