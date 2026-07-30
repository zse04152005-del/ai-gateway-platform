//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/virtualkey"
)

const (
	lifecycleTenantID  = "12000000-0000-4000-8000-000000000001"
	lifecycleProjectID = "22000000-0000-4000-8000-000000000001"
)

func TestVirtualKeyLifecycle(t *testing.T) {
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
	cleanupVirtualKeyLifecycleFixtures(t, database)
	t.Cleanup(func() { cleanupVirtualKeyLifecycleFixtures(t, database) })
	insertTenant(ctx, t, database, lifecycleTenantID, "lifecycle-tenant", "Lifecycle Tenant", "")
	insertProject(ctx, t, database, lifecycleProjectID, lifecycleTenantID, "lifecycle-project", "Lifecycle Project", "")

	store, err := virtualkey.NewPostgresStore(database)
	if err != nil {
		t.Fatalf("NewPostgresStore() error = %v", err)
	}
	digester, err := virtualkey.NewHMACDigester("integration-v1", bytes.Repeat([]byte{0x5a}, 32))
	if err != nil {
		t.Fatalf("NewHMACDigester() error = %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	manager, err := virtualkey.NewManager(store, digester, rand.Reader, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	models := []string{"chat.default", "embed/default-v1"}
	rpm := int64(120)
	concurrency := int64(8)
	expiresAt := now.Add(time.Hour)
	issued, err := manager.Create(ctx, virtualkey.CreateCommand{
		TenantID: lifecycleTenantID, ProjectID: lifecycleProjectID, Mode: "live",
		ExpiresAt: &expiresAt, AllowedModels: &models,
		Limits: &virtualkey.Limits{RPM: &rpm, Concurrency: &concurrency}, Actor: "integration:create",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if issued.Credential == "" {
		t.Fatal("Create() omitted one-time credential")
	}
	assertPlaintextNotPersisted(ctx, t, database, issued.Metadata.ID, issued.Credential)

	metadata, err := manager.Get(ctx, virtualkey.Locator{
		TenantID: lifecycleTenantID, ProjectID: lifecycleProjectID, ID: issued.Metadata.ID,
	})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if metadata.EffectiveStatus != virtualkey.EffectiveState(virtualkey.StateActive) || metadata.Version != 1 {
		t.Fatalf("created metadata = %+v", metadata)
	}

	rotated, err := manager.Rotate(ctx, virtualkey.RotateCommand{
		Locator: virtualkey.Locator{
			TenantID: lifecycleTenantID, ProjectID: lifecycleProjectID, ID: issued.Metadata.ID,
		},
		GracePeriod: 5 * time.Minute,
		Actor:       "integration:rotate",
	})
	if err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	if rotated.Credential == issued.Credential || rotated.Metadata.RotatedFromID == nil || *rotated.Metadata.RotatedFromID != issued.Metadata.ID {
		t.Fatalf("rotated credential metadata = %+v", rotated.Metadata)
	}
	assertPlaintextNotPersisted(ctx, t, database, rotated.Metadata.ID, rotated.Credential)
	assertRotationRows(ctx, t, database, issued.Metadata.ID, rotated.Metadata.ID, models, rpm, concurrency)

	_, err = manager.Rotate(ctx, virtualkey.RotateCommand{
		Locator: virtualkey.Locator{
			TenantID: lifecycleTenantID, ProjectID: lifecycleProjectID, ID: issued.Metadata.ID,
		},
		GracePeriod: time.Minute, Actor: "integration:second-rotate",
	})
	if !errors.Is(err, virtualkey.ErrAlreadyRotated) {
		t.Fatalf("second Rotate() error = %v, want ErrAlreadyRotated", err)
	}

	revoked, err := manager.Revoke(ctx, virtualkey.RevokeCommand{
		Locator: virtualkey.Locator{
			TenantID: lifecycleTenantID, ProjectID: lifecycleProjectID, ID: rotated.Metadata.ID,
		},
		Actor: "integration:revoke",
	})
	if err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}
	if revoked.Status != virtualkey.StateRevoked || revoked.RevokedBy == nil || *revoked.RevokedBy != "integration:revoke" {
		t.Fatalf("revoked metadata = %+v", revoked)
	}
	firstRevokedVersion := revoked.Version
	revokedAgain, err := manager.Revoke(ctx, virtualkey.RevokeCommand{
		Locator: virtualkey.Locator{
			TenantID: lifecycleTenantID, ProjectID: lifecycleProjectID, ID: rotated.Metadata.ID,
		},
		Actor: "integration:different-actor",
	})
	if err != nil {
		t.Fatalf("idempotent Revoke() error = %v", err)
	}
	if revokedAgain.Version != firstRevokedVersion || revokedAgain.RevokedBy == nil || *revokedAgain.RevokedBy != "integration:revoke" {
		t.Fatalf("idempotent revocation changed original fact: %+v", revokedAgain)
	}

	concurrentSource, err := manager.Create(ctx, virtualkey.CreateCommand{
		TenantID: lifecycleTenantID, ProjectID: lifecycleProjectID, Mode: "test", Actor: "integration:concurrent",
	})
	if err != nil {
		t.Fatalf("create concurrent rotation source: %v", err)
	}
	assertExactlyOneConcurrentRotation(ctx, t, database, manager, concurrentSource.Metadata.ID)

	futureManager, err := virtualkey.NewManager(store, digester, rand.Reader, func() time.Time { return expiresAt.Add(time.Second) })
	if err != nil {
		t.Fatalf("NewManager(future) error = %v", err)
	}
	expiredMetadata, err := futureManager.Get(ctx, virtualkey.Locator{
		TenantID: lifecycleTenantID, ProjectID: lifecycleProjectID, ID: issued.Metadata.ID,
	})
	if err != nil {
		t.Fatalf("future Get() error = %v", err)
	}
	if expiredMetadata.EffectiveStatus != virtualkey.EffectiveExpired {
		t.Fatalf("future effective status = %q, want %q", expiredMetadata.EffectiveStatus, virtualkey.EffectiveExpired)
	}
	_, err = futureManager.Rotate(ctx, virtualkey.RotateCommand{
		Locator: virtualkey.Locator{
			TenantID: lifecycleTenantID, ProjectID: lifecycleProjectID, ID: issued.Metadata.ID,
		},
		GracePeriod: time.Minute, Actor: "integration:expired-rotate",
	})
	if !errors.Is(err, virtualkey.ErrExpired) {
		t.Fatalf("expired Rotate() error = %v, want ErrExpired", err)
	}
}

func assertExactlyOneConcurrentRotation(
	ctx context.Context,
	t *testing.T,
	database *sql.DB,
	manager *virtualkey.Manager,
	sourceID string,
) {
	t.Helper()
	start := make(chan struct{})
	results := make(chan error, 2)
	var waitGroup sync.WaitGroup
	for range 2 {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			<-start
			_, err := manager.Rotate(ctx, virtualkey.RotateCommand{
				Locator: virtualkey.Locator{
					TenantID: lifecycleTenantID, ProjectID: lifecycleProjectID, ID: sourceID,
				},
				GracePeriod: time.Minute, Actor: "integration:concurrent-rotate",
			})
			results <- err
		}()
	}
	close(start)
	waitGroup.Wait()
	close(results)

	var succeeded, alreadyRotated int
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, virtualkey.ErrAlreadyRotated):
			alreadyRotated++
		default:
			t.Errorf("concurrent Rotate() unexpected error = %v", err)
		}
	}
	if succeeded != 1 || alreadyRotated != 1 {
		t.Fatalf("concurrent rotations success/already-rotated = %d/%d, want 1/1", succeeded, alreadyRotated)
	}

	var replacementCount int
	if err := database.QueryRowContext(ctx, `
		SELECT count(*) FROM app.virtual_api_keys WHERE rotated_from_id = $1`, sourceID,
	).Scan(&replacementCount); err != nil {
		t.Fatalf("count concurrent replacements: %v", err)
	}
	if replacementCount != 1 {
		t.Fatalf("replacement count = %d, want 1", replacementCount)
	}
}

