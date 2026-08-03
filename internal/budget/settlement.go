package budget

import "errors"

var (
	// ErrReservationNotFound means no reservation exists in the trusted account scope.
	ErrReservationNotFound = errors.New("budget reservation not found")
	// ErrSettlementConflict means terminal facts or request outcome disagree with the command.
	ErrSettlementConflict = errors.New("budget settlement conflict")
)

// SettlementOutcome describes why a request reached its billable terminal state.
type SettlementOutcome string

const (
	// SettlementSucceeded commits all priced attempt charges for a successful request.
	SettlementSucceeded SettlementOutcome = "succeeded"
	// SettlementFailed commits any priced work performed before a failed request.
	SettlementFailed SettlementOutcome = "failed"
	// SettlementCancelled commits observed work or releases the whole hold when zero.
	SettlementCancelled SettlementOutcome = "cancelled"
	// SettlementCacheHit commits one explicitly priced cache result without attempts.
	SettlementCacheHit SettlementOutcome = "cache_hit"
)

// ChargeKind identifies the mutually exclusive source of one priced component.
type ChargeKind string

const (
	// ChargeAttempt is one independently priced physical provider Attempt.
	ChargeAttempt ChargeKind = "attempt"
	// ChargeCache is one explicitly priced cache-hit result.
	ChargeCache ChargeKind = "cache"
)

// SettlementCharge is one content-free, already-priced cost fact.
type SettlementCharge struct {
	Kind         ChargeKind
	ReferenceID  string
	AmountMicros uint64
}

// SettlementInput reconciles exactly one pending Reservation.
type SettlementInput struct {
	TenantID      string
	AccountID     string
	ReservationID string
	RequestID     string
	Outcome       SettlementOutcome
	Charges       []SettlementCharge
	Actor         string
}

// Validate rejects duplicate sources, ambiguous cache/attempt mixes and overflow.
func (input SettlementInput) Validate() error {
	if !budgetUUIDPattern.MatchString(input.TenantID) || !budgetUUIDPattern.MatchString(input.AccountID) ||
		!budgetUUIDPattern.MatchString(input.ReservationID) || !budgetRefPattern.MatchString(input.RequestID) ||
		!validActor(input.Actor) {
		return ErrInvalid
	}
	seen := make(map[string]struct{}, len(input.Charges))
	actual := uint64(0)
	for _, charge := range input.Charges {
		if charge.AmountMicros > MaximumAmount ||
			(charge.Kind == ChargeAttempt && !budgetUUIDPattern.MatchString(charge.ReferenceID)) ||
			(charge.Kind == ChargeCache && !budgetRefPattern.MatchString(charge.ReferenceID)) ||
			(charge.Kind != ChargeAttempt && charge.Kind != ChargeCache) {
			return ErrInvalid
		}
		identity := string(charge.Kind) + ":" + charge.ReferenceID
		if _, exists := seen[identity]; exists || actual > MaximumAmount-charge.AmountMicros {
			return ErrInvalid
		}
		seen[identity] = struct{}{}
		actual += charge.AmountMicros
	}
	switch input.Outcome {
	case SettlementSucceeded:
		if len(input.Charges) == 0 || !allChargesAre(input.Charges, ChargeAttempt) {
			return ErrInvalid
		}
	case SettlementFailed, SettlementCancelled:
		if !allChargesAre(input.Charges, ChargeAttempt) {
			return ErrInvalid
		}
	case SettlementCacheHit:
		if len(input.Charges) != 1 || input.Charges[0].Kind != ChargeCache {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

// ActualMicros returns the checked sum of all independently priced components.
func (input SettlementInput) ActualMicros() (uint64, error) {
	if input.Validate() != nil {
		return 0, ErrInvalid
	}
	actual := uint64(0)
	for _, charge := range input.Charges {
		actual += charge.AmountMicros
	}
	return actual, nil
}

// SettlementOptions shares the bounded CAS controls used by reservation.
type SettlementOptions = ReserveOptions

// SettlementResult is the stable terminal reconciliation fact.
type SettlementResult struct {
	Reservation           Reservation
	LedgerEntry           LedgerEntry
	Outcome               SettlementOutcome
	AccountVersion        int64
	ActualMicros          uint64
	ReleasedMicros        uint64
	OverageMicros         uint64
	ResultCommittedMicros uint64
	ResultReservedMicros  uint64
	RemainingHardMicros   uint64
	SoftLimitExceeded     bool
	Idempotent            bool
	Attempts              int
}

func buildSettlementResult(
	account Account,
	reservation Reservation,
	entry LedgerEntry,
	outcome SettlementOutcome,
	idempotent bool,
	attempts int,
) (SettlementResult, error) {
	if account.Validate() != nil || reservation.Validate() != nil || entry.Validate() != nil || attempts < 1 ||
		reservation.ActualMicros == nil || reservation.ReleasedMicros == nil || reservation.OverageMicros == nil ||
		reservation.TenantID != account.Scope.TenantID || reservation.AccountID != account.ID ||
		entry.TenantID != account.Scope.TenantID || entry.AccountID != account.ID ||
		entry.ReservationID != reservation.ID {
		return SettlementResult{}, ErrUnavailable
	}
	expectedStatus := settlementReservationStatus(outcome)
	expectedKind := settlementEntryKind(outcome, *reservation.ActualMicros)
	committedDelta, ok := signedExactAmount(*reservation.ActualMicros)
	if !ok || reservation.Status != expectedStatus || entry.Kind != expectedKind ||
		entry.IdempotencyKey != settlementLedgerKey(expectedKind, reservation.ID) ||
		entry.CommittedDeltaMicros != committedDelta ||
		entry.ReservedDeltaMicros != -signedStoredAmount(reservation.ReservedMicros) {
		return SettlementResult{}, ErrUnavailable
	}
	spent := entry.ResultCommittedMicros + entry.ResultReservedMicros
	remaining := uint64(0)
	if spent < account.HardLimitMicros {
		remaining = account.HardLimitMicros - spent
	}
	return SettlementResult{
		Reservation: reservation, LedgerEntry: entry, Outcome: outcome, AccountVersion: account.Version,
		ActualMicros: *reservation.ActualMicros, ReleasedMicros: *reservation.ReleasedMicros,
		OverageMicros: *reservation.OverageMicros, ResultCommittedMicros: entry.ResultCommittedMicros,
		ResultReservedMicros: entry.ResultReservedMicros, RemainingHardMicros: remaining,
		SoftLimitExceeded: spent > account.SoftLimitMicros, Idempotent: idempotent, Attempts: attempts,
	}, nil
}

func allChargesAre(charges []SettlementCharge, kind ChargeKind) bool {
	for _, charge := range charges {
		if charge.Kind != kind {
			return false
		}
	}
	return true
}

func settlementReservationStatus(outcome SettlementOutcome) ReservationStatus {
	if outcome == SettlementCancelled {
		return ReservationCancelled
	}
	return ReservationSettled
}

func settlementEntryKind(outcome SettlementOutcome, actual uint64) EntryKind {
	if outcome == SettlementCancelled && actual == 0 {
		return EntryRelease
	}
	return EntrySettle
}

func settlementLedgerKey(kind EntryKind, reservationID string) string {
	return string(kind) + ":" + reservationID
}
