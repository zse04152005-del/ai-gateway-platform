package controlplane

import (
	"bytes"
	"context"
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

const (
	credentialCollectionPath = "/admin/v1/tenants/10000000-0000-4000-8000-000000000001/projects/20000000-0000-4000-8000-000000000001/virtual-keys"
	httpTenantID             = "10000000-0000-4000-8000-000000000001"
	httpProjectID            = "20000000-0000-4000-8000-000000000001"
)

func TestVirtualKeyCreateReturnsPlaintextAndGetReturnsMetadataOnly(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	metadata := virtualKeyTestMetadata(now)
	oneTimeValue := strings.Repeat("x", 48)
	var createCommand virtualkey.CreateCommand
	lifecycle := &stubVirtualKeyLifecycle{
		create: func(_ context.Context, command virtualkey.CreateCommand) (virtualkey.IssuedCredential, error) {
			createCommand = command
			return virtualkey.IssuedCredential{Metadata: metadata, Credential: oneTimeValue}, nil
		},
		get: func(_ context.Context, locator virtualkey.Locator) (virtualkey.Metadata, error) {
			if locator.ID != metadata.ID {
				t.Fatalf("Get() locator = %+v", locator)
			}
			return metadata, nil
		},
	}
	handler := NewHandlerWithVirtualKeys("test", lifecycle)

	createBody := `{"mode":"test","allowed_models":[],"limits":{"rpm":100}}`
	createRequest := httptest.NewRequest(http.MethodPost, credentialCollectionPath, strings.NewReader(createBody))
	createRequest.Header.Set("Content-Type", "application/json")
	createRequest.Header.Set(adminActorHeader, "unit:admin")
	createResponse := httptest.NewRecorder()
	handler.ServeHTTP(createResponse, createRequest)
	if createResponse.Code != http.StatusCreated {
		t.Fatalf("create status = %d; body = %s", createResponse.Code, createResponse.Body)
	}
	if createCommand.Mode != "test" || createCommand.Actor != "unit:admin" || createCommand.AllowedModels == nil || len(*createCommand.AllowedModels) != 0 {
		t.Fatalf("create command = %+v", createCommand)
	}
	if got := createResponse.Header().Get("Location"); got != credentialCollectionPath+"/"+metadata.ID {
		t.Fatalf("Location = %q", got)
	}
	if !bytes.Contains(createResponse.Body.Bytes(), []byte(oneTimeValue)) {
		t.Fatalf("create response omitted one-time value: %s", createResponse.Body)
	}

	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, httptest.NewRequest(http.MethodGet, credentialCollectionPath+"/"+metadata.ID, nil))
	if getResponse.Code != http.StatusOK {
		t.Fatalf("get status = %d; body = %s", getResponse.Code, getResponse.Body)
	}
	for _, forbidden := range []string{oneTimeValue, "credential", "secret_hash"} {
		if bytes.Contains(getResponse.Body.Bytes(), []byte(forbidden)) {
			t.Fatalf("get response contains %q: %s", forbidden, getResponse.Body)
		}
	}
}

func TestVirtualKeyRotateDefaultsGraceAndRevokeUsesActor(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	metadata := virtualKeyTestMetadata(now)
	var rotateCommand virtualkey.RotateCommand
	var revokeCommand virtualkey.RevokeCommand
	lifecycle := &stubVirtualKeyLifecycle{
		rotate: func(_ context.Context, command virtualkey.RotateCommand) (virtualkey.IssuedCredential, error) {
			rotateCommand = command
			return virtualkey.IssuedCredential{Metadata: metadata, Credential: strings.Repeat("y", 48)}, nil
		},
		revoke: func(_ context.Context, command virtualkey.RevokeCommand) (virtualkey.Metadata, error) {
			revokeCommand = command
			return metadata, nil
		},
	}
	handler := NewHandlerWithVirtualKeys("test", lifecycle)
	resourcePath := credentialCollectionPath + "/" + metadata.ID

	rotateRequest := httptest.NewRequest(http.MethodPost, resourcePath+"/rotate", strings.NewReader(`{}`))
	rotateRequest.Header.Set("Content-Type", "application/json")
	rotateRequest.Header.Set(adminActorHeader, "unit:rotate")
	rotateResponse := httptest.NewRecorder()
	handler.ServeHTTP(rotateResponse, rotateRequest)
	if rotateResponse.Code != http.StatusCreated || rotateCommand.GracePeriod != defaultGracePeriod || rotateCommand.Actor != "unit:rotate" {
		t.Fatalf("rotate status/command = %d/%+v; body = %s", rotateResponse.Code, rotateCommand, rotateResponse.Body)
	}

	revokeRequest := httptest.NewRequest(http.MethodPost, resourcePath+"/revoke", nil)
	revokeRequest.Header.Set(adminActorHeader, "unit:revoke")
	revokeResponse := httptest.NewRecorder()
	handler.ServeHTTP(revokeResponse, revokeRequest)
	if revokeResponse.Code != http.StatusOK || revokeCommand.Actor != "unit:revoke" {
		t.Fatalf("revoke status/command = %d/%+v; body = %s", revokeResponse.Code, revokeCommand, revokeResponse.Body)
	}
}

