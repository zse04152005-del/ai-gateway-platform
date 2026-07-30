BEGIN;

-- Development rollback requires every retained request model to exist in the
-- current tenant catalog. Production uses corrective forward migrations.
ALTER TABLE app.gateway_requests
    ADD CONSTRAINT gateway_requests_logical_model_fk
    FOREIGN KEY (tenant_id, logical_model)
    REFERENCES app.logical_models (tenant_id, name)
    ON UPDATE RESTRICT
    ON DELETE RESTRICT;

COMMENT ON COLUMN app.gateway_requests.logical_model IS NULL;

COMMIT;
