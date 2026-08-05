package streaming

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
)

const testEstimationAlgorithm = "utf8-byte-bound"

func TestUsageAggregatorPreservesThreeSourcesAndSelectsProviderFinal(t *testing.T) {
	aggregator := newTestUsageAggregator(t, 8)
	estimate := adapter.NormalizedUsage{
		InputTokens: adapter.Tokens(8), OutputTokens: adapter.Tokens(2),
		Source: adapter.UsageSourceEstimated, Complete: false, Estimate: streamEstimateMetadata(),
	}
	if err := aggregator.SetLocalEstimate(estimate); err != nil {
		t.Fatalf("SetLocalEstimate() error = %v", err)
	}
	observeUsageChunk(t, aggregator, usageMessageStart(1))
	observeUsageChunk(t, aggregator, providerUsageDelta(t, 2, 10, 1, "chunk-1"))
	observeUsageChunk(t, aggregator, providerUsageDelta(t, 3, 10, 3, "chunk-2"))
	observeUsageChunk(t, aggregator, providerUsageEnd(t, 4, adapter.UsageStatusPresent, 10, 4, true, "final"))

	known, origin := aggregator.KnownUsage()
	if origin != UsageOriginProviderFinal || known == nil || !known.Complete ||
		known.InputTokens != adapter.Tokens(10) || known.OutputTokens != adapter.Tokens(4) ||
		known.RawEvidenceHash() != usageEvidenceHash(t, 10, 4, "final") {
		t.Fatalf("KnownUsage() = %+v/%s", known, origin)
	}
	snapshot := aggregator.Snapshot()
	if snapshot.Effective == nil || snapshot.Effective.Origin != UsageOriginProviderFinal ||
		snapshot.ProviderFinal == nil || snapshot.ProviderChunks == nil || snapshot.LocalEstimate == nil ||
		snapshot.ProviderChunks.Events != 2 || snapshot.ProviderChunks.FirstSequence != 2 ||
		snapshot.ProviderChunks.LastSequence != 3 || snapshot.UsageEvents != 3 ||
		!snapshot.TerminalObserved || snapshot.TerminalStatus != adapter.UsageStatusPresent ||
		!snapshot.SequenceObserved || snapshot.LastSequence != 4 || !snapshot.Closed {
		t.Fatalf("Snapshot() = %+v", snapshot)
	}
	if snapshot.ProviderChunks.Usage.OutputTokens != adapter.Tokens(3) ||
		snapshot.LocalEstimate.Usage.OutputTokens != adapter.Tokens(2) {
		t.Fatalf("source tracks collapsed = %+v", snapshot)
	}

	snapshot.ProviderChunks.Usage.UnmappedFields = append(snapshot.ProviderChunks.Usage.UnmappedFields, "/mutated")
	if len(aggregator.Snapshot().ProviderChunks.Usage.UnmappedFields) != 0 {
		t.Fatal("Snapshot() aliases aggregator usage")
	}
}

func TestUsageAggregatorAcceptsAdapterSequenceStartingAtZero(t *testing.T) {
	aggregator := newTestUsageAggregator(t, 2)
	observeUsageChunk(t, aggregator, usageMessageStart(0))
	observeUsageChunk(t, aggregator, providerUsageDelta(t, 1, 3, 1, "sequence-one-usage"))
	snapshot := aggregator.Snapshot()
	if !snapshot.SequenceObserved || snapshot.LastSequence != 1 || snapshot.ProviderChunks == nil ||
		snapshot.ProviderChunks.FirstSequence != 1 {
		t.Fatalf("zero-based Snapshot() = %+v", snapshot)
	}
	if err := aggregator.Observe(usageHeartbeat(0)); !errors.Is(err, ErrUsageAggregationOrder) {
		t.Fatalf("stale zero Sequence error = %v", err)
	}
}

