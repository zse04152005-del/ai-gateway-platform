# Metering Outbox

`meteringoutbox` relays immutable UsageEvent facts from PostgreSQL to the Kafka-compatible topic `ai-gateway.usage.v1` without adding event-bus latency to the request path.

- Attempt completion writes Outbox rows in the same transaction as its Request/Attempt terminal facts.
- Relay claims at most a bounded batch with `FOR UPDATE SKIP LOCKED`, then publishes outside the database transaction.
- Rows move `pending → publishing → published`; broker failure returns them to `pending` with bounded exponential backoff.
- Expired leases make crashed-worker claims recoverable across Gateway instances.
- Kafka acknowledgement followed by a process failure may cause a repeat, so the stable event ID is also the Kafka Key and consumers must deduplicate it.
- Persisted errors are finite safe codes. Broker internals and authentication material never enter the Outbox.

The Kafka topic must be provisioned before Gateway traffic. The producer requests all in-sync replica acknowledgements and keeps a bounded local buffer; this package does not enable broker auto-creation or claim end-to-end exactly-once delivery.
