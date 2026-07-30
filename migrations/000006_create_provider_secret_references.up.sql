BEGIN;

CREATE TABLE app.provider_secret_references (
    id uuid PRIMARY KEY,
    provider_id uuid NOT NULL,
    name text NOT NULL,
    backend text NOT NULL,
    locator text,
    ciphertext bytea,
    nonce bytea,
    key_version text,
    status text NOT NULL DEFAULT 'active',
    version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_by text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_by text NOT NULL,
    CONSTRAINT provider_secret_references_provider_fk FOREIGN KEY (provider_id)
        REFERENCES app.providers (id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT provider_secret_references_provider_id_unique UNIQUE (provider_id, id),
    CONSTRAINT provider_secret_references_provider_name_unique UNIQUE (provider_id, name),
    CONSTRAINT provider_secret_references_name_format CHECK (
        char_length(name) BETWEEN 1 AND 63
        AND name ~ '^[a-z0-9][a-z0-9._-]*$'
    ),
    CONSTRAINT provider_secret_references_backend_valid CHECK (
        backend IN ('local_envelope', 'vault', 'kms')
    ),
    CONSTRAINT provider_secret_references_locator_format CHECK (
        locator IS NULL
        OR (
            char_length(locator) BETWEEN 10 AND 2048
            AND locator = btrim(locator)
            AND locator !~ '[[:space:]]'
            AND locator !~ '^[a-z][a-z0-9+.-]*://[^/?#]*@'
            AND (
                (backend = 'vault' AND locator ~ '^vault://[^[:space:]]+$')
                OR (backend = 'kms' AND locator ~ '^kms://[^[:space:]]+$')
            )
        )
    ),
    CONSTRAINT provider_secret_references_ciphertext_size CHECK (
        ciphertext IS NULL OR octet_length(ciphertext) BETWEEN 17 AND 65536
    ),
    CONSTRAINT provider_secret_references_nonce_size CHECK (
        nonce IS NULL OR octet_length(nonce) = 12
    ),
    CONSTRAINT provider_secret_references_key_version_format CHECK (
        key_version IS NULL
        OR (
            char_length(key_version) BETWEEN 1 AND 64
            AND key_version ~ '^[A-Za-z0-9][A-Za-z0-9._:-]*$'
        )
    ),
    CONSTRAINT provider_secret_references_backend_material CHECK (
        (
            backend = 'local_envelope'
            AND locator IS NULL
            AND ciphertext IS NOT NULL
            AND nonce IS NOT NULL
            AND key_version IS NOT NULL
        )
        OR (
            backend IN ('vault', 'kms')
            AND locator IS NOT NULL
            AND ciphertext IS NULL
            AND nonce IS NULL
            AND key_version IS NULL
        )
    ),
    CONSTRAINT provider_secret_references_status_valid CHECK (status IN ('active', 'disabled')),
    CONSTRAINT provider_secret_references_version_positive CHECK (version > 0),
    CONSTRAINT provider_secret_references_created_by_format CHECK (
        created_by = btrim(created_by) AND char_length(created_by) BETWEEN 1 AND 200
    ),
    CONSTRAINT provider_secret_references_updated_by_format CHECK (
        updated_by = btrim(updated_by) AND char_length(updated_by) BETWEEN 1 AND 200
    ),
    CONSTRAINT provider_secret_references_update_time_valid CHECK (updated_at >= created_at)
);

CREATE INDEX idx_provider_secret_references_provider_status
    ON app.provider_secret_references (provider_id, status, name, id);

ALTER TABLE app.deployments
    ADD COLUMN secret_reference_id uuid,
    ADD CONSTRAINT deployments_provider_secret_reference_fk
        FOREIGN KEY (provider_id, secret_reference_id)
        REFERENCES app.provider_secret_references (provider_id, id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT;

CREATE INDEX idx_deployments_secret_reference
    ON app.deployments (secret_reference_id)
    WHERE secret_reference_id IS NOT NULL;

COMMENT ON TABLE app.provider_secret_references IS
    'Provider-bound secret references; local development stores only AES-GCM envelopes and external backends store only locators';
COMMENT ON COLUMN app.provider_secret_references.ciphertext IS
    'Authenticated ciphertext only; recoverable plaintext must never enter PostgreSQL';
COMMENT ON COLUMN app.provider_secret_references.locator IS
    'Internal Vault/KMS locator; never include it in public API, logs, errors, metrics, or traces';
COMMENT ON COLUMN app.deployments.secret_reference_id IS
    'Optional provider-bound credential reference; composite FK prevents cross-provider reuse';

COMMIT;