func TestUsageAggregatorFallsBackFromMissingFinalToChunksThenEstimate(t *testing.T) {
	t.Run("provider chunks", func(t *testing.T) {
		aggregator := newTestUsageAggregator(t, 4)
		if err := aggregator.SetLocalEstimate(adapter.NormalizedUsage{
			InputTokens: adapter.Tokens(7), Source: adapter.UsageSourceEstimated,
			Estimate: streamEstimateMetadata(),
		}); err != nil {
			t.Fatalf("SetLocalEstimate() error = %v", err)
		}
		observeUsageChunk(t, aggregator, providerUsageDelta(t, 1, 9, 2, "chunk"))
		observeUsageChunk(t, aggregator, usageMissingEnd(2))
		known, origin := aggregator.KnownUsage()
		if origin != UsageOriginProviderChunks || known == nil || known.Complete ||
			known.InputTokens != adapter.Tokens(9) || known.OutputTokens != adapter.Tokens(2) {
			t.Fatalf("KnownUsage() = %+v/%s", known, origin)
		}
		if aggregator.Snapshot().ProviderFinal != nil || aggregator.Snapshot().TerminalStatus != adapter.UsageStatusMissing {
			t.Fatalf("missing terminal snapshot = %+v", aggregator.Snapshot())
		}
	})

	t.Run("local estimate", func(t *testing.T) {
		aggregator := newTestUsageAggregator(t, 2)
		estimate := adapter.NormalizedUsage{
			OutputTokens: adapter.Tokens(5), Source: adapter.UsageSourceEstimated,
			Estimate: streamEstimateMetadata(),
		}
		if err := aggregator.SetLocalEstimate(estimate); err != nil {
			t.Fatalf("SetLocalEstimate() error = %v", err)
		}
		observeUsageChunk(t, aggregator, usageMissingEnd(1))
		known, origin := aggregator.KnownUsage()
		if origin != UsageOriginLocalEstimate || known == nil || known.OutputTokens != adapter.Tokens(5) {
			t.Fatalf("KnownUsage() = %+v/%s", known, origin)
		}
	})

	t.Run("unknown remains absent", func(t *testing.T) {
		aggregator := newTestUsageAggregator(t, 1)
		if err := aggregator.ClosePartial(); err != nil {
			t.Fatalf("ClosePartial() error = %v", err)
		}
		if known, origin := aggregator.KnownUsage(); known != nil || origin != "" {
			t.Fatalf("KnownUsage() = %+v/%s", known, origin)
		}
	})
}

func TestUsageAggregatorAcceptsPartialTerminalAndRejectsRegressions(t *testing.T) {
	aggregator := newTestUsageAggregator(t, 4)
	observeUsageChunk(t, aggregator, providerUsageDelta(t, 1, 10, 3, "chunk"))

	regressions := []adapter.NormalizedChunk{
		providerUsageDelta(t, 2, 9, 3, "decreased"),
		providerUsageDeltaMissingInput(t, 2, 4, "missing-input"),
	}
	for index, chunk := range regressions {
		if err := aggregator.Observe(chunk); !errors.Is(err, ErrUsageAggregationRegression) {
			t.Fatalf("regression[%d] error = %v", index, err)
		}
		if aggregator.Snapshot().LastSequence != 1 || aggregator.Snapshot().UsageEvents != 1 {
			t.Fatalf("regression[%d] mutated state: %+v", index, aggregator.Snapshot())
		}
	}
	partialEnd := providerUsageEnd(t, 2, adapter.UsageStatusPartial, 10, 4, false, "partial-final")
	observeUsageChunk(t, aggregator, partialEnd)
	known, origin := aggregator.KnownUsage()
	if origin != UsageOriginProviderFinal || known == nil || known.Complete || known.OutputTokens != adapter.Tokens(4) {
		t.Fatalf("partial KnownUsage() = %+v/%s", known, origin)
	}
}

