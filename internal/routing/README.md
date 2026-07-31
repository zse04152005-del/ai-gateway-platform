# routing

`routing` owns provider-independent deployment selection. P08-T01 evaluates
every returned deployment with a deterministic, first-failure policy:

1. tenant/key model allowlist;
2. logical-model and concrete-request capabilities;
3. allowed region;
4. logical model, binding, deployment, and provider lifecycle status;
5. health;
6. budget;
7. capacity.

`CandidateFilter.Filter` returns a safe `FilterResult`. Each decision contains
only the deployment ID, an eligibility bit, and a finite `FilterReason`; it
never contains endpoint URLs, secret references, provider errors, prompts, or
request content. `DecisionFor` supports direct lookup, while `Clone` permits
alias-free storage and asynchronous observation. Persistent route-decision
storage remains a P08-T08 responsibility.

The selector never reads credentials, sends HTTP, retries, reserves budget or
capacity, or mutates health. `BudgetReader` and `CapacityReader` receive a
minimal secret-free projection and are evaluated only after all earlier rules
pass. Reader errors fail closed under stable error classes. The compatibility
constructor uses explicit allow-all bootstrap readers until the owning budget
and capacity stages install production readers.

Candidate ordering is ascending binding priority, then provider code,
deployment code, and deployment ID. It sorts locally even when the source is
already ordered so an alternate source cannot silently change policy. All
candidates are evaluated for an explainable report before one of three
versioned P08-T02 selection modes runs:

- `fixed`: select only the configured eligible deployment; never silently
  substitute another deployment;
- `priority`: select the first eligible candidate in stable order;
- `weighted`: draw over all eligible candidates using each binding's positive
  weight and stable cumulative intervals.

`PolicyResolver` receives only tenant, project, and logical-model identity.
Resolved policies are validated before use. `RandomSource` makes the weighted
draw injectable; `NewSeededRandom` provides a deterministic, mutex-protected
PCG stream for repeatable and race-safe tests. The process default uses a
concurrency-safe non-cryptographic source because a load-distribution draw is
not a security decision.

Every `Selection` carries a safe `PolicyDecision`: policy version, mode,
selected deployment ID, priority, weight, eligible count, and—only for a real
multi-candidate weighted choice—total weight and random draw. This is enough to
replay interval selection without exposing content or infrastructure secrets.
P08-T08 owns persistence; published multi-tenant policy storage is deliberately
outside the process environment configuration.

`PassiveHealth` is the P08-T03 production bootstrap `HealthReader`. It keeps a
bounded, per-deployment ring of time buckets and records request totals, HTTP
429, HTTP 5xx, provider timeouts, caller cancellations, other failures, TTFT,
and total latency. Snapshots expose average, maximum, and fixed-histogram
P50/P95/P99 upper bounds without retaining request content.

Only success, 429, 5xx, and timeout outcomes count as health samples. Caller
cancellation and configuration/protocol failures remain visible in request
totals but cannot satisfy the minimum sample or dilute the provider failure
ratio. A deployment is `warmup` and eligible below the sample floor; it becomes
`degraded` only when a sufficient sliding-window sample reaches the configured
failure ratio. Expired buckets naturally return it to warmup so passive-only
signals cannot blacklist a deployment forever. P08-T05 later owns explicit
circuit state and half-open probing.

The deployment map has deterministic least-recently-observed eviction and a
hard size bound. `ActiveCatalogHealth` remains an explicit compatibility/test
implementation and is not presented as measured health.

P08-T04 combines `PassiveHealth` with the independent `activehealth.Tracker`
through `CompositeHealth`; both signals must allow a deployment. Active state
uses three consecutive failures to remove a route and two consecutive
successes to recover it. Unknown or expired active evidence fails open so an
outage in the monitoring path cannot blackhole every production route; the
passive signal and the later circuit breaker remain independent safeguards.

`PassiveHealth.NeedsActiveProbe` also acts as the cost gate: any real success,
429, 5xx, or timeout still present in the passive window suppresses the paid
active request. Active probes therefore concentrate on cold/backup routes and
resume automatically when passive samples expire. Cancellation and local
configuration/protocol failures do not suppress a probe because they are not
provider-health samples.

P08-T05 adds a third independent signal, `CircuitBreaker`, with a versioned
`closed → open → half_open → closed` state machine. Closed opens after five
consecutive attributable Provider failures; a success resets the consecutive
counter, while caller cancellation and local/auth/request failures are ignored.
Open denies selection for 30 seconds. Expiry moves the record to half-open,
where routing advertises availability only while a slot may exist and the
Attempt executor performs the authoritative atomic reservation.

Half-open defaults to two concurrent permits and requires two successful
completions. Any attributable failure immediately reopens with a new cooldown.
Permits carry the state Generation and complete exactly once; late completions
from an older Generation are ignored so they cannot close a re-opened circuit
or underflow current in-flight accounting. The bounded map evicts only idle
Closed entries. If all 10,000 entries are Open, Half-Open, or in flight, new
state allocation fails closed with `ErrCircuitCapacity` rather than forgetting
an unhealthy deployment.

Catalog records are structurally and relationally validated before policy
evaluation, without treating a valid disabled status or a policy mismatch as
corrupt data. Cross-tenant facts, broken relationships, duplicate deployment
candidates, malformed records, and more than 256 candidates fail closed as an
untrusted source.

Failure classes are finite:

- `ErrNoCandidate`: no authorized, compatible, healthy deployment exists;
- `ErrCandidateSource`: catalog query or stored facts are untrustworthy;
- `ErrHealthUnavailable`: the health dependency cannot decide safely;
- `ErrBudgetUnavailable`: the budget dependency cannot decide safely;
- `ErrCapacityUnavailable`: the capacity dependency cannot decide safely;
- `ErrPolicyUnavailable`: the policy dependency or resolved policy is untrustworthy;
- `ErrRandomUnavailable`: weighted selection could not obtain an in-range draw.

The Gateway maps no-candidate to `503 MODEL_UNAVAILABLE` and infrastructure
failures to `503 ROUTING_UNAVAILABLE`. Neither response includes catalog,
database, endpoint, provider, or health-system details.
