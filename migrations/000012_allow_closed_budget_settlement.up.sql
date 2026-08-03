BEGIN;

CREATE OR REPLACE FUNCTION app.enforce_budget_account_transition()
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
    IF NEW.status NOT IN ('open', 'closed') THEN
        RAISE EXCEPTION 'budget account lifecycle is invalid' USING ERRCODE = '23514';
    END IF;
    IF OLD.status = 'closed'
       AND (NEW.status <> 'closed' OR NEW.closed_at IS DISTINCT FROM OLD.closed_at) THEN
        RAISE EXCEPTION 'closed budget account cannot reopen or change closure' USING ERRCODE = '23514';
    END IF;
    IF NEW.version <> OLD.version + 1 OR NEW.updated_at < OLD.updated_at THEN
        RAISE EXCEPTION 'budget account version/time transition is invalid' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$function$;

COMMENT ON FUNCTION app.enforce_budget_account_transition() IS
    'Keeps account identity/limits immutable and permits versioned balance reconciliation after closure';

COMMIT;
