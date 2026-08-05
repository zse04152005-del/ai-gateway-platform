BEGIN;

CREATE TABLE app.usage_event_receipts (
    event_id uuid PRIMARY KEY,
    schema_version smallint NOT NULL,
    payload_sha256 bytea NOT NULL,
    consumer_group text NOT NULL,
    consumed_at timestamptz NOT NULL,
    created_by text NOT NULL,
    CONSTRAINT usage_event_receipts_ledger_fk FOREIGN KEY (event_id)
        REFERENCES app.usage_ledger_entries (event_id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT
        DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT usage_event_receipts_schema_version_valid CHECK (schema_version = 1),
    CONSTRAINT usage_event_receipts_payload_sha256_valid CHECK (octet_length(payload_sha256) = 32),
    CONSTRAINT usage_event_receipts_consumer_group_valid CHECK (
        consumer_group ~ '^[a-z0-9][a-z0-9._-]{2,127}$'
    ),
    CONSTRAINT usage_event_receipts_created_by_valid CHECK (
        created_by = btrim(created_by) AND char_length(created_by) BETWEEN 1 AND 200
    )
);

CREATE FUNCTION app.reject_usage_event_receipt_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
BEGIN
    RAISE EXCEPTION 'usage event receipts are append-only'
        USING ERRCODE = '23514', CONSTRAINT = 'usage_event_receipts_append_only';
END;
$function$;

CREATE TRIGGER trg_usage_event_receipts_append_only
BEFORE UPDATE OR DELETE ON app.usage_event_receipts
FOR EACH ROW EXECUTE FUNCTION app.reject_usage_event_receipt_mutation();

CREATE INDEX idx_usage_event_receipts_consumed_at
    ON app.usage_event_receipts (consumed_at DESC, event_id);

COMMENT ON TABLE app.usage_event_receipts IS
    'Immutable semantic idempotency receipt committed atomically with one usage ledger entry';
COMMENT ON COLUMN app.usage_event_receipts.payload_sha256 IS
    'SHA-256 of the validated canonical UsageEvent; the same event ID with different facts is rejected';

COMMIT;
