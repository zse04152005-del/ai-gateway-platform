package budget

import (
	"errors"
	"testing"
	"time"
)

const (
	testTenantID  = "77000000-0000-4000-8000-000000000001"
	testProjectID = "77000000-0000-4000-8000-000000000002"
	testKeyID     = "77000000-0000-4000-8000-000000000003"
	testAccountID = "77000000-0000-4000-8000-000000000004"
	testReserveID = "77000000-0000-4000-8000-000000000005"
)

func TestScopeValidationCoversIndependentDimensions(t *testing.T) {
	valid := []Scope{
		{Kind: ScopeTenant, TenantID: testTenantID},
		{Kind: ScopeProject, TenantID: testTenantID, ProjectID: testProjectID},
		{Kind: ScopeKey, TenantID: testTenantID, ProjectID: testProjectID, VirtualKeyID: testKeyID},
		{Kind: ScopeUser, TenantID: testTenantID, PrincipalRef: "user:hashed-123"},
		{Kind: ScopeSession, TenantID: testTenantID, SessionRef: "session:hashed-456"},
	}
	for _, scope := range valid {
		if err := scope.Validate(); err != nil {
			t.Fatalf("Scope.Validate(%s) error = %v", scope.Kind, err)
		}
	}
	invalid := []Scope{
		{},
		{Kind: ScopeTenant, TenantID: "bad"},
		{Kind: ScopeTenant, TenantID: testTenantID, ProjectID: testProjectID},
		{Kind: ScopeProject, TenantID: testTenantID},
		{Kind: ScopeProject, TenantID: testTenantID, ProjectID: testProjectID, PrincipalRef: "user"},
		{Kind: ScopeKey, TenantID: testTenantID, ProjectID: testProjectID},
		{Kind: ScopeUser, TenantID: testTenantID, PrincipalRef: "bad ref"},
		{Kind: ScopeSession, TenantID: testTenantID, SessionRef: ""},
		{Kind: "device", TenantID: testTenantID},
	}
	for index, scope := range invalid {
		if err := scope.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Scope.Validate(invalid %d) error = %v", index, err)
		}
	}
}

func TestAccountValidation(t *testing.T) {
	account := validAccount()
	if err := account.Validate(); err != nil {
		t.Fatalf("Account.Validate() error = %v", err)
	}
	closedAt := account.CreatedAt.Add(time.Minute)
	account.Status = AccountClosed
	account.ClosedAt = &closedAt
	if err := account.Validate(); err != nil {
		t.Fatalf("closed Account.Validate() error = %v", err)
	}

	mutations := []func(*Account){
		func(value *Account) { value.ID = "bad" },
		func(value *Account) { value.Scope = Scope{} },
		func(value *Account) { value.Currency = "usd" },
		func(value *Account) { value.PeriodEnd = value.PeriodStart },
		func(value *Account) { value.SoftLimitMicros = 0 },
		func(value *Account) { value.SoftLimitMicros = value.HardLimitMicros + 1 },
		func(value *Account) { value.CommittedMicros = MaximumAmount; value.ReservedMicros = 1 },
		func(value *Account) { value.Version = 0 },
		func(value *Account) { value.UpdatedAt = value.CreatedAt.Add(-time.Second) },
		func(value *Account) { value.CreatedBy = " actor" },
		func(value *Account) { value.Status = "paused" },
		func(value *Account) { value.ClosedAt = ptrTime(value.CreatedAt) },
	}
	for index, mutate := range mutations {
		selected := validAccount()
		mutate(&selected)
		if err := selected.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Account.Validate(invalid %d) error = %v", index, err)
		}
	}
}

