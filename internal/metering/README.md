# Metering domain

`metering` owns the finite ledger taxonomy used by event publication, idempotent consumption, pricing, reconciliation, adjustments, and cost queries.

- Token types are independent billing dimensions: `input`, `output`, `cache_read`, `cache_write`, `reasoning`, `audio_input`, `audio_output`, `image_input`, and `image_output`.
- Sources reuse the normalized Adapter taxonomy: `provider`, `estimated`, `reconciled`, and `adjustment`.
- Parsing is exact and fail closed. Case folding, trimming, or silently accepting a vendor-specific meter could change billing meaning.
- Dimensions are not automatically additive. Cache may overlap Input and Reasoning may overlap Output.
- `PriceVersion` binds one Deployment, Region, ISO-style uppercase currency, effective instant, and per-token-type rates. Text/cache/reasoning use tokens; audio uses tokens or seconds; image uses tokens or images.
- Only a published version effective at `observed_at` can price a fact. Missing rates fail closed, and a historical ledger row keeps the exact published version identity.
- Amounts use exact integer micros and ceiling division with arbitrary-precision intermediates; a positive rate cannot truncate a small fact to zero, and values beyond the JSON-safe ledger range fail closed.

Provider-specific fields that cannot yet map to this taxonomy remain in bounded `adapter.UsageEvidence` and must not be charged as a known type. Image dimensions are defined now for later multimodal pricing even though the current chat Adapter exposes only audio extensions.

`UsageEvent` v2 is the current publication contract while the same topic and Consumer remain backward-compatible with v1. One positive Token dimension becomes one event; missing and explicit zero values produce no billable fact. Current Gateway facts explicitly use `billing_unit=token`; consumers treat the field as token when reading older version-1 messages that predate it, but never substitute a second/image rate. Gateway publication accepts only `provider → usage.observed` and `estimated → usage.estimated`. Every v2 estimated fact must carry `estimated=true`, tokenizer/version, physical model, Deployment version and provider protocol version; provider facts must not carry those fields. Reconciliation identities belong to the later trusted import workflow; Adjustment is authored directly through `internal/meteringadjustment` and never masquerades as a Gateway UsageEvent. Event payloads carry content-free execution/trace attribution and never carry prompts, responses, credentials, endpoints, or raw provider evidence.

`internal/meteringcost` consumes the priced side of this domain: it rebuilds terminal Request totals from every immutable Attempt Ledger fact, keeps currencies separate, and refuses to return a partial total while any durable Outbox event is still unpriced.
