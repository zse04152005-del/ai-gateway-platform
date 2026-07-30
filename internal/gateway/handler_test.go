package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandlerProtectsOnlyDataPlaneRoutes(t *testing.T) {
	authenticator := &stubAuthenticator{}
	handler, err := NewHandler(authenticator)
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	dataPlane := httptest.NewRecorder()
	handler.ServeHTTP(dataPlane, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if dataPlane.Code != http.StatusNotFound || authenticator.calls != 1 {
		t.Fatalf("data-plane status/auth calls = %d/%d", dataPlane.Code, authenticator.calls)
	}

	unknown := httptest.NewRecorder()
	handler.ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "/internal", nil))
	if unknown.Code != http.StatusNotFound || authenticator.calls != 1 {
		t.Fatalf("unknown status/auth calls = %d/%d", unknown.Code, authenticator.calls)
	}
}

func TestNewHandlerRejectsNilAuthenticator(t *testing.T) {
	if _, err := NewHandler(nil); err == nil {
		t.Fatal("NewHandler(nil) error = nil")
	}
}

type stubAuthenticator struct {
	calls int
}

func (stub *stubAuthenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		stub.calls++
		next.ServeHTTP(writer, request)
	})
}
