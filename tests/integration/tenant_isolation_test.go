//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/apierror"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/gateway"
	"github.com/zse04152005-del/ai-gateway-platform/internal/keyauth"
	"github.com/zse04152005-del/ai-gateway-platform/internal/providersecret"
	"github.com/zse04152005-del/ai-gateway-platform/internal/virtualkey"
)

const (
	tenantIsolationTenantOneID    = "17000000-0000-4000-8000-000000000001"
	tenantIsolationTenantTwoID    = "17000000-0000-4000-8000-000000000002"
	tenantIsolationProjectOneID   = "27000000-0000-4000-8000-000000000001"
	tenantIsolationProjectTwoID   = "27000000-0000-4000-8000-000000000002"
	tenantIsolationProviderOneID  = "47000000-0000-4000-8000-000000000001"
	tenantIsolationProviderTwoID  = "47000000-0000-4000-8000-000000000002"
	tenantIsolationModelOneID     = "57000000-0000-4000-8000-000000000001"
	tenantIsolationModelTwoID     = "57000000-0000-4000-8000-000000000002"
	tenantIsolationDeploymentOne  = "67000000-0000-4000-8000-000000000001"
	tenantIsolationDeploymentTwo  = "67000000-0000-4000-8000-000000000002"
	tenantIsolationDuplicateKeyID = "77000000-0000-4000-8000-000000000001"
	tenantIsolationMissingKeyID   = "77000000-0000-4000-8000-000000000099"
	tenantIsolationMissingRefID   = "87000000-0000-4000-8000-000000000099"
	tenantIsolationModelOneName   = "tenant-one-chat"
	tenantIsolationModelTwoName   = "tenant-two-chat"
)

