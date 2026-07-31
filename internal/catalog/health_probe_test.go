package catalog

import (
	"strings"
	"testing"
)

func TestHealthProbeTargetValidateAndClone(t *testing.T) {
	t.Parallel()
	target := HealthProbeTarget{Provider: validProvider(), Deployment: validDeployment()}
	secretReferenceID := "71000000-0000-4000-8000-000000000001"
	target.Deployment.SecretReferenceID = &secretReferenceID
	if err := target.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	cloned := target.Clone()
	*cloned.Deployment.SecretReferenceID = "71000000-0000-4000-8000-000000000002"
	if *target.Deployment.SecretReferenceID != secretReferenceID {
		t.Fatal("Clone() aliases secret reference ID")
	}
}

func TestHealthProbeTargetRejectsUnsafeTargets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*HealthProbeTarget)
		want   string
	}{
		{name: "provider lifecycle", mutate: func(target *HealthProbeTarget) { target.Provider.Status = StatusDisabled }, want: "active"},
		{name: "deployment lifecycle", mutate: func(target *HealthProbeTarget) { target.Deployment.Status = StatusDisabled }, want: "active"},
		{name: "relationship", mutate: func(target *HealthProbeTarget) { target.Deployment.ProviderID = testTenantID }, want: "mismatch"},
		{name: "chat capability", mutate: func(target *HealthProbeTarget) {
			target.Deployment.Capabilities.Chat = false
			target.Deployment.Capabilities.Embeddings = true
		}, want: "chat"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			target := HealthProbeTarget{Provider: validProvider(), Deployment: validDeployment()}
			test.mutate(&target)
			if err := target.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %q", err, test.want)
			}
		})
	}
}
