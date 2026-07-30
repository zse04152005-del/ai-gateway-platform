BEGIN;

DROP TABLE app.virtual_api_keys;
DROP FUNCTION app.valid_virtual_key_limits(jsonb);
DROP FUNCTION app.valid_virtual_key_allowed_models(text[]);

COMMIT;
