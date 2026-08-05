// Package adapter defines provider-neutral request, response, stream, usage,
// and error facts shared by every provider adapter.
package adapter

import (
	"encoding/json"
	"fmt"
	"time"
)

const (
	// RoleSystem is a platform instruction message.
	RoleSystem MessageRole = "system"
	// RoleDeveloper is an application instruction message.
	RoleDeveloper MessageRole = "developer"
	// RoleUser is an end-user message.
	RoleUser MessageRole = "user"
	// RoleAssistant is a model-produced message.
	RoleAssistant MessageRole = "assistant"
	// RoleTool is a tool result message.
	RoleTool MessageRole = "tool"

	// ContentText is a UTF-8 text part.
	ContentText ContentKind = "text"
	// ContentImageReference is an image reference resolved under a later egress policy.
	ContentImageReference ContentKind = "image_reference"
	// ContentAudioReference is an audio reference resolved under a later egress policy.
	ContentAudioReference ContentKind = "audio_reference"

	// ToolChoiceAuto lets the model decide whether to call a tool.
	ToolChoiceAuto ToolChoiceMode = "auto"
	// ToolChoiceNone prevents tool calls.
	ToolChoiceNone ToolChoiceMode = "none"
	// ToolChoiceRequired requires at least one tool call.
	ToolChoiceRequired ToolChoiceMode = "required"
	// ToolChoiceNamed requires one named tool.
	ToolChoiceNamed ToolChoiceMode = "named"

	// ResponseFormatText requests ordinary text.
	ResponseFormatText ResponseFormatType = "text"
	// ResponseFormatJSONObject requests a JSON object without a supplied schema.
	ResponseFormatJSONObject ResponseFormatType = "json_object"
	// ResponseFormatJSONSchema requests output conforming to a supplied JSON schema.
	ResponseFormatJSONSchema ResponseFormatType = "json_schema"

	// FinishStop means the model completed normally.
	FinishStop FinishReason = "stop"
	// FinishLength means an output or context limit stopped generation.
	FinishLength FinishReason = "length"
	// FinishToolCalls means the model requested one or more tools.
	FinishToolCalls FinishReason = "tool_calls"
	// FinishContentPolicy means a safety policy stopped generation.
	FinishContentPolicy FinishReason = "content_policy"
	// FinishCancelled means cancellation stopped generation.
	FinishCancelled FinishReason = "cancelled"
	// FinishError means generation ended after a protocol or provider failure.
	FinishError FinishReason = "error"
	// FinishUnknown preserves an unmapped provider finish reason without guessing.
	FinishUnknown FinishReason = "unknown"

	// ChunkMessageStart starts one normalized message.
	ChunkMessageStart ChunkKind = "message_start"
	// ChunkContentDelta carries visible content.
	ChunkContentDelta ChunkKind = "content_delta"
	// ChunkReasoningDelta carries separately classified reasoning content.
	ChunkReasoningDelta ChunkKind = "reasoning_delta"
	// ChunkToolDelta carries an incremental tool call.
	ChunkToolDelta ChunkKind = "tool_delta"
	// ChunkUsageDelta carries partial or final usage facts.
	ChunkUsageDelta ChunkKind = "usage_delta"
	// ChunkMessageEnd terminates one normalized message.
	ChunkMessageEnd ChunkKind = "message_end"
	// ChunkHeartbeat reports liveness and never contributes model content.
	ChunkHeartbeat ChunkKind = "heartbeat"
	// ChunkProviderExtension isolates a bounded unrecognized provider event.
	ChunkProviderExtension ChunkKind = "provider_extension"

	// UsageSourceProvider means the provider emitted the usage fact.
	UsageSourceProvider UsageSource = "provider"
	// UsageSourceEstimated means a gateway estimator produced the usage fact.
	UsageSourceEstimated UsageSource = "estimated"
	// UsageSourceReconciled means an external bill confirmed or replaced the fact.
	UsageSourceReconciled UsageSource = "reconciled"
	// UsageSourceAdjustment means this fact corrects prior immutable ledger entries.
	UsageSourceAdjustment UsageSource = "adjustment"

	// UsageStatusPresent means final usage is available.
	UsageStatusPresent UsageStatus = "present"
	// UsageStatusPartial means only a lower-bound or incomplete usage fact is available.
	UsageStatusPartial UsageStatus = "partial"
	// UsageStatusMissing means a terminal provider event omitted usage.
	UsageStatusMissing UsageStatus = "missing"

	// ErrorAuth is an upstream credential failure.
	ErrorAuth ErrorCategory = "auth"
	// ErrorPermission is an upstream authorization failure.
	ErrorPermission ErrorCategory = "permission"
	// ErrorInvalidRequest is a provider-neutral request validation failure.
	ErrorInvalidRequest ErrorCategory = "invalid_request"
	// ErrorRateLimit is an upstream rate limit.
	ErrorRateLimit ErrorCategory = "rate_limit"
	// ErrorCapacity is temporary upstream capacity exhaustion.
	ErrorCapacity ErrorCategory = "capacity"
	// ErrorTimeout is an upstream timeout.
	ErrorTimeout ErrorCategory = "timeout"
	// ErrorProvider5xx is an upstream server failure.
	ErrorProvider5xx ErrorCategory = "provider_5xx"
	// ErrorContentPolicy is a provider safety-policy rejection.
	ErrorContentPolicy ErrorCategory = "content_policy"
	// ErrorContextLength is a context-window rejection.
	ErrorContextLength ErrorCategory = "context_length"
	// ErrorProtocol is a malformed or incompatible provider response.
	ErrorProtocol ErrorCategory = "protocol"
	// ErrorCancelled is caller or gateway cancellation.
	ErrorCancelled ErrorCategory = "cancelled"
	// ErrorUnknown preserves an unmapped provider error without guessing retry semantics.
	ErrorUnknown ErrorCategory = "unknown"
)

