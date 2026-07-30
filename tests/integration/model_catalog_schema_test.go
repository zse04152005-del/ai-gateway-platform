//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/lib/pq"
)

const (
	catalogTenantOneID       = "12000000-0000-4000-8000-000000000001"
	catalogTenantTwoID       = "12000000-0000-4000-8000-000000000002"
	catalogProviderOneID     = "42000000-0000-4000-8000-000000000001"
	catalogProviderTwoID     = "42000000-0000-4000-8000-000000000002"
	catalogModelOneID        = "52000000-0000-4000-8000-000000000001"
	catalogModelTwoID        = "52000000-0000-4000-8000-000000000002"
	catalogModelThreeID      = "52000000-0000-4000-8000-000000000003"
	catalogDeploymentOneID   = "62000000-0000-4000-8000-000000000001"
	catalogDeploymentTwoID   = "62000000-0000-4000-8000-000000000002"
	catalogDeploymentThreeID = "62000000-0000-4000-8000-000000000003"
	catalogDeploymentFourID  = "62000000-0000-4000-8000-000000000004"
	catalogDeploymentFiveID  = "62000000-0000-4000-8000-000000000005"
	catalogDeploymentSixID   = "62000000-0000-4000-8000-000000000006"
)

func TestModelCatalogSchemaConstraints(t *testing.T) {
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
	cleanupModelCatalogFixtures(t, database)
	t.Cleanup(func() { cleanupModelCatalogFixtures(t, database) })

	insertTenant(ctx, t, database, catalogTenantOneID, "catalog-tenant-one", "Catalog Tenant One", "")
	insertTenant(ctx, t, database, catalogTenantTwoID, "catalog-tenant-two", "Catalog Tenant Two", "")
	insertCatalogProvider(ctx, t, database, catalogProviderOneID, "mock-provider", "openai_compatible")

	_, err = database.ExecContext(ctx, `
		INSERT INTO app.logical_models (
			id, tenant_id, name, display_name, required_capabilities,
			allowed_regions, created_by, updated_by
		) VALUES ($1, $2, 'general-chat', 'General Chat', $3, $4, 'integration:catalog', 'integration:catalog')`,
		catalogModelOneID,
		catalogTenantOneID,
		`{"chat":true,"stream":true,"tools":true,"min_context_tokens":32000,"min_output_tokens":4096,"data_retention_modes":["zero_retention","self_hosted"]}`,
		pq.Array([]string{"cn-north-1", "ap-southeast-1"}),
	)
	if err != nil {
		t.Fatalf("insert logical model: %v", err)
	}

	insertCatalogDeployment(
		ctx,
		t,
		database,
		catalogDeploymentOneID,
		"chat-primary",
		"cn-north-1",
		`{"chat":true,"stream":true,"tools":true,"max_context_tokens":128000,"max_output_tokens":8192,"data_retention_mode":"zero_retention","provider_protocol_version":"openai-chat-v1"}`,
	)
	_, err = database.ExecContext(ctx, `
		INSERT INTO app.logical_model_deployments (
			logical_model_id, deployment_id, priority, weight, created_by, updated_by
		) VALUES ($1, $2, 10, 750, 'integration:catalog', 'integration:catalog')`,
		catalogModelOneID,
		catalogDeploymentOneID,
	)
	if err != nil {
		t.Fatalf("bind compatible deployment: %v", err)
	}

	var (
		logicalName     string
		physicalModel   string
		providerCode    string
		region          string
		maxContext      int64
		retention       string
		priority        int16
		weight          int16
		logicalVersion  int64
		deploymentState string
	)
	err = database.QueryRowContext(ctx, `
		SELECT logical_model.name, deployment.physical_model, provider.code,
		       deployment.region, (deployment.capabilities ->> 'max_context_tokens')::bigint,
		       deployment.capabilities ->> 'data_retention_mode',
		       binding.priority, binding.weight, logical_model.version, deployment.status
		FROM app.logical_models AS logical_model
		JOIN app.logical_model_deployments AS binding
		  ON binding.logical_model_id = logical_model.id
		JOIN app.deployments AS deployment ON deployment.id = binding.deployment_id
		JOIN app.providers AS provider ON provider.id = deployment.provider_id
		WHERE logical_model.tenant_id = $1 AND logical_model.name = 'general-chat'`,
		catalogTenantOneID,
	).Scan(
		&logicalName,
		&physicalModel,
		&providerCode,
		&region,
		&maxContext,
		&retention,
		&priority,
		&weight,
		&logicalVersion,
		&deploymentState,
	)
	if err != nil {
		t.Fatalf("query catalog separation: %v", err)
	}
	if logicalName != "general-chat" || physicalModel != "vendor-chat-v1" || providerCode != "mock-provider" {
		t.Fatalf("logical/physical/provider = %q/%q/%q", logicalName, physicalModel, providerCode)
	}
	if region != "cn-north-1" || maxContext != 128000 || retention != "zero_retention" {
		t.Fatalf("region/capability = %q/%d/%q", region, maxContext, retention)
	}
	if priority != 10 || weight != 750 || logicalVersion != 1 || deploymentState != "active" {
		t.Fatalf("binding/version/status = %d/%d/%d/%q", priority, weight, logicalVersion, deploymentState)
	}

	assertCatalogHasNoCredentialColumns(ctx, t, database)

	_, err = database.ExecContext(ctx, `
		INSERT INTO app.providers (id, code, name, adapter_type, created_by, updated_by)
		VALUES ($1, 'mock-provider', 'Duplicate', 'openai_compatible', 'integration:catalog', 'integration:catalog')`,
		catalogProviderTwoID,
	)
	expectConstraint(t, err, "providers_code_unique")

	_, err = database.ExecContext(ctx, `
		INSERT INTO app.logical_models (
			id, tenant_id, name, display_name, required_capabilities, created_by, updated_by
		) VALUES ($1, $2, 'general-chat', 'Duplicate', '{"chat":true}', 'integration:catalog', 'integration:catalog')`,
		catalogModelTwoID,
		catalogTenantOneID,
	)
	expectConstraint(t, err, "logical_models_tenant_name_unique")

	_, err = database.ExecContext(ctx, `
		INSERT INTO app.logical_models (
			id, tenant_id, name, display_name, required_capabilities, created_by, updated_by
		) VALUES ($1, $2, 'general-chat', 'Other Tenant Chat', '{"chat":true}', 'integration:catalog', 'integration:catalog')`,
		catalogModelTwoID,
		catalogTenantTwoID,
	)
	if err != nil {
		t.Fatalf("insert same logical name in another tenant: %v", err)
	}

	_, err = database.ExecContext(ctx, `
		INSERT INTO app.logical_models (
			id, tenant_id, name, display_name, required_capabilities, created_by, updated_by
		) VALUES ($1, $2, 'unsafe-contract', 'Unsafe Contract', '{"chat":true,"vendor_magic":true}', 'integration:catalog', 'integration:catalog')`,
		catalogModelThreeID,
		catalogTenantOneID,
	)
	expectConstraint(t, err, "logical_models_requirements_valid")

	_, err = database.ExecContext(ctx, `
		INSERT INTO app.logical_models (
			id, tenant_id, name, display_name, required_capabilities,
			allowed_regions, created_by, updated_by
		) VALUES ($1, $2, 'duplicate-region', 'Duplicate Region', '{"chat":true}', $3, 'integration:catalog', 'integration:catalog')`,
		catalogModelThreeID,
		catalogTenantOneID,
		pq.Array([]string{"cn-north-1", "cn-north-1"}),
	)
	expectConstraint(t, err, "logical_models_allowed_regions_valid")

	_, err = database.ExecContext(ctx, insertCatalogDeploymentStatement,
		catalogDeploymentTwoID,
		catalogProviderOneID,
		"unknown-capability",
		"vendor-chat-v1",
		"https://unknown.example.test/v1",
		"cn-north-1",
		`{"chat":true,"vendor_magic":true,"max_context_tokens":128000,"max_output_tokens":8192,"data_retention_mode":"zero_retention","provider_protocol_version":"v1"}`,
	)
	expectConstraint(t, err, "deployments_capabilities_valid")

	_, err = database.ExecContext(ctx, insertCatalogDeploymentStatement,
		catalogDeploymentThreeID,
		catalogProviderOneID,
		"userinfo-endpoint",
		"vendor-chat-v1",
		"https://user:password@models.example.test/v1",
		"cn-north-1",
		`{"chat":true,"max_context_tokens":128000,"max_output_tokens":8192,"data_retention_mode":"zero_retention","provider_protocol_version":"v1"}`,
	)
	expectConstraint(t, err, "deployments_endpoint_url_format")

	insertCatalogDeployment(
		ctx,
		t,
		database,
		catalogDeploymentFourID,
		"chat-without-tools",
		"cn-north-1",
		`{"chat":true,"stream":true,"max_context_tokens":128000,"max_output_tokens":8192,"data_retention_mode":"zero_retention","provider_protocol_version":"v1"}`,
	)
	_, err = database.ExecContext(ctx, `
		INSERT INTO app.logical_model_deployments (
			logical_model_id, deployment_id, created_by, updated_by
		) VALUES ($1, $2, 'integration:catalog', 'integration:catalog')`,
		catalogModelOneID,
		catalogDeploymentFourID,
	)
	expectConstraint(t, err, "logical_model_deployments_capability_contract")

	insertCatalogDeployment(
		ctx,
		t,
		database,
		catalogDeploymentFiveID,
		"chat-wrong-region",
		"us-east-1",
		`{"chat":true,"stream":true,"tools":true,"max_context_tokens":128000,"max_output_tokens":8192,"data_retention_mode":"zero_retention","provider_protocol_version":"v1"}`,
	)
	_, err = database.ExecContext(ctx, `
		INSERT INTO app.logical_model_deployments (
			logical_model_id, deployment_id, created_by, updated_by
		) VALUES ($1, $2, 'integration:catalog', 'integration:catalog')`,
		catalogModelOneID,
		catalogDeploymentFiveID,
	)
	expectConstraint(t, err, "logical_model_deployments_capability_contract")

	_, err = database.ExecContext(ctx, `
		UPDATE app.deployments
		SET capabilities = capabilities || '{"tools":false}'::jsonb,
		    version = version + 1, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1`,
		catalogDeploymentOneID,
	)
	expectConstraint(t, err, "logical_model_deployments_capability_contract")

	_, err = database.ExecContext(ctx, `
		UPDATE app.logical_models
		SET required_capabilities = required_capabilities || '{"min_context_tokens":200000}'::jsonb,
		    version = version + 1, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1`,
		catalogModelOneID,
	)
	expectConstraint(t, err, "logical_model_deployments_capability_contract")
}

