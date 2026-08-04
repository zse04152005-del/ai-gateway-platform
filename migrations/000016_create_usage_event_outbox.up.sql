BEGIN;

ALTER TABLE app.route_attempts
    ADD CONSTRAINT route_attempts_request_attempt_deployment_unique
    UNIQUE (request_id, id, deployment_id);

CREATE TABLE app.usage_event_outbox (
    event_id uuid PRIMARY KEY,
    schema_version smallint NOT NULL,
    kind text NOT NULL,
    tenant_id uuid NOT NULL,
    request_id text NOT NULL,
    attempt_id uuid NOT NULL,
    deployment_id uuid NOT NULL,
    token_type text NOT NULL,
    quantity bigint NOT NULL,
    source text NOT NULL,
    usage_complete boolean NOT NULL,
    observed_at timestamptz NOT NULL,
    trace_id text NOT NULL,
    span_id text NOT NULL,
    status text NOT NULL DEFAULT 'pending',
    publish_attempts integer NOT NULL DEFAULT 0,
    available_at timestamptz NOT NULL,
    lease_id uuid,
    lease_expires_at timestamptz,
    published_at timestamptz,
    last_error_code text,
    created_at timestamptz NOT NULL,
    created_by text NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT usage_event_outbox_request_fk FOREIGN KEY (tenant_id, request_id)
        REFERENCES app.gateway_requests (tenant_id, id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT usage_event_outbox_attempt_deployment_fk
        FOREIGN KEY (request_id, attempt_id, deployment_id)
        REFERENCES app.route_attempts (request_id, id, deployment_id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT usage_event_outbox_attempt_dimension_unique
        UNIQUE (request_id, attempt_id, token_type, source),
    CONSTRAINT usage_event_outbox_schema_version_valid CHECK (schema_version = 1),
    CONSTRAINT usage_event_outbox_kind_source_valid CHECK (
        (kind = 'usage.observed' AND source = 'provider')
        OR (kind = 'usage.estimated' AND source = 'estimated')
    ),
    CONSTRAINT usage_event_outbox_token_type_valid CHECK (
        token_type IN (
            'input', 'output', 'cache_read', 'cache_write', 'reasoning',
            'audio_input', 'audio_output', 'image_input', 'image_output'
        )
    ),
    CONSTRAINT usage_event_outbox_quantity_valid CHECK (
        quantity BETWEEN 1 AND 9007199254740991
    ),
    CONSTRAINT usage_event_outbox_trace_valid CHECK (trace_id ~ '^[0-9a-f]{32}$'),
    CONSTRAINT usage_event_outbox_span_valid CHECK (span_id ~ '^[0-9a-f]{16}$'),
    CONSTRAINT usage_event_outbox_status_valid CHECK (
        status IN ('pending', 'publishing', 'published')
    ),
    CONSTRAINT usage_event_outbox_publish_attempts_valid CHECK (
        publish_attempts BETWEEN 0 AND 1000000
    ),
    CONSTRAINT usage_event_outbox_last_error_code_valid CHECK (
        last_error_code IS NULL OR last_error_code ~ '^[A-Z][A-Z0-9_]{2,127}$'
    ),
    CONSTRAINT usage_event_outbox_created_by_valid CHECK (
        created_by = btrim(created_by) AND char_length(created_by) BETWEEN 1 AND 200
    ),
    CONSTRAINT usage_event_outbox_time_valid CHECK (
        available_at >= created_at AND updated_at >= created_at
        AND (lease_expires_at IS NULL OR lease_expires_at > updated_at)
        AND (published_at IS NULL OR published_at >= created_at)
    ),
    CONSTRAINT usage_event_outbox_lifecycle_valid CHECK (
        (
            status = 'pending'
            AND lease_id IS NULL AND lease_expires_at IS NULL AND published_at IS NULL
        )
        OR (
            status = 'publishing'
            AND lease_id IS NOT NULL AND lease_expires_at IS NOT NULL AND published_at IS NULL
        )
        OR (
            status = 'published'
            AND lease_id IS NULL AND lease_expires_at IS NULL
            AND published_at IS NOT NULL AND last_error_code IS NULL
        )
    )
);

CREATE FUNCTION app.enforce_usage_event_outbox_update()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
BEGIN
    IF NEW.event_id IS DISTINCT FROM OLD.event_id
       OR NEW.schema_version IS DISTINCT FROM OLD.schema_version
       OR NEW.kind IS DISTINCT FROM OLD.kind
       OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR NEW.request_id IS DISTINCT FROM OLD.request_id
       OR NEW.attempt_id IS DISTINCT FROM OLD.attempt_id
       OR NEW.deployment_id IS DISTINCT FROM OLD.deployment_id
       OR NEW.token_type IS DISTINCT FROM OLD.token_type
       OR NEW.quantity IS DISTINCT FROM OLD.quantity
       OR NEW.source IS DISTINCT FROM OLD.source
       OR NEW.usage_complete IS DISTINCT FROM OLD.usage_complete
       OR NEW.observed_at IS DISTINCT FROM OLD.observed_at
       OR NEW.trace_id IS DISTINCT FROM OLD.trace_id
       OR NEW.span_id IS DISTINCT FROM OLD.span_id
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR NEW.created_by IS DISTINCT FROM OLD.created_by
       OR NEW.updated_at < OLD.updated_at THEN
        RAISE EXCEPTION 'usage event outbox facts are immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'usage_event_outbox_facts_immutable';
    END IF;

    IF OLD.status = 'pending' THEN
        IF NEW.status <> 'publishing'
           OR NEW.publish_attempts <> OLD.publish_attempts
           OR NEW.available_at <> OLD.available_at
           OR NEW.lease_id IS NULL OR NEW.lease_expires_at IS NULL
           OR NEW.published_at IS NOT NULL THEN
            RAISE EXCEPTION 'usage event claim transition is invalid'
                USING ERRCODE = '23514', CONSTRAINT = 'usage_event_outbox_claim_transition';
        END IF;
    ELSIF OLD.status = 'publishing' AND NEW.status = 'pending' THEN
        IF NEW.publish_attempts <> OLD.publish_attempts + 1
           OR NEW.available_at < NEW.updated_at
           OR NEW.lease_id IS NOT NULL OR NEW.lease_expires_at IS NOT NULL
           OR NEW.published_at IS NOT NULL OR NEW.last_error_code IS NULL THEN
            RAISE EXCEPTION 'usage event retry transition is invalid'
                USING ERRCODE = '23514', CONSTRAINT = 'usage_event_outbox_retry_transition';
        END IF;
    ELSIF OLD.status = 'publishing' AND NEW.status = 'published' THEN
        IF NEW.publish_attempts <> OLD.publish_attempts
           OR NEW.available_at <> OLD.available_at
           OR NEW.lease_id IS NOT NULL OR NEW.lease_expires_at IS NOT NULL
           OR NEW.published_at IS NULL OR NEW.last_error_code IS NOT NULL THEN
            RAISE EXCEPTION 'usage event publish transition is invalid'
                USING ERRCODE = '23514', CONSTRAINT = 'usage_event_outbox_publish_transition';
        END IF;
    ELSE
        RAISE EXCEPTION 'usage event outbox transition is invalid'
            USING ERRCODE = '23514', CONSTRAINT = 'usage_event_outbox_transition';
    END IF;
    RETURN NEW;
END;
$function$;

CREATE FUNCTION app.enforce_usage_event_outbox_delete()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
BEGIN
    IF OLD.status <> 'published' THEN
        RAISE EXCEPTION 'only published usage outbox rows may be retained or deleted'
            USING ERRCODE = '23514', CONSTRAINT = 'usage_event_outbox_delete_published_only';
    END IF;
    RETURN OLD;
END;
$function$;

CREATE TRIGGER trg_usage_event_outbox_update
BEFORE UPDATE ON app.usage_event_outbox
FOR EACH ROW EXECUTE FUNCTION app.enforce_usage_event_outbox_update();

CREATE TRIGGER trg_usage_event_outbox_delete
BEFORE DELETE ON app.usage_event_outbox
FOR EACH ROW EXECUTE FUNCTION app.enforce_usage_event_outbox_delete();

CREATE INDEX idx_usage_event_outbox_pending
    ON app.usage_event_outbox (available_at, created_at, event_id)
    WHERE status = 'pending';

CREATE INDEX idx_usage_event_outbox_publishing_lease
    ON app.usage_event_outbox (lease_expires_at, event_id)
    WHERE status = 'publishing';

COMMENT ON TABLE app.usage_event_outbox IS
    'Transactional handoff from terminal RouteAttempt facts to at-least-once usage event publication';
COMMENT ON COLUMN app.usage_event_outbox.event_id IS
    'Global consumer idempotency key preserved across relay retries and uncertain acknowledgements';
COMMENT ON COLUMN app.usage_event_outbox.last_error_code IS
    'Bounded non-sensitive relay failure classification; raw broker errors are never persisted';

COMMIT;
