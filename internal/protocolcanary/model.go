// Package protocolcanary defines safe, minimal recurring probes that detect
// provider protocol drift through the public Provider Adapter contract.
package protocolcanary

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/provideradapter"
)

const (
	// OutcomeStable means the provider exchange matched every canary invariant.
	OutcomeStable Outcome = "stable"
	// OutcomeDrift means the adapter observed new or malformed protocol data.
	OutcomeDrift Outcome = "drift"
	// OutcomeProviderFailure means the provider returned a normalized non-2xx failure.
	OutcomeProviderFailure Outcome = "provider_failure"
	// OutcomeTransportFailure means the exchange failed without a safe provider error.
	OutcomeTransportFailure Outcome = "transport_failure"
	// OutcomeTimeout means the canary-specific deadline expired.
	OutcomeTimeout Outcome = "timeout"
	// OutcomeCancelled means the caller cancelled the canary.
	OutcomeCancelled Outcome = "cancelled"

	// FindingProtocolViolation identifies a safe adapter parser operation/code.
	FindingProtocolViolation FindingCode = "protocol_violation"
	// FindingUnexpectedFinishReason identifies a new or disallowed provider finish reason.
	FindingUnexpectedFinishReason FindingCode = "unexpected_finish_reason"
	// FindingUnmappedUsageField identifies a new billing-related JSON pointer.
	FindingUnmappedUsageField FindingCode = "unmapped_usage_field"
	// FindingProviderExtension identifies an isolated unknown stream event.
	FindingProviderExtension FindingCode = "provider_extension"
	// FindingMissingUsage identifies an expected terminal Usage omission.
	FindingMissingUsage FindingCode = "missing_usage"
	// FindingChunkLimit identifies a stream that exceeded the probe event budget.
	FindingChunkLimit FindingCode = "chunk_limit_exceeded"
)

const (
	minimumProbeTimeout = 10 * time.Millisecond
	maximumProbeTimeout = 30 * time.Second
	maximumPromptBytes  = 256
	maximumOutputTokens = 16
	maximumFindings     = 256
)

