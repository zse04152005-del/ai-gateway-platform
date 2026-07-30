// Package apierror separates internal failure causes from stable public HTTP errors.
package apierror

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	defaultInternalCode    = "INTERNAL_ERROR"
	defaultInternalMessage = "An internal error occurred"
	defaultInternalType    = "internal_error"
	maxPublicMessageLength = 256
	maxParamLength         = 128
)

var (
	codePattern  = regexp.MustCompile(`^[A-Z][A-Z0-9_]{2,127}$`)
	typePattern  = regexp.MustCompile(`^[a-z][a-z0-9_]{2,63}$`)
	paramPattern = regexp.MustCompile(`^[A-Za-z0-9_.\[\]-]+$`)
)

// Definition contains only information that is safe and stable for clients.
// It intentionally has no cause, stack, address, provider body, or credentials.
type Definition struct {
	Status     int
	Code       string
	Message    string
	Type       string
	Param      string
	Retryable  bool
	RetryAfter time.Duration
}

// Error keeps a private internal cause next to a validated public definition.
// Error strings are for trusted internal control flow and must never be serialized directly.
type Error struct {
	definition Definition
	cause      error
}

// Detail is the public error schema shared by every HTTP surface.
type Detail struct {
	Code       string  `json:"code"`
	Message    string  `json:"message"`
	Type       string  `json:"type"`
	Param      *string `json:"param"`
	RequestID  string  `json:"request_id"`
	Retryable  bool    `json:"retryable"`
	RetryAfter *int64  `json:"retry_after_ms"`
}

// Envelope is the stable public error response.
type Envelope struct {
	Error Detail `json:"error"`
}

// New validates a public definition and attaches an optional internal cause.
func New(definition Definition, cause error) (*Error, error) {
	definition.Code = strings.TrimSpace(definition.Code)
	definition.Message = strings.TrimSpace(definition.Message)
	definition.Type = strings.TrimSpace(definition.Type)
	definition.Param = strings.TrimSpace(definition.Param)

	if definition.Status < http.StatusBadRequest || definition.Status > 599 {
		return nil, errors.New("public error status must be between 400 and 599")
	}
	if !codePattern.MatchString(definition.Code) {
		return nil, errors.New("public error code must be 3-128 uppercase letters, digits, or underscores")
	}
	if definition.Message == "" || len(definition.Message) > maxPublicMessageLength || strings.ContainsAny(definition.Message, "\r\n") {
		return nil, fmt.Errorf("public error message must be 1-%d bytes without line breaks", maxPublicMessageLength)
	}
	if !typePattern.MatchString(definition.Type) {
		return nil, errors.New("public error type must be 3-64 lowercase letters, digits, or underscores")
	}
	if definition.Param != "" && (len(definition.Param) > maxParamLength || !paramPattern.MatchString(definition.Param)) {
		return nil, fmt.Errorf("public error param must be at most %d safe path characters", maxParamLength)
	}
	if definition.RetryAfter < 0 {
		return nil, errors.New("public error retry-after must not be negative")
	}
	if definition.RetryAfter > 0 && definition.RetryAfter < time.Millisecond {
		return nil, errors.New("public error retry-after must be at least one millisecond")
	}
	if definition.RetryAfter > 0 && !definition.Retryable {
		return nil, errors.New("public error retry-after requires a retryable error")
	}

	return &Error{definition: definition, cause: cause}, nil
}

// MustNew creates a constant application error definition or panics on programmer error.
func MustNew(definition Definition, cause error) *Error {
	applicationError, err := New(definition, cause)
	if err != nil {
		panic(err)
	}
	return applicationError
}

// Error returns trusted diagnostic context. HTTP renderers never use this value.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.cause == nil {
		return e.definition.Code
	}
	return fmt.Sprintf("%s: %v", e.definition.Code, e.cause)
}

// Unwrap makes errors.Is/errors.As work without exposing the cause to clients.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// Render maps an internal error chain to a status and safe public envelope.
// Unknown errors always become a generic 500 response.
func Render(err error, requestID, fallbackType string) (int, Envelope) {
	definition := internalDefinition(fallbackType)
	var applicationError *Error
	if errors.As(err, &applicationError) && applicationError != nil {
		definition = applicationError.definition
	}

	detail := Detail{
		Code:      definition.Code,
		Message:   definition.Message,
		Type:      definition.Type,
		RequestID: requestID,
		Retryable: definition.Retryable,
	}
	if definition.Param != "" {
		value := definition.Param
		detail.Param = &value
	}
	if definition.RetryAfter > 0 {
		milliseconds := definition.RetryAfter.Milliseconds()
		detail.RetryAfter = &milliseconds
	}
	return definition.Status, Envelope{Error: detail}
}

// WriteHTTP renders an error without serializing err.Error() or its cause.
func WriteHTTP(writer http.ResponseWriter, err error, requestID, fallbackType string) {
	status, envelope := Render(err, requestID, fallbackType)
	body, marshalErr := json.Marshal(envelope)
	if marshalErr != nil {
		body = []byte(`{"error":{"code":"INTERNAL_ERROR","message":"An internal error occurred","type":"internal_error","param":null,"request_id":"","retryable":false,"retry_after_ms":null}}`)
		status = http.StatusInternalServerError
	}
	body = append(body, '\n')

	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	if requestID != "" {
		writer.Header().Set("X-Request-Id", requestID)
	}
	if envelope.Error.RetryAfter != nil {
		seconds := (definitionRetryAfter(err) + time.Second - 1) / time.Second
		writer.Header().Set("Retry-After", fmt.Sprintf("%d", seconds))
	}
	writer.WriteHeader(status)
	_, _ = writer.Write(body)
}

func internalDefinition(fallbackType string) Definition {
	fallbackType = strings.TrimSpace(fallbackType)
	if !typePattern.MatchString(fallbackType) {
		fallbackType = defaultInternalType
	}
	return Definition{
		Status:  http.StatusInternalServerError,
		Code:    defaultInternalCode,
		Message: defaultInternalMessage,
		Type:    fallbackType,
	}
}

func definitionRetryAfter(err error) time.Duration {
	var applicationError *Error
	if errors.As(err, &applicationError) && applicationError != nil {
		return applicationError.definition.RetryAfter
	}
	return 0
}
