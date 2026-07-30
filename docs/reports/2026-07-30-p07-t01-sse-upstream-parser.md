# P07-T01 bounded upstream SSE parser report

## Result

The Mock and OpenAI Adapters now share one provider-neutral, incremental SSE framing implementation in `internal/sse`. Provider-specific JSON normalization remains inside each Adapter, while line boundaries, event assembly, metadata grammar, resource limits, and terminal-sentinel recognition have one tested implementation.

## Framing contract

- Reads incrementally and remains correct when the transport yields one byte at a time.
- Accepts LF and CRLF, an optional stream-leading UTF-8 BOM, repeated empty separators, and a final event terminated by EOF rather than a blank line.
- Joins repeated `data` fields with exactly one newline, including empty data segments.
- Recognizes comment-only blocks as heartbeat events; comments mixed into a data event never become model content.
- Accepts standard `event`, `id`, and numeric `retry` metadata without projecting it into model content.
- Recognizes the OpenAI-compatible `[DONE]` sentinel after bounded whitespace trimming.

## Fail-closed and resource behavior

- Every physical line is limited to 64 KiB in current Adapters.
- Every complete block is limited to 256 KiB, counting comments and metadata in addition to data; many non-data lines cannot bypass the bound.
- Unknown fields, non-numeric retry values, NUL-containing IDs, invalid event names, oversized lines, and oversized blocks return stable sentinel errors that contain no provider bytes.
- Adapter-owned Context cancellation still closes the upstream response body, unblocking a pending decoder read. Body ownership did not move into the framing package.
- Mock/OpenAI Adapters map shared sentinel errors back to stable, safe `ProtocolViolation` operation/code pairs.

## Verification

- `internal/sse` statement coverage: 92.2%.
- Mock Adapter: 85.3%; OpenAI Adapter: 82.3%; shared conformance suite: 83.8%.
- Shared decoder and both Adapter suites pass 20/20 repeated runs.
- Existing real-HTTP conformance fixtures continue to validate Role, Content, Tool, Finish, Usage, heartbeat, cancellation, unknown JSON fields, and strict EOF semantics.

Final repository synchronization, complete gates, commit, push, and GitHub Actions evidence are recorded in the development checklist only after the remote run succeeds.
