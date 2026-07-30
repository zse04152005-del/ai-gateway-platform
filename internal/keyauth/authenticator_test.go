package keyauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/apierror"
	"github.com/zse04152005-del/ai-gateway-platform/internal/virtualkey"
)

func TestMiddlewareAuthenticatesAndIgnoresForgedIdentityHeaders(t *testing.T) {
	now := time.Date(2026, time.July, 30, 13, 0, 0, 0, time.UTC)
	record, completeValue := authenticationFixture(t, now, "auth-v1")
	store := &stubAuthenticationStore{record: record}
	authenticator := mustAuthenticator(t, store, now, 0)

	var principal Principal
	next := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var ok bool
		principal, ok = PrincipalFromContext(request.Context())
		if !ok {
			t.Fatal("trusted principal missing from context")
		}
		for _, header := range []string{"Authorization", "X-Tenant-Id", "X-Project-Id", "X-Virtual-Key-Id"} {
			if got := request.Header.Get(header); got != "" {
				t.Errorf("downstream header %s = %q, want stripped", header, got)
			}
		}
		writer.WriteHeader(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request.Header.Set("Authorization", "Bearer "+completeValue)
	request.Header.Set("X-Tenant-Id", "attacker-tenant")
	request.Header.Set("X-Project-Id", "attacker-project")
	request.Header.Set("X-Virtual-Key-Id", "attacker-key")
	response := httptest.NewRecorder()
	authenticator.Middleware(next).ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d; body = %s", response.Code, response.Body)
	}
	if principal.TenantID != record.TenantID || principal.ProjectID != record.ProjectID || principal.VirtualKeyID != record.ID {
		t.Fatalf("principal = %+v, want database identity", principal)
	}
	if store.calls != 1 {
		t.Fatalf("store calls = %d, want 1", store.calls)
	}
}

func TestMiddlewareReturnsSameUnauthorizedEnvelopeForCredentialFailures(t *testing.T) {
	now := time.Date(2026, time.July, 30, 13, 0, 0, 0, time.UTC)
	baseRecord, completeValue := authenticationFixture(t, now, "auth-v1")
	expiredAt := now
	graceElapsed := now
	tests := []struct {
		name       string
		header     string
		record     Record
		lookupErr  error
		mutateFull func(string) string
	}{
		{name: "missing"},
		{name: "malformed", header: "Basic abc"},
		{name: "multiple", header: "Bearer one, Bearer two"},
		{name: "wrong secret", header: "Bearer", record: baseRecord, mutateFull: replaceCredentialSecret},
		{name: "unknown prefix", header: "Bearer", lookupErr: ErrRecordNotFound},
		{name: "revoked", header: "Bearer", record: withStatus(baseRecord, virtualkey.StateRevoked)},
		{name: "expired", header: "Bearer", record: withExpiry(baseRecord, &expiredAt)},
		{name: "rotation grace elapsed", header: "Bearer", record: withRotationGrace(baseRecord, &graceElapsed)},
		{name: "tenant suspended", header: "Bearer", record: withTenantStatus(baseRecord, "suspended")},
		{name: "project disabled", header: "Bearer", record: withProjectStatus(baseRecord, "disabled")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &stubAuthenticationStore{record: test.record, err: test.lookupErr}
			authenticator := mustAuthenticator(t, store, now, 0)
			request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if test.header == "Bearer" {
				value := completeValue
				if test.mutateFull != nil {
					value = test.mutateFull(value)
				}
				request.Header.Set("Authorization", "Bearer "+value)
			} else if test.header != "" {
				request.Header.Set("Authorization", test.header)
			}
			response := httptest.NewRecorder()
			authenticator.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("invalid credential reached downstream handler")
			})).ServeHTTP(response, request)
			assertAuthenticationError(t, response, http.StatusUnauthorized, "INVALID_API_KEY")
		})
	}
}

