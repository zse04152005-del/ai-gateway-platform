# Local token estimation

`tokenestimate` produces deterministic gateway-owned usage only when a selected
Deployment and validated normalized request are available. Every result carries
`estimated=true`, tokenizer name/version, physical model, catalog Deployment
version, and provider protocol version. It never claims provider billing
accuracy.

The built-in `utf8-byte-bound/v1` tokenizer counts the UTF-8 bytes of a
content-only normalized JSON framing envelope. It is deliberately conservative,
provider-independent, and replaceable through the `Tokenizer` interface.
Request IDs, tenant data, credentials, endpoints, and policy labels are not part
of the envelope.

Counts are cached by SHA-256 over the model/tokenizer identity, direction, and
canonical envelope. The bounded concurrent LRU stores only the digest and count;
prompt and response content are never retained in cache keys or diagnostics.
