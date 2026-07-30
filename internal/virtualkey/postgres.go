package virtualkey

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
)

const recordColumns = `
	id, tenant_id, project_id, key_prefix, secret_hash, hash_key_version,
	status, expires_at, allowed_models, limits, rotated_from_id,
	rotation_grace_expires_at, revoked_at, revoked_by, version,
	created_at, created_by, updated_at, updated_by`

const createStatement = `
	INSERT INTO app.virtual_api_keys (
		id, tenant_id, project_id, key_prefix, secret_hash, hash_key_version,
		status, expires_at, allowed_models, limits, rotated_from_id,
		rotation_grace_expires_at, revoked_at, revoked_by, version,
		created_at, created_by, updated_at, updated_by
	) VALUES (
		$1, $2, $3, $4, $5, $6,
		$7, $8, $9, $10, $11,
		$12, $13, $14, $15,
		$16, $17, $18, $19
	)
	RETURNING ` + recordColumns

const selectStatement = `
	SELECT ` + recordColumns + `
	FROM app.virtual_api_keys
	WHERE tenant_id = $1 AND project_id = $2 AND id = $3`

// PostgresStore persists lifecycle transitions in PostgreSQL.
type PostgresStore struct {
	database *sql.DB
}

// NewPostgresStore validates the database handle.
func NewPostgresStore(database *sql.DB) (*PostgresStore, error) {
	if database == nil {
		return nil, errors.New("virtual credential database must not be nil")
	}
	return &PostgresStore{database: database}, nil
}

// Create inserts one credential and maps only expected identity collisions to a retryable domain error.
func (store *PostgresStore) Create(ctx context.Context, record Record) (Record, error) {
	created, err := insertRecord(ctx, store.database, record)
	if err != nil {
		return Record{}, mapPostgresError(err)
	}
	return created, nil
}

