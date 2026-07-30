# Chat Completions request validation

`POST /v1/chat/completions` is an authenticated, strict JSON boundary. This
document describes the transport parser implemented by P06-T01. Routing and
provider execution remain separate auditable stages. P06-T05 now connects valid
non-streaming requests to the selected Adapter and unified response; streaming
requests still receive the explicit `501 CHAT_STREAMING_NOT_IMPLEMENTED` until
P07, rather than being silently downgraded or given a fabricated completion.

## Envelope limits

- `Content-Type` must resolve to `application/json`. A charset parameter is
  accepted.
- `Content-Encoding` may be absent or `identity`. Compressed bodies are rejected
  before decompression so a compression bomb cannot consume gateway memory.
- The request body is limited to 1 MiB for both known `Content-Length` and
  streaming/unknown-length bodies.
- The body must be valid UTF-8 and contain exactly one JSON object.
- Duplicate JSON object members are rejected recursively. The gateway never
  applies a first-wins or last-wins interpretation.
- Every object uses an explicit field allowlist. Unknown fields return
  `UNSUPPORTED_FIELD`; fields that could change semantics or cost are never
  silently ignored.

## Supported request subset

The parser accepts logical `model`, 1-1024 `messages`, `stream`, `temperature`,
`top_p`, `stop`, function `tools`, `tool_choice`, `response_format`, `user`, and
one output-token limit. Both `max_completion_tokens` and the deprecated
`max_tokens` alias are understood, but using both is an error.

Messages support the `system`, `developer`, `user`, `assistant`, and `tool`
roles. Content may be text or a strict list of `text` and `image_url` parts.
Assistant messages may replace content with function `tool_calls`; tool result
messages must carry the matching `tool_call_id`. Function arguments are strings
that must themselves contain exactly one JSON object.

Structured output supports `text`, `json_object`, and named `json_schema`
formats. Tool parameter schemas and response schemas are bounded at 64 KiB.

## Public errors

| HTTP | Code | Meaning |
| --- | --- | --- |
| 400 | `INVALID_JSON` | Invalid UTF-8, syntax, root type, or trailing JSON value. |
| 400 | `DUPLICATE_FIELD` | A JSON object repeats a member name. |
| 400 | `UNSUPPORTED_FIELD` | A field is outside the documented subset. |
| 400 | `MISSING_REQUIRED_FIELD` | A required field is absent. |
| 400 | `INVALID_PARAMETER_TYPE` | A field has the wrong JSON type. |
| 400 | `INVALID_PARAMETER` | A typed value violates its range or invariant. |
| 400 | `CONFLICTING_PARAMETERS` | Legacy and current output limits appear together. |
| 405 | `METHOD_NOT_ALLOWED` | The endpoint was called with a method other than POST. |
| 413 | `REQUEST_TOO_LARGE` | The body exceeds 1 MiB. |
| 415 | `UNSUPPORTED_MEDIA_TYPE` | The media type is not JSON. |
| 415 | `UNSUPPORTED_CONTENT_ENCODING` | A compressed request body was supplied. |

Errors expose only a bounded safe parameter path such as
`messages[0].content`; they do not echo message content, the original body, or
an internal decoder error.

## Stage boundary

The parser produces an internal validated transport value. P06-T02 now converts
that value into `adapter.NormalizedRequest`, adds the trusted correlation and
idempotency facts, and verifies normalized invariants before any deployment or
provider is selected. P06-T03～T05 then select one Deployment and execute one
non-streaming Attempt. See [Chat request normalization](chat-request-normalization.md)
and [non-stream Chat execution](non-stream-chat-execution.md).
