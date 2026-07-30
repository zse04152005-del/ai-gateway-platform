//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/apierror"
	"github.com/zse04152005-del/ai-gateway-platform/internal/keyauth"
	"github.com/zse04152005-del/ai-gateway-platform/internal/virtualkey"
)

const (
	authTenantOneID  = "13000000-0000-4000-8000-000000000001"
	authTenantTwoID  = "13000000-0000-4000-8000-000000000002"
	authProjectOneID = "23000000-0000-4000-8000-000000000001"
	authProjectTwoID = "23000000-0000-4000-8000-000000000002"
)

func TestVirtualKeyAuthentication(t *testing.T) {
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
	cleanupVirtualKeyAuthenticationFixtures(t, database)
	t.Cleanup(func() { cleanupVirtualKeyAuthenticationFixtures(t, database) })
	insertTenant(ctx, t, database, authTenantOneID, "auth-tenant-one", "Auth Tenant One", "")
	insertTenant(ctx, t, database, authTenantTwoID, "auth-tenant-two", "Auth Tenant Two", "")
	insertProject(ctx, t, database, authProjectOneID, authTenantOneID, "auth-project-one", "Auth Project One", "")
	insertProject(ctx, t, database, authProjectTwoID, authTenantTwoID, "auth-project-two", "Auth Project Two", "")

	digestBytes := bytes.Repeat([]byte{0x6a}, 32)
	digester, err := virtualkey.NewHMACDigester("auth-integration-v1", digestBytes)
	if err != nil {
		t.Fatalf("NewHMACDigester() error = %v", err)
	}
	lifecycleStore, err := virtualkey.NewPostgresStore(database)
	if err != nil {
		t.Fatalf("virtualkey.NewPostgresStore() error = %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	current := now
	manager, err := virtualkey.NewManager(lifecycleStore, digester, rand.Reader, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	authenticationStore, err := keyauth.NewPostgresStore(database)
	if err != nil {
		t.Fatalf("keyauth.NewPostgresStore() error = %v", err)
	}
	keyring, err := keyauth.NewKeyring("auth-integration-v1", digestBytes, nil)
	if err != nil {
		t.Fatalf("NewKeyring() error = %v", err)
	}
	cache, err := keyauth.NewMemoryCache(10*time.Minute, 100, func() time.Time { return current })
	if err != nil {
		t.Fatalf("NewMemoryCache() error = %v", err)
	}
	authenticator, err := keyauth.NewAuthenticator(authenticationStore, keyring, cache, func() time.Time { return current })
	if err != nil {
		t.Fatalf("NewAuthenticator() error = %v", err)
	}

	first := issueAuthenticationCredential(ctx, t, manager, nil, "integration:auth-one")
	assertAuthenticatedPrincipal(ctx, t, authenticator, first.Credential, authTenantTwoID, authProjectTwoID)
	assertAuthenticationRejected(ctx, t, authenticator, replaceIntegrationSecret(first.Credential))
	assertAuthenticationRejected(ctx, t, authenticator, "agw_live_00000000."+base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x77}, 32)))

	if _, err := manager.Revoke(ctx, virtualkey.RevokeCommand{
		Locator: virtualkey.Locator{TenantID: authTenantOneID, ProjectID: authProjectOneID, ID: first.Metadata.ID},
		Actor:   "integration:auth-revoke",
	}); err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}
	authenticator.Invalidate(first.Metadata.Prefix)
	assertAuthenticationRejected(ctx, t, authenticator, first.Credential)

	expiresAt := now.Add(time.Hour)
	expiring := issueAuthenticationCredential(ctx, t, manager, &expiresAt, "integration:auth-expiry")
	assertAuthenticatedPrincipal(ctx, t, authenticator, expiring.Credential, authTenantTwoID, authProjectTwoID)
	current = expiresAt
	assertAuthenticationRejected(ctx, t, authenticator, expiring.Credential)
	current = now

	rotating := issueAuthenticationCredential(ctx, t, manager, nil, "integration:auth-rotation")
	if _, err := manager.Rotate(ctx, virtualkey.RotateCommand{
		Locator:     virtualkey.Locator{TenantID: authTenantOneID, ProjectID: authProjectOneID, ID: rotating.Metadata.ID},
		GracePeriod: 5 * time.Minute,
		Actor:       "integration:auth-rotate",
	}); err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	authenticator.Invalidate(rotating.Metadata.Prefix)
	assertAuthenticatedPrincipal(ctx, t, authenticator, rotating.Credential, authTenantTwoID, authProjectTwoID)
	current = now.Add(5 * time.Minute)
	assertAuthenticationRejected(ctx, t, authenticator, rotating.Credential)
	current = now

	scoped := issueAuthenticationCredential(ctx, t, manager, nil, "integration:auth-scope")
	assertAuthenticatedPrincipal(ctx, t, authenticator, scoped.Credential, authTenantTwoID, authProjectTwoID)
	if _, err := database.ExecContext(ctx, `
		UPDATE app.tenants SET status = 'suspended', version = version + 1,
		updated_at = CURRENT_TIMESTAMP, updated_by = 'integration:auth'
		WHERE id = $1`, authTenantOneID); err != nil {
		t.Fatalf("suspend tenant: %v", err)
	}
	authenticator.Invalidate(scoped.Metadata.Prefix)
	assertAuthenticationRejected(ctx, t, authenticator, scoped.Credential)
	if _, err := database.ExecContext(ctx, `
		UPDATE app.tenants SET status = 'active', version = version + 1,
		updated_at = CURRENT_TIMESTAMP, updated_by = 'integration:auth'
		WHERE id = $1`, authTenantOneID); err != nil {
		t.Fatalf("reactivate tenant: %v", err)
	}
	if _, err := database.ExecContext(ctx, `
		UPDATE app.projects SET status = 'disabled', disabled_at = CURRENT_TIMESTAMP,
		version = version + 1, updated_at = CURRENT_TIMESTAMP, updated_by = 'integration:auth'
		WHERE id = $1 AND tenant_id = $2`, authProjectOneID, authTenantOneID); err != nil {
		t.Fatalf("disable project: %v", err)
	}
	authenticator.Invalidate(scoped.Metadata.Prefix)
	assertAuthenticationRejected(ctx, t, authenticator, scoped.Credential)
}

