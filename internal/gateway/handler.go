// Package gateway assembles data-plane application routes.
package gateway

import (
	"context"
	"errors"
	"net/http"

	"github.com/zse04152005-del/ai-gateway-platform/internal/apierror"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/correlation"
)

// Authenticator is implemented by keyauth.Authenticator.
type Authenticator interface {
	Middleware(http.Handler) http.Handler
}

// ModelCatalog resolves only logical models available in one trusted access scope.
type ModelCatalog interface {
	ListAvailable(context.Context, catalog.Access) ([]catalog.AvailableModel, error)
}

// NewHandler protects every /v1 route while leaving unknown non-data-plane paths as safe 404s.
func NewHandler(authenticator Authenticator, modelCatalog ModelCatalog) (http.Handler, error) {
	if authenticator == nil {
		return nil, errors.New("gateway authenticator must not be nil")
	}
	if modelCatalog == nil {
		return nil, errors.New("gateway model catalog must not be nil")
	}
	notFound := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		apierror.WriteHTTP(writer, apierror.MustNew(apierror.Definition{
			Status: http.StatusNotFound, Code: "NOT_FOUND",
			Message: "The requested resource was not found", Type: "invalid_request_error",
		}, nil), correlation.RequestID(request.Context()), "gateway_error")
	})
	mux := http.NewServeMux()
	mux.Handle("/v1/models", authenticator.Middleware(newModelsHandler(modelCatalog)))
	mux.Handle("/v1/chat/completions", authenticator.Middleware(newChatCompletionsHandler()))
	mux.Handle("/v1/", authenticator.Middleware(notFound))
	mux.Handle("/", notFound)
	return mux, nil
}
