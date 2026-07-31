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

## Timeout controller

`TimeoutController` owns one cancellable Context from before the upstream dial until the stream terminates:

1. Create it before Adapter request construction and use `Context()` for both `BuildRequest` and `upstreamhttp.Client.DoStream`.
2. After a usable Provider HTTP response and `OpenStream`, call `Attach`. This exact transition records `HeadersReceivedAt` and starts the first-model-token clock.
3. Read only through the returned `GuardedStream`. Content, reasoning, and tool deltas are the only events that establish client-visible model output.
4. Close the controller on every exit path. Timeout, caller cancellation, EOF, and parser failure also close the attached stream automatically.

Provider message-start/heartbeat/extension/usage events are upstream progress facts but do not satisfy the first-token deadline. `RecordGatewayHeartbeat` records a gateway-owned keepalive without changing either deadline. After the first model delta, every real upstream event resets no-progress; gateway heartbeats never do. The independent total timer starts at controller creation and therefore also bounds adapter build, dial, TLS, and response-header time.

`TimeoutFailure` is content-free and preserves the policy boundary:

- first-token timeout after headers and before output: retry/failover eligible for P08;
- no-progress timeout after output: partial failure, never transparent failover;
- total timeout: no additional retry budget; it is partial only when output already started.

Every controller-owned timeout cancels the same Context used by upstream HTTP, closes the Adapter stream/response Body, and remains discoverable through `errors.Is`, `Failure`, and `Snapshot` even when closing the socket causes a lower-level read error.

## Optional gateway heartbeat

`Heartbeat` is a request-scoped, blocking runner that writes the fixed SSE comment `: gateway-heartbeat` through `sse.Writer`. It never writes a `data:` field, creates a `NormalizedChunk`, changes model Sequence, or contributes to Usage.

- The platform owns the interval (10 ms～5 min at the component boundary); clients can only send `X-AI-Gateway-SSE-Heartbeat: on|off` and cannot request a frequency.
- `ResolveHeartbeatOptions` treats an absent header as enabled, accepts exact lowercase `on`/`off`, and rejects ambiguous/custom values.
- Disabled mode creates no ticker and touches neither the writer nor timeout recorder.
- Enabled `Run` owns and stops exactly one ticker, creates no hidden Goroutine, and exits on Context cancellation or the first write/recording failure.
- A heartbeat is counted only after `WriteComment` succeeds and Flush completes. Recording it via `TimeoutController.RecordGatewayHeartbeat` does not advance first-token or no-progress state.
- `HeartbeatSnapshot` exposes only start/last-send times and counts, never model or Provider content.

The eventual stream executor may run `Heartbeat.Run` in one explicitly owned Goroutine. `sse.Writer` serializes complete model and comment events, while the shared request Context guarantees cancellation and cleanup.

## Client cancellation evidence

`GuardedStream.Next` registers an `AfterFunc` on the client/request Context before entering a potentially blocking Adapter read. Cancellation atomically establishes the terminal cause, cancels the controller/upstream Context, and calls `ChunkStream.Close` to unblock a decoder even if the implementation has not yet returned from `Next`.

The terminal return path calls the same `sync.Once`-protected close operation before returning to its caller. Therefore, after a cancelled `Next` returns, `TimeoutSnapshot` contains:

- `CancellationObservedAt`;
- `UpstreamReleasedAt`, recorded after Adapter `Close`/Body close returns;
- non-negative `CancellationPropagation` between those two local lifecycle facts.

These fields contain no endpoint, content, client cause text, or Provider response. A remote Provider's actual billing stop cannot be guaranteed by TCP cancellation; real-HTTP tests separately require the upstream server request Context to observe cancellation within one second.

## Irreversible failover boundary

`FailoverGate` owns the concurrency boundary between replacing a physical upstream Attempt and committing normalized model output to the existing client response:

- only a controller-produced first-token `TimeoutFailure` can request the next Attempt;
- HTTP headers, message-start, Provider heartbeat, Gateway heartbeat, Usage, end and extension facts do not commit model output;
- content, reasoning and tool deltas atomically commit the response before the downstream sink is invoked;
- after commitment, even a contradictory/stale retry permit cannot invoke a backup starter;
- once a replacement Attempt is reserved, the predecessor token is stale and its late Chunk cannot reach the sink;
- the first sink error is treated conservatively as a possibly partial client write, so transparent failover remains forbidden;
- a hard 1～32 Attempt bound is enforced here; P08 adds total-time, retry taxonomy, budget and routing policy outside this semantic guard.

`Forward` serializes client events and deep-copies each Chunk. The gate snapshot exposes only Attempt counts, model-output state, forwarded event counts and denial counts; it contains no model content, Provider error or Deployment ID.
