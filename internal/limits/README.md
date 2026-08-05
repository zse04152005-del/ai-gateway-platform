# Local limits

P09-T02 provides one process-local, non-blocking admission layer. Each request is checked atomically against Platform, Tenant, Project and VirtualKey counters using only P09-T01 `limitpolicy.Effective` values. A denial consumes no RPM, TPM or concurrency capacity at any scope.

RPM and TPM use deterministic UTC minute windows; concurrency uses idempotent `Lease.Release`. Soft boundaries admit and return structured threshold facts. Hard RPM/TPM rejections include the local reset time; concurrency has no fabricated time-based retry promise.

`Replace` publishes a strictly newer complete snapshot under the same lock as admission. Unchanged scopes retain current window usage and live concurrency, so lowering a limit cannot reset counters or cancel existing work; it only blocks new admission. Missing scopes and invalid/stale snapshots fail closed.

P09-T03 adds `RedisRPMLimiter`: one Redis TIME-validated minute Hash and Lua script first check all four hard limits, then increment all four fields. Window mismatch retries occur before any mutation; `PEXPIREAT` uses the Redis server's absolute next-minute boundary plus bounded retention. Corrupt counters and unknown protocol shapes fail closed.

P09-T04 adds an explicit versioned input-estimator contract and `RedisTPMLimiter`. P10-T07 extends every estimate and reservation with tokenizer, physical-model, Deployment-version and provider-protocol identity; `tokenestimate.BoundInputEstimator` is the shared production implementation. A reservation is estimated input plus request/deployment maximum output, atomically held at all four scopes in the Redis-authoritative minute, and bound to a unique ID plus Scope fingerprint. Terminal settlement replaces reserved TPM with primary actual input plus output: unused tokens are released, overage is recorded even beyond hard so later admission stops, and identical retries are idempotent. Cache, reasoning and audio meters are never implicitly added. Settlement always references the original window and never extends its absolute expiry.

P09-T05 adds `RedisConcurrencyLimiter`: four same-slot sorted sets hold one server-time-expiring Lease member at Platform, Tenant, Project and Key. Acquire cleans expired members before an all-or-nothing hard check; Renew extends complete active leases; Release removes all four members for normal, failed and cancelled terminal paths. Metadata binds the ID to a Scope fingerprint and one-way active/released/expired state. A crashed process stops renewing, so its expired members are reclaimed atomically by the next operation without a background reaper.

Local admission remains early single-instance protection and may keep the pessimistic estimate until its minute resets; successful local admission is never proof of global capacity. All layers preserve canonical all-or-nothing scope order and fail closed on missing policy or infrastructure facts.
