// Package provideradapter defines the provider adapter runtime port and its
// immutable, explicitly registered factory catalog.
package provideradapter

import (
	"context"
	"net/http"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
)

// Type is the canonical catalog identifier for one provider protocol family.
type Type string

// Adapter converts between provider HTTP protocols and normalized facts.
// Implementations are created for one validated Provider/Deployment pair.
type Adapter interface {
	Type() Type
	Capabilities(ctx context.Context) catalog.CapabilitySet
	BuildRequest(ctx context.Context, input adapter.NormalizedRequest) (*http.Request, error)
	ParseResponse(ctx context.Context, response *http.Response) (adapter.NormalizedResponse, error)
	OpenStream(ctx context.Context, response *http.Response) (ChunkStream, error)
	NormalizeError(ctx context.Context, response *http.Response, body []byte) adapter.NormalizedError
	EstimateUsage(ctx context.Context, input adapter.NormalizedRequest) (adapter.NormalizedUsage, error)
}

// ChunkStream yields one validated normalized stream fact at a time.
type ChunkStream interface {
	Next(ctx context.Context) (adapter.NormalizedChunk, error)
	Close() error
}

// Factory creates a deployment-scoped adapter. Secrets and clients are injected
// into the factory itself; they never enter Registry metadata or error strings.
type Factory interface {
	Type() Type
	New(ctx context.Context, provider catalog.Provider, deployment catalog.Deployment) (Adapter, error)
}
