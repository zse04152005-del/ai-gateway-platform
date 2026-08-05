# Metering Consumer

`meteringconsumer` owns strict UsageEvent decoding, canonical semantic fingerprints, effective PriceRate selection, exact micros calculation and the PostgreSQL Receipt/Ledger idempotency transaction.

- Kafka Key must equal eventId; unknown fields, trailing JSON and payloads above 64 KiB fail closed.
- The first event inserts an immutable Receipt and exactly one priced Usage Ledger row in the same transaction.
- Same-ID/same-fact replays return the original price and amount without writing; same-ID/different-fact payloads are conflicts.
- Price selection verifies Tenant/Request/Attempt/Deployment/Region and requires a published version effective at `observed_at` plus an exact Token type and billing-unit rate.
- Kafka uses consumer group `ai-gateway-metering-v1`, at most 100 records per Poll, no auto-commit and synchronous per-record offset commit only after the database transaction succeeds.

The current failure policy leaves invalid or unavailable-price records uncommitted and stops the process with a safe error classification. P13 must add DLQ/alerting/runbook controls before production readiness; this package never skips a potentially billable fact silently.
