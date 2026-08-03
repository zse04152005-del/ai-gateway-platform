package budget

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
)

func TestReserveInputAndOptionsValidation(t *testing.T) {
	now := time.Date(2026, time.August, 3, 1, 2, 3, 0, time.UTC)
	valid := ReserveInput{
		TenantID: testTenantID, AccountID: testAccountID, RequestID: "request:reserve-1",
		IdempotencyKey: "reserve:1", AmountMicros: 10, ExpiresAt: now.Add(time.Minute), Actor: "test:reserve",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("ReserveInput.Validate() error = %v", err)
	}
	invalid := []ReserveInput{
		{},
		func() ReserveInput { value := valid; value.TenantID = "bad"; return value }(),
		func() ReserveInput { value := valid; value.AccountID = "bad"; return value }(),
		func() ReserveInput { value := valid; value.RequestID = "bad request"; return value }(),
		func() ReserveInput { value := valid; value.IdempotencyKey = ""; return value }(),
		func() ReserveInput { value := valid; value.AmountMicros = 0; return value }(),
		func() ReserveInput { value := valid; value.AmountMicros = MaximumAmount + 1; return value }(),
		func() ReserveInput { value := valid; value.ExpiresAt = time.Time{}; return value }(),
		func() ReserveInput { value := valid; value.Actor = " actor"; return value }(),
		func() ReserveInput { value := valid; value.DegradationHint = "switch_tenant"; return value }(),
	}
	for index, input := range invalid {
		if err := input.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("ReserveInput.Validate(invalid %d) error = %v", index, err)
		}
	}

	defaults, err := (ReserveOptions{}).normalized()
	if err != nil || defaults.MaxAttempts != defaultReserveMaxAttempts || defaults.RetryDelay != defaultReserveRetryDelay {
		t.Fatalf("default options = %+v/%v", defaults, err)
	}
	custom, err := (ReserveOptions{MaxAttempts: 3, RetryDelay: time.Millisecond}).normalized()
	if err != nil || custom.MaxAttempts != 3 || custom.RetryDelay != time.Millisecond {
		t.Fatalf("custom options = %+v/%v", custom, err)
	}
	for _, options := range []ReserveOptions{
		{MaxAttempts: -1}, {MaxAttempts: MaximumReserveAttempts + 1}, {RetryDelay: -1}, {RetryDelay: time.Second + 1},
	} {
		if _, err := options.normalized(); !errors.Is(err, ErrInvalid) {
			t.Errorf("ReserveOptions.normalized(%+v) error = %v", options, err)
		}
	}
}

func TestReserveResultUsesOriginalLedgerBalances(t *testing.T) {
	account := validAccount()
	account.Version = 7
	account.ReservedMicros = 700
	reservation := validReservation()
	reservation.ReservedMicros = 100
	entry := LedgerEntry{
		ID: 1, TenantID: testTenantID, AccountID: testAccountID, ReservationID: testReserveID,
		Kind: EntryReserve, IdempotencyKey: reservation.IdempotencyKey,
		ReservedDeltaMicros: 100, ResultCommittedMicros: 100, ResultReservedMicros: 800,
		OccurredAt: account.CreatedAt, CreatedBy: "test:budget",
	}
	result, err := buildReserveResult(account, reservation, entry, DegradeLowerCostModel, true, 3)
	if err != nil {
		t.Fatalf("buildReserveResult() error = %v", err)
	}
	if !result.Idempotent || result.Attempts != 3 || result.AccountVersion != 7 ||
		result.ResultCommittedMicros != 100 || result.ResultReservedMicros != 800 ||
		result.RemainingHardMicros != 100 || !result.SoftLimitExceeded || result.LimitNotice == nil ||
		result.LimitNotice.Level != LimitSoft || result.LimitNotice.RemainingMicros != 100 ||
		result.LimitNotice.DegradationHint != DegradeLowerCostModel {
		t.Fatalf("reserve result = %+v", result)
	}
	entry.ResultCommittedMicros = 0
	entry.ResultReservedMicros = 800
	result, err = buildReserveResult(account, reservation, entry, "", false, 1)
	if err != nil || result.SoftLimitExceeded || result.LimitNotice != nil {
		t.Fatalf("at soft boundary result = %+v/%v", result, err)
	}
	entry.ReservationID = "bad"
	if _, err := buildReserveResult(account, reservation, entry, "", false, 1); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("invalid facts error = %v", err)
	}
}