const insertCatalogDeploymentStatement = `
	INSERT INTO app.deployments (
		id, provider_id, code, physical_model, endpoint_url, region,
		capabilities, created_by, updated_by
	) VALUES ($1, $2, $3, $4, $5, $6, $7, 'integration:catalog', 'integration:catalog')`

func insertCatalogProvider(ctx context.Context, t *testing.T, database *sql.DB, id, code, adapterType string) {
	t.Helper()
	_, err := database.ExecContext(ctx, `
		INSERT INTO app.providers (id, code, name, adapter_type, created_by, updated_by)
		VALUES ($1, $2, 'Catalog Provider', $3, 'integration:catalog', 'integration:catalog')`,
		id,
		code,
		adapterType,
	)
	if err != nil {
		t.Fatalf("insert catalog provider %s: %v", code, err)
	}
}

func insertCatalogDeployment(
	ctx context.Context,
	t *testing.T,
	database *sql.DB,
	id string,
	code string,
	region string,
	capabilities string,
) {
	t.Helper()
	_, err := database.ExecContext(
		ctx,
		insertCatalogDeploymentStatement,
		id,
		catalogProviderOneID,
		code,
		"vendor-chat-v1",
		"https://"+code+".example.test/v1",
		region,
		capabilities,
	)
	if err != nil {
		t.Fatalf("insert catalog deployment %s: %v", code, err)
	}
}

