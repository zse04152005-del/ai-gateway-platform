BEGIN;

CREATE TABLE app.tenants (
    id uuid PRIMARY KEY,
    slug text NOT NULL,
    name text NOT NULL,
    status text NOT NULL DEFAULT 'active',
    quota_policy_ref text,
    version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_by text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_by text NOT NULL,
    disabled_at timestamptz,
    CONSTRAINT tenants_slug_unique UNIQUE (slug),
    CONSTRAINT tenants_slug_format CHECK (
        char_length(slug) BETWEEN 3 AND 63
        AND slug ~ '^[a-z0-9][a-z0-9-]*[a-z0-9]$'
    ),
    CONSTRAINT tenants_name_format CHECK (
        name = btrim(name)
        AND char_length(name) BETWEEN 1 AND 200
    ),
    CONSTRAINT tenants_status_valid CHECK (status IN ('active', 'suspended', 'disabled')),
    CONSTRAINT tenants_quota_policy_ref_format CHECK (
        quota_policy_ref IS NULL
        OR (
            quota_policy_ref = btrim(quota_policy_ref)
            AND char_length(quota_policy_ref) BETWEEN 1 AND 128
        )
    ),
    CONSTRAINT tenants_version_positive CHECK (version > 0),
    CONSTRAINT tenants_created_by_format CHECK (
        created_by = btrim(created_by)
        AND char_length(created_by) BETWEEN 1 AND 200
    ),
    CONSTRAINT tenants_updated_by_format CHECK (
        updated_by = btrim(updated_by)
        AND char_length(updated_by) BETWEEN 1 AND 200
    ),
    CONSTRAINT tenants_update_time_valid CHECK (updated_at >= created_at),
    CONSTRAINT tenants_disabled_time_valid CHECK (
        (status = 'disabled') = (disabled_at IS NOT NULL)
        AND (disabled_at IS NULL OR disabled_at >= created_at)
    )
);

CREATE INDEX idx_tenants_status_created
    ON app.tenants (status, created_at DESC, id);

CREATE TABLE app.projects (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL,
    slug text NOT NULL,
    name text NOT NULL,
    status text NOT NULL DEFAULT 'active',
    quota_policy_ref text,
    version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_by text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_by text NOT NULL,
    disabled_at timestamptz,
    CONSTRAINT projects_tenant_fk FOREIGN KEY (tenant_id)
        REFERENCES app.tenants (id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT projects_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT projects_tenant_slug_unique UNIQUE (tenant_id, slug),
    CONSTRAINT projects_slug_format CHECK (
        char_length(slug) BETWEEN 3 AND 63
        AND slug ~ '^[a-z0-9][a-z0-9-]*[a-z0-9]$'
    ),
    CONSTRAINT projects_name_format CHECK (
        name = btrim(name)
        AND char_length(name) BETWEEN 1 AND 200
    ),
    CONSTRAINT projects_status_valid CHECK (status IN ('active', 'suspended', 'disabled')),
    CONSTRAINT projects_quota_policy_ref_format CHECK (
        quota_policy_ref IS NULL
        OR (
            quota_policy_ref = btrim(quota_policy_ref)
            AND char_length(quota_policy_ref) BETWEEN 1 AND 128
        )
    ),
    CONSTRAINT projects_version_positive CHECK (version > 0),
    CONSTRAINT projects_created_by_format CHECK (
        created_by = btrim(created_by)
        AND char_length(created_by) BETWEEN 1 AND 200
    ),
    CONSTRAINT projects_updated_by_format CHECK (
        updated_by = btrim(updated_by)
        AND char_length(updated_by) BETWEEN 1 AND 200
    ),
    CONSTRAINT projects_update_time_valid CHECK (updated_at >= created_at),
    CONSTRAINT projects_disabled_time_valid CHECK (
        (status = 'disabled') = (disabled_at IS NOT NULL)
        AND (disabled_at IS NULL OR disabled_at >= created_at)
    )
);

CREATE UNIQUE INDEX uq_projects_tenant_name_ci
    ON app.projects (tenant_id, lower(name));

CREATE INDEX idx_projects_tenant_status_created
    ON app.projects (tenant_id, status, created_at DESC, id);

COMMENT ON TABLE app.tenants IS 'Top-level isolation and governance boundary';
COMMENT ON COLUMN app.tenants.quota_policy_ref IS 'Opaque quota policy reference; NULL means platform default';
COMMENT ON COLUMN app.tenants.version IS 'Application-managed optimistic concurrency version';

COMMENT ON TABLE app.projects IS 'Application and cost-allocation boundary within one tenant';
COMMENT ON COLUMN app.projects.quota_policy_ref IS 'Project override; NULL inherits the tenant quota policy';
COMMENT ON CONSTRAINT projects_tenant_id_unique ON app.projects IS
    'Composite tenant identity target for child-table isolation foreign keys';

COMMIT;
