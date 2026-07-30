# P06-T01 Chat request parsing report

Date: 2026-07-30

## Scope

Implemented and routed the strict transport parser for
`POST /v1/chat/completions`. The parser validates body size, media type,
content encoding, JSON shape, logical model, messages, parameter types,
parameter relationships, nested tools, tool calls, multimodal parts, and
structured-output declarations.

## Security and compatibility decisions

- Limit both fixed-length and chunked bodies to 1 MiB.
- Reject compressed bodies before parsing.
- Reject duplicate members recursively and reject every unknown field.
- Keep public errors independent from raw JSON, decoder errors, prompts, and
  other client content.
- Accept `max_tokens` as a compatibility alias while preferring
  `max_completion_tokens`; never guess when both are supplied.
- Return an explicit 501 after successful parsing until P06-T02 and the
  execution pipeline are connected.

## Evidence

- Package test coverage after implementation: 81.6% statements.
- Tests cover a full valid request, legacy compatibility, malformed JSON,
  duplicate fields, nested unknown fields, type/range failures, role/content
  invariants, tool schemas and arguments, structured output, both body-limit
  paths, authentication placement, method handling, and non-leaking errors.
- The authoritative contract is updated in `api/openapi.yaml`; detailed parser
  behavior is documented in `docs/api/chat-request-validation.md`.

The commit hash, stability run, complete repository checks, push, and GitHub
Actions run are recorded in the execution checklist after the CI gate passes.
