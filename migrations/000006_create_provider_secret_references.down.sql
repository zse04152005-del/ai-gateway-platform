BEGIN;

ALTER TABLE app.deployments
    DROP CONSTRAINT deployments_provider_secret_reference_fk,
    DROP COLUMN secret_reference_id;

DROP TABLE app.provider_secret_references;

COMMIT;
