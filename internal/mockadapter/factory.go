// Package mockadapter implements the provider adapter contract against the
// deterministic local Mock Provider HTTP protocol.
package mockadapter

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/provideradapter"
)

const (
	// Type is the explicit registry key for the deterministic Mock Adapter.
	Type provideradapter.Type = "mock"

	// ScenarioNormal returns one ordinary completion.
	ScenarioNormal = "normal"
	// ScenarioSSE returns a valid event stream.
	ScenarioSSE = "sse"
	// ScenarioFixedUsage returns cache/reasoning-aware fixed usage.
	ScenarioFixedUsage = "fixed-usage"
	// ScenarioCachedUsage returns a non-zero cache-read count.
	ScenarioCachedUsage = "cached-usage"
	// ScenarioToolCall returns one deterministic function call.
	ScenarioToolCall = "tool-call"
	// ScenarioDelay delays a response until the configured duration or cancellation.
	ScenarioDelay = "delay"
	// ScenarioRateLimit returns HTTP 429.
	ScenarioRateLimit = "rate-limit"
	// ScenarioServerError returns HTTP 503.
	ScenarioServerError = "server-error"
	// ScenarioDisconnect sends a truncated JSON response.
	ScenarioDisconnect = "disconnect"
	// ScenarioMalformedChunk sends an invalid SSE JSON event.
	ScenarioMalformedChunk = "malformed-chunk"
)

var (
	// ErrUnsupportedParameter means a normalized input cannot be expressed safely.
	ErrUnsupportedParameter = errors.New("mock adapter parameter is unsupported")
	// ErrProtocol means a provider response violated the bounded Mock protocol.
	ErrProtocol = errors.New("mock provider protocol violation")
	// ErrRequestTooLarge means the encoded upstream request exceeds one MiB.
	ErrRequestTooLarge = errors.New("mock adapter request exceeds size limit")
	// ErrResponseTooLarge means a JSON response or stream event exceeds its limit.
	ErrResponseTooLarge = errors.New("mock provider response exceeds size limit")
	// ErrUsageEstimationUnavailable prevents an unverified local estimate from being treated as billed usage.
	ErrUsageEstimationUnavailable = errors.New("mock adapter usage estimate is unavailable")
)

// FactoryOptions contains deterministic construction dependencies.
type FactoryOptions struct {
	Now func() time.Time
}

// Factory creates local-loopback, deployment-scoped Mock Adapters.
type Factory struct {
	now func() time.Time
}

// NewFactory constructs a registry-ready Mock Adapter factory.
func NewFactory(options FactoryOptions) (*Factory, error) {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	if now().IsZero() {
		return nil, errors.New("mock adapter clock must return a non-zero time")
	}
	return &Factory{now: now}, nil
}

// Type returns the exact immutable registry key.
func (*Factory) Type() provideradapter.Type {
	return Type
}

// New validates the local-only deployment and creates one Adapter.
func (factory *Factory) New(
	ctx context.Context,
	provider catalog.Provider,
	deployment catalog.Deployment,
) (provideradapter.Adapter, error) {
	if factory == nil || factory.now == nil {
		return nil, errors.New("mock adapter factory is not initialized")
	}
	if ctx == nil {
		return nil, errors.New("mock adapter factory context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("mock adapter construction cancelled: %w", err)
	}
	if err := provider.Validate(); err != nil {
		return nil, fmt.Errorf("validate mock provider catalog record: %w", err)
	}
	if err := deployment.Validate(); err != nil {
		return nil, fmt.Errorf("validate mock deployment catalog record: %w", err)
	}
	if provider.AdapterType != string(Type) {
		return nil, errors.New("provider does not declare the mock adapter type")
	}
	if provider.Status != catalog.StatusActive || deployment.Status != catalog.StatusActive {
		return nil, errors.New("mock provider and deployment must be active")
	}
	if deployment.ProviderID != provider.ID {
		return nil, errors.New("mock deployment does not belong to provider")
	}
	if !deployment.Capabilities.Chat {
		return nil, errors.New("mock deployment must declare chat capability")
	}
	if deployment.SecretReferenceID != nil {
		return nil, errors.New("mock deployment must not declare a provider secret reference")
	}
	endpoint, err := parseLocalEndpoint(deployment.EndpointURL)
	if err != nil {
		return nil, err
	}
	return &mockAdapter{
		endpoint:      endpoint,
		physicalModel: deployment.PhysicalModel,
		capabilities:  deployment.Capabilities,
		now:           factory.now,
	}, nil
}

type mockAdapter struct {
	endpoint      *url.URL
	physicalModel string
	capabilities  catalog.CapabilitySet
	now           func() time.Time
}

// Type returns the exact registry identity.
func (*mockAdapter) Type() provideradapter.Type {
	return Type
}

// Capabilities returns the immutable catalog declaration bound at construction.
func (mock *mockAdapter) Capabilities(context.Context) catalog.CapabilitySet {
	if mock == nil {
		return catalog.CapabilitySet{}
	}
	return mock.capabilities
}

// EstimateUsage deliberately refuses to invent a tokenizer-derived billed fact.
func (*mockAdapter) EstimateUsage(context.Context, adapter.NormalizedRequest) (adapter.NormalizedUsage, error) {
	return adapter.NormalizedUsage{}, ErrUsageEstimationUnavailable
}

func parseLocalEndpoint(raw string) (*url.URL, error) {
	endpoint, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse mock deployment endpoint: %w", err)
	}
	if endpoint.Scheme != "http" {
		return nil, errors.New("mock deployment endpoint must use http")
	}
	ip := net.ParseIP(endpoint.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("mock deployment endpoint must use an explicit loopback IP")
	}
	path := strings.TrimSuffix(endpoint.EscapedPath(), "/")
	if path != "" && path != "/v1" && path != "/v1/chat/completions" {
		return nil, errors.New("mock deployment endpoint path must be empty, /v1, or /v1/chat/completions")
	}
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/")
	endpoint.RawPath = ""
	return endpoint, nil
}

var _ provideradapter.Factory = (*Factory)(nil)
var _ provideradapter.Adapter = (*mockAdapter)(nil)
