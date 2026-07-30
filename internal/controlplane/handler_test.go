package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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

	unknown := httptest.NewRecorder()
	handler.ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "/admin/v1/unknown", nil))
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown status = %d, want 404", unknown.Code)
	}
}
