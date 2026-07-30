// Package adapterconformance provides the reusable, real-HTTP protocol test
// suite that every provider adapter must pass before it can be published.
package adapterconformance

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/provideradapter"
)

var (
	// ErrInvalidRegistration means an adapter omitted or contradicted a
	// mandatory conformance fixture.
	ErrInvalidRegistration = errors.New("adapter conformance registration is invalid")
	fixtureNamePattern     = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
)

// AdapterBuilder binds an adapter to the isolated HTTP fixture endpoint.
// Implementations normally construct their existing deployment-scoped Factory;
// the conformance package never reaches into an adapter's private parser.
type AdapterBuilder func(ctx context.Context, endpoint string) (provideradapter.Adapter, error)

// HandlerFactory returns a fresh provider-protocol fixture handler. A fresh
// handler prevents state from leaking between parallel conformance cases.
type HandlerFactory func() http.Handler

// CancellationHandlerFactory creates a streaming fixture that flushes response
// headers, blocks on the HTTP request context, and closes cancelled when the
// adapter releases the upstream response body.
type CancellationHandlerFactory func(cancelled chan<- struct{}) http.Handler

// ResponseFixture describes one non-streaming provider exchange and its exact
// normalized result. ObservedAt must be non-zero but is not compared to wall
// clock time by the suite.
type ResponseFixture struct {
	Name       string
	Request    adapter.NormalizedRequest
	NewHandler HandlerFactory
	Want       adapter.NormalizedResponse
}

// StreamFixture describes one complete streaming exchange. The suite reads to
// EOF, checks every normalized chunk, and verifies monotonic sequence numbers.
type StreamFixture struct {
	Name       string
	Request    adapter.NormalizedRequest
	NewHandler HandlerFactory
	Want       []adapter.NormalizedChunk
}

// ErrorFixture describes one non-2xx provider exchange normalized through the
// ordinary adapter entry point. ForbiddenText is synthetic sensitive content
// that must not appear in the returned error or safe normalized facts.
type ErrorFixture struct {
	Name          string
	Request       adapter.NormalizedRequest
	NewHandler    HandlerFactory
	Want          adapter.NormalizedError
	ForbiddenText []string
}

// ProtocolErrorFixture describes malformed or drifted non-streaming provider
// data that must fail closed with a registered sentinel.
type ProtocolErrorFixture struct {
	Name          string
	Request       adapter.NormalizedRequest
	NewHandler    HandlerFactory
	Want          error
	ForbiddenText []string
}

// CancellationFixture verifies that cancelling a blocked ChunkStream.Next
// closes the HTTP response body and propagates cancellation to the provider.
type CancellationFixture struct {
	Name       string
	Request    adapter.NormalizedRequest
	NewHandler CancellationHandlerFactory
}

// FixtureSet is the mandatory provider-protocol conformance matrix. Explicit
// fields prevent an adapter from silently skipping a costly or security-relevant
// behavior by returning an empty slice.
type FixtureSet struct {
	Ordinary        ResponseFixture
	Stream          StreamFixture
	Cancellation    CancellationFixture
	RateLimit       ErrorFixture
	ProviderFailure ErrorFixture
	CachedUsage     ResponseFixture
	ToolCall        ResponseFixture
	FinishReasons   []ResponseFixture
	UnknownOrdinary ProtocolErrorFixture
	UnknownStream   StreamFixture
}

// Registration is the only adapter-specific input to the shared suite.
type Registration struct {
	Name       string
	NewAdapter AdapterBuilder
	Fixtures   FixtureSet
}

