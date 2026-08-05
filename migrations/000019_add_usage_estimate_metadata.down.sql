BEGIN;

DROP TRIGGER trg_usage_event_outbox_estimate_immutable ON app.usage_event_outbox;
DROP FUNCTION app.reject_usage_event_outbox_estimate_update();

ALTER TABLE app.usage_event_outbox
    DROP CONSTRAINT usage_event_outbox_estimate_metadata_valid,
    DROP CONSTRAINT usage_event_outbox_schema_version_valid,
    DROP COLUMN provider_protocol_version,
    DROP COLUMN deployment_version,
    DROP COLUMN physical_model,
    DROP COLUMN tokenizer_version,
    DROP COLUMN tokenizer,
    ADD CONSTRAINT usage_event_outbox_schema_version_valid CHECK (schema_version = 1);

ALTER TABLE app.usage_event_receipts
    DROP CONSTRAINT usage_event_receipts_schema_version_valid,
    ADD CONSTRAINT usage_event_receipts_schema_version_valid CHECK (schema_version = 1);

ALTER TABLE app.usage_ledger_entries
    DROP CONSTRAINT usage_ledger_entries_estimate_metadata_valid,
    DROP COLUMN provider_protocol_version,
    DROP COLUMN deployment_version,
    DROP COLUMN physical_model,
    DROP COLUMN tokenizer_version,
    DROP COLUMN tokenizer,
    DROP COLUMN event_schema_version;

COMMIT;
