// Package tokenestimate provides bounded, model-bound local usage estimates.
// Its counts are always marked estimated and never represent provider billing
// facts, even when a tokenizer happens to resemble a provider implementation.
package tokenestimate

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/limitpolicy"
)

const (
	// DefaultMaximumEntries bounds the process-local content-digest cache.
	DefaultMaximumEntries = 4096
	maximumCacheEntries   = 1_000_000
	defaultAlgorithm      = "utf8-byte-bound"
	defaultAlgorithmV1    = "v1"
)

var (
	// ErrInvalid means an estimator dependency, model, or normalized fact is unsafe.
	ErrInvalid = errors.New("local token estimate input is invalid")
	// ErrUnavailable means the bounded tokenizer cannot produce a safe estimate.
	ErrUnavailable = errors.New("local token estimate is unavailable")
)

// Tokenizer counts a canonical content envelope. Implementations must be
// deterministic and safe for concurrent use.
type Tokenizer interface {
	Name() string
	Version() string
	Count(context.Context, []byte) (uint64, error)
}

// Options configures one process-local estimator and its finite digest cache.
type Options struct {
	MaximumEntries int
	Tokenizer      Tokenizer
}

// CacheStats exposes only aggregate cache behavior; keys are SHA-256 digests
// and no prompt or response content is retained.
type CacheStats struct {
	Entries int
	Hits    uint64
	Misses  uint64
}

// Estimator binds one explicitly named tokenizer to selected Deployment facts.
type Estimator struct {
	tokenizer Tokenizer
	cache     *digestCache
}

// New creates a bounded concurrent estimator. A nil Tokenizer selects the
// deliberately conservative UTF-8 byte-bound tokenizer.
func New(options Options) (*Estimator, error) {
	if options.MaximumEntries == 0 {
		options.MaximumEntries = DefaultMaximumEntries
	}
	if options.MaximumEntries < 1 || options.MaximumEntries > maximumCacheEntries {
		return nil, ErrInvalid
	}
	if options.Tokenizer == nil {
		options.Tokenizer = byteTokenizer{}
	}
	metadata := adapter.UsageEstimateMetadata{
		Estimated: true, Tokenizer: options.Tokenizer.Name(),
		TokenizerVersion: options.Tokenizer.Version(), PhysicalModel: "validation-model",
		DeploymentVersion: 1, ProviderProtocolVersion: "validation-v1",
	}
	if metadata.Validate() != nil {
		return nil, ErrInvalid
	}
	return &Estimator{
		tokenizer: options.Tokenizer,
		cache:     newDigestCache(options.MaximumEntries),
	}, nil
}

// EstimateInput returns a model-bound input count and immutable estimate metadata.
func (estimator *Estimator) EstimateInput(
	ctx context.Context,
	deployment catalog.Deployment,
	request adapter.NormalizedRequest,
) (uint64, adapter.UsageEstimateMetadata, error) {
	if estimator == nil || estimator.tokenizer == nil || estimator.cache == nil || ctx == nil ||
		deployment.Validate() != nil || request.Validate() != nil {
		return 0, adapter.UsageEstimateMetadata{}, ErrInvalid
	}
	metadata := estimator.metadata(deployment)
	if metadata.Validate() != nil {
		return 0, adapter.UsageEstimateMetadata{}, ErrInvalid
	}
	payload, err := json.Marshal(inputEnvelopeFrom(request.Clone()))
	if err != nil {
		return 0, adapter.UsageEstimateMetadata{}, fmt.Errorf("%w: encode input envelope: %w", ErrUnavailable, err)
	}
	tokens, err := estimator.count(ctx, metadata, "input", payload)
	if err != nil {
		return 0, adapter.UsageEstimateMetadata{}, err
	}
	maximumContext := uint64(deployment.Capabilities.MaxContextTokens) //nolint:gosec // catalog validation requires a positive int64.
	if tokens == 0 || tokens > maximumContext ||
		tokens > limitpolicy.MaximumValue {
		return 0, adapter.UsageEstimateMetadata{}, ErrUnavailable
	}
	return tokens, metadata, nil
}

// EstimateUsage returns input-only usage when response is nil and input/output
// usage otherwise. Complete is always false because local framing and future
// provider tokenizer changes remain outside this process's authority.
func (estimator *Estimator) EstimateUsage(
	ctx context.Context,
	deployment catalog.Deployment,
	request adapter.NormalizedRequest,
	response *adapter.NormalizedResponse,
) (adapter.NormalizedUsage, error) {
	inputTokens, metadata, err := estimator.EstimateInput(ctx, deployment, request)
	if err != nil {
		return adapter.NormalizedUsage{}, err
	}
	usage := adapter.NormalizedUsage{
		InputTokens: adapter.Tokens(int64(inputTokens)), //nolint:gosec // count is bounded by 2^53-1 above.
		Source:      adapter.UsageSourceEstimated,
		Complete:    false, Estimate: &metadata,
	}
	if response == nil {
		return usage, nil
	}
	if response.Validate() != nil {
		return adapter.NormalizedUsage{}, ErrInvalid
	}
	payload, err := json.Marshal(outputEnvelopeFrom(response.Clone()))
	if err != nil {
		return adapter.NormalizedUsage{}, fmt.Errorf("%w: encode output envelope: %w", ErrUnavailable, err)
	}
	outputTokens, err := estimator.count(ctx, metadata, "output", payload)
	if err != nil {
		return adapter.NormalizedUsage{}, err
	}
	maximumContext := uint64(deployment.Capabilities.MaxContextTokens) //nolint:gosec // catalog validation requires a positive int64.
	maximumOutput := uint64(deployment.Capabilities.MaxOutputTokens)   //nolint:gosec // catalog validation requires a positive int64.
	if outputTokens == 0 || outputTokens > maximumOutput ||
		outputTokens > limitpolicy.MaximumValue || inputTokens > maximumContext-outputTokens {
		return adapter.NormalizedUsage{}, ErrUnavailable
	}
	usage.OutputTokens = adapter.Tokens(int64(outputTokens)) //nolint:gosec // count is bounded by 2^53-1 above.
	if usage.Validate() != nil {
		return adapter.NormalizedUsage{}, ErrUnavailable
	}
	return usage, nil
}

