# retry

`retry` owns one pure, fail-closed classification step between a failed physical
Attempt and the P08-T07 failover orchestrator. It never selects a Deployment,
starts an Attempt, sleeps, mutates health, reserves money, or retains an error.

Every call must explicitly provide the failed Attempt number, maximum Attempt
count, current time, request-wide deadline, minimum useful next-Attempt window,
additional-cost permission, upstream submission state, and the irreversible
client-output fact. The result is one of:

- `no_retry`;
- `retry_allowed`;
- `different_deployment_only`.

The client-visible model-output boundary always wins. Authentication,
permission, request validation, context length, content policy, caller
cancellation, unknown errors, and local Adapter construction failures never
retry. Trusted 429, capacity, timeout, temporary 5xx, transport, protocol, and
first-token timeout facts remain subject to the Attempt, cost, total-deadline,
minimum-window, and `Retry-After` gates.

Timeout, temporary 5xx, and transport failures are restricted to a different
Deployment whenever submission is confirmed or unknown, limiting repeated
calls to one uncertain upstream target. Protocol and first-token failures are
always different-Deployment-only. Rate-limit and capacity responses may retry
after their bounded provider hint because they are explicit rejection facts.

`Decision` contains only finite enums, bounded counters, rounded-up
milliseconds, and submission/output booleans. It never contains `error`, raw
Provider bodies, endpoints, request/model content, secret references, or
Provider request IDs, so P08-T08 can persist it safely.
