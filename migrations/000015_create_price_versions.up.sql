BEGIN;

DO $block$
BEGIN
    IF EXISTS (SELECT 1 FROM app.usage_ledger_entries) THEN
        RAISE EXCEPTION 'usage ledger must be empty before adding required price locks'
            USING ERRCODE = '23514';
    END IF;
END;
$block$;

ALTER TABLE app.deployments
    ADD CONSTRAINT deployments_id_region_unique UNIQUE (id, region);

CREATE TABLE app.price_versions (
    id uuid PRIMARY KEY,
    deployment_id uuid NOT NULL,
    region text NOT NULL,
    currency text NOT NULL,
    effective_at timestamptz NOT NULL,
    status text NOT NULL DEFAULT 'draft',
    version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_by text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_by text NOT NULL,
    published_at timestamptz,
    CONSTRAINT price_versions_deployment_region_fk FOREIGN KEY (deployment_id, region)
        REFERENCES app.deployments (id, region)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT price_versions_deployment_effective_unique UNIQUE (
        deployment_id, region, effective_at
    ),
    CONSTRAINT price_versions_currency_format CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT price_versions_status_valid CHECK (status IN ('draft', 'published')),
    CONSTRAINT price_versions_lifecycle_valid CHECK (
        (
            status = 'draft' AND version = 1 AND published_at IS NULL
        )
        OR (
            status = 'published' AND version = 2 AND published_at IS NOT NULL
            AND published_at >= created_at AND updated_at >= published_at
        )
    ),
    CONSTRAINT price_versions_created_by_format CHECK (
        created_by = btrim(created_by) AND char_length(created_by) BETWEEN 1 AND 200
    ),
    CONSTRAINT price_versions_updated_by_format CHECK (
        updated_by = btrim(updated_by) AND char_length(updated_by) BETWEEN 1 AND 200
    ),
    CONSTRAINT price_versions_update_time_valid CHECK (updated_at >= created_at)
);

CREATE TABLE app.price_version_rates (
    price_version_id uuid NOT NULL,
    token_type text NOT NULL,
    billing_unit text NOT NULL,
    unit_quantity bigint NOT NULL,
    unit_price_micros bigint NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_by text NOT NULL,
    PRIMARY KEY (price_version_id, token_type),
    CONSTRAINT price_version_rates_version_fk FOREIGN KEY (price_version_id)
        REFERENCES app.price_versions (id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT price_version_rates_token_type_valid CHECK (
        token_type IN (
            'input', 'output', 'cache_read', 'cache_write', 'reasoning',
            'audio_input', 'audio_output', 'image_input', 'image_output'
        )
    ),
    CONSTRAINT price_version_rates_billing_unit_valid CHECK (
        billing_unit IN ('token', 'image', 'second')
    ),
    CONSTRAINT price_version_rates_unit_compatible CHECK (
        (
            token_type IN ('input', 'output', 'cache_read', 'cache_write', 'reasoning')
            AND billing_unit = 'token'
        )
        OR (
            token_type IN ('audio_input', 'audio_output')
            AND billing_unit IN ('token', 'second')
        )
        OR (
            token_type IN ('image_input', 'image_output')
            AND billing_unit IN ('token', 'image')
        )
    ),
    CONSTRAINT price_version_rates_values_valid CHECK (
        unit_quantity BETWEEN 1 AND 9007199254740991
        AND unit_price_micros BETWEEN 0 AND 9007199254740991
    ),
    CONSTRAINT price_version_rates_created_by_format CHECK (
        created_by = btrim(created_by) AND char_length(created_by) BETWEEN 1 AND 200
    )
);

CREATE FUNCTION app.enforce_price_version_insert()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
BEGIN
    IF NEW.status <> 'draft' OR NEW.version <> 1 OR NEW.published_at IS NOT NULL THEN
        RAISE EXCEPTION 'price version must be created as draft version 1'
            USING ERRCODE = '23514', CONSTRAINT = 'price_versions_initial_draft';
    END IF;
    RETURN NEW;
END;
$function$;

CREATE FUNCTION app.enforce_price_version_publish()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
BEGIN
    IF NEW.id <> OLD.id
       OR NEW.deployment_id <> OLD.deployment_id
       OR NEW.region <> OLD.region
       OR NEW.currency <> OLD.currency
       OR NEW.effective_at <> OLD.effective_at
       OR NEW.created_at <> OLD.created_at
       OR NEW.created_by <> OLD.created_by THEN
        RAISE EXCEPTION 'price version identity is immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'price_versions_identity_immutable';
    END IF;
    IF OLD.status <> 'draft' OR NEW.status <> 'published'
       OR NEW.version <> OLD.version + 1 OR NEW.updated_at < OLD.updated_at THEN
        RAISE EXCEPTION 'price version publication transition is invalid'
            USING ERRCODE = '23514', CONSTRAINT = 'price_versions_publish_transition';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM app.price_version_rates WHERE price_version_id = OLD.id
    ) THEN
        RAISE EXCEPTION 'price version requires at least one rate before publication'
            USING ERRCODE = '23514', CONSTRAINT = 'price_versions_rates_required';
    END IF;
    RETURN NEW;