func TestTenantIsolationBoundaries(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set")
	}
	database, err := sql.Open("postgres", databaseURL)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	database.SetMaxOpenConns(8)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		t.Fatalf("database.PingContext() error = %v", err)
	}
	cleanupTenantIsolationFixtures(t, database)
	t.Cleanup(func() { cleanupTenantIsolationFixtures(t, database) })
	seedTenantIsolationCatalog(ctx, t, database)

	now := time.Now().UTC().Truncate(time.Microsecond)
	digestKey := bytes.Repeat([]byte{0x8a}, 32)
	digester, err := virtualkey.NewHMACDigester("tenant-isolation-v1", digestKey)
	if err != nil {
		t.Fatalf("virtualkey.NewHMACDigester() error = %v", err)
	}
	virtualKeyStore, err := virtualkey.NewPostgresStore(database)
	if err != nil {
		t.Fatalf("virtualkey.NewPostgresStore() error = %v", err)
	}
	virtualKeyManager, err := virtualkey.NewManager(virtualKeyStore, digester, rand.Reader, func() time.Time { return now })
	if err != nil {
		t.Fatalf("virtualkey.NewManager() error = %v", err)
	}
	modelsOne := []string{tenantIsolationModelOneName}
	modelsTwo := []string{tenantIsolationModelTwoName}
	keyOne := issueTenantIsolationCredential(
		ctx, t, virtualKeyManager, tenantIsolationTenantOneID, tenantIsolationProjectOneID, &modelsOne,
	)
	keyTwo := issueTenantIsolationCredential(
		ctx, t, virtualKeyManager, tenantIsolationTenantTwoID, tenantIsolationProjectTwoID, &modelsTwo,
	)

	postgresAuthenticationStore, err := keyauth.NewPostgresStore(database)
	if err != nil {
		t.Fatalf("keyauth.NewPostgresStore() error = %v", err)
	}
	authenticationStore := &countingTenantIsolationAuthenticationStore{delegate: postgresAuthenticationStore}
	keyring, err := keyauth.NewKeyring("tenant-isolation-v1", digestKey, nil)
	if err != nil {
		t.Fatalf("keyauth.NewKeyring() error = %v", err)
	}
	cache, err := keyauth.NewMemoryCache(10*time.Minute, 100, func() time.Time { return now })
	if err != nil {
		t.Fatalf("keyauth.NewMemoryCache() error = %v", err)
	}
	authenticator, err := keyauth.NewAuthenticator(authenticationStore, keyring, cache, func() time.Time { return now })
	if err != nil {
		t.Fatalf("keyauth.NewAuthenticator() error = %v", err)
	}
	modelCatalog, err := catalog.NewPostgresStore(database)
	if err != nil {
		t.Fatalf("catalog.NewPostgresStore() error = %v", err)
	}
	handler, err := gateway.NewHandler(authenticator, modelCatalog)
	if err != nil {
		t.Fatalf("gateway.NewHandler() error = %v", err)
	}

	t.Run("direct IDs and lifecycle operations stay in tenant and project scope", func(t *testing.T) {
		crossLocator := virtualkey.Locator{
			TenantID: tenantIsolationTenantTwoID, ProjectID: tenantIsolationProjectTwoID, ID: keyOne.Metadata.ID,
		}
		missingLocator := virtualkey.Locator{
			TenantID: tenantIsolationTenantTwoID, ProjectID: tenantIsolationProjectTwoID, ID: tenantIsolationMissingKeyID,
		}
		_, crossGetErr := virtualKeyManager.Get(ctx, crossLocator)
		_, missingGetErr := virtualKeyManager.Get(ctx, missingLocator)
		assertSameConcealedNotFound(t, crossGetErr, missingGetErr, virtualkey.ErrNotFound)

		_, crossRotateErr := virtualKeyManager.Rotate(ctx, virtualkey.RotateCommand{
			Locator: crossLocator, GracePeriod: time.Minute, Actor: "integration:tenant-isolation",
		})
		_, missingRotateErr := virtualKeyManager.Rotate(ctx, virtualkey.RotateCommand{
			Locator: missingLocator, GracePeriod: time.Minute, Actor: "integration:tenant-isolation",
		})
		assertSameConcealedNotFound(t, crossRotateErr, missingRotateErr, virtualkey.ErrNotFound)

		_, crossRevokeErr := virtualKeyManager.Revoke(ctx, virtualkey.RevokeCommand{
			Locator: crossLocator, Actor: "integration:tenant-isolation",
		})
		_, missingRevokeErr := virtualKeyManager.Revoke(ctx, virtualkey.RevokeCommand{
			Locator: missingLocator, Actor: "integration:tenant-isolation",
		})
		assertSameConcealedNotFound(t, crossRevokeErr, missingRevokeErr, virtualkey.ErrNotFound)

		stored, err := virtualKeyManager.Get(ctx, virtualkey.Locator{
			TenantID: tenantIsolationTenantOneID, ProjectID: tenantIsolationProjectOneID, ID: keyOne.Metadata.ID,
		})
		if err != nil || stored.Status != virtualkey.StateActive {
			t.Fatalf("source key changed after cross-scope operations: metadata/error = %+v/%v", stored, err)
		}
	})

	t.Run("list queries reject mixed tenant and project scopes", func(t *testing.T) {
		assertTenantIsolationCatalogNames(ctx, t, modelCatalog, catalog.Access{
			TenantID: tenantIsolationTenantOneID, ProjectID: tenantIsolationProjectOneID,
		}, []string{tenantIsolationModelOneName})
		assertTenantIsolationCatalogNames(ctx, t, modelCatalog, catalog.Access{
			TenantID: tenantIsolationTenantTwoID, ProjectID: tenantIsolationProjectTwoID,
		}, []string{tenantIsolationModelTwoName})
		assertTenantIsolationCatalogNames(ctx, t, modelCatalog, catalog.Access{
			TenantID: tenantIsolationTenantOneID, ProjectID: tenantIsolationProjectTwoID,
		}, []string{})
		assertTenantIsolationCatalogNames(ctx, t, modelCatalog, catalog.Access{
			TenantID: tenantIsolationTenantTwoID, ProjectID: tenantIsolationProjectOneID,
		}, []string{})
		assertTenantIsolationCatalogNames(ctx, t, modelCatalog, catalog.Access{
			TenantID: tenantIsolationTenantOneID, ProjectID: tenantIsolationMissingKeyID,
		}, []string{})
	})

	t.Run("cache and forged headers cannot replace trusted scope", func(t *testing.T) {
		principal, err := authenticator.Authenticate(ctx, keyOne.Credential)
		if err != nil {
			t.Fatalf("Authenticate(tenant one) error = %v", err)
		}
		if principal.TenantID != tenantIsolationTenantOneID || principal.ProjectID != tenantIsolationProjectOneID {
			t.Fatalf("tenant-one principal scope = %s/%s", principal.TenantID, principal.ProjectID)
		}
		(*principal.AllowedModels)[0] = tenantIsolationModelTwoName
		principal.TenantID = tenantIsolationTenantTwoID
		principal.ProjectID = tenantIsolationProjectTwoID
		reloaded, err := authenticator.Authenticate(ctx, keyOne.Credential)
		if err != nil {
			t.Fatalf("Authenticate(cached tenant one) error = %v", err)
		}
		if reloaded.TenantID != tenantIsolationTenantOneID || reloaded.ProjectID != tenantIsolationProjectOneID ||
			reloaded.AllowedModels == nil || !reflect.DeepEqual(*reloaded.AllowedModels, modelsOne) {
			t.Fatalf("cached principal was polluted: %+v", reloaded)
		}
		if authenticationStore.calls != 1 {
			t.Fatalf("authentication store calls after cache hit = %d, want 1", authenticationStore.calls)
		}

		bodyOne := requestTenantIsolationModels(
			t, handler, keyOne.Credential, tenantIsolationTenantTwoID, tenantIsolationProjectTwoID,
			[]string{tenantIsolationModelOneName},
		)
		assertTenantIsolationResponseDoesNotLeak(t, bodyOne, []string{
			tenantIsolationTenantTwoID, tenantIsolationProjectTwoID, tenantIsolationModelTwoName,
			"tenant-isolation-provider-two", "tenant-two-physical", "tenant-two.private.example.test",
		})
		if authenticationStore.calls != 1 {
			t.Fatalf("authentication store calls after cached HTTP request = %d, want 1", authenticationStore.calls)
		}

		bodyTwo := requestTenantIsolationModels(
			t, handler, keyTwo.Credential, tenantIsolationTenantOneID, tenantIsolationProjectOneID,
			[]string{tenantIsolationModelTwoName},
		)
		assertTenantIsolationResponseDoesNotLeak(t, bodyTwo, []string{
			tenantIsolationTenantOneID, tenantIsolationProjectOneID, tenantIsolationModelOneName,
			"tenant-isolation-provider-one", "tenant-one-physical", "tenant-one.private.example.test",
		})
		if authenticationStore.calls != 2 {
			t.Fatalf("authentication store calls after tenant-two request = %d, want 2", authenticationStore.calls)
		}
	})

	t.Run("key creation and cache identity constraints fail closed", func(t *testing.T) {
		_, err := virtualKeyManager.Create(ctx, virtualkey.CreateCommand{
			TenantID: tenantIsolationTenantOneID, ProjectID: tenantIsolationProjectTwoID,
			Mode: "live", Actor: "integration:tenant-isolation-mixed-scope",
		})
		if !errors.Is(err, virtualkey.ErrNotFound) {
			t.Fatalf("Create(mixed tenant/project) error = %v, want concealed not found", err)
		}

		_, err = database.ExecContext(ctx, `
			INSERT INTO app.virtual_api_keys (
				id, tenant_id, project_id, key_prefix, secret_hash, hash_key_version,
				status, created_by, updated_by
			) VALUES ($1, $2, $3, $4, $5, 'tenant-isolation-v1',
			          'active', 'integration:tenant-isolation', 'integration:tenant-isolation')`,
			tenantIsolationDuplicateKeyID,
			tenantIsolationTenantTwoID,
			tenantIsolationProjectTwoID,
			keyOne.Metadata.Prefix,
			bytes.Repeat([]byte{0x91}, 32),
		)
		expectConstraint(t, err, "virtual_api_keys_prefix_unique")
	})

	t.Run("provider references and deployments remain provider scoped", func(t *testing.T) {
		providerSecretStore, err := providersecret.NewPostgresStore(database)
		if err != nil {
			t.Fatalf("providersecret.NewPostgresStore() error = %v", err)
		}
		localCipher, err := providersecret.NewLocalCipher(
			"tenant-isolation-local-v1", bytes.Repeat([]byte{0xa1}, 32), nil, rand.Reader,
		)
		if err != nil {
			t.Fatalf("providersecret.NewLocalCipher() error = %v", err)
		}
		providerSecretManager, err := providersecret.NewManager(
			providerSecretStore, localCipher, nil, rand.Reader, func() time.Time { return now },
		)
		if err != nil {
			t.Fatalf("providersecret.NewManager() error = %v", err)
		}
		secretOne := []byte("tenant-isolation-provider-one-secret")
		secretTwo := []byte("tenant-isolation-provider-two-secret")
		referenceOne, err := providerSecretManager.CreateLocal(ctx, providersecret.CreateLocalCommand{
			ProviderID: tenantIsolationProviderOneID, Name: "primary", Plaintext: secretOne,
			Actor: "integration:tenant-isolation",
		})
		if err != nil {
			t.Fatalf("CreateLocal(provider one) error = %v", err)
		}
		referenceTwo, err := providerSecretManager.CreateLocal(ctx, providersecret.CreateLocalCommand{
			ProviderID: tenantIsolationProviderTwoID, Name: "primary", Plaintext: secretTwo,
			Actor: "integration:tenant-isolation",
		})
		if err != nil {
			t.Fatalf("CreateLocal(provider two) error = %v", err)
		}
		resolved, err := providerSecretManager.Resolve(ctx, providersecret.Locator{
			ProviderID: tenantIsolationProviderOneID, ID: referenceOne.ID,
		})
		if err != nil || !bytes.Equal(resolved, secretOne) {
			t.Fatalf("Resolve(provider one) value/error = %q/%v", resolved, err)
		}
		clear(resolved)
		clear(secretOne)
		clear(secretTwo)

		_, crossProviderErr := providerSecretManager.Resolve(ctx, providersecret.Locator{
			ProviderID: tenantIsolationProviderTwoID, ID: referenceOne.ID,
		})
		_, missingProviderErr := providerSecretManager.Resolve(ctx, providersecret.Locator{
			ProviderID: tenantIsolationProviderTwoID, ID: tenantIsolationMissingRefID,
		})
		assertSameConcealedNotFound(t, crossProviderErr, missingProviderErr, providersecret.ErrNotFound)

		_, err = database.ExecContext(ctx, `
			UPDATE app.deployments SET secret_reference_id = $1,
				updated_at = CURRENT_TIMESTAMP, updated_by = 'integration:tenant-isolation'
			WHERE id = $2`, referenceOne.ID, tenantIsolationDeploymentOne)
		if err != nil {
			t.Fatalf("attach same-provider reference: %v", err)
		}
		_, err = database.ExecContext(ctx, `
			UPDATE app.deployments SET secret_reference_id = $1,
				updated_at = CURRENT_TIMESTAMP, updated_by = 'integration:tenant-isolation'
			WHERE id = $2`, referenceTwo.ID, tenantIsolationDeploymentOne)
		expectConstraint(t, err, "deployments_provider_secret_reference_fk")
		var retainedReferenceID string
		if err := database.QueryRowContext(ctx, `
			SELECT secret_reference_id FROM app.deployments WHERE id = $1`,
			tenantIsolationDeploymentOne,
		).Scan(&retainedReferenceID); err != nil {
			t.Fatalf("query retained deployment reference: %v", err)
		}
		if retainedReferenceID != referenceOne.ID {
			t.Fatalf("deployment reference = %s, want %s", retainedReferenceID, referenceOne.ID)
		}
	})

	t.Run("credential failures do not reveal key existence or tenant scope", func(t *testing.T) {
		wrongSecret := replaceTenantIsolationCredentialSecret(keyOne.Credential)
		unknownValue := unknownTenantIsolationValue()
		wrongResponse := requestTenantIsolationAuthenticationFailure(t, handler, wrongSecret)
		unknownResponse := requestTenantIsolationAuthenticationFailure(t, handler, unknownValue)
		if wrongResponse != unknownResponse {
			t.Fatalf("wrong-secret and unknown-key responses differ:\nwrong=%s\nunknown=%s", wrongResponse, unknownResponse)
		}
		assertTenantIsolationResponseDoesNotLeak(t, wrongResponse, []string{
			tenantIsolationTenantOneID, tenantIsolationTenantTwoID,
			tenantIsolationProjectOneID, tenantIsolationProjectTwoID,
			keyOne.Metadata.ID, keyTwo.Metadata.ID,
			tenantIsolationModelOneName, tenantIsolationModelTwoName,
		})
	})
}

