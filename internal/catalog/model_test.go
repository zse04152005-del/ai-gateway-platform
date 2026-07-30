package catalog

import (
	"strings"
	"testing"
	"time"
)

const (
	testProviderID   = "41000000-0000-4000-8000-000000000001"
	testTenantID     = "11000000-0000-4000-8000-000000000001"
	testModelID      = "51000000-0000-4000-8000-000000000001"
	testDeploymentID = "61000000-0000-4000-8000-000000000001"
)

func TestDeploymentSatisfiesCapabilityAndRegionContract(t *testing.T) {
	t.Parallel()
	minimumContext := int64(32000)
	minimumOutput := int64(4096)
	regions := []string{"cn-north-1", "ap-southeast-1"}
	model := validLogicalModel()
	model.RequiredCapabilities = CapabilityRequirements{
		Chat: true, Stream: true, Tools: true, StructuredOutput: true,
		MinContextTokens: &minimumContext, MinOutputTokens: &minimumOutput,
		DataRetentionModes: []DataRetentionMode{RetentionZero, RetentionSelfHosted},
	}
	model.AllowedRegions = &regions

	deployment := validDeployment()
	deployment.Region = "cn-north-1"
	deployment.Capabilities.Tools = true
	deployment.Capabilities.StructuredOutput = true
	deployment.Capabilities.DataRetentionMode = RetentionZero
	if !deployment.Satisfies(model) {
		t.Fatal("Deployment.Satisfies() = false for compatible contract")
	}

	tests := []struct {
		name   string
		mutate func(*Deployment)
	}{
		{name: "missing tools", mutate: func(value *Deployment) { value.Capabilities.Tools = false }},
		{name: "context too small", mutate: func(value *Deployment) { value.Capabilities.MaxContextTokens = 16000 }},
		{name: "output too small", mutate: func(value *Deployment) { value.Capabilities.MaxOutputTokens = 2048 }},
		{name: "retention mismatch", mutate: func(value *Deployment) { value.Capabilities.DataRetentionMode = RetentionProviderDefault }},
		{name: "region mismatch", mutate: func(value *Deployment) { value.Region = "us-east-1" }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			candidate := deployment
			test.mutate(&candidate)
			if candidate.Satisfies(model) {
				t.Fatal("Deployment.Satisfies() = true for incompatible contract")
			}
		})
	}
}

func TestCapabilityValidationRejectsAmbiguousDeclarations(t *testing.T) {
	t.Parallel()
	positive := int64(1)
	negative := int64(-1)
	tests := []struct {
		name string
		got  error
	}{
		{name: "empty requirements", got: (CapabilityRequirements{}).Validate()},
		{name: "parallel tools without tools", got: (CapabilityRequirements{ParallelTools: true}).Validate()},
		{name: "negative minimum", got: (CapabilityRequirements{Chat: true, MinContextTokens: &negative}).Validate()},
		{name: "duplicate retention", got: (CapabilityRequirements{Chat: true, MinContextTokens: &positive, DataRetentionModes: []DataRetentionMode{RetentionZero, RetentionZero}}).Validate()},
		{name: "no primary interface", got: (CapabilitySet{MaxContextTokens: 1, MaxOutputTokens: 1, DataRetentionMode: RetentionZero, ProviderProtocolVersion: "v1"}).Validate()},
		{name: "stream usage without stream", got: (CapabilitySet{Chat: true, UsageInStream: true, MaxContextTokens: 1, MaxOutputTokens: 1, DataRetentionMode: RetentionZero, ProviderProtocolVersion: "v1"}).Validate()},
		{name: "oversized output", got: (CapabilitySet{Chat: true, MaxContextTokens: 1, MaxOutputTokens: 2, DataRetentionMode: RetentionZero, ProviderProtocolVersion: "v1"}).Validate()},
		{name: "unknown retention", got: (CapabilitySet{Chat: true, MaxContextTokens: 1, MaxOutputTokens: 1, DataRetentionMode: "forever", ProviderProtocolVersion: "v1"}).Validate()},
	}
	for _, test := range tests {
		if test.got == nil || !IsValidationError(test.got) {
			t.Errorf("%s: error = %v, want ValidationError", test.name, test.got)
		}
	}
}

