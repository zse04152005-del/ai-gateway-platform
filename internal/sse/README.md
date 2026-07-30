# Bounded SSE decoder

`internal/sse` owns provider-neutral SSE framing. It incrementally handles arbitrary network fragmentation, LF/CRLF, optional UTF-8 BOM, empty separators, comment heartbeats, standard `event`/`data`/`id`/`retry` fields, multiline data joining, EOF dispatch, and OpenAI-compatible `[DONE]` recognition.

Both physical-line and aggregate-event limits are mandatory. The aggregate limit counts comments and metadata as well as data, preventing a provider from bypassing the bound with many non-data lines. Unknown fields, invalid retry values, NUL identifiers, overlong lines, and oversized blocks return stable sentinel errors without including upstream bytes.

The decoder does not own the response body or cancellation policy. The Adapter owns `Close`, closes the body to unblock a read when its Context is cancelled, and maps decoder sentinel errors into its own safe `ProtocolViolation` codes.
