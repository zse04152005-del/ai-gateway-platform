//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
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
	"github.com/zse04152005-del/ai-gateway-platform/internal/virtualkey"
)

const (
	modelListTenantOneID        = "14000000-0000-4000-8000-000000000001"
	modelListTenantTwoID        = "14000000-0000-4000-8000-000000000002"
	modelListProjectOneID       = "24000000-0000-4000-8000-000000000001"
	modelListProjectTwoID       = "24000000-0000-4000-8000-000000000002"
	modelListProviderActiveID   = "44000000-0000-4000-8000-000000000001"
	modelListProviderDisabledID = "44000000-0000-4000-8000-000000000002"
	modelListModelAID           = "54000000-0000-4000-8000-000000000001"
	modelListModelBID           = "54000000-0000-4000-8000-000000000002"
	modelListModelCID           = "54000000-0000-4000-8000-000000000003"
	modelListModelDID           = "54000000-0000-4000-8000-000000000004"
	modelListCrossTenantID      = "54000000-0000-4000-8000-000000000005"
	modelListDeploymentAID      = "64000000-0000-4000-8000-000000000001"
	modelListDeploymentBID      = "64000000-0000-4000-8000-000000000002"
	modelListDeploymentCID      = "64000000-0000-4000-8000-000000000003"
	modelListDeploymentDID      = "64000000-0000-4000-8000-000000000004"
)