type countingTenantIsolationAuthenticationStore struct {
	delegate keyauth.Store
	calls    int
}

func (store *countingTenantIsolationAuthenticationStore) Lookup(ctx context.Context, prefix string) (keyauth.Record, error) {
	store.calls++
	return store.delegate.Lookup(ctx, prefix)
}

func issueTenantIsolationCredential(
	ctx context.Context,
	t *testing.T,
	manager *virtualkey.Manager,
	tenantID string,
	projectID string,
	allowedModels *[]string,
) virtualkey.IssuedCredential {
	t.Helper()
	issued, err := manager.Create(ctx, virtualkey.CreateCommand{
		TenantID: tenantID, ProjectID: projectID, Mode: "live",
		AllowedModels: allowedModels, Actor: "integration:tenant-isolation",
	})
	if err != nil {
		t.Fatalf("create tenant-isolation credential for %s/%s: %v", tenantID, projectID, err)
	}
	return issued
}

func assertSameConcealedNotFound(t *testing.T, crossErr, missingErr, sentinel error) {
	t.Helper()
	if !errors.Is(crossErr, sentinel) || !errors.Is(missingErr, sentinel) {
		t.Fatalf("cross/missing errors = %v/%v, want %v", crossErr, missingErr, sentinel)
	}
	if crossErr.Error() != missingErr.Error() {
		t.Fatalf("cross-scope and missing-object errors differ: %q/%q", crossErr, missingErr)
	}
}

