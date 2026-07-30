// Package controlplane provides the control-plane HTTP application surface.
package controlplane

import (
	"encoding/json"
	"net/http"
	"strings"
)

const defaultVersion = "dev"

// NewHandler creates the management-plane HTTP routes that are available at process bootstrap.
func NewHandler(version string) http.Handler {
	version = strings.TrimSpace(version)
	if version == "" {
		version = defaultVersion
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/v1/status", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, statusResponse{
			Status:  "ok",
			Service: "control-plane",
			Version: version,
		})
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
