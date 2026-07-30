package provideradapter

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
)

const (
	// ValidationStartup is the process bootstrap provider validation phase.
	ValidationStartup ValidationPhase = "startup"
	// ValidationPublication is the candidate catalog publication validation phase.
	ValidationPublication ValidationPhase = "publication"
)

var (
	// ErrInvalidFactory means a factory is nil or declares an invalid type.
	ErrInvalidFactory = errors.New("provider adapter factory is invalid")
	// ErrDuplicateAdapterType means two factories declare the same type.
	ErrDuplicateAdapterType = errors.New("provider adapter type is registered more than once")
	// ErrUnknownAdapterType means no explicitly registered factory owns a type.
	ErrUnknownAdapterType = errors.New("provider adapter type is not registered")
	// ErrInvalidProviderReference means startup/publication input is malformed or duplicated.
	ErrInvalidProviderReference = errors.New("provider adapter reference is invalid")
	// ErrFactoryFailed means a selected factory could not create an adapter.
	ErrFactoryFailed = errors.New("provider adapter factory failed")
	// ErrAdapterTypeMismatch means a factory returned an adapter with another type.
	ErrAdapterTypeMismatch = errors.New("provider adapter type does not match factory")

	typePattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)
)

// ValidationPhase identifies where an unknown type blocked progress.
type ValidationPhase string

// ReferenceProblem is one deterministic, safe provider catalog diagnostic.
type ReferenceProblem struct {
	ProviderID   string
	ProviderCode string
	AdapterType  Type
	Code         string
}

// ReferenceValidationError aggregates every invalid/duplicate/unknown provider
// reference so a publication can be fixed atomically instead of one item at a time.
type ReferenceValidationError struct {
	phase    ValidationPhase
	problems []ReferenceProblem
}

// Error returns deterministic identifiers only; it never formats provider secrets.
func (err *ReferenceValidationError) Error() string {
	if err == nil {
		return "<nil>"
	}
	return fmt.Sprintf("provider adapter %s validation failed with %d problem(s)", err.phase, len(err.problems))
}

// Is supports errors.Is for unknown types and invalid references.
func (err *ReferenceValidationError) Is(target error) bool {
	if err == nil {
		return false
	}
	for _, problem := range err.problems {
		if target == ErrUnknownAdapterType && problem.Code == "unknown_adapter_type" {
			return true
		}
		if target == ErrInvalidProviderReference && problem.Code != "unknown_adapter_type" {
			return true
		}
	}
	return false
}

// Phase returns the failed validation phase.
func (err *ReferenceValidationError) Phase() ValidationPhase {
	if err == nil {
		return ""
	}
	return err.phase
}

// Problems returns a defensive copy ordered by Provider ID, code, type, then code.
func (err *ReferenceValidationError) Problems() []ReferenceProblem {
	if err == nil {
		return nil
	}
	return append([]ReferenceProblem(nil), err.problems...)
}

// FactoryError safely identifies a factory failure while preserving the private
// cause for errors.Is/As. Error deliberately does not include cause.Error().
type FactoryError struct {
	AdapterType  Type
	DeploymentID string
	cause        error
}

// Error returns a non-sensitive factory diagnostic.
func (err *FactoryError) Error() string {
	if err == nil {
		return "<nil>"
	}
	return fmt.Sprintf("build provider adapter type %q for deployment %q: %v", err.AdapterType, err.DeploymentID, ErrFactoryFailed)
}

// Unwrap preserves programmatic access to the private cause.
func (err *FactoryError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.cause
}

// Is reports ErrFactoryFailed without exposing the private cause text.
func (err *FactoryError) Is(target error) bool {
	return err != nil && target == ErrFactoryFailed
}

// Registry is immutable after construction and safe for concurrent reads.
type Registry struct {
	factories map[Type]Factory
	types     []Type
}

// NewRegistry validates all built-in factories and constructs an immutable catalog.
// Runtime mutation and dynamic shared-library loading are intentionally unsupported.
func NewRegistry(factories ...Factory) (*Registry, error) {
	byType := make(map[Type]Factory, len(factories))
	for index, factory := range factories {
		if isNilFactory(factory) {
			return nil, fmt.Errorf("factory[%d]: %w", index, ErrInvalidFactory)
		}
		adapterType := factory.Type()
		if !typePattern.MatchString(string(adapterType)) {
			return nil, fmt.Errorf("factory[%d] type %q: %w", index, adapterType, ErrInvalidFactory)
		}
		if _, exists := byType[adapterType]; exists {
			return nil, fmt.Errorf("type %q: %w", adapterType, ErrDuplicateAdapterType)
		}
		byType[adapterType] = factory
	}
	types := make([]Type, 0, len(byType))
	for adapterType := range byType {
		types = append(types, adapterType)
	}
	sort.Slice(types, func(left, right int) bool { return types[left] < types[right] })
	return &Registry{factories: byType, types: types}, nil
}

// Types returns a sorted defensive copy of every explicitly registered type.
func (registry *Registry) Types() []Type {
	if registry == nil {
		return nil
	}
	return append([]Type(nil), registry.types...)
}