func assertCatalogHasNoCredentialColumns(ctx context.Context, t *testing.T, database *sql.DB) {
	t.Helper()
	rows, err := database.QueryContext(ctx, `
		SELECT table_name, column_name
		FROM information_schema.columns
		WHERE table_schema = 'app'
		  AND table_name IN ('providers', 'logical_models', 'deployments', 'logical_model_deployments')`)
	if err != nil {
		t.Fatalf("query catalog columns: %v", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("close catalog column rows: %v", err)
		}
	}()

	forbidden := map[string]struct{}{
		"api_key": {}, "secret": {}, "credential": {}, "password": {}, "token": {}, "ciphertext": {},
	}
	for rows.Next() {
		var tableName string
		var columnName string
		if err := rows.Scan(&tableName, &columnName); err != nil {
			t.Fatalf("scan catalog column: %v", err)
		}
		if _, exists := forbidden[columnName]; exists {
			t.Errorf("catalog table %s contains forbidden credential column %s", tableName, columnName)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate catalog columns: %v", err)
	}
}

func cleanupModelCatalogFixtures(t *testing.T, database *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := database.ExecContext(ctx, `
		DELETE FROM app.logical_model_deployments
		WHERE logical_model_id IN ($1, $2, $3)
		   OR deployment_id IN ($4, $5, $6, $7, $8, $9)`,
		catalogModelOneID,
		catalogModelTwoID,
		catalogModelThreeID,
		catalogDeploymentOneID,
		catalogDeploymentTwoID,
		catalogDeploymentThreeID,
		catalogDeploymentFourID,
		catalogDeploymentFiveID,
		catalogDeploymentSixID,
	); err != nil {
		t.Errorf("cleanup catalog bindings: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		DELETE FROM app.deployments
		WHERE provider_id IN ($1, $2)
		   OR id IN ($3, $4, $5, $6, $7, $8)`,
		catalogProviderOneID,
		catalogProviderTwoID,
		catalogDeploymentOneID,
		catalogDeploymentTwoID,
		catalogDeploymentThreeID,
		catalogDeploymentFourID,
		catalogDeploymentFiveID,
		catalogDeploymentSixID,
	); err != nil {
		t.Errorf("cleanup catalog deployments: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		DELETE FROM app.logical_models
		WHERE tenant_id IN ($1, $2) OR id IN ($3, $4, $5)`,
		catalogTenantOneID,
		catalogTenantTwoID,
		catalogModelOneID,
		catalogModelTwoID,
		catalogModelThreeID,
	); err != nil {
		t.Errorf("cleanup logical models: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		DELETE FROM app.providers WHERE id IN ($1, $2)`,
		catalogProviderOneID,
		catalogProviderTwoID,
	); err != nil {
		t.Errorf("cleanup providers: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		DELETE FROM app.tenants WHERE id IN ($1, $2)`,
		catalogTenantOneID,
		catalogTenantTwoID,
	); err != nil {
		t.Errorf("cleanup catalog tenants: %v", err)
	}
}