func TestReservationValidationPendingAndTerminal(t *testing.T) {
	pending := validReservation()
	if err := pending.Validate(); err != nil {
		t.Fatalf("pending Reservation.Validate() error = %v", err)
	}
	terminalAt := pending.CreatedAt.Add(time.Second)
	actual, released, overage := uint64(60), uint64(40), uint64(0)
	settled := pending
	settled.Status = ReservationSettled
	settled.ActualMicros = &actual
	settled.ReleasedMicros = &released
	settled.OverageMicros = &overage
	settled.TerminalAt = &terminalAt
	if err := settled.Validate(); err != nil {
		t.Fatalf("settled Reservation.Validate() error = %v", err)
	}
	actual, released, overage = 120, 0, 20
	settled.Status = ReservationCancelled
	if err := settled.Validate(); err != nil {
		t.Fatalf("overage Reservation.Validate() error = %v", err)
	}
	settled.Status = ReservationExpired
	if err := settled.Validate(); err != nil {
		t.Fatalf("expired Reservation.Validate() error = %v", err)
	}

	invalid := []Reservation{
		{},
		func() Reservation { value := validReservation(); value.Status = "lost"; return value }(),
		func() Reservation { value := validReservation(); value.ActualMicros = ptrUint64(1); return value }(),
		func() Reservation {
			value := validReservation()
			value.Status = ReservationSettled
			value.TerminalAt = &terminalAt
			return value
		}(),
		func() Reservation {
			value := settled
			wrong := uint64(1)
			value.ReleasedMicros = &wrong
			return value
		}(),
		func() Reservation { value := validReservation(); value.ExpiresAt = value.CreatedAt; return value }(),
	}
	for index, reservation := range invalid {
		if err := reservation.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Reservation.Validate(invalid %d) error = %v", index, err)
		}
	}
}

func TestLedgerEntryValidation(t *testing.T) {
	base := LedgerEntry{
		ID: 1, TenantID: testTenantID, AccountID: testAccountID, ReservationID: testReserveID,
		IdempotencyKey: "ledger:1", OccurredAt: time.Now().UTC(), CreatedBy: "test:budget",
		ResultCommittedMicros: 10, ResultReservedMicros: 20,
	}
	valid := []LedgerEntry{
		withEntry(base, EntryReserve, 0, 20, testReserveID),
		withEntry(base, EntrySettle, 10, -20, testReserveID),
		withEntry(base, EntryRelease, 0, -20, testReserveID),
		withEntry(base, EntryExpire, 0, -20, testReserveID),
		withEntry(base, EntryAdjustment, -5, 0, ""),
	}
	for _, entry := range valid {
		if err := entry.Validate(); err != nil {
			t.Fatalf("LedgerEntry.Validate(%s) error = %v", entry.Kind, err)
		}
	}
	invalid := []LedgerEntry{
		{},
		withEntry(base, EntryReserve, 1, 20, testReserveID),
		withEntry(base, EntrySettle, -1, -20, testReserveID),
		withEntry(base, EntryRelease, 0, 1, testReserveID),
		withEntry(base, EntryAdjustment, 1, 0, testReserveID),
		withEntry(base, "refund", 1, 0, ""),
		withEntry(base, EntryAdjustment, 0, 0, ""),
		func() LedgerEntry {
			value := withEntry(base, EntryAdjustment, 1, 0, "")
			value.ResultCommittedMicros = MaximumAmount
			value.ResultReservedMicros = 1
			return value
		}(),
	}
	for index, entry := range invalid {
		if err := entry.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("LedgerEntry.Validate(invalid %d) error = %v", index, err)
		}
	}
}

func validAccount() Account {
	now := time.Date(2026, time.July, 31, 14, 0, 0, 0, time.UTC)
	return Account{
		ID: testAccountID, Scope: Scope{Kind: ScopeTenant, TenantID: testTenantID}, Currency: "USD",
		PeriodStart: now, PeriodEnd: now.Add(24 * time.Hour),
		SoftLimitMicros: 800, HardLimitMicros: 1_000,
		CommittedMicros: 100, ReservedMicros: 50, Status: AccountOpen, Version: 1,
		CreatedAt: now, CreatedBy: "test:budget", UpdatedAt: now, UpdatedBy: "test:budget",
	}
}

func validReservation() Reservation {
	now := time.Date(2026, time.July, 31, 14, 0, 0, 0, time.UTC)
	return Reservation{
		ID: testReserveID, TenantID: testTenantID, AccountID: testAccountID,
		RequestID: "request:budget-1", IdempotencyKey: "reserve:budget-1",
		Status: ReservationPending, ReservedMicros: 100, ExpiresAt: now.Add(time.Minute),
		Version: 1, CreatedAt: now, CreatedBy: "test:budget", UpdatedAt: now, UpdatedBy: "test:budget",
	}
}

func withEntry(base LedgerEntry, kind EntryKind, committed, reserved int64, reservationID string) LedgerEntry {
	base.Kind = kind
	base.CommittedDeltaMicros = committed
	base.ReservedDeltaMicros = reserved
	base.ReservationID = reservationID
	return base
}

func ptrUint64(value uint64) *uint64     { return &value }
func ptrTime(value time.Time) *time.Time { return &value }