func TestModelListAuthorizationIntersection(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		t.Fatalf("database.PingContext() error = %v", err)
	}
	cleanupModelListFixtures(t, database)
	t.Cleanup(func() { cleanupModelListFixtures(t, database) })
	seedModelListCatalog(ctx, t, database)

	digestKey := bytes.Repeat([]byte{0x7a}, 32)
	digester, err := virtualkey.NewHMACDigester("model-list-integration-v1", digestKey)
	if err != nil {
		t.Fatalf("NewHMACDigester() error = %v", err)
	}
	lifecycleStore, err := virtualkey.NewPostgresStore(database)
	if err != nil {
		t.Fatalf("virtualkey.NewPostgresStore() error = %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	manager, err := virtualkey.NewManager(lifecycleStore, digester, rand.Reader, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	authenticationStore, err := keyauth.NewPostgresStore(database)
	if err != nil {
		t.Fatalf("keyauth.NewPostgresStore() error = %v", err)
	}
	keyring, err := keyauth.NewKeyring("model-list-integration-v1", digestKey, nil)
	if err != nil {
		t.Fatalf("NewKeyring() error = %v", err)
	}
	cache, err := keyauth.NewMemoryCache(0, 10, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewMemoryCache() error = %v", err)
	}
	authenticator, err := keyauth.NewAuthenticator(authenticationStore, keyring, cache, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewAuthenticator() error = %v", err)
	}
	modelCatalog, err := catalog.NewPostgresStore(database)
	if err != nil {
		t.Fatalf("catalog.NewPostgresStore() error = %v", err)
	}
	handler, err := gateway.NewHandler(authenticator, modelCatalog)
	if err != nil {
		t.Fatalf("gateway.NewHandler() error = %v", err)
	}

	explicitModels := []string{"MODEL-A", "model-c", "model-d"}
	explicit := issueModelListCredential(ctx, t, manager, &explicitModels, "integration:model-list-explicit")
	inherited := issueModelListCredential(ctx, t, manager, nil, "integration:model-list-inherited")
	emptyModels := []string{}
	denied := issueModelListCredential(ctx, t, manager, &emptyModels, "integration:model-list-denied")

	assertModelListResponse(t, handler, explicit.Credential, []string{"model-a"})
	assertModelListResponse(t, handler, inherited.Credential, []string{"model-a", "model-b"})
	assertModelListResponse(t, handler, denied.Credential, []string{})

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	assertModelListError(t, unauthorized, http.StatusUnauthorized, "INVALID_API_KEY")

	wrongMethodRequest := httptest.NewRequest(http.MethodPost, "/v1/models", nil)
	wrongMethodRequest.Header.Set("Authorization", "Bearer "+explicit.Credential)
	wrongMethod := httptest.NewRecorder()
	handler.ServeHTTP(wrongMethod, wrongMethodRequest)
	assertModelListError(t, wrongMethod, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")

	_, err = database.ExecContext(ctx, `
		INSERT INTO app.project_logical_models (
			tenant_id, project_id, logical_model_id, created_by, updated_by
		) VALUES ($1, $2, $3, 'integration:model-list', 'integration:model-list')`,
		modelListTenantOneID,
		modelListProjectOneID,
		modelListCrossTenantID,
	)
	expectConstraint(t, err, "project_logical_models_model_fk")
}

func seedModelListCatalog(ctx context.Context, t *testing.T, database *sql.DB) {
	t.Helper()
	insertTenant(ctx, t, database, modelListTenantOneID, "model-list-tenant-one", "Model List Tenant One", "")
	insertTenant(ctx, t, database, modelListTenantTwoID, "model-list-tenant-two", "Model List Tenant Two", "")
	insertProject(ctx, t, database, modelListProjectOneID, modelListTenantOneID, "model-list-project-one", "Model List Project One", "")
	insertProject(ctx, t, database, modelListProjectTwoID, modelListTenantTwoID, "model-list-project-two", "Model List Project Two", "")

	insertModelListProvider(ctx, t, database, modelListProviderActiveID, "model-list-active", "active")
	insertModelListProvider(ctx, t, database, modelListProviderDisabledID, "model-list-disabled", "disabled")
	insertModelListLogical(ctx, t, database, modelListModelAID, modelListTenantOneID, "model-a", `{"chat":true,"stream":true}`)
	insertModelListLogical(ctx, t, database, modelListModelBID, modelListTenantOneID, "model-b", `{"chat":true,"tools":true}`)
	insertModelListLogical(ctx, t, database, modelListModelCID, modelListTenantOneID, "model-c", `{"chat":true}`)
	insertModelListLogical(ctx, t, database, modelListModelDID, modelListTenantOneID, "model-d", `{"chat":true}`)
	insertModelListLogical(ctx, t, database, modelListCrossTenantID, modelListTenantTwoID, "model-a", `{"chat":true}`)

	insertModelListDeployment(ctx, t, database, modelListDeploymentAID, modelListProviderActiveID, "model-a-deploy", "model-a-physical")
	insertModelListDeployment(ctx, t, database, modelListDeploymentBID, modelListProviderActiveID, "model-b-deploy", "model-b-physical")
	insertModelListDeployment(ctx, t, database, modelListDeploymentCID, modelListProviderDisabledID, "model-c-deploy", "model-c-physical")
	insertModelListDeployment(ctx, t, database, modelListDeploymentDID, modelListProviderActiveID, "model-d-deploy", "model-d-physical")
	bindModelListDeployment(ctx, t, database, modelListModelAID, modelListDeploymentAID)
	bindModelListDeployment(ctx, t, database, modelListModelBID, modelListDeploymentBID)
	bindModelListDeployment(ctx, t, database, modelListModelCID, modelListDeploymentCID)
	bindModelListDeployment(ctx, t, database, modelListModelDID, modelListDeploymentDID)
	bindModelListDeployment(ctx, t, database, modelListCrossTenantID, modelListDeploymentAID)

	allowProjectModel(ctx, t, database, modelListTenantOneID, modelListProjectOneID, modelListModelAID)
	allowProjectModel(ctx, t, database, modelListTenantOneID, modelListProjectOneID, modelListModelBID)
	allowProjectModel(ctx, t, database, modelListTenantOneID, modelListProjectOneID, modelListModelCID)
	allowProjectModel(ctx, t, database, modelListTenantTwoID, modelListProjectTwoID, modelListCrossTenantID)
}

func insertModelListProvider(ctx context.Context, t *testing.T, database *sql.DB, id, code, status string) {
	t.Helper()
	_, err := database.ExecContext(ctx, `
		INSERT INTO app.providers (id, code, name, adapter_type, status, created_by, updated_by)
		VALUES ($1, $2, $2, 'openai_compatible', $3, 'integration:model-list', 'integration:model-list')`,
		id,
		code,
		status,
	)
	if err != nil {
		t.Fatalf("insert model-list provider %s: %v", code, err)
	}
}

func insertModelListLogical(ctx context.Context, t *testing.T, database *sql.DB, id, tenantID, name, requirements string) {
	t.Helper()
	_, err := database.ExecContext(ctx, `
		INSERT INTO app.logical_models (
			id, tenant_id, name, display_name, required_capabilities, created_by, updated_by
		) VALUES ($1, $2, $3, $3, $4, 'integration:model-list', 'integration:model-list')`,
		id,
		tenantID,
		name,
		requirements,
	)
	if err != nil {
		t.Fatalf("insert model-list logical model %s: %v", name, err)
	}
}

func insertModelListDeployment(ctx context.Context, t *testing.T, database *sql.DB, id, providerID, code, physical string) {
	t.Helper()
	_, err := database.ExecContext(ctx, `
		INSERT INTO app.deployments (
			id, provider_id, code, physical_model, endpoint_url, region,
			capabilities, created_by, updated_by
		) VALUES ($1, $2, $3, $4, $5, 'cn-north-1', $6, 'integration:model-list', 'integration:model-list')`,
		id,
		providerID,
		code,
		physical,
		"https://"+code+".private.example.test/v1",
		`{"chat":true,"stream":true,"tools":true,"max_context_tokens":128000,"max_output_tokens":8192,"data_retention_mode":"zero_retention","provider_protocol_version":"v1"}`,
	)
	if err != nil {
		t.Fatalf("insert model-list deployment %s: %v", code, err)
	}
}

func bindModelListDeployment(ctx context.Context, t *testing.T, database *sql.DB, modelID, deploymentID string) {
	t.Helper()
	_, err := database.ExecContext(ctx, `
		INSERT INTO app.logical_model_deployments (
			logical_model_id, deployment_id, created_by, updated_by
		) VALUES ($1, $2, 'integration:model-list', 'integration:model-list')`,
		modelID,
		deploymentID,
	)
	if err != nil {
		t.Fatalf("bind model-list deployment: %v", err)
	}
}

func allowProjectModel(ctx context.Context, t *testing.T, database *sql.DB, tenantID, projectID, modelID string) {
	t.Helper()
	_, err := database.ExecContext(ctx, `
		INSERT INTO app.project_logical_models (
			tenant_id, project_id, logical_model_id, created_by, updated_by
		) VALUES ($1, $2, $3, 'integration:model-list', 'integration:model-list')`,
		tenantID,
		projectID,
		modelID,
	)
	if err != nil {
		t.Fatalf("allow project model: %v", err)
	}
}

func issueModelListCredential(
	ctx context.Context,
	t *testing.T,
	manager *virtualkey.Manager,
	allowedModels *[]string,
	actor string,
) virtualkey.IssuedCredential {
	t.Helper()
	issued, err := manager.Create(ctx, virtualkey.CreateCommand{
		TenantID: modelListTenantOneID, ProjectID: modelListProjectOneID, Mode: "live",
		AllowedModels: allowedModels, Actor: actor,
	})
	if err != nil {
		t.Fatalf("create model-list credential: %v", err)
	}
	return issued
}

func assertModelListResponse(t *testing.T, handler http.Handler, credential string, want []string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("X-Tenant-Id", modelListTenantTwoID)
	request.Header.Set("X-Project-Id", modelListProjectTwoID)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("model list status = %d; body = %s", response.Code, response.Body)
	}
	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode model list: %v", err)
	}
	got := make([]string, 0, len(body.Data))
	for _, model := range body.Data {
		got = append(got, model.ID)
	}
	if body.Object != "list" || !reflect.DeepEqual(got, want) {
		t.Fatalf("model list = %q/%#v, want list/%#v", body.Object, got, want)
	}
	for _, forbidden := range []string{
		"model-list-active", "model-list-disabled", "physical", "private.example.test", modelListTenantTwoID,
	} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Errorf("model list leaked %q: %s", forbidden, response.Body)
		}
	}
}