func assertTenantIsolationCatalogNames(
	ctx context.Context,
	t *testing.T,
	store *catalog.PostgresStore,
	access catalog.Access,
	want []string,
) {
	t.Helper()
	models, err := store.ListAvailable(ctx, access)
	if err != nil {
		t.Fatalf("ListAvailable(%s/%s) error = %v", access.TenantID, access.ProjectID, err)
	}
	got := make([]string, 0, len(models))
	for _, model := range models {
		got = append(got, model.Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListAvailable(%s/%s) = %#v, want %#v", access.TenantID, access.ProjectID, got, want)
	}
}

func requestTenantIsolationModels(
	t *testing.T,
	handler http.Handler,
	credential string,
	forgedTenantID string,
	forgedProjectID string,
	want []string,
) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("X-Tenant-Id", forgedTenantID)
	request.Header.Set("X-Project-Id", forgedProjectID)
	request.Header.Set("X-Virtual-Key-Id", tenantIsolationMissingKeyID)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /v1/models status = %d; body = %s", response.Code, response.Body)
	}
	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode model response: %v", err)
	}
	got := make([]string, 0, len(body.Data))
	for _, model := range body.Data {
		got = append(got, model.ID)
	}
	if body.Object != "list" || !reflect.DeepEqual(got, want) {
		t.Fatalf("model response = %q/%#v, want list/%#v", body.Object, got, want)
	}
	return response.Body.String()
}

