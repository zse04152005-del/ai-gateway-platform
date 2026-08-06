// Package meteringadjustment appends auditable corrections to immutable usage ledger facts.
package meteringadjustment

import (
	"errors"
	"regexp"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/metering"
)

var (
	// ErrInvalid means a scope, identity, corrected result, or audit field is malformed.
	ErrInvalid = errors.New("metering adjustment input is invalid")
	// ErrNotFound means the referenced original entry does not exist in the trusted scope.
	ErrNotFound = errors.New("metering adjustment target not found")
	// ErrInvalidTarget means stored facts cannot safely accept the requested correction.
	ErrInvalidTarget = errors.New("metering adjustment target is invalid")
	// ErrNoChange means the requested effective quantity and amount already match the ledger.
	ErrNoChange = errors.New("metering adjustment does not change the effective ledger fact")
	// ErrConflict means an idempotency key or event ID was reused for different facts.
	ErrConflict = errors.New("metering adjustment conflicts with an existing fact")
	// ErrStoreUnavailable means the durable adjustment transaction could not complete.
	ErrStoreUnavailable = errors.New("metering adjustment store is unavailable")

	uuidPattern = regexp.MustCompile(
		`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`,
	)
	idempotencyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{7,127}$`)
	reasonPattern      = regexp.MustCompile(`^[a-z][a-z0-9._:-]{0,127}$`)
	auditPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,199}$`)
)

// Origin identifies the bounded workflow that authorized a correction.
type Origin string

// Supported correction origins remain explicit for audit and policy checks.
const (
	OriginManual                 Origin = "manual"
	OriginProviderReconciliation Origin = "provider_reconciliation"
	OriginSystemRepair           Origin = "system_repair"
)

// Valid reports whether an origin belongs to the finite audit contract.
func (origin Origin) Valid() bool {
	switch origin {
	case OriginManual, OriginProviderReconciliation, OriginSystemRepair:
		return true
	default:
		return false
	}
}

// Scope is the trusted tenant/project boundary for one correction.
type Scope struct {
	TenantID  string
	ProjectID string
}

// Validate rejects malformed or ambiguous trusted scope identities.
func (scope Scope) Validate() error {
	if !uuidPattern.MatchString(scope.TenantID) || !uuidPattern.MatchString(scope.ProjectID) {
		return ErrInvalid
	}
	return nil
}

// Command describes an absolute corrected result. The writer derives signed
// deltas from the current append-only chain so retries never repeat a delta.
type Command struct {
	Scope                 Scope
	EventID               string
	IdempotencyKey        string
	TargetEventID         string
	CorrectedQuantity     int64
	CorrectedAmountMicros int64
	Origin                Origin
	Reason                string
	Reference             string
	Actor                 string
}

// Validate checks content-free correction identities and exact integer bounds.
func (command Command) Validate() error {
	if command.Scope.Validate() != nil || !uuidPattern.MatchString(command.EventID) ||
		!uuidPattern.MatchString(command.TargetEventID) || command.EventID == command.TargetEventID ||
		!idempotencyPattern.MatchString(command.IdempotencyKey) || !command.Origin.Valid() ||
		!reasonPattern.MatchString(command.Reason) || !auditPattern.MatchString(command.Reference) ||
		!auditPattern.MatchString(command.Actor) || command.CorrectedQuantity < 0 ||
		command.CorrectedQuantity > metering.MaximumExactInteger || command.CorrectedAmountMicros < 0 ||
		command.CorrectedAmountMicros > metering.MaximumExactInteger {
		return ErrInvalid
	}
	return nil
}

// Result is one inserted or idempotently replayed immutable correction.
type Result struct {
	EventID               string
	TargetEventID         string
	TenantID              string
	RequestID             string
	AttemptID             string
	TokenType             metering.TokenType
	PriceVersionID        string
	Currency              string
	QuantityDelta         int64
	AmountMicrosDelta     int64
	CorrectedQuantity     int64
	CorrectedAmountMicros int64
	Origin                Origin
	Reason                string
	Reference             string
	Actor                 string
	CreatedAt             time.Time
	Inserted              bool
	Replayed              bool
}