func issueAuthenticationCredential(
	ctx context.Context,
	t *testing.T,
	manager *virtualkey.Manager,
	expiresAt *time.Time,
	actor string,
) virtualkey.IssuedCredential {
	t.Helper()
	issued, err := manager.Create(ctx, virtualkey.CreateCommand{
		TenantID: authTenantOneID, ProjectID: authProjectOneID, Mode: "live",
		ExpiresAt: expiresAt, Actor: actor,
	})
	if err != nil {
		t.Fatalf("Create(%s) error = %v", actor, err)
	}
	return issued
}

func assertAuthenticatedPrincipal(
	ctx context.Context,
	t *testing.T,
	authenticator *keyauth.Authenticator,
	completeValue string,
	forgedTenant string,
	forgedProject string,
) {
	t.Helper()
	var principal keyauth.Principal
	next := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var ok bool
		principal, ok = keyauth.PrincipalFromContext(request.Context())
		if !ok {
			t.Fatal("authenticated principal missing")
		}
		if request.Header.Get("Authorization") != "" || request.Header.Get("X-Tenant-Id") != "" || request.Header.Get("X-Project-Id") != "" {
			t.Fatalf("credential or forged identity header reached downstream")
		}
		writer.WriteHeader(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+completeValue)
	request.Header.Set("X-Tenant-Id", forgedTenant)
	request.Header.Set("X-Project-Id", forgedProject)
	response := httptest.NewRecorder()
	authenticator.Middleware(next).ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("authenticated response = %d/%s", response.Code, response.Body)
	}
	if principal.TenantID != authTenantOneID || principal.ProjectID != authProjectOneID {
		t.Fatalf("principal trusted forged scope: %+v", principal)
	}
}

func assertAuthenticationRejected(ctx context.Context, t *testing.T, authenticator *keyauth.Authenticator, completeValue string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+completeValue)
	response := httptest.NewRecorder()
	authenticator.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("rejected credential reached downstream")
	})).ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("rejected response = %d/%s", response.Code, response.Body)
	}
	var envelope apierror.Envelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode authentication rejection: %v", err)
	}
	if envelope.Error.Code != "INVALID_API_KEY" || envelope.Error.Retryable {
		t.Fatalf("authentication rejection = %+v", envelope.Error)
	}
	if strings.Contains(response.Body.String(), "tenant") || strings.Contains(response.Body.String(), "project") {
		t.Fatalf("authentication rejection leaks scope: %s", response.Body)
	}
}

func replaceIntegrationSecret(completeValue string) string {
	prefix, _, _ := strings.Cut(completeValue, ".")
	return prefix + "." + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x78}, 32))
}

func cleanupVirtualKeyAuthenticationFixtures(t *testing.T, database *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := database.ExecContext(ctx, `DELETE FROM app.virtual_api_keys WHERE tenant_id IN ($1, $2)`, authTenantOneID, authTenantTwoID); err != nil {
		t.Errorf("cleanup authentication credentials: %v", err)
	}
	if _, err := database.ExecContext(ctx, `DELETE FROM app.projects WHERE id IN ($1, $2)`, authProjectOneID, authProjectTwoID); err != nil {
		t.Errorf("cleanup authentication projects: %v", err)
	}
	if _, err := database.ExecContext(ctx, `DELETE FROM app.tenants WHERE id IN ($1, $2)`, authTenantOneID, authTenantTwoID); err != nil {
		t.Errorf("cleanup authentication tenants: %v", err)
	}
}