var (
	probeIDPattern       = regexp.MustCompile(`^[a-z][a-z0-9._:-]{0,127}$`)
	protocolTokenPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	fingerprintPattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Outcome is the operational result of one completed probe.
type Outcome string

// FindingCode is a finite drift signal category.
type FindingCode string

// Baseline describes the result semantics accepted by a minimal probe.
// Unmapped usage and provider extensions are never allowlisted: they remain
// drift until the adapter explicitly maps them.
type Baseline struct {
	AllowedFinishReasons []adapter.FinishReason
	RequireUsage         bool
}

// Probe binds one minimal synthetic request to one catalog deployment.
type Probe struct {
	ID         string
	Provider   catalog.Provider
	Deployment catalog.Deployment
	Request    adapter.NormalizedRequest
	Timeout    time.Duration
	Baseline   Baseline
}

// Finding contains safe structural metadata only. Fingerprint is a SHA-256 of
// an unknown value/event and cannot reconstruct provider content.
type Finding struct {
	Code        FindingCode `json:"code"`
	Path        string      `json:"path,omitempty"`
	Fingerprint string      `json:"fingerprint,omitempty"`
}

// ProviderFailure is the safe subset of a NormalizedError retained by a probe.
type ProviderFailure struct {
	Code           string                `json:"code"`
	Category       adapter.ErrorCategory `json:"category"`
	Retryable      bool                  `json:"retryable"`
	ProviderStatus int                   `json:"provider_status,omitempty"`
}

// Result deliberately has no prompt, response, raw body, tool arguments,
// provider message, credential, or internal error field.
type Result struct {
	ProbeID                 string               `json:"probe_id"`
	ProviderID              string               `json:"provider_id"`
	DeploymentID            string               `json:"deployment_id"`
	AdapterType             provideradapter.Type `json:"adapter_type"`
	ProviderProtocolVersion string               `json:"provider_protocol_version"`
	Outcome                 Outcome              `json:"outcome"`
	StartedAt               time.Time            `json:"started_at"`
	FinishedAt              time.Time            `json:"finished_at"`
	Duration                time.Duration        `json:"duration"`
	Findings                []Finding            `json:"findings,omitempty"`
	Failure                 *ProviderFailure     `json:"failure,omitempty"`
}

// Validate enforces minimal-cost, synthetic-only canary input.
func (probe Probe) Validate() error {
	if !probeIDPattern.MatchString(probe.ID) {
		return errors.New("protocol canary id must be a canonical 1-128 character identifier")
	}
	if err := probe.Provider.Validate(); err != nil {
		return fmt.Errorf("validate protocol canary provider: %w", err)
	}
	if err := probe.Deployment.Validate(); err != nil {
		return fmt.Errorf("validate protocol canary deployment: %w", err)
	}
	if probe.Provider.Status != catalog.StatusActive || probe.Deployment.Status != catalog.StatusActive {
		return errors.New("protocol canary provider and deployment must be active")
	}
	if probe.Deployment.ProviderID != probe.Provider.ID {
		return errors.New("protocol canary deployment does not belong to provider")
	}
	if probe.Timeout != 0 && (probe.Timeout < minimumProbeTimeout || probe.Timeout > maximumProbeTimeout) {
		return fmt.Errorf("protocol canary timeout must be zero or between %s and %s", minimumProbeTimeout, maximumProbeTimeout)
	}
	if err := probe.Request.Validate(); err != nil {
		return fmt.Errorf("validate protocol canary request: %w", err)
	}
	if err := validateMinimalRequest(probe.Request); err != nil {
		return err
	}
	if probe.Request.Stream && !probe.Deployment.Capabilities.Stream {
		return errors.New("protocol canary stream request requires deployment stream capability")
	}
	return probe.Baseline.Validate()
}

// Validate checks the explicit accepted finish outcomes.
func (baseline Baseline) Validate() error {
	if len(baseline.AllowedFinishReasons) == 0 || len(baseline.AllowedFinishReasons) > 8 {
		return errors.New("protocol canary baseline must allow 1-8 finish reasons")
	}
	if !slices.IsSorted(baseline.AllowedFinishReasons) {
		return errors.New("protocol canary finish reasons must be sorted")
	}
	for index, reason := range baseline.AllowedFinishReasons {
		if !knownFinishReason(reason) || reason == adapter.FinishUnknown {
			return errors.New("protocol canary baseline contains an unsupported finish reason")
		}
		if index > 0 && baseline.AllowedFinishReasons[index-1] == reason {
			return errors.New("protocol canary finish reasons must be unique")
		}
	}
	return nil
}

// Validate checks that a persisted/transported result remains content-free and coherent.
func (result Result) Validate() error {
	if !probeIDPattern.MatchString(result.ProbeID) {
		return errors.New("protocol canary result has an invalid probe id")
	}
	if result.ProviderID == "" || result.DeploymentID == "" || result.AdapterType == "" ||
		!protocolTokenPattern.MatchString(result.ProviderProtocolVersion) {
		return errors.New("protocol canary result identity is incomplete")
	}
	if !validOutcome(result.Outcome) {
		return errors.New("protocol canary result has an invalid outcome")
	}
	if result.StartedAt.IsZero() || result.FinishedAt.Before(result.StartedAt) ||
		result.Duration != result.FinishedAt.Sub(result.StartedAt) {
		return errors.New("protocol canary result timestamps are invalid")
	}
	if len(result.Findings) > maximumFindings {
		return errors.New("protocol canary result has too many findings")
	}
	for index, finding := range result.Findings {
		if err := finding.Validate(); err != nil {
			return fmt.Errorf("validate protocol canary finding %d: %w", index, err)
		}
		if index > 0 && compareFinding(result.Findings[index-1], finding) >= 0 {
			return errors.New("protocol canary findings must be sorted and unique")
		}
	}
	if result.Outcome == OutcomeDrift && len(result.Findings) == 0 {
		return errors.New("protocol canary drift result requires findings")
	}
	if result.Outcome != OutcomeDrift && len(result.Findings) != 0 {
		return errors.New("protocol canary findings are only allowed for drift")
	}
	if result.Outcome == OutcomeProviderFailure {
		if result.Failure == nil {
			return errors.New("protocol canary provider failure requires safe failure facts")
		}
		return result.Failure.Validate()
	}
	if result.Failure != nil {
		return errors.New("protocol canary failure facts require provider_failure outcome")
	}
	return nil
}

// Validate checks one safe structural finding.
func (finding Finding) Validate() error {
	if !validFindingCode(finding.Code) {
		return errors.New("protocol canary finding code is invalid")
	}
	if finding.Path != "" && (finding.Path != strings.TrimSpace(finding.Path) ||
		len(finding.Path) > 512 || !utf8.ValidString(finding.Path) || !strings.HasPrefix(finding.Path, "/")) {
		return errors.New("protocol canary finding path must be a bounded JSON-style path")
	}
	if finding.Fingerprint != "" && !fingerprintPattern.MatchString(finding.Fingerprint) {
		return errors.New("protocol canary finding fingerprint must be a lowercase SHA-256")
	}
	return nil
}

// Validate checks the safe subset of provider error facts.
func (failure ProviderFailure) Validate() error {
	normalized := adapter.NormalizedError{
		Code: failure.Code, Category: failure.Category, Retryable: failure.Retryable,
		ProviderStatus: failure.ProviderStatus, SafeMessage: "Protocol canary provider failure",
	}
	return normalized.Validate()
}

func validateMinimalRequest(request adapter.NormalizedRequest) error {
	if len(request.Messages) != 1 || request.Messages[0].Role != adapter.RoleUser ||
		len(request.Messages[0].Parts) != 1 || request.Messages[0].Parts[0].Kind != adapter.ContentText {
		return errors.New("protocol canary request must contain exactly one synthetic user text part")
	}
	text := request.Messages[0].Parts[0].Text
	if len(text) == 0 || len(text) > maximumPromptBytes {
		return fmt.Errorf("protocol canary synthetic prompt must be 1-%d bytes", maximumPromptBytes)
	}
	if request.MaxOutputTokens == nil || *request.MaxOutputTokens < 1 || *request.MaxOutputTokens > maximumOutputTokens {
		return fmt.Errorf("protocol canary max output tokens must be 1-%d", maximumOutputTokens)
	}
	if len(request.Tools) != 0 || request.ToolChoice != nil || request.ResponseFormat != nil || len(request.PolicyLabels) != 0 {
		return errors.New("protocol canary request must not enable tools, structured output, or policy content")
	}
	return nil
}

func knownFinishReason(reason adapter.FinishReason) bool {
	switch reason {
	case adapter.FinishStop, adapter.FinishLength, adapter.FinishToolCalls, adapter.FinishContentPolicy,
		adapter.FinishCancelled, adapter.FinishError, adapter.FinishUnknown:
		return true
	default:
		return false
	}
}

func validOutcome(outcome Outcome) bool {
	switch outcome {
	case OutcomeStable, OutcomeDrift, OutcomeProviderFailure, OutcomeTransportFailure, OutcomeTimeout, OutcomeCancelled:
		return true
	default:
		return false
	}
}

func validFindingCode(code FindingCode) bool {
	switch code {
	case FindingProtocolViolation, FindingUnexpectedFinishReason, FindingUnmappedUsageField,
		FindingProviderExtension, FindingMissingUsage, FindingChunkLimit:
		return true
	default:
		return false
	}
}

func compareFinding(left, right Finding) int {
	leftKey := string(left.Code) + "\x00" + left.Path + "\x00" + left.Fingerprint
	rightKey := string(right.Code) + "\x00" + right.Path + "\x00" + right.Fingerprint
	return strings.Compare(leftKey, rightKey)
}
