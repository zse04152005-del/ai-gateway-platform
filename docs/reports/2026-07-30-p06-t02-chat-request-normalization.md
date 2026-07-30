# P06-T02 Chat request normalization report

Date: 2026-07-30

## Scope

Implemented deterministic conversion from the P06-T01 transport value to
`adapter.NormalizedRequest`, with a gateway-owned idempotency envelope. The
conversion preserves trusted Request ID, logical model, end-user reference,
messages, image detail, tool calls/results, sampling controls, output limit,
stop sequences, tool schemas/choice, response format, stream mode, and the
optional Idempotency-Key.

## Safety properties

- Missing trusted correlation fails closed.
- Idempotency is a single bounded opaque value, remains outside provider
  requests, and is represented only as a presence boolean in logs.
- Mutable pointers, slices, tool arguments, and schemas are defensively copied.
- The final provider-neutral request must pass domain validation.
- OpenAI and Mock adapters preserve end-user and image-detail semantics instead
  of silently discarding them.
- Logs were tested against prompt, image URL, end-user, and idempotency markers.

## Evidence

- Gateway package coverage: 83.2% statements.
- Tests cover full semantic mapping, deep-clone isolation, safe logging,
  optional/invalid/multiple idempotency keys, missing trusted correlation,
  invalid normalized facts, HTTP error rendering, and both adapter mappings.
- Full repository tests and both lint configurations pass before the stability
  and repository synchronization gates.

Commit, 20-round stability, SHA-256 synchronization, complete checks, and the
GitHub Actions run are recorded in the execution checklist after CI passes.
