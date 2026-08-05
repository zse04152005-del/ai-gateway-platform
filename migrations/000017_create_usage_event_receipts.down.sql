BEGIN;

DROP TRIGGER IF EXISTS trg_usage_event_receipts_append_only ON app.usage_event_receipts;
DROP FUNCTION IF EXISTS app.reject_usage_event_receipt_mutation();
DROP TABLE IF EXISTS app.usage_event_receipts;

COMMIT;
