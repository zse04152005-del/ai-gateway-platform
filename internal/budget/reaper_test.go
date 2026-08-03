package budget

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestReaperOptionsValidation(t *testing.T) {
	defaults, err := (ReaperOptions{}).normalized()
	if err != nil || defaults.BatchSize != defaultReaperBatchSize ||
		defaults.MaxAttempts != defaultReserveMaxAttempts || defaults.RetryDelay != defaultReserveRetryDelay {
		t.Fatalf("default ReaperOptions = %+v/%v", defaults, err)
	}
	custom, err := (ReaperOptions{BatchSize: 7, MaxAttempts: 3, RetryDelay: time.Millisecond}).normalized()
	if err != nil || custom.BatchSize != 7 || custom.MaxAttempts != 3 || custom.RetryDelay != time.Millisecond {
		t.Fatalf("custom ReaperOptions = %+v/%v", custom, err)
	}
	for _, options := range []ReaperOptions{
		{BatchSize: -1}, {BatchSize: MaximumReaperBatchSize + 1},
		{MaxAttempts: -1}, {RetryDelay: -1},
	} {
		if _, err := options.normalized(); !errors.Is(err, ErrInvalid) {
			t.Errorf("ReaperOptions.normalized(%+v) error = %v", options, err)
		}
	}
}

func TestExpirationEventBuildsPersistentAuditFact(t *testing.T) {
	reservation := validReservation()
	actual, released, overage := uint64(0), reservation.ReservedMicros, uint64(0)
	terminalAt := reservation.ExpiresAt.Add(time.Second)
	reservation.Status = ReservationExpired
	reservation.ActualMicros = &actual
	reservation.ReleasedMicros = &released
	reservation.OverageMicros = &overage
	reservation.TerminalAt = &terminalAt
	reservation.Version = 2
	reservation.UpdatedAt = terminalAt
	reservation.UpdatedBy = "test:reaper"

	account := validAccount()
	account.ReservedMicros = 0
	account.Version = 2
	account.UpdatedAt = terminalAt
	account.UpdatedBy = "test:reaper"
	entry := LedgerEntry{
		ID: 2, TenantID: testTenantID, AccountID: testAccountID, ReservationID: testReserveID,
		Kind: EntryExpire, IdempotencyKey: expirationLedgerKey(testReserveID),
		CommittedDeltaMicros: 0, ReservedDeltaMicros: -100,
		ResultCommittedMicros: account.CommittedMicros, ResultReservedMicros: 0,
		OccurredAt: terminalAt, CreatedBy: "test:reaper",
	}
	event, err := buildExpirationEvent(account, reservation, entry, 3)
	if err != nil || event.EventID != "expire:"+testReserveID || event.Attempts != 3 ||
		event.ReleasedMicros != 100 || event.AccountVersion != 2 ||
		event.ResultCommittedMicros != account.CommittedMicros || event.ResultReservedMicros != 0 ||
		!event.OccurredAt.Equal(terminalAt) {
		t.Fatalf("buildExpirationEvent() = %+v/%v", event, err)
	}

	invalidEntry := entry
	invalidEntry.Kind = EntryRelease
	if _, err := buildExpirationEvent(account, reservation, invalidEntry, 1); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("wrong entry kind error = %v", err)
	}
	invalidEntry = entry
	invalidEntry.OccurredAt = reservation.ExpiresAt.Add(-time.Second)
	if _, err := buildExpirationEvent(account, reservation, invalidEntry, 1); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("early expiration event error = %v", err)
	}
	if _, err := buildExpirationEvent(account, reservation, entry, 0); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("zero attempts error = %v", err)
	}
}

func TestPostgresReaperDependenciesAndSafeValidation(t *testing.T) {
	if _, err := NewPostgresReaper(nil, ReaperOptions{}); err == nil {
		t.Fatal("NewPostgresReaper(nil) error = nil")
	}
	if _, err := NewPostgresReaper(&sql.DB{}, ReaperOptions{BatchSize: -1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("NewPostgresReaper(invalid options) error = %v", err)
	}
	reaper, err := NewPostgresReaper(&sql.DB{}, ReaperOptions{})
	if err != nil {
		t.Fatalf("NewPostgresReaper(valid) error = %v", err)
	}
	if _, err := reaper.Reap(nil, "test:reaper"); !errors.Is(err, ErrInvalid) { //nolint:staticcheck // explicit nil boundary
		t.Fatalf("Reap(nil context) error = %v", err)
	}
	if _, err := reaper.Reap(context.Background(), " reaper"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Reap(invalid actor) error = %v", err)
	}
	var nilReaper *PostgresReaper
	if _, err := nilReaper.Reap(context.Background(), "test:reaper"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil Reaper.Reap() error = %v", err)
	}
	if expirationLedgerKey(testReserveID) != "expire:"+testReserveID {
		t.Fatal("expiration ledger key mismatch")
	}
}
