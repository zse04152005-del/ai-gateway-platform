package provideradapter_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	normalized "github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/provideradapter"
)

func TestNewRegistryIsSortedImmutableAndExact(t *testing.T) {
	t.Parallel()

	registry, err := provideradapter.NewRegistry(
		&fakeFactory{adapterType: "zeta_protocol"},
		&fakeFactory{adapterType: "alpha_protocol"},
	)
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	want := []provideradapter.Type{"alpha_protocol", "zeta_protocol"}
	if got := registry.Types(); !reflect.DeepEqual(got, want) {
		t.Fatalf("types = %v, want %v", got, want)
	}
	types := registry.Types()
	types[0] = "mutated"
	if got := registry.Types(); !reflect.DeepEqual(got, want) {
		t.Fatalf("registry types alias caller: %v", got)
	}
	if !registry.Has("alpha_protocol") || registry.Has("ALPHA_PROTOCOL") || registry.Has("missing") {
		t.Fatal("registry type lookup is not exact")
	}
	factory, err := registry.Resolve("zeta_protocol")
	if err != nil || factory.Type() != "zeta_protocol" {
		t.Fatalf("resolve registered factory: factory=%v err=%v", factory, err)
	}
	if _, err := registry.Resolve("missing"); !errors.Is(err, provideradapter.ErrUnknownAdapterType) {
		t.Fatalf("resolve unknown error = %v", err)
	}
	if _, err := registry.Resolve("Bad Type"); !errors.Is(err, provideradapter.ErrUnknownAdapterType) {
		t.Fatalf("resolve invalid error = %v", err)
	}
}

func TestNewRegistryRejectsInvalidAndDuplicateFactories(t *testing.T) {
	t.Parallel()

	var typedNil *fakeFactory
	tests := []struct {
		name      string
		factories []provideradapter.Factory
		want      error
	}{
		{"nil", []provideradapter.Factory{nil}, provideradapter.ErrInvalidFactory},
		{"typed nil", []provideradapter.Factory{typedNil}, provideradapter.ErrInvalidFactory},
		{"invalid type", []provideradapter.Factory{&fakeFactory{adapterType: "Bad Type"}}, provideradapter.ErrInvalidFactory},
		{"duplicate", []provideradapter.Factory{&fakeFactory{adapterType: "mock"}, &fakeFactory{adapterType: "mock"}}, provideradapter.ErrDuplicateAdapterType},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := provideradapter.NewRegistry(test.factories...); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want errors.Is %v", err, test.want)
			}
		})
	}

	empty, err := provideradapter.NewRegistry()
	if err != nil || len(empty.Types()) != 0 {
		t.Fatalf("empty explicit registry: types=%v err=%v", empty.Types(), err)
	}
}

func TestRegistryStartupAndPublicationValidation(t *testing.T) {
	t.Parallel()

	registry, err := provideradapter.NewRegistry(&fakeFactory{adapterType: "mock"})
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	known := validProvider("11111111-1111-4111-8111-111111111111", "known-provider", "mock")
	if err := registry.ValidateStartup([]catalog.Provider{known}); err != nil {
		t.Fatalf("validate known startup provider: %v", err)
	}
	if err := registry.ValidatePublication(nil); err != nil {
		t.Fatalf("empty publication should be valid: %v", err)
	}

	unknown := validProvider("22222222-2222-4222-8222-222222222222", "future-provider", "future_protocol")
	assertReferenceFailure(t, registry.ValidateStartup([]catalog.Provider{unknown}), provideradapter.ValidationStartup, provideradapter.ErrUnknownAdapterType)
	assertReferenceFailure(t, registry.ValidatePublication([]catalog.Provider{unknown}), provideradapter.ValidationPublication, provideradapter.ErrUnknownAdapterType)
}

