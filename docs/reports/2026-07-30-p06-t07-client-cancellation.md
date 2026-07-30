# P06-T07 Client cancellation propagation report

## Result

Client cancellation now has an automated end-to-end control-flow proof across the non-stream Gateway boundary: the inbound request Context reaches the selected executor and real upstream HTTP request, releases an active provider response body, maps to public 499 `REQUEST_CANCELLED`, and durably terminates both GatewayRequest and RouteAttempt as `cancelled/client_cancelled`.

## Guarantees

- The Gateway passes the inbound Context unchanged to routing and the one-attempt executor.
- Adapter request construction uses that Context, and the shared HTTP client clones the request without replacing it.
- Cancellation while an upstream response body is active releases the real `httptest.Server` request Context; tests wait for the upstream release signal and enforce a one-second upper bound.
- `NonStreamExecutor` retains `errors.Is(context.Canceled)` instead of reducing cancellation to an opaque transport or protocol error.
- The Gateway maps cancellation to an `AttemptCancelled`/`RequestCancelled` outcome with `client_cancelled` and stable code `CLIENT_CANCELLED`.
- Final recording uses a detached two-second deadline so the already-cancelled inbound Context does not prevent durable audit evidence.
- The public response contains no provider body, network cause, endpoint, credential, or database detail.

## Verification matrix

- Gateway unit test: cancel after `StartAttempt`, executor unblocks, detached recorder Context remains live and bounded, outcome is `client_cancelled`, public status is 499.
- Proxy real-HTTP test: cancel after response headers while body read is active, executor and provider Handler both unblock within one second.
- PostgreSQL integration test: active connecting Attempt transitions to cancelled together with its parent Request; both terminal reasons equal `client_cancelled` and no false `headers_received` fact is created.
- Existing upstream client and Adapter cancellation tests remain part of the full repository gate.
- A repeated run exposed and fixed a cancellation-versus-EOF race that could previously surface as `provider protocol failed`; Executor error boundaries now check the Context terminal state before accepting Adapter/HTTP parse classification and retain both `context.Canceled` and a trusted cancellation cause.
- The active-upstream cancellation case passes 100/100 repeated runs; the broader cancellation set passes 20/20, with Proxy/Gateway statement coverage at 86.1%/81.0%.

Final full-gate, repeated stability, exact synchronization, commit, push, and GitHub Actions evidence are added to the development checklist only after the remote run completes.
