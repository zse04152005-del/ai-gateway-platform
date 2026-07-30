BEGIN;

CREATE FUNCTION app.valid_catalog_regions(regions text[])
RETURNS boolean
LANGUAGE sql
IMMUTABLE
STRICT
PARALLEL SAFE
AS $function$
    SELECT
        cardinality(regions) BETWEEN 1 AND 64
        AND array_ndims(regions) = 1
        AND NOT EXISTS (
            SELECT 1
            FROM unnest(regions) AS candidate(region)
            WHERE region IS NULL
               OR region <> btrim(region)
               OR char_length(region) NOT BETWEEN 1 AND 63
               OR region !~ '^[a-z0-9][a-z0-9-]*$'
        )
        AND cardinality(regions) = (
            SELECT count(DISTINCT region)
            FROM unnest(regions) AS candidate(region)
        );
$function$;

CREATE FUNCTION app.valid_catalog_requirements(requirements jsonb)
RETURNS boolean
LANGUAGE plpgsql
IMMUTABLE
STRICT
PARALLEL SAFE
AS $function$
DECLARE
    key_name text;
    boolean_name text;
    retention_modes jsonb;
BEGIN
    IF jsonb_typeof(requirements) <> 'object' THEN
        RETURN false;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM jsonb_object_keys(requirements)) THEN
        RETURN false;
    END IF;

    FOR key_name IN SELECT jsonb_object_keys(requirements)
    LOOP
        IF key_name NOT IN (
            'chat', 'stream', 'tools', 'parallel_tools', 'structured_output',
            'vision', 'audio_input', 'audio_output', 'embeddings',
            'min_context_tokens', 'min_output_tokens', 'data_retention_modes'
        ) THEN
            RETURN false;
        END IF;
    END LOOP;

    FOREACH boolean_name IN ARRAY ARRAY[
        'chat', 'stream', 'tools', 'parallel_tools', 'structured_output',
        'vision', 'audio_input', 'audio_output', 'embeddings'
    ]
    LOOP
        IF requirements ? boolean_name AND (
            jsonb_typeof(requirements -> boolean_name) <> 'boolean'
            OR (requirements ->> boolean_name) <> 'true'
        ) THEN
            RETURN false;
        END IF;
    END LOOP;

    FOREACH key_name IN ARRAY ARRAY['min_context_tokens', 'min_output_tokens']
    LOOP
        IF requirements ? key_name AND (
            jsonb_typeof(requirements -> key_name) <> 'number'
            OR (requirements ->> key_name) !~ '^[1-9][0-9]{0,17}$'
        ) THEN
            RETURN false;
        END IF;
    END LOOP;

    IF requirements ? 'min_context_tokens'
       AND requirements ? 'min_output_tokens'
       AND (requirements ->> 'min_output_tokens')::bigint
           > (requirements ->> 'min_context_tokens')::bigint THEN
        RETURN false;
    END IF;

    IF requirements ? 'data_retention_modes' THEN
        retention_modes := requirements -> 'data_retention_modes';
        IF jsonb_typeof(retention_modes) <> 'array'
           OR jsonb_array_length(retention_modes) NOT BETWEEN 1 AND 4
           OR EXISTS (
               SELECT 1
               FROM jsonb_array_elements(retention_modes) AS entry(value)
               WHERE jsonb_typeof(value) <> 'string'
                  OR value #>> '{}' NOT IN (
                      'provider_default', 'no_training', 'zero_retention', 'self_hosted'
                  )
           )
           OR jsonb_array_length(retention_modes) <> (
               SELECT count(DISTINCT value #>> '{}')
               FROM jsonb_array_elements(retention_modes) AS entry(value)
           ) THEN
            RETURN false;
        END IF;
    END IF;

    RETURN true;
END;
$function$;

CREATE FUNCTION app.valid_catalog_capabilities(capabilities jsonb)
RETURNS boolean
LANGUAGE plpgsql
IMMUTABLE
STRICT
PARALLEL SAFE
AS $function$
DECLARE
    key_name text;
    boolean_name text;
BEGIN
    IF jsonb_typeof(capabilities) <> 'object' THEN
        RETURN false;
    END IF;

    FOR key_name IN SELECT jsonb_object_keys(capabilities)
    LOOP
        IF key_name NOT IN (
            'chat', 'stream', 'tools', 'parallel_tools', 'structured_output',
            'vision', 'audio_input', 'audio_output', 'embeddings',
            'usage_in_stream', 'cache_usage', 'reasoning_usage',
            'max_context_tokens', 'max_output_tokens',
            'data_retention_mode', 'provider_protocol_version'
        ) THEN
            RETURN false;
        END IF;
    END LOOP;

    FOREACH boolean_name IN ARRAY ARRAY[
        'chat', 'stream', 'tools', 'parallel_tools', 'structured_output',
        'vision', 'audio_input', 'audio_output', 'embeddings',
        'usage_in_stream', 'cache_usage', 'reasoning_usage'
    ]
    LOOP
        IF capabilities ? boolean_name
           AND jsonb_typeof(capabilities -> boolean_name) <> 'boolean' THEN
            RETURN false;
        END IF;
    END LOOP;

    IF COALESCE((capabilities ->> 'chat')::boolean, false) = false
       AND COALESCE((capabilities ->> 'embeddings')::boolean, false) = false THEN
        RETURN false;
    END IF;

    FOREACH key_name IN ARRAY ARRAY['max_context_tokens', 'max_output_tokens']
    LOOP
        IF NOT capabilities ? key_name
           OR jsonb_typeof(capabilities -> key_name) <> 'number'
           OR (capabilities ->> key_name) !~ '^[1-9][0-9]{0,17}$' THEN
            RETURN false;
        END IF;
    END LOOP;

    IF (capabilities ->> 'max_output_tokens')::bigint
       > (capabilities ->> 'max_context_tokens')::bigint THEN
        RETURN false;
    END IF;

    IF jsonb_typeof(capabilities -> 'data_retention_mode') <> 'string'
       OR capabilities ->> 'data_retention_mode' NOT IN (
           'provider_default', 'no_training', 'zero_retention', 'self_hosted'
       ) THEN
        RETURN false;
    END IF;

    IF jsonb_typeof(capabilities -> 'provider_protocol_version') <> 'string'
       OR capabilities ->> 'provider_protocol_version'
           !~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$' THEN
        RETURN false;
    END IF;

    RETURN true;
END;
$function$;

CREATE FUNCTION app.catalog_deployment_satisfies(
    requirements jsonb,
    allowed_regions text[],
    capabilities jsonb,
    deployment_region text
)
RETURNS boolean
LANGUAGE plpgsql
IMMUTABLE
PARALLEL SAFE
AS $function$
DECLARE
    boolean_name text;
BEGIN
    IF allowed_regions IS NOT NULL AND NOT (deployment_region = ANY(allowed_regions)) THEN
        RETURN false;
    END IF;

    FOREACH boolean_name IN ARRAY ARRAY[
        'chat', 'stream', 'tools', 'parallel_tools', 'structured_output',
        'vision', 'audio_input', 'audio_output', 'embeddings'
    ]
    LOOP
        IF requirements ? boolean_name
           AND COALESCE((capabilities ->> boolean_name)::boolean, false) = false THEN
            RETURN false;
        END IF;
    END LOOP;

    IF requirements ? 'min_context_tokens'
       AND (capabilities ->> 'max_context_tokens')::bigint
           < (requirements ->> 'min_context_tokens')::bigint THEN
        RETURN false;
    END IF;

    IF requirements ? 'min_output_tokens'
       AND (capabilities ->> 'max_output_tokens')::bigint
           < (requirements ->> 'min_output_tokens')::bigint THEN
        RETURN false;
    END IF;

    IF requirements ? 'data_retention_modes'
       AND NOT ((requirements -> 'data_retention_modes')
           ? (capabilities ->> 'data_retention_mode')) THEN
        RETURN false;
    END IF;

    RETURN true;
END;
$function$;

CREATE TABLE app.providers (
    id uuid PRIMARY KEY,
    code text NOT NULL,
    name text NOT NULL,
    adapter_type text NOT NULL,
    status text NOT NULL DEFAULT 'active',
    version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_by text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_by text NOT NULL,
    CONSTRAINT providers_code_unique UNIQUE (code),
    CONSTRAINT providers_code_format CHECK (
        char_length(code) BETWEEN 2 AND 63
        AND code ~ '^[a-z0-9][a-z0-9-]*[a-z0-9]$'
    ),
    CONSTRAINT providers_name_format CHECK (
        name = btrim(name) AND char_length(name) BETWEEN 1 AND 200
    ),
    CONSTRAINT providers_adapter_type_format CHECK (
        adapter_type = btrim(adapter_type)
        AND char_length(adapter_type) BETWEEN 1 AND 64
        AND adapter_type ~ '^[a-z][a-z0-9._-]*$'
    ),
    CONSTRAINT providers_status_valid CHECK (status IN ('active', 'disabled')),
    CONSTRAINT providers_version_positive CHECK (version > 0),
    CONSTRAINT providers_created_by_format CHECK (
        created_by = btrim(created_by) AND char_length(created_by) BETWEEN 1 AND 200
    ),
    CONSTRAINT providers_updated_by_format CHECK (
        updated_by = btrim(updated_by) AND char_length(updated_by) BETWEEN 1 AND 200
    ),
    CONSTRAINT providers_update_time_valid CHECK (updated_at >= created_at)
);

CREATE INDEX idx_providers_status_code ON app.providers (status, code, id);

CREATE TABLE app.logical_models (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL,
    name text NOT NULL,
    display_name text NOT NULL,
    description text,
    required_capabilities jsonb NOT NULL,
    allowed_regions text[],
    status text NOT NULL DEFAULT 'active',
    version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_by text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_by text NOT NULL,
    CONSTRAINT logical_models_tenant_fk FOREIGN KEY (tenant_id)
        REFERENCES app.tenants (id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT logical_models_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT logical_models_tenant_name_unique UNIQUE (tenant_id, name),
    CONSTRAINT logical_models_name_format CHECK (
        char_length(name) BETWEEN 1 AND 128
        AND name ~ '^[a-z0-9][a-z0-9._:/-]*$'
    ),
    CONSTRAINT logical_models_display_name_format CHECK (
        display_name = btrim(display_name)
        AND char_length(display_name) BETWEEN 1 AND 200
    ),
    CONSTRAINT logical_models_description_format CHECK (
        description IS NULL
        OR (description = btrim(description) AND char_length(description) BETWEEN 1 AND 1000)
    ),
    CONSTRAINT logical_models_requirements_valid CHECK (
        app.valid_catalog_requirements(required_capabilities)
    ),
    CONSTRAINT logical_models_allowed_regions_valid CHECK (
        allowed_regions IS NULL OR app.valid_catalog_regions(allowed_regions)
    ),
    CONSTRAINT logical_models_status_valid CHECK (status IN ('active', 'disabled')),
    CONSTRAINT logical_models_version_positive CHECK (version > 0),
    CONSTRAINT logical_models_created_by_format CHECK (
        created_by = btrim(created_by) AND char_length(created_by) BETWEEN 1 AND 200
    ),
    CONSTRAINT logical_models_updated_by_format CHECK (
        updated_by = btrim(updated_by) AND char_length(updated_by) BETWEEN 1 AND 200
    ),
    CONSTRAINT logical_models_update_time_valid CHECK (updated_at >= created_at)
);

CREATE INDEX idx_logical_models_tenant_status_name
    ON app.logical_models (tenant_id, status, name, id);

CREATE TABLE app.deployments (
    id uuid PRIMARY KEY,
    provider_id uuid NOT NULL,
    code text NOT NULL,
    physical_model text NOT NULL,
    endpoint_url text NOT NULL,
    region text NOT NULL,
    capabilities jsonb NOT NULL,
    status text NOT NULL DEFAULT 'active',
    version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_by text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_by text NOT NULL,
    CONSTRAINT deployments_provider_fk FOREIGN KEY (provider_id)
        REFERENCES app.providers (id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT deployments_provider_code_unique UNIQUE (provider_id, code),
    CONSTRAINT deployments_physical_identity_unique UNIQUE (
        provider_id, physical_model, endpoint_url, region
    ),
    CONSTRAINT deployments_code_format CHECK (
        char_length(code) BETWEEN 1 AND 63
        AND code ~ '^[a-z0-9][a-z0-9._-]*$'
    ),
    CONSTRAINT deployments_physical_model_format CHECK (
        physical_model = btrim(physical_model)
        AND char_length(physical_model) BETWEEN 1 AND 200
        AND physical_model ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]*$'
    ),
    CONSTRAINT deployments_endpoint_url_format CHECK (
        char_length(endpoint_url) BETWEEN 10 AND 2048
        AND endpoint_url ~ '^https?://[^[:space:]?#]+(/[^[:space:]?#]*)?$'
        AND endpoint_url !~ '^https?://[^/?#]*@'
    ),
    CONSTRAINT deployments_region_format CHECK (
        char_length(region) BETWEEN 1 AND 63
        AND region ~ '^[a-z0-9][a-z0-9-]*$'
    ),
    CONSTRAINT deployments_capabilities_valid CHECK (
        app.valid_catalog_capabilities(capabilities)
    ),
    CONSTRAINT deployments_status_valid CHECK (status IN ('active', 'disabled')),
    CONSTRAINT deployments_version_positive CHECK (version > 0),
    CONSTRAINT deployments_created_by_format CHECK (
        created_by = btrim(created_by) AND char_length(created_by) BETWEEN 1 AND 200
    ),
    CONSTRAINT deployments_updated_by_format CHECK (
        updated_by = btrim(updated_by) AND char_length(updated_by) BETWEEN 1 AND 200
    ),
    CONSTRAINT deployments_update_time_valid CHECK (updated_at >= created_at)
);

CREATE INDEX idx_deployments_provider_status_region
    ON app.deployments (provider_id, status, region, code, id);

CREATE TABLE app.logical_model_deployments (
    logical_model_id uuid NOT NULL,
    deployment_id uuid NOT NULL,
    priority smallint NOT NULL DEFAULT 100,
    weight smallint NOT NULL DEFAULT 100,
    status text NOT NULL DEFAULT 'active',
    version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_by text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_by text NOT NULL,
    PRIMARY KEY (logical_model_id, deployment_id),
    CONSTRAINT logical_model_deployments_model_fk FOREIGN KEY (logical_model_id)
        REFERENCES app.logical_models (id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT logical_model_deployments_deployment_fk FOREIGN KEY (deployment_id)
        REFERENCES app.deployments (id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT logical_model_deployments_priority_valid CHECK (priority BETWEEN 1 AND 1000),
    CONSTRAINT logical_model_deployments_weight_valid CHECK (weight BETWEEN 1 AND 10000),
    CONSTRAINT logical_model_deployments_status_valid CHECK (status IN ('active', 'disabled')),
    CONSTRAINT logical_model_deployments_version_positive CHECK (version > 0),
    CONSTRAINT logical_model_deployments_created_by_format CHECK (
        created_by = btrim(created_by) AND char_length(created_by) BETWEEN 1 AND 200
    ),
    CONSTRAINT logical_model_deployments_updated_by_format CHECK (
        updated_by = btrim(updated_by) AND char_length(updated_by) BETWEEN 1 AND 200
    ),
    CONSTRAINT logical_model_deployments_update_time_valid CHECK (updated_at >= created_at)
);

CREATE INDEX idx_logical_model_deployments_route_order
    ON app.logical_model_deployments (logical_model_id, status, priority, deployment_id);

CREATE FUNCTION app.enforce_catalog_binding_contract()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
DECLARE
    requirements jsonb;
    allowed_regions text[];
    capabilities jsonb;
    deployment_region text;
BEGIN
    SELECT required_capabilities, logical_models.allowed_regions
    INTO requirements, allowed_regions
    FROM app.logical_models
    WHERE id = NEW.logical_model_id;

    IF NOT FOUND THEN
        RETURN NEW;
    END IF;

    SELECT deployments.capabilities, deployments.region
    INTO capabilities, deployment_region
    FROM app.deployments
    WHERE id = NEW.deployment_id;

    IF NOT FOUND THEN
        RETURN NEW;
    END IF;

    IF NOT app.catalog_deployment_satisfies(
        requirements, allowed_regions, capabilities, deployment_region
    ) THEN
        RAISE EXCEPTION 'deployment does not satisfy logical model capability or region contract'
            USING ERRCODE = '23514',
                  CONSTRAINT = 'logical_model_deployments_capability_contract';
    END IF;

    RETURN NEW;
END;
$function$;

CREATE FUNCTION app.enforce_logical_model_contract_update()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM app.logical_model_deployments AS binding
        JOIN app.deployments AS deployment ON deployment.id = binding.deployment_id
        WHERE binding.logical_model_id = NEW.id
          AND NOT app.catalog_deployment_satisfies(
              NEW.required_capabilities,
              NEW.allowed_regions,
              deployment.capabilities,
              deployment.region
          )
    ) THEN
        RAISE EXCEPTION 'logical model update would invalidate an existing deployment binding'
            USING ERRCODE = '23514',
                  CONSTRAINT = 'logical_model_deployments_capability_contract';
    END IF;

    RETURN NEW;
END;
$function$;

CREATE FUNCTION app.enforce_deployment_contract_update()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM app.logical_model_deployments AS binding
        JOIN app.logical_models AS logical_model ON logical_model.id = binding.logical_model_id
        WHERE binding.deployment_id = NEW.id
          AND NOT app.catalog_deployment_satisfies(
              logical_model.required_capabilities,
              logical_model.allowed_regions,
              NEW.capabilities,
              NEW.region
          )
    ) THEN
        RAISE EXCEPTION 'deployment update would invalidate an existing logical model binding'
            USING ERRCODE = '23514',
                  CONSTRAINT = 'logical_model_deployments_capability_contract';
    END IF;

    RETURN NEW;
END;
$function$;

CREATE TRIGGER trg_logical_model_deployments_contract
BEFORE INSERT OR UPDATE OF logical_model_id, deployment_id
ON app.logical_model_deployments
FOR EACH ROW
EXECUTE FUNCTION app.enforce_catalog_binding_contract();

CREATE TRIGGER trg_logical_models_contract_update
BEFORE UPDATE OF required_capabilities, allowed_regions
ON app.logical_models
FOR EACH ROW
WHEN (
    OLD.required_capabilities IS DISTINCT FROM NEW.required_capabilities
    OR OLD.allowed_regions IS DISTINCT FROM NEW.allowed_regions
)
EXECUTE FUNCTION app.enforce_logical_model_contract_update();

CREATE TRIGGER trg_deployments_contract_update
BEFORE UPDATE OF capabilities, region
ON app.deployments
FOR EACH ROW
WHEN (
    OLD.capabilities IS DISTINCT FROM NEW.capabilities
    OR OLD.region IS DISTINCT FROM NEW.region
)
EXECUTE FUNCTION app.enforce_deployment_contract_update();

COMMENT ON TABLE app.providers IS
    'Platform provider definitions; credentials and mutable secrets are deliberately stored elsewhere';
COMMENT ON TABLE app.logical_models IS
    'Tenant-scoped stable client model names and their minimum capability/residency contract';
COMMENT ON TABLE app.deployments IS
    'Physical provider endpoints and observed callable capability declarations';
COMMENT ON TABLE app.logical_model_deployments IS
    'Capability-checked mapping between stable logical models and physical deployments';
COMMENT ON COLUMN app.logical_models.allowed_regions IS
    'NULL allows any region; otherwise the physical deployment region must be listed';
COMMENT ON COLUMN app.deployments.endpoint_url IS
    'Syntactic endpoint only; runtime SSRF and DNS/IP policy is enforced by the security layer';
COMMENT ON COLUMN app.deployments.capabilities IS
    'Strict capability contract; unknown fields are rejected instead of silently ignored';

COMMIT;
