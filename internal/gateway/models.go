package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/apierror"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/correlation"
	"github.com/zse04152005-del/ai-gateway-platform/internal/keyauth"
)

const modelOwner = "ai-gateway"

type modelListResponse struct {
	Object string          `json:"object"`
	Data   []modelResponse `json:"data"`
}

type modelResponse struct {
	ID           string   `json:"id"`
	Object       string   `json:"object"`
	OwnedBy      string   `json:"owned_by"`
	Capabilities []string `json:"capabilities"`
}

func newModelsHandler(modelCatalog ModelCatalog) http.Handler {
	methodError := apierror.MustNew(apierror.Definition{
		Status: http.StatusMethodNotAllowed, Code: "METHOD_NOT_ALLOWED",
		Message: "The HTTP method is not allowed for this resource", Type: "invalid_request_error",
	}, nil)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			writer.Header().Set("Allow", http.MethodGet)
			apierror.WriteHTTP(writer, methodError, correlation.RequestID(request.Context()), "gateway_error")
			return
		}

		principal, ok := keyauth.PrincipalFromContext(request.Context())
		if !ok {
			writeCatalogUnavailable(writer, request, errors.New("trusted authentication principal is missing"))
			return
		}
		models, err := modelCatalog.ListAvailable(request.Context(), catalog.Access{
			TenantID: principal.TenantID, ProjectID: principal.ProjectID,
			KeyAllowedModels: principal.AllowedModels,
		})
		if err != nil {
			writeCatalogUnavailable(writer, request, err)
			return
		}

		data := make([]modelResponse, 0, len(models))
		for _, model := range models {
			data = append(data, modelResponse{
				ID: model.Name, Object: "model", OwnedBy: modelOwner,
				Capabilities: append([]string(nil), model.Capabilities...),
			})
		}
		writeModelJSON(writer, modelListResponse{Object: "list", Data: data})
	})
}

func writeCatalogUnavailable(writer http.ResponseWriter, request *http.Request, cause error) {
	apierror.WriteHTTP(writer, apierror.MustNew(apierror.Definition{
		Status: http.StatusServiceUnavailable, Code: "MODEL_CATALOG_UNAVAILABLE",
		Message: "The model catalog is temporarily unavailable", Type: "gateway_error",
		Retryable: true, RetryAfter: time.Second,
	}, cause), correlation.RequestID(request.Context()), "gateway_error")
}

func writeModelJSON(writer http.ResponseWriter, response modelListResponse) {
	body, err := json.Marshal(response)
	if err != nil {
		apierror.WriteHTTP(writer, err, "", "gateway_error")
		return
	}
	body = append(body, '\n')
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(body)
}
