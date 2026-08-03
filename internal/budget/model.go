// Package budget defines exact-money accounts, reservations and ledger facts.
package budget

import (
	"errors"
	"regexp"
	"time"
)

const (
	// MaximumAmount is the largest cross-system exact integer shared with Redis Lua.
	MaximumAmount uint64 = 9_007_199_254_740_991
)

var (
	// ErrInvalid means a budget fact is incomplete, ambiguous or unsafe.
	ErrInvalid = errors.New("budget fact is invalid")

	budgetUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	budgetRefPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
	currencyPattern   = regexp.MustCompile(`^[A-Z]{3}$`)
)

// ScopeKind is the independently budgeted tenant-owned dimension.
type ScopeKind string

const (
	// ScopeTenant owns one budget directly at the tenant isolation root.
	ScopeTenant ScopeKind = "tenant"
	// ScopeProject owns one budget for a project inside its tenant.
	ScopeProject ScopeKind = "project"
	// ScopeKey owns one budget for a virtual key inside its project and tenant.
	ScopeKey ScopeKind = "key"
	// ScopeUser owns one budget for a tenant-local opaque principal reference.
	ScopeUser ScopeKind = "user"
	// ScopeSession owns one budget for a tenant-local opaque session reference.
	ScopeSession ScopeKind = "session"
)

// AccountStatus controls whether new reservations may be created.
type AccountStatus string

const (
	// AccountOpen permits new reservations subject to the hard limit.
	AccountOpen AccountStatus = "open"
	// AccountClosed is terminal and rejects new reservations.
	AccountClosed AccountStatus = "closed"
)

// ReservationStatus is the one-way reservation lifecycle.
type ReservationStatus string

const (
	// ReservationPending represents an active hold awaiting reconciliation.
	ReservationPending ReservationStatus = "pending"
	// ReservationSettled records a completed request's actual charge.
	ReservationSettled ReservationStatus = "settled"
	// ReservationCancelled records a caller or gateway cancellation.
	ReservationCancelled ReservationStatus = "cancelled"
	// ReservationExpired records recovery of a stale pending hold.
	ReservationExpired ReservationStatus = "expired"
)

// EntryKind describes one immutable balance transition.
type EntryKind string

const (
	// EntryReserve adds a new hold to the reserved balance.
	EntryReserve EntryKind = "reserve"
	// EntrySettle moves a hold into committed spend and releases any difference.
	EntrySettle EntryKind = "settle"
	// EntryRelease removes a cancelled hold without committing spend.
	EntryRelease EntryKind = "release"
	// EntryExpire removes a stale hold recovered by the reaper.
	EntryExpire EntryKind = "expire"
	// EntryAdjustment records an authorized correction without a reservation.
	EntryAdjustment EntryKind = "adjustment"
)

// Scope contains exactly the identity required by Kind. User and Session refs
// are opaque tenant-local identifiers, never raw prompts or personal data.
type Scope struct {
	Kind         ScopeKind
	TenantID     string
	ProjectID    string
	VirtualKeyID string
	PrincipalRef string
	SessionRef   string
}

