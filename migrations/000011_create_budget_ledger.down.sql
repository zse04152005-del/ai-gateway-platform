BEGIN;

DROP TRIGGER IF EXISTS trg_budget_ledger_entries_append_only ON app.budget_ledger_entries;
DROP FUNCTION IF EXISTS app.reject_budget_ledger_mutation();

DROP TRIGGER IF EXISTS trg_budget_reservations_transition ON app.budget_reservations;
DROP FUNCTION IF EXISTS app.enforce_budget_reservation_transition();

DROP TRIGGER IF EXISTS trg_budget_accounts_transition ON app.budget_accounts;
DROP FUNCTION IF EXISTS app.enforce_budget_account_transition();

DROP TABLE IF EXISTS app.budget_ledger_entries;
DROP TABLE IF EXISTS app.budget_reservations;
DROP TABLE IF EXISTS app.budget_accounts;

ALTER TABLE app.gateway_requests
    DROP CONSTRAINT IF EXISTS gateway_requests_tenant_id_unique;

COMMIT;
