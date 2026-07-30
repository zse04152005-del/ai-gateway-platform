package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lib/pq"
)

const maximumRouteCandidates = 256

const listRouteCandidatesStatement = `
	SELECT
		logical_model.id::text, logical_model.tenant_id::text, logical_model.name,
		logical_model.display_name, logical_model.description,
		logical_model.required_capabilities, logical_model.allowed_regions,
		logical_model.status, logical_model.version, logical_model.created_at,
		logical_model.created_by, logical_model.updated_at, logical_model.updated_by,
		binding.logical_model_id::text, binding.deployment_id::text,
		binding.priority, binding.weight, binding.status, binding.version,
		binding.created_at, binding.created_by, binding.updated_at, binding.updated_by,
		deployment.id::text, deployment.provider_id::text, deployment.code,
		deployment.physical_model, deployment.endpoint_url, deployment.region,
		deployment.capabilities, deployment.secret_reference_id::text,
		deployment.status, deployment.version, deployment.created_at,
		deployment.created_by, deployment.updated_at, deployment.updated_by,
		provider.id::text, provider.code, provider.name, provider.adapter_type,
		provider.status, provider.version, provider.created_at,
		provider.created_by, provider.updated_at, provider.updated_by
	FROM app.project_logical_models AS project_model
	JOIN app.projects AS project
	  ON project.tenant_id = project_model.tenant_id
	 AND project.id = project_model.project_id
	JOIN app.tenants AS tenant ON tenant.id = project_model.tenant_id
	JOIN app.logical_models AS logical_model
	  ON logical_model.tenant_id = project_model.tenant_id
	 AND logical_model.id = project_model.logical_model_id
	JOIN app.logical_model_deployments AS binding
	  ON binding.logical_model_id = logical_model.id
	JOIN app.deployments AS deployment ON deployment.id = binding.deployment_id
	JOIN app.providers AS provider ON provider.id = deployment.provider_id
	WHERE project_model.tenant_id = $1
	  AND project_model.project_id = $2
	  AND logical_model.name = $3
	  AND tenant.status = 'active'
	  AND project.status = 'active'
	  AND project_model.status = 'active'
	  AND logical_model.status = 'active'
	  AND binding.status = 'active'
	  AND deployment.status = 'active'
	  AND provider.status = 'active'
	  AND ($4::text[] IS NULL OR logical_model.name = ANY($4::text[]))
	ORDER BY binding.priority, provider.code, deployment.code, deployment.id
	LIMIT 257`

// ListRouteCandidates returns only authorized, active candidate records for one
// exact logical model. Ordering is deterministic but the selector also sorts so
// alternate sources cannot change priority semantics.
func (store *PostgresStore) ListRouteCandidates(ctx context.Context, query RouteQuery) ([]RouteCandidate, error) {
	if ctx == nil {
		return nil, errors.New("catalog route context must not be nil")
	}
	allowedModels, err := validateAccess(query.Access)
	if err != nil {
		return nil, err
	}
	if query.LogicalModel != strings.TrimSpace(query.LogicalModel) || !modelNamePattern.MatchString(query.LogicalModel) {
		return nil, invalid("route_query.logical_model", "must be a canonical logical model name")
	}
	rows, err := store.database.QueryContext(
		ctx,
		listRouteCandidatesStatement,
		query.Access.TenantID,
		query.Access.ProjectID,
		query.LogicalModel,
		allowedModels,
	)
	if err != nil {
		return nil, fmt.Errorf("query route candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	candidates := make([]RouteCandidate, 0)
	for rows.Next() {
		candidate, scanErr := scanRouteCandidate(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate route candidates: %w", err)
	}
	if len(candidates) > maximumRouteCandidates {
		return nil, errors.New("route candidate count exceeds the safe limit")
	}
	return candidates, nil
}

func scanRouteCandidate(rows *sql.Rows) (RouteCandidate, error) {
	var candidate RouteCandidate
	var description sql.NullString
	var requirementsJSON []byte
	var allowedRegions pq.StringArray
	var capabilitiesJSON []byte
	var secretReferenceID sql.NullString
	err := rows.Scan(
		&candidate.LogicalModel.ID, &candidate.LogicalModel.TenantID, &candidate.LogicalModel.Name,
		&candidate.LogicalModel.DisplayName, &description, &requirementsJSON, &allowedRegions,
		&candidate.LogicalModel.Status, &candidate.LogicalModel.Version, &candidate.LogicalModel.CreatedAt,
		&candidate.LogicalModel.CreatedBy, &candidate.LogicalModel.UpdatedAt, &candidate.LogicalModel.UpdatedBy,
		&candidate.Binding.LogicalModelID, &candidate.Binding.DeploymentID,
		&candidate.Binding.Priority, &candidate.Binding.Weight, &candidate.Binding.Status, &candidate.Binding.Version,
		&candidate.Binding.CreatedAt, &candidate.Binding.CreatedBy, &candidate.Binding.UpdatedAt, &candidate.Binding.UpdatedBy,
		&candidate.Deployment.ID, &candidate.Deployment.ProviderID, &candidate.Deployment.Code,
		&candidate.Deployment.PhysicalModel, &candidate.Deployment.EndpointURL, &candidate.Deployment.Region,
		&capabilitiesJSON, &secretReferenceID,
		&candidate.Deployment.Status, &candidate.Deployment.Version, &candidate.Deployment.CreatedAt,
		&candidate.Deployment.CreatedBy, &candidate.Deployment.UpdatedAt, &candidate.Deployment.UpdatedBy,
		&candidate.Provider.ID, &candidate.Provider.Code, &candidate.Provider.Name, &candidate.Provider.AdapterType,
		&candidate.Provider.Status, &candidate.Provider.Version, &candidate.Provider.CreatedAt,
		&candidate.Provider.CreatedBy, &candidate.Provider.UpdatedAt, &candidate.Provider.UpdatedBy,
	)
	if err != nil {
		return RouteCandidate{}, fmt.Errorf("scan route candidate: %w", err)
	}
	if description.Valid {
		candidate.LogicalModel.Description = &description.String
	}
	if allowedRegions != nil {
		regions := append([]string(nil), allowedRegions...)
		candidate.LogicalModel.AllowedRegions = &regions
	}
	if secretReferenceID.Valid {
		candidate.Deployment.SecretReferenceID = &secretReferenceID.String
	}
	if err := json.Unmarshal(requirementsJSON, &candidate.LogicalModel.RequiredCapabilities); err != nil {
		return RouteCandidate{}, fmt.Errorf("decode route logical model requirements: %w", err)
	}
	if err := json.Unmarshal(capabilitiesJSON, &candidate.Deployment.Capabilities); err != nil {
		return RouteCandidate{}, fmt.Errorf("decode route deployment capabilities: %w", err)
	}
	if err := candidate.Validate(); err != nil {
		return RouteCandidate{}, fmt.Errorf("validate stored route candidate: %w", err)
	}
	return candidate, nil
}
