# Active deployment health

`activehealth` provides the P08-T04 low-frequency backstop for deployments that
may not currently receive enough production traffic for passive health to be
representative.

- PostgreSQL publishes only active, chat-capable deployments that back an active
  logical model for an active tenant. Secret material is never listed.
- A deterministic startup spread and ±20% stable cadence jitter prevent a fleet
  restart from creating a provider probe storm.
- Each probe sends the fixed text `ping`, requests at most one output token, has
  its own five-second timeout, four-worker/16-item batch bounds, and uses a
  dedicated HTTP transport and connection pool.
- Probe requests carry `X-AI-Gateway-Traffic-Class: active-health/v1` and bypass
  the public Handler, authentication, production Attempt recorder, metering,
  retries, and passive-health observer.
- Three consecutive failures mark a target unhealthy. Two consecutive successes
  are required to recover. Unknown and stale state fail open because a broken
  monitoring path must not blackhole every production route; passive health
  remains an independent signal.
- Snapshots and scheduler counters contain identifiers, finite reason codes,
  timestamps, durations, and counts only. Provider bodies, prompt/response
  content, endpoints, and credentials are excluded.
