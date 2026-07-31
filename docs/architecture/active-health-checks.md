# Active health checks

Status: Implemented by P08-T04

## Problem

Passive statistics are accurate for hot deployments but cannot tell whether an
idle backup will work at failover time. Naively polling every model creates
three production problems: paid token waste, provider quota pressure, and a
thundering herd after gateway restarts. Treating monitoring failure as provider
failure can also remove every route at once.

## Target publication

`catalog.PostgresStore.ListHealthProbeTargets` publishes a deterministic,
bounded snapshot of physical deployments. A target must be active,
chat-capable, backed by an active Provider, and reachable through at least one
active Tenant → Project → Logical Model → Binding chain. A deployment shared by
many tenants is probed once because health belongs to the physical deployment.
The target contains a Secret Reference identifier but never decrypted secret
material.

The Scheduler validates the complete snapshot before atomically replacing its
target set. Duplicate IDs, malformed records, query failure, or more than
10,000 targets preserve the last valid set and increment a finite failure
counter.

## Cost-aware scheduling

Defaults are deliberately conservative:

| Control | Default |
|---|---:|
| Per-deployment interval | 5 minutes |
| Catalog refresh | 30 seconds |
| Dispatch resolution | 1 second |
| Deterministic cadence jitter | ±20% |
| Probe timeout | 5 seconds |
| Global workers | 4 |
| Batch ceiling | 16 |
| Per-host HTTP connections | 2 |

New targets receive a stable hash-derived startup phase across the full
interval. Later probes use a stable interval in the 80%～120% range, preventing
fleet restarts and multiple gateway replicas from aligning all deployments at
one instant.

Before spending a token, the Scheduler calls
`routing.PassiveHealth.NeedsActiveProbe`. A success, 429, 5xx, or provider
timeout in the current passive window suppresses the active request because
real traffic already measures that deployment. Cancellation and local failures
do not suppress it. When the passive window expires, cold-route probing resumes
automatically. Gate read failure skips the probe and increments `GateFailures`;
it never creates synthetic negative health evidence.

## Probe and isolation boundary

`AdapterProber` uses the real deployment-scoped Adapter so endpoint and secret
resolution follow the same protocol contract as production. The normalized
request is fixed:

- one user text part: `ping`;
- `temperature = 0`;
- `max_output_tokens = 1`;
- no tools, media, Provider options, tenant identity, or user content;
- `X-AI-Gateway-Traffic-Class: active-health/v1` marker.

The probe has a dedicated hardened HTTP transport and connection pool. It does
not traverse the public Gateway Handler, authentication, selection, retry,
durable Request/Attempt recorder, metering pipeline, or passive observer. This
provides application-accounting and network-pool isolation. Complete Provider
quota/billing isolation still requires a dedicated Provider credential in the
Secret Reference; the gateway does not claim that two requests using the same
Provider account have separate external quotas.

All failures collapse to one of: `timed_out`, `cancelled`,
`adapter_unavailable`, `transport_failure`, `provider_failure`, or
`protocol_failure`. Raw response bodies, error text, endpoints, credentials,
and prompt/output content never enter snapshots or scheduler statistics.

## Routing state and failure containment

The active tracker uses hysteresis:

- unknown → healthy/eligible until enough negative evidence exists;
- three consecutive failures → unhealthy;
- two consecutive successes → healthy again;
- evidence older than 20 minutes → stale and eligible.

Stale state fails open by design. A database outage, scheduler crash, or probe
credential problem must not blackhole every deployment. `CompositeHealth`
requires both current passive and current active signals to allow a route; P08-T05
adds explicit circuit state without merging monitoring failures into provider
facts.

Process cancellation stops dispatch, cancels in-flight probe contexts, waits
for workers, and closes the independent connection pool. Shutdown-driven
`cancelled` results are not recorded as provider failures.
