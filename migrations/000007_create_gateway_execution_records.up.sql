BEGIN;

CREATE TABLE app.gateway_requests (
    id text PRIMARY KEY,
    tenant_id uuid NOT NULL,
    project_id uuid NOT NULL,
    virtual_key_id uuid NOT NULL,
    logical_model text NOT NULL,
    trace_id text NOT NULL,
    span_id text NOT NULL,
    status text NOT NULL DEFAULT 'authorized',
    attempt_count integer NOT NULL DEFAULT 0,
    started_at timestamptz NOT NULL,
    ended_at timestamptz,
    end_reason text,
    version bigint NOT NULL DEFAULT 1,
    updated_at timestamptz NOT NULL,
    CONSTRAINT gateway_requests_tenant_fk FOREIGN KEY (tenant_id)
        REFERENCES app.tenants (id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT gateway_requests_project_fk FOREIGN KEY (tenant_id, project_id)
        REFERENCES app.projects (tenant_id, id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT gateway_requests_virtual_key_fk FOREIGN KEY (tenant_id, project_id, virtual_key_id)
        REFERENCES app.virtual_api_keys (tenant_id, project_id, id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT gateway_requests_logical_model_fk FOREIGN KEY (tenant_id, logical_model)
        REFERENCES app.logical_models (tenant_id, name)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT gateway_requests_id_format CHECK (
        char_length(id) BETWEEN 8 AND 128
        AND id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]*$'
    ),
    CONSTRAINT gateway_requests_trace_id_format CHECK (trace_id ~ '^[0-9a-f]{32}$'),
    CONSTRAINT gateway_requests_span_id_format CHECK (span_id ~ '^[0-9a-f]{16}$'),
    CONSTRAINT gateway_requests_status_valid CHECK (
        status IN (
            'received', 'authorized', 'reserved', 'routing', 'running',
            'succeeded', 'partial_failed', 'failed', 'cancelled'
        )
    ),
    CONSTRAINT gateway_requests_attempt_count_valid CHECK (attempt_count >= 0),
    CONSTRAINT gateway_requests_end_reason_format CHECK (
        end_reason IS NULL
        OR (
            char_length(end_reason) BETWEEN 1 AND 64
            AND end_reason ~ '^[a-z][a-z0-9_]*$'
        )
    ),
    CONSTRAINT gateway_requests_lifecycle_valid CHECK (
        (
            status IN ('received', 'authorized', 'reserved', 'routing', 'running')
            AND ended_at IS NULL
            AND end_reason IS NULL
        )
        OR (
            status IN ('succeeded', 'partial_failed', 'failed', 'cancelled')
            AND ended_at IS NOT NULL
            AND ended_at >= started_at
            AND end_reason IS NOT NULL
        )
    ),
    CONSTRAINT gateway_requests_version_positive CHECK (version > 0),
    CONSTRAINT gateway_requests_update_time_valid CHECK (updated_at >= started_at)
);

CREATE TABLE app.route_attempts (
    id uuid PRIMARY KEY,
    request_id text NOT NULL,
    attempt_no integer NOT NULL,
    deployment_id uuid NOT NULL,
    status text NOT NULL DEFAULT 'created',
    started_at timestamptz NOT NULL,
    headers_received_at timestamptz,
    first_byte_at timestamptz,
    ended_at timestamptz,
    end_reason text,
    provider_request_id text,
    error_category text,
    error_code text,
    usage_summary jsonb,
    version bigint NOT NULL DEFAULT 1,
    updated_at timestamptz NOT NULL,
    CONSTRAINT route_attempts_request_fk FOREIGN KEY (request_id)
        REFERENCES app.gateway_requests (id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT route_attempts_deployment_fk FOREIGN KEY (deployment_id)
        REFERENCES app.deployments (id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT route_attempts_request_number_unique UNIQUE (request_id, attempt_no),
    CONSTRAINT route_attempts_number_positive CHECK (attempt_no > 0),
    CONSTRAINT route_attempts_status_valid CHECK (
        status IN (
            'created', 'connecting', 'headers_received', 'streaming',
            'succeeded', 'retryable_failed', 'failed', 'partial_failed', 'cancelled'
        )
    ),
    CONSTRAINT route_attempts_end_reason_format CHECK (
        end_reason IS NULL
        OR (
            char_length(end_reason) BETWEEN 1 AND 64
            AND end_reason ~ '^[a-z][a-z0-9_]*$'
        )
    ),
    CONSTRAINT route_attempts_provider_request_id_format CHECK (
        provider_request_id IS NULL
        OR (
            char_length(provider_request_id) BETWEEN 1 AND 256
            AND provider_request_id ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]*$'
        )
    ),
    CONSTRAINT route_attempts_error_category_format CHECK (
        error_category IS NULL
        OR (
            char_length(error_category) BETWEEN 1 AND 64
            AND error_category ~ '^[a-z][a-z0-9_]*$'
        )
    ),
    CONSTRAINT route_attempts_error_code_format CHECK (
        error_code IS NULL
        OR (
            char_length(error_code) BETWEEN 3 AND 128
            AND error_code ~ '^[A-Z][A-Z0-9_]*$'
        )
    ),
    CONSTRAINT route_attempts_usage_summary_valid CHECK (
        usage_summary IS NULL
        OR (
            jsonb_typeof(usage_summary) = 'object'
            AND octet_length(usage_summary::text) <= 16384
        )
    ),
    CONSTRAINT route_attempts_timestamps_valid CHECK (
        updated_at >= started_at
        AND (headers_received_at IS NULL OR headers_received_at >= started_at)
        AND (first_byte_at IS NULL OR first_byte_at >= started_at)
        AND (ended_at IS NULL OR ended_at >= started_at)
        AND (first_byte_at IS NULL OR headers_received_at IS NOT NULL)
        AND (ended_at IS NULL OR headers_received_at IS NULL OR ended_at >= headers_received_at)
        AND (ended_at IS NULL OR first_byte_at IS NULL OR ended_at >= first_byte_at)
    ),
    CONSTRAINT route_attempts_lifecycle_valid CHECK (
        (
            status IN ('created', 'connecting')
            AND headers_received_at IS NULL
            AND first_byte_at IS NULL
            AND ended_at IS NULL
            AND end_reason IS NULL
            AND provider_request_id IS NULL
            AND error_category IS NULL
            AND error_code IS NULL
            AND usage_summary IS NULL
        )
        OR (
            status IN ('headers_received', 'streaming')
            AND headers_received_at IS NOT NULL
            AND ended_at IS NULL
            AND end_reason IS NULL
            AND error_category IS NULL
            AND error_code IS NULL
            AND usage_summary IS NULL
        )
        OR (
            status = 'succeeded'
            AND headers_received_at IS NOT NULL
            AND ended_at IS NOT NULL
            AND end_reason IS NOT NULL
            AND error_category IS NULL
            AND error_code IS NULL
        )
        OR (
            status IN ('retryable_failed', 'failed', 'partial_failed', 'cancelled')
            AND ended_at IS NOT NULL
            AND end_reason IS NOT NULL
            AND error_category IS NOT NULL
            AND error_code IS NOT NULL
        )
    ),
    CONSTRAINT route_attempts_version_positive CHECK (version > 0)
);

CREATE TABLE app.gateway_request_status_events (
    request_id text NOT NULL,
    request_version bigint NOT NULL,
    from_status text,
    to_status text NOT NULL,
    observed_at timestamptz NOT NULL,
    reason_code text,
    PRIMARY KEY (request_id, request_version),
    CONSTRAINT gateway_request_events_request_fk FOREIGN KEY (request_id)
        REFERENCES app.gateway_requests (id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT
);

CREATE TABLE app.route_attempt_status_events (
    attempt_id uuid NOT NULL,
    attempt_version bigint NOT NULL,
    from_status text,
    to_status text NOT NULL,
    observed_at timestamptz NOT NULL,
    reason_code text,
    PRIMARY KEY (attempt_id, attempt_version),
    CONSTRAINT route_attempt_events_attempt_fk FOREIGN KEY (attempt_id)
        REFERENCES app.route_attempts (id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT
);

CREATE FUNCTION app.enforce_gateway_request_transition()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
BEGIN
    IF NEW.id <> OLD.id
       OR NEW.tenant_id <> OLD.tenant_id
       OR NEW.project_id <> OLD.project_id
       OR NEW.virtual_key_id <> OLD.virtual_key_id
       OR NEW.logical_model <> OLD.logical_model
       OR NEW.trace_id <> OLD.trace_id
       OR NEW.span_id <> OLD.span_id
       OR NEW.started_at <> OLD.started_at THEN
        RAISE EXCEPTION 'gateway request identity is immutable' USING ERRCODE = '23514';
    END IF;

    IF NEW.version <> OLD.version + 1 OR NEW.updated_at < OLD.updated_at THEN
        RAISE EXCEPTION 'gateway request version/time transition is invalid' USING ERRCODE = '23514';
    END IF;

    IF NEW.attempt_count < OLD.attempt_count OR NEW.attempt_count > OLD.attempt_count + 1 THEN
        RAISE EXCEPTION 'gateway request attempt count transition is invalid' USING ERRCODE = '23514';
    END IF;
    IF NEW.attempt_count > OLD.attempt_count AND NEW.status <> 'running' THEN
        RAISE EXCEPTION 'gateway request attempt count may increase only while running' USING ERRCODE = '23514';
    END IF;

    IF NOT (
        (OLD.status = 'received' AND NEW.status IN ('authorized', 'failed', 'cancelled'))
        OR (OLD.status = 'authorized' AND NEW.status IN ('reserved', 'routing', 'failed', 'cancelled'))
        OR (OLD.status = 'reserved' AND NEW.status IN ('routing', 'failed', 'cancelled'))
        OR (OLD.status = 'routing' AND NEW.status IN ('running', 'failed', 'cancelled'))
        OR (OLD.status = 'running' AND NEW.status IN ('running', 'succeeded', 'partial_failed', 'failed', 'cancelled'))
    ) THEN
        RAISE EXCEPTION 'gateway request status transition is invalid' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$function$;

CREATE FUNCTION app.enforce_route_attempt_transition()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
BEGIN
    IF NEW.id <> OLD.id
       OR NEW.request_id <> OLD.request_id
       OR NEW.attempt_no <> OLD.attempt_no
       OR NEW.deployment_id <> OLD.deployment_id
       OR NEW.started_at <> OLD.started_at THEN
        RAISE EXCEPTION 'route attempt identity is immutable' USING ERRCODE = '23514';
    END IF;

    IF NEW.version <> OLD.version + 1 OR NEW.updated_at < OLD.updated_at THEN
        RAISE EXCEPTION 'route attempt version/time transition is invalid' USING ERRCODE = '23514';
    END IF;

    IF NOT (
        (OLD.status = 'created' AND NEW.status IN ('connecting', 'failed', 'cancelled'))
        OR (OLD.status = 'connecting' AND NEW.status IN ('headers_received', 'retryable_failed', 'failed', 'cancelled'))
        OR (OLD.status = 'headers_received' AND NEW.status IN ('streaming', 'succeeded', 'retryable_failed', 'failed', 'cancelled'))
        OR (OLD.status = 'streaming' AND NEW.status IN ('succeeded', 'partial_failed', 'failed', 'cancelled'))
    ) THEN
        RAISE EXCEPTION 'route attempt status transition is invalid' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$function$;

CREATE FUNCTION app.record_gateway_request_status_event()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
BEGIN
    INSERT INTO app.gateway_request_status_events (
        request_id, request_version, from_status, to_status, observed_at, reason_code
    ) VALUES (
        NEW.id,
        NEW.version,
        CASE WHEN TG_OP = 'UPDATE' THEN OLD.status ELSE NULL END,
        NEW.status,
        NEW.updated_at,
        NEW.end_reason
    );
    RETURN NEW;
END;
$function$;

CREATE FUNCTION app.record_route_attempt_status_event()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
BEGIN
    INSERT INTO app.route_attempt_status_events (
        attempt_id, attempt_version, from_status, to_status, observed_at, reason_code
    ) VALUES (
        NEW.id,
        NEW.version,
        CASE WHEN TG_OP = 'UPDATE' THEN OLD.status ELSE NULL END,
        NEW.status,
        NEW.updated_at,
        NEW.end_reason
    );
    RETURN NEW;
END;
$function$;

CREATE TRIGGER trg_gateway_requests_transition
BEFORE UPDATE ON app.gateway_requests
FOR EACH ROW
EXECUTE FUNCTION app.enforce_gateway_request_transition();

CREATE TRIGGER trg_route_attempts_transition
BEFORE UPDATE ON app.route_attempts
FOR EACH ROW
EXECUTE FUNCTION app.enforce_route_attempt_transition();

CREATE TRIGGER trg_gateway_requests_status_event
AFTER INSERT OR UPDATE ON app.gateway_requests
FOR EACH ROW
EXECUTE FUNCTION app.record_gateway_request_status_event();

CREATE TRIGGER trg_route_attempts_status_event
AFTER INSERT OR UPDATE ON app.route_attempts
FOR EACH ROW
EXECUTE FUNCTION app.record_route_attempt_status_event();

CREATE INDEX idx_gateway_requests_scope_started
    ON app.gateway_requests (tenant_id, project_id, started_at DESC, id);

CREATE INDEX idx_gateway_requests_key_started
    ON app.gateway_requests (virtual_key_id, started_at DESC, id);

CREATE INDEX idx_gateway_requests_status_updated
    ON app.gateway_requests (status, updated_at, id);

CREATE INDEX idx_route_attempts_request_status
    ON app.route_attempts (request_id, status, attempt_no);

CREATE INDEX idx_route_attempts_deployment_started
    ON app.route_attempts (deployment_id, started_at DESC, id);

CREATE INDEX idx_route_attempts_status_updated
    ON app.route_attempts (status, updated_at, id);

COMMENT ON TABLE app.gateway_requests IS
    'Tenant-scoped client execution facts; request content and credentials are never stored';
COMMENT ON TABLE app.route_attempts IS
    'One row per physical upstream call; retries and failovers require distinct attempt rows';
COMMENT ON COLUMN app.route_attempts.usage_summary IS
    'Bounded normalized count summary only; raw provider evidence belongs to the immutable usage ledger';
COMMENT ON TABLE app.gateway_request_status_events IS
    'Append-only request status transition evidence keyed by optimistic version';
COMMENT ON TABLE app.route_attempt_status_events IS
    'Append-only attempt status transition evidence keyed by optimistic version';

COMMIT;
