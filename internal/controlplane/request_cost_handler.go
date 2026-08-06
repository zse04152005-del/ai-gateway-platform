package controlplane

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/zse04152005-del/ai-gateway-platform/internal/apierror"
	"github.com/zse04152005-del/ai-gateway-platform/internal/correlation"
	"github.com/zse04152005-del/ai-gateway-platform/internal/meteringcost"
)

type requestCostReader interface {
	Aggregate(context.Context, meteringcost.Scope, string) (meteringcost.RequestCost, error)
}

type requestCostHTTPHandler struct {
	reader requestCostReader
}

type requestCostRoute struct {
	scope     meteringcost.Scope
	requestID string
}

func newRequestCostHTTPHandler(reader requestCostReader) http.Handler {
	return &requestCostHTTPHandler{reader: reader}
}

func (handler *requestCostHTTPHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	route, ok := parseRequestCostRoute(request.URL.Path)
	if !ok || handler == nil || handler.reader == nil {
		writeRequestCostError(writer, request, routeNotFoundError(nil))
		return
	}
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writeRequestCostError(writer, request, apierror.MustNew(apierror.Definition{
			Status: http.StatusMethodNotAllowed, Code: "METHOD_NOT_ALLOWED",
			Message: "The HTTP method is not allowed for this resource", Type: "invalid_request_error",
		}, nil))
		return
	}
	result, err := handler.reader.Aggregate(request.Context(), route.scope, route.requestID)
	if err != nil {
		if errors.Is(err, meteringcost.ErrPending) {
			writer.Header().Set("Retry-After", "1")
		}
		writeRequestCostError(writer, request, requestCostError(err))
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func parseRequestCostRoute(path string) (requestCostRoute, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 9 || parts[0] != "admin" || parts[1] != "v1" || parts[2] != "tenants" ||
		parts[4] != "projects" || parts[6] != "requests" || parts[8] != "cost" ||
		parts[3] == "" || parts[5] == "" || parts[7] == "" {
		return requestCostRoute{}, false
	}
	return requestCostRoute{
		scope:     meteringcost.Scope{TenantID: parts[3], ProjectID: parts[5]},
		requestID: parts[7],
	}, true
}

func requestCostError(cause error) error {
	switch {
	case errors.Is(cause, meteringcost.ErrInvalid):
		return apierror.MustNew(apierror.Definition{
			Status: http.StatusBadRequest, Code: "INVALID_COST_QUERY",
			Message: "The request cost query is invalid", Type: "invalid_request_error",
		}, cause)
	case errors.Is(cause, meteringcost.ErrNotFound):
		return apierror.MustNew(apierror.Definition{
			Status: http.StatusNotFound, Code: "REQUEST_COST_NOT_FOUND",
			Message: "The request cost was not found", Type: "invalid_request_error",
		}, cause)
	case errors.Is(cause, meteringcost.ErrNotTerminal):
		return apierror.MustNew(apierror.Definition{
			Status: http.StatusConflict, Code: "REQUEST_NOT_TERMINAL",
			Message: "The request is not terminal", Type: "conflict_error",
		}, cause)
	case errors.Is(cause, meteringcost.ErrPending):
		return apierror.MustNew(apierror.Definition{
			Status: http.StatusConflict, Code: "REQUEST_COST_PENDING",
			Message: "The request cost is still being finalized", Type: "conflict_error",
		}, cause)
	default:
		return apierror.MustNew(apierror.Definition{
			Status: http.StatusServiceUnavailable, Code: "REQUEST_COST_UNAVAILABLE",
			Message: "The request cost is temporarily unavailable", Type: "server_error",
		}, cause)
	}
}

func writeRequestCostError(writer http.ResponseWriter, request *http.Request, err error) {
	apierror.WriteHTTP(writer, err, correlation.RequestID(request.Context()), "control_plane_error")
}