func TestUsageAggregatorRejectsOrderSourceLimitAndDuplicateEstimate(t *testing.T) {
	aggregator := newTestUsageAggregator(t, 1)
	estimate := adapter.NormalizedUsage{
		InputTokens: adapter.Tokens(1), Source: adapter.UsageSourceEstimated,
		Estimate: streamEstimateMetadata(),
	}
	if err := aggregator.SetLocalEstimate(estimate); err != nil {
		t.Fatalf("SetLocalEstimate() error = %v", err)
	}
	if err := aggregator.SetLocalEstimate(estimate); !errors.Is(err, ErrUsageEstimateExists) {
		t.Fatalf("duplicate estimate error = %v", err)
	}
	observeUsageChunk(t, aggregator, providerUsageDelta(t, 2, 4, 1, "chunk"))
	if err := aggregator.Observe(usageHeartbeat(2)); !errors.Is(err, ErrUsageAggregationOrder) {
		t.Fatalf("duplicate Sequence error = %v", err)
	}
	if err := aggregator.Observe(providerUsageEnd(t, 3, adapter.UsageStatusPresent, 4, 2, true, "final")); !errors.Is(err, ErrUsageAggregationLimit) {
		t.Fatalf("usage limit error = %v", err)
	}
	if aggregator.Snapshot().Closed || aggregator.Snapshot().LastSequence != 2 {
		t.Fatalf("limit mutated state = %+v", aggregator.Snapshot())
	}

	estimatedChunk := providerUsageDelta(t, 3, 4, 2, "estimated-source")
	estimatedChunk.Usage.Source = adapter.UsageSourceEstimated
	estimatedChunk.Usage.RawEvidence = adapter.UsageEvidence{}
	estimatedChunk.Usage.Estimate = streamEstimateMetadata()
	if err := aggregator.Observe(estimatedChunk); !errors.Is(err, ErrUsageAggregationInvalid) {
		t.Fatalf("estimated Provider chunk error = %v", err)
	}
	if err := aggregator.Observe(usageMessageStart(3)); err != nil {
		t.Fatalf("non-usage event after limit error = %v", err)
	}
	if err := aggregator.ClosePartial(); err != nil {
		t.Fatalf("ClosePartial() error = %v", err)
	}
	if err := aggregator.Observe(usageHeartbeat(4)); !errors.Is(err, ErrUsageAggregationClosed) {
		t.Fatalf("Observe(after close) error = %v", err)
	}
}

func TestUsageAggregatorConcurrentSnapshotsRemainConsistent(t *testing.T) {
	aggregator := newTestUsageAggregator(t, 128)
	var wait sync.WaitGroup
	wait.Add(2)
	start := make(chan struct{})
	go func() {
		defer wait.Done()
		<-start
		for range 1000 {
			_ = aggregator.Snapshot()
		}
	}()
	go func() {
		defer wait.Done()
		<-start
		for range 1000 {
			_, _ = aggregator.KnownUsage()
		}
	}()
	close(start)
	for sequence := uint64(1); sequence <= 64; sequence++ {
		observeUsageChunk(t, aggregator, providerUsageDelta(t, sequence, 10, int64(sequence), fmt.Sprintf("chunk-%d", sequence)))
	}
	wait.Wait()
	if snapshot := aggregator.Snapshot(); snapshot.UsageEvents != 64 || snapshot.ProviderChunks.Events != 64 {
		t.Fatalf("concurrent Snapshot() = %+v", snapshot)
	}
}

func streamEstimateMetadata() *adapter.UsageEstimateMetadata {
	return &adapter.UsageEstimateMetadata{
		Estimated: true, Tokenizer: testEstimationAlgorithm, TokenizerVersion: "v1",
		PhysicalModel: "model-fixture", DeploymentVersion: 1,
		ProviderProtocolVersion: "protocol-v1",
	}
}

