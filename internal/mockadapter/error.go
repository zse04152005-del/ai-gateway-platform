package mockadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
)

const maximumErrorBodyBytes = 64 * 1024

var safeProviderRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)

type providerErrorEnvelope struct {
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
}

// NormalizeError maps only bounded status/code/header facts. Provider messages
// and raw bodies are never copied into the normalized error.
func (mock *mockAdapter) NormalizeError(
	ctx context.Context,
	response *http.Response,
	body []byte,
) adapter.NormalizedError {
	status := 0
	requestID := ""
	providerCode := ""
	if response != nil {
		status = response.StatusCode
		requestID = safeProviderRequestID(response.Header.Get("X-Request-ID"))
	}
	if len(body) <= maximumErrorBodyBytes {
		var envelope providerErrorEnvelope
		if err := json.NewDecoder(bytes.NewReader(body)).Decode(&envelope); err == nil {
			providerCode = envelope.Error.Code
		}
	}
	normalized := classifyProviderError(status, providerCode)
	normalized.ProviderStatus = status
	normalized.ProviderRequestID = requestID
	if ctx != nil && ctx.Err() != nil {
		normalized.Code = "UPSTREAM_CANCELLED"
		normalized.Category = adapter.ErrorCancelled
		normalized.Retryable = false
		normalized.RetryAfter = nil
		normalized.SafeMessage = "Upstream request was cancelled"
		return normalized
	}
	if response != nil && normalized.Retryable {
		normalized.RetryAfter = parseRetryAfter(response.Header.Get("Retry-After"), mockNow(mock))
	}
	if err := normalized.Validate(); err != nil {
		return adapter.NormalizedError{
			Code: "MOCK_PROTOCOL_ERROR", Category: adapter.ErrorProtocol,
			Retryable: false, ProviderStatus: validProviderStatus(status),
			SafeMessage: "Mock provider returned an invalid error response",
		}
	}
	return normalized
}

func classifyProviderError(status int, providerCode string) adapter.NormalizedError {
	switch status {
	case http.StatusUnauthorized:
		return normalizedError("MOCK_AUTH_FAILED", adapter.ErrorAuth, false, "Mock provider authentication failed")
	case http.StatusForbidden:
		return normalizedError("MOCK_PERMISSION_DENIED", adapter.ErrorPermission, false, "Mock provider denied the request")
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return normalizedError("MOCK_PROVIDER_TIMEOUT", adapter.ErrorTimeout, true, "Mock provider timed out")
	case http.StatusTooManyRequests:
		return normalizedError("MOCK_RATE_LIMITED", adapter.ErrorRateLimit, true, "Mock provider rate limited the request")
	case http.StatusServiceUnavailable:
		if providerCode == "mock_provider_unavailable" {
			return normalizedError("MOCK_PROVIDER_UNAVAILABLE", adapter.ErrorCapacity, true, "Mock provider is temporarily unavailable")
		}
		return normalizedError("MOCK_PROVIDER_FAILED", adapter.ErrorProvider5xx, true, "Mock provider request failed")
	case http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed,
		http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType:
		return normalizedError("MOCK_INVALID_REQUEST", adapter.ErrorInvalidRequest, false, "Mock provider rejected the request")
	default:
		if status >= 500 && status <= 599 {
			return normalizedError("MOCK_PROVIDER_FAILED", adapter.ErrorProvider5xx, true, "Mock provider request failed")
		}
		return normalizedError("MOCK_PROVIDER_ERROR", adapter.ErrorUnknown, false, "Mock provider returned an unknown error")
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

func mockNow(mock *mockAdapter) time.Time {
	if mock == nil || mock.now == nil {
		return time.Now().UTC()
	}
	return mock.now().UTC()
}

func validProviderStatus(status int) int {
	if status >= 100 && status <= 599 {
		return status
	}
	return 0
}
