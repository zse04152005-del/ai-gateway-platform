BEGIN;

DO $block$
BEGIN
    IF EXISTS (
        SELECT 1 FROM app.usage_ledger_entries WHERE source = 'adjustment'
    ) THEN
        RAISE EXCEPTION 'usage ledger adjustments must be empty before migration 20 down'
            USING ERRCODE = '23514';
    END IF;
END;
$block$;

DROP TRIGGER trg_usage_ledger_entries_adjustment_integrity ON app.usage_ledger_entries;
DROP FUNCTION app.enforce_usage_ledger_adjustment_integrity();

ALTER TABLE app.usage_ledger_entries
    DROP CONSTRAINT usage_ledger_entries_estimate_metadata_valid,
    DROP CONSTRAINT usage_ledger_entries_adjustment_metadata_valid,
    DROP CONSTRAINT usage_ledger_entries_amount_valid,
    DROP CONSTRAINT usage_ledger_entries_quantity_valid,
    DROP CONSTRAINT usage_ledger_entries_adjustment_idempotency_unique,
    DROP CONSTRAINT usage_ledger_entries_adjusts_event_fk,
    DROP COLUMN adjustment_result_amount_micros,
    DROP COLUMN adjustment_result_quantity,
    DROP COLUMN adjustment_actor,
    DROP COLUMN adjustment_reference,
    DROP COLUMN adjustment_reason,
    DROP COLUMN adjustment_origin,
    DROP COLUMN adjustment_idempotency_key,
    DROP COLUMN adjusts_event_id,
    ALTER COLUMN event_schema_version SET NOT NULL,
    ADD CONSTRAINT usage_ledger_entries_quantity_valid CHECK (
        quantity BETWEEN 1 AND 9007199254740991
    ),
    ADD CONSTRAINT usage_ledger_entries_amount_valid CHECK (
        amount_micros BETWEEN 0 AND 9007199254740991
    ),
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

COMMIT;
