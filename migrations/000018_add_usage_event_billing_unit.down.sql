BEGIN;

DROP TRIGGER IF EXISTS trg_usage_event_outbox_billing_unit_immutable ON app.usage_event_outbox;
DROP FUNCTION IF EXISTS app.reject_usage_event_outbox_billing_unit_update();
ALTER TABLE app.usage_event_outbox DROP COLUMN IF EXISTS billing_unit;

COMMIT;