// Validate rejects ambiguous or cross-dimensional scope shapes.
func (scope Scope) Validate() error {
	if !budgetUUIDPattern.MatchString(scope.TenantID) {
		return ErrInvalid
	}
	switch scope.Kind {
	case ScopeTenant:
		if scope.ProjectID != "" || scope.VirtualKeyID != "" || scope.PrincipalRef != "" || scope.SessionRef != "" {
			return ErrInvalid
		}
	case ScopeProject:
		if !budgetUUIDPattern.MatchString(scope.ProjectID) || scope.VirtualKeyID != "" ||
			scope.PrincipalRef != "" || scope.SessionRef != "" {
			return ErrInvalid
		}
	case ScopeKey:
		if !budgetUUIDPattern.MatchString(scope.ProjectID) || !budgetUUIDPattern.MatchString(scope.VirtualKeyID) ||
			scope.PrincipalRef != "" || scope.SessionRef != "" {
			return ErrInvalid
		}
	case ScopeUser:
		if scope.ProjectID != "" || scope.VirtualKeyID != "" || !budgetRefPattern.MatchString(scope.PrincipalRef) ||
			scope.SessionRef != "" {
			return ErrInvalid
		}
	case ScopeSession:
		if scope.ProjectID != "" || scope.VirtualKeyID != "" || scope.PrincipalRef != "" ||
			!budgetRefPattern.MatchString(scope.SessionRef) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

// Account is one currency budget for one exact Scope and half-open period.
type Account struct {
	ID              string
	Scope           Scope
	Currency        string
	PeriodStart     time.Time
	PeriodEnd       time.Time
	SoftLimitMicros uint64
	HardLimitMicros uint64
	CommittedMicros uint64
	ReservedMicros  uint64
	Status          AccountStatus
	Version         int64
	CreatedAt       time.Time
	CreatedBy       string
	UpdatedAt       time.Time
	UpdatedBy       string
	ClosedAt        *time.Time
}

// Validate checks an account snapshot without consulting database parents.
func (account Account) Validate() error {
	if !budgetUUIDPattern.MatchString(account.ID) || account.Scope.Validate() != nil ||
		!currencyPattern.MatchString(account.Currency) || account.PeriodStart.IsZero() ||
		!account.PeriodEnd.After(account.PeriodStart) || !validAmount(account.SoftLimitMicros) ||
		!validAmount(account.HardLimitMicros) || account.SoftLimitMicros > account.HardLimitMicros ||
		account.CommittedMicros > MaximumAmount || account.ReservedMicros > MaximumAmount ||
		account.CommittedMicros > MaximumAmount-account.ReservedMicros || account.Version < 1 ||
		account.CreatedAt.IsZero() || account.UpdatedAt.Before(account.CreatedAt) ||
		!validActor(account.CreatedBy) || !validActor(account.UpdatedBy) ||
		(account.Status != AccountOpen && account.Status != AccountClosed) {
		return ErrInvalid
	}
	if (account.Status == AccountClosed) != (account.ClosedAt != nil) ||
		(account.ClosedAt != nil && account.ClosedAt.Before(account.CreatedAt)) {
		return ErrInvalid
	}
	return nil
}

// Reservation is a pending hold or one immutable terminal reconciliation.
type Reservation struct {
	ID             string
	TenantID       string
	AccountID      string
	RequestID      string
	IdempotencyKey string
	Status         ReservationStatus
	ReservedMicros uint64
	ActualMicros   *uint64
	ReleasedMicros *uint64
	OverageMicros  *uint64
	ExpiresAt      time.Time
	Version        int64
	CreatedAt      time.Time
	CreatedBy      string
	UpdatedAt      time.Time
	UpdatedBy      string
	TerminalAt     *time.Time
}

// Validate enforces pending/terminal amount identities.
func (reservation Reservation) Validate() error {
	if !budgetUUIDPattern.MatchString(reservation.ID) || !budgetUUIDPattern.MatchString(reservation.TenantID) ||
		!budgetUUIDPattern.MatchString(reservation.AccountID) || !budgetRefPattern.MatchString(reservation.RequestID) ||
		!budgetRefPattern.MatchString(reservation.IdempotencyKey) || !validAmount(reservation.ReservedMicros) ||
		reservation.CreatedAt.IsZero() || !reservation.ExpiresAt.After(reservation.CreatedAt) ||
		reservation.UpdatedAt.Before(reservation.CreatedAt) || reservation.Version < 1 ||
		!validActor(reservation.CreatedBy) || !validActor(reservation.UpdatedBy) {
		return ErrInvalid
	}
	if reservation.Status == ReservationPending {
		if reservation.ActualMicros != nil || reservation.ReleasedMicros != nil ||
			reservation.OverageMicros != nil || reservation.TerminalAt != nil {
			return ErrInvalid
		}
		return nil
	}
	if reservation.Status != ReservationSettled && reservation.Status != ReservationCancelled &&
		reservation.Status != ReservationExpired || reservation.ActualMicros == nil ||
		reservation.ReleasedMicros == nil || reservation.OverageMicros == nil || reservation.TerminalAt == nil ||
		reservation.TerminalAt.Before(reservation.CreatedAt) {
		return ErrInvalid
	}
	actual := *reservation.ActualMicros
	if actual > MaximumAmount || *reservation.ReleasedMicros > MaximumAmount ||
		*reservation.OverageMicros > MaximumAmount {
		return ErrInvalid
	}
	wantReleased, wantOverage := reservationDifference(reservation.ReservedMicros, actual)
	if *reservation.ReleasedMicros != wantReleased || *reservation.OverageMicros != wantOverage {
		return ErrInvalid
	}
	return nil
}

// LedgerEntry is one immutable signed transition plus resulting balances.
type LedgerEntry struct {
	ID                    int64
	TenantID              string
	AccountID             string
	ReservationID         string
	Kind                  EntryKind
	IdempotencyKey        string
	CommittedDeltaMicros  int64
	ReservedDeltaMicros   int64
	ResultCommittedMicros uint64
	ResultReservedMicros  uint64
	OccurredAt            time.Time
	CreatedBy             string
}

// Validate checks entry shape and exact result bounds.
func (entry LedgerEntry) Validate() error {
	if entry.ID < 1 || !budgetUUIDPattern.MatchString(entry.TenantID) ||
		!budgetUUIDPattern.MatchString(entry.AccountID) || !budgetRefPattern.MatchString(entry.IdempotencyKey) ||
		entry.OccurredAt.IsZero() || !validActor(entry.CreatedBy) ||
		!validDelta(entry.CommittedDeltaMicros) || !validDelta(entry.ReservedDeltaMicros) ||
		(entry.CommittedDeltaMicros == 0 && entry.ReservedDeltaMicros == 0) ||
		entry.ResultCommittedMicros > MaximumAmount || entry.ResultReservedMicros > MaximumAmount ||
		entry.ResultCommittedMicros > MaximumAmount-entry.ResultReservedMicros {
		return ErrInvalid
	}
	hasReservation := budgetUUIDPattern.MatchString(entry.ReservationID)
	switch entry.Kind {
	case EntryReserve:
		if !hasReservation || entry.CommittedDeltaMicros != 0 || entry.ReservedDeltaMicros <= 0 {
			return ErrInvalid
		}
	case EntrySettle:
		if !hasReservation || entry.CommittedDeltaMicros < 0 || entry.ReservedDeltaMicros >= 0 {
			return ErrInvalid
		}
	case EntryRelease, EntryExpire:
		if !hasReservation || entry.CommittedDeltaMicros != 0 || entry.ReservedDeltaMicros >= 0 {
			return ErrInvalid
		}
	case EntryAdjustment:
		if entry.ReservationID != "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func reservationDifference(reserved, actual uint64) (uint64, uint64) {
	if actual <= reserved {
		return reserved - actual, 0
	}
	return 0, actual - reserved
}

func validAmount(value uint64) bool { return value > 0 && value <= MaximumAmount }

func validDelta(value int64) bool {
	return value >= -int64(MaximumAmount) && value <= int64(MaximumAmount)
}

func validActor(value string) bool {
	return len(value) >= 1 && len(value) <= 200 && value[0] != ' ' && value[len(value)-1] != ' '
}
