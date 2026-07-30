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

const maximumListedModels = 1000

const listAvailableModelsStatement = `
	SELECT logical_model.name, logical_model.required_capabilities
	FROM app.project_logical_models AS project_model
	JOIN app.projects AS project
	  ON project.tenant_id = project_model.tenant_id
	 AND project.id = project_model.project_id
	JOIN app.tenants AS tenant ON tenant.id = project_model.tenant_id
	JOIN app.logical_models AS logical_model
	  ON logical_model.tenant_id = project_model.tenant_id
	 AND logical_model.id = project_model.logical_model_id
	WHERE project_model.tenant_id = $1
	  AND project_model.project_id = $2
	  AND tenant.status = 'active'
	  AND project.status = 'active'
	  AND project_model.status = 'active'
	  AND logical_model.status = 'active'
	  AND ($3::text[] IS NULL OR logical_model.name = ANY($3::text[]))
	  AND EXISTS (
		SELECT 1
		FROM app.logical_model_deployments AS binding
		JOIN app.deployments AS deployment ON deployment.id = binding.deployment_id
		JOIN app.providers AS provider ON provider.id = deployment.provider_id
		WHERE binding.logical_model_id = logical_model.id
		  AND binding.status = 'active'
		  AND deployment.status = 'active'
		  AND provider.status = 'active'
	  )
	ORDER BY logical_model.name, logical_model.id
	LIMIT 1001`

// PostgresStore reads tenant-safe model availability from the authoritative catalog.
type PostgresStore struct {
	database *sql.DB
}

// NewPostgresStore validates the database handle.
func NewPostgresStore(database *sql.DB) (*PostgresStore, error) {
	if database == nil {
		return nil, errors.New("catalog database must not be nil")
	}
	return &PostgresStore{database: database}, nil
}

// ListAvailable intersects project grant, key narrowing policy, record lifecycle, and physical availability.
func (store *PostgresStore) ListAvailable(ctx context.Context, access Access) ([]AvailableModel, error) {
	if ctx == nil {
		return nil, errors.New("catalog list context must not be nil")
	}
	allowedModels, err := validateAccess(access)
	if err != nil {
		return nil, err
	}

	rows, err := store.database.QueryContext(
		ctx,
		listAvailableModelsStatement,
		access.TenantID,
		access.ProjectID,
		allowedModels,
	)
	if err != nil {
		return nil, fmt.Errorf("query available logical models: %w", err)
	}
	defer func() { _ = rows.Close() }()

	models := make([]AvailableModel, 0)
	for rows.Next() {
		var name string
		var requirementsJSON []byte
		if err := rows.Scan(&name, &requirementsJSON); err != nil {
			return nil, fmt.Errorf("scan available logical model: %w", err)
		}
		var requirements CapabilityRequirements
		if err := json.Unmarshal(requirementsJSON, &requirements); err != nil {
			return nil, fmt.Errorf("decode logical model capability requirements: %w", err)
		}
		if err := requirements.Validate(); err != nil {
			return nil, fmt.Errorf("validate stored logical model capability requirements: %w", err)
		}
		models = append(models, AvailableModel{Name: name, Capabilities: requirements.Names()})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate available logical models: %w", err)
	}
	if len(models) > maximumListedModels {
		return nil, errors.New("available logical model count exceeds the safe response limit")
	}
	return models, nil
}

func validateAccess(access Access) (any, error) {
	if err := validateUUID("access.tenant_id", access.TenantID); err != nil {
		return nil, err
	}
	if err := validateUUID("access.project_id", access.ProjectID); err != nil {
		return nil, err
	}
	if access.KeyAllowedModels == nil {
		return nil, nil
	}
	if len(*access.KeyAllowedModels) > 256 {
		return nil, invalid("access.key_allowed_models", "must contain at most 256 entries")
	}
	normalized := make([]string, 0, len(*access.KeyAllowedModels))
	seen := make(map[string]struct{}, len(*access.KeyAllowedModels))
	for _, rawName := range *access.KeyAllowedModels {
		name := strings.ToLower(rawName)
		if rawName != strings.TrimSpace(rawName) || !modelNamePattern.MatchString(name) {
			return nil, invalid("access.key_allowed_models", "contains an invalid model identifier")
		}
		if _, exists := seen[name]; exists {
			return nil, invalid("access.key_allowed_models", "contains a duplicate model identifier")
		}
		seen[name] = struct{}{}
		normalized = append(normalized, name)
	}
	return pq.Array(normalized), nil
}
