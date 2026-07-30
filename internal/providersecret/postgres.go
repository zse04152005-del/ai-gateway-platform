package providersecret

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/lib/pq"
)

const referenceColumns = `
	id, provider_id, name, backend, locator, ciphertext, nonce, key_version,
	status, version, created_at, created_by, updated_at, updated_by`

const createReferenceStatement = `
	INSERT INTO app.provider_secret_references (
		id, provider_id, name, backend, locator, ciphertext, nonce, key_version,
		status, version, created_at, created_by, updated_at, updated_by
	) VALUES (
		$1, $2, $3, $4, $5, $6, $7, $8,
		$9, $10, $11, $12, $13, $14
	)
	RETURNING ` + referenceColumns

// PostgresStore persists only envelopes or external locators.
type PostgresStore struct {
	database *sql.DB
}

// NewPostgresStore validates the database handle.
func NewPostgresStore(database *sql.DB) (*PostgresStore, error) {
	if database == nil {
		return nil, errors.New("provider secret database must not be nil")
	}
	return &PostgresStore{database: database}, nil
}

// Create inserts one validated reference.
func (store *PostgresStore) Create(ctx context.Context, record Record) (Record, error) {
	if err := record.Validate(); err != nil {
		return Record{}, err
	}
	created, err := scanRecord(store.database.QueryRowContext(
		ctx,
		createReferenceStatement,
		record.ID,
		record.ProviderID,
		record.Name,
		record.Backend,
		record.Locator,
		record.Ciphertext,
		record.Nonce,
		record.KeyVersion,
		record.Status,
		record.Version,
		record.CreatedAt,
		record.CreatedBy,
		record.UpdatedAt,
		record.UpdatedBy,
	))
	if err != nil {
		return Record{}, mapPostgresError(err)
	}
	return created, nil
}

// Get applies provider scope to every reference lookup.
func (store *PostgresStore) Get(ctx context.Context, locator Locator) (Record, error) {
	record, err := scanRecord(store.database.QueryRowContext(ctx, `
		SELECT `+referenceColumns+`
		FROM app.provider_secret_references
		WHERE provider_id = $1 AND id = $2`, locator.ProviderID, locator.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("query provider secret reference: %w", err)
	}
	return record, nil
}

type rowScanner interface {
	Scan(...any) error
}

func scanRecord(scanner rowScanner) (Record, error) {
	var (
		record     Record
		backend    string
		status     string
		locator    sql.NullString
		keyVersion sql.NullString
	)
	if err := scanner.Scan(
		&record.ID,
		&record.ProviderID,
		&record.Name,
		&backend,
		&locator,
		&record.Ciphertext,
		&record.Nonce,
		&keyVersion,
		&status,
		&record.Version,
		&record.CreatedAt,
		&record.CreatedBy,
		&record.UpdatedAt,
		&record.UpdatedBy,
	); err != nil {
		return Record{}, err
	}
	record.Backend = Backend(backend)
	record.Status = Status(status)
	if locator.Valid {
		record.Locator = &locator.String
	}
	if keyVersion.Valid {
		record.KeyVersion = &keyVersion.String
	}
	return record, nil
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
		return ErrConflict
	default:
		return err
	}
}