func TestPostgresReserverDependenciesUUIDRetryAndPrivateErrors(t *testing.T) {
	now := time.Date(2026, time.August, 3, 1, 2, 3, 0, time.UTC)
	random := bytes.NewReader(make([]byte, 16))
	if _, err := NewPostgresReserver(nil, func() time.Time { return now }, random, ReserveOptions{}); err == nil {
		t.Fatal("NewPostgresReserver(nil database) error = nil")
	}
	database := &sql.DB{}
	if _, err := NewPostgresReserver(database, nil, random, ReserveOptions{}); err == nil {
		t.Fatal("NewPostgresReserver(nil clock) error = nil")
	}
	if _, err := NewPostgresReserver(database, func() time.Time { return time.Time{} }, random, ReserveOptions{}); err == nil {
		t.Fatal("NewPostgresReserver(zero clock) error = nil")
	}
	if _, err := NewPostgresReserver(database, func() time.Time { return now }, nil, ReserveOptions{}); err == nil {
		t.Fatal("NewPostgresReserver(nil random) error = nil")
	}
	if _, err := NewPostgresReserver(database, func() time.Time { return now }, random, ReserveOptions{MaxAttempts: -1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("NewPostgresReserver(invalid options) error = %v", err)
	}

	identifier, err := newBudgetUUID(bytes.NewReader(make([]byte, 16)))
	if err != nil || identifier != "00000000-0000-4000-8000-000000000000" ||
		!budgetUUIDPattern.MatchString(identifier) {
		t.Fatalf("newBudgetUUID() = %q/%v", identifier, err)
	}
	if _, err := newBudgetUUID(bytes.NewReader(nil)); err == nil {
		t.Fatal("newBudgetUUID(short random) error = nil")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitForReserveRetry(cancelled, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForReserveRetry(cancelled) error = %v", err)
	}
	if err := waitForReserveRetry(context.Background(), 0); err != nil {
		t.Fatalf("waitForReserveRetry(0) error = %v", err)
	}

	private := errors.New("postgres://private-user:private-password@private-host/internal")
	failure := newReserveError(ErrUnavailable, private)
	if failure.Error() != ErrUnavailable.Error() || strings.Contains(failure.Error(), "private") ||
		!errors.Is(failure, ErrUnavailable) || !errors.Is(failure, private) {
		t.Fatalf("reserve error safety/unwrap = %q/%v", failure, failure)
	}
	var nilFailure *reserveError
	if nilFailure.Error() != "budget reservation failed" || nilFailure.Unwrap() != nil {
		t.Fatalf("nil reserveError = %q/%#v", nilFailure.Error(), nilFailure.Unwrap())
	}
	for _, databaseError := range []*pq.Error{
		{Code: "23503", Message: "private fk"},
		{Code: "23505", Message: "private unique"},
		{Code: "23514", Message: "private check"},
	} {
		mapped := mapReserveDatabaseError(databaseError)
		if !errors.Is(mapped, ErrConflict) || strings.Contains(mapped.Error(), "private") {
			t.Errorf("mapReserveDatabaseError(%s) = %q", databaseError.Code, mapped)
		}
	}
	if !isRetryableReserveDatabaseError(&pq.Error{Code: "40001"}) ||
		!isRetryableReserveDatabaseError(&pq.Error{Code: "40P01"}) ||
		isRetryableReserveDatabaseError(&pq.Error{Code: "23514"}) {
		t.Fatal("retryable database error classification mismatch")
	}
	if !isIdempotencyUniqueViolation(&pq.Error{
		Code: "23505", Constraint: "budget_reservations_account_idempotency_unique",
	}) || isIdempotencyUniqueViolation(&pq.Error{Code: "23505", Constraint: "budget_reservations_pkey"}) {
		t.Fatal("idempotency unique classification mismatch")
	}
}
