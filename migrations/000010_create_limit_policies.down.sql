BEGIN;

DROP INDEX IF EXISTS app.idx_virtual_api_keys_tenant_limit_policy;
DROP INDEX IF EXISTS app.idx_projects_tenant_limit_policy;
DROP INDEX IF EXISTS app.idx_tenants_limit_policy;

ALTER TABLE app.virtual_api_keys
    DROP CONSTRAINT IF EXISTS virtual_api_keys_limit_policy_source_valid,
    DROP CONSTRAINT IF EXISTS virtual_api_keys_limit_policy_fk,
    DROP COLUMN IF EXISTS limit_policy_id;

ALTER TABLE app.projects
    DROP CONSTRAINT IF EXISTS projects_limit_policy_source_valid,
    DROP CONSTRAINT IF EXISTS projects_limit_policy_fk,
    DROP COLUMN IF EXISTS limit_policy_id;

ALTER TABLE app.tenants
    DROP CONSTRAINT IF EXISTS tenants_limit_policy_source_valid,
    DROP CONSTRAINT IF EXISTS tenants_limit_policy_fk,
    DROP COLUMN IF EXISTS limit_policy_id;

DROP TABLE IF EXISTS app.limit_policies;

COMMIT;
