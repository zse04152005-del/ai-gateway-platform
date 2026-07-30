package openaiadapter

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
)

const maximumErrorBodyBytes = 64 * 1024

var safeProviderRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)

// NormalizeError maps only bounded status and header facts. The raw body is
// deliberately ignored so provider messages and credentials cannot escape.
func (openAI *openAIAdapter) NormalizeError(
	ctx context.Context,
	response *http.Response,
	_ []byte,
) adapter.NormalizedError {
	status := 0
	requestID := ""
	if response != nil {
		status = response.StatusCode
		requestID = safeProviderRequestID(response.Header.Get("X-Request-ID"))
	}
	normalized := classifyProviderError(status)
	normalized.ProviderStatus = status
	normalized.ProviderRequestID = requestID
	if ctx != nil && ctx.Err() != nil {
		normalized = normalizedError("UPSTREAM_CANCELLED", adapter.ErrorCancelled, false, "Upstream request was cancelled")
		normalized.ProviderStatus = status
		normalized.ProviderRequestID = requestID
		return normalized
	}
	if response != nil && normalized.Retryable {
		normalized.RetryAfter = parseRetryAfter(response.Header.Get("Retry-After"), openAINow(openAI))
	}
	if err := normalized.Validate(); err != nil {
		return adapter.NormalizedError{
			Code: "OPENAI_PROTOCOL_ERROR", Category: adapter.ErrorProtocol,
			Retryable: false, ProviderStatus: validProviderStatus(status),
			SafeMessage: "OpenAI returned an invalid error response",
		}
	}
	return normalized
}

func classifyProviderError(status int) adapter.NormalizedError {
	switch status {
	case http.StatusUnauthorized:
		return normalizedError("OPENAI_AUTHENTICATION_FAILED", adapter.ErrorAuth, false, "OpenAI authentication failed")
	case http.StatusForbidden:
		return normalizedError("OPENAI_PERMISSION_DENIED", adapter.ErrorPermission, false, "OpenAI denied the request")
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return normalizedError("OPENAI_TIMEOUT", adapter.ErrorTimeout, true, "OpenAI request timed out")
	case http.StatusTooManyRequests:
		return normalizedError("OPENAI_RATE_LIMITED", adapter.ErrorRateLimit, true, "OpenAI rate limited the request")
	case http.StatusServiceUnavailable:
		return normalizedError("OPENAI_CAPACITY_UNAVAILABLE", adapter.ErrorCapacity, true, "OpenAI is temporarily unavailable")
	case http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed,
		http.StatusConflict, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType,
		http.StatusUnprocessableEntity:
		return normalizedError("OPENAI_INVALID_REQUEST", adapter.ErrorInvalidRequest, false, "OpenAI rejected the request")
	default:
		if status >= 500 && status <= 599 {
			return normalizedError("OPENAI_PROVIDER_FAILED", adapter.ErrorProvider5xx, true, "OpenAI request failed")
		}
		return normalizedError("OPENAI_PROVIDER_ERROR", adapter.ErrorUnknown, false, "OpenAI returned an unknown error")
	}
}

func normalizedError(code string, category adapter.ErrorCategory, retryable bool, message string) adapter.NormalizedError {
	return adapter.NormalizedError{Code: code, Category: category, Retryable: retryable, SafeMessage: message}
}

func parseRetryAfter(value string, now time.Time) *time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if seconds, err := strconv.ParseInt(value, 10, 32); err == nil {
		duration := time.Duration(seconds) * time.Second
		if duration > 0 && duration <= 24*time.Hour {
			return &duration
		}
		return nil
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return nil
	}
	duration := when.Sub(now)
	if duration <= 0 || duration > 24*time.Hour {
		return nil
	}
	return &duration
}

func safeProviderRequestID(value string) string {
	value = strings.TrimSpace(value)
	if !safeProviderRequestIDPattern.MatchString(value) {
		return ""
	}
	return value
}

func openAINow(openAI *openAIAdapter) time.Time {
	if openAI == nil || openAI.now == nil {
		return time.Now().UTC()
	}
	return openAI.now().UTC()
}

func validProviderStatus(status int) int {
	if status >= 100 && status <= 599 {
		return status
	}
	return 0
}
