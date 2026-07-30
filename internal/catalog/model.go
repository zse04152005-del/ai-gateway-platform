// Package catalog defines the provider, logical-model, deployment, and capability domain.
package catalog

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// StatusActive makes a catalog record eligible for later routing filters.
	StatusActive Status = "active"
	// StatusDisabled makes a catalog record ineligible without deleting history.
	StatusDisabled Status = "disabled"

	// RetentionProviderDefault means the provider's published default applies.
	RetentionProviderDefault DataRetentionMode = "provider_default"
	// RetentionNoTraining means provider contractually excludes customer data from training.
	RetentionNoTraining DataRetentionMode = "no_training"
	// RetentionZero means provider contractually retains no request content after processing.
	RetentionZero DataRetentionMode = "zero_retention"
	// RetentionSelfHosted means the deployment runs inside a customer-controlled boundary.
	RetentionSelfHosted DataRetentionMode = "self_hosted"
)

var (
	uuidPattern           = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	providerCodePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,61}[a-z0-9]$`)
	adapterTypePattern    = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)
	modelNamePattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]{0,127}$`)
	deploymentCodePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	physicalModelPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,199}$`)
	regionPattern         = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	protocolPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
)

// Status is the persisted availability state shared by current catalog records.
type Status string

// DataRetentionMode is a normalized provider data-retention guarantee.
type DataRetentionMode string

// CapabilityRequirements describes the minimum contract promised by one logical model.
// False booleans are not requirements; callers must not use them to express prohibition.
type CapabilityRequirements struct {
	Chat               bool                `json:"chat,omitempty"`
	Stream             bool                `json:"stream,omitempty"`
	Tools              bool                `json:"tools,omitempty"`
	ParallelTools      bool                `json:"parallel_tools,omitempty"`
	StructuredOutput   bool                `json:"structured_output,omitempty"`
	Vision             bool                `json:"vision,omitempty"`
	AudioInput         bool                `json:"audio_input,omitempty"`
	AudioOutput        bool                `json:"audio_output,omitempty"`
	Embeddings         bool                `json:"embeddings,omitempty"`
	MinContextTokens   *int64              `json:"min_context_tokens,omitempty"`
	MinOutputTokens    *int64              `json:"min_output_tokens,omitempty"`
	DataRetentionModes []DataRetentionMode `json:"data_retention_modes,omitempty"`
}

// CapabilitySet is the declared callable contract for one physical deployment.
type CapabilitySet struct {
	Chat                    bool              `json:"chat,omitempty"`
	Stream                  bool              `json:"stream,omitempty"`
	Tools                   bool              `json:"tools,omitempty"`
	ParallelTools           bool              `json:"parallel_tools,omitempty"`
	StructuredOutput        bool              `json:"structured_output,omitempty"`
	Vision                  bool              `json:"vision,omitempty"`
	AudioInput              bool              `json:"audio_input,omitempty"`
	AudioOutput             bool              `json:"audio_output,omitempty"`
	Embeddings              bool              `json:"embeddings,omitempty"`
	UsageInStream           bool              `json:"usage_in_stream,omitempty"`
	CacheUsage              bool              `json:"cache_usage,omitempty"`
	ReasoningUsage          bool              `json:"reasoning_usage,omitempty"`
	MaxContextTokens        int64             `json:"max_context_tokens"`
	MaxOutputTokens         int64             `json:"max_output_tokens"`
	DataRetentionMode       DataRetentionMode `json:"data_retention_mode"`
	ProviderProtocolVersion string            `json:"provider_protocol_version"`
}

// Provider identifies a protocol adapter family without storing credentials.
type Provider struct {
	ID          string
	Code        string
	Name        string
	AdapterType string
	Status      Status
	Version     int64
	CreatedAt   time.Time
	CreatedBy   string
	UpdatedAt   time.Time
	UpdatedBy   string
}

// LogicalModel is a tenant-scoped stable client model name.
type LogicalModel struct {
	ID                   string
	TenantID             string
	Name                 string
	DisplayName          string
	Description          *string
	RequiredCapabilities CapabilityRequirements
	AllowedRegions       *[]string
	Status               Status
	Version              int64
	CreatedAt            time.Time
	CreatedBy            string
	UpdatedAt            time.Time
	UpdatedBy            string
}

// Deployment is a physical provider model endpoint and its callable contract.
// Credentials deliberately do not belong to this P04-T05 model.
type Deployment struct {
	ID                string
	ProviderID        string
	Code              string
	PhysicalModel     string
	EndpointURL       string
	Region            string
	Capabilities      CapabilitySet
	SecretReferenceID *string
	Status            Status
	Version           int64
	CreatedAt         time.Time
	CreatedBy         string
	UpdatedAt         time.Time
	UpdatedBy         string
}

// Binding maps a stable logical name to a compatible physical deployment.
type Binding struct {
	LogicalModelID string
	DeploymentID   string
	Priority       int16
	Weight         int16
	Status         Status
	Version        int64
	CreatedAt      time.Time
	CreatedBy      string
	UpdatedAt      time.Time
	UpdatedBy      string
}

// Access identifies the trusted tenant/project scope and optional per-key narrowing filter.
// A nil KeyAllowedModels inherits the project allowlist; an empty slice denies every model.
type Access struct {
	TenantID         string
	ProjectID        string
	KeyAllowedModels *[]string
}

// AvailableModel is the safe logical-model projection returned to data-plane clients.
type AvailableModel struct {
	Name         string
	Capabilities []string
}

// ValidationError identifies one safe invalid domain field.
type ValidationError struct {
	Field  string
	Reason string
}

// Error implements error.
func (err *ValidationError) Error() string {
	if err == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s: %s", err.Field, err.Reason)
}

// Validate checks provider identity, lifecycle, and audit invariants.
func (provider Provider) Validate() error {
	if err := validateUUID("provider.id", provider.ID); err != nil {
		return err
	}
	if !providerCodePattern.MatchString(provider.Code) {
		return invalid("provider.code", "must be a canonical 2-63 character identifier")
	}
	if err := validateTrimmed("provider.name", provider.Name, 1, 200); err != nil {
		return err
	}
	if !adapterTypePattern.MatchString(provider.AdapterType) {
		return invalid("provider.adapter_type", "must be a canonical adapter identifier")
	}
	return validateRecord("provider", provider.Status, provider.Version, provider.CreatedAt, provider.CreatedBy, provider.UpdatedAt, provider.UpdatedBy)
}

// Validate checks logical-model tenant scope, naming, and capability requirements.
func (model LogicalModel) Validate() error {
	if err := validateUUID("logical_model.id", model.ID); err != nil {
		return err
	}
	if err := validateUUID("logical_model.tenant_id", model.TenantID); err != nil {
		return err
	}
	if !modelNamePattern.MatchString(model.Name) {
		return invalid("logical_model.name", "must be a canonical 1-128 character model identifier")
	}
	if err := validateTrimmed("logical_model.display_name", model.DisplayName, 1, 200); err != nil {
		return err
	}
	if model.Description != nil {
		if err := validateTrimmed("logical_model.description", *model.Description, 1, 1000); err != nil {
			return err
		}
	}
	if err := model.RequiredCapabilities.Validate(); err != nil {
		return err
	}
	if err := validateRegions(model.AllowedRegions); err != nil {
		return err
	}
	return validateRecord("logical_model", model.Status, model.Version, model.CreatedAt, model.CreatedBy, model.UpdatedAt, model.UpdatedBy)
}

// Validate checks physical endpoint syntax and its declared capability contract.
// Network destination approval is intentionally deferred to the SSRF policy layer.
func (deployment Deployment) Validate() error {
	if err := validateUUID("deployment.id", deployment.ID); err != nil {
		return err
	}
	if err := validateUUID("deployment.provider_id", deployment.ProviderID); err != nil {
		return err
	}
	if !deploymentCodePattern.MatchString(deployment.Code) {
		return invalid("deployment.code", "must be a canonical 1-63 character identifier")
	}
	if !physicalModelPattern.MatchString(deployment.PhysicalModel) {
		return invalid("deployment.physical_model", "must be a canonical provider model identifier")
	}
	if err := validateEndpoint(deployment.EndpointURL); err != nil {
		return err
	}
	if !regionPattern.MatchString(deployment.Region) {
		return invalid("deployment.region", "must be a canonical 1-63 character region")
	}
	if err := deployment.Capabilities.Validate(); err != nil {
		return err
	}
	if deployment.SecretReferenceID != nil {
		if err := validateUUID("deployment.secret_reference_id", *deployment.SecretReferenceID); err != nil {
			return err
		}
	}
	return validateRecord("deployment", deployment.Status, deployment.Version, deployment.CreatedAt, deployment.CreatedBy, deployment.UpdatedAt, deployment.UpdatedBy)
}

// Validate checks mapping identity, route ordering, and audit invariants.
func (binding Binding) Validate() error {
	if err := validateUUID("binding.logical_model_id", binding.LogicalModelID); err != nil {
		return err
	}
	if err := validateUUID("binding.deployment_id", binding.DeploymentID); err != nil {
		return err
	}
	if binding.Priority < 1 || binding.Priority > 1000 {
		return invalid("binding.priority", "must be between 1 and 1000")
	}
	if binding.Weight < 1 || binding.Weight > 10000 {
		return invalid("binding.weight", "must be between 1 and 10000")
	}
	return validateRecord("binding", binding.Status, binding.Version, binding.CreatedAt, binding.CreatedBy, binding.UpdatedAt, binding.UpdatedBy)
}

// Validate checks the minimum capability contract.
func (requirements CapabilityRequirements) Validate() error {
	if requirements.ParallelTools && !requirements.Tools {
		return invalid("required_capabilities.parallel_tools", "requires tools")
	}
	if !requirements.hasRequirement() {
		return invalid("required_capabilities", "must contain at least one requirement")
	}
	if err := validatePositiveTokenMinimum("required_capabilities.min_context_tokens", requirements.MinContextTokens); err != nil {
		return err
	}
	if err := validatePositiveTokenMinimum("required_capabilities.min_output_tokens", requirements.MinOutputTokens); err != nil {
		return err
	}
	if requirements.MinContextTokens != nil && requirements.MinOutputTokens != nil && *requirements.MinOutputTokens > *requirements.MinContextTokens {
		return invalid("required_capabilities.min_output_tokens", "must not exceed min_context_tokens")
	}
	if len(requirements.DataRetentionModes) > 4 {
		return invalid("required_capabilities.data_retention_modes", "must contain at most four modes")
	}
	seen := make(map[DataRetentionMode]struct{}, len(requirements.DataRetentionModes))
	for _, mode := range requirements.DataRetentionModes {
		if !mode.Valid() {
			return invalid("required_capabilities.data_retention_modes", "contains an unsupported mode")
		}
		if _, exists := seen[mode]; exists {
			return invalid("required_capabilities.data_retention_modes", "contains a duplicate mode")
		}
		seen[mode] = struct{}{}
	}
	return nil
}

// Validate checks a complete physical capability declaration.
func (capabilities CapabilitySet) Validate() error {
	if !capabilities.Chat && !capabilities.Embeddings {
		return invalid("capabilities", "must enable chat or embeddings")
	}
	if capabilities.ParallelTools && !capabilities.Tools {
		return invalid("capabilities.parallel_tools", "requires tools")
	}
	if capabilities.UsageInStream && !capabilities.Stream {
		return invalid("capabilities.usage_in_stream", "requires stream")
	}
	if capabilities.MaxContextTokens <= 0 {
		return invalid("capabilities.max_context_tokens", "must be a positive integer")
	}
	if capabilities.MaxOutputTokens <= 0 || capabilities.MaxOutputTokens > capabilities.MaxContextTokens {
		return invalid("capabilities.max_output_tokens", "must be positive and not exceed max_context_tokens")
	}
	if !capabilities.DataRetentionMode.Valid() {
		return invalid("capabilities.data_retention_mode", "is unsupported")
	}
	if !protocolPattern.MatchString(capabilities.ProviderProtocolVersion) {
		return invalid("capabilities.provider_protocol_version", "must be a canonical 1-64 character identifier")
	}
	return nil
}

// Satisfies reports whether a physical capability set honors every logical requirement.
func (capabilities CapabilitySet) Satisfies(requirements CapabilityRequirements) bool {
	booleanContract := (!requirements.Chat || capabilities.Chat) &&
		(!requirements.Stream || capabilities.Stream) &&
		(!requirements.Tools || capabilities.Tools) &&
		(!requirements.ParallelTools || capabilities.ParallelTools) &&
		(!requirements.StructuredOutput || capabilities.StructuredOutput) &&
		(!requirements.Vision || capabilities.Vision) &&
		(!requirements.AudioInput || capabilities.AudioInput) &&
		(!requirements.AudioOutput || capabilities.AudioOutput) &&
		(!requirements.Embeddings || capabilities.Embeddings)
	if !booleanContract {
		return false
	}
	if requirements.MinContextTokens != nil && capabilities.MaxContextTokens < *requirements.MinContextTokens {
		return false
	}
	if requirements.MinOutputTokens != nil && capabilities.MaxOutputTokens < *requirements.MinOutputTokens {
		return false
	}
	if len(requirements.DataRetentionModes) > 0 && !containsRetention(requirements.DataRetentionModes, capabilities.DataRetentionMode) {
		return false
	}
	return true
}

// Names returns a deterministic list of client-visible boolean capabilities.
func (requirements CapabilityRequirements) Names() []string {
	names := make([]string, 0, 9)
	for _, candidate := range []struct {
		name    string
		enabled bool
	}{
		{name: "chat", enabled: requirements.Chat},
		{name: "stream", enabled: requirements.Stream},
		{name: "tools", enabled: requirements.Tools},
		{name: "parallel_tools", enabled: requirements.ParallelTools},
		{name: "structured_output", enabled: requirements.StructuredOutput},
		{name: "vision", enabled: requirements.Vision},
		{name: "audio_input", enabled: requirements.AudioInput},
		{name: "audio_output", enabled: requirements.AudioOutput},
		{name: "embeddings", enabled: requirements.Embeddings},
	} {
		if candidate.enabled {
			names = append(names, candidate.name)
		}
	}
	return names
}

// Satisfies reports whether a deployment honors capability and region requirements.
func (deployment Deployment) Satisfies(model LogicalModel) bool {
	if !deployment.Capabilities.Satisfies(model.RequiredCapabilities) {
		return false
	}
	if model.AllowedRegions == nil {
		return true
	}
	for _, region := range *model.AllowedRegions {
		if region == deployment.Region {
			return true
		}
	}
	return false
}

// Valid reports whether the retention mode is part of the normalized contract.
func (mode DataRetentionMode) Valid() bool {
	switch mode {
	case RetentionProviderDefault, RetentionNoTraining, RetentionZero, RetentionSelfHosted:
		return true
	default:
		return false
	}
}

func (requirements CapabilityRequirements) hasRequirement() bool {
	return requirements.Chat || requirements.Stream || requirements.Tools || requirements.ParallelTools ||
		requirements.StructuredOutput || requirements.Vision || requirements.AudioInput ||
		requirements.AudioOutput || requirements.Embeddings || requirements.MinContextTokens != nil ||
		requirements.MinOutputTokens != nil || len(requirements.DataRetentionModes) > 0
}

func validateEndpoint(rawURL string) error {
	if rawURL != strings.TrimSpace(rawURL) || len(rawURL) < 10 || len(rawURL) > 2048 {
		return invalid("deployment.endpoint_url", "must be a trimmed 10-2048 character URL")
	}
	if strings.IndexFunc(rawURL, func(character rune) bool {
		return unicode.IsSpace(character) || unicode.IsControl(character)
	}) >= 0 {
		return invalid("deployment.endpoint_url", "must not contain whitespace or control characters")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return invalid("deployment.endpoint_url", "must be a valid absolute URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return invalid("deployment.endpoint_url", "scheme must be http or https")
	}
	if parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" {
		return invalid("deployment.endpoint_url", "must have a host and no userinfo, query, or fragment")
	}
	return nil
}

func validateRegions(regions *[]string) error {
	if regions == nil {
		return nil
	}
	if len(*regions) < 1 || len(*regions) > 64 {
		return invalid("logical_model.allowed_regions", "must contain 1-64 entries when set")
	}
	seen := make(map[string]struct{}, len(*regions))
	for _, region := range *regions {
		if !regionPattern.MatchString(region) {
			return invalid("logical_model.allowed_regions", "contains an invalid region")
		}
		if _, exists := seen[region]; exists {
			return invalid("logical_model.allowed_regions", "contains a duplicate region")
		}
		seen[region] = struct{}{}
	}
	return nil
}

func validateRecord(prefix string, status Status, version int64, createdAt time.Time, createdBy string, updatedAt time.Time, updatedBy string) error {
	if status != StatusActive && status != StatusDisabled {
		return invalid(prefix+".status", "must be active or disabled")
	}
	if version <= 0 {
		return invalid(prefix+".version", "must be positive")
	}
	if createdAt.IsZero() || updatedAt.Before(createdAt) {
		return invalid(prefix+".timestamps", "must contain ordered non-zero timestamps")
	}
	if err := validateTrimmed(prefix+".created_by", createdBy, 1, 200); err != nil {
		return err
	}
	return validateTrimmed(prefix+".updated_by", updatedBy, 1, 200)
}

func validateUUID(field, value string) error {
	if !uuidPattern.MatchString(strings.ToLower(value)) {
		return invalid(field, "must be a valid UUID")
	}
	return nil
}

func validateTrimmed(field, value string, minimum, maximum int) error {
	length := utf8.RuneCountInString(value)
	if value != strings.TrimSpace(value) || length < minimum || length > maximum || !utf8.ValidString(value) {
		return invalid(field, fmt.Sprintf("must be %d-%d trimmed characters", minimum, maximum))
	}
	return nil
}

func validatePositiveTokenMinimum(field string, value *int64) error {
	if value != nil && *value <= 0 {
		return invalid(field, "must be a positive integer")
	}
	return nil
}

func containsRetention(values []DataRetentionMode, wanted DataRetentionMode) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func invalid(field, reason string) error {
	return &ValidationError{Field: field, Reason: reason}
}

// IsValidationError reports whether an error is a safe catalog input failure.
func IsValidationError(err error) bool {
	var validationError *ValidationError
	return errors.As(err, &validationError)
}