func TestVirtualKeyRoutesRejectUnsafeRequests(t *testing.T) {
	lifecycle := &stubVirtualKeyLifecycle{}
	handler := NewHandlerWithVirtualKeys("test", lifecycle)
	tests := []struct {
		name        string
		method      string
		path        string
		contentType string
		body        string
		wantStatus  int
		wantCode    string
	}{
		{name: "wrong method", method: http.MethodGet, path: credentialCollectionPath, wantStatus: 405, wantCode: "METHOD_NOT_ALLOWED"},
		{name: "unsupported content type", method: http.MethodPost, path: credentialCollectionPath, contentType: "text/plain", body: `{}`, wantStatus: 415, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "unknown JSON field", method: http.MethodPost, path: credentialCollectionPath, contentType: "application/json", body: `{"unknown":true}`, wantStatus: 400, wantCode: "INVALID_JSON"},
		{name: "excessive rotation grace", method: http.MethodPost, path: credentialCollectionPath + "/30000000-0000-4000-8000-000000000001/rotate", contentType: "application/json", body: `{"grace_period_seconds":9223372036854775807}`, wantStatus: 400, wantCode: "INVALID_VIRTUAL_KEY_REQUEST"},
		{name: "unknown action", method: http.MethodPost, path: credentialCollectionPath + "/30000000-0000-4000-8000-000000000001/reset", wantStatus: 404, wantCode: "NOT_FOUND"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.wantStatus, response.Body)
			}
			var envelope apierror.Envelope
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if envelope.Error.Code != test.wantCode {
				t.Fatalf("error code = %q, want %q", envelope.Error.Code, test.wantCode)
			}
		})
	}
}

func TestVirtualKeyLifecycleErrorsAreStableAndSafe(t *testing.T) {
	lifecycle := &stubVirtualKeyLifecycle{
		get: func(context.Context, virtualkey.Locator) (virtualkey.Metadata, error) {
			return virtualkey.Metadata{}, errors.Join(virtualkey.ErrNotFound, errors.New("postgres://private-host/internal"))
		},
	}
	handler := NewHandlerWithVirtualKeys("test", lifecycle)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodGet,
		credentialCollectionPath+"/30000000-0000-4000-8000-000000000001",
		nil,
	))
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "VIRTUAL_KEY_NOT_FOUND") {
		t.Fatalf("response = %d/%s", response.Code, response.Body)
	}
	if strings.Contains(response.Body.String(), "private-host") || strings.Contains(response.Body.String(), "postgres") {
		t.Fatalf("response leaked internal cause: %s", response.Body)
	}
}

type stubVirtualKeyLifecycle struct {
	create func(context.Context, virtualkey.CreateCommand) (virtualkey.IssuedCredential, error)
	get    func(context.Context, virtualkey.Locator) (virtualkey.Metadata, error)
	rotate func(context.Context, virtualkey.RotateCommand) (virtualkey.IssuedCredential, error)
	revoke func(context.Context, virtualkey.RevokeCommand) (virtualkey.Metadata, error)
}

func (stub *stubVirtualKeyLifecycle) Create(ctx context.Context, command virtualkey.CreateCommand) (virtualkey.IssuedCredential, error) {
	if stub.create == nil {
		return virtualkey.IssuedCredential{}, errors.New("unexpected Create call")
	}
	return stub.create(ctx, command)
}

func (stub *stubVirtualKeyLifecycle) Get(ctx context.Context, locator virtualkey.Locator) (virtualkey.Metadata, error) {
	if stub.get == nil {
		return virtualkey.Metadata{}, errors.New("unexpected Get call")
	}
	return stub.get(ctx, locator)
}

func (stub *stubVirtualKeyLifecycle) Rotate(ctx context.Context, command virtualkey.RotateCommand) (virtualkey.IssuedCredential, error) {
	if stub.rotate == nil {
		return virtualkey.IssuedCredential{}, errors.New("unexpected Rotate call")
	}
	return stub.rotate(ctx, command)
}

func (stub *stubVirtualKeyLifecycle) Revoke(ctx context.Context, command virtualkey.RevokeCommand) (virtualkey.Metadata, error) {
	if stub.revoke == nil {
		return virtualkey.Metadata{}, errors.New("unexpected Revoke call")
	}
	return stub.revoke(ctx, command)
}

func virtualKeyTestMetadata(now time.Time) virtualkey.Metadata {
	return virtualkey.Metadata{
		ID: "30000000-0000-4000-8000-000000000001", TenantID: httpTenantID,
		ProjectID: httpProjectID, Prefix: "agw_live_00000001", Status: virtualkey.StateActive,
		EffectiveStatus: virtualkey.EffectiveState(virtualkey.StateActive), Version: 1,
		CreatedAt: now, CreatedBy: "unit:create", UpdatedAt: now, UpdatedBy: "unit:create",
	}
}