func TestRegistryPublicationAggregatesInvalidDuplicateAndUnknownReferences(t *testing.T) {
	t.Parallel()

	registry, err := provideradapter.NewRegistry(&fakeFactory{adapterType: "mock"})
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	duplicate := validProvider("11111111-1111-4111-8111-111111111111", "duplicate-provider", "mock")
	invalid := validProvider("22222222-2222-4222-8222-222222222222", "invalid-provider", "future_protocol")
	invalid.Name = " padded "
	err = registry.ValidatePublication([]catalog.Provider{duplicate, duplicate, invalid})
	if !errors.Is(err, provideradapter.ErrInvalidProviderReference) || !errors.Is(err, provideradapter.ErrUnknownAdapterType) {
		t.Fatalf("aggregate errors.Is mismatch: %v", err)
	}
	var validationError *provideradapter.ReferenceValidationError
	if !errors.As(err, &validationError) {
		t.Fatalf("expected ReferenceValidationError, got %T", err)
	}
	if validationError.Phase() != provideradapter.ValidationPublication {
		t.Fatalf("phase = %q", validationError.Phase())
	}
	problems := validationError.Problems()
	codes := make([]string, len(problems))
	for index := range problems {
		codes[index] = problems[index].Code
	}
	for _, want := range []string{"duplicate_provider_code", "duplicate_provider_id", "invalid_provider", "unknown_adapter_type"} {
		if !contains(codes, want) {
			t.Fatalf("problems %v missing %q", codes, want)
		}
	}
	problems[0].Code = "mutated"
	if validationError.Problems()[0].Code == "mutated" {
		t.Fatal("validation problems alias caller")
	}
	if strings.Contains(err.Error(), invalid.Name) || strings.Contains(err.Error(), invalid.Code) {
		t.Fatalf("aggregate error string exposes record values: %s", err)
	}
	if (*provideradapter.ReferenceValidationError)(nil).Error() != "<nil>" {
		t.Fatal("nil reference validation error string changed")
	}
}

func TestRegistryBuildSelectsExactFactoryAndDefensivelyCopiesDeployment(t *testing.T) {
	t.Parallel()

	factory := &fakeFactory{adapterType: "mock"}
	factory.newAdapter = &fakeAdapter{adapterType: "mock"}
	factory.mutateSecretReference = true
	registry, err := provideradapter.NewRegistry(factory)
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	provider := validProvider("11111111-1111-4111-8111-111111111111", "mock-provider", "mock")
	deployment := validDeployment(provider.ID)
	originalReference := *deployment.SecretReferenceID
	built, err := registry.Build(context.Background(), provider, deployment)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if built.Type() != "mock" || factory.calls.Load() != 1 {
		t.Fatalf("unexpected built adapter or call count: type=%q calls=%d", built.Type(), factory.calls.Load())
	}
	if *deployment.SecretReferenceID != originalReference {
		t.Fatal("factory mutated caller deployment secret reference")
	}
}

func TestRegistryBuildRejectsInvalidInputsBeforeFactory(t *testing.T) {
	t.Parallel()

	factory := &fakeFactory{adapterType: "mock", newAdapter: &fakeAdapter{adapterType: "mock"}}
	registry, err := provideradapter.NewRegistry(factory)
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	provider := validProvider("11111111-1111-4111-8111-111111111111", "mock-provider", "mock")
	deployment := validDeployment(provider.ID)

	var nilContext context.Context
	if _, err := registry.Build(nilContext, provider, deployment); err == nil {
		t.Fatal("nil context must fail")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := registry.Build(cancelled, provider, deployment); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled error = %v", err)
	}
	invalidProvider := provider
	invalidProvider.Name = " padded "
	if _, err := registry.Build(context.Background(), invalidProvider, deployment); err == nil {
		t.Fatal("invalid provider must fail")
	}
	invalidDeployment := deployment
	invalidDeployment.EndpointURL = "not-a-url"
	if _, err := registry.Build(context.Background(), provider, invalidDeployment); err == nil {
		t.Fatal("invalid deployment must fail")
	}
	otherDeployment := validDeployment("22222222-2222-4222-8222-222222222222")
	if _, err := registry.Build(context.Background(), provider, otherDeployment); err == nil {
		t.Fatal("cross-provider deployment must fail")
	}
	unknownProvider := validProvider("33333333-3333-4333-8333-333333333333", "unknown-provider", "future")
	unknownDeployment := validDeployment(unknownProvider.ID)
	if _, err := registry.Build(context.Background(), unknownProvider, unknownDeployment); !errors.Is(err, provideradapter.ErrUnknownAdapterType) {
		t.Fatalf("unknown build error = %v", err)
	}
	if factory.calls.Load() != 0 {
		t.Fatalf("factory called for rejected inputs: %d", factory.calls.Load())
	}
}

