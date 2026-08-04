BEGIN;

DROP TRIGGER IF EXISTS trg_usage_event_outbox_delete ON app.usage_event_outbox;
DROP TRIGGER IF EXISTS trg_usage_event_outbox_update ON app.usage_event_outbox;
DROP FUNCTION IF EXISTS app.enforce_usage_event_outbox_delete();
DROP FUNCTION IF EXISTS app.enforce_usage_event_outbox_update();
DROP TABLE IF EXISTS app.usage_event_outbox;

ALTER TABLE app.route_attempts
    DROP CONSTRAINT IF EXISTS route_attempts_request_attempt_deployment_unique;

COMMIT;
