package activehealth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/provideradapter"
)

const (
	probeHeaderName  = "X-AI-Gateway-Traffic-Class"
	probeHeaderValue = "active-health/v1"
)

// Registry constructs the exact deployment-scoped adapter.
type Registry interface {
	Build(context.Context, catalog.Provider, catalog.Deployment) (provideradapter.Adapter, error)
}

// HTTPClient is satisfied by a dedicated upstreamhttp.Client.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// ProbeResult is safe to aggregate and never contains provider bodies/errors.
type ProbeResult struct {
	Code    ResultCode
	Latency time.Duration
}

// Prober executes exactly one active probe.
type Prober interface {
	Probe(context.Context, catalog.HealthProbeTarget) ProbeResult
}

// AdapterProber sends one fixed one-token chat request through the real adapter
// using a client/pool that is separate from production traffic.
type AdapterProber struct {
	registry Registry
	client   HTTPClient
	now      func() time.Time
}

// NewAdapterProber validates isolated probe dependencies.
func NewAdapterProber(registry Registry, client HTTPClient, now func() time.Time) (*AdapterProber, error) {
	if registry == nil || client == nil || now == nil {
		return nil, errors.New("active health prober dependencies must not be nil")
	}
	return &AdapterProber{registry: registry, client: client, now: now}, nil
}

// Probe performs one bounded request. All private causes collapse into a finite
// result code before leaving this boundary.
func (prober *AdapterProber) Probe(ctx context.Context, target catalog.HealthProbeTarget) ProbeResult {
	startedAt := time.Time{}
	if prober != nil && prober.now != nil {
		startedAt = prober.now().UTC()
	}
	finish := func(code ResultCode) ProbeResult {
		latency := time.Duration(0)
		if prober != nil && prober.now != nil && !startedAt.IsZero() {
			latency = prober.now().UTC().Sub(startedAt)
			if latency < 0 {
				latency = 0
			}
			if latency > time.Minute {
				latency = time.Minute
			}
		}
		return ProbeResult{Code: code, Latency: latency}
	}
	if prober == nil || prober.registry == nil || prober.client == nil || prober.now == nil || ctx == nil {
		return finish(ResultAdapterUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return finish(classifyContext(ctx))
	}
	if err := target.Validate(); err != nil {
		return finish(ResultAdapterUnavailable)
	}
	built, err := prober.registry.Build(ctx, target.Provider, target.Deployment)
	if err != nil || built == nil {
		if ctx.Err() != nil {
			return finish(classifyContext(ctx))
		}
		return finish(ResultAdapterUnavailable)
	}
	maximumOutput := int64(1)
	temperature := float64(0)
	request := adapter.NormalizedRequest{
		RequestID:    "probe-" + target.Deployment.ID,
		LogicalModel: "active-health",
		Messages: []adapter.Message{{
			Role:  adapter.RoleUser,
			Parts: []adapter.ContentPart{{Kind: adapter.ContentText, Text: "ping"}},
		}},
		Temperature: &temperature, MaxOutputTokens: &maximumOutput,
	}
	upstreamRequest, err := built.BuildRequest(ctx, request)
	if err != nil || upstreamRequest == nil {
		if ctx.Err() != nil {
			return finish(classifyContext(ctx))
		}
		return finish(ResultAdapterUnavailable)
	}
	upstreamRequest.Header.Set(probeHeaderName, probeHeaderValue)
	response, err := prober.client.Do(upstreamRequest)
	if err != nil {
		if ctx.Err() != nil {
			return finish(classifyContext(ctx))
		}
		return finish(ResultTransportFailure)
	}
	if response == nil || response.Body == nil {
		return finish(ResultTransportFailure)
	}
	defer func() { _ = response.Body.Close() }()
	normalized, err := built.ParseResponse(ctx, response)
	if err != nil {
		if ctx.Err() != nil {
			return finish(classifyContext(ctx))
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			return finish(ResultProviderFailure)
		}
		return finish(ResultProtocolFailure)
	}
	if normalized.Validate() != nil || normalized.Model != target.Deployment.PhysicalModel {
		return finish(ResultProtocolFailure)
	}
	return finish(ResultSucceeded)
}

func classifyContext(ctx context.Context) ResultCode {
	if ctx != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ResultTimedOut
	}
	return ResultCancelled
}

var _ Prober = (*AdapterProber)(nil)
