# Local limits

P09-T02 provides one process-local, non-blocking admission layer. Each request is checked atomically against Platform, Tenant, Project and VirtualKey counters using only P09-T01 `limitpolicy.Effective` values. A denial consumes no RPM, TPM or concurrency capacity at any scope.

RPM and TPM use deterministic UTC minute windows; concurrency uses idempotent `Lease.Release`. Soft boundaries admit and return structured threshold facts. Hard RPM/TPM rejections include the local reset time; concurrency has no fabricated time-based retry promise.

`Replace` publishes a strictly newer complete snapshot under the same lock as admission. Unchanged scopes retain current window usage and live concurrency, so lowering a limit cannot reset counters or cancel existing work; it only blocks new admission. Missing scopes and invalid/stale snapshots fail closed.

P09-T03 adds `RedisRPMLimiter`: one Redis TIME-validated minute Hash and Lua script first check all four hard limits, then increment all four fields. Window mismatch retries occur before any mutation; `PEXPIREAT` uses the Redis server's absolute next-minute boundary plus bounded retention. Corrupt counters and unknown protocol shapes fail closed.

Local admission is still only early single-instance protection. P09-T05 adds Redis-backed distributed concurrency and P09-T04 adds TPM estimate/settlement. These layers preserve all-or-nothing scope order and never treat a local admission as proof of global capacity.
