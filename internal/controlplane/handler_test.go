package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zse04152005-del/ai-gateway-platform/internal/apierror"
)

func TestStatusRoute(t *testing.T) {
	handler := NewHandler("test-version")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/v1/status", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var body statusResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if body.Status != "ok" || body.Service != "control-plane" || body.Version != "test-version" {
		t.Fatalf("response = %+v", body)
	}
}

func TestStatusRouteUsesDevelopmentVersionFallback(t *testing.T) {
	handler := NewHandler("  ")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/v1/status", nil))

	var body statusResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if body.Version != defaultVersion {
		t.Fatalf("version = %q, want %q", body.Version, defaultVersion)
	}
}

func TestStatusRouteRejectsWrongMethodAndUnknownPath(t *testing.T) {
	handler := NewHandler("test")

	wrongMethod := httptest.NewRecorder()
	handler.ServeHTTP(wrongMethod, httptest.NewRequest(http.MethodPost, "/admin/v1/status", nil))
	if wrongMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", wrongMethod.Code)
	}
	if got := wrongMethod.Header().Get("Allow"); got != http.MethodGet {
		t.Fatalf("Allow = %q, want GET", got)
	}
	var methodBody apierror.Envelope
	if err := json.Unmarshal(wrongMethod.Body.Bytes(), &methodBody); err != nil {
		t.Fatalf("decode method error: %v", err)
	}
	if methodBody.Error.Code != "METHOD_NOT_ALLOWED" {
		t.Fatalf("method error = %+v", methodBody)
	}

	unknown := httptest.NewRecorder()
	handler.ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "/admin/v1/unknown", nil))
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown status = %d, want 404", unknown.Code)
	}
	var unknownBody apierror.Envelope
	if err := json.Unmarshal(unknown.Body.Bytes(), &unknownBody); err != nil {
		t.Fatalf("decode unknown error: %v", err)
	}
	if unknownBody.Error.Code != "NOT_FOUND" {
		t.Fatalf("unknown error = %+v", unknownBody)
	}
}