// Has reports whether an exact canonical type is registered.
func (registry *Registry) Has(adapterType Type) bool {
	if registry == nil {
		return false
	}
	_, exists := registry.factories[adapterType]
	return exists
}

// Resolve selects one factory by exact type. It never falls back to a default.
func (registry *Registry) Resolve(adapterType Type) (Factory, error) {
	if registry == nil || !typePattern.MatchString(string(adapterType)) {
		return nil, fmt.Errorf("type %q: %w", adapterType, ErrUnknownAdapterType)
	}
	factory, exists := registry.factories[adapterType]
	if !exists {
		return nil, fmt.Errorf("type %q: %w", adapterType, ErrUnknownAdapterType)
	}
	return factory, nil
}

// ValidateStartup rejects malformed, duplicate, or unknown Provider adapter types
// before a process marks itself ready.
func (registry *Registry) ValidateStartup(providers []catalog.Provider) error {
	return registry.validateProviders(ValidationStartup, providers)
}

// ValidatePublication rejects the entire candidate Provider catalog atomically.
func (registry *Registry) ValidatePublication(providers []catalog.Provider) error {
	return registry.validateProviders(ValidationPublication, providers)
}

// Build validates a Provider/Deployment pair, selects its exact factory, and
// verifies the returned adapter identity before exposing it to the data plane.
func (registry *Registry) Build(
	ctx context.Context,
	provider catalog.Provider,
	deployment catalog.Deployment,
) (Adapter, error) {
	if ctx == nil {
		return nil, errors.New("provider adapter build context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("provider adapter build cancelled: %w", err)
	}
	if err := provider.Validate(); err != nil {
		return nil, fmt.Errorf("validate provider for adapter build: %w", err)
	}
	if err := deployment.Validate(); err != nil {
		return nil, fmt.Errorf("validate deployment for adapter build: %w", err)
	}
	if deployment.ProviderID != provider.ID {
		return nil, errors.New("deployment does not belong to provider")
	}
	adapterType := Type(provider.AdapterType)
	factory, err := registry.Resolve(adapterType)
	if err != nil {
		return nil, err
	}
	if factory.Type() != adapterType {
		return nil, &FactoryError{AdapterType: adapterType, DeploymentID: deployment.ID, cause: ErrAdapterTypeMismatch}
	}
	built, err := factory.New(ctx, provider, cloneDeployment(deployment))
	if err != nil {
		return nil, &FactoryError{AdapterType: adapterType, DeploymentID: deployment.ID, cause: err}
	}
	if isNilAdapter(built) {
		return nil, &FactoryError{AdapterType: adapterType, DeploymentID: deployment.ID, cause: errors.New("factory returned a nil adapter")}
	}
	if built.Type() != adapterType {
		return nil, &FactoryError{AdapterType: adapterType, DeploymentID: deployment.ID, cause: ErrAdapterTypeMismatch}
	}
	return built, nil
}

func (registry *Registry) validateProviders(phase ValidationPhase, providers []catalog.Provider) error {
	problems := make([]ReferenceProblem, 0)
	seenID := make(map[string]struct{}, len(providers))
	seenCode := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		adapterType := Type(provider.AdapterType)
		base := ReferenceProblem{ProviderID: provider.ID, ProviderCode: provider.Code, AdapterType: adapterType}
		if err := provider.Validate(); err != nil {
			problem := base
			problem.Code = "invalid_provider"
			problems = append(problems, problem)
		}
		if _, exists := seenID[provider.ID]; exists {
			problem := base
			problem.Code = "duplicate_provider_id"
			problems = append(problems, problem)
		}
		seenID[provider.ID] = struct{}{}
		canonicalCode := strings.ToLower(provider.Code)
		if _, exists := seenCode[canonicalCode]; exists {
			problem := base
			problem.Code = "duplicate_provider_code"
			problems = append(problems, problem)
		}
		seenCode[canonicalCode] = struct{}{}
		if !registry.Has(adapterType) {
			problem := base
			problem.Code = "unknown_adapter_type"
			problems = append(problems, problem)
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Slice(problems, func(left, right int) bool {
		leftKey := []string{problems[left].ProviderID, problems[left].ProviderCode, string(problems[left].AdapterType), problems[left].Code}
		rightKey := []string{problems[right].ProviderID, problems[right].ProviderCode, string(problems[right].AdapterType), problems[right].Code}
		return slices.Compare(leftKey, rightKey) < 0
	})
	return &ReferenceValidationError{phase: phase, problems: problems}
}

func cloneDeployment(deployment catalog.Deployment) catalog.Deployment {
	cloned := deployment
	if deployment.SecretReferenceID != nil {
		secretReferenceID := *deployment.SecretReferenceID
		cloned.SecretReferenceID = &secretReferenceID
	}
	return cloned
}

func isNilFactory(factory Factory) bool {
	return isNilInterface(factory)
}

func isNilAdapter(adapter Adapter) bool {
	return isNilInterface(adapter)
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	kind := reflected.Kind()
	if kind == reflect.Chan || kind == reflect.Func || kind == reflect.Interface ||
		kind == reflect.Map || kind == reflect.Pointer || kind == reflect.Slice {
		return reflected.IsNil()
	}
	return false
}