func requestTenantIsolationAuthenticationFailure(t *testing.T, handler http.Handler, credential string) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+credential)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("authentication failure status = %d, want 401; body = %s", response.Code, response.Body)
	}
	var envelope apierror.Envelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode authentication failure: %v", err)
	}
	if envelope.Error.Code != "INVALID_API_KEY" || envelope.Error.Message != "The API key is invalid" {
		t.Fatalf("authentication failure envelope = %+v", envelope.Error)
	}
	return response.Body.String()
}

func replaceTenantIsolationCredentialSecret(credential string) string {
	prefix, _, _ := strings.Cut(credential, ".")
	return prefix + "." + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xc1}, 32))
}

func unknownTenantIsolationValue() string {
	prefix := strings.Join([]string{"agw", "live", "unknown01"}, "_")
	return prefix + "." + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xb1}, 32))
}

func assertTenantIsolationResponseDoesNotLeak(t *testing.T, body string, forbidden []string) {
	t.Helper()
	for _, value := range forbidden {
		if strings.Contains(body, value) {
			t.Fatalf("response leaked %q: %s", value, body)
		}
	}
}

func seedTenantIsolationCatalog(ctx context.Context, t *testing.T, database *sql.DB) {
	t.Helper()
	insertTenant(ctx, t, database, tenantIsolationTenantOneID, "tenant-isolation-one", "Tenant Isolation One", "")
	insertTenant(ctx, t, database, tenantIsolationTenantTwoID, "tenant-isolation-two", "Tenant Isolation Two", "")
	insertProject(
		ctx, t, database, tenantIsolationProjectOneID, tenantIsolationTenantOneID,
		"tenant-isolation-one", "Tenant Isolation One", "",
	)
	insertProject(
		ctx, t, database, tenantIsolationProjectTwoID, tenantIsolationTenantTwoID,
		"tenant-isolation-two", "Tenant Isolation Two", "",
	)
	insertTenantIsolationProvider(ctx, t, database, tenantIsolationProviderOneID, "tenant-isolation-provider-one")
	insertTenantIsolationProvider(ctx, t, database, tenantIsolationProviderTwoID, "tenant-isolation-provider-two")
	insertTenantIsolationLogicalModel(
		ctx, t, database, tenantIsolationModelOneID, tenantIsolationTenantOneID, tenantIsolationModelOneName,
	)
	insertTenantIsolationLogicalModel(
		ctx, t, database, tenantIsolationModelTwoID, tenantIsolationTenantTwoID, tenantIsolationModelTwoName,
	)
	insertTenantIsolationDeployment(
		ctx, t, database, tenantIsolationDeploymentOne, tenantIsolationProviderOneID,
		"tenant-one-deployment", "tenant-one-physical", "tenant-one.private.example.test",
	)
	insertTenantIsolationDeployment(
		ctx, t, database, tenantIsolationDeploymentTwo, tenantIsolationProviderTwoID,
		"tenant-two-deployment", "tenant-two-physical", "tenant-two.private.example.test",
	)
	bindTenantIsolationDeployment(ctx, t, database, tenantIsolationModelOneID, tenantIsolationDeploymentOne)
	bindTenantIsolationDeployment(ctx, t, database, tenantIsolationModelTwoID, tenantIsolationDeploymentTwo)
	allowProjectModel(ctx, t, database, tenantIsolationTenantOneID, tenantIsolationProjectOneID, tenantIsolationModelOneID)
	allowProjectModel(ctx, t, database, tenantIsolationTenantTwoID, tenantIsolationProjectTwoID, tenantIsolationModelTwoID)
}

