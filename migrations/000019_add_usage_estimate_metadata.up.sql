BEGIN;

ALTER TABLE app.usage_event_outbox
    DROP CONSTRAINT usage_event_outbox_schema_version_valid,
    ADD COLUMN tokenizer text,
    ADD COLUMN tokenizer_version text,
    ADD COLUMN physical_model text,
    ADD COLUMN deployment_version bigint,
    ADD COLUMN provider_protocol_version text,
    ADD CONSTRAINT usage_event_outbox_schema_version_valid CHECK (schema_version IN (1, 2)),
    ADD CONSTRAINT usage_event_outbox_estimate_metadata_valid CHECK (
        (
            schema_version = 1
            AND tokenizer IS NULL
            AND tokenizer_version IS NULL
            AND physical_model IS NULL
            AND deployment_version IS NULL
            AND provider_protocol_version IS NULL
        )
        OR (
            schema_version = 2
            AND source <> 'estimated'
            AND tokenizer IS NULL
            AND tokenizer_version IS NULL
            AND physical_model IS NULL
            AND deployment_version IS NULL
            AND provider_protocol_version IS NULL
        )
        OR (
            schema_version = 2
            AND source = 'estimated'
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

CREATE FUNCTION app.reject_usage_event_outbox_estimate_update()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
BEGIN
    IF NEW.tokenizer IS DISTINCT FROM OLD.tokenizer
       OR NEW.tokenizer_version IS DISTINCT FROM OLD.tokenizer_version
       OR NEW.physical_model IS DISTINCT FROM OLD.physical_model
       OR NEW.deployment_version IS DISTINCT FROM OLD.deployment_version
       OR NEW.provider_protocol_version IS DISTINCT FROM OLD.provider_protocol_version THEN
        RAISE EXCEPTION 'usage event estimate metadata is immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'usage_event_outbox_estimate_metadata_immutable';
    END IF;
    RETURN NEW;
END;
$function$;

CREATE TRIGGER trg_usage_event_outbox_estimate_immutable
BEFORE UPDATE ON app.usage_event_outbox
FOR EACH ROW EXECUTE FUNCTION app.reject_usage_event_outbox_estimate_update();

ALTER TABLE app.usage_event_receipts
    DROP CONSTRAINT usage_event_receipts_schema_version_valid,
    ADD CONSTRAINT usage_event_receipts_schema_version_valid CHECK (schema_version IN (1, 2));

ALTER TABLE app.usage_ledger_entries
    ADD COLUMN event_schema_version smallint NOT NULL DEFAULT 1,
    ADD COLUMN tokenizer text,
    ADD COLUMN tokenizer_version text,
    ADD COLUMN physical_model text,
    ADD COLUMN deployment_version bigint,
    ADD COLUMN provider_protocol_version text,
    ADD CONSTRAINT usage_ledger_entries_estimate_metadata_valid CHECK (
        (
            event_schema_version = 1
            AND tokenizer IS NULL
            AND tokenizer_version IS NULL
            AND physical_model IS NULL
            AND deployment_version IS NULL
            AND provider_protocol_version IS NULL
        )
        OR (
            event_schema_version = 2
            AND source <> 'estimated'
            AND tokenizer IS NULL
            AND tokenizer_version IS NULL
            AND physical_model IS NULL
            AND deployment_version IS NULL
            AND provider_protocol_version IS NULL
        )
        OR (
            event_schema_version = 2
            AND source = 'estimated'
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

COMMENT ON COLUMN app.usage_event_outbox.tokenizer IS
    'Gateway-local tokenizer identity for schema v2 estimated facts; never provider billing evidence';
COMMENT ON COLUMN app.usage_ledger_entries.event_schema_version IS
    'UsageEvent wire schema that supplied this immutable ledger fact';
COMMENT ON COLUMN app.usage_ledger_entries.tokenizer IS
    'Gateway-local tokenizer identity retained with schema v2 estimated ledger facts';

COMMIT;
