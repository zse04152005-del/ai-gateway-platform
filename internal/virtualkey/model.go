// Package virtualkey owns virtual credential issuance and lifecycle semantics.
package virtualkey

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	// StateActive means the credential is active unless its absolute expiry has elapsed.
	StateActive State = "active"
	// StateRotating means the old credential remains usable only until its grace deadline.
	StateRotating State = "rotating"
	// StateRevoked means the credential is permanently disabled.
	StateRevoked State = "revoked"

	// EffectiveExpired is derived when expires_at has elapsed.
	EffectiveExpired EffectiveState = "expired"
	// EffectiveRotationGraceElapsed is derived when a rotating credential's grace period has elapsed.
	EffectiveRotationGraceElapsed EffectiveState = "rotation_grace_elapsed"
)

var (
	// ErrNotFound hides whether a tenant, project, or credential identifier was absent.
	ErrNotFound = errors.New("virtual credential not found")
	// ErrCollision means randomly generated public or digest identity already exists.
	ErrCollision = errors.New("virtual credential identity collision")
	// ErrAlreadyRotated means a credential already has a replacement.
	ErrAlreadyRotated = errors.New("virtual credential already rotated")
	// ErrInvalidState means the requested transition is not allowed from the stored state.
	ErrInvalidState = errors.New("virtual credential lifecycle transition is invalid")
	// ErrExpired means an expired credential cannot enter the requested transition.
	ErrExpired = errors.New("virtual credential is expired")

	uuidPattern  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	modelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
)

// State is the persisted lifecycle state.
type State string

// EffectiveState includes persisted and time-derived lifecycle states.
type EffectiveState string

// Limits is the optional per-credential limit override.
type Limits struct {
	RPM         *int64 `json:"rpm,omitempty"`
	TPM         *int64 `json:"tpm,omitempty"`
	Concurrency *int64 `json:"concurrency,omitempty"`
}

// Locator identifies a credential without allowing cross-tenant lookups.
type Locator struct {
	TenantID  string
	ProjectID string
	ID        string
}

// Record is the internal persistence model. SecretHash must never be serialized to an API response.
type Record struct {
	ID                     string
	TenantID               string
	ProjectID              string
	Prefix                 string
	SecretHash             []byte `json:"-"`
	HashKeyVersion         string
	Status                 State
	ExpiresAt              *time.Time
	AllowedModels          *[]string
	Limits                 *Limits
	RotatedFromID          *string
	RotationGraceExpiresAt *time.Time
	RevokedAt              *time.Time
	RevokedBy              *string
	Version                int64
	CreatedAt              time.Time
	CreatedBy              string
	UpdatedAt              time.Time
	UpdatedBy              string
}

// Metadata is the safe control-plane representation of a credential.
type Metadata struct {
	ID                     string         `json:"id"`
	TenantID               string         `json:"tenant_id"`
	ProjectID              string         `json:"project_id"`
	Prefix                 string         `json:"prefix"`
	Status                 State          `json:"status"`
	EffectiveStatus        EffectiveState `json:"effective_status"`
	ExpiresAt              *time.Time     `json:"expires_at"`
	AllowedModels          *[]string      `json:"allowed_models"`
	Limits                 *Limits        `json:"limits"`
	RotatedFromID          *string        `json:"rotated_from_id"`
	RotationGraceExpiresAt *time.Time     `json:"rotation_grace_expires_at"`
	RevokedAt              *time.Time     `json:"revoked_at"`
	RevokedBy              *string        `json:"revoked_by"`
	Version                int64          `json:"version"`
	CreatedAt              time.Time      `json:"created_at"`
	CreatedBy              string         `json:"created_by"`
	UpdatedAt              time.Time      `json:"updated_at"`
	UpdatedBy              string         `json:"updated_by"`
}

// IssuedCredential contains plaintext exactly at the create/rotate call boundary.
// Callers must return it once and must never log or persist Credential.
type IssuedCredential struct {
	Metadata   Metadata `json:"metadata"`
	Credential string   `json:"credential"`
}

// CreateCommand contains control-plane creation inputs.
type CreateCommand struct {
	TenantID      string
	ProjectID     string
	Mode          string
	ExpiresAt     *time.Time
	AllowedModels *[]string
	Limits        *Limits
	Actor         string
}