// Stats returns aggregate LRU activity without exposing content digests.
func (estimator *Estimator) Stats() CacheStats {
	if estimator == nil || estimator.cache == nil {
		return CacheStats{}
	}
	return estimator.cache.stats()
}

func (estimator *Estimator) metadata(deployment catalog.Deployment) adapter.UsageEstimateMetadata {
	return adapter.UsageEstimateMetadata{
		Estimated: true, Tokenizer: estimator.tokenizer.Name(),
		TokenizerVersion: estimator.tokenizer.Version(), PhysicalModel: deployment.PhysicalModel,
		DeploymentVersion:       deployment.Version,
		ProviderProtocolVersion: deployment.Capabilities.ProviderProtocolVersion,
	}
}

func (estimator *Estimator) count(
	ctx context.Context,
	metadata adapter.UsageEstimateMetadata,
	direction string,
	payload []byte,
) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	keyMaterial, err := json.Marshal(struct {
		Metadata  adapter.UsageEstimateMetadata `json:"metadata"`
		Direction string                        `json:"direction"`
		Payload   json.RawMessage               `json:"payload"`
	}{Metadata: metadata, Direction: direction, Payload: payload})
	if err != nil {
		return 0, fmt.Errorf("%w: encode cache identity: %w", ErrUnavailable, err)
	}
	key := sha256.Sum256(keyMaterial)
	if tokens, ok := estimator.cache.get(key); ok {
		return tokens, nil
	}
	tokens, err := estimator.tokenizer.Count(ctx, payload)
	if err != nil {
		return 0, fmt.Errorf("%w: tokenizer: %w", ErrUnavailable, err)
	}
	if tokens == 0 || tokens > limitpolicy.MaximumValue {
		return 0, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	estimator.cache.put(key, tokens)
	return tokens, nil
}

type inputEnvelope struct {
	Messages        []adapter.Message        `json:"messages"`
	Temperature     *float64                 `json:"temperature,omitempty"`
	TopP            *float64                 `json:"top_p,omitempty"`
	MaxOutputTokens *int64                   `json:"max_output_tokens,omitempty"`
	Stop            []string                 `json:"stop,omitempty"`
	Tools           []adapter.ToolDefinition `json:"tools,omitempty"`
	ToolChoice      *adapter.ToolChoice      `json:"tool_choice,omitempty"`
	ResponseFormat  *adapter.ResponseFormat  `json:"response_format,omitempty"`
	ProviderOptions json.RawMessage          `json:"provider_options,omitempty"`
}

func inputEnvelopeFrom(request adapter.NormalizedRequest) inputEnvelope {
	return inputEnvelope{
		Messages: request.Messages, Temperature: request.Temperature, TopP: request.TopP,
		MaxOutputTokens: request.MaxOutputTokens, Stop: request.Stop, Tools: request.Tools,
		ToolChoice: request.ToolChoice, ResponseFormat: request.ResponseFormat,
		ProviderOptions: request.ProviderOptions,
	}
}

type outputEnvelope struct {
	Choices []adapter.NormalizedChoice `json:"choices"`
}

func outputEnvelopeFrom(response adapter.NormalizedResponse) outputEnvelope {
	return outputEnvelope{Choices: response.Choices}
}

// byteTokenizer intentionally treats every UTF-8 byte of the normalized JSON
// envelope as one local token. It is deterministic and conservative, but it is
// not advertised as any provider's BPE or billing tokenizer.
type byteTokenizer struct{}

func (byteTokenizer) Name() string    { return defaultAlgorithm }
func (byteTokenizer) Version() string { return defaultAlgorithmV1 }
func (byteTokenizer) Count(ctx context.Context, payload []byte) (uint64, error) {
	if ctx == nil || len(payload) == 0 {
		return 0, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return uint64(len(payload)), nil
}

type cacheItem struct {
	key    [sha256.Size]byte
	tokens uint64
}

type digestCache struct {
	mu      sync.Mutex
	maximum int
	items   map[[sha256.Size]byte]*list.Element
	order   *list.List
	hits    uint64
	misses  uint64
}

func newDigestCache(maximum int) *digestCache {
	return &digestCache{
		maximum: maximum, items: make(map[[sha256.Size]byte]*list.Element, maximum), order: list.New(),
	}
}

func (cache *digestCache) get(key [sha256.Size]byte) (uint64, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	element, ok := cache.items[key]
	if !ok {
		cache.misses++
		return 0, false
	}
	cache.order.MoveToFront(element)
	cache.hits++
	return element.Value.(cacheItem).tokens, true
}

func (cache *digestCache) put(key [sha256.Size]byte, tokens uint64) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if element, ok := cache.items[key]; ok {
		element.Value = cacheItem{key: key, tokens: tokens}
		cache.order.MoveToFront(element)
		return
	}
	cache.items[key] = cache.order.PushFront(cacheItem{key: key, tokens: tokens})
	if cache.order.Len() <= cache.maximum {
		return
	}
	oldest := cache.order.Back()
	delete(cache.items, oldest.Value.(cacheItem).key)
	cache.order.Remove(oldest)
}

func (cache *digestCache) stats() CacheStats {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return CacheStats{Entries: cache.order.Len(), Hits: cache.hits, Misses: cache.misses}
}