func TestMiddlewareFailsClosedWhenStoreOrDigestVersionUnavailable(t *testing.T) {
	now := time.Date(2026, time.July, 30, 13, 0, 0, 0, time.UTC)
	record, completeValue := authenticationFixture(t, now, "auth-v1")
	tests := []struct {
		name  string
		store *stubAuthenticationStore
	}{
		{name: "database failure", store: &stubAuthenticationStore{err: errors.New("postgres://private-host/internal")}},
		{name: "unknown digest version", store: &stubAuthenticationStore{record: withDigestVersion(record, "retired-v9")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authenticator := mustAuthenticator(t, test.store, now, 0)
			request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			request.Header.Set("Authorization", "Bearer "+completeValue)
			response := httptest.NewRecorder()
			authenticator.Middleware(http.NotFoundHandler()).ServeHTTP(response, request)
			assertAuthenticationError(t, response, http.StatusServiceUnavailable, "AUTHENTICATION_UNAVAILABLE")
			if strings.Contains(response.Body.String(), "private-host") || strings.Contains(response.Body.String(), "retired-v9") {
				t.Fatalf("response leaked internal authentication cause: %s", response.Body)
			}
		})
	}
}

func TestAuthenticationCacheExplicitInvalidationAndTTL(t *testing.T) {
	now := time.Date(2026, time.July, 30, 13, 0, 0, 0, time.UTC)
	current := now
	record, completeValue := authenticationFixture(t, now, "auth-v1")
	store := &stubAuthenticationStore{record: record}
	cache, err := NewMemoryCache(time.Minute, 10, func() time.Time { return current })
	if err != nil {
		t.Fatalf("NewMemoryCache() error = %v", err)
	}
	keyring := mustKeyring(t)
	authenticator, err := NewAuthenticator(store, keyring, cache, func() time.Time { return current })
	if err != nil {
		t.Fatalf("NewAuthenticator() error = %v", err)
	}

	if _, err := authenticator.Authenticate(context.Background(), completeValue); err != nil {
		t.Fatalf("initial Authenticate() error = %v", err)
	}
	store.record.Status = virtualkey.StateRevoked
	if _, err := authenticator.Authenticate(context.Background(), completeValue); err != nil {
		t.Fatalf("cached Authenticate() error = %v, want cached active decision before invalidation", err)
	}
	if store.calls != 1 {
		t.Fatalf("store calls before invalidation = %d, want 1", store.calls)
	}

	authenticator.Invalidate(record.Prefix)
	if _, err := authenticator.Authenticate(context.Background(), completeValue); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("post-invalidation Authenticate() error = %v, want invalid", err)
	}
	if store.calls != 2 {
		t.Fatalf("store calls after invalidation = %d, want 2", store.calls)
	}

	store.record = record
	authenticator.Invalidate(record.Prefix)
	if _, err := authenticator.Authenticate(context.Background(), completeValue); err != nil {
		t.Fatalf("reload active Authenticate() error = %v", err)
	}
	current = current.Add(time.Minute)
	store.record.Status = virtualkey.StateRevoked
	if _, err := authenticator.Authenticate(context.Background(), completeValue); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("post-TTL Authenticate() error = %v, want invalid", err)
	}
	if store.calls != 4 {
		t.Fatalf("store calls after TTL = %d, want 4", store.calls)
	}
}

func TestRetainedKeyringVersionAuthenticates(t *testing.T) {
	now := time.Date(2026, time.July, 30, 13, 0, 0, 0, time.UTC)
	retainedKey := bytes.Repeat([]byte{0x33}, 32)
	prefix := "agw_live_00000001"
	secret := bytes.Repeat([]byte{0x44}, 32)
	digester, err := virtualkey.NewHMACDigester("retained-v1", retainedKey)
	if err != nil {
		t.Fatalf("NewHMACDigester() error = %v", err)
	}
	record := activeAuthenticationRecord(now)
	record.Prefix = prefix
	record.HashKeyVersion = "retained-v1"
	record.SecretHash = digester.Digest(prefix, secret)
	keyring, err := NewKeyring("auth-v1", bytes.Repeat([]byte{0x22}, 32), map[string][]byte{"retained-v1": retainedKey})
	if err != nil {
		t.Fatalf("NewKeyring() error = %v", err)
	}
	cache, err := NewMemoryCache(0, 1, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewMemoryCache() error = %v", err)
	}
	authenticator, err := NewAuthenticator(&stubAuthenticationStore{record: record}, keyring, cache, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewAuthenticator() error = %v", err)
	}
	completeValue := prefix + "." + base64.RawURLEncoding.EncodeToString(secret)
	if _, err := authenticator.Authenticate(context.Background(), completeValue); err != nil {
		t.Fatalf("Authenticate(retained version) error = %v", err)
	}
}

