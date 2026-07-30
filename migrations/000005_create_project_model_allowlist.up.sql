BEGIN;

CREATE TABLE app.project_logical_models (
    tenant_id uuid NOT NULL,
    project_id uuid NOT NULL,
    logical_model_id uuid NOT NULL,
    status text NOT NULL DEFAULT 'active',
    version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_by text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_by text NOT NULL,
    PRIMARY KEY (tenant_id, project_id, logical_model_id),
    CONSTRAINT project_logical_models_project_fk FOREIGN KEY (tenant_id, project_id)
        REFERENCES app.projects (tenant_id, id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT project_logical_models_model_fk FOREIGN KEY (tenant_id, logical_model_id)
        REFERENCES app.logical_models (tenant_id, id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT project_logical_models_status_valid CHECK (status IN ('active', 'disabled')),
    CONSTRAINT project_logical_models_version_positive CHECK (version > 0),
    CONSTRAINT project_logical_models_created_by_format CHECK (
        created_by = btrim(created_by) AND char_length(created_by) BETWEEN 1 AND 200
    ),
    CONSTRAINT project_logical_models_updated_by_format CHECK (
        updated_by = btrim(updated_by) AND char_length(updated_by) BETWEEN 1 AND 200
    ),
    CONSTRAINT project_logical_models_update_time_valid CHECK (updated_at >= created_at)
);

CREATE INDEX idx_project_logical_models_available
    ON app.project_logical_models (tenant_id, project_id, status, logical_model_id);

COMMENT ON TABLE app.project_logical_models IS
    'Tenant-safe project allowlist for logical models; virtual key allowlists can only narrow this set';
COMMENT ON COLUMN app.project_logical_models.tenant_id IS
    'Duplicated isolation key used in composite foreign keys to prevent cross-tenant model grants';

COMMIT;
