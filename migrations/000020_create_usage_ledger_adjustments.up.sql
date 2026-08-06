BEGIN;

ALTER TABLE app.usage_ledger_entries
    DROP CONSTRAINT usage_ledger_entries_quantity_valid,
    DROP CONSTRAINT usage_ledger_entries_amount_valid,
    DROP CONSTRAINT usage_ledger_entries_estimate_metadata_valid,
    ALTER COLUMN event_schema_version DROP NOT NULL,
    ADD COLUMN adjusts_event_id uuid,
    ADD COLUMN adjustment_idempotency_key text,
    ADD COLUMN adjustment_origin text,
    ADD COLUMN adjustment_reason text,
    ADD COLUMN adjustment_reference text,
    ADD COLUMN adjustment_actor text,
    ADD COLUMN adjustment_result_quantity bigint,
    ADD COLUMN adjustment_result_amount_micros bigint,
    ADD CONSTRAINT usage_ledger_entries_adjusts_event_fk FOREIGN KEY (adjusts_event_id)
        REFERENCES app.usage_ledger_entries (event_id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    ADD CONSTRAINT usage_ledger_entries_adjustment_idempotency_unique UNIQUE (
        tenant_id, adjustment_idempotency_key
    ),
    ADD CONSTRAINT usage_ledger_entries_quantity_valid CHECK (
        (
            source = 'adjustment'
            AND quantity BETWEEN -9007199254740991 AND 9007199254740991
        )
        OR (
            source <> 'adjustment'
            AND quantity BETWEEN 1 AND 9007199254740991
        )
    ),
    ADD CONSTRAINT usage_ledger_entries_amount_valid CHECK (
        (
            source = 'adjustment'
            AND amount_micros BETWEEN -9007199254740991 AND 9007199254740991
        )
        OR (
            source <> 'adjustment'
            AND amount_micros BETWEEN 0 AND 9007199254740991
        )
    ),
    ADD CONSTRAINT usage_ledger_entries_adjustment_metadata_valid CHECK (
        (
            source <> 'adjustment'
            AND adjusts_event_id IS NULL
            AND adjustment_idempotency_key IS NULL
            AND adjustment_origin IS NULL
            AND adjustment_reason IS NULL
            AND adjustment_reference IS NULL
            AND adjustment_actor IS NULL
            AND adjustment_result_quantity IS NULL
            AND adjustment_result_amount_micros IS NULL
        )
        OR (
            source = 'adjustment'
            AND adjusts_event_id IS NOT NULL
            AND adjustment_idempotency_key IS NOT NULL
            AND adjustment_idempotency_key = btrim(adjustment_idempotency_key)
            AND char_length(adjustment_idempotency_key) BETWEEN 8 AND 128
            AND adjustment_idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]*$'
            AND adjustment_origin IS NOT NULL
            AND adjustment_origin IN ('manual', 'provider_reconciliation', 'system_repair')
            AND adjustment_reason IS NOT NULL
            AND adjustment_reason ~ '^[a-z][a-z0-9._:-]{0,127}$'
            AND adjustment_reference IS NOT NULL
            AND adjustment_reference = btrim(adjustment_reference)
            AND char_length(adjustment_reference) BETWEEN 1 AND 200
            AND adjustment_reference ~ '^[A-Za-z0-9][A-Za-z0-9._:@/-]*$'
            AND adjustment_actor IS NOT NULL
            AND adjustment_actor = btrim(adjustment_actor)
            AND char_length(adjustment_actor) BETWEEN 1 AND 200
            AND adjustment_actor ~ '^[A-Za-z0-9][A-Za-z0-9._:@/-]*$'
            AND adjustment_result_quantity IS NOT NULL
            AND adjustment_result_quantity BETWEEN 0 AND 9007199254740991
            AND adjustment_result_amount_micros IS NOT NULL
            AND adjustment_result_amount_micros BETWEEN 0 AND 9007199254740991
            AND (quantity <> 0 OR amount_micros <> 0)
        )
    ),
    ADD CONSTRAINT usage_ledger_entries_estimate_metadata_valid CHECK (
        (
            source = 'adjustment'
            AND event_schema_version IS NULL
            AND tokenizer IS NULL
            AND tokenizer_version IS NULL
            AND physical_model IS NULL
            AND deployment_version IS NULL
            AND provider_protocol_version IS NULL
        )
        OR (
            source <> 'adjustment'
            AND event_schema_version = 1
            AND tokenizer IS NULL
            AND tokenizer_version IS NULL
            AND physical_model IS NULL
            AND deployment_version IS NULL
            AND provider_protocol_version IS NULL
        )
        OR (
            source <> 'adjustment'
            AND event_schema_version = 2
            AND source <> 'estimated'
            AND tokenizer IS NULL
            AND tokenizer_version IS NULL
            AND physical_model IS NULL
            AND deployment_version IS NULL
            AND provider_protocol_version IS NULL
        )
        OR (
            source = 'estimated'
            AND event_schema_version = 2
            AND tokenizer IS NOT NULL
            AND tokenizer ~ '^[a-z][a-z0-9._:-]{0,127}$'
            AND tokenizer_version IS NOT NULL
            AND tokenizer_version ~ '^[a-z][a-z0-9._:-]{0,127}$'
            AND physical_model IS NOT NULL
            AND physical_model ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]{0,199}$'
            AND deployment_version IS NOT NULL
            AND deployment_version > 0
            AND provider_protocol_version IS NOT NULL
            AND provider_protocol_version ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]{0,63}$'
        )
    );

