// Package metering defines the finite usage taxonomy shared by ledger writers,
// pricing, reconciliation, and cost queries.
package metering

import (
	"errors"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
)

var (
	// ErrInvalidTokenType means a usage fact names an unsupported billing dimension.
	ErrInvalidTokenType = errors.New("metering token type is invalid")
	// ErrInvalidSource means a usage fact names an unsupported producer.
	ErrInvalidSource = errors.New("metering source is invalid")
)

// TokenType identifies one independently priced usage dimension. Types do not
// imply additivity: cache may overlap input and reasoning may overlap output.
type TokenType string

// Supported TokenType values cover text, cache, reasoning, audio, and image dimensions.
const (
	TokenTypeInput       TokenType = "input"
	TokenTypeOutput      TokenType = "output"
	TokenTypeCacheRead   TokenType = "cache_read"
	TokenTypeCacheWrite  TokenType = "cache_write"
	TokenTypeReasoning   TokenType = "reasoning"
	TokenTypeAudioInput  TokenType = "audio_input"
	TokenTypeAudioOutput TokenType = "audio_output"
	TokenTypeImageInput  TokenType = "image_input"
	TokenTypeImageOutput TokenType = "image_output"
)

var supportedTokenTypes = [...]TokenType{
	TokenTypeInput,
	TokenTypeOutput,
	TokenTypeCacheRead,
	TokenTypeCacheWrite,
	TokenTypeReasoning,
	TokenTypeAudioInput,
	TokenTypeAudioOutput,
	TokenTypeImageInput,
	TokenTypeImageOutput,
}

// TokenTypes returns the complete stable taxonomy in canonical display order.
func TokenTypes() []TokenType {
	result := make([]TokenType, len(supportedTokenTypes))
	copy(result, supportedTokenTypes[:])
	return result
}

// ParseTokenType accepts only an exact canonical value; callers must not trim
// or case-fold untrusted facts into a different billable dimension.
func ParseTokenType(value string) (TokenType, error) {
	tokenType := TokenType(value)
	if !tokenType.Valid() {
		return "", ErrInvalidTokenType
	}
	return tokenType, nil
}

// Valid reports whether the type belongs to the finite metering taxonomy.
func (tokenType TokenType) Valid() bool {
	for _, candidate := range supportedTokenTypes {
		if tokenType == candidate {
			return true
		}
	}
	return false
}

// Source is the normalized producer taxonomy already enforced at the Provider
// adapter boundary. The alias prevents a second, drifting set of source values.
type Source = adapter.UsageSource

// Supported Source values identify the only producers allowed in the ledger.
const (
	SourceProvider   Source = adapter.UsageSourceProvider
	SourceEstimated  Source = adapter.UsageSourceEstimated
	SourceReconciled Source = adapter.UsageSourceReconciled
	SourceAdjustment Source = adapter.UsageSourceAdjustment
)

var supportedSources = [...]Source{
	SourceProvider,
	SourceEstimated,
	SourceReconciled,
	SourceAdjustment,
}

// Sources returns all supported producers in canonical display order.
func Sources() []Source {
	result := make([]Source, len(supportedSources))
	copy(result, supportedSources[:])
	return result
}

// ParseSource accepts only an exact canonical producer value.
func ParseSource(value string) (Source, error) {
	source := Source(value)
	if !ValidSource(source) {
		return "", ErrInvalidSource
	}
	return source, nil
}

// ValidSource reports whether the producer belongs to the finite taxonomy.
func ValidSource(source Source) bool {
	for _, candidate := range supportedSources {
		if source == candidate {
			return true
		}
	}
	return false
}
