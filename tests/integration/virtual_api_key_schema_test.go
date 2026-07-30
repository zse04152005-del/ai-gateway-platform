//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/lib/pq"
)

const (
	virtualKeyTenantOneID  = "11000000-0000-4000-8000-000000000001"
	virtualKeyTenantTwoID  = "11000000-0000-4000-8000-000000000002"
	virtualKeyProjectOneID = "21000000-0000-4000-8000-000000000001"
	virtualKeyProjectTwoID = "21000000-0000-4000-8000-000000000002"
	virtualKeyOneID        = "31000000-0000-4000-8000-000000000001"
	virtualKeyTwoID        = "31000000-0000-4000-8000-000000000002"
	virtualKeyThreeID      = "31000000-0000-4000-8000-000000000003"
	virtualKeyFourID       = "31000000-0000-4000-8000-000000000004"
)

func TestVirtualAPIKeySchemaConstraints(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		t.Fatalf("database.PingContext() error = %v", err)
	}
	cleanupVirtualAPIKeyFixtures(t, database)
	t.Cleanup(func() { cleanupVirtualAPIKeyFixtures(t, database) })

	insertTenant(ctx, t, database, virtualKeyTenantOneID, "key-tenant-one", "Key Tenant One", "")
	insertTenant(ctx, t, database, virtualKeyTenantTwoID, "key-tenant-two", "Key Tenant Two", "")
	insertProject(ctx, t, database, virtualKeyProjectOneID, virtualKeyTenantOneID, "key-project-one", "Key Project One", "")
	insertProject(ctx, t, database, virtualKeyProjectTwoID, virtualKeyTenantTwoID, "key-project-two", "Key Project Two", "")

	assertVirtualAPIKeyHasNoPlaintextColumn(ctx, t, database)

	allowedModels := []string{"chat.default", "embed/default-v1"}
	insertVirtualAPIKey(
		ctx,
		t,
		database,
		virtualKeyOneID,
		virtualKeyTenantOneID,
		virtualKeyProjectOneID,
		"agw_live_00000001",
		0x11,
		pq.Array(allowedModels),
		`{"rpm":120,"tpm":100000,"concurrency":8}`,
		102,
	)

	var (
		storedHash       []byte
		storedModels     pq.StringArray
		storedRPM        int64
		storedTPM        int64
		storedConcurrent int64
		storedVersion    int64
	)
	err = database.QueryRowContext(ctx, `
		SELECT secret_hash, allowed_models,
		       (limits ->> 'rpm')::bigint,
		       (limits ->> 'tpm')::bigint,
		       (limits ->> 'concurrency')::bigint,
		       version
		FROM app.virtual_api_keys
		WHERE id = $1 AND tenant_id = $2 AND project_id = $3`,
		virtualKeyOneID, virtualKeyTenantOneID, virtualKeyProjectOneID,
	).Scan(&storedHash, &storedModels, &storedRPM, &storedTPM, &storedConcurrent, &storedVersion)
	if err != nil {
		t.Fatalf("query valid virtual API key: %v", err)
	}
	if len(storedHash) != 32 || !bytes.Equal(storedHash, bytes.Repeat([]byte{0x11}, 32)) {
		t.Fatalf("stored digest length/content = %d/%x", len(storedHash), storedHash)
	}
	if len(storedModels) != 2 || storedModels[0] != allowedModels[0] || storedModels[1] != allowedModels[1] {
		t.Fatalf("stored allowed models = %#v", storedModels)
	}
	if storedRPM != 120 || storedTPM != 100000 || storedConcurrent != 8 || storedVersion != 1 {
		t.Fatalf(
			"stored limits/version = rpm:%d tpm:%d concurrency:%d version:%d",
			storedRPM,
			storedTPM,
			storedConcurrent,
			storedVersion,
		)
	}

	_, err = database.ExecContext(ctx, insertVirtualIdentityStatement,
		virtualKeyTwoID, virtualKeyTenantOneID, virtualKeyProjectOneID,
		"agw_live_00000002", bytes.Repeat([]byte{0x22}, 31), "hmac-v1", nil, nil, 103,
	)
	expectConstraint(t, err, "virtual_api_keys_secret_hash_length")

	_, err = database.ExecContext(ctx, insertVirtualIdentityStatement,
		virtualKeyTwoID, virtualKeyTenantOneID, virtualKeyProjectTwoID,
		"agw_live_00000003", bytes.Repeat([]byte{0x23}, 32), "hmac-v1", nil, nil, 104,
	)
	expectConstraint(t, err, "virtual_api_keys_project_fk")

	_, err = database.ExecContext(ctx, insertVirtualIdentityStatement,
		virtualKeyTwoID, virtualKeyTenantOneID, virtualKeyProjectOneID,
		"agw_live_00000001", bytes.Repeat([]byte{0x24}, 32), "hmac-v1", nil, nil, 105,
	)
	expectConstraint(t, err, "virtual_api_keys_prefix_unique")

	_, err = database.ExecContext(ctx, insertVirtualIdentityStatement,
		virtualKeyTwoID, virtualKeyTenantOneID, virtualKeyProjectOneID,
		"agw_live_00000004", bytes.Repeat([]byte{0x11}, 32), "hmac-v1", nil, nil, 106,
	)
	expectConstraint(t, err, "virtual_api_keys_hash_unique")

	_, err = database.ExecContext(ctx, insertVirtualIdentityStatement,
		virtualKeyTwoID, virtualKeyTenantOneID, virtualKeyProjectOneID,
		"vk-unsafe-prefix", bytes.Repeat([]byte{0x25}, 32), "hmac-v1", nil, nil, 107,
	)
	expectConstraint(t, err, "virtual_api_keys_prefix_format")

	_, err = database.ExecContext(ctx, insertVirtualIdentityStatement,
		virtualKeyTwoID, virtualKeyTenantOneID, virtualKeyProjectOneID,
		"agw_test_00000005", bytes.Repeat([]byte{0x26}, 32), "hmac-v1",
		pq.Array([]string{"chat.default", "CHAT.DEFAULT"}), nil, 108,
	)
	expectConstraint(t, err, "virtual_api_keys_allowed_models_valid")

	_, err = database.ExecContext(ctx, insertVirtualIdentityStatement,
		virtualKeyTwoID, virtualKeyTenantOneID, virtualKeyProjectOneID,
		"agw_test_00000006", bytes.Repeat([]byte{0x27}, 32), "hmac-v1",
		pq.Array([]string{"chat model with spaces"}), nil, 109,
	)
	expectConstraint(t, err, "virtual_api_keys_allowed_models_valid")

	_, err = database.ExecContext(ctx, insertVirtualIdentityStatement,
		virtualKeyTwoID, virtualKeyTenantOneID, virtualKeyProjectOneID,
		"agw_test_00000007", bytes.Repeat([]byte{0x28}, 32), "hmac-v1", nil,
		`{"rpm":1,"burst":2}`, 110,
	)
	expectConstraint(t, err, "virtual_api_keys_limits_valid")

	_, err = database.ExecContext(ctx, insertVirtualIdentityStatement,
		virtualKeyTwoID, virtualKeyTenantOneID, virtualKeyProjectOneID,
		"agw_test_00000008", bytes.Repeat([]byte{0x29}, 32), "hmac-v1", nil,
		`{"concurrency":0}`, 111,
	)
	expectConstraint(t, err, "virtual_api_keys_limits_valid")

	_, err = database.ExecContext(ctx, insertVirtualIdentityStatement,
		virtualKeyTwoID, virtualKeyTenantOneID, virtualKeyProjectOneID,
		"agw_test_00000009", bytes.Repeat([]byte{0x2a}, 32), "hmac-v1", nil,
		`{"tpm":1.5}`, 112,
	)
	expectConstraint(t, err, "virtual_api_keys_limits_valid")

	_, err = database.ExecContext(ctx, `
		INSERT INTO app.virtual_api_keys (
			id, tenant_id, project_id, key_prefix, secret_hash, hash_key_version,
			status, created_by, updated_by
		) VALUES ($1, $2, $3, 'agw_test_00000010', $4, 'hmac-v1', 'rotating', 'integration:test', 'integration:test')`,
		virtualKeyTwoID, virtualKeyTenantOneID, virtualKeyProjectOneID, bytes.Repeat([]byte{0x2b}, 32),
	)
	expectConstraint(t, err, "virtual_api_keys_lifecycle_valid")

	_, err = database.ExecContext(ctx, `
		INSERT INTO app.virtual_api_keys (
			id, tenant_id, project_id, key_prefix, secret_hash, hash_key_version,
			status, created_by, updated_by
		) VALUES ($1, $2, $3, 'agw_test_00000011', $4, 'hmac-v1', 'revoked', 'integration:test', 'integration:test')`,
		virtualKeyTwoID, virtualKeyTenantOneID, virtualKeyProjectOneID, bytes.Repeat([]byte{0x2c}, 32),
	)
	expectConstraint(t, err, "virtual_api_keys_lifecycle_valid")

	_, err = database.ExecContext(ctx, `
		INSERT INTO app.virtual_api_keys (
			id, tenant_id, project_id, key_prefix, secret_hash, hash_key_version,
			expires_at, created_by, updated_by
		) VALUES ($1, $2, $3, 'agw_test_00000012', $4, 'hmac-v1', CURRENT_TIMESTAMP - INTERVAL '1 second', 'integration:test', 'integration:test')`,
		virtualKeyTwoID, virtualKeyTenantOneID, virtualKeyProjectOneID, bytes.Repeat([]byte{0x2d}, 32),
	)
	expectConstraint(t, err, "virtual_api_keys_expiry_time_valid")

	_, err = database.ExecContext(ctx, `
		INSERT INTO app.virtual_api_keys (
			id, tenant_id, project_id, key_prefix, secret_hash, hash_key_version,
			rotated_from_id, created_by, updated_by
		) VALUES ($1, $2, $3, 'agw_live_00000013', $4, 'hmac-v1', $1, 'integration:test', 'integration:test')`,
		virtualKeyTwoID, virtualKeyTenantOneID, virtualKeyProjectOneID, bytes.Repeat([]byte{0x2e}, 32),
	)
	expectConstraint(t, err, "virtual_api_keys_rotation_not_self")

	_, err = database.ExecContext(ctx, `
		UPDATE app.virtual_api_keys
		SET status = 'rotating', rotation_grace_expires_at = CURRENT_TIMESTAMP + INTERVAL '15 minutes',
			version = version + 1, updated_at = CURRENT_TIMESTAMP, updated_by = 'integration:rotate'
		WHERE id = $1`, virtualKeyOneID)
	if err != nil {
		t.Fatalf("mark source key rotating: %v", err)
	}
	insertVirtualAPIKey(
		ctx, t, database, virtualKeyThreeID, virtualKeyTenantTwoID, virtualKeyProjectTwoID,
		"agw_live_00000014", 0x30, nil, nil, 113,
	)
	_, err = database.ExecContext(ctx, `
		UPDATE app.virtual_api_keys
		SET rotated_from_id = $1, version = version + 1, updated_at = CURRENT_TIMESTAMP
		WHERE id = $2`,
		virtualKeyOneID, virtualKeyThreeID,
	)
	expectConstraint(t, err, "virtual_api_keys_rotation_source_fk")

	insertVirtualAPIKey(
		ctx, t, database, virtualKeyTwoID, virtualKeyTenantOneID, virtualKeyProjectOneID,
		"agw_live_00000015", 0x31, nil, nil, 114,
	)
	_, err = database.ExecContext(ctx, `
		UPDATE app.virtual_api_keys
		SET rotated_from_id = $1, version = version + 1, updated_at = CURRENT_TIMESTAMP
		WHERE id = $2`, virtualKeyOneID, virtualKeyTwoID)
	if err != nil {
		t.Fatalf("link replacement key: %v", err)
	}

	insertVirtualAPIKey(
		ctx, t, database, virtualKeyFourID, virtualKeyTenantOneID, virtualKeyProjectOneID,
		"agw_live_00000016", 0x32, nil, nil, 115,
	)
	_, err = database.ExecContext(ctx, `
		UPDATE app.virtual_api_keys SET rotated_from_id = $1 WHERE id = $2`,
		virtualKeyOneID, virtualKeyFourID,
	)
	expectConstraint(t, err, "uq_virtual_api_keys_rotated_from")
}