func TestDeploymentValidationRejectsUnsafeEndpointSyntax(t *testing.T) {
	t.Parallel()
	tests := []string{
		"ftp://models.example.test/v1",
		"https://user:password@models.example.test/v1",
		"https://models.example.test/v1?credential=value",
		"https://models.example.test/v1#fragment",
		"https://models.example.test/v1\ninternal",
		"https:///v1",
	}
	for _, endpoint := range tests {
		deployment := validDeployment()
		deployment.EndpointURL = endpoint
		if err := deployment.Validate(); err == nil || !strings.Contains(err.Error(), "endpoint_url") {
			t.Errorf("Deployment.Validate(%q) error = %v, want endpoint error", endpoint, err)
		}
	}
}

func TestCatalogRecordsValidate(t *testing.T) {
	t.Parallel()
	provider := validProvider()
	if err := provider.Validate(); err != nil {
		t.Fatalf("Provider.Validate() error = %v", err)
	}
	model := validLogicalModel()
	if err := model.Validate(); err != nil {
		t.Fatalf("LogicalModel.Validate() error = %v", err)
	}
	deployment := validDeployment()
	if err := deployment.Validate(); err != nil {
		t.Fatalf("Deployment.Validate() error = %v", err)
	}
	binding := validBinding()
	if err := binding.Validate(); err != nil {
		t.Fatalf("Binding.Validate() error = %v", err)
	}

	model.Name = "Vendor Internal Model"
	if err := model.Validate(); err == nil || !IsValidationError(err) {
		t.Fatalf("LogicalModel.Validate() error = %v, want canonical-name validation", err)
	}
	regions := []string{"cn-north-1", "cn-north-1"}
	model = validLogicalModel()
	model.AllowedRegions = &regions
	if err := model.Validate(); err == nil || !IsValidationError(err) {
		t.Fatalf("LogicalModel.Validate() error = %v, want duplicate-region validation", err)
	}
}

func validProvider() Provider {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	return Provider{
		ID: testProviderID, Code: "mock-one", Name: "Mock One", AdapterType: "openai_compatible",
		Status: StatusActive, Version: 1, CreatedAt: now, CreatedBy: "test:catalog",
		UpdatedAt: now, UpdatedBy: "test:catalog",
	}
}

func validLogicalModel() LogicalModel {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	return LogicalModel{
		ID: testModelID, TenantID: testTenantID, Name: "general-chat", DisplayName: "General Chat",
		RequiredCapabilities: CapabilityRequirements{Chat: true, Stream: true},
		Status:               StatusActive, Version: 1, CreatedAt: now, CreatedBy: "test:catalog",
		UpdatedAt: now, UpdatedBy: "test:catalog",
	}
}

func validDeployment() Deployment {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	return Deployment{
		ID: testDeploymentID, ProviderID: testProviderID, Code: "chat-primary", PhysicalModel: "vendor-chat-v1",
		EndpointURL: "https://models.example.test/v1", Region: "cn-north-1",
		Capabilities: CapabilitySet{
			Chat: true, Stream: true, MaxContextTokens: 128000, MaxOutputTokens: 8192,
			DataRetentionMode: RetentionNoTraining, ProviderProtocolVersion: "openai-chat-v1",
		},
		Status: StatusActive, Version: 1, CreatedAt: now, CreatedBy: "test:catalog",
		UpdatedAt: now, UpdatedBy: "test:catalog",
	}
}

func validBinding() Binding {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	return Binding{
		LogicalModelID: testModelID, DeploymentID: testDeploymentID, Priority: 100, Weight: 100,
		Status: StatusActive, Version: 1, CreatedAt: now, CreatedBy: "test:catalog",
		UpdatedAt: now, UpdatedBy: "test:catalog",
	}
}
