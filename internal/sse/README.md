# Bounded SSE decoder

`internal/sse` owns provider-neutral SSE framing. It incrementally handles arbitrary network fragmentation, LF/CRLF, optional UTF-8 BOM, empty separators, comment heartbeats, standard `event`/`data`/`id`/`retry` fields, multiline data joining, EOF dispatch, and OpenAI-compatible `[DONE]` recognition.

Both physical-line and aggregate-event limits are mandatory. The aggregate limit counts comments and metadata as well as data, preventing a provider from bypassing the bound with many non-data lines. Unknown fields, invalid retry values, NUL identifiers, overlong lines, and oversized blocks return stable sentinel errors without including upstream bytes.

The decoder does not own the response body or cancellation policy. The Adapter owns `Close`, closes the body to unblock a read when its Context is cancelled, and maps decoder sentinel errors into its own safe `ProtocolViolation` codes.

## Downstream writer

`Writer` validates Flush and write-deadline support before mutating headers. It sets the bounded SSE/no-buffering/no-sniff response headers, removes `Content-Length`, prefixes every data/comment line, applies a fresh write deadline to each complete event, flushes immediately, and clears the deadline after success. `WriteDone` is terminal and can run once.

Timeout, client disconnect, unsupported controller, closed writer, invalid input, and other write failures have distinct safe sentinel errors. The raw socket error is retained for `errors.Is`/diagnostics but never appears in `Error()`. A cancelled request Context is checked before writing and permanently closes the writer, so no model bytes are written after a known disconnect.