// Validate rejects incomplete registrations before any HTTP server starts.
func (registration Registration) Validate() error {
	if !fixtureNamePattern.MatchString(registration.Name) {
		return registrationError("name must be a canonical 1-64 character identifier")
	}
	if registration.NewAdapter == nil {
		return registrationError("adapter builder must be present")
	}

	names := make(map[string]struct{})
	responses := []struct {
		label   string
		fixture ResponseFixture
	}{
		{label: "ordinary", fixture: registration.Fixtures.Ordinary},
		{label: "cached_usage", fixture: registration.Fixtures.CachedUsage},
		{label: "tool_call", fixture: registration.Fixtures.ToolCall},
	}
	for _, candidate := range responses {
		if err := validateResponseFixture(candidate.label, candidate.fixture, names); err != nil {
			return err
		}
	}
	for index, fixture := range registration.Fixtures.FinishReasons {
		if err := validateResponseFixture(fmt.Sprintf("finish_reasons[%d]", index), fixture, names); err != nil {
			return err
		}
	}
	if err := validateFinishReasonCoverage(registration.Fixtures); err != nil {
		return err
	}

	for _, candidate := range []struct {
		label   string
		fixture StreamFixture
	}{
		{label: "stream", fixture: registration.Fixtures.Stream},
		{label: "unknown_stream", fixture: registration.Fixtures.UnknownStream},
	} {
		if err := validateStreamFixture(candidate.label, candidate.fixture, names); err != nil {
			return err
		}
	}
	if !containsChunkKind(registration.Fixtures.UnknownStream.Want, adapter.ChunkProviderExtension) {
		return registrationError("unknown_stream must produce a provider_extension chunk")
	}

	for _, candidate := range []struct {
		label   string
		fixture ErrorFixture
	}{
		{label: "rate_limit", fixture: registration.Fixtures.RateLimit},
		{label: "provider_failure", fixture: registration.Fixtures.ProviderFailure},
	} {
		if err := validateErrorFixture(candidate.label, candidate.fixture, names); err != nil {
			return err
		}
	}
	if registration.Fixtures.RateLimit.Want.Category != adapter.ErrorRateLimit ||
		!registration.Fixtures.RateLimit.Want.Retryable {
		return registrationError("rate_limit must be a retryable rate_limit error")
	}
	providerCategory := registration.Fixtures.ProviderFailure.Want.Category
	if (providerCategory != adapter.ErrorCapacity && providerCategory != adapter.ErrorProvider5xx) ||
		!registration.Fixtures.ProviderFailure.Want.Retryable {
		return registrationError("provider_failure must be retryable capacity or provider_5xx")
	}

	if err := validateCancellationFixture(registration.Fixtures.Cancellation, names); err != nil {
		return err
	}
	if err := validateProtocolFixture("unknown_ordinary", registration.Fixtures.UnknownOrdinary, names); err != nil {
		return err
	}
	return validateSemanticCoverage(registration.Fixtures)
}

func validateResponseFixture(label string, fixture ResponseFixture, names map[string]struct{}) error {
	if err := validateFixtureBase(label, fixture.Name, fixture.Request, fixture.NewHandler, false, names); err != nil {
		return err
	}
	if err := fixture.Want.Validate(); err != nil {
		return registrationError(label + " expected response is invalid: " + err.Error())
	}
	return nil
}

func validateStreamFixture(label string, fixture StreamFixture, names map[string]struct{}) error {
	if err := validateFixtureBase(label, fixture.Name, fixture.Request, fixture.NewHandler, true, names); err != nil {
		return err
	}
	if len(fixture.Want) == 0 {
		return registrationError(label + " expected chunks must not be empty")
	}
	for index, chunk := range fixture.Want {
		if chunk.Sequence != uint64(index) {
			return registrationError(fmt.Sprintf("%s expected chunk %d has a non-monotonic sequence", label, index))
		}
		if err := chunk.Validate(); err != nil {
			return registrationError(fmt.Sprintf("%s expected chunk %d is invalid: %s", label, index, err))
		}
	}
	return nil
}

func validateErrorFixture(label string, fixture ErrorFixture, names map[string]struct{}) error {
	if err := validateFixtureBase(label, fixture.Name, fixture.Request, fixture.NewHandler, false, names); err != nil {
		return err
	}
	if err := fixture.Want.Validate(); err != nil {
		return registrationError(label + " expected normalized error is invalid: " + err.Error())
	}
	return validateForbiddenText(label, fixture.ForbiddenText)
}

func validateProtocolFixture(label string, fixture ProtocolErrorFixture, names map[string]struct{}) error {
	if err := validateFixtureBase(label, fixture.Name, fixture.Request, fixture.NewHandler, false, names); err != nil {
		return err
	}
	if fixture.Want == nil {
		return registrationError(label + " expected error sentinel must be present")
	}
	return validateForbiddenText(label, fixture.ForbiddenText)
}

