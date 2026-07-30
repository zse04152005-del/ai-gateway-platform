// Package keyauth authenticates data-plane virtual credentials and attaches trusted identity context.
package keyauth

import (
	"context"
	"errors"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/virtualkey"
)

var (
	// ErrRecordNotFound is an internal lookup result and is rendered like every other invalid credential.
	ErrRecordNotFound = errors.New("authentication record not found")
	// ErrInvalidCredential deliberately combines missing, malformed, wrong, disabled, and expired credentials.
	ErrInvalidCredential = errors.New("invalid virtual credential")
	// ErrUnavailable means authentication could not make a trustworthy decision.
	ErrUnavailable = errors.New("virtual credential authentication unavailable")
	// ErrUnknownDigestVersion means the gateway lacks key material required by a stored record.
	ErrUnknownDigestVersion = errors.New("unknown virtual credential digest version")
)

// Record is the minimum database projection needed for an authentication decision.
type Record struct {
	ID                     string
	TenantID               string
	ProjectID              string
	Prefix                 string
	SecretHash             []byte `json:"-"`
	HashKeyVersion         string
	Status                 virtualkey.State
	ExpiresAt              *time.Time
	AllowedModels          *[]string
	Limits                 *virtualkey.Limits
	RotationGraceExpiresAt *time.Time
	TenantStatus           string
	ProjectStatus          string
}

// Principal is safe trusted identity passed to downstream data-plane handlers.
type Principal struct {
	TenantID      string
	ProjectID     string
	VirtualKeyID  string
	Prefix        string
	AllowedModels *[]string
	Limits        *virtualkey.Limits
}

type principalContextKey struct{}

// WithPrincipal attaches a deep-copied trusted identity.
func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, clonePrincipal(principal))
}

// PrincipalFromContext returns a deep copy so handlers cannot mutate cached policy values.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(Principal)
	if !ok {
		return Principal{}, false
	}
	return clonePrincipal(principal), true
}

func (record Record) principal() Principal {
	return Principal{
		TenantID:      record.TenantID,
		ProjectID:     record.ProjectID,
		VirtualKeyID:  record.ID,
		Prefix:        record.Prefix,
		AllowedModels: cloneStrings(record.AllowedModels),
		Limits:        cloneLimits(record.Limits),
	}
}

func cloneRecord(record Record) Record {
	record.SecretHash = append([]byte(nil), record.SecretHash...)
	record.ExpiresAt = cloneTime(record.ExpiresAt)
	record.AllowedModels = cloneStrings(record.AllowedModels)
	record.Limits = cloneLimits(record.Limits)
	record.RotationGraceExpiresAt = cloneTime(record.RotationGraceExpiresAt)
	return record
}

func clonePrincipal(principal Principal) Principal {
	principal.AllowedModels = cloneStrings(principal.AllowedModels)
	principal.Limits = cloneLimits(principal.Limits)
	return principal
}

func cloneStrings(values *[]string) *[]string {
	if values == nil {
		return nil
	}
	copyOfValues := make([]string, len(*values))
	copy(copyOfValues, *values)
	return &copyOfValues
}

func cloneLimits(value *virtualkey.Limits) *virtualkey.Limits {
	if value == nil {
		return nil
	}
	return &virtualkey.Limits{
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
