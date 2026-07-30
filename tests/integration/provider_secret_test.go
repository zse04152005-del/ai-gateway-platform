//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/providersecret"
)

const (
	providerSecretProviderOneID   = "46000000-0000-4000-8000-000000000001"
	providerSecretProviderTwoID   = "46000000-0000-4000-8000-000000000002"
	providerSecretDeploymentOneID = "66000000-0000-4000-8000-000000000001"
	providerSecretDeploymentTwoID = "66000000-0000-4000-8000-000000000002"
)

func TestProviderSecretEnvelopeAndReferenceIsolation(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set")
	}
	database, err := sql.Open("postgres", databaseURL)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	database.SetMaxOpenConns(4)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		t.Fatalf("database.PingContext() error = %v", err)
	}
	cleanupProviderSecretFixtures(t, database)
	t.Cleanup(func() { cleanupProviderSecretFixtures(t, database) })
	insertProviderSecretCatalogFixtures(ctx, t, database)

	store, err := providersecret.NewPostgresStore(database)
	if err != nil {
		t.Fatalf("NewPostgresStore() error = %v", err)
	}
	local, err := providersecret.NewLocalCipher(
		"integration-local-v1",
		bytes.Repeat([]byte{0x71}, 32),
		nil,
		rand.Reader,
	)
	if err != nil {
		t.Fatalf("NewLocalCipher() error = %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	manager, err := providersecret.NewManager(store, local, nil, rand.Reader, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	plaintext := []byte("integration-provider-credential")
	metadata, err := manager.CreateLocal(ctx, providersecret.CreateLocalCommand{
		ProviderID: providerSecretProviderOneID, Name: "primary", Plaintext: plaintext,
		Actor: "integration:provider-secret",
	})
	if err != nil {
		t.Fatalf("CreateLocal() error = %v", err)
	}

	var (
		ciphertext []byte
		nonce      []byte
		keyVersion string
		backend    string
		locator    sql.NullString
	)
	err = database.QueryRowContext(ctx, `
		SELECT ciphertext, nonce, key_version, backend, locator
		FROM app.provider_secret_references
		WHERE provider_id = $1 AND id = $2`,
		providerSecretProviderOneID,
		metadata.ID,
	).Scan(&ciphertext, &nonce, &keyVersion, &backend, &locator)
	if err != nil {
		t.Fatalf("query stored provider secret envelope: %v", err)
	}
	if len(ciphertext) <= len(plaintext) || bytes.Contains(ciphertext, plaintext) || len(nonce) != 12 {
		t.Fatalf("stored ciphertext/nonce length = %d/%d or contains plaintext", len(ciphertext), len(nonce))
	}
	if keyVersion != "integration-local-v1" || backend != "local_envelope" || locator.Valid {
		t.Fatalf("stored version/backend/locator = %q/%q/%v", keyVersion, backend, locator)
	}
	resolved, err := manager.Resolve(ctx, providersecret.Locator{ProviderID: providerSecretProviderOneID, ID: metadata.ID})
	if err != nil || !bytes.Equal(resolved, plaintext) {
		t.Fatalf("Resolve() value/error = %q/%v", resolved, err)
	}
	clear(resolved)

	encodedMetadata, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("json.Marshal(metadata) error = %v", err)
	}
	if bytes.Contains(encodedMetadata, plaintext) || bytes.Contains(encodedMetadata, ciphertext) || strings.Contains(string(encodedMetadata), "key_version") {
		t.Fatalf("metadata leaked provider secret material: %s", encodedMetadata)
	}

	_, err = database.ExecContext(ctx, `
		UPDATE app.deployments
		SET secret_reference_id = $1, version = version + 1,
		    updated_at = CURRENT_TIMESTAMP, updated_by = 'integration:provider-secret'
		WHERE id = $2 AND provider_id = $3`,
		metadata.ID,
		providerSecretDeploymentOneID,
		providerSecretProviderOneID,
	)
	if err != nil {
		t.Fatalf("attach same-provider secret reference: %v", err)
	}
	_, err = database.ExecContext(ctx, `
		UPDATE app.deployments
		SET secret_reference_id = $1, version = version + 1,
		    updated_at = CURRENT_TIMESTAMP, updated_by = 'integration:provider-secret'
		WHERE id = $2 AND provider_id = $3`,
		metadata.ID,
		providerSecretDeploymentTwoID,
		providerSecretProviderTwoID,
	)
	expectConstraint(t, err, "deployments_provider_secret_reference_fk")

	if _, err := store.Get(ctx, providersecret.Locator{ProviderID: providerSecretProviderTwoID, ID: metadata.ID}); !errors.Is(err, providersecret.ErrNotFound) {
		t.Fatalf("Get(cross-provider) error = %v, want ErrNotFound", err)
	}

	external, err := manager.RegisterExternal(ctx, providersecret.RegisterExternalCommand{
		ProviderID: providerSecretProviderOneID, Name: "vault-primary", Backend: providersecret.BackendVault,
		Locator: "vault://provider-secrets/integration#primary", Actor: "integration:vault",
	})
	if err != nil {
		t.Fatalf("RegisterExternal() error = %v", err)
	}
	var externalCiphertext []byte
	var externalLocator string
	if err := database.QueryRowContext(ctx, `
		SELECT ciphertext, locator
		FROM app.provider_secret_references
		WHERE id = $1 AND provider_id = $2`, external.ID, providerSecretProviderOneID,
	).Scan(&externalCiphertext, &externalLocator); err != nil {
		t.Fatalf("query external provider reference: %v", err)
	}
	if externalCiphertext != nil || externalLocator != "vault://provider-secrets/integration#primary" {
		t.Fatalf("external ciphertext/locator = %x/%q", externalCiphertext, externalLocator)
	}

	_, err = manager.CreateLocal(ctx, providersecret.CreateLocalCommand{
		ProviderID: providerSecretProviderOneID, Name: "primary", Plaintext: []byte("duplicate-value"),
		Actor: "integration:duplicate",
	})
	if !errors.Is(err, providersecret.ErrConflict) {
		t.Fatalf("CreateLocal(duplicate) error = %v, want ErrConflict", err)
	}

	_, err = database.ExecContext(ctx, `
		INSERT INTO app.provider_secret_references (
			id, provider_id, name, backend, locator, ciphertext, nonce, key_version,
			created_by, updated_by
		) VALUES (
			'66000000-0000-4000-8000-000000000099', $1, 'invalid-mixed',
			'local_envelope', 'vault://must-not-coexist/path', $2, $3, 'local-v1',
			'integration:test', 'integration:test'
		)`,
		providerSecretProviderOneID,
		bytes.Repeat([]byte{0x44}, 32),
		bytes.Repeat([]byte{0x45}, 12),
	)
	expectConstraint(t, err, "provider_secret_references_backend_material")

	assertProviderSecretSchemaHasNoPlaintextColumn(ctx, t, database)
	if _, err := database.ExecContext(ctx, `
		UPDATE app.provider_secret_references
		SET status = 'disabled', version = version + 1,
		    updated_at = CURRENT_TIMESTAMP, updated_by = 'integration:disable'
		WHERE id = $1 AND provider_id = $2`, metadata.ID, providerSecretProviderOneID,
	); err != nil {
		t.Fatalf("disable provider secret reference: %v", err)
	}
	if _, err := manager.Resolve(ctx, providersecret.Locator{ProviderID: providerSecretProviderOneID, ID: metadata.ID}); !errors.Is(err, providersecret.ErrDisabled) {
		t.Fatalf("Resolve(disabled) error = %v, want ErrDisabled", err)
	}
}

