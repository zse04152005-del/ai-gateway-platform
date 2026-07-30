package keyauth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/apierror"
	"github.com/zse04152005-del/ai-gateway-platform/internal/correlation"
	"github.com/zse04152005-del/ai-gateway-platform/internal/virtualkey"
)

const (
	maximumAuthorizationLength = 256
	expectedSecretBytes        = 32
)

var credentialPrefixPattern = regexp.MustCompile(`^agw_(live|test)_[a-z0-9]{8,32}$`)

// Store loads one globally unique safe prefix together with its tenant/project states.
type Store interface {
	Lookup(context.Context, string) (Record, error)
}

// Authenticator makes fail-closed virtual credential decisions.
type Authenticator struct {
	store   Store
	keyring *Keyring
	cache   Cache
	clock   func() time.Time
}

// NewAuthenticator validates its dependencies.
func NewAuthenticator(store Store, keyring *Keyring, cache Cache, clock func() time.Time) (*Authenticator, error) {
	if store == nil {
		return nil, errors.New("authentication store must not be nil")
	}
	if keyring == nil {
		return nil, errors.New("authentication keyring must not be nil")
	}
	if cache == nil {
		return nil, errors.New("authentication cache must not be nil")
	}
	if clock == nil {
		return nil, errors.New("authentication clock must not be nil")
	}
	return &Authenticator{store: store, keyring: keyring, cache: cache, clock: clock}, nil
}

// Authenticate verifies one complete credential and returns trusted database identity.
func (authenticator *Authenticator) Authenticate(ctx context.Context, credential string) (Principal, error) {
	prefix, secret, err := parseCredential(credential)
	if err != nil {
		return Principal{}, ErrInvalidCredential
	}
	defer clear(secret)

	record, cached := authenticator.cache.Get(prefix)
	if !cached {
		record, err = authenticator.store.Lookup(ctx, prefix)
		if errors.Is(err, ErrRecordNotFound) {
			authenticator.keyring.Burn(prefix, secret)
			return Principal{}, ErrInvalidCredential
		}
		if err != nil {
			return Principal{}, errors.Join(ErrUnavailable, fmt.Errorf("lookup authentication record: %w", err))
		}
		authenticator.cache.Set(prefix, record)
	}

	if record.Prefix != prefix || len(record.SecretHash) != expectedSecretBytes {
		return Principal{}, errors.Join(ErrUnavailable, errors.New("authentication record is internally inconsistent"))
	}
	matched, versionKnown := authenticator.keyring.Verify(
		record.HashKeyVersion,
		prefix,
		secret,
		record.SecretHash,
	)
	if !versionKnown {
		return Principal{}, errors.Join(ErrUnavailable, ErrUnknownDigestVersion)
	}
	if !matched || !recordUsable(record, authenticator.clock().UTC()) {
		return Principal{}, ErrInvalidCredential
	}
	return record.principal(), nil
}

// Invalidate removes one cached prefix, for revoke/rotate/config invalidation consumers.
func (authenticator *Authenticator) Invalidate(prefix string) {
	if authenticator == nil {
		return
	}
	authenticator.cache.Invalidate(prefix)
}

// Middleware enforces exactly one Bearer header and strips all client-asserted identity headers.
func (authenticator *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		credential, ok := bearerCredential(request.Header.Values("Authorization"))
		if !ok {
			writeAuthenticationError(writer, request, ErrInvalidCredential)
			return
		}
		principal, err := authenticator.Authenticate(request.Context(), credential)
		if err != nil {
			writeAuthenticationError(writer, request, err)
			return
		}

		request.Header.Del("Authorization")
		request.Header.Del("X-Tenant-Id")
		request.Header.Del("X-Project-Id")
		request.Header.Del("X-Virtual-Key-Id")
		writer.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(writer, request.WithContext(WithPrincipal(request.Context(), principal)))
	})
}

func parseCredential(credential string) (string, []byte, error) {
	if len(credential) < 1 || len(credential) > maximumAuthorizationLength || strings.TrimSpace(credential) != credential {
		return "", nil, ErrInvalidCredential
	}
	prefix, encodedSecret, ok := strings.Cut(credential, ".")
	if !ok || strings.Contains(encodedSecret, ".") || !credentialPrefixPattern.MatchString(prefix) {
		return "", nil, ErrInvalidCredential
	}
	secret, err := base64.RawURLEncoding.DecodeString(encodedSecret)
	if err != nil || len(secret) != expectedSecretBytes || base64.RawURLEncoding.EncodeToString(secret) != encodedSecret {
		clear(secret)
		return "", nil, ErrInvalidCredential
	}
	return prefix, secret, nil
}

func bearerCredential(values []string) (string, bool) {
	if len(values) != 1 || len(values[0]) > maximumAuthorizationLength+len("Bearer ") {
		return "", false
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	return parts[1], true
}

func recordUsable(record Record, now time.Time) bool {
	if record.TenantStatus != "active" || record.ProjectStatus != "active" {
		return false
	}
	if record.ExpiresAt != nil && !now.Before(*record.ExpiresAt) {
		return false
	}
	switch record.Status {
	case virtualkey.StateActive:
		return true
	case virtualkey.StateRotating:
		return record.RotationGraceExpiresAt != nil && now.Before(*record.RotationGraceExpiresAt)
	case virtualkey.StateRevoked:
		return false
	default:
		return false
	}
}

func writeAuthenticationError(writer http.ResponseWriter, request *http.Request, cause error) {
	writer.Header().Set("WWW-Authenticate", `Bearer realm="ai-gateway"`)
	if errors.Is(cause, ErrUnavailable) {
		apierror.WriteHTTP(writer, apierror.MustNew(apierror.Definition{
			Status: http.StatusServiceUnavailable, Code: "AUTHENTICATION_UNAVAILABLE",
			Message: "Authentication is temporarily unavailable", Type: "authentication_error",
			Retryable: true, RetryAfter: time.Second,
		}, cause), correlation.RequestID(request.Context()), "gateway_error")
		return
	}
	apierror.WriteHTTP(writer, apierror.MustNew(apierror.Definition{
		Status: http.StatusUnauthorized, Code: "INVALID_API_KEY",
		Message: "The API key is invalid", Type: "authentication_error",
	}, cause), correlation.RequestID(request.Context()), "gateway_error")
}