func assertPlaintextNotPersisted(ctx context.Context, t *testing.T, database *sql.DB, id, plaintext string) {
	t.Helper()
	var (
		digestLength int
		containsText bool
	)
	err := database.QueryRowContext(ctx, `
		SELECT octet_length(secret_hash), to_jsonb(v)::text LIKE '%' || $2 || '%'
		FROM app.virtual_api_keys AS v
		WHERE id = $1`, id, plaintext).Scan(&digestLength, &containsText)
	if err != nil {
		t.Fatalf("inspect persisted virtual credential: %v", err)
	}
	if digestLength != 32 || containsText {
		t.Fatalf("persisted digest length/plaintext-presence = %d/%t", digestLength, containsText)
	}
}

func assertRotationRows(
	ctx context.Context,
	t *testing.T,
	database *sql.DB,
	sourceID string,
	replacementID string,
	wantModels []string,
	wantRPM int64,
	wantConcurrency int64,
) {
	t.Helper()
	var (
		sourceStatus  string
		sourceVersion int64
		graceDeadline time.Time
		rotatedFrom   string
		modelCount    int
		rpm           int64
		concurrency   int64
	)
	err := database.QueryRowContext(ctx, `
		SELECT source.status, source.version, source.rotation_grace_expires_at,
		       replacement.rotated_from_id, cardinality(replacement.allowed_models),
		       (replacement.limits ->> 'rpm')::bigint,
		       (replacement.limits ->> 'concurrency')::bigint
		FROM app.virtual_api_keys AS source
		JOIN app.virtual_api_keys AS replacement
		  ON replacement.tenant_id = source.tenant_id
		 AND replacement.project_id = source.project_id
		 AND replacement.id = $2
		WHERE source.id = $1`, sourceID, replacementID).Scan(
		&sourceStatus, &sourceVersion, &graceDeadline, &rotatedFrom, &modelCount, &rpm, &concurrency,
	)
	if err != nil {
		t.Fatalf("inspect rotation rows: %v", err)
	}
	if sourceStatus != "rotating" || sourceVersion != 2 || graceDeadline.IsZero() || rotatedFrom != sourceID {
		t.Fatalf("rotation source/replacement facts = %s/%d/%v/%s", sourceStatus, sourceVersion, graceDeadline, rotatedFrom)
	}
	if modelCount != len(wantModels) || rpm != wantRPM || concurrency != wantConcurrency {
		t.Fatalf("inherited policy = models:%d rpm:%d concurrency:%d", modelCount, rpm, concurrency)
	}
}

func cleanupVirtualKeyLifecycleFixtures(t *testing.T, database *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := database.ExecContext(ctx, `DELETE FROM app.virtual_api_keys WHERE tenant_id = $1`, lifecycleTenantID); err != nil {
		t.Errorf("cleanup lifecycle virtual credentials: %v", err)
	}
	if _, err := database.ExecContext(ctx, `DELETE FROM app.projects WHERE id = $1`, lifecycleProjectID); err != nil {
		t.Errorf("cleanup lifecycle project: %v", err)
	}
	if _, err := database.ExecContext(ctx, `DELETE FROM app.tenants WHERE id = $1`, lifecycleTenantID); err != nil {
		t.Errorf("cleanup lifecycle tenant: %v", err)
	}
}