const insertVirtualIdentityStatement = `
	INSERT INTO app.virtual_api_keys (
		id, tenant_id, project_id, key_prefix, secret_hash, hash_key_version,
		allowed_models, limits, expires_at, created_by, updated_by
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
		CURRENT_TIMESTAMP + make_interval(secs => $9), 'integration:test', 'integration:test')`

func insertVirtualAPIKey(
	ctx context.Context,
	t *testing.T,
	database *sql.DB,
	id string,
	tenantID string,
	projectID string,
	prefix string,
	hashByte byte,
	allowedModels any,
	limits any,
	expiresInSeconds int,
) {
	t.Helper()
	_, err := database.ExecContext(
		ctx,
		insertVirtualIdentityStatement,
		id,
		tenantID,
		projectID,
		prefix,
		bytes.Repeat([]byte{hashByte}, 32),
		"hmac-v1",
		allowedModels,
		limits,
		expiresInSeconds,
	)
	if err != nil {
		t.Fatalf("insert virtual API key %s: %v", prefix, err)
	}
}

func assertVirtualAPIKeyHasNoPlaintextColumn(ctx context.Context, t *testing.T, database *sql.DB) {
	t.Helper()
	rows, err := database.QueryContext(ctx, `
		SELECT column_name
		FROM information_schema.columns
		WHERE table_schema = 'app' AND table_name = 'virtual_api_keys'
		ORDER BY ordinal_position`)
	if err != nil {
		t.Fatalf("query virtual API key columns: %v", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("close virtual API key column rows: %v", err)
		}
	}()

	columns := make(map[string]struct{})
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatalf("scan virtual API key column: %v", err)
		}
		columns[column] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate virtual API key columns: %v", err)
	}
	for _, required := range []string{"key_prefix", "secret_hash", "hash_key_version", "tenant_id", "project_id"} {
		if _, ok := columns[required]; !ok {
			t.Errorf("required column %q is missing", required)
		}
	}
	for _, forbidden := range []string{"secret", "plaintext", "raw_key", "api_key", "ciphertext"} {
		if _, ok := columns[forbidden]; ok {
			t.Errorf("recoverable credential column %q must not exist", forbidden)
		}
	}
}

func cleanupVirtualAPIKeyFixtures(t *testing.T, database *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := database.ExecContext(ctx, `
		DELETE FROM app.virtual_api_keys
		WHERE tenant_id IN ($1, $2)
		   OR id IN ($3, $4, $5, $6)`,
		virtualKeyTenantOneID,
		virtualKeyTenantTwoID,
		virtualKeyOneID,
		virtualKeyTwoID,
		virtualKeyThreeID,
		virtualKeyFourID,
	); err != nil {
		t.Errorf("cleanup virtual API key fixtures: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		DELETE FROM app.projects WHERE id IN ($1, $2)`,
		virtualKeyProjectOneID,
		virtualKeyProjectTwoID,
	); err != nil {
		t.Errorf("cleanup virtual API key projects: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		DELETE FROM app.tenants WHERE id IN ($1, $2)`,
		virtualKeyTenantOneID,
		virtualKeyTenantTwoID,
	); err != nil {
		t.Errorf("cleanup virtual API key tenants: %v", err)
	}
}