type stubAuthenticationStore struct {
	record Record
	err    error
	calls  int
}

func (store *stubAuthenticationStore) Lookup(_ context.Context, _ string) (Record, error) {
	store.calls++
	if store.err != nil {
		return Record{}, store.err
	}
	return cloneRecord(store.record), nil
}

func mustAuthenticator(t *testing.T, store Store, now time.Time, ttl time.Duration) *Authenticator {
	t.Helper()
	cache, err := NewMemoryCache(ttl, 10, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewMemoryCache() error = %v", err)
	}
	authenticator, err := NewAuthenticator(store, mustKeyring(t), cache, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewAuthenticator() error = %v", err)
	}
	return authenticator
}

func mustKeyring(t *testing.T) *Keyring {
	t.Helper()
	keyring, err := NewKeyring("auth-v1", bytes.Repeat([]byte{0x22}, 32), nil)
	if err != nil {
		t.Fatalf("NewKeyring() error = %v", err)
	}
	return keyring
}

func authenticationFixture(t *testing.T, now time.Time, version string) (Record, string) {
	t.Helper()
	prefix := "agw_live_00000001"
	secret := bytes.Repeat([]byte{0x55}, 32)
	digester, err := virtualkey.NewHMACDigester(version, bytes.Repeat([]byte{0x22}, 32))
	if err != nil {
		t.Fatalf("NewHMACDigester() error = %v", err)
	}
	record := activeAuthenticationRecord(now)
	record.Prefix = prefix
	record.HashKeyVersion = version
	record.SecretHash = digester.Digest(prefix, secret)
	completeValue := prefix + "." + base64.RawURLEncoding.EncodeToString(secret)
	return record, completeValue
}

func activeAuthenticationRecord(now time.Time) Record {
	expiresAt := now.Add(time.Hour)
	models := []string{"chat.default"}
	rpm := int64(100)
	return Record{
		ID: "30000000-0000-4000-8000-000000000001", TenantID: "10000000-0000-4000-8000-000000000001",
		ProjectID: "20000000-0000-4000-8000-000000000001", Prefix: "agw_live_00000001",
		Status: virtualkey.StateActive, ExpiresAt: &expiresAt, AllowedModels: &models,
		Limits: &virtualkey.Limits{RPM: &rpm}, TenantStatus: "active", ProjectStatus: "active",
	}
}

func replaceCredentialSecret(value string) string {
	prefix, _, _ := strings.Cut(value, ".")
	return prefix + "." + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x66}, 32))
}

func withStatus(record Record, status virtualkey.State) Record {
	record.Status = status
	return record
}

func withExpiry(record Record, expiresAt *time.Time) Record {
	record.ExpiresAt = expiresAt
	return record
}

func withRotationGrace(record Record, deadline *time.Time) Record {
	record.Status = virtualkey.StateRotating
	record.RotationGraceExpiresAt = deadline
	return record
}

func withTenantStatus(record Record, status string) Record {
	record.TenantStatus = status
	return record
}

func withProjectStatus(record Record, status string) Record {
	record.ProjectStatus = status
	return record
}

func withDigestVersion(record Record, version string) Record {
	record.HashKeyVersion = version
	return record
}

func assertAuthenticationError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, status, response.Body)
	}
	if got := response.Header().Get("WWW-Authenticate"); got != `Bearer realm="ai-gateway"` {
		t.Fatalf("WWW-Authenticate = %q", got)
	}
	var envelope apierror.Envelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if envelope.Error.Code != code {
		t.Fatalf("error code = %q, want %q", envelope.Error.Code, code)
	}
}