CREATE FUNCTION app.enforce_usage_ledger_adjustment_integrity()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
DECLARE
    target_tenant_id uuid;
    target_request_id text;
    target_attempt_id uuid;
    target_token_type text;
    target_price_version_id uuid;
    target_source text;
    target_quantity bigint;
    target_amount_micros bigint;
    target_observed_at timestamptz;
    prior_quantity numeric;
    prior_amount_micros numeric;
BEGIN
    IF NEW.source <> 'adjustment' THEN
        RETURN NEW;
    END IF;

    SELECT tenant_id, request_id, attempt_id, token_type, price_version_id,
        source, quantity, amount_micros, observed_at
    INTO target_tenant_id, target_request_id, target_attempt_id, target_token_type,
        target_price_version_id, target_source, target_quantity, target_amount_micros,
        target_observed_at
    FROM app.usage_ledger_entries
    WHERE event_id = NEW.adjusts_event_id
    FOR UPDATE;

    IF NOT FOUND OR target_source = 'adjustment' THEN
        RAISE EXCEPTION 'adjustment must reference an existing original ledger entry'
            USING ERRCODE = '23514', CONSTRAINT = 'usage_ledger_entries_adjustment_target_original';
    END IF;
    IF NEW.tenant_id <> target_tenant_id
       OR NEW.request_id <> target_request_id
       OR NEW.attempt_id IS DISTINCT FROM target_attempt_id
       OR NEW.token_type <> target_token_type
       OR NEW.price_version_id <> target_price_version_id THEN
        RAISE EXCEPTION 'adjustment attribution must match its original ledger entry'
            USING ERRCODE = '23514', CONSTRAINT = 'usage_ledger_entries_adjustment_attribution';
    END IF;
    IF NEW.observed_at < target_observed_at OR NEW.created_by <> NEW.adjustment_actor THEN
        RAISE EXCEPTION 'adjustment audit identity or time is invalid'
            USING ERRCODE = '23514', CONSTRAINT = 'usage_ledger_entries_adjustment_audit';
    END IF;

    SELECT COALESCE(sum(quantity), 0), COALESCE(sum(amount_micros), 0)
    INTO prior_quantity, prior_amount_micros
    FROM app.usage_ledger_entries
    WHERE adjusts_event_id = NEW.adjusts_event_id;

    IF target_quantity::numeric + prior_quantity + NEW.quantity
           <> NEW.adjustment_result_quantity::numeric
       OR target_amount_micros::numeric + prior_amount_micros + NEW.amount_micros
           <> NEW.adjustment_result_amount_micros::numeric THEN
        RAISE EXCEPTION 'adjustment result does not match the append-only correction chain'
            USING ERRCODE = '23514', CONSTRAINT = 'usage_ledger_entries_adjustment_result';
    END IF;
    RETURN NEW;
END;
$function$;

CREATE TRIGGER trg_usage_ledger_entries_adjustment_integrity
BEFORE INSERT ON app.usage_ledger_entries
FOR EACH ROW EXECUTE FUNCTION app.enforce_usage_ledger_adjustment_integrity();

CREATE INDEX idx_usage_ledger_entries_adjustment_target
    ON app.usage_ledger_entries (adjusts_event_id, id)
    WHERE adjusts_event_id IS NOT NULL;

COMMENT ON COLUMN app.usage_ledger_entries.adjusts_event_id IS
    'Original non-adjustment event corrected by this immutable signed ledger entry';
COMMENT ON COLUMN app.usage_ledger_entries.adjustment_origin IS
    'Finite workflow that authorized the correction; never free-form provider evidence';
COMMENT ON COLUMN app.usage_ledger_entries.adjustment_reference IS
    'Content-free external ticket, reconciliation item, or repair reference';
COMMENT ON COLUMN app.usage_ledger_entries.adjustment_actor IS
    'Authenticated operator or service identity responsible for the correction';

COMMIT;
