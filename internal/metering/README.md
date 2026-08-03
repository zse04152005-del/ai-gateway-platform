# Metering domain

`metering` owns the finite ledger taxonomy used by event publication, idempotent consumption, pricing, reconciliation, adjustments, and cost queries.

- Token types are independent billing dimensions: `input`, `output`, `cache_read`, `cache_write`, `reasoning`, `audio_input`, `audio_output`, `image_input`, and `image_output`.
- Sources reuse the normalized Adapter taxonomy: `provider`, `estimated`, `reconciled`, and `adjustment`.
- Parsing is exact and fail closed. Case folding, trimming, or silently accepting a vendor-specific meter could change billing meaning.
- Dimensions are not automatically additive. Cache may overlap Input and Reasoning may overlap Output; a future immutable PriceVersion decides units and valuation.

Provider-specific fields that cannot yet map to this taxonomy remain in bounded `adapter.UsageEvidence` and must not be charged as a known type. Image dimensions are defined now for later multimodal pricing even though the current chat Adapter exposes only audio extensions.