func assertModelListError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, status, response.Body)
	}
	var envelope apierror.Envelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode model-list error: %v", err)
	}
	if envelope.Error.Code != code {
		t.Fatalf("error code = %q, want %q", envelope.Error.Code, code)
	}
}

func cleanupModelListFixtures(t *testing.T, database *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	statements := []struct {
		name  string
		query string
		args  []any
	}{
		{name: "credentials", query: `DELETE FROM app.virtual_api_keys WHERE tenant_id IN ($1, $2)`, args: []any{modelListTenantOneID, modelListTenantTwoID}},
		{name: "project grants", query: `DELETE FROM app.project_logical_models WHERE tenant_id IN ($1, $2)`, args: []any{modelListTenantOneID, modelListTenantTwoID}},
		{name: "bindings", query: `DELETE FROM app.logical_model_deployments WHERE logical_model_id IN ($1, $2, $3, $4, $5)`, args: []any{modelListModelAID, modelListModelBID, modelListModelCID, modelListModelDID, modelListCrossTenantID}},
		{name: "deployments", query: `DELETE FROM app.deployments WHERE provider_id IN ($1, $2)`, args: []any{modelListProviderActiveID, modelListProviderDisabledID}},
		{name: "models", query: `DELETE FROM app.logical_models WHERE tenant_id IN ($1, $2)`, args: []any{modelListTenantOneID, modelListTenantTwoID}},
		{name: "providers", query: `DELETE FROM app.providers WHERE id IN ($1, $2)`, args: []any{modelListProviderActiveID, modelListProviderDisabledID}},
		{name: "projects", query: `DELETE FROM app.projects WHERE tenant_id IN ($1, $2)`, args: []any{modelListTenantOneID, modelListTenantTwoID}},
		{name: "tenants", query: `DELETE FROM app.tenants WHERE id IN ($1, $2)`, args: []any{modelListTenantOneID, modelListTenantTwoID}},
	}
	for _, statement := range statements {
		if _, err := database.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Errorf("cleanup model-list %s: %v", statement.name, err)
		}
	}
}
