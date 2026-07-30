BEGIN;

CREATE FUNCTION app.valid_virtual_key_allowed_models(models text[])
RETURNS boolean
LANGUAGE sql
IMMUTABLE
STRICT
PARALLEL SAFE
AS $function$
    SELECT
        cardinality(models) <= 256
        AND (cardinality(models) = 0 OR array_ndims(models) = 1)
        AND NOT EXISTS (
            SELECT 1
            FROM unnest(models) AS allowed(model_name)
            WHERE model_name IS NULL
               OR model_name <> btrim(model_name)
               OR char_length(model_name) NOT BETWEEN 1 AND 128
               OR model_name !~ '^[A-Za-z0-9][A-Za-z0-9._:/-]*$'
        )
        AND cardinality(models) = (
            SELECT count(DISTINCT lower(model_name))
            FROM unnest(models) AS allowed(model_name)
        );
$function$;

CREATE FUNCTION app.valid_virtual_key_limits(limits jsonb)
RETURNS boolean
LANGUAGE plpgsql
IMMUTABLE
STRICT
PARALLEL SAFE
AS $function$
DECLARE
    limit_name text;
BEGIN
    IF jsonb_typeof(limits) <> 'object' THEN
        RETURN false;
    END IF;

    IF EXISTS (
        SELECT 1
        FROM jsonb_object_keys(limits) AS candidate(key_name)
        WHERE key_name NOT IN ('rpm', 'tpm', 'concurrency')
    ) THEN
        RETURN false;
    END IF;

    FOREACH limit_name IN ARRAY ARRAY['rpm', 'tpm', 'concurrency']
    LOOP
        IF limits ? limit_name AND (
            jsonb_typeof(limits -> limit_name) <> 'number'
            OR (limits ->> limit_name) !~ '^[1-9][0-9]{0,17}$'
        ) THEN
            RETURN false;
        END IF;
    END LOOP;

    RETURN true;
END;
$function$;