func TestRegistryBuildSafelyWrapsFactoryFailures(t *testing.T) {
	t.Parallel()

	privateCause := errors.New("upstream credential private-marker")
	provider := validProvider("11111111-1111-4111-8111-111111111111", "mock-provider", "mock")
	deployment := validDeployment(provider.ID)
	tests := []struct {
		name    string
		factory *fakeFactory
		want    error
	}{
		{"private cause", &fakeFactory{adapterType: "mock", newError: privateCause}, privateCause},
		{"nil adapter", &fakeFactory{adapterType: "mock"}, provideradapter.ErrFactoryFailed},
		{"type mismatch", &fakeFactory{adapterType: "mock", newAdapter: &fakeAdapter{adapterType: "other"}}, provideradapter.ErrAdapterTypeMismatch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			registry, err := provideradapter.NewRegistry(test.factory)
			if err != nil {
				t.Fatalf("new registry: %v", err)
			}
			_, err = registry.Build(context.Background(), provider, deployment)
			if !errors.Is(err, provideradapter.ErrFactoryFailed) || !errors.Is(err, test.want) {
				t.Fatalf("build error = %v, want factory failed and %v", err, test.want)
			}
			if strings.Contains(err.Error(), "private-marker") {
				t.Fatalf("safe factory error leaked private cause: %s", err)
			}
			var factoryError *provideradapter.FactoryError
			if !errors.As(err, &factoryError) || factoryError.AdapterType != "mock" || factoryError.DeploymentID != deployment.ID {
				t.Fatalf("factory diagnostic = %#v", factoryError)
			}
		})
	}
	if (*provideradapter.FactoryError)(nil).Error() != "<nil>" {
		t.Fatal("nil factory error string changed")
	}
}

func TestRegistryBuildRejectsFactoryIdentityMutation(t *testing.T) {
	t.Parallel()

	factory := &fakeFactory{adapterType: "mock", newAdapter: &fakeAdapter{adapterType: "mock"}}
	registry, err := provideradapter.NewRegistry(factory)
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	factory.adapterType = "mutated"
	provider := validProvider("11111111-1111-4111-8111-111111111111", "mock-provider", "mock")
	_, err = registry.Build(context.Background(), provider, validDeployment(provider.ID))
	if !errors.Is(err, provideradapter.ErrFactoryFailed) || !errors.Is(err, provideradapter.ErrAdapterTypeMismatch) {
		t.Fatalf("mutated factory error = %v", err)
	}
	if factory.calls.Load() != 0 {
		t.Fatal("mutated factory was invoked")
	}
}

func TestRegistryConcurrentBuildAndResolve(t *testing.T) {
	t.Parallel()

	factory := &fakeFactory{adapterType: "mock", newAdapter: &fakeAdapter{adapterType: "mock"}}
	registry, err := provideradapter.NewRegistry(factory)
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	provider := validProvider("11111111-1111-4111-8111-111111111111", "mock-provider", "mock")
	deployment := validDeployment(provider.ID)
	const workers = 64
	errorsFound := make(chan error, workers)
	var waitGroup sync.WaitGroup
	waitGroup.Add(workers)
	for range workers {
		go func() {
			defer waitGroup.Done()
			if _, resolveErr := registry.Resolve("mock"); resolveErr != nil {
				errorsFound <- resolveErr
				return
			}
			if _, buildErr := registry.Build(context.Background(), provider, deployment); buildErr != nil {
				errorsFound <- buildErr
			}
		}()
	}
	waitGroup.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Errorf("concurrent operation: %v", err)
	}
	if factory.calls.Load() != workers {
		t.Fatalf("factory calls = %d, want %d", factory.calls.Load(), workers)
	}
}

