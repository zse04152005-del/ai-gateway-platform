// Package providersecret owns provider credential references and resolution boundaries.
package providersecret

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// BackendLocalEnvelope stores AES-GCM envelope fields for local development only.
	BackendLocalEnvelope Backend = "local_envelope"
	// BackendVault resolves an internal vault:// locator through an injected adapter.
	BackendVault Backend = "vault"
	// BackendKMS resolves an internal kms:// locator through an injected adapter.
	BackendKMS Backend = "kms"

	// StatusActive permits credential resolution.
	StatusActive Status = "active"
	// StatusDisabled permanently blocks credential resolution until a managed update.
	StatusDisabled Status = "disabled"

	maximumSecretBytes = 16 * 1024
)

var (
	// ErrNotFound hides whether a provider or secret reference identifier was absent.
	ErrNotFound = errors.New("provider secret reference not found")
	// ErrConflict means the requested reference identity already exists.
	ErrConflict = errors.New("provider secret reference conflicts with an existing record")
	// ErrDisabled means the reference is not eligible for resolution.
	ErrDisabled = errors.New("provider secret reference is disabled")
	// ErrDecryptionFailed deliberately hides key-version and authentication-tag details.
	ErrDecryptionFailed = errors.New("provider secret envelope cannot be decrypted")
	// ErrBackendUnavailable hides locator and external backend diagnostics from callers.
	ErrBackendUnavailable = errors.New("provider secret backend is unavailable")

	uuidPattern       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	namePattern       = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	keyVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
)

// Backend is the storage/resolution implementation selected by one reference.
type Backend string

// Status is the persisted reference lifecycle state.
type Status string

// Locator identifies one secret only within its provider boundary.
type Locator struct {
	ProviderID string
	ID         string
}

// Envelope is the authenticated local ciphertext representation.
type Envelope struct {
	Ciphertext []byte
	Nonce      []byte
	KeyVersion string
}

// Binding is authenticated as AEAD additional data so envelope rows cannot be swapped.
type Binding struct {
	ReferenceID string
	ProviderID  string
	Name        string
}

// Record is the internal persistence model. Sensitive material and locators never serialize.
type Record struct {
	ID         string
	ProviderID string
	Name       string
	Backend    Backend
	Locator    *string `json:"-"`
	Ciphertext []byte  `json:"-"`
	Nonce      []byte  `json:"-"`
	KeyVersion *string `json:"-"`
	Status     Status
	Version    int64
	CreatedAt  time.Time
	CreatedBy  string
	UpdatedAt  time.Time
	UpdatedBy  string
}

// Metadata is safe for control-plane responses and intentionally omits locator and envelope fields.
type Metadata struct {
	ID         string    `json:"id"`
	ProviderID string    `json:"provider_id"`
	Name       string    `json:"name"`
	Backend    Backend   `json:"backend"`
	Status     Status    `json:"status"`
	Version    int64     `json:"version"`
	CreatedAt  time.Time `json:"created_at"`
	CreatedBy  string    `json:"created_by"`
	UpdatedAt  time.Time `json:"updated_at"`
	UpdatedBy  string    `json:"updated_by"`
}

// CreateLocalCommand accepts plaintext only at the encryption boundary.
// Callers retain ownership and should clear Plaintext immediately after the call.
type CreateLocalCommand struct {
	ProviderID string
	Name       string
	Plaintext  []byte `json:"-"`
	Actor      string
}

// RegisterExternalCommand stores only a Vault/KMS locator, never provider plaintext.
type RegisterExternalCommand struct {
	ProviderID string
	Name       string
	Backend    Backend
	Locator    string `json:"-"`
	Actor      string
}

// Store persists only encrypted local envelopes or external locators.
type Store interface {
	Create(context.Context, Record) (Record, error)
	Get(context.Context, Locator) (Record, error)
}

// EnvelopeCipher is the replaceable local envelope boundary.
type EnvelopeCipher interface {
	Encrypt(context.Context, Binding, []byte) (Envelope, error)
	Decrypt(context.Context, Binding, Envelope) ([]byte, error)
}

// ExternalResolver is implemented by future Vault/KMS adapters.
// Implementations must return safe errors that do not echo locator or secret values.
type ExternalResolver interface {
	Resolve(context.Context, string) ([]byte, error)
}

// ValidationError identifies a safe invalid field without echoing its value.
type ValidationError struct {
	Field  string
	Reason string
}

// Error implements error.
func (err *ValidationError) Error() string {
	if err == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s: %s", err.Field, err.Reason)
}

// Metadata returns the non-sensitive public projection.
func (record Record) Metadata() Metadata {
	return Metadata{
		ID: record.ID, ProviderID: record.ProviderID, Name: record.Name,
		Backend: record.Backend, Status: record.Status, Version: record.Version,
		CreatedAt: record.CreatedAt, CreatedBy: record.CreatedBy,
		UpdatedAt: record.UpdatedAt, UpdatedBy: record.UpdatedBy,
	}
}