func insertProviderSecretCatalogFixtures(ctx context.Context, t *testing.T, database *sql.DB) {
	t.Helper()
	for _, provider := range []struct {
		id   string
		code string
	}{
		{id: providerSecretProviderOneID, code: "secret-provider-one"},
		{id: providerSecretProviderTwoID, code: "secret-provider-two"},
	} {
		_, err := database.ExecContext(ctx, `
			INSERT INTO app.providers (id, code, name, adapter_type, created_by, updated_by)
			VALUES ($1, $2, $2, 'openai_compatible', 'integration:provider-secret', 'integration:provider-secret')`,
			provider.id,
			provider.code,
		)
		if err != nil {
			t.Fatalf("insert provider secret provider %s: %v", provider.code, err)
		}
	}
	for _, deployment := range []struct {
		id         string
		providerID string
		code       string
	}{
		{id: providerSecretDeploymentOneID, providerID: providerSecretProviderOneID, code: "secret-deploy-one"},
		{id: providerSecretDeploymentTwoID, providerID: providerSecretProviderTwoID, code: "secret-deploy-two"},
	} {
		_, err := database.ExecContext(ctx, `
			INSERT INTO app.deployments (
				id, provider_id, code, physical_model, endpoint_url, region,
				capabilities, created_by, updated_by
			) VALUES ($1, $2, $3, 'vendor-chat-v1', $4, 'cn-north-1', $5,
			          'integration:provider-secret', 'integration:provider-secret')`,
			deployment.id,
			deployment.providerID,
			deployment.code,
			"https://"+deployment.code+".example.test/v1",
			`{"chat":true,"max_context_tokens":128000,"max_output_tokens":8192,"data_retention_mode":"zero_retention","provider_protocol_version":"v1"}`,
		)
		if err != nil {
			t.Fatalf("insert provider secret deployment %s: %v", deployment.code, err)
		}
	}
}

func assertProviderSecretSchemaHasNoPlaintextColumn(ctx context.Context, t *testing.T, database *sql.DB) {
	t.Helper()
	rows, err := database.QueryContext(ctx, `
		SELECT column_name
		FROM information_schema.columns
		WHERE table_schema = 'app' AND table_name = 'provider_secret_references'`)
	if err != nil {
		t.Fatalf("query provider secret columns: %v", err)
	}
	defer func() { _ = rows.Close() }()
	forbidden := map[string]struct{}{
		"plaintext": {}, "api_key": {}, "password": {}, "secret_value": {}, "raw_secret": {},
	}
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatalf("scan provider secret column: %v", err)
		}
		if _, exists := forbidden[column]; exists {
			t.Errorf("forbidden provider secret column %q exists", column)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate provider secret columns: %v", err)
	}
}

func cleanupProviderSecretFixtures(t *testing.T, database *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := database.ExecContext(ctx, `
		UPDATE app.deployments SET secret_reference_id = NULL
		WHERE provider_id IN ($1, $2)`, providerSecretProviderOneID, providerSecretProviderTwoID,
	); err != nil {
		t.Errorf("clear provider secret deployment references: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		DELETE FROM app.deployments WHERE provider_id IN ($1, $2)`,
		providerSecretProviderOneID,
		providerSecretProviderTwoID,
	); err != nil {
		t.Errorf("cleanup provider secret deployments: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		DELETE FROM app.provider_secret_references WHERE provider_id IN ($1, $2)`,
		providerSecretProviderOneID,
		providerSecretProviderTwoID,
	); err != nil {
		t.Errorf("cleanup provider secret references: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		DELETE FROM app.providers WHERE id IN ($1, $2)`,
		providerSecretProviderOneID,
		providerSecretProviderTwoID,
	); err != nil {
		t.Errorf("cleanup provider secret providers: %v", err)
	}
}
