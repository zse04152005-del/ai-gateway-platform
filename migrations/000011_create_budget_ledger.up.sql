BEGIN;

ALTER TABLE app.gateway_requests
    ADD CONSTRAINT gateway_requests_tenant_id_unique UNIQUE (tenant_id, id);

CREATE TABLE app.budget_accounts (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL,
    scope_kind text NOT NULL,
    project_id uuid,
    virtual_key_id uuid,
    principal_ref text,
    session_ref text,
    currency text NOT NULL DEFAULT 'USD',
    period_start timestamptz NOT NULL,
    period_end timestamptz NOT NULL,
    soft_limit_micros bigint NOT NULL,
    hard_limit_micros bigint NOT NULL,
    committed_amount_micros bigint NOT NULL DEFAULT 0,
    reserved_amount_micros bigint NOT NULL DEFAULT 0,
    status text NOT NULL DEFAULT 'open',
    version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_by text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_by text NOT NULL,
    closed_at timestamptz,
    CONSTRAINT budget_accounts_tenant_fk FOREIGN KEY (tenant_id)
        REFERENCES app.tenants (id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT budget_accounts_project_fk FOREIGN KEY (tenant_id, project_id)
        REFERENCES app.projects (tenant_id, id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT budget_accounts_virtual_key_fk FOREIGN KEY (tenant_id, project_id, virtual_key_id)
        REFERENCES app.virtual_api_keys (tenant_id, project_id, id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT budget_accounts_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT budget_accounts_scope_kind_valid CHECK (
        scope_kind IN ('tenant', 'project', 'key', 'user', 'session')
    ),
    CONSTRAINT budget_accounts_scope_shape_valid CHECK (
        (
            scope_kind = 'tenant'
            AND project_id IS NULL AND virtual_key_id IS NULL
            AND principal_ref IS NULL AND session_ref IS NULL
        )
        OR (
            scope_kind = 'project'
            AND project_id IS NOT NULL AND virtual_key_id IS NULL
            AND principal_ref IS NULL AND session_ref IS NULL
        )
        OR (
            scope_kind = 'key'
            AND project_id IS NOT NULL AND virtual_key_id IS NOT NULL
            AND principal_ref IS NULL AND session_ref IS NULL
        )
        OR (
            scope_kind = 'user'
            AND project_id IS NULL AND virtual_key_id IS NULL
            AND principal_ref IS NOT NULL AND session_ref IS NULL
        )
        OR (
            scope_kind = 'session'
            AND project_id IS NULL AND virtual_key_id IS NULL
            AND principal_ref IS NULL AND session_ref IS NOT NULL
        )
    ),
    CONSTRAINT budget_accounts_principal_ref_format CHECK (
        principal_ref IS NULL
        OR (
            char_length(principal_ref) BETWEEN 1 AND 128
            AND principal_ref ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]*$'
        )
    ),
    CONSTRAINT budget_accounts_session_ref_format CHECK (
        session_ref IS NULL
        OR (
            char_length(session_ref) BETWEEN 1 AND 128
            AND session_ref ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]*$'
        )
    ),
    CONSTRAINT budget_accounts_currency_format CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT budget_accounts_period_valid CHECK (period_end > period_start),
    CONSTRAINT budget_accounts_limits_valid CHECK (
        soft_limit_micros BETWEEN 1 AND 9007199254740991
        AND hard_limit_micros BETWEEN 1 AND 9007199254740991
        AND soft_limit_micros <= hard_limit_micros
    ),
    CONSTRAINT budget_accounts_balances_valid CHECK (
        committed_amount_micros BETWEEN 0 AND 9007199254740991
        AND reserved_amount_micros BETWEEN 0 AND 9007199254740991
        AND committed_amount_micros <= 9007199254740991 - reserved_amount_micros
    ),
    CONSTRAINT budget_accounts_status_valid CHECK (status IN ('open', 'closed')),
    CONSTRAINT budget_accounts_version_positive CHECK (version > 0),
    CONSTRAINT budget_accounts_created_by_format CHECK (
        created_by = btrim(created_by) AND char_length(created_by) BETWEEN 1 AND 200
    ),
    CONSTRAINT budget_accounts_updated_by_format CHECK (
        updated_by = btrim(updated_by) AND char_length(updated_by) BETWEEN 1 AND 200
    ),
    CONSTRAINT budget_accounts_update_time_valid CHECK (updated_at >= created_at),
    CONSTRAINT budget_accounts_closed_time_valid CHECK (
        (status = 'closed') = (closed_at IS NOT NULL)
        AND (closed_at IS NULL OR closed_at >= created_at)
    )
);

CREATE UNIQUE INDEX uq_budget_accounts_tenant_period
    ON app.budget_accounts (tenant_id, currency, period_start, period_end)
    WHERE scope_kind = 'tenant';

CREATE UNIQUE INDEX uq_budget_accounts_project_period
    ON app.budget_accounts (tenant_id, project_id, currency, period_start, period_end)
    WHERE scope_kind = 'project';

CREATE UNIQUE INDEX uq_budget_accounts_key_period
    ON app.budget_accounts (tenant_id, project_id, virtual_key_id, currency, period_start, period_end)
    WHERE scope_kind = 'key';

CREATE UNIQUE INDEX uq_budget_accounts_user_period
    ON app.budget_accounts (tenant_id, principal_ref, currency, period_start, period_end)
    WHERE scope_kind = 'user';

CREATE UNIQUE INDEX uq_budget_accounts_session_period
    ON app.budget_accounts (tenant_id, session_ref, currency, period_start, period_end)
    WHERE scope_kind = 'session';

CREATE INDEX idx_budget_accounts_tenant_status_period
    ON app.budget_accounts (tenant_id, status, period_end, id);

CREATE TABLE app.budget_reservations (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL,
    account_id uuid NOT NULL,
    request_id text NOT NULL,
    idempotency_key text NOT NULL,
    status text NOT NULL DEFAULT 'pending',
    reserved_amount_micros bigint NOT NULL,
    actual_amount_micros bigint,
    released_amount_micros bigint,
    overage_amount_micros bigint,
    expires_at timestamptz NOT NULL,
    version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_by text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_by text NOT NULL,
    terminal_at timestamptz,
    CONSTRAINT budget_reservations_account_fk FOREIGN KEY (tenant_id, account_id)
        REFERENCES app.budget_accounts (tenant_id, id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT budget_reservations_request_fk FOREIGN KEY (tenant_id, request_id)
        REFERENCES app.gateway_requests (tenant_id, id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT budget_reservations_tenant_account_id_unique UNIQUE (tenant_id, account_id, id),
    CONSTRAINT budget_reservations_account_idempotency_unique UNIQUE (account_id, idempotency_key),
    CONSTRAINT budget_reservations_idempotency_format CHECK (
        char_length(idempotency_key) BETWEEN 1 AND 128
        AND idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]*$'
    ),
    CONSTRAINT budget_reservations_status_valid CHECK (
        status IN ('pending', 'settled', 'cancelled', 'expired')
    ),
    CONSTRAINT budget_reservations_reserved_amount_valid CHECK (
        reserved_amount_micros BETWEEN 1 AND 9007199254740991
    ),
    CONSTRAINT budget_reservations_terminal_amounts_valid CHECK (
        (
            status = 'pending'
            AND actual_amount_micros IS NULL
            AND released_amount_micros IS NULL
            AND overage_amount_micros IS NULL
            AND terminal_at IS NULL
        )
        OR (
            status IN ('settled', 'cancelled', 'expired')
            AND actual_amount_micros BETWEEN 0 AND 9007199254740991
            AND released_amount_micros BETWEEN 0 AND 9007199254740991
            AND overage_amount_micros BETWEEN 0 AND 9007199254740991
            AND released_amount_micros = GREATEST(reserved_amount_micros - actual_amount_micros, 0)
            AND overage_amount_micros = GREATEST(actual_amount_micros - reserved_amount_micros, 0)
            AND terminal_at IS NOT NULL
            AND terminal_at >= created_at
        )
    ),
    CONSTRAINT budget_reservations_expiry_valid CHECK (expires_at > created_at),
    CONSTRAINT budget_reservations_version_positive CHECK (version > 0),
    CONSTRAINT budget_reservations_created_by_format CHECK (
        created_by = btrim(created_by) AND char_length(created_by) BETWEEN 1 AND 200
    ),
    CONSTRAINT budget_reservations_updated_by_format CHECK (
        updated_by = btrim(updated_by) AND char_length(updated_by) BETWEEN 1 AND 200
    ),
    CONSTRAINT budget_reservations_update_time_valid CHECK (updated_at >= created_at)
);

CREATE INDEX idx_budget_reservations_pending_expiry
    ON app.budget_reservations (expires_at, id)
    WHERE status = 'pending';

CREATE INDEX idx_budget_reservations_request
    ON app.budget_reservations (tenant_id, request_id, account_id, id);

CREATE TABLE app.budget_ledger_entries (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id uuid NOT NULL,
    account_id uuid NOT NULL,
    reservation_id uuid,
    entry_kind text NOT NULL,
    idempotency_key text NOT NULL,
    committed_delta_micros bigint NOT NULL,
    reserved_delta_micros bigint NOT NULL,
    result_committed_micros bigint NOT NULL,
    result_reserved_micros bigint NOT NULL,
    occurred_at timestamptz NOT NULL,
    created_by text NOT NULL,
    CONSTRAINT budget_ledger_entries_account_fk FOREIGN KEY (tenant_id, account_id)
        REFERENCES app.budget_accounts (tenant_id, id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT budget_ledger_entries_reservation_fk FOREIGN KEY (tenant_id, account_id, reservation_id)
        REFERENCES app.budget_reservations (tenant_id, account_id, id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT budget_ledger_entries_account_idempotency_unique UNIQUE (account_id, idempotency_key),
    CONSTRAINT budget_ledger_entries_kind_valid CHECK (
        entry_kind IN ('reserve', 'settle', 'release', 'expire', 'adjustment')
    ),
    CONSTRAINT budget_ledger_entries_idempotency_format CHECK (
        char_length(idempotency_key) BETWEEN 1 AND 128
        AND idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]*$'
    ),
    CONSTRAINT budget_ledger_entries_deltas_valid CHECK (
        committed_delta_micros BETWEEN -9007199254740991 AND 9007199254740991
        AND reserved_delta_micros BETWEEN -9007199254740991 AND 9007199254740991
        AND (committed_delta_micros <> 0 OR reserved_delta_micros <> 0)
    ),
    CONSTRAINT budget_ledger_entries_shape_valid CHECK (
        (
            entry_kind = 'reserve' AND reservation_id IS NOT NULL
            AND committed_delta_micros = 0 AND reserved_delta_micros > 0
        )
        OR (
            entry_kind = 'settle' AND reservation_id IS NOT NULL
            AND committed_delta_micros >= 0 AND reserved_delta_micros < 0
        )
        OR (
            entry_kind IN ('release', 'expire') AND reservation_id IS NOT NULL
            AND committed_delta_micros = 0 AND reserved_delta_micros < 0
        )
        OR (entry_kind = 'adjustment' AND reservation_id IS NULL)
    ),
    CONSTRAINT budget_ledger_entries_results_valid CHECK (
        result_committed_micros BETWEEN 0 AND 9007199254740991
        AND result_reserved_micros BETWEEN 0 AND 9007199254740991
        AND result_committed_micros <= 9007199254740991 - result_reserved_micros
    ),
    CONSTRAINT budget_ledger_entries_created_by_format CHECK (
        created_by = btrim(created_by) AND char_length(created_by) BETWEEN 1 AND 200
    )
);

CREATE INDEX idx_budget_ledger_entries_account_sequence
    ON app.budget_ledger_entries (tenant_id, account_id, id);

CREATE INDEX idx_budget_ledger_entries_reservation
    ON app.budget_ledger_entries (reservation_id, id)
    WHERE reservation_id IS NOT NULL;

CREATE FUNCTION app.enforce_budget_account_transition()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
BEGIN
    IF NEW.id <> OLD.id
       OR NEW.tenant_id <> OLD.tenant_id
       OR NEW.scope_kind <> OLD.scope_kind
       OR NEW.project_id IS DISTINCT FROM OLD.project_id
       OR NEW.virtual_key_id IS DISTINCT FROM OLD.virtual_key_id
       OR NEW.principal_ref IS DISTINCT FROM OLD.principal_ref
       OR NEW.session_ref IS DISTINCT FROM OLD.session_ref
       OR NEW.currency <> OLD.currency
       OR NEW.period_start <> OLD.period_start
       OR NEW.period_end <> OLD.period_end
       OR NEW.soft_limit_micros <> OLD.soft_limit_micros
       OR NEW.hard_limit_micros <> OLD.hard_limit_micros
       OR NEW.created_at <> OLD.created_at
       OR NEW.created_by <> OLD.created_by THEN
        RAISE EXCEPTION 'budget account identity and limits are immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.status = 'closed' OR NEW.status NOT IN ('open', 'closed') THEN
        RAISE EXCEPTION 'budget account lifecycle is terminal' USING ERRCODE = '23514';
    END IF;
    IF NEW.version <> OLD.version + 1 OR NEW.updated_at < OLD.updated_at THEN
        RAISE EXCEPTION 'budget account version/time transition is invalid' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$function$;

CREATE TRIGGER trg_budget_accounts_transition
BEFORE UPDATE ON app.budget_accounts
FOR EACH ROW EXECUTE FUNCTION app.enforce_budget_account_transition();

CREATE FUNCTION app.enforce_budget_reservation_transition()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
BEGIN
    IF NEW.id <> OLD.id
       OR NEW.tenant_id <> OLD.tenant_id
       OR NEW.account_id <> OLD.account_id
       OR NEW.request_id <> OLD.request_id
       OR NEW.idempotency_key <> OLD.idempotency_key
       OR NEW.reserved_amount_micros <> OLD.reserved_amount_micros
       OR NEW.expires_at <> OLD.expires_at
       OR NEW.created_at <> OLD.created_at
       OR NEW.created_by <> OLD.created_by THEN
        RAISE EXCEPTION 'budget reservation identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.status <> 'pending' OR NEW.status NOT IN ('settled', 'cancelled', 'expired') THEN
        RAISE EXCEPTION 'budget reservation lifecycle is terminal' USING ERRCODE = '23514';
    END IF;
    IF NEW.version <> OLD.version + 1 OR NEW.updated_at < OLD.updated_at THEN
        RAISE EXCEPTION 'budget reservation version/time transition is invalid' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$function$;

CREATE TRIGGER trg_budget_reservations_transition
BEFORE UPDATE ON app.budget_reservations
FOR EACH ROW EXECUTE FUNCTION app.enforce_budget_reservation_transition();

CREATE FUNCTION app.reject_budget_ledger_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $function$
BEGIN
    RAISE EXCEPTION 'budget ledger entries are append-only' USING ERRCODE = '23514';
END;
$function$;

CREATE TRIGGER trg_budget_ledger_entries_append_only
BEFORE UPDATE OR DELETE ON app.budget_ledger_entries
FOR EACH ROW EXECUTE FUNCTION app.reject_budget_ledger_mutation();

COMMENT ON TABLE app.budget_accounts IS
    'Tenant-owned exact-micros budget account for one independent scope and half-open period';
COMMENT ON COLUMN app.budget_accounts.committed_amount_micros IS
    'Settled spend; may exceed hard after actual overage so future admission stops without losing usage';
COMMENT ON COLUMN app.budget_accounts.reserved_amount_micros IS
    'Pending holds included with committed spend by atomic budget admission';
COMMENT ON TABLE app.budget_reservations IS
    'Idempotent request/account hold with pending to terminal reconciliation lifecycle';
COMMENT ON TABLE app.budget_ledger_entries IS
    'Append-only signed committed/reserved deltas and resulting exact balances';

COMMIT;
