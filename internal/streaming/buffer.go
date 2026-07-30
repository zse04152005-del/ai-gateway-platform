// Package streaming coordinates bounded provider-to-client stream flow.
package streaming

import (
	"context"
	"errors"
	"io"
	"math"
	"sync"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
)

const chunkAllocationOverhead = 512

var (
	// ErrBufferInvalid means configuration or a chunk violated the queue contract.
	ErrBufferInvalid = errors.New("stream buffer input is invalid")
	// ErrBufferClosed means the producer attempted to append after termination.
	ErrBufferClosed = errors.New("stream buffer is closed")
	// ErrBackpressure means a full buffer did not make progress within its window.
	ErrBackpressure = errors.New("stream backpressure limit exceeded")
	// ErrChunkTooLarge means one valid chunk cannot fit in the byte budget.
	ErrChunkTooLarge = errors.New("stream chunk exceeds buffer byte limit")
)

// BufferOptions defines simultaneous count, memory, and wait bounds.
type BufferOptions struct {
	MaxChunks           int
	MaxBytes            int
	BackpressureTimeout time.Duration
}

// BufferStats is a safe snapshot for metrics and pressure tests.
type BufferStats struct {
	QueuedChunks      int
	QueuedBytes       int
	MaxObservedChunks int
	MaxObservedBytes  int
	BackpressureWaits uint64
	OverflowCount     uint64
}

type bufferedChunk struct {
	chunk adapter.NormalizedChunk
	bytes int
}

// Buffer is a FIFO with independent chunk-count and estimated-memory bounds.
// Push blocks only for BackpressureTimeout; expiry aborts the queue and cancels
// Context so the same Context can stop the upstream HTTP read.
type Buffer struct {
	ctx     context.Context
	cancel  context.CancelCauseFunc
	options BufferOptions

	mu        sync.Mutex
	queue     []bufferedChunk
	bytes     int
	finished  bool
	finishErr error
	aborted   bool
	abortErr  error
	changed   chan struct{}
	stats     BufferStats
}

// NewBuffer validates hard resource bounds and derives the Context used by the
// producer/upstream request.
func NewBuffer(parent context.Context, options BufferOptions) (*Buffer, error) {
	if parent == nil || options.MaxChunks < 1 || options.MaxChunks > 4096 ||
		options.MaxBytes < 1024 || options.MaxBytes > 64<<20 ||
		options.BackpressureTimeout <= 0 || options.BackpressureTimeout > 30*time.Second {
		return nil, ErrBufferInvalid
	}
	ctx, cancel := context.WithCancelCause(parent)
	initialCapacity := min(options.MaxChunks, 64, max(1, options.MaxBytes/chunkAllocationOverhead))
	return &Buffer{
		ctx: ctx, cancel: cancel, options: options, changed: make(chan struct{}),
		queue: make([]bufferedChunk, 0, initialCapacity),
	}, nil
}

// Context must be used by the upstream request and parser. Backpressure Abort
// therefore stops provider work without a separate cancellation channel.
func (buffer *Buffer) Context() context.Context {
	if buffer == nil {
		return nil
	}
	return buffer.ctx
}

// Push deep-copies one validated chunk. It applies producer backpressure until
// space exists or the bounded wait expires.
func (buffer *Buffer) Push(ctx context.Context, chunk adapter.NormalizedChunk) error {
	if buffer == nil || ctx == nil || chunk.Validate() != nil {
		return ErrBufferInvalid
	}
	weight, err := estimateChunkBytes(chunk)
	if err != nil {
		return err
	}
	if weight > buffer.options.MaxBytes {
		buffer.abort(ErrChunkTooLarge, true)
		return ErrChunkTooLarge
	}
	queued := bufferedChunk{chunk: chunk.Clone(), bytes: weight}

	var timer *time.Timer
	var timerChannel <-chan time.Time
	waitRecorded := false
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		if err := cancellationError(ctx); err != nil {
			return err
		}
		if err := cancellationError(buffer.ctx); err != nil {
			return err
		}

		buffer.mu.Lock()
		if buffer.aborted || buffer.finished {
			abortErr := buffer.abortErr
			buffer.mu.Unlock()
			if abortErr != nil {
				return abortErr
			}
			return ErrBufferClosed
		}
		if len(buffer.queue) < buffer.options.MaxChunks && buffer.bytes <= buffer.options.MaxBytes-weight {
			buffer.queue = append(buffer.queue, queued)
			buffer.bytes += weight
			buffer.stats.QueuedChunks = len(buffer.queue)
			buffer.stats.QueuedBytes = buffer.bytes
			buffer.stats.MaxObservedChunks = max(buffer.stats.MaxObservedChunks, len(buffer.queue))
			buffer.stats.MaxObservedBytes = max(buffer.stats.MaxObservedBytes, buffer.bytes)
			buffer.notifyLocked()
			buffer.mu.Unlock()
			return nil
		}
		if !waitRecorded {
			buffer.stats.BackpressureWaits++
			waitRecorded = true
		}
		changed := buffer.changed
		buffer.mu.Unlock()

		if timer == nil {
			timer = time.NewTimer(buffer.options.BackpressureTimeout)
			timerChannel = timer.C
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return cancellationError(ctx)
		case <-buffer.ctx.Done():
			return cancellationError(buffer.ctx)
		case <-timerChannel:
			buffer.abort(ErrBackpressure, true)
			return ErrBackpressure
		}
	}
}

