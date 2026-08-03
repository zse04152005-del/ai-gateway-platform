package budget

import "time"

const (
	defaultReaperBatchSize = 100

	// MaximumReaperBatchSize bounds work and committed side effects per call.
	MaximumReaperBatchSize = 1_000
)

// ReaperOptions bounds one expired-reservation sweep and its account CAS retries.
type ReaperOptions struct {
	BatchSize   int
	MaxAttempts int
	RetryDelay  time.Duration
}

func (options ReaperOptions) normalized() (ReaperOptions, error) {
	if options.BatchSize == 0 {
		options.BatchSize = defaultReaperBatchSize
	}
	if options.BatchSize < 1 || options.BatchSize > MaximumReaperBatchSize {
		return ReaperOptions{}, ErrInvalid
	}
	retries, err := (ReserveOptions{
		MaxAttempts: options.MaxAttempts,
		RetryDelay:  options.RetryDelay,
	}).normalized()
	if err != nil {
		return ReaperOptions{}, err
	}
	options.MaxAttempts = retries.MaxAttempts
	options.RetryDelay = retries.RetryDelay
	return options, nil
}

// ExpirationEvent is the content-free audit/reconciliation fact persisted by
// one expire Ledger entry. EventID is stable across downstream replays.
type ExpirationEvent struct {
	EventID               string
	Reservation           Reservation
	LedgerEntry           LedgerEntry
	AccountVersion        int64
	ReleasedMicros        uint64
	ResultCommittedMicros uint64
	ResultReservedMicros  uint64
	OccurredAt            time.Time
	Attempts              int
}

// ReapResult contains independently committed expiration events. A non-nil
// error can accompany this partial result when a later item fails.
type ReapResult struct {
	Events     []ExpirationEvent
	AtCapacity bool
}

func buildExpirationEvent(
	account Account,
	reservation Reservation,
	entry LedgerEntry,
	attempts int,
) (ExpirationEvent, error) {
	if account.Validate() != nil || reservation.Validate() != nil || entry.Validate() != nil || attempts < 1 ||
		reservation.Status != ReservationExpired || reservation.ActualMicros == nil ||
		reservation.ReleasedMicros == nil || reservation.OverageMicros == nil || reservation.TerminalAt == nil ||
		*reservation.ActualMicros != 0 || *reservation.ReleasedMicros != reservation.ReservedMicros ||
		*reservation.OverageMicros != 0 || reservation.TenantID != account.Scope.TenantID ||
		reservation.AccountID != account.ID || entry.TenantID != reservation.TenantID ||
		entry.AccountID != reservation.AccountID || entry.ReservationID != reservation.ID ||
		entry.Kind != EntryExpire || entry.IdempotencyKey != expirationLedgerKey(reservation.ID) ||
		entry.CommittedDeltaMicros != 0 ||
		entry.ReservedDeltaMicros != -signedStoredAmount(reservation.ReservedMicros) ||
		entry.ResultCommittedMicros != account.CommittedMicros ||
		entry.ResultReservedMicros != account.ReservedMicros ||
		!entry.OccurredAt.Equal(*reservation.TerminalAt) || entry.OccurredAt.Before(reservation.ExpiresAt) {
		return ExpirationEvent{}, ErrUnavailable
	}
	return ExpirationEvent{
		EventID: expirationLedgerKey(reservation.ID), Reservation: reservation, LedgerEntry: entry,
		AccountVersion: account.Version, ReleasedMicros: reservation.ReservedMicros,
		ResultCommittedMicros: entry.ResultCommittedMicros,
		ResultReservedMicros:  entry.ResultReservedMicros,
		OccurredAt:            entry.OccurredAt, Attempts: attempts,
	}, nil
}

func expirationLedgerKey(reservationID string) string {
	return string(EntryExpire) + ":" + reservationID
}
