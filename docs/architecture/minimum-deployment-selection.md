# Minimum deployment selection

Status: Implemented by P06-T03

## Data flow

1. Key authentication creates trusted tenant, project, and optional model
   allowlist scope.
2. Request parsing and normalization produce a validated logical model request.
3. `catalog.PostgresStore.ListRouteCandidates` joins the project grant,
   logical model, binding, deployment, and provider inside that scope.
4. SQL filters every lifecycle record to active and orders by binding priority
   plus stable identifiers. A hard limit prevents unbounded route sets.
5. The selector revalidates all catalog records and relationships, derives
   request-specific capabilities, sorts deterministically, and asks the injected
   health reader only for compatible candidates.
6. The first healthy candidate becomes the first-attempt selection.

The query returns complete Provider, LogicalModel, Binding, and Deployment
domain records because the next stage must build an adapter without a second,
potentially inconsistent catalog lookup. It may include a secret reference ID,
but never an encrypted envelope, external locator value, or plaintext secret.

## Request capability filter

In addition to the logical model contract, the selector derives requirements
from the actual request: chat, streaming, tools/tool results, structured output,
vision, audio input, and the requested maximum output tokens. This prevents an
optional feature from reaching a deployment that only satisfies the logical
model's minimum declaration.

## Failure semantics

An empty authorized set, capability mismatch, or all-unhealthy set returns
`MODEL_UNAVAILABLE`. Catalog errors, corrupt relationship facts, candidate
overflow, and health dependency errors return `ROUTING_UNAVAILABLE`. Both are
retryable 503 responses with a bounded retry hint; internal causes are retained
for trusted diagnostics but are not serialized.

## Current boundary

P06 uses `ActiveCatalogHealth`, which truthfully means “active in the catalog,”
not “recent probes succeeded.” P08 adds passive metrics, active checks, circuit
states, retry classification, and route-decision explanation. Keeping health as
an injected interface prevents the P06 bootstrap assumption from becoming a
hidden permanent policy.
