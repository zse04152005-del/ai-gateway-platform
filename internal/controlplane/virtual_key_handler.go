package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/apierror"
	"github.com/zse04152005-del/ai-gateway-platform/internal/correlation"
	"github.com/zse04152005-del/ai-gateway-platform/internal/virtualkey"
)

const (
	adminActorHeader   = "X-Admin-Actor"
	maximumRequestBody = 64 << 10
	defaultGracePeriod = 15 * time.Minute
)

type virtualKeyLifecycle interface {
	Create(context.Context, virtualkey.CreateCommand) (virtualkey.IssuedCredential, error)
	Get(context.Context, virtualkey.Locator) (virtualkey.Metadata, error)
	Rotate(context.Context, virtualkey.RotateCommand) (virtualkey.IssuedCredential, error)
	Revoke(context.Context, virtualkey.RevokeCommand) (virtualkey.Metadata, error)
}

type virtualKeyHTTPHandler struct {
	lifecycle virtualKeyLifecycle
}

type createVirtualKeyRequest struct {
	Mode          string             `json:"mode"`
	ExpiresAt     *time.Time         `json:"expires_at"`
	AllowedModels *[]string          `json:"allowed_models"`
	Limits        *virtualkey.Limits `json:"limits"`
}

type rotateVirtualKeyRequest struct {
	GracePeriodSeconds *int64 `json:"grace_period_seconds"`
}

type virtualKeyRoute struct {
	locator    virtualkey.Locator
	action     string
	collection bool
}

func newVirtualKeyHTTPHandler(lifecycle virtualKeyLifecycle) http.Handler {
	return &virtualKeyHTTPHandler{lifecycle: lifecycle}
}

func (handler *virtualKeyHTTPHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	route, ok := parseVirtualKeyRoute(request.URL.Path)
	if !ok {
		writeVirtualKeyError(writer, request, routeNotFoundError(nil))
		return
	}

	if route.collection {
		if request.Method != http.MethodPost {
			writeVirtualKeyMethodError(writer, request, http.MethodPost)
			return
		}
		handler.create(writer, request, route.locator)
		return
	}

	switch route.action {
	case "":
		if request.Method != http.MethodGet {
			writeVirtualKeyMethodError(writer, request, http.MethodGet)
			return
		}
		handler.get(writer, request, route.locator)
	case "rotate":
		if request.Method != http.MethodPost {
			writeVirtualKeyMethodError(writer, request, http.MethodPost)
			return
		}
		handler.rotate(writer, request, route.locator)
	case "revoke":
		if request.Method != http.MethodPost {
			writeVirtualKeyMethodError(writer, request, http.MethodPost)
			return
		}
		handler.revoke(writer, request, route.locator)
	default:
		writeVirtualKeyError(writer, request, routeNotFoundError(nil))
	}
}

func (handler *virtualKeyHTTPHandler) create(writer http.ResponseWriter, request *http.Request, locator virtualkey.Locator) {
	var body createVirtualKeyRequest
	if err := decodeVirtualKeyJSON(writer, request, &body); err != nil {
		writeVirtualKeyError(writer, request, requestBodyError(err))
		return
	}
	if body.Mode == "" {
		body.Mode = "live"
	}
	issued, err := handler.lifecycle.Create(request.Context(), virtualkey.CreateCommand{
		TenantID:      locator.TenantID,
		ProjectID:     locator.ProjectID,
		Mode:          body.Mode,
		ExpiresAt:     body.ExpiresAt,
		AllowedModels: body.AllowedModels,
		Limits:        body.Limits,
		Actor:         request.Header.Get(adminActorHeader),
	})
	if err != nil {
		writeVirtualKeyError(writer, request, lifecycleError(err))
		return
	}
	writer.Header().Set("Location", virtualKeyResourcePath(issued.Metadata))
	writeJSON(writer, http.StatusCreated, issued)
}

func (handler *virtualKeyHTTPHandler) get(writer http.ResponseWriter, request *http.Request, locator virtualkey.Locator) {
	metadata, err := handler.lifecycle.Get(request.Context(), locator)
	if err != nil {
		writeVirtualKeyError(writer, request, lifecycleError(err))
		return
	}
	writeJSON(writer, http.StatusOK, metadata)
}

func (handler *virtualKeyHTTPHandler) rotate(writer http.ResponseWriter, request *http.Request, locator virtualkey.Locator) {
	var body rotateVirtualKeyRequest
	if err := decodeVirtualKeyJSON(writer, request, &body); err != nil {
		writeVirtualKeyError(writer, request, requestBodyError(err))
		return
	}
	gracePeriod := defaultGracePeriod
	if body.GracePeriodSeconds != nil {
		if *body.GracePeriodSeconds < 1 || *body.GracePeriodSeconds > int64((24*time.Hour)/time.Second) {
			writeVirtualKeyError(writer, request, lifecycleError(&virtualkey.ValidationError{
				Field: "grace_period_seconds", Reason: "must be between 1 and 86400",
			}))
			return
		}
		gracePeriod = time.Duration(*body.GracePeriodSeconds) * time.Second
	}
	issued, err := handler.lifecycle.Rotate(request.Context(), virtualkey.RotateCommand{
		Locator: locator, GracePeriod: gracePeriod, Actor: request.Header.Get(adminActorHeader),
	})
	if err != nil {
		writeVirtualKeyError(writer, request, lifecycleError(err))
		return
	}
	writer.Header().Set("Location", virtualKeyResourcePath(issued.Metadata))
	writeJSON(writer, http.StatusCreated, issued)
}

