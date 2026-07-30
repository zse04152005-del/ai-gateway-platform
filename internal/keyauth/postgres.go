package keyauth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lib/pq"

	"github.com/zse04152005-del/ai-gateway-platform/internal/virtualkey"
)

const lookupStatement = `
	SELECT v.id, v.tenant_id, v.project_id, v.key_prefix,
	       v.secret_hash, v.hash_key_version, v.status, v.expires_at,
	       v.allowed_models, v.limits, v.rotation_grace_expires_at,
	       t.status, p.status
	FROM app.virtual_api_keys AS v
	JOIN app.tenants AS t ON t.id = v.tenant_id
	JOIN app.projects AS p ON p.tenant_id = v.tenant_id AND p.id = v.project_id
	WHERE v.key_prefix = $1`

// PostgresStore uses the globally unique safe prefix and always joins tenant/project state.
type PostgresStore struct {
	database *sql.DB
}

// NewPostgresStore validates the database handle.
func NewPostgresStore(database *sql.DB) (*PostgresStore, error) {
	if database == nil {
		return nil, errors.New("authentication database must not be nil")
	}
	return &PostgresStore{database: database}, nil
}

// Lookup returns the minimum authentication projection.
func (store *PostgresStore) Lookup(ctx context.Context, prefix string) (Record, error) {
	var (
		record        Record
		status        string
		expiresAt     sql.NullTime
		allowedModels pq.StringArray
		limitsJSON    []byte
		graceExpires  sql.NullTime
	)
	err := store.database.QueryRowContext(ctx, lookupStatement, prefix).Scan(
		&record.ID,
		&record.TenantID,
		&record.ProjectID,
		&record.Prefix,
		&record.SecretHash,
		&record.HashKeyVersion,
		&status,
		&expiresAt,
		&allowedModels,
		&limitsJSON,
		&graceExpires,
		&record.TenantStatus,
		&record.ProjectStatus,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrRecordNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("query authentication record: %w", err)
	}
	record.Status = virtualkey.State(status)
	if expiresAt.Valid {
		record.ExpiresAt = &expiresAt.Time
	}
	if allowedModels != nil {
		models := append([]string(nil), allowedModels...)
		record.AllowedModels = &models
	}
	if len(limitsJSON) > 0 {
		if err := json.Unmarshal(limitsJSON, &record.Limits); err != nil {
			return Record{}, fmt.Errorf("decode authentication limits: %w", err)
		}
	}
	if graceExpires.Valid {
		record.RotationGraceExpiresAt = &graceExpires.Time
	}
	return record, nil
}
