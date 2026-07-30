BEGIN;

CREATE SCHEMA IF NOT EXISTS app;
CREATE SCHEMA IF NOT EXISTS audit;

COMMENT ON SCHEMA app IS 'AI gateway transactional domain data';
COMMENT ON SCHEMA audit IS 'Append-oriented audit and security records';

COMMIT;