// Validate checks the complete persistence invariant without inspecting plaintext.
func (record Record) Validate() error {
	if err := validateUUID("reference.id", record.ID); err != nil {
		return err
	}
	if err := validateUUID("reference.provider_id", record.ProviderID); err != nil {
		return err
	}
	if !namePattern.MatchString(record.Name) {
		return invalid("reference.name", "must be a canonical 1-63 character identifier")
	}
	if record.Status != StatusActive && record.Status != StatusDisabled {
		return invalid("reference.status", "must be active or disabled")
	}
	if record.Version <= 0 {
		return invalid("reference.version", "must be positive")
	}
	if record.CreatedAt.IsZero() || record.UpdatedAt.Before(record.CreatedAt) {
		return invalid("reference.timestamps", "must contain ordered non-zero timestamps")
	}
	if err := validateActor("reference.created_by", record.CreatedBy); err != nil {
		return err
	}
	if err := validateActor("reference.updated_by", record.UpdatedBy); err != nil {
		return err
	}

	switch record.Backend {
	case BackendLocalEnvelope:
		if record.Locator != nil || len(record.Ciphertext) < 17 || len(record.Ciphertext) > 65536 || len(record.Nonce) != 12 || record.KeyVersion == nil || !keyVersionPattern.MatchString(*record.KeyVersion) {
			return invalid("reference.envelope", "must contain valid local authenticated ciphertext fields only")
		}
	case BackendVault, BackendKMS:
		if record.Locator == nil || len(record.Ciphertext) != 0 || len(record.Nonce) != 0 || record.KeyVersion != nil {
			return invalid("reference.external", "must contain one external locator and no local envelope")
		}
		if err := validateExternalLocator(record.Backend, *record.Locator); err != nil {
			return err
		}
	default:
		return invalid("reference.backend", "must be local_envelope, vault, or kms")
	}
	return nil
}

func validateCreateLocal(command CreateLocalCommand) error {
	if err := validateUUID("provider_id", command.ProviderID); err != nil {
		return err
	}
	if !namePattern.MatchString(command.Name) {
		return invalid("name", "must be a canonical 1-63 character identifier")
	}
	if len(command.Plaintext) < 1 || len(command.Plaintext) > maximumSecretBytes {
		return invalid("plaintext", "must contain 1-16384 bytes")
	}
	return validateActor("actor", command.Actor)
}

func validateRegisterExternal(command RegisterExternalCommand) error {
	if err := validateUUID("provider_id", command.ProviderID); err != nil {
		return err
	}
	if !namePattern.MatchString(command.Name) {
		return invalid("name", "must be a canonical 1-63 character identifier")
	}
	if command.Backend != BackendVault && command.Backend != BackendKMS {
		return invalid("backend", "must be vault or kms")
	}
	if err := validateExternalLocator(command.Backend, command.Locator); err != nil {
		return err
	}
	return validateActor("actor", command.Actor)
}

func validateExternalLocator(backend Backend, raw string) error {
	if raw != strings.TrimSpace(raw) || len(raw) < 10 || len(raw) > 2048 || strings.ContainsAny(raw, "\r\n\t ") {
		return invalid("locator", "must be a trimmed internal URI without whitespace")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil || parsed.Host == "" || parsed.RawQuery != "" {
		return invalid("locator", "must be an absolute internal URI without userinfo or query")
	}
	if parsed.Scheme != string(backend) {
		return invalid("locator", "scheme must match the selected backend")
	}
	return nil
}

func validateLocator(locator Locator) error {
	if err := validateUUID("provider_id", locator.ProviderID); err != nil {
		return err
	}
	return validateUUID("reference_id", locator.ID)
}

func validateUUID(field, value string) error {
	if !uuidPattern.MatchString(strings.ToLower(value)) {
		return invalid(field, "must be a valid UUID")
	}
	return nil
}

func validateActor(field, value string) error {
	if !utf8.ValidString(value) || value != strings.TrimSpace(value) || utf8.RuneCountInString(value) < 1 || utf8.RuneCountInString(value) > 200 {
		return invalid(field, "must be 1-200 trimmed characters")
	}
	return nil
}

func invalid(field, reason string) error {
	return &ValidationError{Field: field, Reason: reason}
}

// IsValidationError reports whether an error is a safe input failure.
func IsValidationError(err error) bool {
	var validationError *ValidationError
	return errors.As(err, &validationError)
}

func readRandom(reader io.Reader, target []byte) error {
	if reader == nil {
		return errors.New("provider secret random source must not be nil")
	}
	if _, err := io.ReadFull(reader, target); err != nil {
		return errors.New("provider secret random generation failed")
	}
	return nil
}
