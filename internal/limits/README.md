# Local limits

P09-T02 provides one process-local, non-blocking admission layer. Each request is checked atomically against Platform, Tenant, Project and VirtualKey counters using only P09-T01 `limitpolicy.Effective` values. A denial consumes no RPM, TPM or concurrency capacity at any scope.

RPM and TPM use deterministic UTC minute windows; concurrency uses idempotent `Lease.Release`. Soft boundaries admit and return structured threshold facts. Hard RPM/TPM rejections include the local reset time; concurrency has no fabricated time-based retry promise.

`Replace` publishes a strictly newer complete snapshot under the same lock as admission. Unchanged scopes retain current window usage and live concurrency, so lowering a limit cannot reset counters or cancel existing work; it only blocks new admission. Missing scopes and invalid/stale snapshots fail closed.

This is early single-instance protection, not the global source of truth. P09-T03/P09-T05 add Redis-backed distributed RPM/concurrency checks, and P09-T04 adds TPM estimate/settlement. Those layers must preserve this all-or-nothing scope order and must not treat a local admission as proof of global capacity.
