BEGIN;

CREATE TABLE app.limit_policies (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL,
    policy_ref text NOT NULL,
    status text NOT NULL DEFAULT 'active',
    rpm_soft bigint,
    rpm_hard bigint,
    tpm_soft bigint,
    tpm_hard bigint,
    concurrency_soft bigint,
    concurrency_hard bigint,
    version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_by text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_by text NOT NULL,
    disabled_at timestamptz,
    CONSTRAINT limit_policies_tenant_fk FOREIGN KEY (tenant_id)
        REFERENCES app.tenants (id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT limit_policies_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT limit_policies_tenant_ref_unique UNIQUE (tenant_id, policy_ref),
    CONSTRAINT limit_policies_ref_format CHECK (
        char_length(policy_ref) BETWEEN 1 AND 128
        AND policy_ref ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]*$'
    ),
    CONSTRAINT limit_policies_status_valid CHECK (status IN ('active', 'disabled')),
    CONSTRAINT limit_policies_has_override CHECK (
        num_nonnulls(
            rpm_soft, rpm_hard,
            tpm_soft, tpm_hard,
            concurrency_soft, concurrency_hard
        ) > 0
    ),
    CONSTRAINT limit_policies_values_valid CHECK (
        (rpm_soft IS NULL OR rpm_soft BETWEEN 1 AND 9007199254740991)
        AND (rpm_hard IS NULL OR rpm_hard BETWEEN 1 AND 9007199254740991)
        AND (tpm_soft IS NULL OR tpm_soft BETWEEN 1 AND 9007199254740991)
        AND (tpm_hard IS NULL OR tpm_hard BETWEEN 1 AND 9007199254740991)
        AND (concurrency_soft IS NULL OR concurrency_soft BETWEEN 1 AND 9007199254740991)
        AND (concurrency_hard IS NULL OR concurrency_hard BETWEEN 1 AND 9007199254740991)
    ),
    CONSTRAINT limit_policies_local_pairs_valid CHECK (
        (rpm_soft IS NULL OR rpm_hard IS NULL OR rpm_soft <= rpm_hard)
        AND (tpm_soft IS NULL OR tpm_hard IS NULL OR tpm_soft <= tpm_hard)
        AND (
            concurrency_soft IS NULL
            OR concurrency_hard IS NULL
            OR concurrency_soft <= concurrency_hard
        )
    ),
    CONSTRAINT limit_policies_version_positive CHECK (version > 0),
    CONSTRAINT limit_policies_created_by_format CHECK (
        created_by = btrim(created_by)
        AND char_length(created_by) BETWEEN 1 AND 200
    ),
    CONSTRAINT limit_policies_updated_by_format CHECK (
        updated_by = btrim(updated_by)
        AND char_length(updated_by) BETWEEN 1 AND 200
    ),
    CONSTRAINT limit_policies_update_time_valid CHECK (updated_at >= created_at),
    CONSTRAINT limit_policies_disabled_time_valid CHECK (
        (status = 'disabled') = (disabled_at IS NOT NULL)
        AND (disabled_at IS NULL OR disabled_at >= created_at)
    )
);

CREATE INDEX idx_limit_policies_tenant_status_updated
    ON app.limit_policies (tenant_id, status, updated_at DESC, id);

ALTER TABLE app.tenants
    ADD COLUMN limit_policy_id uuid,
    ADD CONSTRAINT tenants_limit_policy_fk FOREIGN KEY (id, limit_policy_id)
        REFERENCES app.limit_policies (tenant_id, id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    ADD CONSTRAINT tenants_limit_policy_source_valid CHECK (
        quota_policy_ref IS NULL OR limit_policy_id IS NULL
    );

ALTER TABLE app.projects
    ADD COLUMN limit_policy_id uuid,
    ADD CONSTRAINT projects_limit_policy_fk FOREIGN KEY (tenant_id, limit_policy_id)
        REFERENCES app.limit_policies (tenant_id, id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    ADD CONSTRAINT projects_limit_policy_source_valid CHECK (
        quota_policy_ref IS NULL OR limit_policy_id IS NULL
    );

ALTER TABLE app.virtual_api_keys
    ADD COLUMN limit_policy_id uuid,
    ADD CONSTRAINT virtual_api_keys_limit_policy_fk FOREIGN KEY (tenant_id, limit_policy_id)
        REFERENCES app.limit_policies (tenant_id, id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    ADD CONSTRAINT virtual_api_keys_limit_policy_source_valid CHECK (
        limits IS NULL OR limit_policy_id IS NULL
    );

CREATE INDEX idx_tenants_limit_policy
    ON app.tenants (limit_policy_id)
    WHERE limit_policy_id IS NOT NULL;

CREATE INDEX idx_projects_tenant_limit_policy
    ON app.projects (tenant_id, limit_policy_id)
    WHERE limit_policy_id IS NOT NULL;

CREATE INDEX idx_virtual_api_keys_tenant_limit_policy
    ON app.virtual_api_keys (tenant_id, limit_policy_id)
    WHERE limit_policy_id IS NOT NULL;

COMMENT ON TABLE app.limit_policies IS
    'Tenant-owned sparse RPM, TPM and concurrency overrides resolved Platform -> Tenant -> Project -> Key';
COMMENT ON COLUMN app.limit_policies.policy_ref IS
    'Stable tenant-local control-plane name; id is the strong stored reference';
COMMENT ON COLUMN app.limit_policies.rpm_soft IS
    'Nullable soft RPM boundary; NULL inherits the same boundary from the parent layer';
COMMENT ON COLUMN app.limit_policies.rpm_hard IS
    'Nullable hard RPM boundary; NULL inherits the same boundary from the parent layer';
COMMENT ON COLUMN app.limit_policies.tpm_soft IS
    'Nullable soft TPM boundary; NULL inherits the same boundary from the parent layer';
COMMENT ON COLUMN app.limit_policies.tpm_hard IS
    'Nullable hard TPM boundary; NULL inherits the same boundary from the parent layer';
COMMENT ON COLUMN app.limit_policies.concurrency_soft IS
    'Nullable soft concurrency boundary; NULL inherits the same boundary from the parent layer';
COMMENT ON COLUMN app.limit_policies.concurrency_hard IS
    'Nullable hard concurrency boundary; NULL inherits the same boundary from the parent layer';
COMMENT ON COLUMN app.tenants.limit_policy_id IS
    'Strong tenant-scoped LimitPolicy reference; NULL inherits the complete platform policy';
COMMENT ON COLUMN app.projects.limit_policy_id IS
    'Strong tenant-scoped LimitPolicy reference; NULL inherits the Tenant layer';
COMMENT ON COLUMN app.virtual_api_keys.limit_policy_id IS
    'Strong tenant-scoped LimitPolicy reference; NULL inherits the Project layer';

COMMIT;