func (handler *virtualKeyHTTPHandler) revoke(writer http.ResponseWriter, request *http.Request, locator virtualkey.Locator) {
	metadata, err := handler.lifecycle.Revoke(request.Context(), virtualkey.RevokeCommand{
		Locator: locator, Actor: request.Header.Get(adminActorHeader),
	})
	if err != nil {
		writeVirtualKeyError(writer, request, lifecycleError(err))
		return
	}
	writeJSON(writer, http.StatusOK, metadata)
}

func parseVirtualKeyRoute(path string) (virtualKeyRoute, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 7 || parts[0] != "admin" || parts[1] != "v1" || parts[2] != "tenants" ||
		parts[4] != "projects" || parts[6] != "virtual-keys" {
		return virtualKeyRoute{}, false
	}
	locator := virtualkey.Locator{TenantID: parts[3], ProjectID: parts[5]}
	switch len(parts) {
	case 7:
		return virtualKeyRoute{locator: locator, collection: true}, true
	case 8:
		locator.ID = parts[7]
		return virtualKeyRoute{locator: locator}, true
	case 9:
		locator.ID = parts[7]
		return virtualKeyRoute{locator: locator, action: parts[8]}, true
	default:
		return virtualKeyRoute{}, false
	}
}

func decodeVirtualKeyJSON(writer http.ResponseWriter, request *http.Request, target any) error {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return errUnsupportedMediaType
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maximumRequestBody)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errMultipleJSONValues
		}
		return err
	}
	return nil
}

var (
	errUnsupportedMediaType = errors.New("content type must be application/json")
	errMultipleJSONValues   = errors.New("request body must contain one JSON value")
)

func requestBodyError(cause error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(cause, &tooLarge) {
		return apierror.MustNew(apierror.Definition{
			Status: http.StatusRequestEntityTooLarge, Code: "REQUEST_TOO_LARGE",
			Message: "The request body is too large", Type: "invalid_request_error",
		}, cause)
	}
	if errors.Is(cause, errUnsupportedMediaType) {
		return apierror.MustNew(apierror.Definition{
			Status: http.StatusUnsupportedMediaType, Code: "UNSUPPORTED_MEDIA_TYPE",
			Message: "Content-Type must be application/json", Type: "invalid_request_error", Param: "Content-Type",
		}, cause)
	}
	return apierror.MustNew(apierror.Definition{
		Status: http.StatusBadRequest, Code: "INVALID_JSON",
		Message: "The request body must be one valid JSON object", Type: "invalid_request_error",
	}, cause)
}

func lifecycleError(cause error) error {
	var validationError *virtualkey.ValidationError
	switch {
	case errors.As(cause, &validationError):
		return apierror.MustNew(apierror.Definition{
			Status: http.StatusBadRequest, Code: "INVALID_VIRTUAL_KEY_REQUEST",
			Message: "The virtual key request is invalid", Type: "invalid_request_error", Param: validationError.Field,
		}, cause)
	case errors.Is(cause, virtualkey.ErrNotFound):
		return apierror.MustNew(apierror.Definition{
			Status: http.StatusNotFound, Code: "VIRTUAL_KEY_NOT_FOUND",
			Message: "The virtual key was not found", Type: "invalid_request_error",
		}, cause)
	case errors.Is(cause, virtualkey.ErrAlreadyRotated):
		return apierror.MustNew(apierror.Definition{
			Status: http.StatusConflict, Code: "VIRTUAL_KEY_ALREADY_ROTATED",
			Message: "The virtual key already has a replacement", Type: "conflict_error",
		}, cause)
	case errors.Is(cause, virtualkey.ErrExpired):
		return apierror.MustNew(apierror.Definition{
			Status: http.StatusConflict, Code: "VIRTUAL_KEY_EXPIRED",
			Message: "An expired virtual key cannot be rotated", Type: "conflict_error",
		}, cause)
	case errors.Is(cause, virtualkey.ErrInvalidState):
		return apierror.MustNew(apierror.Definition{
			Status: http.StatusConflict, Code: "VIRTUAL_KEY_STATE_CONFLICT",
			Message: "The virtual key lifecycle transition conflicts with its current state", Type: "conflict_error",
		}, cause)
	default:
		return cause
	}
}

func writeVirtualKeyMethodError(writer http.ResponseWriter, request *http.Request, allowed string) {
	writer.Header().Set("Allow", allowed)
	writeVirtualKeyError(writer, request, apierror.MustNew(apierror.Definition{
		Status: http.StatusMethodNotAllowed, Code: "METHOD_NOT_ALLOWED",
		Message: "The HTTP method is not allowed for this resource", Type: "invalid_request_error",
	}, nil))
}

func routeNotFoundError(cause error) error {
	return apierror.MustNew(apierror.Definition{
		Status: http.StatusNotFound, Code: "NOT_FOUND",
		Message: "The requested resource was not found", Type: "invalid_request_error",
	}, cause)
}

func writeVirtualKeyError(writer http.ResponseWriter, request *http.Request, err error) {
	apierror.WriteHTTP(writer, err, correlation.RequestID(request.Context()), "control_plane_error")
}

func virtualKeyResourcePath(metadata virtualkey.Metadata) string {
	return "/admin/v1/tenants/" + metadata.TenantID + "/projects/" + metadata.ProjectID + "/virtual-keys/" + metadata.ID
}
