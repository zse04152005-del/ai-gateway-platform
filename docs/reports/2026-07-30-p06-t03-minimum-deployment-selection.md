# P06-T03 Minimum deployment selection report

Date: 2026-07-30

## Scope

Implemented tenant-safe catalog candidate retrieval and deterministic first
healthy deployment selection. The Gateway now performs authentication, strict
parsing, normalization, and route selection before reaching the intentionally
unimplemented provider execution stage.

## Guarantees

- Candidate SQL binds trusted tenant, project, exact logical model, project
  grant, and optional Key allowlist; every lifecycle record must be active.
- Complete catalog records and relationships are validated after scanning.
- Candidate sets are bounded at 256 and sorted independently of source order.
- Concrete request capabilities filter deployments before health calls.
- Candidate pointer/slice fields are cloned for attempt isolation.
- No candidate maps to `MODEL_UNAVAILABLE`; catalog/health failures map to
  `ROUTING_UNAVAILABLE` without exposing their private cause.
- Bootstrap health is explicitly catalog-active only and remains replaceable.

## Evidence

- `internal/routing` statement coverage: 82.0%.
- Unit tests cover stable priority/tie ordering, unhealthy skipping, dynamic
  capability filtering, no-candidate, source and health errors, invalid catalog
  facts, candidate limits, context cancellation, constructor boundaries, clone
  isolation, and safe Gateway errors.
- A real PostgreSQL integration test verifies two priorities, active joins,
  project/Key authorization, cross-tenant scope, complete row scanning, and
  domain validation.
- Full repository tests and both lint configurations pass before stability,
  synchronization, commit, and CI gates.

The final commit, SHA-256 synchronization, 20-round stability, complete checks,
and GitHub Actions run are recorded in the execution checklist after CI passes.