CREATE TABLE app.virtual_api_keys (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL,
    project_id uuid NOT NULL,
    key_prefix text NOT NULL,
    secret_hash bytea NOT NULL,
    hash_key_version text NOT NULL,
    status text NOT NULL DEFAULT 'active',
    expires_at timestamptz,
    allowed_models text[],
    limits jsonb,
    rotated_from_id uuid,
    rotation_grace_expires_at timestamptz,
    revoked_at timestamptz,
    revoked_by text,
    version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_by text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_by text NOT NULL,
    CONSTRAINT virtual_api_keys_tenant_fk FOREIGN KEY (tenant_id)
        REFERENCES app.tenants (id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT virtual_api_keys_project_fk FOREIGN KEY (tenant_id, project_id)
        REFERENCES app.projects (tenant_id, id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT virtual_api_keys_tenant_project_id_unique UNIQUE (tenant_id, project_id, id),
    CONSTRAINT virtual_api_keys_prefix_unique UNIQUE (key_prefix),
    CONSTRAINT virtual_api_keys_hash_unique UNIQUE (hash_key_version, secret_hash),
    CONSTRAINT virtual_api_keys_rotation_source_fk FOREIGN KEY (tenant_id, project_id, rotated_from_id)
        REFERENCES app.virtual_api_keys (tenant_id, project_id, id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT virtual_api_keys_prefix_format CHECK (
        key_prefix ~ '^agw_(live|test)_[a-z0-9]{8,32}$'
    ),
    CONSTRAINT virtual_api_keys_secret_hash_length CHECK (
        octet_length(secret_hash) = 32
    ),
    CONSTRAINT virtual_api_keys_hash_key_version_format CHECK (
        hash_key_version = btrim(hash_key_version)
        AND char_length(hash_key_version) BETWEEN 1 AND 64
        AND hash_key_version ~ '^[A-Za-z0-9][A-Za-z0-9._:-]*$'
    ),
    CONSTRAINT virtual_api_keys_status_valid CHECK (
        status IN ('active', 'rotating', 'revoked')
    ),
    CONSTRAINT virtual_api_keys_expiry_time_valid CHECK (
        expires_at IS NULL OR expires_at > created_at
    ),
    CONSTRAINT virtual_api_keys_allowed_models_valid CHECK (
        allowed_models IS NULL OR app.valid_virtual_key_allowed_models(allowed_models)
    ),
    CONSTRAINT virtual_api_keys_limits_valid CHECK (
        limits IS NULL OR app.valid_virtual_key_limits(limits)
    ),
    CONSTRAINT virtual_api_keys_rotation_not_self CHECK (
        rotated_from_id IS NULL OR rotated_from_id <> id
    ),
    CONSTRAINT virtual_api_keys_rotation_grace_time_valid CHECK (
        rotation_grace_expires_at IS NULL OR rotation_grace_expires_at > created_at
    ),
    CONSTRAINT virtual_api_keys_revoked_by_format CHECK (
        revoked_by IS NULL
        OR (
            revoked_by = btrim(revoked_by)
            AND char_length(revoked_by) BETWEEN 1 AND 200
        )
    ),
    CONSTRAINT virtual_api_keys_lifecycle_valid CHECK (
        (
            status = 'active'
            AND rotation_grace_expires_at IS NULL
            AND revoked_at IS NULL
            AND revoked_by IS NULL
        )
        OR (
            status = 'rotating'
            AND rotation_grace_expires_at IS NOT NULL
            AND revoked_at IS NULL
            AND revoked_by IS NULL
        )
        OR (
            status = 'revoked'
            AND rotation_grace_expires_at IS NULL
            AND revoked_at IS NOT NULL
            AND revoked_at >= created_at
            AND revoked_by IS NOT NULL
        )
    ),
    CONSTRAINT virtual_api_keys_version_positive CHECK (version > 0),
    CONSTRAINT virtual_api_keys_created_by_format CHECK (
        created_by = btrim(created_by)
        AND char_length(created_by) BETWEEN 1 AND 200
    ),
    CONSTRAINT virtual_api_keys_updated_by_format CHECK (
        updated_by = btrim(updated_by)
        AND char_length(updated_by) BETWEEN 1 AND 200
    ),
    CONSTRAINT virtual_api_keys_update_time_valid CHECK (updated_at >= created_at)
);

CREATE UNIQUE INDEX uq_virtual_api_keys_rotated_from
    ON app.virtual_api_keys (rotated_from_id)
    WHERE rotated_from_id IS NOT NULL;

CREATE INDEX idx_virtual_api_keys_tenant_project_status
    ON app.virtual_api_keys (tenant_id, project_id, status, created_at DESC, id);

CREATE INDEX idx_virtual_api_keys_actionable_expiry
    ON app.virtual_api_keys (expires_at, id)
    WHERE status IN ('active', 'rotating') AND expires_at IS NOT NULL;

CREATE INDEX idx_virtual_api_keys_rotation_grace
    ON app.virtual_api_keys (rotation_grace_expires_at, id)
    WHERE status = 'rotating';

COMMENT ON TABLE app.virtual_api_keys IS
    'Virtual data-plane credentials; recoverable credential plaintext must never be persisted';
COMMENT ON COLUMN app.virtual_api_keys.key_prefix IS
    'Non-secret lookup and support identifier; this is not the credential secret';
COMMENT ON COLUMN app.virtual_api_keys.secret_hash IS
    '32-byte keyed digest of the secret; never reversible ciphertext or plaintext';
COMMENT ON COLUMN app.virtual_api_keys.hash_key_version IS
    'Identifier for the server-side digest key version; no key material is stored here';
COMMENT ON COLUMN app.virtual_api_keys.allowed_models IS
    'NULL inherits the project allowlist; an empty array explicitly allows no models';
COMMENT ON COLUMN app.virtual_api_keys.limits IS
    'NULL inherits project limits; only positive-integer rpm, tpm and concurrency overrides are accepted';
COMMENT ON COLUMN app.virtual_api_keys.rotated_from_id IS
    'Previous credential replaced by this key; constrained to the same tenant and project';
COMMENT ON COLUMN app.virtual_api_keys.version IS
    'Application-managed optimistic concurrency version';

COMMIT;
