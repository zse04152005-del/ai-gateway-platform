// Package openaiadapter implements the official OpenAI Chat Completions HTTP protocol.
package openaiadapter

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
	"github.com/zse04152005-del/ai-gateway-platform/internal/providersecret"
)

const (
	// Type is the immutable registry key for the official OpenAI protocol.
	Type             provideradapter.Type = "openai"
	defaultUserAgent string               = "ai-gateway-platform/openai-adapter"
)

var (
	// ErrUnsupportedParameter means a normalized input cannot be represented without changing semantics.
	ErrUnsupportedParameter = errors.New("openai adapter parameter is unsupported")
	// ErrProtocol means the upstream response violated the bounded OpenAI protocol contract.
	ErrProtocol = errors.New("openai protocol violation")
	// ErrRequestTooLarge protects the upstream transport from oversized encoded requests.
	ErrRequestTooLarge = errors.New("openai adapter request exceeds size limit")
	// ErrResponseTooLarge protects response and SSE parsing from unbounded provider data.
	ErrResponseTooLarge = errors.New("openai response exceeds size limit")
	// ErrUsageEstimationUnavailable prevents an unverified estimate from becoming billing evidence.
	ErrUsageEstimationUnavailable = errors.New("openai adapter usage estimate is unavailable")
	// ErrCredentialUnavailable is a stable secret-resolution failure that never includes credential material.
	ErrCredentialUnavailable = errors.New("openai credential is unavailable")
)

// SecretResolver resolves a provider-scoped secret reference at request construction time.
// Implementations must return a caller-owned byte slice and safe errors.
type SecretResolver interface {
	Resolve(context.Context, providersecret.Locator) ([]byte, error)
}

// FactoryOptions contains construction dependencies. Insecure HTTP is reserved
// for numeric loopback test fixtures and is never enabled implicitly.
type FactoryOptions struct {
	Secrets               SecretResolver
	Now                   func() time.Time
	UserAgent             string
	AllowInsecureLoopback bool
}

// Factory creates deployment-scoped OpenAI adapters.
type Factory struct {
	secrets               SecretResolver
	now                   func() time.Time
	userAgent             string
	allowInsecureLoopback bool
}

// NewFactory constructs a registry-ready OpenAI factory.
func NewFactory(options FactoryOptions) (*Factory, error) {
	if options.Secrets == nil {
		return nil, errors.New("openai secret resolver must not be nil")
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	if now().IsZero() {
		return nil, errors.New("openai adapter clock must return a non-zero time")
	}
	userAgent := strings.TrimSpace(options.UserAgent)
	if userAgent == "" {
		userAgent = defaultUserAgent
	}
	if len(userAgent) > 256 || strings.ContainsAny(userAgent, "\r\n") {
		return nil, errors.New("openai adapter user agent is invalid")
	}
	return &Factory{
		secrets: options.Secrets, now: now, userAgent: userAgent,
		allowInsecureLoopback: options.AllowInsecureLoopback,
	}, nil
}

// Type returns the exact registry identity.
func (*Factory) Type() provideradapter.Type {
	return Type
}

// New validates catalog identity, credentials, capabilities, and endpoint policy.
func (factory *Factory) New(
	ctx context.Context,
	provider catalog.Provider,
	deployment catalog.Deployment,
) (provideradapter.Adapter, error) {
	if factory == nil || factory.secrets == nil || factory.now == nil {
		return nil, errors.New("openai adapter factory is not initialized")
	}
	if ctx == nil {
		return nil, errors.New("openai adapter factory context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("openai adapter construction cancelled: %w", err)
	}
	if err := provider.Validate(); err != nil {
		return nil, fmt.Errorf("validate openai provider catalog record: %w", err)
	}
	if err := deployment.Validate(); err != nil {
		return nil, fmt.Errorf("validate openai deployment catalog record: %w", err)
	}
	if provider.AdapterType != string(Type) {
		return nil, errors.New("provider does not declare the openai adapter type")
	}
	if provider.Status != catalog.StatusActive || deployment.Status != catalog.StatusActive {
		return nil, errors.New("openai provider and deployment must be active")
	}
	if deployment.ProviderID != provider.ID {
		return nil, errors.New("openai deployment does not belong to provider")
	}
	if !deployment.Capabilities.Chat {
		return nil, errors.New("openai deployment must declare chat capability")
	}
	if deployment.SecretReferenceID == nil {
		return nil, errors.New("openai deployment must declare a provider secret reference")
	}
	endpoint, err := parseEndpoint(deployment.EndpointURL, factory.allowInsecureLoopback)
	if err != nil {
		return nil, err
	}
	return &openAIAdapter{
		endpoint: endpoint, physicalModel: deployment.PhysicalModel,
		capabilities: deployment.Capabilities, secrets: factory.secrets,
		secretLocator: providersecret.Locator{ProviderID: provider.ID, ID: *deployment.SecretReferenceID},
		now:           factory.now, userAgent: factory.userAgent,
	}, nil
}

type openAIAdapter struct {
	endpoint      *url.URL
	physicalModel string
	capabilities  catalog.CapabilitySet
	secrets       SecretResolver
	secretLocator providersecret.Locator
	now           func() time.Time
	userAgent     string
}

// Type returns the exact registry identity.
func (*openAIAdapter) Type() provideradapter.Type {
	return Type
}

// Capabilities returns the immutable deployment declaration.
func (openAI *openAIAdapter) Capabilities(context.Context) catalog.CapabilitySet {
	if openAI == nil {
		return catalog.CapabilitySet{}
	}
	return openAI.capabilities
}

// EstimateUsage refuses to manufacture provider billing evidence locally.
func (*openAIAdapter) EstimateUsage(context.Context, adapter.NormalizedRequest) (adapter.NormalizedUsage, error) {
	return adapter.NormalizedUsage{}, ErrUsageEstimationUnavailable
}

func parseEndpoint(raw string, allowInsecureLoopback bool) (*url.URL, error) {
	endpoint, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse openai deployment endpoint: %w", err)
	}
	if endpoint.Opaque != "" || endpoint.User != nil || endpoint.Host == "" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("openai deployment endpoint must be an absolute URL without userinfo, query, or fragment")
	}
	switch endpoint.Scheme {
	case "https":
	case "http":
		ip := net.ParseIP(endpoint.Hostname())
		if !allowInsecureLoopback || ip == nil || !ip.IsLoopback() {
			return nil, errors.New("openai deployment endpoint must use https")
		}
	default:
		return nil, errors.New("openai deployment endpoint must use https")
	}
	path := strings.TrimSuffix(endpoint.EscapedPath(), "/")
	if path != "" && path != "/v1" && path != "/v1/chat/completions" {
		return nil, errors.New("openai deployment endpoint path must be empty, /v1, or /v1/chat/completions")
	}
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/")
	endpoint.RawPath = ""
	return endpoint, nil
}

var _ provideradapter.Factory = (*Factory)(nil)
var _ provideradapter.Adapter = (*openAIAdapter)(nil)
