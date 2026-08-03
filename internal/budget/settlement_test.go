package budget

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

const (
	testAttemptOneID = "77000000-0000-4000-8000-000000000011"
	testAttemptTwoID = "77000000-0000-4000-8000-000000000012"
)

func TestSettlementInputOutcomeAndChargeMatrix(t *testing.T) {
	base := SettlementInput{
		TenantID: testTenantID, AccountID: testAccountID, ReservationID: testReserveID,
		RequestID: "request:settle-1", Outcome: SettlementSucceeded,
		Charges: []SettlementCharge{
			{Kind: ChargeAttempt, ReferenceID: testAttemptOneID, AmountMicros: 40},
			{Kind: ChargeAttempt, ReferenceID: testAttemptTwoID, AmountMicros: 30},
		},
		Actor: "test:settlement",
	}
	valid := []SettlementInput{
		base,
		func() SettlementInput {
			value := base
			value.Outcome = SettlementFailed
			value.Charges = nil
			return value
		}(),
		func() SettlementInput {
			value := base
			value.Outcome = SettlementCancelled
			value.Charges = nil
			return value
		}(),
		func() SettlementInput {
			value := base
			value.Outcome = SettlementCacheHit
			value.Charges = []SettlementCharge{{Kind: ChargeCache, ReferenceID: "cache:sha256:abc", AmountMicros: 5}}
			return value
		}(),
	}
	for _, input := range valid {
		if err := input.Validate(); err != nil {
			t.Errorf("SettlementInput.Validate(%s) error = %v", input.Outcome, err)
		}
	}
	actual, err := base.ActualMicros()
	if err != nil || actual != 70 {
		t.Fatalf("SettlementInput.ActualMicros() = %d/%v", actual, err)
	}

	invalid := []SettlementInput{
		{},
		func() SettlementInput { value := base; value.Outcome = "refunded"; return value }(),
		func() SettlementInput { value := base; value.Charges = nil; return value }(),
		func() SettlementInput {
			value := base
			value.Charges = append(value.Charges, value.Charges[0])
			return value
		}(),
		func() SettlementInput {
			value := base
			value.Charges = append([]SettlementCharge(nil), base.Charges...)
			value.Charges[0].ReferenceID = "bad"
			return value
		}(),
		func() SettlementInput {
			value := base
			value.Charges = []SettlementCharge{
				{Kind: ChargeAttempt, ReferenceID: testAttemptOneID, AmountMicros: MaximumAmount},
				{Kind: ChargeAttempt, ReferenceID: testAttemptTwoID, AmountMicros: 1},
			}
			return value
		}(),
		func() SettlementInput {
			value := base
			value.Outcome = SettlementCacheHit
			value.Charges = []SettlementCharge{{Kind: ChargeAttempt, ReferenceID: testAttemptOneID, AmountMicros: 1}}
			return value
		}(),
		func() SettlementInput {
			value := base
			value.Outcome = SettlementFailed
			value.Charges = []SettlementCharge{{Kind: ChargeCache, ReferenceID: "cache:1", AmountMicros: 1}}
			return value
		}(),
	}
	for index, input := range invalid {
		if err := input.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("SettlementInput.Validate(invalid %d) error = %v", index, err)
		}
		if _, err := input.ActualMicros(); !errors.Is(err, ErrInvalid) {
			t.Errorf("SettlementInput.ActualMicros(invalid %d) error = %v", index, err)
		}
	}
}

