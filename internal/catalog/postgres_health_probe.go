package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

const maximumHealthProbeTargets = 10_000

const listHealthProbeTargetsStatement = `
	SELECT
		deployment.id::text, deployment.provider_id::text, deployment.code,
		deployment.physical_model, deployment.endpoint_url, deployment.region,
		deployment.capabilities, deployment.secret_reference_id::text,
		deployment.status, deployment.version, deployment.created_at,
		deployment.created_by, deployment.updated_at, deployment.updated_by,
		provider.id::text, provider.code, provider.name, provider.adapter_type,
		provider.status, provider.version, provider.created_at,
		provider.created_by, provider.updated_at, provider.updated_by
	FROM app.deployments AS deployment
	JOIN app.providers AS provider ON provider.id = deployment.provider_id
	WHERE deployment.status = 'active'
	  AND provider.status = 'active'
	  AND COALESCE((deployment.capabilities ->> 'chat')::boolean, false)
	  AND EXISTS (
		SELECT 1
		FROM app.logical_model_deployments AS binding
		JOIN app.logical_models AS logical_model ON logical_model.id = binding.logical_model_id
		JOIN app.project_logical_models AS project_model
		  ON project_model.tenant_id = logical_model.tenant_id
		 AND project_model.logical_model_id = logical_model.id
		JOIN app.projects AS project
		  ON project.tenant_id = project_model.tenant_id
		 AND project.id = project_model.project_id
		JOIN app.tenants AS tenant ON tenant.id = logical_model.tenant_id
		WHERE binding.deployment_id = deployment.id
		  AND binding.status = 'active'
		  AND logical_model.status = 'active'
		  AND project_model.status = 'active'
		  AND project.status = 'active'
		  AND tenant.status = 'active'
	  )
	ORDER BY provider.code, deployment.code, deployment.id
	LIMIT 10001`

// ListHealthProbeTargets returns unique active deployments that back at least
// one active logical model for an active tenant. The query is intentionally
// global because physical health is deployment-scoped, not tenant-scoped.
func (store *PostgresStore) ListHealthProbeTargets(ctx context.Context) ([]HealthProbeTarget, error) {
	if store == nil || store.database == nil {
		return nil, errors.New("health probe catalog is not initialized")
	}
	if ctx == nil {
		return nil, errors.New("health probe catalog context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := store.database.QueryContext(ctx, listHealthProbeTargetsStatement)
	if err != nil {
		return nil, fmt.Errorf("query health probe targets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	targets := make([]HealthProbeTarget, 0)
	for rows.Next() {
		target, scanErr := scanHealthProbeTarget(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate health probe targets: %w", err)
	}
	if len(targets) > maximumHealthProbeTargets {
		return nil, errors.New("health probe target count exceeds the safe limit")
	}
	return targets, nil
}

func scanHealthProbeTarget(rows *sql.Rows) (HealthProbeTarget, error) {
	var target HealthProbeTarget
	var capabilitiesJSON []byte
	var secretReferenceID sql.NullString
	err := rows.Scan(
		&target.Deployment.ID, &target.Deployment.ProviderID, &target.Deployment.Code,
		&target.Deployment.PhysicalModel, &target.Deployment.EndpointURL, &target.Deployment.Region,
		&capabilitiesJSON, &secretReferenceID,
		&target.Deployment.Status, &target.Deployment.Version, &target.Deployment.CreatedAt,
		&target.Deployment.CreatedBy, &target.Deployment.UpdatedAt, &target.Deployment.UpdatedBy,
		&target.Provider.ID, &target.Provider.Code, &target.Provider.Name, &target.Provider.AdapterType,
		&target.Provider.Status, &target.Provider.Version, &target.Provider.CreatedAt,
		&target.Provider.CreatedBy, &target.Provider.UpdatedAt, &target.Provider.UpdatedBy,
	)
	if err != nil {
		return HealthProbeTarget{}, fmt.Errorf("scan health probe target: %w", err)
	}
	if secretReferenceID.Valid {
		target.Deployment.SecretReferenceID = &secretReferenceID.String
	}
	if err := json.Unmarshal(capabilitiesJSON, &target.Deployment.Capabilities); err != nil {
		return HealthProbeTarget{}, fmt.Errorf("decode health probe capabilities: %w", err)
	}
	if err := target.Validate(); err != nil {
		return HealthProbeTarget{}, fmt.Errorf("validate stored health probe target: %w", err)
	}
	return target, nil
}