// Next returns one buffered fact. A successful Finish drains all queued facts
// before EOF; a Finish error is returned after the same drain.
func (buffer *Buffer) Next(ctx context.Context) (adapter.NormalizedChunk, error) {
	if buffer == nil || ctx == nil {
		return adapter.NormalizedChunk{}, ErrBufferInvalid
	}
	for {
		if err := cancellationError(ctx); err != nil {
			return adapter.NormalizedChunk{}, err
		}
		if err := cancellationError(buffer.ctx); err != nil {
			return adapter.NormalizedChunk{}, err
		}

		buffer.mu.Lock()
		if len(buffer.queue) > 0 {
			queued := buffer.queue[0]
			buffer.queue[0] = bufferedChunk{}
			buffer.queue = buffer.queue[1:]
			buffer.bytes -= queued.bytes
			buffer.stats.QueuedChunks = len(buffer.queue)
			buffer.stats.QueuedBytes = buffer.bytes
			buffer.notifyLocked()
			buffer.mu.Unlock()
			return queued.chunk, nil
		}
		if buffer.finished {
			finishErr := buffer.finishErr
			buffer.mu.Unlock()
			if finishErr != nil {
				return adapter.NormalizedChunk{}, finishErr
			}
			return adapter.NormalizedChunk{}, io.EOF
		}
		if buffer.aborted {
			abortErr := buffer.abortErr
			buffer.mu.Unlock()
			if abortErr != nil {
				return adapter.NormalizedChunk{}, abortErr
			}
			return adapter.NormalizedChunk{}, ErrBufferClosed
		}
		changed := buffer.changed
		buffer.mu.Unlock()

		select {
		case <-changed:
		case <-ctx.Done():
			return adapter.NormalizedChunk{}, cancellationError(ctx)
		case <-buffer.ctx.Done():
			return adapter.NormalizedChunk{}, cancellationError(buffer.ctx)
		}
	}
}

// Finish closes the producer side while preserving already accepted chunks.
// The first terminal result wins.
func (buffer *Buffer) Finish(err error) {
	if buffer == nil {
		return
	}
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	if buffer.finished || buffer.aborted {
		return
	}
	buffer.finished = true
	buffer.finishErr = err
	buffer.notifyLocked()
}

// Abort immediately discards queued chunks and cancels the shared Context.
func (buffer *Buffer) Abort(cause error) {
	if cause == nil {
		cause = ErrBufferClosed
	}
	buffer.abort(cause, false)
}

// Stats returns current and high-water queue evidence.
func (buffer *Buffer) Stats() BufferStats {
	if buffer == nil {
		return BufferStats{}
	}
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.stats
}

func (buffer *Buffer) abort(cause error, overflow bool) {
	if buffer == nil {
		return
	}
	buffer.mu.Lock()
	if buffer.finished || buffer.aborted {
		buffer.mu.Unlock()
		return
	}
	buffer.aborted = true
	buffer.abortErr = cause
	for index := range buffer.queue {
		buffer.queue[index] = bufferedChunk{}
	}
	buffer.queue = nil
	buffer.bytes = 0
	buffer.stats.QueuedChunks = 0
	buffer.stats.QueuedBytes = 0
	if overflow {
		buffer.stats.OverflowCount++
	}
	buffer.notifyLocked()
	buffer.mu.Unlock()
	buffer.cancel(cause)
}

func (buffer *Buffer) notifyLocked() {
	close(buffer.changed)
	buffer.changed = make(chan struct{})
}

func estimateChunkBytes(chunk adapter.NormalizedChunk) (int, error) {
	if err := chunk.Validate(); err != nil {
		return 0, ErrBufferInvalid
	}
	weight := chunkAllocationOverhead
	parts := []int{
		len(chunk.Kind), len(chunk.Role), len(chunk.ContentDelta), len(chunk.ReasoningDelta),
		len(chunk.FinishReason), len(chunk.ProviderFinishReason), len(chunk.UsageStatus),
		len(chunk.ProviderEventType), len(chunk.ProviderExtension),
	}
	if chunk.ToolDelta != nil {
		parts = append(parts, 128, len(chunk.ToolDelta.ID), len(chunk.ToolDelta.Name), len(chunk.ToolDelta.ArgumentsFragment))
	}
	if chunk.Usage != nil {
		parts = append(parts, 256, chunk.Usage.RawEvidence.Size())
		for _, field := range chunk.Usage.UnmappedFields {
			parts = append(parts, 16, len(field))
		}
	}
	for _, part := range parts {
		if part < 0 || weight > math.MaxInt-part {
			return 0, ErrChunkTooLarge
		}
		weight += part
	}
	return weight, nil
}

func cancellationError(ctx context.Context) error {
	if ctx == nil || ctx.Err() == nil {
		return nil
	}
	cause := context.Cause(ctx)
	if cause == nil {
		return ctx.Err()
	}
	return errors.Join(ctx.Err(), cause)
}
