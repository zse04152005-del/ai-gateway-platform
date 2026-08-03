BEGIN;

ALTER TABLE app.usage_ledger_entries
    DROP CONSTRAINT usage_ledger_entries_token_type_valid,
    DROP CONSTRAINT usage_ledger_entries_source_valid,
    ADD CONSTRAINT usage_ledger_entries_token_type_format CHECK (
        char_length(token_type) BETWEEN 1 AND 64
        AND token_type ~ '^[a-z][a-z0-9_]*$'
    ),
    ADD CONSTRAINT usage_ledger_entries_source_format CHECK (
        char_length(source) BETWEEN 1 AND 64
        AND source ~ '^[a-z][a-z0-9_]*$'
    );

COMMENT ON COLUMN app.usage_ledger_entries.token_type IS NULL;
COMMENT ON COLUMN app.usage_ledger_entries.source IS NULL;

COMMIT;
