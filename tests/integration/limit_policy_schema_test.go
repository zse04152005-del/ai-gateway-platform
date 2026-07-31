//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"testing"
	"time"
)

const (
	limitPolicyTenantOneID  = "72000000-0000-4000-8000-000000000001"
	limitPolicyTenantTwoID  = "72000000-0000-4000-8000-000000000002"
	limitPolicyProjectOneID = "72000000-0000-4000-8000-000000000011"
	limitPolicyProjectTwoID = "72000000-0000-4000-8000-000000000012"
	limitPolicyKeyOneID     = "72000000-0000-4000-8000-000000000021"
	limitPolicyTenantID     = "72000000-0000-4000-8000-000000000101"
	limitPolicyProjectID    = "72000000-0000-4000-8000-000000000102"
	limitPolicyKeyID        = "72000000-0000-4000-8000-000000000103"
	limitPolicyOtherID      = "72000000-0000-4000-8000-000000000104"
	limitPolicyInvalidID    = "72000000-0000-4000-8000-000000000199"
)

func TestLimitPolicySchemaConstraints(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set")
	}
	database, err := sql.Open("postgres", databaseURL)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	database.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		t.Fatalf("database.PingContext() error = %v", err)
	}
	cleanupLimitPolicyFixtures(t, database)
	t.Cleanup(func() { cleanupLimitPolicyFixtures(t, database) })

	insertTenant(ctx, t, database, limitPolicyTenantOneID, "limit-tenant-one", "Limit Tenant One", "")
	insertTenant(ctx, t, database, limitPolicyTenantTwoID, "limit-tenant-two", "Limit Tenant Two", "")
	insertProject(
		ctx, t, database, limitPolicyProjectOneID, limitPolicyTenantOneID,
		"limit-project-one", "Limit Project One", "",
	)
	insertProject(
		ctx, t, database, limitPolicyProjectTwoID, limitPolicyTenantTwoID,
		"limit-project-two", "Limit Project Two", "",
	)

	insertLimitPolicy(
		ctx, t, database, limitPolicyTenantID, limitPolicyTenantOneID, "tenant/default-v1",
		80, 100, 80_000, 100_000, 8, 10,
	)
	insertLimitPolicy(
		ctx, t, database, limitPolicyProjectID, limitPolicyTenantOneID, "project/chat-v1",
		90, nil, nil, 95_000, nil, nil,
	)
	insertLimitPolicy(
		ctx, t, database, limitPolicyKeyID, limitPolicyTenantOneID, "key/batch-v1",
		nil, 92, nil, nil, 4, 5,
	)
	insertLimitPolicy(
		ctx, t, database, limitPolicyOtherID, limitPolicyTenantTwoID, "tenant/default-v1",
		70, 90, nil, int64(9_007_199_254_740_991), nil, nil,
	)

	_, err = database.ExecContext(ctx, `
		UPDATE app.tenants SET limit_policy_id = $1 WHERE id = $2`,
		limitPolicyTenantID, limitPolicyTenantOneID,
	)
	if err != nil {
		t.Fatalf("bind tenant policy: %v", err)
	}
	_, err = database.ExecContext(ctx, `
		UPDATE app.projects SET limit_policy_id = $1 WHERE id = $2`,
		limitPolicyProjectID, limitPolicyProjectOneID,
	)
	if err != nil {
		t.Fatalf("bind project policy: %v", err)
	}
	_, err = database.ExecContext(ctx, `
		INSERT INTO app.virtual_api_keys (
			id, tenant_id, project_id, key_prefix, secret_hash, hash_key_version,
			limit_policy_id, expires_at, created_by, updated_by
		) VALUES (
			$1, $2, $3, 'agw_test_72000001', $4, 'hmac-v1', $5,
			CURRENT_TIMESTAMP + INTERVAL '1 hour', 'integration:test', 'integration:test'
		)`,
		limitPolicyKeyOneID,
		limitPolicyTenantOneID,
		limitPolicyProjectOneID,
		bytes.Repeat([]byte{0x72}, 32),
		limitPolicyKeyID,
	)
	if err != nil {
		t.Fatalf("insert key with policy: %v", err)
	}

	var (
		storedRPMSoft         sql.NullInt64
		storedRPMHard         sql.NullInt64
		storedTPMSoft         sql.NullInt64
		storedTPMHard         sql.NullInt64
		storedConcurrencySoft sql.NullInt64
		storedConcurrencyHard sql.NullInt64
		tenantPolicyID        sql.NullString
		projectPolicyID       sql.NullString
		keyPolicyID           sql.NullString
	)
	err = database.QueryRowContext(ctx, `
		SELECT lp.rpm_soft, lp.rpm_hard, lp.tpm_soft, lp.tpm_hard,
		       lp.concurrency_soft, lp.concurrency_hard,
		       t.limit_policy_id::text, p.limit_policy_id::text, vk.limit_policy_id::text
		FROM app.limit_policies lp
		JOIN app.tenants t ON t.id = lp.tenant_id
		JOIN app.projects p ON p.tenant_id = t.id
		JOIN app.virtual_api_keys vk ON vk.tenant_id = p.tenant_id AND vk.project_id = p.id
		WHERE lp.id = $1 AND p.id = $2 AND vk.id = $3`,
		limitPolicyProjectID, limitPolicyProjectOneID, limitPolicyKeyOneID,
	).Scan(
		&storedRPMSoft, &storedRPMHard, &storedTPMSoft, &storedTPMHard,
		&storedConcurrencySoft, &storedConcurrencyHard,
		&tenantPolicyID, &projectPolicyID, &keyPolicyID,
	)
	if err != nil {
		t.Fatalf("query sparse policy and bindings: %v", err)
	}
	if storedRPMSoft.Int64 != 90 || !storedRPMSoft.Valid || storedRPMHard.Valid || storedTPMSoft.Valid ||
		storedTPMHard.Int64 != 95_000 || !storedTPMHard.Valid ||
		storedConcurrencySoft.Valid || storedConcurrencyHard.Valid {
		t.Fatalf(
			"sparse policy = rpm:%v/%v tpm:%v/%v concurrency:%v/%v",
			storedRPMSoft, storedRPMHard, storedTPMSoft, storedTPMHard,
			storedConcurrencySoft, storedConcurrencyHard,
		)
	}
	if tenantPolicyID.String != limitPolicyTenantID || projectPolicyID.String != limitPolicyProjectID ||
		keyPolicyID.String != limitPolicyKeyID {
		t.Fatalf(
			"bound policies = tenant:%q project:%q key:%q",
			tenantPolicyID.String, projectPolicyID.String, keyPolicyID.String,
		)
	}

	invalidPolicies := []struct {
		name       string
		reference  string
		values     [6]any
		constraint string
	}{
		{name: "all inherited", reference: "invalid-all", constraint: "limit_policies_has_override"},
		{name: "zero", reference: "invalid-zero", values: [6]any{int64(0)}, constraint: "limit_policies_values_valid"},
		{name: "negative", reference: "invalid-negative", values: [6]any{nil, int64(-1)}, constraint: "limit_policies_values_valid"},
		{
			name: "above exact integer maximum", reference: "invalid-maximum",
			values:     [6]any{nil, nil, int64(9_007_199_254_740_992)},
			constraint: "limit_policies_values_valid",
		},
		{
			name: "local soft above hard", reference: "invalid-pair",
			values:     [6]any{int64(101), int64(100)},
			constraint: "limit_policies_local_pairs_valid",
		},
	}
	for _, test := range invalidPolicies {
		t.Run(test.name, func(t *testing.T) {
			_, insertErr := database.ExecContext(ctx, insertLimitPolicyStatement,
				limitPolicyInvalidID,
				limitPolicyTenantOneID,
				test.reference,
				test.values[0], test.values[1], test.values[2],
				test.values[3], test.values[4], test.values[5],
			)
			expectConstraint(t, insertErr, test.constraint)
		})
	}

	_, err = database.ExecContext(ctx, insertLimitPolicyStatement,
		limitPolicyInvalidID, limitPolicyTenantOneID, "tenant/default-v1",
		1, nil, nil, nil, nil, nil,
	)
	expectConstraint(t, err, "limit_policies_tenant_ref_unique")

	_, err = database.ExecContext(ctx, insertLimitPolicyStatement,
		limitPolicyInvalidID, limitPolicyTenantOneID, " invalid ref",
		1, nil, nil, nil, nil, nil,
	)
	expectConstraint(t, err, "limit_policies_ref_format")

	_, err = database.ExecContext(ctx, `
		UPDATE app.tenants SET limit_policy_id = $1 WHERE id = $2`,
		limitPolicyOtherID, limitPolicyTenantOneID,
	)
	expectConstraint(t, err, "tenants_limit_policy_fk")

	_, err = database.ExecContext(ctx, `
		UPDATE app.projects SET limit_policy_id = $1 WHERE id = $2`,
		limitPolicyOtherID, limitPolicyProjectOneID,
	)
	expectConstraint(t, err, "projects_limit_policy_fk")

	_, err = database.ExecContext(ctx, `
		UPDATE app.virtual_api_keys SET limit_policy_id = $1 WHERE id = $2`,
		limitPolicyOtherID, limitPolicyKeyOneID,
	)
	expectConstraint(t, err, "virtual_api_keys_limit_policy_fk")

	_, err = database.ExecContext(ctx, `
		UPDATE app.tenants SET quota_policy_ref = 'legacy-policy' WHERE id = $1`,
		limitPolicyTenantOneID,
	)
	expectConstraint(t, err, "tenants_limit_policy_source_valid")

	_, err = database.ExecContext(ctx, `
		UPDATE app.projects SET quota_policy_ref = 'legacy-policy' WHERE id = $1`,
		limitPolicyProjectOneID,
	)
	expectConstraint(t, err, "projects_limit_policy_source_valid")

	_, err = database.ExecContext(ctx, `
		UPDATE app.virtual_api_keys SET limits = '{"rpm": 1}'::jsonb WHERE id = $1`,
		limitPolicyKeyOneID,
	)
	expectConstraint(t, err, "virtual_api_keys_limit_policy_source_valid")

	_, err = database.ExecContext(ctx, `DELETE FROM app.limit_policies WHERE id = $1`, limitPolicyTenantID)
	expectConstraint(t, err, "tenants_limit_policy_fk")

	_, err = database.ExecContext(ctx, `
		UPDATE app.limit_policies
		SET status = 'disabled', disabled_at = NULL
		WHERE id = $1`, limitPolicyProjectID)
	expectConstraint(t, err, "limit_policies_disabled_time_valid")
}