func newTestUsageAggregator(t *testing.T, maxEvents int) *UsageAggregator {
	t.Helper()
	aggregator, err := NewUsageAggregator(UsageAggregatorOptions{MaxUsageEvents: maxEvents})
	if err != nil {
		t.Fatalf("NewUsageAggregator() error = %v", err)
	}
	return aggregator
}

func observeUsageChunk(t *testing.T, aggregator *UsageAggregator, chunk adapter.NormalizedChunk) {
	t.Helper()
	if err := aggregator.Observe(chunk); err != nil {
		t.Fatalf("Observe(%s sequence=%d) error = %v", chunk.Kind, chunk.Sequence, err)
	}
}

func providerUsageDelta(t *testing.T, sequence uint64, input, output int64, marker string) adapter.NormalizedChunk {
	t.Helper()
	usage := providerStreamingUsage(t, input, output, false, marker)
	return adapter.NormalizedChunk{
		Sequence: sequence, Kind: adapter.ChunkUsageDelta, Usage: &usage,
		UsageStatus: adapter.UsageStatusPartial, ObservedAt: time.Now().UTC(),
	}
}

func providerUsageDeltaMissingInput(t *testing.T, sequence uint64, output int64, marker string) adapter.NormalizedChunk {
	t.Helper()
	usage := providerStreamingUsage(t, 0, output, false, marker)
	usage.InputTokens = adapter.TokenCount{}
	return adapter.NormalizedChunk{
		Sequence: sequence, Kind: adapter.ChunkUsageDelta, Usage: &usage,
		UsageStatus: adapter.UsageStatusPartial, ObservedAt: time.Now().UTC(),
	}
}

func providerUsageEnd(
	t *testing.T,
	sequence uint64,
	status adapter.UsageStatus,
	input, output int64,
	complete bool,
	marker string,
) adapter.NormalizedChunk {
	t.Helper()
	usage := providerStreamingUsage(t, input, output, complete, marker)
	return adapter.NormalizedChunk{
		Sequence: sequence, Kind: adapter.ChunkMessageEnd, FinishReason: adapter.FinishStop,
		ProviderFinishReason: "stop", Usage: &usage, UsageStatus: status, ObservedAt: time.Now().UTC(),
	}
}

func providerStreamingUsage(t *testing.T, input, output int64, complete bool, marker string) adapter.NormalizedUsage {
	t.Helper()
	raw := []byte(fmt.Sprintf(`{"input_tokens":%d,"output_tokens":%d,"marker":%q}`, input, output, marker))
	evidence, err := adapter.NewUsageEvidence(raw)
	if err != nil {
		t.Fatalf("NewUsageEvidence() error = %v", err)
	}
	return adapter.NormalizedUsage{
		InputTokens: adapter.Tokens(input), OutputTokens: adapter.Tokens(output),
		Source: adapter.UsageSourceProvider, Complete: complete, RawEvidence: evidence,
	}
}

func usageEvidenceHash(t *testing.T, input, output int64, marker string) string {
	t.Helper()
	return providerStreamingUsage(t, input, output, true, marker).RawEvidenceHash()
}

func usageMessageStart(sequence uint64) adapter.NormalizedChunk {
	return adapter.NormalizedChunk{
		Sequence: sequence, Kind: adapter.ChunkMessageStart, Role: adapter.RoleAssistant,
		ObservedAt: time.Now().UTC(),
	}
}

func usageHeartbeat(sequence uint64) adapter.NormalizedChunk {
	return adapter.NormalizedChunk{Sequence: sequence, Kind: adapter.ChunkHeartbeat, ObservedAt: time.Now().UTC()}
}

func usageMissingEnd(sequence uint64) adapter.NormalizedChunk {
	return adapter.NormalizedChunk{
		Sequence: sequence, Kind: adapter.ChunkMessageEnd, FinishReason: adapter.FinishStop,
		ProviderFinishReason: "stop", UsageStatus: adapter.UsageStatusMissing, ObservedAt: time.Now().UTC(),
	}
}
