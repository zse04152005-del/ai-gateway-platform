//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
)

func TestHealthProbeCatalogListsOnlyReachableActiveDeployments(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set")
	}
	database, err := sql.Open("postgres", databaseURL)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		t.Fatalf("database.PingContext() error = %v", err)
	}
	cleanupModelListFixtures(t, database)
	t.Cleanup(func() { cleanupModelListFixtures(t, database) })
	seedModelListCatalog(ctx, t, database)

	store, err := catalog.NewPostgresStore(database)
	if err != nil {
		t.Fatalf("catalog.NewPostgresStore() error = %v", err)
	}
	targets, err := store.ListHealthProbeTargets(ctx)
	if err != nil {
		t.Fatalf("ListHealthProbeTargets() error = %v", err)
	}
	assertHealthProbeTargetIDs(t, targets, []string{modelListDeploymentAID, modelListDeploymentBID})
	for _, target := range targets {
		if err := target.Validate(); err != nil {
			t.Fatalf("target validation error = %v", err)
		}
	}

	if _, err := database.ExecContext(ctx, `UPDATE app.tenants SET status = 'suspended' WHERE id = $1`, modelListTenantOneID); err != nil {
		t.Fatalf("suspend tenant one: %v", err)
	}
	targets, err = store.ListHealthProbeTargets(ctx)
	if err != nil {
		t.Fatalf("ListHealthProbeTargets(after tenant suspension) error = %v", err)
	}
	assertHealthProbeTargetIDs(t, targets, []string{modelListDeploymentAID})
}

func assertHealthProbeTargetIDs(t *testing.T, targets []catalog.HealthProbeTarget, want []string) {
	t.Helper()
	if len(targets) != len(want) {
		t.Fatalf("health probe target count = %d, want %d: %+v", len(targets), len(want), targets)
	}
	for index := range want {
		if targets[index].Deployment.ID != want[index] {
			t.Fatalf("health probe target[%d] = %s, want %s", index, targets[index].Deployment.ID, want[index])
		}
	}
}