func TestSettlementResultForSettleReleaseAndOverage(t *testing.T) {
	account := validAccount()
	account.Version = 3
	account.CommittedMicros = 70
	account.ReservedMicros = 0
	reservation := terminalReservation(ReservationSettled, 70)
	entry := LedgerEntry{
		ID: 2, TenantID: testTenantID, AccountID: testAccountID, ReservationID: testReserveID,
		Kind: EntrySettle, IdempotencyKey: settlementLedgerKey(EntrySettle, testReserveID),
		CommittedDeltaMicros: 70, ReservedDeltaMicros: -100,
		ResultCommittedMicros: 70, ResultReservedMicros: 0,
		OccurredAt: account.UpdatedAt, CreatedBy: "test:settlement",
	}
	result, err := buildSettlementResult(account, reservation, entry, SettlementSucceeded, false, 2)
	if err != nil || result.Idempotent || result.Attempts != 2 || result.ActualMicros != 70 ||
		result.ReleasedMicros != 30 || result.OverageMicros != 0 || result.RemainingHardMicros != 930 {
		t.Fatalf("settled result = %+v/%v", result, err)
	}

	cancelled := terminalReservation(ReservationCancelled, 0)
	release := entry
	release.Kind = EntryRelease
	release.IdempotencyKey = settlementLedgerKey(EntryRelease, testReserveID)
	release.CommittedDeltaMicros = 0
	release.ResultCommittedMicros = 0
	result, err = buildSettlementResult(account, cancelled, release, SettlementCancelled, true, 1)
	if err != nil || !result.Idempotent || result.ActualMicros != 0 || result.ReleasedMicros != 100 {
		t.Fatalf("cancelled result = %+v/%v", result, err)
	}

	overage := terminalReservation(ReservationSettled, 120)
	entry.CommittedDeltaMicros = 120
	entry.ResultCommittedMicros = 120
	result, err = buildSettlementResult(account, overage, entry, SettlementFailed, false, 1)
	if err != nil || result.OverageMicros != 20 || result.ReleasedMicros != 0 {
		t.Fatalf("overage result = %+v/%v", result, err)
	}
	entry.ReservationID = "bad"
	if _, err := buildSettlementResult(account, overage, entry, SettlementFailed, false, 1); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("invalid settlement facts error = %v", err)
	}
}

func TestPostgresSettlerDependenciesAndHelpers(t *testing.T) {
	if _, err := NewPostgresSettler(nil, SettlementOptions{}); err == nil {
		t.Fatal("NewPostgresSettler(nil) error = nil")
	}
	if _, err := NewPostgresSettler(&sql.DB{}, SettlementOptions{MaxAttempts: -1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("NewPostgresSettler(invalid options) error = %v", err)
	}
	if _, err := NewPostgresSettler(&sql.DB{}, SettlementOptions{}); err != nil {
		t.Fatalf("NewPostgresSettler(valid) error = %v", err)
	}
	statuses := []struct {
		status  string
		outcome SettlementOutcome
		want    bool
	}{
		{"succeeded", SettlementSucceeded, true},
		{"succeeded", SettlementCacheHit, true},
		{"failed", SettlementFailed, true},
		{"partial_failed", SettlementFailed, true},
		{"cancelled", SettlementCancelled, true},
		{"running", SettlementSucceeded, false},
		{"succeeded", SettlementFailed, false},
		{"failed", "unknown", false},
	}
	for _, test := range statuses {
		if got := requestStatusMatchesOutcome(test.status, test.outcome); got != test.want {
			t.Errorf("requestStatusMatchesOutcome(%q, %q) = %t, want %t", test.status, test.outcome, got, test.want)
		}
	}
	if settlementReservationStatus(SettlementCancelled) != ReservationCancelled ||
		settlementReservationStatus(SettlementFailed) != ReservationSettled ||
		settlementEntryKind(SettlementCancelled, 0) != EntryRelease ||
		settlementEntryKind(SettlementCancelled, 1) != EntrySettle ||
		settlementLedgerKey(EntrySettle, testReserveID) != "settle:"+testReserveID {
		t.Fatal("settlement status/kind/key helpers mismatch")
	}
}

func terminalReservation(status ReservationStatus, actual uint64) Reservation {
	reservation := validReservation()
	reservation.Status = status
	released, overage := reservationDifference(reservation.ReservedMicros, actual)
	reservation.ActualMicros = &actual
	reservation.ReleasedMicros = &released
	reservation.OverageMicros = &overage
	terminalAt := reservation.CreatedAt.Add(time.Second)
	reservation.TerminalAt = &terminalAt
	reservation.Version = 2
	reservation.UpdatedAt = terminalAt
	return reservation
}
