// Package limitpolicy defines hierarchical RPM, TPM, and concurrency limits.
package limitpolicy

import (
	"errors"
	"regexp"
	"time"
)

const (
	// MaximumValue is the largest integer represented exactly by Redis Lua's
	// IEEE-754 number type. All future distributed counters share this bound.
	MaximumValue uint64 = 9_007_199_254_740_991
)

var (
	// ErrInvalid means a policy or inheritance stack is incomplete or unsafe.
	ErrInvalid = errors.New("limit policy is invalid")

	uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	refPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
)

// Status is the finite control-plane lifecycle of a stored policy.
type Status string

const (
	// StatusActive permits a policy to participate in effective resolution.
	StatusActive Status = "active"
	// StatusDisabled prevents a policy from being selected for new snapshots.
	StatusDisabled Status = "disabled"
)

// Source identifies the layer that supplied one effective boundary.
type Source string

const (
	// SourcePlatform identifies a boundary supplied by the complete base policy.
	SourcePlatform Source = "platform"
	// SourceTenant identifies a boundary supplied by the Tenant override.
	SourceTenant Source = "tenant"
	// SourceProject identifies a boundary supplied by the Project override.
	SourceProject Source = "project"
	// SourceKey identifies a boundary supplied by the VirtualKey override.
	SourceKey Source = "key"
)

// Threshold is one optional soft/hard override. Nil inherits that individual
// boundary from the parent layer; zero never means unlimited.
type Threshold struct {
	Soft *uint64 `json:"soft,omitempty"`
	Hard *uint64 `json:"hard,omitempty"`
}

// Limits is the complete set of sparse per-resource overrides.
type Limits struct {
	RPM         Threshold `json:"rpm"`
	TPM         Threshold `json:"tpm"`
	Concurrency Threshold `json:"concurrency"`
}

// Policy is one tenant-owned, versioned set of reusable overrides.
type Policy struct {
	ID         string
	TenantID   string
	Reference  string
	Status     Status
	Limits     Limits
	Version    int64
	CreatedAt  time.Time
	CreatedBy  string
	UpdatedAt  time.Time
	UpdatedBy  string
	DisabledAt *time.Time
}

// Stack is resolved in fixed Platform -> Tenant -> Project -> Key order.
// Platform must be fully concrete; a nil child layer inherits every boundary.
type Stack struct {
	Platform Limits
	Tenant   *Limits
	Project  *Limits
	Key      *Limits
}

// EffectiveThreshold contains concrete admission boundaries and provenance.
type EffectiveThreshold struct {
	Soft       uint64 `json:"soft"`
	Hard       uint64 `json:"hard"`
	SoftSource Source `json:"soft_source"`
	HardSource Source `json:"hard_source"`
}

// Effective is the fully resolved policy consumed by local and Redis limits.
type Effective struct {
	RPM         EffectiveThreshold `json:"rpm"`
	TPM         EffectiveThreshold `json:"tpm"`
	Concurrency EffectiveThreshold `json:"concurrency"`
}

// Validate checks one stored policy without consulting another layer.
func (policy Policy) Validate() error {
	if !uuidPattern.MatchString(policy.ID) || !uuidPattern.MatchString(policy.TenantID) ||
		!refPattern.MatchString(policy.Reference) ||
		(policy.Status != StatusActive && policy.Status != StatusDisabled) || policy.Version < 1 ||
		policy.CreatedAt.IsZero() || policy.UpdatedAt.Before(policy.CreatedAt) ||
		!validActor(policy.CreatedBy) || !validActor(policy.UpdatedBy) ||
		policy.Limits.empty() || policy.Limits.validateOverride() != nil {
		return ErrInvalid
	}
	if (policy.Status == StatusDisabled) != (policy.DisabledAt != nil) ||
		(policy.DisabledAt != nil && policy.DisabledAt.Before(policy.CreatedAt)) {
		return ErrInvalid
	}
	return nil
}

// Resolve applies sparse overrides in the only supported hierarchy.
func Resolve(stack Stack) (Effective, error) {
	if stack.Platform.validateBase() != nil {
		return Effective{}, ErrInvalid
	}
	effective := Effective{
		RPM:         baseThreshold(stack.Platform.RPM),
		TPM:         baseThreshold(stack.Platform.TPM),
		Concurrency: baseThreshold(stack.Platform.Concurrency),
	}
	for _, layer := range []struct {
		source Source
		limits *Limits
	}{
		{SourceTenant, stack.Tenant},
		{SourceProject, stack.Project},
		{SourceKey, stack.Key},
	} {
		if layer.limits == nil {
			continue
		}
		if layer.limits.empty() || layer.limits.validateOverride() != nil {
			return Effective{}, ErrInvalid
		}
		applyThreshold(&effective.RPM, layer.limits.RPM, layer.source)
		applyThreshold(&effective.TPM, layer.limits.TPM, layer.source)
		applyThreshold(&effective.Concurrency, layer.limits.Concurrency, layer.source)
	}
	if effective.Validate() != nil {
		return Effective{}, ErrInvalid
	}
	return effective, nil
}

// Validate checks that every effective resource is concrete and soft <= hard.
func (effective Effective) Validate() error {
	for _, threshold := range []EffectiveThreshold{
		effective.RPM, effective.TPM, effective.Concurrency,
	} {
		if !validValue(threshold.Soft) || !validValue(threshold.Hard) || threshold.Soft > threshold.Hard ||
			!validSource(threshold.SoftSource) || !validSource(threshold.HardSource) {
			return ErrInvalid
		}
	}
	return nil
}

func (limits Limits) validateBase() error {
	if limits.validateOverride() != nil {
		return ErrInvalid
	}
	for _, threshold := range []Threshold{limits.RPM, limits.TPM, limits.Concurrency} {
		if threshold.Soft == nil || threshold.Hard == nil {
			return ErrInvalid
		}
	}
	return nil
}

func (limits Limits) validateOverride() error {
	for _, threshold := range []Threshold{limits.RPM, limits.TPM, limits.Concurrency} {
		if threshold.Soft != nil && !validValue(*threshold.Soft) ||
			threshold.Hard != nil && !validValue(*threshold.Hard) ||
			threshold.Soft != nil && threshold.Hard != nil && *threshold.Soft > *threshold.Hard {
			return ErrInvalid
		}
	}
	return nil
}

func (limits Limits) empty() bool {
	return limits.RPM.Soft == nil && limits.RPM.Hard == nil &&
		limits.TPM.Soft == nil && limits.TPM.Hard == nil &&
		limits.Concurrency.Soft == nil && limits.Concurrency.Hard == nil
}

func baseThreshold(threshold Threshold) EffectiveThreshold {
	return EffectiveThreshold{
		Soft: *threshold.Soft, Hard: *threshold.Hard,
		SoftSource: SourcePlatform, HardSource: SourcePlatform,
	}
}

func applyThreshold(effective *EffectiveThreshold, override Threshold, source Source) {
	if override.Soft != nil {
		effective.Soft = *override.Soft
		effective.SoftSource = source
	}
	if override.Hard != nil {
		effective.Hard = *override.Hard
		effective.HardSource = source
	}
}

func validValue(value uint64) bool { return value > 0 && value <= MaximumValue }

func validSource(source Source) bool {
	return source == SourcePlatform || source == SourceTenant || source == SourceProject || source == SourceKey
}

func validActor(actor string) bool {
	return len(actor) >= 1 && len(actor) <= 200 && actor[0] != ' ' && actor[len(actor)-1] != ' '
}
