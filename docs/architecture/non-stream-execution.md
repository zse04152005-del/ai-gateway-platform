# Non-stream execution boundary

Status: Implemented by P06-T05

## Ownership

- `gateway` owns client protocol projection and public error mapping.
- `routing` owns the authorized immutable Selection.
- `provideradapter.Registry` owns exact protocol-family construction.
- `proxy.NonStreamExecutor` owns one selected HTTP exchange.
- each Adapter owns bounded response-body consumption and protocol normalization.
- `upstreamhttp.Client` owns the process-wide connection pool and transport policy.

The executor never queries the catalog again. Provider, Deployment, Binding, and LogicalModel come from the same selected catalog fact, avoiding a time-of-check/time-of-use split between routing and adapter construction.

## Error containment

`executionError` exposes only a stable kind from `Error()` while retaining private causes through `Unwrap()` for `errors.Is/As`. A `ProviderError` contains only a previously validated `adapter.NormalizedError`; it has no raw body field. The Gateway maps finite categories to provider-neutral HTTP codes and messages, so changing Provider adapters does not change the public failure contract.

## Response containment

The public response uses the requested LogicalModel. Physical model identity and Provider Request ID remain internal attempt facts. Tool arguments are already validated JSON objects and are serialized as strings for OpenAI compatibility. Usage preserves missing-versus-zero: incomplete core counts suppress the public Usage object instead of manufacturing zero, while Gateway metadata declares completeness.

## Attempt boundary

One `NonStreamExecutor.Execute` invocation equals one upstream attempt. P06-T06 surrounds it with Request/Attempt state transitions. P08 may invoke it again only after retry classification and a fresh Selection; it must never hide multiple upstream charges behind a single attempt record.
