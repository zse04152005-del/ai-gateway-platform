//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/lib/pq"
)

const (
	tenantOneID  = "10000000-0000-4000-8000-000000000001"
	tenantTwoID  = "10000000-0000-4000-8000-000000000002"
	projectOneID = "20000000-0000-4000-8000-000000000001"
	projectTwoID = "20000000-0000-4000-8000-000000000002"
	projectTriID = "20000000-0000-4000-8000-000000000003"
)

func TestTenantProjectSchemaConstraints(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		t.Fatalf("database.PingContext() error = %v", err)
	}
	cleanupTenantProjectFixtures(t, database)
	t.Cleanup(func() { cleanupTenantProjectFixtures(t, database) })

	insertTenant(ctx, t, database, tenantOneID, "tenant-one", "Tenant One", "tenant-policy")
	insertTenant(ctx, t, database, tenantTwoID, "tenant-two", "Tenant Two", "")

	_, err = database.ExecContext(ctx, `
        INSERT INTO app.tenants (id, slug, name, created_by, updated_by)
        VALUES ('10000000-0000-4000-8000-000000000003', 'tenant-one', 'Duplicate Slug', 'test', 'test')`)
	expectConstraint(t, err, "tenants_slug_unique")

	_, err = database.ExecContext(ctx, `
        INSERT INTO app.tenants (id, slug, name, status, created_by, updated_by)
        VALUES ('10000000-0000-4000-8000-000000000004', 'tenant-bad', 'Bad Status', 'unknown', 'test', 'test')`)
	expectConstraint(t, err, "tenants_status_valid")

	insertProject(ctx, t, database, projectOneID, tenantOneID, "primary-app", "Primary App", "project-policy")
	insertProject(ctx, t, database, projectTwoID, tenantTwoID, "primary-app", "Primary App", "")

	_, err = database.ExecContext(ctx, `
        INSERT INTO app.projects (id, tenant_id, slug, name, created_by, updated_by)
        VALUES ($1, $2, 'second-app', 'primary app', 'test', 'test')`, projectTriID, tenantOneID)
	expectConstraint(t, err, "uq_projects_tenant_name_ci")

	_, err = database.ExecContext(ctx, `
        INSERT INTO app.projects (id, tenant_id, slug, name, created_by, updated_by)
        VALUES ($1, $2, 'primary-app', 'Different Name', 'test', 'test')`, projectTriID, tenantOneID)
	expectConstraint(t, err, "projects_tenant_slug_unique")

	_, err = database.ExecContext(ctx, `
        INSERT INTO app.projects (id, tenant_id, slug, name, created_by, updated_by)
        VALUES ($1, '10000000-0000-4000-8000-000000000099', 'orphan-app', 'Orphan App', 'test', 'test')`, projectTriID)
	expectConstraint(t, err, "projects_tenant_fk")

	_, err = database.ExecContext(ctx, `
        UPDATE app.projects
        SET status = 'disabled', disabled_at = NULL, version = version + 1, updated_at = CURRENT_TIMESTAMP
        WHERE id = $1`, projectOneID)
	expectConstraint(t, err, "projects_disabled_time_valid")

	_, err = database.ExecContext(ctx, `DELETE FROM app.tenants WHERE id = $1`, tenantOneID)
	expectConstraint(t, err, "projects_tenant_fk")

	var (
		projectVersion int64
		createdAt      time.Time
		updatedAt      time.Time
		projectQuota   sql.NullString
		tenantQuota    sql.NullString
		tenantVersion  int64
		tenantCreated  time.Time
		createdBy      string
		updatedBy      string
	)
	err = database.QueryRowContext(ctx, `
        SELECT p.version, p.created_at, p.updated_at, p.quota_policy_ref,
               t.quota_policy_ref, t.version, t.created_at, p.created_by, p.updated_by
        FROM app.projects p
        JOIN app.tenants t ON t.id = p.tenant_id
        WHERE p.id = $1 AND p.tenant_id = $2`, projectOneID, tenantOneID).Scan(
		&projectVersion, &createdAt, &updatedAt, &projectQuota,
		&tenantQuota, &tenantVersion, &tenantCreated, &createdBy, &updatedBy,
	)
	if err != nil {
		t.Fatalf("query project audit fields: %v", err)
	}
	if projectVersion != 1 || tenantVersion != 1 || createdAt.IsZero() || tenantCreated.IsZero() || updatedAt.Before(createdAt) {
		t.Fatalf("audit/version = project %d/%v/%v, tenant %d/%v", projectVersion, createdAt, updatedAt, tenantVersion, tenantCreated)
	}
	if createdBy != "integration:test" || updatedBy != "integration:test" {
		t.Fatalf("project audit actors = %q, %q", createdBy, updatedBy)
	}
	if projectQuota.String != "project-policy" || tenantQuota.String != "tenant-policy" {
		t.Fatalf("quota references = project %q, tenant %q", projectQuota.String, tenantQuota.String)
	}

	var inheritedQuota sql.NullString
	if err := database.QueryRowContext(ctx, `SELECT quota_policy_ref FROM app.projects WHERE id = $1`, projectTwoID).Scan(&inheritedQuota); err != nil {
		t.Fatalf("query inherited quota reference: %v", err)
	}
	if inheritedQuota.Valid {
		t.Fatalf("project quota override = %q, want NULL tenant inheritance", inheritedQuota.String)
	}
}

func insertTenant(ctx context.Context, t *testing.T, database *sql.DB, id, slug, name, quotaRef string) {
	t.Helper()
	var quota any
	if quotaRef != "" {
		quota = quotaRef
	}
	_, err := database.ExecContext(ctx, `
        INSERT INTO app.tenants (id, slug, name, quota_policy_ref, created_by, updated_by)
        VALUES ($1, $2, $3, $4, 'integration:test', 'integration:test')`, id, slug, name, quota)
	if err != nil {
		t.Fatalf("insert tenant %s: %v", slug, err)
	}
}

func insertProject(ctx context.Context, t *testing.T, database *sql.DB, id, tenantID, slug, name, quotaRef string) {
	t.Helper()
	var quota any
	if quotaRef != "" {
		quota = quotaRef
	}
	_, err := database.ExecContext(ctx, `
        INSERT INTO app.projects (id, tenant_id, slug, name, quota_policy_ref, created_by, updated_by)
        VALUES ($1, $2, $3, $4, $5, 'integration:test', 'integration:test')`, id, tenantID, slug, name, quota)
	if err != nil {
		t.Fatalf("insert project %s: %v", slug, err)
	}
}

func expectConstraint(t *testing.T, err error, constraint string) {
	t.Helper()
	var databaseError *pq.Error
	if !errors.As(err, &databaseError) {
		t.Fatalf("error = %v, want PostgreSQL constraint %q", err, constraint)
	}
	if databaseError.Constraint != constraint {
		t.Fatalf("constraint = %q, want %q; error = %v", databaseError.Constraint, constraint, err)
	}
}

func cleanupTenantProjectFixtures(t *testing.T, database *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := database.ExecContext(ctx, `
        DELETE FROM app.projects
        WHERE id IN ($1, $2, $3) OR tenant_id IN ($4, $5)`,
		projectOneID, projectTwoID, projectTriID, tenantOneID, tenantTwoID,
	); err != nil {
		t.Errorf("cleanup project fixtures: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
        DELETE FROM app.tenants
        WHERE id IN ($1, $2, '10000000-0000-4000-8000-000000000003', '10000000-0000-4000-8000-000000000004')`,
		tenantOneID, tenantTwoID,
	); err != nil {
		t.Errorf("cleanup tenant fixtures: %v", err)
	}
}
