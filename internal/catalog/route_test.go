package catalog

import (
	"strings"
	"testing"
)

func TestRouteCandidateValidateAndClone(t *testing.T) {
	t.Parallel()
	candidate := validRouteCandidate()
	if err := candidate.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	cloned := candidate.Clone()
	*cloned.LogicalModel.Description = "Changed"
	(*cloned.LogicalModel.AllowedRegions)[0] = "us-east-1"
	cloned.LogicalModel.RequiredCapabilities.DataRetentionModes[0] = RetentionProviderDefault
	*cloned.Deployment.SecretReferenceID = "71000000-0000-4000-8000-000000000002"
	if *candidate.LogicalModel.Description != "General model" || (*candidate.LogicalModel.AllowedRegions)[0] != "cn-north-1" ||
		candidate.LogicalModel.RequiredCapabilities.DataRetentionModes[0] != RetentionZero ||
		*candidate.Deployment.SecretReferenceID != "71000000-0000-4000-8000-000000000001" {
		t.Fatal("RouteCandidate.Clone() aliases pointer or slice fields")
	}
}

func TestRouteCandidateRejectsRelationshipAndLifecycleDrift(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*RouteCandidate)
		want   string
	}{
		{name: "inactive", mutate: func(candidate *RouteCandidate) { candidate.Binding.Status = StatusDisabled }, want: "active"},
		{name: "logical relationship", mutate: func(candidate *RouteCandidate) { candidate.Binding.LogicalModelID = testTenantID }, want: "logical model mismatch"},
		{name: "deployment relationship", mutate: func(candidate *RouteCandidate) { candidate.Binding.DeploymentID = testTenantID }, want: "deployment mismatch"},
		{name: "provider relationship", mutate: func(candidate *RouteCandidate) { candidate.Deployment.ProviderID = testTenantID }, want: "provider mismatch"},
		{name: "capability drift", mutate: func(candidate *RouteCandidate) {
			candidate.Deployment.Capabilities.Chat = false
			candidate.Deployment.Capabilities.Embeddings = true
		}, want: "violates"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			candidate := validRouteCandidate()
			test.mutate(&candidate)
			if err := candidate.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %q", err, test.want)
			}
		})
	}
}

func validRouteCandidate() RouteCandidate {
	provider := validProvider()
	model := validLogicalModel()
	description := "General model"
	regions := []string{"cn-north-1"}
	model.Description = &description
	model.AllowedRegions = &regions
	model.RequiredCapabilities.DataRetentionModes = []DataRetentionMode{RetentionZero}
	deployment := validDeployment()
	deployment.Capabilities.DataRetentionMode = RetentionZero
	secretReferenceID := "71000000-0000-4000-8000-000000000001"
	deployment.SecretReferenceID = &secretReferenceID
	return RouteCandidate{
		LogicalModel: model,
		Binding:      validBinding(),
		Deployment:   deployment,
		Provider:     provider,
	}
}
