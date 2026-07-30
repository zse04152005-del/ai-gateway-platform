// Package controlplane provides the control-plane HTTP application surface.
package controlplane

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/zse04152005-del/ai-gateway-platform/internal/apierror"
)

const defaultVersion = "dev"

// NewHandler creates the management-plane HTTP routes that are available at process bootstrap.
func NewHandler(version string) http.Handler {
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
			apierror.WriteHTTP(writer, methodError, "", "control_plane_error")
			return
		}
		writeJSON(writer, http.StatusOK, statusResponse{
			Status:  "ok",
			Service: "control-plane",
			Version: version,
		})
	})
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		apierror.WriteHTTP(writer, notFoundError, "", "control_plane_error")
	})
	return mux
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