func insertTenantIsolationProvider(ctx context.Context, t *testing.T, database *sql.DB, id, code string) {
	t.Helper()
	_, err := database.ExecContext(ctx, `
		INSERT INTO app.providers (id, code, name, adapter_type, created_by, updated_by)
		VALUES ($1, $2, $2, 'openai_compatible', 'integration:tenant-isolation', 'integration:tenant-isolation')`,
		id,
		code,
	)
	if err != nil {
		t.Fatalf("insert tenant-isolation provider %s: %v", code, err)
	}
}

func insertTenantIsolationLogicalModel(
	ctx context.Context,
	t *testing.T,
	database *sql.DB,
	id string,
	tenantID string,
	name string,
) {
	t.Helper()
	_, err := database.ExecContext(ctx, `
		INSERT INTO app.logical_models (
			id, tenant_id, name, display_name, required_capabilities, created_by, updated_by
		) VALUES ($1, $2, $3, $3, '{"chat":true}',
		          'integration:tenant-isolation', 'integration:tenant-isolation')`,
		id,
		tenantID,
		name,
	)
	if err != nil {
		t.Fatalf("insert tenant-isolation model %s: %v", name, err)
	}
}

func insertTenantIsolationDeployment(
	ctx context.Context,
	t *testing.T,
	database *sql.DB,
	id string,
	providerID string,
	code string,
	physicalModel string,
	host string,
) {
	t.Helper()
	_, err := database.ExecContext(ctx, `
		INSERT INTO app.deployments (
			id, provider_id, code, physical_model, endpoint_url, region,
			capabilities, created_by, updated_by
		) VALUES ($1, $2, $3, $4, $5, 'cn-north-1', $6,
		          'integration:tenant-isolation', 'integration:tenant-isolation')`,
		id,
		providerID,
		code,
		physicalModel,
		"https://"+host+"/v1",
		`{"chat":true,"max_context_tokens":128000,"max_output_tokens":8192,"data_retention_mode":"zero_retention","provider_protocol_version":"v1"}`,
	)
	if err != nil {
		t.Fatalf("insert tenant-isolation deployment %s: %v", code, err)
	}
}

