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

const routePriorityDeploymentID = "64000000-0000-4000-8000-000000000011"

func TestRouteCandidateQueryEnforcesScopeAndPriority(t *testing.T) {
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
	insertModelListDeployment(ctx, t, database, routePriorityDeploymentID, modelListProviderActiveID, "model-a-priority", "model-a-priority-physical")
	_, err = database.ExecContext(ctx, `
		INSERT INTO app.logical_model_deployments (
			logical_model_id, deployment_id, priority, created_by, updated_by
		) VALUES ($1, $2, 10, 'integration:routing', 'integration:routing')`,
		modelListModelAID,
		routePriorityDeploymentID,
	)
	if err != nil {
		t.Fatalf("insert priority route binding: %v", err)
	}

	store, err := catalog.NewPostgresStore(database)
	if err != nil {
		t.Fatalf("catalog.NewPostgresStore() error = %v", err)
	}
	access := catalog.Access{TenantID: modelListTenantOneID, ProjectID: modelListProjectOneID}
	candidates, err := store.ListRouteCandidates(ctx, catalog.RouteQuery{Access: access, LogicalModel: "model-a"})
	if err != nil {
		t.Fatalf("ListRouteCandidates() error = %v", err)
	}
	if len(candidates) != 2 || candidates[0].Deployment.ID != routePriorityDeploymentID || candidates[0].Binding.Priority != 10 ||
		candidates[1].Deployment.ID != modelListDeploymentAID || candidates[1].Binding.Priority != 100 {
		t.Fatalf("route candidates = %+v", candidates)
	}
	for _, candidate := range candidates {
		if candidate.LogicalModel.TenantID != modelListTenantOneID || candidate.Provider.ID != modelListProviderActiveID {
			t.Fatalf("route candidate escaped scope = %+v", candidate)
		}
		if err := candidate.Validate(); err != nil {
			t.Fatalf("route candidate validation error = %v", err)
		}
	}

	deniedModels := []string{"model-b"}
	denied, err := store.ListRouteCandidates(ctx, catalog.RouteQuery{
		Access: catalog.Access{
			TenantID: modelListTenantOneID, ProjectID: modelListProjectOneID,
			KeyAllowedModels: &deniedModels,
		},
		LogicalModel: "model-a",
	})
	if err != nil || len(denied) != 0 {
		t.Fatalf("key narrowed candidates = %+v/%v", denied, err)
	}

	tenantTwo, err := store.ListRouteCandidates(ctx, catalog.RouteQuery{
		Access:       catalog.Access{TenantID: modelListTenantTwoID, ProjectID: modelListProjectTwoID},
		LogicalModel: "model-a",
	})
	if err != nil || len(tenantTwo) != 1 || tenantTwo[0].LogicalModel.TenantID != modelListTenantTwoID {
		t.Fatalf("tenant-two candidates = %+v/%v", tenantTwo, err)
	}
}
