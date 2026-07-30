package streaming

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
)

func TestBufferPreservesFIFOClonesAndFinishSemantics(t *testing.T) {
	buffer := newTestBuffer(t, BufferOptions{MaxChunks: 4, MaxBytes: 16 * 1024, BackpressureTimeout: time.Second})
	first := testChunk(0, "first")
	second := testChunk(1, "second")
	if err := buffer.Push(context.Background(), first); err != nil {
		t.Fatalf("Push(first) error = %v", err)
	}
	if err := buffer.Push(context.Background(), second); err != nil {
		t.Fatalf("Push(second) error = %v", err)
	}
	first.ContentDelta = "mutated-after-push"
	buffer.Finish(nil)

	gotFirst, err := buffer.Next(context.Background())
	if err != nil || gotFirst.Sequence != 0 || gotFirst.ContentDelta != "first" {
		t.Fatalf("first Next() = %+v/%v", gotFirst, err)
	}
	gotSecond, err := buffer.Next(context.Background())
	if err != nil || gotSecond.Sequence != 1 || gotSecond.ContentDelta != "second" {
		t.Fatalf("second Next() = %+v/%v", gotSecond, err)
	}
	if _, err := buffer.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal Next() error = %v, want EOF", err)
	}
	stats := buffer.Stats()
	if stats.QueuedChunks != 0 || stats.QueuedBytes != 0 || stats.MaxObservedChunks != 2 || stats.MaxObservedBytes <= 0 {
		t.Fatalf("stats = %+v", stats)
	}
	buffer.Finish(errors.New("ignored second finish"))
}

