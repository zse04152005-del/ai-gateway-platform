# Chat request normalization

P06-T02 converts the validated Chat Completions transport model into the
provider-neutral `adapter.NormalizedRequest`. The conversion happens after
authentication and correlation, but before deployment selection. It does not
read provider configuration, credentials, tenant IDs, or endpoints.

## Mapping

| Client fact | Normalized fact |
| --- | --- |
| Correlation context | `RequestID` |
| `model` | `LogicalModel` |
| `user` | `EndUserReference` |
| Message role/name/content | `MessageRole`, `ContentPart` |
| `image_url.url/detail` | `ContentImageReference.Reference/Detail` |
| Assistant function call | `ToolCall` with copied JSON arguments |
| Tool result | `RoleTool` plus `ToolCallID` |
| Sampling and output limit | Presence-preserving scalar pointers |
| Function tools | `ToolDefinition` with copied JSON Schema |
| Tool choice | finite `ToolChoiceMode` |
| Response format | finite `ResponseFormatType` and copied JSON Schema |

The result is validated with `NormalizedRequest.Validate()` before it can enter
routing. Every pointer, slice, and `json.RawMessage` is independently owned so a
route attempt cannot mutate the parsed input or another attempt.

## Idempotency boundary

`Idempotency-Key` is gateway execution metadata, not a provider option. It is
stored next to the normalized request and is never added to adapter HTTP
requests. The header is optional at this stage. If present it must be exactly
one 8-128 byte ASCII value matching
`^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`; whitespace, commas, control characters,
and multiple header values are rejected as `INVALID_IDEMPOTENCY_KEY`.

Later replay protection can key records by trusted tenant/project scope plus
this opaque value. A key is never trusted as identity and is not logged in
plaintext.

## Content and privacy

`EndUserReference` and image detail were added to the neutral contract so the
gateway does not silently discard valid client semantics. Both the OpenAI and
Mock adapters preserve these fields. The normalized request log view exposes
request ID, logical model, stream flag, and counts; the gateway wrapper adds
only booleans indicating whether end-user and idempotency values exist. It does
not expose prompts, image URLs, user references, idempotency values, tool
arguments, stop sequences, or schemas.

If trusted correlation is unexpectedly absent, normalization fails closed as a
generic internal error. Client errors and internal validation causes are never
serialized directly.
