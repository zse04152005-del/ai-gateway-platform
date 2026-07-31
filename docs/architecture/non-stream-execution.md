# Non-stream execution boundary

Status: Implemented by P06-T05

## Ownership

- `gateway` owns client protocol projection and public error mapping.
- `routing` owns the authorized immutable Selection.
- `provideradapter.Registry` owns exact protocol-family construction.
- `proxy.NonStreamExecutor` owns one selected HTTP exchange.
- each Adapter owns bounded response-body consumption and protocol normalization.
- `upstreamhttp.Client` owns the process-wide connection pool and transport policy.
- `gateway.ObservedChatExecutor` owns best-effort passive-health classification
  around the exact physical attempt; `routing.PassiveHealth` owns the bounded
  sliding-window aggregate and eligibility view.
- `activehealth.AdapterProber` owns a separate one-token synthetic exchange for
  cold deployments. It never enters this production Attempt boundary, while
  `routing.CompositeHealth` combines its state with passive eligibility.
- `gateway.CircuitChatExecutor` owns the final pre-execution circuit Permit and
  result classification; `routing.CircuitBreaker` owns the state machine.

The executor never queries the catalog again. Provider, Deployment, Binding, and LogicalModel come from the same selected catalog fact, avoiding a time-of-check/time-of-use split between routing and adapter construction.

## Error containment

`executionError` exposes only a stable kind from `Error()` while retaining private causes through `Unwrap()` for `errors.Is/As`. A `ProviderError` contains only a previously validated `adapter.NormalizedError`; it has no raw body field. The Gateway maps finite categories to provider-neutral HTTP codes and messages, so changing Provider adapters does not change the public failure contract.

## Response containment

The public response uses the requested LogicalModel. Physical model identity and Provider Request ID remain internal attempt facts. Tool arguments are already validated JSON objects and are serialized as strings for OpenAI compatibility. Usage preserves missing-versus-zero: incomplete core counts suppress the public Usage object instead of manufacturing zero, while Gateway metadata declares completeness.

## Attempt boundary

One `NonStreamExecutor.Execute` invocation equals one upstream attempt and the Gateway surrounds it with durable Request/Attempt state transitions. P08 may invoke it again only after retry classification and a fresh Selection; it must never hide multiple upstream charges behind a single attempt record.

The observed executor never changes the wrapped response or error when a local
health observation is rejected. It increments a queryable local failure count
instead. Client cancellation is removed only from the in-memory observation
context, not from provider execution. Non-stream total latency is recorded, but
TTFT remains absent rather than being fabricated from whole-response latency.

Active probes deliberately do not call `NonStreamExecutor.Execute`: doing so
would make synthetic traffic indistinguishable from production attempts and
could contaminate usage, retries, route decisions, or request records. They use
the same validated Adapter contract but a dedicated HTTP client, fixed request,
finite result taxonomy, and independent tracker.

The circuit wrapper sits outside `ObservedChatExecutor`. A rejected Open or
saturated Half-Open reservation therefore produces no Provider call and no
passive observation. Once acquired, the inner executor runs exactly once and
the Permit completes from the same result. Permit completion uses a
non-cancelled local context so caller cancellation can release in-flight state,
but cancellation is classified as ignored provider-health evidence.