// RotateCommand contains an atomic rotation request.
type RotateCommand struct {
	Locator     Locator
	GracePeriod time.Duration
	Actor       string
}

// RevokeCommand contains an idempotent permanent-disable request.
type RevokeCommand struct {
	Locator Locator
	Actor   string
}

// ValidationError identifies a safe client input field without exposing internal state.
type ValidationError struct {
	Field  string
	Reason string
}

// Error implements error.
func (e *ValidationError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s: %s", e.Field, e.Reason)
}

// Metadata derives effective state at a caller-supplied instant and deep-copies mutable values.
func (record Record) Metadata(now time.Time) Metadata {
	effective := EffectiveState(record.Status)
	if record.Status != StateRevoked && record.ExpiresAt != nil && !now.Before(*record.ExpiresAt) {
		effective = EffectiveExpired
	} else if record.Status == StateRotating && record.RotationGraceExpiresAt != nil && !now.Before(*record.RotationGraceExpiresAt) {
		effective = EffectiveRotationGraceElapsed
	}

	return Metadata{
		ID:                     record.ID,
		TenantID:               record.TenantID,
		ProjectID:              record.ProjectID,
		Prefix:                 record.Prefix,
		Status:                 record.Status,
		EffectiveStatus:        effective,
		ExpiresAt:              cloneTime(record.ExpiresAt),
		AllowedModels:          cloneStrings(record.AllowedModels),
		Limits:                 cloneLimits(record.Limits),
		RotatedFromID:          cloneString(record.RotatedFromID),
		RotationGraceExpiresAt: cloneTime(record.RotationGraceExpiresAt),
		RevokedAt:              cloneTime(record.RevokedAt),
		RevokedBy:              cloneString(record.RevokedBy),
		Version:                record.Version,
		CreatedAt:              record.CreatedAt,
		CreatedBy:              record.CreatedBy,
		UpdatedAt:              record.UpdatedAt,
		UpdatedBy:              record.UpdatedBy,
	}
}

func validateLocator(locator Locator) error {
	for field, value := range map[string]string{
		"tenant_id":  locator.TenantID,
		"project_id": locator.ProjectID,
		"key_id":     locator.ID,
	} {
		if !uuidPattern.MatchString(strings.ToLower(value)) {
			return &ValidationError{Field: field, Reason: "must be a valid UUID"}
		}
	}
	return nil
}

func validateActor(actor string) error {
	if actor != strings.TrimSpace(actor) || len(actor) < 1 || len(actor) > 200 {
		return &ValidationError{Field: "actor", Reason: "must be 1-200 trimmed characters"}
	}
	return nil
}

func validateAllowedModels(models *[]string) error {
	if models == nil {
		return nil
	}
	if len(*models) > 256 {
		return &ValidationError{Field: "allowed_models", Reason: "must contain at most 256 entries"}
	}
	seen := make(map[string]struct{}, len(*models))
	for _, model := range *models {
		if model != strings.TrimSpace(model) || !modelPattern.MatchString(model) {
			return &ValidationError{Field: "allowed_models", Reason: "contains an invalid model identifier"}
		}
		canonical := strings.ToLower(model)
		if _, exists := seen[canonical]; exists {
			return &ValidationError{Field: "allowed_models", Reason: "contains a duplicate model identifier"}
		}
		seen[canonical] = struct{}{}
	}
	return nil
}

func validateLimits(limits *Limits) error {
	if limits == nil {
		return nil
	}
	for field, value := range map[string]*int64{
		"limits.rpm":         limits.RPM,
		"limits.tpm":         limits.TPM,
		"limits.concurrency": limits.Concurrency,
	} {
		if value != nil && *value <= 0 {
			return &ValidationError{Field: field, Reason: "must be a positive integer"}
		}
	}
	return nil
}

func cloneStrings(values *[]string) *[]string {
	if values == nil {
		return nil
	}
	copyOfValues := append([]string(nil), (*values)...)
	return &copyOfValues
}

func cloneLimits(value *Limits) *Limits {
	if value == nil {
		return nil
	}
	return &Limits{
		RPM:         cloneInt64(value.RPM),
		TPM:         cloneInt64(value.TPM),
		Concurrency: cloneInt64(value.Concurrency),
	}
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copyOfValue := *value
	return &copyOfValue
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copyOfValue := *value
	return &copyOfValue
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copyOfValue := *value
	return &copyOfValue
}