// Get enforces tenant and project predicates in every lookup.
func (store *PostgresStore) Get(ctx context.Context, locator Locator) (Record, error) {
	record, err := scanRecord(store.database.QueryRowContext(
		ctx,
		selectStatement,
		locator.TenantID,
		locator.ProjectID,
		locator.ID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("query virtual credential: %w", err)
	}
	return record, nil
}

// Rotate locks the source row, moves it into grace, and inserts exactly one inheriting replacement.
func (store *PostgresStore) Rotate(
	ctx context.Context,
	locator Locator,
	replacement Replacement,
	graceExpiresAt time.Time,
	actor string,
	now time.Time,
) (Record, error) {
	transaction, err := store.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Record{}, fmt.Errorf("begin virtual credential rotation: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()

	source, err := scanRecord(transaction.QueryRowContext(
		ctx,
		selectStatement+" FOR UPDATE",
		locator.TenantID,
		locator.ProjectID,
		locator.ID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("lock virtual credential for rotation: %w", err)
	}
	if source.Status == StateRevoked {
		return Record{}, ErrInvalidState
	}
	if source.Status == StateRotating {
		return Record{}, ErrAlreadyRotated
	}
	if source.ExpiresAt != nil && !now.Before(*source.ExpiresAt) {
		return Record{}, ErrExpired
	}

	result, err := transaction.ExecContext(ctx, `
		UPDATE app.virtual_api_keys
		SET status = 'rotating',
		    rotation_grace_expires_at = $1,
		    version = version + 1,
		    updated_at = $2,
		    updated_by = $3
		WHERE tenant_id = $4 AND project_id = $5 AND id = $6
		  AND version = $7 AND status = 'active'`,
		graceExpiresAt,
		now,
		actor,
		locator.TenantID,
		locator.ProjectID,
		locator.ID,
		source.Version,
	)
	if err != nil {
		return Record{}, fmt.Errorf("move virtual credential into rotation grace: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return Record{}, fmt.Errorf("inspect virtual credential rotation update: %w", err)
	}
	if updated != 1 {
		return Record{}, ErrInvalidState
	}

	sourceID := source.ID
	created, err := insertRecord(ctx, transaction, Record{
		ID:             replacement.ID,
		TenantID:       source.TenantID,
		ProjectID:      source.ProjectID,
		Prefix:         replacement.Prefix,
		SecretHash:     replacement.SecretHash,
		HashKeyVersion: replacement.HashKeyVersion,
		Status:         StateActive,
		ExpiresAt:      cloneTime(source.ExpiresAt),
		AllowedModels:  cloneStrings(source.AllowedModels),
		Limits:         cloneLimits(source.Limits),
		RotatedFromID:  &sourceID,
		Version:        1,
		CreatedAt:      now,
		CreatedBy:      actor,
		UpdatedAt:      now,
		UpdatedBy:      actor,
	})
	if err != nil {
		return Record{}, mapPostgresError(err)
	}
	if err := transaction.Commit(); err != nil {
		return Record{}, fmt.Errorf("commit virtual credential rotation: %w", err)
	}
	return created, nil
}

// Revoke permanently disables a credential; repeated calls return the original revocation fact.
func (store *PostgresStore) Revoke(ctx context.Context, locator Locator, actor string, now time.Time) (Record, error) {
	transaction, err := store.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Record{}, fmt.Errorf("begin virtual credential revocation: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()

	record, err := scanRecord(transaction.QueryRowContext(
		ctx,
		selectStatement+" FOR UPDATE",
		locator.TenantID,
		locator.ProjectID,
		locator.ID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("lock virtual credential for revocation: %w", err)
	}
	if record.Status == StateRevoked {
		if err := transaction.Commit(); err != nil {
			return Record{}, fmt.Errorf("commit idempotent virtual credential revocation: %w", err)
		}
		return record, nil
	}

	result, err := transaction.ExecContext(ctx, `
		UPDATE app.virtual_api_keys
		SET status = 'revoked',
		    rotation_grace_expires_at = NULL,
		    revoked_at = $1,
		    revoked_by = $2,
		    version = version + 1,
		    updated_at = $1,
		    updated_by = $2
		WHERE tenant_id = $3 AND project_id = $4 AND id = $5
		  AND version = $6 AND status IN ('active', 'rotating')`,
		now,
		actor,
		locator.TenantID,
		locator.ProjectID,
		locator.ID,
		record.Version,
	)
	if err != nil {
		return Record{}, fmt.Errorf("revoke virtual credential: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return Record{}, fmt.Errorf("inspect virtual credential revocation update: %w", err)
	}
	if updated != 1 {
		return Record{}, ErrInvalidState
	}

	revoked, err := scanRecord(transaction.QueryRowContext(
		ctx,
		selectStatement,
		locator.TenantID,
		locator.ProjectID,
		locator.ID,
	))
	if err != nil {
		return Record{}, fmt.Errorf("reload revoked virtual credential: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return Record{}, fmt.Errorf("commit virtual credential revocation: %w", err)
	}
	return revoked, nil
}

type rowScanner interface {
	Scan(...any) error
}

type rowQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func insertRecord(ctx context.Context, queryer rowQueryer, record Record) (Record, error) {
	limits, err := encodeLimits(record.Limits)
	if err != nil {
		return Record{}, err
	}
	var allowedModels any
	if record.AllowedModels != nil {
		allowedModels = pq.Array(*record.AllowedModels)
	}
	return scanRecord(queryer.QueryRowContext(
		ctx,
		createStatement,
		record.ID,
		record.TenantID,
		record.ProjectID,
		record.Prefix,
		record.SecretHash,
		record.HashKeyVersion,
		record.Status,
		record.ExpiresAt,
		allowedModels,
		limits,
		record.RotatedFromID,
		record.RotationGraceExpiresAt,
		record.RevokedAt,
		record.RevokedBy,
		record.Version,
		record.CreatedAt,
		record.CreatedBy,
		record.UpdatedAt,
		record.UpdatedBy,
	))
}

func scanRecord(scanner rowScanner) (Record, error) {
	var (
		record        Record
		status        string
		expiresAt     sql.NullTime
		allowedModels pq.StringArray
		limitsJSON    []byte
		rotatedFromID sql.NullString
		graceExpires  sql.NullTime
		revokedAt     sql.NullTime
		revokedBy     sql.NullString
	)
	err := scanner.Scan(
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
		&rotatedFromID,
		&graceExpires,
		&revokedAt,
		&revokedBy,
		&record.Version,
		&record.CreatedAt,
		&record.CreatedBy,
		&record.UpdatedAt,
		&record.UpdatedBy,
	)
	if err != nil {
		return Record{}, err
	}
	record.Status = State(status)
	if expiresAt.Valid {
		record.ExpiresAt = &expiresAt.Time
	}
	if allowedModels != nil {
		models := append([]string(nil), allowedModels...)
		record.AllowedModels = &models
	}
	if len(limitsJSON) > 0 {
		if err := json.Unmarshal(limitsJSON, &record.Limits); err != nil {
			return Record{}, fmt.Errorf("decode virtual credential limits: %w", err)
		}
	}
	if rotatedFromID.Valid {
		record.RotatedFromID = &rotatedFromID.String
	}
	if graceExpires.Valid {
		record.RotationGraceExpiresAt = &graceExpires.Time
	}
	if revokedAt.Valid {
		record.RevokedAt = &revokedAt.Time
	}
	if revokedBy.Valid {
		record.RevokedBy = &revokedBy.String
	}
	return record, nil
}

func encodeLimits(limits *Limits) (any, error) {
	if limits == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(limits)
	if err != nil {
		return nil, fmt.Errorf("encode virtual credential limits: %w", err)
	}
	return encoded, nil
}

func mapPostgresError(err error) error {
	var databaseError *pq.Error
	if !errors.As(err, &databaseError) {
		return err
	}
	switch databaseError.Code {
	case "23503":
		return ErrNotFound
	case "23505":
		if databaseError.Constraint == "uq_virtual_api_keys_rotated_from" {
			return ErrAlreadyRotated
		}
		return ErrCollision
	default:
		return err
	}
}
