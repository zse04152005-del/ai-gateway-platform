# Gateway execution records

## Purpose

The gateway records one durable `GatewayRequest` for the authenticated client operation and one independent `RouteAttempt` for every physical provider call. This layer answers which trusted scope initiated a call, which deployment was charged, which legal states were observed, and whether usage was present, without retaining prompts, responses, credentials, endpoints, or raw provider evidence.

Migration `000007_create_gateway_execution_records` is the PostgreSQL source of truth. `internal/execution.Recorder` is the application port and `PostgresRecorder` is its compare-and-swap implementation.

## Data boundaries

`gateway_requests` stores the correlation request ID, trusted tenant/project/virtual-key IDs, logical model, trace/span IDs, current state, attempt count, timestamps, terminal reason, and optimistic version.

`route_attempts` stores a generated UUID, parent request ID, monotonic attempt number, selected deployment ID, current state, response timing facts, bounded provider request ID, terminal classification, presence-preserving usage summary, and optimistic version.

The following data is deliberately excluded:

- prompt and response content;
- complete Virtual API Keys or Provider secrets;
- provider endpoint and private adapter configuration;
- provider error body/message and private database causes;
- raw usage evidence, which belongs to the immutable P10 ledger.

## State machines

The current non-stream path uses:

```text
GatewayRequest: authorized -> routing -> running -> succeeded|failed|cancelled
RouteAttempt:   created -> connecting -> [headers_received] -> succeeded|retryable_failed|failed|cancelled
```

Future reservation and streaming states already have database vocabulary, but application methods expose only transitions implemented in the current phase. `StartAttempt` atomically advances the request, increments `attempt_count`, inserts the attempt, and records `created -> connecting`. `CompleteAttempt` atomically records optional `headers_received`, the terminal attempt, and the terminal request.

Every update requires the expected `status + version`. PostgreSQL triggers independently reject identity mutation, skipped versions, time reversal, invalid attempt-count changes, illegal state edges, and terminal overwrite. Trigger-maintained request and attempt event tables retain one append-only fact per version.

## Gateway ordering and failure policy

The executable handler applies this order:

```text
authenticate -> parse -> normalize -> StartRequest -> MarkRouting
-> select deployment -> StartAttempt -> provider call -> CompleteAttempt
-> write client response
```

If durable recording fails before the provider boundary, the handler returns `503 EXECUTION_RECORD_UNAVAILABLE` and does not call the provider. If terminal recording fails after a provider response, the handler still returns the same safe 503 and leaves the active request/attempt for later reconciliation instead of claiming an unrecorded success.

Terminal recording uses a two-second context derived with `context.WithoutCancel`. This lets a client cancellation stop provider I/O while still giving PostgreSQL a bounded opportunity to persist `client_cancelled`.

## Verification

Unit tests cover trusted input and outcome validation, UUID version/variant generation, provider request ID safety, missing-versus-zero usage, raw-evidence exclusion, stable private-error rendering, gateway call ordering, provider/cancellation classification, and fail-closed recorder boundaries.

`tests/integration/gateway_execution_test.go` uses real PostgreSQL to verify complete success and provider-failure sequences, status event versions, CAS conflicts, duplicate attempt numbers, mixed-scope foreign keys, illegal direct transitions, and terminal immutability.