END;
$function$;

CREATE FUNCTION app.reject_price_version_delete()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
BEGIN
    RAISE EXCEPTION 'price versions are immutable publication records'
        USING ERRCODE = '23514';
END;
$function$;

CREATE FUNCTION app.enforce_price_version_rate_insert()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
DECLARE
    parent_status text;
BEGIN
    SELECT status INTO parent_status
    FROM app.price_versions
    WHERE id = NEW.price_version_id
    FOR UPDATE;
    IF FOUND AND parent_status <> 'draft' THEN
        RAISE EXCEPTION 'price rates may be inserted only while the version is draft'
            USING ERRCODE = '23514', CONSTRAINT = 'price_version_rates_parent_draft';
    END IF;
    RETURN NEW;
END;
$function$;

CREATE FUNCTION app.reject_price_version_rate_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
BEGIN
    RAISE EXCEPTION 'price version rates are append-only'
        USING ERRCODE = '23514';
END;
$function$;

CREATE TRIGGER trg_price_versions_initial_draft
BEFORE INSERT ON app.price_versions
FOR EACH ROW EXECUTE FUNCTION app.enforce_price_version_insert();

CREATE TRIGGER trg_price_versions_publish
BEFORE UPDATE ON app.price_versions
FOR EACH ROW EXECUTE FUNCTION app.enforce_price_version_publish();

CREATE TRIGGER trg_price_versions_no_delete
BEFORE DELETE ON app.price_versions
FOR EACH ROW EXECUTE FUNCTION app.reject_price_version_delete();

CREATE TRIGGER trg_price_version_rates_draft_insert
BEFORE INSERT ON app.price_version_rates
FOR EACH ROW EXECUTE FUNCTION app.enforce_price_version_rate_insert();

CREATE TRIGGER trg_price_version_rates_append_only
BEFORE UPDATE OR DELETE ON app.price_version_rates
FOR EACH ROW EXECUTE FUNCTION app.reject_price_version_rate_mutation();

ALTER TABLE app.usage_ledger_entries
    ADD COLUMN price_version_id uuid NOT NULL,
    ADD COLUMN amount_micros bigint NOT NULL,
    ADD CONSTRAINT usage_ledger_entries_price_rate_fk FOREIGN KEY (price_version_id, token_type)
        REFERENCES app.price_version_rates (price_version_id, token_type)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    ADD CONSTRAINT usage_ledger_entries_amount_valid CHECK (
        amount_micros BETWEEN 0 AND 9007199254740991
    );

CREATE FUNCTION app.enforce_usage_ledger_price_lock()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
DECLARE
    locked_deployment_id uuid;
    locked_status text;
    locked_effective_at timestamptz;
    attempt_deployment_id uuid;
BEGIN
    SELECT deployment_id, status, effective_at
    INTO locked_deployment_id, locked_status, locked_effective_at
    FROM app.price_versions
    WHERE id = NEW.price_version_id;

    IF FOUND THEN
        IF locked_status <> 'published' THEN
            RAISE EXCEPTION 'usage ledger price version must be published'
                USING ERRCODE = '23514', CONSTRAINT = 'usage_ledger_entries_price_published';
        END IF;
        IF locked_effective_at > NEW.observed_at THEN
            RAISE EXCEPTION 'usage ledger price version is not yet effective'
                USING ERRCODE = '23514', CONSTRAINT = 'usage_ledger_entries_price_effective';
        END IF;
        IF NEW.attempt_id IS NOT NULL THEN
            SELECT deployment_id INTO attempt_deployment_id
            FROM app.route_attempts
            WHERE request_id = NEW.request_id AND id = NEW.attempt_id;
            IF FOUND AND attempt_deployment_id <> locked_deployment_id THEN
                RAISE EXCEPTION 'usage ledger price deployment does not match attempt'
                    USING ERRCODE = '23514', CONSTRAINT = 'usage_ledger_entries_price_deployment';
            END IF;
        END IF;
    END IF;
    RETURN NEW;
END;
$function$;

CREATE TRIGGER trg_usage_ledger_entries_price_lock
BEFORE INSERT ON app.usage_ledger_entries
FOR EACH ROW EXECUTE FUNCTION app.enforce_usage_ledger_price_lock();

CREATE INDEX idx_price_versions_deployment_effective
    ON app.price_versions (deployment_id, region, status, effective_at DESC, id);

CREATE INDEX idx_usage_ledger_entries_price_version
    ON app.usage_ledger_entries (price_version_id, token_type, id);

COMMENT ON TABLE app.price_versions IS
    'Deployment/region/currency price publication selected by immutable effective time';
COMMENT ON TABLE app.price_version_rates IS
    'Per-token-type unit and exact-micros rates locked when the parent version is published';
COMMENT ON COLUMN app.usage_ledger_entries.price_version_id IS
    'Immutable price publication selected for this observed usage fact';
COMMENT ON COLUMN app.usage_ledger_entries.amount_micros IS
    'Calculated amount in the locked price version currency; adjustment signs are added by P10-T08';

COMMIT;