// MessageRole is a provider-neutral conversation role.
type MessageRole string

// ContentKind identifies one provider-neutral message content representation.
type ContentKind string

// ToolChoiceMode is the normalized tool selection policy.
type ToolChoiceMode string

// ResponseFormatType is the normalized output structure request.
type ResponseFormatType string

// FinishReason is a finite normalized completion outcome.
type FinishReason string

// ChunkKind identifies one normalized stream event.
type ChunkKind string

// UsageSource identifies who produced a usage fact.
type UsageSource string

// UsageStatus distinguishes missing usage from a real zero and a partial count.
type UsageStatus string

// ErrorCategory is the finite provider-independent error taxonomy.
type ErrorCategory string

// ContentPart contains exactly one text or media-reference value.
type ContentPart struct {
	Kind      ContentKind
	Text      string
	Reference string
	MediaType string
	Detail    string
}

// ToolCall is one complete assistant tool request.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// Message is one normalized conversation message.
type Message struct {
	Role       MessageRole
	Name       string
	Parts      []ContentPart
	ToolCalls  []ToolCall
	ToolCallID string
}

// ToolDefinition is a provider-independent function tool contract.
type ToolDefinition struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// ToolChoice selects automatic, prohibited, required, or named tool behavior.
type ToolChoice struct {
	Mode ToolChoiceMode
	Name string
}

// ResponseFormat requests text, an arbitrary JSON object, or a named JSON schema.
type ResponseFormat struct {
	Type        ResponseFormatType
	Name        string
	Description string
	Schema      json.RawMessage
	Strict      *bool
}

// NormalizedRequest is the provider-neutral input to every adapter.
// It deliberately contains no provider credential, endpoint, tenant ID, or project ID.
type NormalizedRequest struct {
	RequestID        string
	LogicalModel     string
	EndUserReference string
	Messages         []Message
	Stream           bool
	Temperature      *float64
	TopP             *float64
	MaxOutputTokens  *int64
	Stop             []string
	Tools            []ToolDefinition
	ToolChoice       *ToolChoice
	ResponseFormat   *ResponseFormat
	PolicyLabels     []string
	ProviderOptions  json.RawMessage
}

// NormalizedChoice is one complete provider-neutral response choice.
type NormalizedChoice struct {
	Index                int
	Message              Message
	FinishReason         FinishReason
	ProviderFinishReason string
}

// NormalizedResponse is a complete non-streaming provider result.
type NormalizedResponse struct {
	ResponseID        string
	Model             string
	Choices           []NormalizedChoice
	Usage             *NormalizedUsage
	ProviderRequestID string
	ObservedAt        time.Time
}

// ToolCallDelta is one incremental tool-call fragment.
type ToolCallDelta struct {
	Index             int
	ID                string
	Name              string
	ArgumentsFragment string
}

// NormalizedChunk is one finite, sequenced stream fact.
type NormalizedChunk struct {
	Sequence             uint64
	Kind                 ChunkKind
	ChoiceIndex          int
	Role                 MessageRole
	ContentDelta         string
	ReasoningDelta       string
	ToolDelta            *ToolCallDelta
	FinishReason         FinishReason
	ProviderFinishReason string
	Usage                *NormalizedUsage
	UsageStatus          UsageStatus
	ProviderEventType    string
	ProviderExtension    json.RawMessage
	ObservedAt           time.Time
}

// TokenCount preserves the difference between a missing meter and a reported zero.
type TokenCount struct {
	Value   int64
	Present bool
}

// UsageEstimateMetadata identifies the local algorithm and selected catalog
// model that produced an estimated Usage fact. Estimated is deliberately
// redundant with UsageSourceEstimated so serialized facts cannot be mistaken
// for provider-reported counts after being separated from their parent value.
type UsageEstimateMetadata struct {
	Estimated               bool   `json:"estimated"`
	Tokenizer               string `json:"tokenizer"`
	TokenizerVersion        string `json:"tokenizer_version"`
	PhysicalModel           string `json:"physical_model"`
	DeploymentVersion       int64  `json:"deployment_version"`
	ProviderProtocolVersion string `json:"provider_protocol_version"`
}

// NormalizedUsage preserves independent billing dimensions. Cache counts are not
// assumed to be additive to or subsets of input tokens; price semantics decide that later.
type NormalizedUsage struct {
	InputTokens       TokenCount
	OutputTokens      TokenCount
	CacheReadTokens   TokenCount
	CacheWriteTokens  TokenCount
	ReasoningTokens   TokenCount
	AudioInputTokens  TokenCount
	AudioOutputTokens TokenCount
	Source            UsageSource
	Complete          bool
	Estimate          *UsageEstimateMetadata
	RawEvidence       UsageEvidence
	UnmappedFields    []string
}

// NormalizedError contains only safe, provider-neutral failure facts.
// Raw provider bodies and internal causes deliberately have no field here.
type NormalizedError struct {
	Code              string
	Category          ErrorCategory
	Retryable         bool
	RetryAfter        *time.Duration
	ProviderStatus    int
	SafeMessage       string
	ProviderRequestID string
}

// ValidationError identifies a safe invalid normalized field.
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

// Error returns only the safe normalized code and message.
func (err NormalizedError) Error() string {
	if err.Code == "" {
		return err.SafeMessage
	}
	if err.SafeMessage == "" {
		return err.Code
	}
	return fmt.Sprintf("%s: %s", err.Code, err.SafeMessage)
}

// Tokens returns a present token count. Validate rejects negative values.
func Tokens(value int64) TokenCount {
	return TokenCount{Value: value, Present: true}
}
