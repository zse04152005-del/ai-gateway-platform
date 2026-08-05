BEGIN;

ALTER TABLE app.usage_event_outbox
    ADD COLUMN billing_unit text NOT NULL DEFAULT 'token',
    ADD CONSTRAINT usage_event_outbox_billing_unit_valid CHECK (billing_unit = 'token');

CREATE FUNCTION app.reject_usage_event_outbox_billing_unit_update()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
BEGIN
    IF NEW.billing_unit IS DISTINCT FROM OLD.billing_unit THEN
        RAISE EXCEPTION 'usage event outbox billing unit is immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'usage_event_outbox_billing_unit_immutable';
    END IF;
    RETURN NEW;
END;
$function$;

CREATE TRIGGER trg_usage_event_outbox_billing_unit_immutable
BEFORE UPDATE ON app.usage_event_outbox
FOR EACH ROW EXECUTE FUNCTION app.reject_usage_event_outbox_billing_unit_update();

COMMENT ON COLUMN app.usage_event_outbox.billing_unit IS
    'Explicit quantity unit; version 1 gateway events are normalized Token counts';

COMMIT;
