BEGIN;

DROP TABLE app.logical_model_deployments;
DROP TABLE app.deployments;
DROP TABLE app.logical_models;
DROP TABLE app.providers;
DROP FUNCTION app.enforce_deployment_contract_update();
DROP FUNCTION app.enforce_logical_model_contract_update();
DROP FUNCTION app.enforce_catalog_binding_contract();
DROP FUNCTION app.catalog_deployment_satisfies(jsonb, text[], jsonb, text);
DROP FUNCTION app.valid_catalog_capabilities(jsonb);
DROP FUNCTION app.valid_catalog_requirements(jsonb);
DROP FUNCTION app.valid_catalog_regions(text[]);

COMMIT;
