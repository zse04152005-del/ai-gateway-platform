# P07-T02 downstream SSE writer report

## Result

`internal/sse.Writer` now provides the downstream commit boundary for Gateway streaming. It writes protocol-correct SSE events through `net/http` with explicit per-event deadlines, immediate flush, safe response headers, terminal-state enforcement, and stable disconnect/error classification.

## Response contract

- Requires a safe 8–128 byte Request ID and writes it as `X-Request-Id`.
- Sets `Content-Type: text/event-stream; charset=utf-8`, `Cache-Control: no-cache, no-store`, `X-Accel-Buffering: no`, and `X-Content-Type-Options: nosniff`.
- Removes `Content-Length` and does not add the HTTP/1-only `Connection` header, preserving HTTP/2 correctness.
- Validates both Flush and `SetWriteDeadline` capability through the standard `http.ResponseController`/`Unwrap` contract before headers are mutated.

## Event and timeout behavior

- `WriteData` normalizes CRLF/CR boundaries and prefixes every logical line with `data: `.
- `WriteJSON` uses bounded JSON encoding; `WriteComment` produces heartbeat-safe comment lines.
- Each complete event gets a new deadline of `now + writeTimeout`, is fully written, immediately flushed, and then has its deadline cleared.
- Data is limited to 256 KiB and comments to 4 KiB at this boundary; invalid UTF-8, empty data/comment, NUL comments, and oversized values fail before any bytes are written.
- `WriteDone` emits `data: [DONE]` once and permanently closes the logical writer.

## Failure semantics

- Request Context cancellation and closed/reset connections map to `ErrClientDisconnected`.
- deadline/network timeout maps to `ErrWriteTimeout`.
- unsupported controller capability, invalid input, terminal/prior failure, and unclassified writes have separate sentinel errors.
- Error strings are stable and omit raw socket/private writer details, while wrapped causes remain available to trusted diagnostics.
- Any cancellation or write/flush/deadline failure makes subsequent writes return `ErrWriterClosed`.

## Verification

- A real `httptest.Server` test reads the first data line before the handler returns, proving timely Flush, then closes the response and verifies the handler observes `ErrClientDisconnected`.
- Deterministic controller tests verify all headers, exact multiline framing, four events/four flushes, deadline set/clear pairs, terminal behavior, timeout/disconnect/general error classification, and pre-write validation.
- The combined decoder/writer package has 89.5% statement coverage and passes 20/20 repeated runs.

Final repository synchronization, complete gates, commit, push, and GitHub Actions evidence are recorded in the development checklist only after the remote run succeeds.
