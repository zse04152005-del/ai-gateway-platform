# Deployment circuit breaker

Status: Implemented by P08-T05

## State machine

Each physical Deployment has an independent, process-local circuit:

```text
Closed --5 consecutive failures--> Open
Open --30 second cooldown--> Half-Open
Half-Open --2 successes--> Closed
Half-Open --1 failure--> Open
```

A Closed success resets consecutive failures. Ignored outcomes release the
Attempt Permit without resetting or increasing failure evidence. This preserves
a real consecutive sequence across caller cancellations while keeping
authentication, request validation, and local configuration failures from
punishing Provider health.

The default policy is `circuit-breaker/v1`; thresholds are constructor options
with strict safe bounds so later versioned configuration can replace defaults
without changing state semantics.

## Two-phase admission

Candidate filtering calls `Healthy`:

- Closed is eligible;
- Open is ineligible until its exact `RetryAt`;
- expired Open transitions to Half-Open;
- Half-Open is eligible only if an in-flight slot may be available.

That read is not a reservation because the selector evaluates every candidate
before fixed/priority/weighted policy chooses one. Reserving inside filtering
would leak slots for candidates that were never selected.

`CircuitChatExecutor` performs the authoritative `Acquire` immediately before
the selected physical Attempt. Under one mutex it transitions expired Open,
rejects current Open, and caps Half-Open permits. A race after selection can
therefore return `ErrHalfOpenSaturated`, but it cannot send an unbounded probe.
The public response for Open, saturation, or state-capacity denial is the same
retryable 503 `MODEL_UNAVAILABLE`; internal state is not exposed.

## Generation-safe completion

Every Permit captures the circuit Generation and uses an atomic exactly-once
completion bit. Open, Half-Open, re-open, and close transitions advance the
Generation. When one Half-Open failure reopens the circuit, later successes from
other already-running permits belong to the old Generation and are ignored.
They cannot close the new circuit or decrement its fresh in-flight count.

Closed permits are also counted in flight. The bounded state map may evict only
idle Closed records, never Open, Half-Open, or records with outstanding
completions. If the 10,000-entry default is full of protected records, new
allocation returns `ErrCircuitCapacity` and fails closed instead of silently
forgetting unhealthy state.

## Failure attribution

The Gateway maps completed results to three outcomes:

| Circuit outcome | Attempt result |
|---|---|
| succeeded | complete valid Provider response |
| failed | 429, capacity, timeout, 5xx, Provider protocol failure, or transport failure |
| ignored | caller cancellation, auth/permission, invalid request/context, content policy, non-retryable unknown, local Adapter configuration |

The mapping contains finite normalized categories only. Raw Provider errors,
bodies, endpoints, credentials, prompts, and responses never enter the circuit
snapshot. Completion bookkeeping failure increments a local counter but cannot
replace the response/error already produced by the physical Attempt.

The circuit wrapper is outside passive observation. Admission rejection creates
no Provider traffic and no passive sample; admitted traffic remains visible to
the passive window and durable Attempt recorder.
