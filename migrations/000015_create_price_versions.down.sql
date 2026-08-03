BEGIN;

DROP TRIGGER IF EXISTS trg_usage_ledger_entries_price_lock ON app.usage_ledger_entries;
DROP FUNCTION IF EXISTS app.enforce_usage_ledger_price_lock();

DROP INDEX IF EXISTS app.idx_usage_ledger_entries_price_version;

ALTER TABLE app.usage_ledger_entries
    DROP CONSTRAINT IF EXISTS usage_ledger_entries_price_rate_fk,
    DROP CONSTRAINT IF EXISTS usage_ledger_entries_amount_valid,
    DROP COLUMN IF EXISTS price_version_id,
    DROP COLUMN IF EXISTS amount_micros;

DROP TRIGGER IF EXISTS trg_price_version_rates_append_only ON app.price_version_rates;
DROP TRIGGER IF EXISTS trg_price_version_rates_draft_insert ON app.price_version_rates;
DROP TRIGGER IF EXISTS trg_price_versions_no_delete ON app.price_versions;
DROP TRIGGER IF EXISTS trg_price_versions_publish ON app.price_versions;
DROP TRIGGER IF EXISTS trg_price_versions_initial_draft ON app.price_versions;

DROP FUNCTION IF EXISTS app.reject_price_version_rate_mutation();
DROP FUNCTION IF EXISTS app.enforce_price_version_rate_insert();
DROP FUNCTION IF EXISTS app.reject_price_version_delete();
DROP FUNCTION IF EXISTS app.enforce_price_version_publish();
DROP FUNCTION IF EXISTS app.enforce_price_version_insert();

DROP TABLE IF EXISTS app.price_version_rates;
DROP TABLE IF EXISTS app.price_versions;

ALTER TABLE app.deployments
    DROP CONSTRAINT IF EXISTS deployments_id_region_unique;

COMMIT;
