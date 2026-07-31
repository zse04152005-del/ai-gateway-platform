package streaming

import (
	"errors"
	"sync"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
)

const maximumUsageEvents = 1_000_000

var (
	// ErrUsageAggregationInvalid means configuration or normalized input is unsafe.
	ErrUsageAggregationInvalid = errors.New("stream usage aggregation input is invalid")
	// ErrUsageAggregationOrder means stream Sequence did not increase monotonically.
	ErrUsageAggregationOrder = errors.New("stream usage event order is invalid")
	// ErrUsageAggregationRegression means cumulative Provider usage lost or decreased a known meter.
	ErrUsageAggregationRegression = errors.New("stream provider usage regressed")
	// ErrUsageAggregationLimit means the request-scoped usage event allowance was exhausted.
	ErrUsageAggregationLimit = errors.New("stream usage event limit reached")
	// ErrUsageAggregationClosed means a normalized terminal event already ended observation.
	ErrUsageAggregationClosed = errors.New("stream usage aggregation is closed")
	// ErrUsageEstimateExists means an Attempt tried to replace its local estimate.
	ErrUsageEstimateExists = errors.New("stream local usage estimate already exists")
)

// UsageOrigin distinguishes where the selected streaming usage fact came from.
type UsageOrigin string

const (
	// UsageOriginProviderFinal is usage attached to the normalized terminal event.
	UsageOriginProviderFinal UsageOrigin = "provider_final"
	// UsageOriginProviderChunks is the latest validated cumulative usage_delta.
	UsageOriginProviderChunks UsageOrigin = "provider_chunks"
	// UsageOriginLocalEstimate is a gateway-local fallback estimate.
	UsageOriginLocalEstimate UsageOrigin = "local_estimate"
)

// UsageAggregatorOptions bounds usage-bearing Provider events without limiting
// ordinary model Chunk count.
type UsageAggregatorOptions struct {
	MaxUsageEvents int
}

// UsageTrack preserves one source independently from other available sources.
type UsageTrack struct {
	Origin        UsageOrigin
	Usage         adapter.NormalizedUsage
	Events        int
	FirstSequence uint64
	LastSequence  uint64
}

// UsageAggregationSnapshot is safe to serialize: UsageEvidence emits only its
// digest and size, never raw Provider JSON.
type UsageAggregationSnapshot struct {
	Effective        *UsageTrack
	ProviderFinal    *UsageTrack
	ProviderChunks   *UsageTrack
	LocalEstimate    *UsageTrack
	TerminalObserved bool
	TerminalStatus   adapter.UsageStatus
	SequenceObserved bool
	LastSequence     uint64
	UsageEvents      int
	Closed           bool
}

// UsageAggregator consumes a complete normalized stream in Sequence order.
// Provider usage deltas are cumulative attempt-to-date snapshots, not additive
// increments. Keeping only the latest monotonic snapshot preserves its exact
// immutable RawEvidence while preventing duplicate input-token accounting.
type UsageAggregator struct {
	options UsageAggregatorOptions

	mu               sync.Mutex
	sequenceObserved bool
	lastSequence     uint64
	usageEvents      int
	providerFinal    *UsageTrack
	providerChunks   *UsageTrack
	localEstimate    *UsageTrack
	terminalObserved bool
	terminalStatus   adapter.UsageStatus
	closed           bool
}

// NewUsageAggregator creates one request-Attempt scoped aggregator.
func NewUsageAggregator(options UsageAggregatorOptions) (*UsageAggregator, error) {
	if options.MaxUsageEvents < 1 || options.MaxUsageEvents > maximumUsageEvents {
		return nil, ErrUsageAggregationInvalid
	}
	return &UsageAggregator{options: options}, nil
}

// SetLocalEstimate records an independent fallback exactly once. Provider
// observations never overwrite or relabel it.
func (aggregator *UsageAggregator) SetLocalEstimate(usage adapter.NormalizedUsage) error {
	if aggregator == nil || usage.Validate() != nil || usage.Source != adapter.UsageSourceEstimated {
		return ErrUsageAggregationInvalid
	}
	aggregator.mu.Lock()
	defer aggregator.mu.Unlock()
	if aggregator.localEstimate != nil {
		return ErrUsageEstimateExists
	}
	aggregator.localEstimate = &UsageTrack{Origin: UsageOriginLocalEstimate, Usage: usage.Clone(), Events: 1}
	return nil
}

// Observe consumes every normalized Chunk, not only usage-bearing events, so
// stale or reordered usage cannot hide behind ignored model events.
func (aggregator *UsageAggregator) Observe(chunk adapter.NormalizedChunk) error {
	if aggregator == nil || chunk.Validate() != nil {
		return ErrUsageAggregationInvalid
	}
	aggregator.mu.Lock()
	defer aggregator.mu.Unlock()
	if aggregator.closed {
		return ErrUsageAggregationClosed
	}
	if aggregator.sequenceObserved && chunk.Sequence <= aggregator.lastSequence {
		return ErrUsageAggregationOrder
	}

	switch chunk.Kind {
	case adapter.ChunkUsageDelta:
		if err := aggregator.observeProviderUsageLocked(chunk, false); err != nil {
			return err
		}
	case adapter.ChunkMessageEnd:
		if chunk.Usage != nil {
			if err := aggregator.observeProviderUsageLocked(chunk, true); err != nil {
				return err
			}
		}
		aggregator.terminalObserved = true
		aggregator.terminalStatus = chunk.UsageStatus
		aggregator.closed = true
	case adapter.ChunkMessageStart, adapter.ChunkContentDelta, adapter.ChunkReasoningDelta,
		adapter.ChunkToolDelta, adapter.ChunkHeartbeat, adapter.ChunkProviderExtension:
	}
	aggregator.sequenceObserved = true
	aggregator.lastSequence = chunk.Sequence
	return nil
}