type fakeFactory struct {
	adapterType           provideradapter.Type
	newAdapter            provideradapter.Adapter
	newError              error
	mutateSecretReference bool
	calls                 atomic.Int64
}

func (factory *fakeFactory) Type() provideradapter.Type {
	return factory.adapterType
}

func (factory *fakeFactory) New(
	_ context.Context,
	_ catalog.Provider,
	deployment catalog.Deployment,
) (provideradapter.Adapter, error) {
	factory.calls.Add(1)
	if factory.mutateSecretReference && deployment.SecretReferenceID != nil {
		*deployment.SecretReferenceID = "99999999-9999-4999-8999-999999999999"
	}
	return factory.newAdapter, factory.newError
}

type fakeAdapter struct {
	adapterType provideradapter.Type
}

func (fake *fakeAdapter) Type() provideradapter.Type {
	return fake.adapterType
}

func (*fakeAdapter) Capabilities(context.Context) catalog.CapabilitySet {
	return catalog.CapabilitySet{}
}

func (*fakeAdapter) BuildRequest(context.Context, normalized.NormalizedRequest) (*http.Request, error) {
	return nil, errors.New("not implemented in registry fixture")
}

func (*fakeAdapter) ParseResponse(context.Context, *http.Response) (normalized.NormalizedResponse, error) {
	return normalized.NormalizedResponse{}, errors.New("not implemented in registry fixture")
}

func (*fakeAdapter) OpenStream(context.Context, *http.Response) (provideradapter.ChunkStream, error) {
	return nil, errors.New("not implemented in registry fixture")
}

func (*fakeAdapter) NormalizeError(context.Context, *http.Response, []byte) normalized.NormalizedError {
	return normalized.NormalizedError{}
}

func (*fakeAdapter) EstimateUsage(context.Context, normalized.NormalizedRequest) (normalized.NormalizedUsage, error) {
	return normalized.NormalizedUsage{}, nil
}

func validProvider(id, code, adapterType string) catalog.Provider {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	return catalog.Provider{
		ID: id, Code: code, Name: "Provider " + code, AdapterType: adapterType,
		Status: catalog.StatusActive, Version: 1,
		CreatedAt: now, CreatedBy: "test", UpdatedAt: now, UpdatedBy: "test",
	}
}

func validDeployment(providerID string) catalog.Deployment {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	secretReferenceID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	return catalog.Deployment{
		ID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", ProviderID: providerID,
		Code: "primary", PhysicalModel: "model-v1", EndpointURL: "https://provider.invalid/v1", Region: "cn-east",
		Capabilities: catalog.CapabilitySet{
			Chat: true, Stream: true, MaxContextTokens: 8192, MaxOutputTokens: 2048,
			DataRetentionMode: catalog.RetentionNoTraining, ProviderProtocolVersion: "v1",
		},
		SecretReferenceID: &secretReferenceID,
		Status:            catalog.StatusActive, Version: 1,
		CreatedAt: now, CreatedBy: "test", UpdatedAt: now, UpdatedBy: "test",
	}
}

func assertReferenceFailure(t *testing.T, err error, phase provideradapter.ValidationPhase, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("error = %v, want errors.Is %v", err, target)
	}
	var validationError *provideradapter.ReferenceValidationError
	if !errors.As(err, &validationError) {
		t.Fatalf("expected ReferenceValidationError, got %T", err)
	}
	if validationError.Phase() != phase || len(validationError.Problems()) != 1 {
		t.Fatalf("phase/problems = %q/%v", validationError.Phase(), validationError.Problems())
	}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

var _ provideradapter.Factory = (*fakeFactory)(nil)
var _ provideradapter.Adapter = (*fakeAdapter)(nil)
var _ io.Closer = (provideradapter.ChunkStream)(nil)
