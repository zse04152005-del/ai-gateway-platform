# P07-T03 bounded stream buffer and backpressure report

## Result

`internal/streaming.Buffer` now provides a bounded FIFO handoff between an upstream `provideradapter.ChunkStream` producer and the downstream SSE writer consumer. It limits both queue length and conservatively estimated resident bytes, applies a finite producer wait, and uses one derived CancelCause Context to stop upstream work when a slow client exhausts the pressure window.

## Bounds and ownership

- Configuration requires 1–4096 queued Chunks, 1 KiB–64 MiB estimated bytes, and a positive backpressure timeout no greater than 30 seconds.
- Every accepted `NormalizedChunk` is validated and deep-copied before it enters the queue.
- Estimated bytes include a fixed allocation allowance plus all dynamic strings, tool fragments, provider extensions, usage evidence, and unmapped usage fields.
- Initial slice capacity is itself capped by both the Chunk and byte budgets; the queue does not preallocate beyond a small bounded high-water estimate.
- A single Chunk that cannot fit aborts immediately with `ErrChunkTooLarge`.

## Backpressure policy

- Producer `Push` waits only while either count or byte capacity is exhausted.
- Consumer `Next` wakes waiting producers after each dequeue.
- If no capacity appears within the configured window, `Push` returns `ErrBackpressure`, discards the bounded queue, and cancels `Buffer.Context()` with the same cause.
- The upstream HTTP request and Adapter parser use `Buffer.Context()`, so overflow cancellation stops provider reads instead of continuing to accrue output and cost.
- No unbounded channel and no per-Chunk Goroutine is created.

## Terminal behavior

- Normal `Finish(nil)` preserves FIFO order, drains all accepted Chunks, then returns EOF.
- `Finish(err)` also drains accepted facts before returning the producer error, preserving partial-stream evidence.
- `Abort(cause)` is immediate: it discards queued Chunks, wakes both sides, and cancels the shared Context. Client disconnect, write timeout, shutdown, and policy cancellation use this path.
- The first Finish/Abort result wins; later writes fail closed.

## Verification

- Count-bound test proves a third producer blocks at capacity two and resumes only after consumer progress.
- Timeout test proves a full buffer waits for the configured window, returns `ErrBackpressure`, cancels the upstream Context, and releases all queued bytes.
- Oversize, invalid input, cancellation, Finish-error drain, and immediate Abort paths are covered.
- A concurrent 1,000-Chunk producer/consumer test preserves strict sequence and asserts observed Chunk/byte high-water marks never exceed configuration.
- Statistics expose only current/high-water counts, backpressure waits, and overflows; no model content becomes a metric label.
- Package statement coverage is 81.6%; the package passes 20/20 repeated runs.

Final repository synchronization, complete gates, commit, push, and GitHub Actions evidence are recorded in the development checklist only after the remote run succeeds.
