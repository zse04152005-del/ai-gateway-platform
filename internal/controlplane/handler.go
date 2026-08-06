// Package controlplane provides the control-plane HTTP application surface.
package controlplane

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/zse04152005-del/ai-gateway-platform/internal/apierror"
	"github.com/zse04152005-del/ai-gateway-platform/internal/correlation"
)

const defaultVersion = "dev"

// NewHandler creates the management-plane HTTP routes that are available at process bootstrap.
func NewHandler(version string) http.Handler {
	return newHandler(version, nil, nil)
}

// NewHandlerWithVirtualKeys creates the bootstrap routes plus the virtual credential lifecycle API.
func NewHandlerWithVirtualKeys(version string, lifecycle virtualKeyLifecycle) http.Handler {
	return newHandler(version, lifecycle, nil)
}

// NewHandlerWithServices creates the management routes backed by durable
// virtual-key lifecycle and request-cost services.
func NewHandlerWithServices(
	version string,
	lifecycle virtualKeyLifecycle,
	costs requestCostReader,
) http.Handler {
	return newHandler(version, lifecycle, costs)
}

func newHandler(version string, lifecycle virtualKeyLifecycle, costs requestCostReader) http.Handler {
	version = strings.TrimSpace(version)
	if version == "" {
		version = defaultVersion
	}

	methodError := apierror.MustNew(apierror.Definition{
		Status:  http.StatusMethodNotAllowed,
		Code:    "METHOD_NOT_ALLOWED",
		Message: "The HTTP method is not allowed for this resource",
		Type:    "invalid_request_error",
	}, nil)
	notFoundError := apierror.MustNew(apierror.Definition{
		Status:  http.StatusNotFound,
		Code:    "NOT_FOUND",
		Message: "The requested resource was not found",
		Type:    "invalid_request_error",
	}, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("/admin/v1/status", func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			writer.Header().Set("Allow", http.MethodGet)
			apierror.WriteHTTP(writer, methodError, correlation.RequestID(request.Context()), "control_plane_error")
			return
		}
		writeJSON(writer, http.StatusOK, statusResponse{
			Status:  "ok",
			Service: "control-plane",
			Version: version,
		})
	})
	if lifecycle != nil || costs != nil {
		var virtualKeys http.Handler
		if lifecycle != nil {
			virtualKeys = newVirtualKeyHTTPHandler(lifecycle)
		}
		var requestCosts http.Handler
		if costs != nil {
			requestCosts = newRequestCostHTTPHandler(costs)
		}
		mux.Handle("/admin/v1/tenants/", &tenantAdminHTTPHandler{
			virtualKeys: virtualKeys, requestCosts: requestCosts,
		})
	}
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		apierror.WriteHTTP(writer, notFoundError, correlation.RequestID(request.Context()), "control_plane_error")
	})
	return mux
}

type tenantAdminHTTPHandler struct {
	virtualKeys  http.Handler
	requestCosts http.Handler
}

func (handler *tenantAdminHTTPHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if _, ok := parseRequestCostRoute(request.URL.Path); ok {
		if handler != nil && handler.requestCosts != nil {
			handler.requestCosts.ServeHTTP(writer, request)
			return
		}
		writeVirtualKeyError(writer, request, routeNotFoundError(nil))
		return
	}
	if _, ok := parseVirtualKeyRoute(request.URL.Path); ok {
		if handler != nil && handler.virtualKeys != nil {
			handler.virtualKeys.ServeHTTP(writer, request)
			return
		}
	}
	writeVirtualKeyError(writer, request, routeNotFoundError(nil))
}

type statusResponse struct {
	Status  string `json:"status"`
	Service string `json:"service"`
	Version string `json:"version"`
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		http.Error(writer, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	body = append(body, '\n')

	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)
	if _, err := writer.Write(body); err != nil {
		return
	}
}