func validateCancellationFixture(fixture CancellationFixture, names map[string]struct{}) error {
	if !fixtureNamePattern.MatchString(fixture.Name) {
		return registrationError("cancellation fixture name must be canonical")
	}
	if _, exists := names[fixture.Name]; exists {
		return registrationError("fixture names must be unique")
	}
	names[fixture.Name] = struct{}{}
	if fixture.NewHandler == nil {
		return registrationError("cancellation handler factory must be present")
	}
	if err := fixture.Request.Validate(); err != nil {
		return registrationError("cancellation request is invalid: " + err.Error())
	}
	if !fixture.Request.Stream {
		return registrationError("cancellation request must enable streaming")
	}
	return nil
}

func validateFixtureBase(
	label string,
	name string,
	request adapter.NormalizedRequest,
	newHandler HandlerFactory,
	stream bool,
	names map[string]struct{},
) error {
	if !fixtureNamePattern.MatchString(name) {
		return registrationError(label + " fixture name must be canonical")
	}
	if _, exists := names[name]; exists {
		return registrationError("fixture names must be unique")
	}
	names[name] = struct{}{}
	if newHandler == nil {
		return registrationError(label + " handler factory must be present")
	}
	if err := request.Validate(); err != nil {
		return registrationError(label + " request is invalid: " + err.Error())
	}
	if request.Stream != stream {
		return registrationError(fmt.Sprintf("%s request stream must be %t", label, stream))
	}
	return nil
}

func validateFinishReasonCoverage(fixtures FixtureSet) error {
	wanted := []adapter.FinishReason{adapter.FinishLength, adapter.FinishContentPolicy, adapter.FinishUnknown}
	found := make([]adapter.FinishReason, 0, len(fixtures.FinishReasons))
	for _, fixture := range fixtures.FinishReasons {
		if len(fixture.Want.Choices) != 1 {
			return registrationError("each finish reason fixture must contain exactly one choice")
		}
		found = append(found, fixture.Want.Choices[0].FinishReason)
	}
	for _, reason := range wanted {
		if !slices.Contains(found, reason) {
			return registrationError("finish reason fixtures must cover length, content_policy, and unknown")
		}
	}
	return nil
}

func validateSemanticCoverage(fixtures FixtureSet) error {
	ordinary := fixtures.Ordinary.Want
	if ordinary.Usage == nil || !ordinary.Usage.Complete || ordinary.Usage.Source != adapter.UsageSourceProvider {
		return registrationError("ordinary fixture must contain complete provider usage")
	}
	if len(ordinary.Choices) != 1 || ordinary.Choices[0].FinishReason != adapter.FinishStop {
		return registrationError("ordinary fixture must finish with stop")
	}
	cached := fixtures.CachedUsage.Want.Usage
	if cached == nil || !cached.CacheReadTokens.Present || cached.CacheReadTokens.Value <= 0 {
		return registrationError("cached_usage fixture must contain a positive cache-read token count")
	}
	tool := fixtures.ToolCall.Want
	if len(tool.Choices) != 1 || tool.Choices[0].FinishReason != adapter.FinishToolCalls ||
		len(tool.Choices[0].Message.ToolCalls) == 0 {
		return registrationError("tool_call fixture must contain a tool call and tool_calls finish reason")
	}
	if !containsChunkKind(fixtures.Stream.Want, adapter.ChunkMessageStart) ||
		!containsChunkKind(fixtures.Stream.Want, adapter.ChunkContentDelta) ||
		!containsChunkKind(fixtures.Stream.Want, adapter.ChunkMessageEnd) {
		return registrationError("stream fixture must contain message_start, content_delta, and message_end")
	}
	return nil
}

func validateForbiddenText(label string, values []string) error {
	for _, value := range values {
		if value == "" || value != strings.TrimSpace(value) {
			return registrationError(label + " forbidden text must be non-empty and trimmed")
		}
	}
	return nil
}

func containsChunkKind(chunks []adapter.NormalizedChunk, kind adapter.ChunkKind) bool {
	for _, chunk := range chunks {
		if chunk.Kind == kind {
			return true
		}
	}
	return false
}

func registrationError(message string) error {
	return fmt.Errorf("%w: %s", ErrInvalidRegistration, message)
}
