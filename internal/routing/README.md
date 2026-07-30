# routing

`routing` owns provider-independent deployment selection. P06-T03 implements
the minimum deterministic policy: query candidates within the trusted
tenant/project/key scope, filter the capabilities required by the concrete
request, evaluate health in priority order, and choose the first eligible
deployment.

The selector never reads credentials, sends HTTP, retries, or mutates health.
Candidate ordering is ascending binding priority, then provider code,
deployment code, and deployment ID. It sorts locally even when the source is
already ordered so an alternate source cannot silently change policy.

`ActiveCatalogHealth` is an explicit bootstrap implementation: a catalog-active
deployment is considered healthy unless the request context is cancelled. P08
replaces it with active/passive measurements and a circuit breaker without
changing the selector interface. It is not presented as measured health.

Failure classes are finite:

- `ErrNoCandidate`: no authorized, compatible, healthy deployment exists;
- `ErrCandidateSource`: catalog query or stored facts are untrustworthy;
- `ErrHealthUnavailable`: the health dependency cannot decide safely.

The Gateway maps no-candidate to `503 MODEL_UNAVAILABLE` and infrastructure
failures to `503 ROUTING_UNAVAILABLE`. Neither response includes catalog,
database, endpoint, provider, or health-system details.
