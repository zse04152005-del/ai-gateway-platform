# Limit policy

P09-T01 defines sparse, tenant-owned RPM, TPM, and concurrency policies. Every resource has an independently inheritable soft and hard boundary; zero never means unlimited. Resolution always follows Platform → Tenant → Project → Key and returns both concrete values and the source of each boundary.

The platform layer must define all six boundaries. Child layers may override individual fields, but an explicit empty layer is invalid and the final effective policy must satisfy `0 < soft <= hard <= 2^53-1`. The maximum preserves exact integer behavior in future Redis Lua counters.

Soft thresholds admit traffic and produce telemetry/alerts; hard thresholds reject admission. P09-T02 and later consumers must use the resolved `Effective` value rather than reimplement inheritance.