func TestBufferAppliesBackpressureThenResumesWhenConsumerProgresses(t *testing.T) {
	buffer := newTestBuffer(t, BufferOptions{MaxChunks: 2, MaxBytes: 16 * 1024, BackpressureTimeout: time.Second})
	if err := buffer.Push(context.Background(), testChunk(0, "first")); err != nil {
		t.Fatal(err)
	}
	if err := buffer.Push(context.Background(), testChunk(1, "second")); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- buffer.Push(context.Background(), testChunk(2, "third")) }()
	select {
	case err := <-result:
		t.Fatalf("third Push returned before consumer progress: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	if _, err := buffer.Next(context.Background()); err != nil {
		t.Fatalf("Next() error = %v", err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("resumed Push() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("producer did not resume after consumer progress")
	}
	stats := buffer.Stats()
	if stats.BackpressureWaits != 1 || stats.OverflowCount != 0 || stats.MaxObservedChunks > 2 {
		t.Fatalf("backpressure stats = %+v", stats)
	}
}

func TestBufferTimeoutCancelsUpstreamAndDiscardsBoundedQueue(t *testing.T) {
	buffer := newTestBuffer(t, BufferOptions{MaxChunks: 1, MaxBytes: 4096, BackpressureTimeout: 30 * time.Millisecond})
	if err := buffer.Push(context.Background(), testChunk(0, "first")); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err := buffer.Push(context.Background(), testChunk(1, "second"))
	if !errors.Is(err, ErrBackpressure) || time.Since(started) < 20*time.Millisecond {
		t.Fatalf("timed Push() = %v after %v", err, time.Since(started))
	}
	if !errors.Is(context.Cause(buffer.Context()), ErrBackpressure) {
		t.Fatalf("buffer Context cause = %v", context.Cause(buffer.Context()))
	}
	if _, err := buffer.Next(context.Background()); !errors.Is(err, ErrBackpressure) || !errors.Is(err, context.Canceled) {
		t.Fatalf("Next() after overflow error = %v", err)
	}
	stats := buffer.Stats()
	if stats.QueuedChunks != 0 || stats.QueuedBytes != 0 || stats.OverflowCount != 1 || stats.BackpressureWaits != 1 {
		t.Fatalf("overflow stats = %+v", stats)
	}
}

func TestBufferRejectsSingleOversizedChunkAndInvalidInput(t *testing.T) {
	chunk := testChunk(0, strings.Repeat("x", 600))
	weight, err := estimateChunkBytes(chunk)
	if err != nil {
		t.Fatal(err)
	}
	buffer := newTestBuffer(t, BufferOptions{MaxChunks: 2, MaxBytes: weight - 1, BackpressureTimeout: time.Second})
	if err := buffer.Push(context.Background(), chunk); !errors.Is(err, ErrChunkTooLarge) {
		t.Fatalf("oversized Push() error = %v", err)
	}
	if !errors.Is(context.Cause(buffer.Context()), ErrChunkTooLarge) || buffer.Stats().OverflowCount != 1 {
		t.Fatalf("oversized context/stats = %v/%+v", context.Cause(buffer.Context()), buffer.Stats())
	}

	valid := newTestBuffer(t, BufferOptions{MaxChunks: 2, MaxBytes: 4096, BackpressureTimeout: time.Second})
	if err := valid.Push(nil, chunk); !errors.Is(err, ErrBufferInvalid) { //nolint:staticcheck // explicit nil boundary
		t.Fatalf("nil-context Push() error = %v", err)
	}
	invalidChunk := chunk
	invalidChunk.ContentDelta = ""
	if err := valid.Push(context.Background(), invalidChunk); !errors.Is(err, ErrBufferInvalid) {
		t.Fatalf("invalid-chunk Push() error = %v", err)
	}
	var nilBuffer *Buffer
	if err := nilBuffer.Push(context.Background(), chunk); !errors.Is(err, ErrBufferInvalid) {
		t.Fatalf("nil Buffer.Push() error = %v", err)
	}
	if _, err := nilBuffer.Next(context.Background()); !errors.Is(err, ErrBufferInvalid) {
		t.Fatalf("nil Buffer.Next() error = %v", err)
	}
}

func TestBufferDrainsBeforeProducerErrorAndAbortIsImmediate(t *testing.T) {
	producerFailure := errors.New("provider stream failed")
	buffer := newTestBuffer(t, BufferOptions{MaxChunks: 2, MaxBytes: 4096, BackpressureTimeout: time.Second})
	if err := buffer.Push(context.Background(), testChunk(0, "accepted")); err != nil {
		t.Fatal(err)
	}
	buffer.Finish(producerFailure)
	if chunk, err := buffer.Next(context.Background()); err != nil || chunk.ContentDelta != "accepted" {
		t.Fatalf("drained chunk = %+v/%v", chunk, err)
	}
	if _, err := buffer.Next(context.Background()); !errors.Is(err, producerFailure) {
		t.Fatalf("producer terminal error = %v", err)
	}

	aborted := newTestBuffer(t, BufferOptions{MaxChunks: 2, MaxBytes: 4096, BackpressureTimeout: time.Second})
	if err := aborted.Push(context.Background(), testChunk(0, "discarded")); err != nil {
		t.Fatal(err)
	}
	disconnect := errors.New("client disconnected")
	aborted.Abort(disconnect)
	if !errors.Is(context.Cause(aborted.Context()), disconnect) || aborted.Stats().QueuedChunks != 0 {
		t.Fatalf("abort cause/stats = %v/%+v", context.Cause(aborted.Context()), aborted.Stats())
	}
	if _, err := aborted.Next(context.Background()); !errors.Is(err, disconnect) {
		t.Fatalf("aborted Next() error = %v", err)
	}
}

func TestBufferConcurrentProducerConsumerNeverExceedsBounds(t *testing.T) {
	const total = 1000
	buffer := newTestBuffer(t, BufferOptions{MaxChunks: 8, MaxBytes: 8192, BackpressureTimeout: 2 * time.Second})
	var wait sync.WaitGroup
	wait.Add(1)
	producerErrors := make(chan error, 1)
	go func() {
		defer wait.Done()
		for index := range total {
			if err := buffer.Push(context.Background(), testChunk(uint64(index), fmt.Sprintf("chunk-%04d", index))); err != nil {
				producerErrors <- err
				return
			}
		}
		buffer.Finish(nil)
	}()

	for expected := range total {
		chunk, err := buffer.Next(context.Background())
		if err != nil || chunk.Sequence != uint64(expected) {
			t.Fatalf("Next(%d) = %+v/%v", expected, chunk, err)
		}
	}
	if _, err := buffer.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("concurrent terminal error = %v", err)
	}
	wait.Wait()
	select {
	case err := <-producerErrors:
		t.Fatalf("producer error = %v", err)
	default:
	}
	stats := buffer.Stats()
	if stats.MaxObservedChunks > 8 || stats.MaxObservedBytes > 8192 || stats.OverflowCount != 0 {
		t.Fatalf("concurrent high-water stats = %+v", stats)
	}
}

func TestBufferCancellationAndConfigurationValidation(t *testing.T) {
	valid := BufferOptions{MaxChunks: 1, MaxBytes: 1024, BackpressureTimeout: time.Millisecond}
	tests := []struct {
		name    string
		parent  context.Context
		options BufferOptions
	}{
		{name: "nil parent", options: valid},
		{name: "zero chunks", parent: context.Background(), options: BufferOptions{MaxBytes: 1024, BackpressureTimeout: time.Second}},
		{name: "too many chunks", parent: context.Background(), options: BufferOptions{MaxChunks: 4097, MaxBytes: 1024, BackpressureTimeout: time.Second}},
		{name: "low bytes", parent: context.Background(), options: BufferOptions{MaxChunks: 1, MaxBytes: 1023, BackpressureTimeout: time.Second}},
		{name: "high bytes", parent: context.Background(), options: BufferOptions{MaxChunks: 1, MaxBytes: 64<<20 + 1, BackpressureTimeout: time.Second}},
		{name: "zero timeout", parent: context.Background(), options: BufferOptions{MaxChunks: 1, MaxBytes: 1024}},
		{name: "high timeout", parent: context.Background(), options: BufferOptions{MaxChunks: 1, MaxBytes: 1024, BackpressureTimeout: 31 * time.Second}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			buffer, err := NewBuffer(test.parent, test.options)
			if !errors.Is(err, ErrBufferInvalid) || buffer != nil {
				t.Fatalf("NewBuffer() = %#v/%v", buffer, err)
			}
		})
	}

	buffer := newTestBuffer(t, valid)
	waitCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := buffer.Next(waitCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Next() error = %v", err)
	}
	buffer.Abort(nil)
	if !errors.Is(context.Cause(buffer.Context()), ErrBufferClosed) {
		t.Fatalf("nil-cause Abort context = %v", context.Cause(buffer.Context()))
	}
	if buffer.Context() == nil || (*Buffer)(nil).Context() != nil || (*Buffer)(nil).Stats() != (BufferStats{}) {
		t.Fatal("Context/Stats nil receiver contract failed")
	}
}

func newTestBuffer(t *testing.T, options BufferOptions) *Buffer {
	t.Helper()
	buffer, err := NewBuffer(context.Background(), options)
	if err != nil {
		t.Fatalf("NewBuffer() error = %v", err)
	}
	return buffer
}

func testChunk(sequence uint64, content string) adapter.NormalizedChunk {
	return adapter.NormalizedChunk{
		Sequence: sequence, Kind: adapter.ChunkContentDelta, ChoiceIndex: 0,
		ContentDelta: content, ProviderEventType: "message", ObservedAt: time.Unix(100, 0).UTC(),
	}
}
