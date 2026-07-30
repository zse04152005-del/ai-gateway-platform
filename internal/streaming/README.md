# Streaming flow control

`Buffer` is the bounded handoff between an upstream `provideradapter.ChunkStream` producer and a downstream SSE writer consumer.

- Limits both queued Chunk count and conservatively estimated resident bytes.
- Deep-copies accepted chunks so an Adapter cannot mutate queued facts.
- Blocks a producer only for the configured backpressure window.
- On timeout or a single oversize Chunk, discards the queue and cancels the derived Context with `ErrBackpressure` or `ErrChunkTooLarge`; the upstream HTTP request must use this Context.
- Normal producer completion drains queued chunks before EOF; producer error also drains accepted facts before returning the error.
- Explicit Abort is immediate and is used for client disconnect, write timeout, shutdown, and policy cancellation.
- Exposes current and high-water Chunk/byte counts, backpressure waits, and overflow count without content labels.

The queue never creates an unbounded channel or a per-Chunk Goroutine. P07 streaming execution composes this component with `internal/sse.Writer` and the Adapter-owned upstream `ChunkStream`.
