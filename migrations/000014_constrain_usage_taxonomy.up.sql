BEGIN;

ALTER TABLE app.usage_ledger_entries
    DROP CONSTRAINT usage_ledger_entries_token_type_format,
    DROP CONSTRAINT usage_ledger_entries_source_format,
    ADD CONSTRAINT usage_ledger_entries_token_type_valid CHECK (
        token_type IN (
            'input', 'output', 'cache_read', 'cache_write', 'reasoning',
            'audio_input', 'audio_output', 'image_input', 'image_output'
        )
    ) NOT VALID,
    ADD CONSTRAINT usage_ledger_entries_source_valid CHECK (
        source IN ('provider', 'estimated', 'reconciled', 'adjustment')
    ) NOT VALID;

ALTER TABLE app.usage_ledger_entries
    VALIDATE CONSTRAINT usage_ledger_entries_token_type_valid,
    VALIDATE CONSTRAINT usage_ledger_entries_source_valid;

COMMENT ON COLUMN app.usage_ledger_entries.token_type IS
    'Finite independently priced usage dimension shared with internal/metering';
COMMENT ON COLUMN app.usage_ledger_entries.source IS
    'Finite producer taxonomy: provider, estimated, reconciled, or adjustment';

COMMIT;
