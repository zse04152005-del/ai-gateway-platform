package budget

import (
	"errors"
	"time"
)

const (
	defaultReserveMaxAttempts = 128
	defaultReserveRetryDelay  = 250 * time.Microsecond

	// MaximumReserveAttempts is the hard process bound for one CAS operation.
	MaximumReserveAttempts = 256
)

var (
	// ErrBudgetExceeded means the requested hold would cross the account hard limit.
	ErrBudgetExceeded = errors.New("budget hard limit exceeded")
	// ErrAccountNotFound means no account exists in the trusted tenant scope.
	ErrAccountNotFound = errors.New("budget account not found")
	// ErrAccountInactive means the account is closed or outside its active period.
	ErrAccountInactive = errors.New("budget account is not active")
	// ErrIdempotencyConflict means a key was reused with different reservation facts.
	ErrIdempotencyConflict = errors.New("budget reservation idempotency conflict")
	// ErrRetryExhausted means every bounded account-version CAS attempt conflicted.
	ErrRetryExhausted = errors.New("budget reservation retry limit exhausted")
	// ErrConflict means a referenced request or stored transition conflicts.
	ErrConflict = errors.New("budget reservation conflict")
	// ErrUnavailable means reservation facts could not be safely persisted or decoded.
	ErrUnavailable = errors.New("budget reservation unavailable")
)

// ReserveInput is one authenticated request/account hold command.
type ReserveInput struct {
	TenantID        string
	AccountID       string
	RequestID       string
	IdempotencyKey  string
	AmountMicros    uint64
	ExpiresAt       time.Time
	Actor           string
	DegradationHint DegradationHint
}

// Validate checks stable command shape without consulting mutable account state.
func (input ReserveInput) Validate() error {
	if !budgetUUIDPattern.MatchString(input.TenantID) || !budgetUUIDPattern.MatchString(input.AccountID) ||
		!budgetRefPattern.MatchString(input.RequestID) || !budgetRefPattern.MatchString(input.IdempotencyKey) ||
		!validAmount(input.AmountMicros) || input.ExpiresAt.IsZero() || !validActor(input.Actor) ||
		!validDegradationHint(input.DegradationHint) {
		return ErrInvalid
	}
	return nil
}

// ReserveOptions bounds optimistic retries and their context-aware delay.
type ReserveOptions struct {
	MaxAttempts int
	RetryDelay  time.Duration
}

func (options ReserveOptions) normalized() (ReserveOptions, error) {
	if options.MaxAttempts == 0 {
		options.MaxAttempts = defaultReserveMaxAttempts
	}
	if options.RetryDelay == 0 {
		options.RetryDelay = defaultReserveRetryDelay
	}
	if options.MaxAttempts < 1 || options.MaxAttempts > MaximumReserveAttempts ||
		options.RetryDelay < 0 || options.RetryDelay > time.Second {
		return ReserveOptions{}, ErrInvalid
	}
	return options, nil
}

// ReserveResult is the stable admission fact returned after one committed hold.
type ReserveResult struct {
	Reservation           Reservation
	LedgerEntry           LedgerEntry
	AccountVersion        int64
	ResultCommittedMicros uint64
	ResultReservedMicros  uint64
	RemainingHardMicros   uint64
	SoftLimitExceeded     bool
	LimitNotice           *LimitNotice
	Idempotent            bool
	Attempts              int
}

func buildReserveResult(
	account Account,
	reservation Reservation,
	entry LedgerEntry,
	hint DegradationHint,
	idempotent bool,
	attempts int,
) (ReserveResult, error) {
	if account.Validate() != nil || reservation.Validate() != nil || entry.Validate() != nil ||
		reservation.TenantID != account.Scope.TenantID || reservation.AccountID != account.ID ||
		entry.TenantID != account.Scope.TenantID || entry.AccountID != account.ID ||
		entry.ReservationID != reservation.ID || entry.Kind != EntryReserve || !validDegradationHint(hint) ||
		entry.IdempotencyKey != reservation.IdempotencyKey || attempts < 1 {
		return ReserveResult{}, ErrUnavailable
	}
	spent := entry.ResultCommittedMicros + entry.ResultReservedMicros
	remaining := uint64(0)
	if spent < account.HardLimitMicros {
		remaining = account.HardLimitMicros - spent
	}
	softLimitExceeded := spent > account.SoftLimitMicros
	var notice *LimitNotice
	if softLimitExceeded {
		value := newBudgetLimitNotice(LimitSoft, account, spent, hint)
		if value.Validate() != nil {
			return ReserveResult{}, ErrUnavailable
		}
		notice = &value
	}
	return ReserveResult{
		Reservation: reservation, LedgerEntry: entry, AccountVersion: account.Version,
		ResultCommittedMicros: entry.ResultCommittedMicros,
		ResultReservedMicros:  entry.ResultReservedMicros,
		RemainingHardMicros:   remaining, SoftLimitExceeded: softLimitExceeded, LimitNotice: notice,
		Idempotent: idempotent, Attempts: attempts,
	}, nil
}
