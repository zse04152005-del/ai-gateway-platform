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
candidates are evaluated for an explainable report; selection still returns
the first eligible candidate in this stable order.

`ActiveCatalogHealth` is an explicit bootstrap implementation: a catalog-active
deployment is considered healthy unless the request context is cancelled. P08
replaces it with active/passive measurements and a circuit breaker without
changing the selector interface. It is not presented as measured health.

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
- `ErrCapacityUnavailable`: the capacity dependency cannot decide safely.

The Gateway maps no-candidate to `503 MODEL_UNAVAILABLE` and infrastructure
failures to `503 ROUTING_UNAVAILABLE`. Neither response includes catalog,
database, endpoint, provider, or health-system details.