func bindTenantIsolationDeployment(ctx context.Context, t *testing.T, database *sql.DB, modelID, deploymentID string) {
	t.Helper()
	_, err := database.ExecContext(ctx, `
		INSERT INTO app.logical_model_deployments (
			logical_model_id, deployment_id, created_by, updated_by
		) VALUES ($1, $2, 'integration:tenant-isolation', 'integration:tenant-isolation')`,
		modelID,
		deploymentID,
	)
	if err != nil {
		t.Fatalf("bind tenant-isolation deployment: %v", err)
	}
}

func cleanupTenantIsolationFixtures(t *testing.T, database *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	statements := []struct {
		name  string
		query string
		args  []any
	}{
		{name: "credentials", query: `DELETE FROM app.virtual_api_keys WHERE tenant_id IN ($1, $2)`, args: []any{tenantIsolationTenantOneID, tenantIsolationTenantTwoID}},
		{name: "project model grants", query: `DELETE FROM app.project_logical_models WHERE tenant_id IN ($1, $2)`, args: []any{tenantIsolationTenantOneID, tenantIsolationTenantTwoID}},
		{name: "bindings", query: `DELETE FROM app.logical_model_deployments WHERE logical_model_id IN ($1, $2)`, args: []any{tenantIsolationModelOneID, tenantIsolationModelTwoID}},
		{name: "deployments", query: `DELETE FROM app.deployments WHERE provider_id IN ($1, $2)`, args: []any{tenantIsolationProviderOneID, tenantIsolationProviderTwoID}},
		{name: "provider references", query: `DELETE FROM app.provider_secret_references WHERE provider_id IN ($1, $2)`, args: []any{tenantIsolationProviderOneID, tenantIsolationProviderTwoID}},
		{name: "models", query: `DELETE FROM app.logical_models WHERE tenant_id IN ($1, $2)`, args: []any{tenantIsolationTenantOneID, tenantIsolationTenantTwoID}},
		{name: "providers", query: `DELETE FROM app.providers WHERE id IN ($1, $2)`, args: []any{tenantIsolationProviderOneID, tenantIsolationProviderTwoID}},
		{name: "projects", query: `DELETE FROM app.projects WHERE tenant_id IN ($1, $2)`, args: []any{tenantIsolationTenantOneID, tenantIsolationTenantTwoID}},
		{name: "tenants", query: `DELETE FROM app.tenants WHERE id IN ($1, $2)`, args: []any{tenantIsolationTenantOneID, tenantIsolationTenantTwoID}},
	}
	for _, statement := range statements {
		if _, err := database.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Errorf("cleanup tenant-isolation %s: %v", statement.name, err)
		}
	}
}
