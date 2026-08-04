# Metering domain

`metering` owns the finite ledger taxonomy used by event publication, idempotent consumption, pricing, reconciliation, adjustments, and cost queries.

- Token types are independent billing dimensions: `input`, `output`, `cache_read`, `cache_write`, `reasoning`, `audio_input`, `audio_output`, `image_input`, and `image_output`.
- Sources reuse the normalized Adapter taxonomy: `provider`, `estimated`, `reconciled`, and `adjustment`.
- Parsing is exact and fail closed. Case folding, trimming, or silently accepting a vendor-specific meter could change billing meaning.
- Dimensions are not automatically additive. Cache may overlap Input and Reasoning may overlap Output.
- `PriceVersion` binds one Deployment, Region, ISO-style uppercase currency, effective instant, and per-token-type rates. Text/cache/reasoning use tokens; audio uses tokens or seconds; image uses tokens or images.
- Only a published version effective at `observed_at` can price a fact. Missing rates fail closed, and a historical ledger row keeps the exact published version identity.

Provider-specific fields that cannot yet map to this taxonomy remain in bounded `adapter.UsageEvidence` and must not be charged as a known type. Image dimensions are defined now for later multimodal pricing even though the current chat Adapter exposes only audio extensions.

`UsageEvent` is the immutable version-1 publication contract. One positive Token dimension becomes one event; missing and explicit zero values produce no billable fact. Gateway publication accepts only `provider → usage.observed` and `estimated → usage.estimated`. Reconciliation and adjustment identities belong to later trusted workflows. Event payloads carry content-free execution/trace attribution and never carry prompts, responses, credentials, endpoints, or raw provider evidence.
