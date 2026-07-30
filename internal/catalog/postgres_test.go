package catalog

import (
	"reflect"
	"testing"
)

func TestValidateAccessPreservesInheritanceAndExplicitDeny(t *testing.T) {
	t.Parallel()
	inherited, err := validateAccess(Access{TenantID: testTenantID, ProjectID: "21000000-0000-4000-8000-000000000001"})
	if err != nil {
		t.Fatalf("validateAccess(inherited) error = %v", err)
	}
	if inherited != nil {
		t.Fatalf("validateAccess(inherited) = %#v, want nil SQL array", inherited)
	}

	empty := []string{}
	explicitDeny, err := validateAccess(Access{
		TenantID: testTenantID, ProjectID: "21000000-0000-4000-8000-000000000001", KeyAllowedModels: &empty,
	})
	if err != nil {
		t.Fatalf("validateAccess(empty) error = %v", err)
	}
	if explicitDeny == nil {
		t.Fatal("validateAccess(empty) returned nil, which would incorrectly inherit project policy")
	}
}

func TestValidateAccessNormalizesCaseAndRejectsDuplicates(t *testing.T) {
	t.Parallel()
	models := []string{"General-Chat", "embed/v1"}
	value, err := validateAccess(Access{
		TenantID: testTenantID, ProjectID: "21000000-0000-4000-8000-000000000001", KeyAllowedModels: &models,
	})
	if err != nil {
		t.Fatalf("validateAccess() error = %v", err)
	}
	if value == nil {
		t.Fatal("validateAccess() = nil")
	}

	duplicates := []string{"general-chat", "GENERAL-CHAT"}
	if _, err := validateAccess(Access{
		TenantID: testTenantID, ProjectID: "21000000-0000-4000-8000-000000000001", KeyAllowedModels: &duplicates,
	}); err == nil || !IsValidationError(err) {
		t.Fatalf("validateAccess(duplicates) error = %v, want validation error", err)
	}
}

func TestCapabilityRequirementNamesAreDeterministic(t *testing.T) {
	t.Parallel()
	requirements := CapabilityRequirements{Chat: true, Stream: true, Tools: true, Vision: true}
	want := []string{"chat", "stream", "tools", "vision"}
	if got := requirements.Names(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Names() = %#v, want %#v", got, want)
	}
}