const insertLimitPolicyStatement = `
	INSERT INTO app.limit_policies (
		id, tenant_id, policy_ref,
		rpm_soft, rpm_hard, tpm_soft, tpm_hard, concurrency_soft, concurrency_hard,
		created_by, updated_by
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'integration:test', 'integration:test')`

func insertLimitPolicy(
	ctx context.Context,
	t *testing.T,
	database *sql.DB,
	id string,
	tenantID string,
	reference string,
	rpmSoft any,
	rpmHard any,
	tpmSoft any,
	tpmHard any,
	concurrencySoft any,
	concurrencyHard any,
) {
	t.Helper()
	_, err := database.ExecContext(
		ctx,
		insertLimitPolicyStatement,
		id,
		tenantID,
		reference,
		rpmSoft,
		rpmHard,
		tpmSoft,
		tpmHard,
		concurrencySoft,
		concurrencyHard,
	)
	if err != nil {
		t.Fatalf("insert limit policy %s: %v", reference, err)
	}
}

func cleanupLimitPolicyFixtures(t *testing.T, database *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := database.ExecContext(ctx, `
		DELETE FROM app.virtual_api_keys
		WHERE id = $1 OR tenant_id IN ($2, $3)`,
		limitPolicyKeyOneID, limitPolicyTenantOneID, limitPolicyTenantTwoID,
	); err != nil {
		t.Errorf("cleanup limit policy keys: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		UPDATE app.projects SET limit_policy_id = NULL
		WHERE tenant_id IN ($1, $2)`, limitPolicyTenantOneID, limitPolicyTenantTwoID,
	); err != nil {
		t.Errorf("clear project limit policies: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		UPDATE app.tenants SET limit_policy_id = NULL
		WHERE id IN ($1, $2)`, limitPolicyTenantOneID, limitPolicyTenantTwoID,
	); err != nil {
		t.Errorf("clear tenant limit policies: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		DELETE FROM app.projects
		WHERE id IN ($1, $2) OR tenant_id IN ($3, $4)`,
		limitPolicyProjectOneID, limitPolicyProjectTwoID,
		limitPolicyTenantOneID, limitPolicyTenantTwoID,
	); err != nil {
		t.Errorf("cleanup limit policy projects: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		DELETE FROM app.limit_policies
		WHERE tenant_id IN ($1, $2)
		   OR id IN ($3, $4, $5, $6, $7)`,
		limitPolicyTenantOneID, limitPolicyTenantTwoID,
		limitPolicyTenantID, limitPolicyProjectID, limitPolicyKeyID,
		limitPolicyOtherID, limitPolicyInvalidID,
	); err != nil {
		t.Errorf("cleanup limit policies: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		DELETE FROM app.tenants WHERE id IN ($1, $2)`,
		limitPolicyTenantOneID, limitPolicyTenantTwoID,
	); err != nil {
		t.Errorf("cleanup limit policy tenants: %v", err)
	}
}
