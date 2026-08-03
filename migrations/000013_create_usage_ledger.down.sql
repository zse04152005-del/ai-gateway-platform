BEGIN;

DROP TRIGGER IF EXISTS trg_usage_ledger_entries_append_only ON app.usage_ledger_entries;
DROP TABLE IF EXISTS app.usage_ledger_entries;
DROP FUNCTION IF EXISTS app.reject_usage_ledger_mutation();

ALTER TABLE app.route_attempts
    DROP CONSTRAINT IF EXISTS route_attempts_request_id_unique;

COMMIT;
