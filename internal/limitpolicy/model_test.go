package limitpolicy

import (
	"errors"
	"testing"
	"time"
)

const (
	testPolicyID = "71000000-0000-4000-8000-000000000001"
	testTenantID = "71000000-0000-4000-8000-000000000002"
)

func TestResolveInheritanceAndPerBoundaryOverrides(t *testing.T) {
	platform := concreteLimits(80, 100, 8_000, 10_000, 8, 10)
	tenant := Limits{RPM: Threshold{Soft: value(90)}}
	project := Limits{
		RPM: Threshold{Hard: value(95)},
		TPM: Threshold{Soft: value(8_500), Hard: value(9_000)},
	}
	key := Limits{Concurrency: Threshold{Soft: value(4), Hard: value(5)}}

	effective, err := Resolve(Stack{
		Platform: platform, Tenant: &tenant, Project: &project, Key: &key,
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if effective.RPM.Soft != 90 || effective.RPM.Hard != 95 ||
		effective.RPM.SoftSource != SourceTenant || effective.RPM.HardSource != SourceProject ||
		effective.TPM.Soft != 8_500 || effective.TPM.Hard != 9_000 ||
		effective.TPM.SoftSource != SourceProject || effective.TPM.HardSource != SourceProject ||
		effective.Concurrency.Soft != 4 || effective.Concurrency.Hard != 5 ||
		effective.Concurrency.SoftSource != SourceKey || effective.Concurrency.HardSource != SourceKey {
		t.Fatalf("effective policy = %+v", effective)
	}
	if effective.Validate() != nil {
		t.Fatalf("Effective.Validate() error = %v", effective.Validate())
	}
}

func TestResolveRejectsIncompleteOrUnsafeStacks(t *testing.T) {
	valid := concreteLimits(80, 100, 8_000, 10_000, 8, 10)
	tooLarge := MaximumValue + 1
	tests := []struct {
		name  string
		stack Stack
	}{
		{name: "missing platform fields", stack: Stack{Platform: Limits{RPM: valid.RPM}}},
		{name: "empty explicit child", stack: Stack{Platform: valid, Tenant: &Limits{}}},
		{name: "zero child", stack: Stack{Platform: valid, Tenant: &Limits{RPM: Threshold{Soft: value(0)}}}},
		{name: "too large child", stack: Stack{Platform: valid, Project: &Limits{TPM: Threshold{Hard: &tooLarge}}}},
		{name: "local soft above hard", stack: Stack{
			Platform: valid, Key: &Limits{Concurrency: Threshold{Soft: value(7), Hard: value(6)}},
		}},
		{name: "inherited soft above overridden hard", stack: Stack{
			Platform: valid, Project: &Limits{RPM: Threshold{Hard: value(70)}},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Resolve(test.stack); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Resolve() error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestPolicyValidationBoundaries(t *testing.T) {
	now := time.Date(2026, time.July, 31, 11, 0, 0, 0, time.UTC)
	valid := Policy{
		ID: testPolicyID, TenantID: testTenantID, Reference: "default-chat/v1", Status: StatusActive,
		Limits:  Limits{RPM: Threshold{Soft: value(80), Hard: value(100)}},
		Version: 1, CreatedAt: now, CreatedBy: "admin:test", UpdatedAt: now, UpdatedBy: "admin:test",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Policy.Validate(valid) error = %v", err)
	}
	disabledAt := now.Add(time.Minute)
	disabled := valid
	disabled.Status = StatusDisabled
	disabled.DisabledAt = &disabledAt
	disabled.UpdatedAt = disabledAt
	if err := disabled.Validate(); err != nil {
		t.Fatalf("Policy.Validate(disabled) error = %v", err)
	}

	invalid := []Policy{
		{},
		func() Policy { policy := valid; policy.ID = "bad"; return policy }(),
		func() Policy { policy := valid; policy.Reference = " private"; return policy }(),
		func() Policy { policy := valid; policy.Status = "draft"; return policy }(),
		func() Policy { policy := valid; policy.Limits = Limits{}; return policy }(),
		func() Policy { policy := valid; policy.Version = 0; return policy }(),
		func() Policy { policy := valid; policy.CreatedBy = " actor "; return policy }(),
		func() Policy { policy := valid; policy.UpdatedAt = now.Add(-time.Second); return policy }(),
		func() Policy { policy := valid; policy.DisabledAt = &disabledAt; return policy }(),
	}
	for index, policy := range invalid {
		if err := policy.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Policy.Validate(invalid[%d]) error = %v, want ErrInvalid", index, err)
		}
	}
}

func concreteLimits(rpmSoft, rpmHard, tpmSoft, tpmHard, concurrencySoft, concurrencyHard uint64) Limits {
	return Limits{
		RPM:         Threshold{Soft: value(rpmSoft), Hard: value(rpmHard)},
		TPM:         Threshold{Soft: value(tpmSoft), Hard: value(tpmHard)},
		Concurrency: Threshold{Soft: value(concurrencySoft), Hard: value(concurrencyHard)},
	}
}

func value(input uint64) *uint64 { return &input }