// ClosePartial seals an interrupted stream without manufacturing a terminal
// Provider event or a zero Usage fact.
func (aggregator *UsageAggregator) ClosePartial() error {
	if aggregator == nil {
		return ErrUsageAggregationInvalid
	}
	aggregator.mu.Lock()
	defer aggregator.mu.Unlock()
	if aggregator.closed {
		return nil
	}
	aggregator.closed = true
	return nil
}

// KnownUsage returns the source-selected exact normalized fact suitable for an
// Attempt outcome. Provider terminal usage wins, then cumulative Chunk usage,
// then the independent local estimate. Missing remains nil rather than zero.
func (aggregator *UsageAggregator) KnownUsage() (*adapter.NormalizedUsage, UsageOrigin) {
	if aggregator == nil {
		return nil, ""
	}
	aggregator.mu.Lock()
	defer aggregator.mu.Unlock()
	track := effectiveUsageTrack(aggregator.providerFinal, aggregator.providerChunks, aggregator.localEstimate)
	if track == nil {
		return nil, ""
	}
	usage := track.Usage.Clone()
	return &usage, track.Origin
}

// Snapshot preserves all three tracks for observability and tests.
func (aggregator *UsageAggregator) Snapshot() UsageAggregationSnapshot {
	if aggregator == nil {
		return UsageAggregationSnapshot{}
	}
	aggregator.mu.Lock()
	defer aggregator.mu.Unlock()
	return UsageAggregationSnapshot{
		Effective:     cloneUsageTrack(effectiveUsageTrack(aggregator.providerFinal, aggregator.providerChunks, aggregator.localEstimate)),
		ProviderFinal: cloneUsageTrack(aggregator.providerFinal), ProviderChunks: cloneUsageTrack(aggregator.providerChunks),
		LocalEstimate: cloneUsageTrack(aggregator.localEstimate), TerminalObserved: aggregator.terminalObserved,
		TerminalStatus: aggregator.terminalStatus, SequenceObserved: aggregator.sequenceObserved,
		LastSequence: aggregator.lastSequence,
		UsageEvents:  aggregator.usageEvents, Closed: aggregator.closed,
	}
}

func (aggregator *UsageAggregator) observeProviderUsageLocked(chunk adapter.NormalizedChunk, terminal bool) error {
	if chunk.Usage == nil || chunk.Usage.Source != adapter.UsageSourceProvider {
		return ErrUsageAggregationInvalid
	}
	if aggregator.usageEvents >= aggregator.options.MaxUsageEvents {
		return ErrUsageAggregationLimit
	}
	if aggregator.providerChunks != nil {
		if err := validateCumulativeUsage(aggregator.providerChunks.Usage, *chunk.Usage); err != nil {
			return err
		}
	}
	track := &UsageTrack{
		Usage: chunk.Usage.Clone(), Events: 1, FirstSequence: chunk.Sequence, LastSequence: chunk.Sequence,
	}
	if terminal {
		track.Origin = UsageOriginProviderFinal
		aggregator.providerFinal = track
	} else {
		track.Origin = UsageOriginProviderChunks
		if aggregator.providerChunks != nil {
			track.Events = aggregator.providerChunks.Events + 1
			track.FirstSequence = aggregator.providerChunks.FirstSequence
		}
		aggregator.providerChunks = track
	}
	aggregator.usageEvents++
	return nil
}

func validateCumulativeUsage(previous, next adapter.NormalizedUsage) error {
	pairs := [][2]adapter.TokenCount{
		{previous.InputTokens, next.InputTokens}, {previous.OutputTokens, next.OutputTokens},
		{previous.CacheReadTokens, next.CacheReadTokens}, {previous.CacheWriteTokens, next.CacheWriteTokens},
		{previous.ReasoningTokens, next.ReasoningTokens}, {previous.AudioInputTokens, next.AudioInputTokens},
		{previous.AudioOutputTokens, next.AudioOutputTokens},
	}
	for _, pair := range pairs {
		if pair[0].Present && (!pair[1].Present || pair[1].Value < pair[0].Value) {
			return ErrUsageAggregationRegression
		}
	}
	return nil
}

func effectiveUsageTrack(providerFinal, providerChunks, localEstimate *UsageTrack) *UsageTrack {
	if providerFinal != nil {
		return providerFinal
	}
	if providerChunks != nil {
		return providerChunks
	}
	return localEstimate
}

func cloneUsageTrack(track *UsageTrack) *UsageTrack {
	if track == nil {
		return nil
	}
	cloned := *track
	cloned.Usage = track.Usage.Clone()
	return &cloned
}
