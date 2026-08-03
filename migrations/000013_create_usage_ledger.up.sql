BEGIN;

ALTER TABLE app.route_attempts
    ADD CONSTRAINT route_attempts_request_id_unique UNIQUE (request_id, id);

CREATE TABLE app.usage_ledger_entries (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    request_id text NOT NULL,
    attempt_id uuid,
    token_type text NOT NULL,
    quantity bigint NOT NULL,
    source text NOT NULL,
    observed_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_by text NOT NULL,
    CONSTRAINT usage_ledger_entries_event_id_unique UNIQUE (event_id),
    CONSTRAINT usage_ledger_entries_request_fk FOREIGN KEY (tenant_id, request_id)
        REFERENCES app.gateway_requests (tenant_id, id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT usage_ledger_entries_attempt_fk FOREIGN KEY (request_id, attempt_id)
        REFERENCES app.route_attempts (request_id, id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT usage_ledger_entries_token_type_format CHECK (
        char_length(token_type) BETWEEN 1 AND 64
        AND token_type ~ '^[a-z][a-z0-9_]*$'
    ),
    CONSTRAINT usage_ledger_entries_quantity_valid CHECK (
        quantity BETWEEN 1 AND 9007199254740991
    ),
    CONSTRAINT usage_ledger_entries_source_format CHECK (
        char_length(source) BETWEEN 1 AND 64
        AND source ~ '^[a-z][a-z0-9_]*$'
    ),
    CONSTRAINT usage_ledger_entries_created_by_format CHECK (
        created_by = btrim(created_by) AND char_length(created_by) BETWEEN 1 AND 200
    )
);

CREATE INDEX idx_usage_ledger_entries_tenant_request_time
    ON app.usage_ledger_entries (tenant_id, request_id, observed_at DESC, id);

CREATE INDEX idx_usage_ledger_entries_attempt_time
    ON app.usage_ledger_entries (request_id, attempt_id, observed_at DESC, id)
    WHERE attempt_id IS NOT NULL;

CREATE FUNCTION app.reject_usage_ledger_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
BEGIN
    RAISE EXCEPTION 'usage ledger entries are append-only' USING ERRCODE = '23514';
END;
$function$;

CREATE TRIGGER trg_usage_ledger_entries_append_only
BEFORE UPDATE OR DELETE ON app.usage_ledger_entries
FOR EACH ROW EXECUTE FUNCTION app.reject_usage_ledger_mutation();

COMMENT ON TABLE app.usage_ledger_entries IS
    'Append-only tenant usage facts; one globally unique event ID creates at most one effective entry';
COMMENT ON COLUMN app.usage_ledger_entries.attempt_id IS
    'Physical attempt when applicable; NULL keeps request-level facts such as cache-only usage attributable';
COMMENT ON COLUMN app.usage_ledger_entries.quantity IS
    'Exact integer usage units; token taxonomy and source enumeration are versioned by later metering migrations';

COMMIT;
