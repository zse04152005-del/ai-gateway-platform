package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zse04152005-del/ai-gateway-platform/internal/apierror"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/keyauth"
)

const (
	gatewayTenantID  = "10000000-0000-4000-8000-000000000001"
	gatewayProjectID = "20000000-0000-4000-8000-000000000001"
)

func TestHandlerListsOnlyCatalogProjectionInTrustedScope(t *testing.T) {
	t.Parallel()
	allowed := []string{"general-chat"}
	authenticator := &stubAuthenticator{principal: keyauth.Principal{
		TenantID: gatewayTenantID, ProjectID: gatewayProjectID, VirtualKeyID: "30000000-0000-4000-8000-000000000001",
		AllowedModels: &allowed,
	}}
	modelCatalog := &stubModelCatalog{models: []catalog.AvailableModel{
		{Name: "general-chat", Capabilities: []string{"chat", "stream"}},
	}}
	handler, err := NewHandler(authenticator, modelCatalog)
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", response.Code, response.Body)
	}
	if authenticator.calls != 1 || modelCatalog.calls != 1 {
		t.Fatalf("auth/catalog calls = %d/%d", authenticator.calls, modelCatalog.calls)
	}
	if modelCatalog.access.TenantID != gatewayTenantID || modelCatalog.access.ProjectID != gatewayProjectID {
		t.Fatalf("catalog access = %+v", modelCatalog.access)
	}
	if modelCatalog.access.KeyAllowedModels == nil || len(*modelCatalog.access.KeyAllowedModels) != 1 || (*modelCatalog.access.KeyAllowedModels)[0] != allowed[0] {
		t.Fatalf("catalog key allowlist = %#v", modelCatalog.access.KeyAllowedModels)
	}
	var body modelListResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode model list: %v", err)
	}
	if body.Object != "list" || len(body.Data) != 1 || body.Data[0].ID != "general-chat" || body.Data[0].OwnedBy != modelOwner {
		t.Fatalf("model list = %+v", body)
	}
	for _, forbidden := range []string{"provider", "deployment", "endpoint", "physical"} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Errorf("response leaked %q: %s", forbidden, response.Body)
		}
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("security headers = %#v", response.Header())
	}
}

func TestHandlerProtectsUnknownDataPlaneRoutesOnly(t *testing.T) {
	t.Parallel()
	authenticator := &stubAuthenticator{principal: validGatewayPrincipal()}
	handler, err := NewHandler(authenticator, &stubModelCatalog{})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	dataPlane := httptest.NewRecorder()
	handler.ServeHTTP(dataPlane, httptest.NewRequest(http.MethodGet, "/v1/not-implemented", nil))
	if dataPlane.Code != http.StatusNotFound || authenticator.calls != 1 {
		t.Fatalf("data-plane status/auth calls = %d/%d", dataPlane.Code, authenticator.calls)
	}

	unknown := httptest.NewRecorder()
	handler.ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "/internal", nil))
	if unknown.Code != http.StatusNotFound || authenticator.calls != 1 {
		t.Fatalf("unknown status/auth calls = %d/%d", unknown.Code, authenticator.calls)
	}
}

func TestModelsHandlerMethodAndFailureSemantics(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		authenticator *stubAuthenticator
		modelCatalog  *stubModelCatalog
		method        string
		wantStatus    int
		wantCode      string
	}{
		{
			name: "method rejected", authenticator: &stubAuthenticator{principal: validGatewayPrincipal()},
			modelCatalog: &stubModelCatalog{}, method: http.MethodPost,
			wantStatus: http.StatusMethodNotAllowed, wantCode: "METHOD_NOT_ALLOWED",
		},
		{
			name: "missing principal fails closed", authenticator: &stubAuthenticator{},
			modelCatalog: &stubModelCatalog{}, method: http.MethodGet,
			wantStatus: http.StatusServiceUnavailable, wantCode: "MODEL_CATALOG_UNAVAILABLE",
		},
		{
			name: "database failure is safe", authenticator: &stubAuthenticator{principal: validGatewayPrincipal()},
			modelCatalog: &stubModelCatalog{err: errors.New("postgres://private-host/catalog")}, method: http.MethodGet,
			wantStatus: http.StatusServiceUnavailable, wantCode: "MODEL_CATALOG_UNAVAILABLE",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			handler, err := NewHandler(test.authenticator, test.modelCatalog)
			if err != nil {
				t.Fatalf("NewHandler() error = %v", err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(test.method, "/v1/models", nil))
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.wantStatus, response.Body)
			}
			var envelope apierror.Envelope
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode error envelope: %v", err)
			}
			if envelope.Error.Code != test.wantCode || strings.Contains(response.Body.String(), "private-host") {
				t.Fatalf("error envelope = %+v; body = %s", envelope.Error, response.Body)
			}
			if test.wantStatus == http.StatusMethodNotAllowed && response.Header().Get("Allow") != http.MethodGet {
				t.Fatalf("Allow = %q", response.Header().Get("Allow"))
			}
		})
	}
}

func TestNewHandlerRejectsNilDependencies(t *testing.T) {
	t.Parallel()
	if _, err := NewHandler(nil, &stubModelCatalog{}); err == nil {
		t.Fatal("NewHandler(nil authenticator) error = nil")
	}
	if _, err := NewHandler(&stubAuthenticator{}, nil); err == nil {
		t.Fatal("NewHandler(nil catalog) error = nil")
	}
}

type stubAuthenticator struct {
	principal keyauth.Principal
	calls     int
}

func (stub *stubAuthenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		stub.calls++
		ctx := request.Context()
		if stub.principal.TenantID != "" {
			ctx = keyauth.WithPrincipal(ctx, stub.principal)
		}
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

type stubModelCatalog struct {
	models []catalog.AvailableModel
	err    error
	access catalog.Access
	calls  int
}

func (stub *stubModelCatalog) ListAvailable(_ context.Context, access catalog.Access) ([]catalog.AvailableModel, error) {
	stub.calls++
	stub.access = access
	if stub.err != nil {
		return nil, stub.err
	}
	return append([]catalog.AvailableModel(nil), stub.models...), nil
}

func validGatewayPrincipal() keyauth.Principal {
	return keyauth.Principal{
		TenantID: gatewayTenantID, ProjectID: gatewayProjectID,
		VirtualKeyID: "30000000-0000-4000-8000-000000000001",
	}
}
