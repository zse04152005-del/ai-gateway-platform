# P06-T06 GatewayRequest / RouteAttempt delivery report

## Result

The non-stream gateway now creates a durable request before routing and a distinct attempt before each physical provider call. Success, provider failure, protocol failure, transport/timeout, cancellation, no-candidate, unimplemented streaming, and recorder dependency failures all terminate through explicit safe semantics.

## Delivered scope

- Migration 000007 creates current-state request/attempt tables, compound trusted-scope foreign keys, lifecycle constraints, supporting indexes, transition triggers, and append-only status event tables.
- `internal/execution` defines finite states, validated start/outcome facts, UUID attempt identity, optimistic handles, transactional recorder operations, safe database error kinds, and a bounded usage summary.
- The executable gateway requires a Recorder dependency and executes `StartRequest -> MarkRouting -> StartAttempt -> CompleteAttempt` around the existing selection and provider boundary.
- Recording failure before provider I/O returns `503 EXECUTION_RECORD_UNAVAILABLE` without provider access. A terminal write failure never returns a false success.
- Terminal writes get a detached, bounded two-second context so caller cancellation can still be recorded without creating an unbounded background operation.

## Security and accounting properties

- No content, credentials, endpoint, provider body, private database error, or raw usage evidence is stored.
- Tenant/project/key/model identity is protected by existing compound foreign keys.
- One physical call maps to one UUID attempt and `(request_id, attempt_no)` is unique.
- Missing token counts remain absent while an observed zero remains zero.
- Provider request IDs and public classifications are constrained to safe bounded characters.
- Database triggers prevent stale workers from overwriting terminal facts.

## Automated evidence

- Execution domain/recorder unit tests pass.
- Gateway unit tests verify lifecycle order, outcome classification, and all recorder fail-closed boundaries.
- The complete integration suite passes against local PostgreSQL migration version 7.
- The execution integration matrix covers success, provider error, headers received, CAS conflict, duplicate attempt number, cross-scope rejection, illegal transition, terminal overwrite, and event-version order.
- Combined unit plus real-PostgreSQL execution coverage is 83.2% of statements, and the execution/gateway/proxy/cmd-gateway stability set passes 20/20 runs.

Final repository gates, repeated stability runs, exact desktop synchronization, commit, push, and GitHub Actions evidence are recorded in the development checklist after remote CI completes.
