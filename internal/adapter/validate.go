package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxMessages               = 1024
	maxMessageParts           = 1024
	maxTextBytes              = 1024 * 1024
	maxMediaReferenceBytes    = 16 * 1024
	maxTools                  = 128
	maxSchemaBytes            = 64 * 1024
	maxProviderOptionsBytes   = 64 * 1024
	maxProviderExtensionBytes = 16 * 1024
	maxChoices                = 128
	maxUnmappedUsageFields    = 256
)

var (
	identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)
	toolNamePattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.:-]{0,127}$`)
	labelPattern      = regexp.MustCompile(`^[a-z][a-z0-9._:-]{0,127}$`)
	errorCodePattern  = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)
)

// Validate checks every provider-neutral request invariant before adapter selection.
func (request NormalizedRequest) Validate() error {
	if err := validateIdentifier("request_id", request.RequestID, 128); err != nil {
		return err
	}
	if err := validateIdentifier("logical_model", request.LogicalModel, 128); err != nil {
		return err
	}
	if err := validateEndUserReference(request.EndUserReference); err != nil {
		return err
	}
	if len(request.Messages) == 0 || len(request.Messages) > maxMessages {
		return invalid("messages", fmt.Sprintf("must contain 1-%d entries", maxMessages))
	}
	for index := range request.Messages {
		if err := validateMessage(fmt.Sprintf("messages[%d]", index), request.Messages[index]); err != nil {
			return err
		}
	}
	if err := validateFinitePointer("temperature", request.Temperature); err != nil {
		return err
	}
	if err := validateProbability("top_p", request.TopP); err != nil {
		return err
	}
	if request.MaxOutputTokens != nil && *request.MaxOutputTokens <= 0 {
		return invalid("max_output_tokens", "must be greater than zero when present")
	}
	if err := validateStop(request.Stop); err != nil {
		return err
	}
	if err := validateTools(request.Tools); err != nil {
		return err
	}
	if err := validateToolChoice(request.ToolChoice, request.Tools); err != nil {
		return err
	}
	if err := validateResponseFormat(request.ResponseFormat); err != nil {
		return err
	}
	if err := validateLabels(request.PolicyLabels); err != nil {
		return err
	}
	if len(request.ProviderOptions) > 0 {
		if err := validateJSONObject("provider_options", request.ProviderOptions, maxProviderOptionsBytes); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks a complete non-streaming provider result.
func (response NormalizedResponse) Validate() error {
	if err := validateIdentifier("response_id", response.ResponseID, 256); err != nil {
		return err
	}
	if err := validateIdentifier("model", response.Model, 256); err != nil {
		return err
	}
	if len(response.Choices) == 0 || len(response.Choices) > maxChoices {
		return invalid("choices", fmt.Sprintf("must contain 1-%d entries", maxChoices))
	}
	seen := make(map[int]struct{}, len(response.Choices))
	for index := range response.Choices {
		choice := response.Choices[index]
		field := fmt.Sprintf("choices[%d]", index)
		if choice.Index < 0 {
			return invalid(field+".index", "must not be negative")
		}
		if _, exists := seen[choice.Index]; exists {
			return invalid(field+".index", "must be unique")
		}
		seen[choice.Index] = struct{}{}
		if choice.Message.Role != RoleAssistant {
			return invalid(field+".message.role", "must be assistant")
		}
		if err := validateMessage(field+".message", choice.Message); err != nil {
			return err
		}
		if err := validateFinishReason(field, choice.FinishReason, choice.ProviderFinishReason); err != nil {
			return err
		}
	}
	if response.Usage != nil {
		if err := response.Usage.Validate(); err != nil {
			return err
		}
	}
	if err := validateOptionalIdentifier("provider_request_id", response.ProviderRequestID, 256); err != nil {
		return err
	}
	if response.ObservedAt.IsZero() {
		return invalid("observed_at", "must be present")
	}
	return nil
}

// Validate checks one normalized stream event and its kind-specific payload.
func (chunk NormalizedChunk) Validate() error {
	if chunk.ChoiceIndex < 0 {
		return invalid("choice_index", "must not be negative")
	}
	if chunk.ObservedAt.IsZero() {
		return invalid("observed_at", "must be present")
	}
	if err := validateOptionalIdentifier("provider_event_type", chunk.ProviderEventType, 256); err != nil {
		return err
	}
	if len(chunk.ProviderExtension) > 0 {
		if err := validateJSONObject("provider_extension", chunk.ProviderExtension, maxProviderExtensionBytes); err != nil {
			return err
		}
	}

	switch chunk.Kind {
	case ChunkMessageStart:
		if !validRole(chunk.Role) {
			return invalid("role", "must be a supported role for message_start")
		}
		return validateChunkEmptyPayload(chunk, "message_start", payloadRole)
	case ChunkContentDelta:
		if chunk.ContentDelta == "" || len(chunk.ContentDelta) > maxTextBytes || !utf8.ValidString(chunk.ContentDelta) {
			return invalid("content_delta", "must be non-empty valid UTF-8 within the size limit")
		}
		return validateChunkEmptyPayload(chunk, "content_delta", payloadContent)
	case ChunkReasoningDelta:
		if chunk.ReasoningDelta == "" || len(chunk.ReasoningDelta) > maxTextBytes || !utf8.ValidString(chunk.ReasoningDelta) {
			return invalid("reasoning_delta", "must be non-empty valid UTF-8 within the size limit")
		}
		return validateChunkEmptyPayload(chunk, "reasoning_delta", payloadReasoning)
	case ChunkToolDelta:
		if err := validateToolDelta(chunk.ToolDelta); err != nil {
			return err
		}
		return validateChunkEmptyPayload(chunk, "tool_delta", payloadTool)
	case ChunkUsageDelta:
		if chunk.Usage == nil {
			return invalid("usage", "must be present for usage_delta")
		}
		if chunk.Usage.Complete {
			return invalid("usage.complete", "must be false for usage_delta")
		}
		if chunk.UsageStatus != UsageStatusPartial {
			return invalid("usage_status", "must be partial for usage_delta")
		}
		if err := chunk.Usage.Validate(); err != nil {
			return err
		}
		return validateChunkEmptyPayload(chunk, "usage_delta", payloadUsage)
	case ChunkMessageEnd:
		if err := validateFinishReason("message_end", chunk.FinishReason, chunk.ProviderFinishReason); err != nil {
			return err
		}
		if err := validateTerminalUsage(chunk.UsageStatus, chunk.Usage); err != nil {
			return err
		}
		return validateChunkEmptyPayload(chunk, "message_end", payloadFinish|payloadUsage)
	case ChunkHeartbeat:
		return validateChunkEmptyPayload(chunk, "heartbeat", 0)
	case ChunkProviderExtension:
		if chunk.ProviderEventType == "" {
			return invalid("provider_event_type", "must be present for provider_extension")
		}
		if len(chunk.ProviderExtension) == 0 {
			return invalid("provider_extension", "must be present for provider_extension")
		}
		return validateChunkEmptyPayload(chunk, "provider_extension", payloadExtension)
	default:
		return invalid("kind", "must be a supported normalized chunk kind")
	}
}

// Validate checks normalized usage and requires exact raw evidence for provider facts.
func (usage NormalizedUsage) Validate() error {
	if !validUsageSource(usage.Source) {
		return invalid("usage.source", "must be provider, estimated, reconciled, or adjustment")
	}
	allowNegative := usage.Source == UsageSourceAdjustment
	counts := []struct {
		field string
		value TokenCount
	}{
		{"usage.input_tokens", usage.InputTokens},
		{"usage.output_tokens", usage.OutputTokens},
		{"usage.cache_read_tokens", usage.CacheReadTokens},
		{"usage.cache_write_tokens", usage.CacheWriteTokens},
		{"usage.reasoning_tokens", usage.ReasoningTokens},
		{"usage.audio_input_tokens", usage.AudioInputTokens},
		{"usage.audio_output_tokens", usage.AudioOutputTokens},
	}
	for _, count := range counts {
		if !count.value.Present && count.value.Value != 0 {
			return invalid(count.field, "missing token count must have a zero value")
		}
		if count.value.Present && count.value.Value < 0 && !allowNegative {
			return invalid(count.field, "must not be negative outside an adjustment")
		}
	}
	if (usage.Source == UsageSourceProvider || usage.Source == UsageSourceReconciled) && !usage.RawEvidence.Present() {
		return invalid("usage.raw_evidence", "must be present for provider or reconciled usage")
	}
	if len(usage.UnmappedFields) > maxUnmappedUsageFields {
		return invalid("usage.unmapped_fields", fmt.Sprintf("must contain at most %d entries", maxUnmappedUsageFields))
	}
	if len(usage.UnmappedFields) > 0 && !usage.RawEvidence.Present() {
		return invalid("usage.raw_evidence", "must be present when unmapped fields exist")
	}
	for index, field := range usage.UnmappedFields {
		if field != strings.TrimSpace(field) || !strings.HasPrefix(field, "/") || len(field) > 256 || containsControl(field) {
			return invalid(fmt.Sprintf("usage.unmapped_fields[%d]", index), "must be a trimmed JSON pointer within 256 bytes")
		}
		if index > 0 && usage.UnmappedFields[index-1] >= field {
			return invalid("usage.unmapped_fields", "must be sorted and unique")
		}
	}
	if usage.Source == UsageSourceEstimated {
		if usage.Estimate == nil {
			return invalid("usage.estimate", "must identify the local tokenizer and model")
		}
		if err := usage.Estimate.Validate(); err != nil {
			return err
		}
	} else if usage.Estimate != nil {
		return invalid("usage.estimate", "must be absent outside estimated usage")
	}
	return nil
}

// Validate checks that local estimate evidence is explicit, bounded, and
// content-free. It does not claim the named tokenizer matches a provider's
// private billing implementation.
func (metadata UsageEstimateMetadata) Validate() error {
	if !metadata.Estimated {
		return invalid("usage.estimate.estimated", "must be true")
	}
	if !labelPattern.MatchString(metadata.Tokenizer) {
		return invalid("usage.estimate.tokenizer", "must be a canonical tokenizer identifier")
	}
	if !labelPattern.MatchString(metadata.TokenizerVersion) {
		return invalid("usage.estimate.tokenizer_version", "must be a canonical version identifier")
	}
	if err := validateIdentifier("usage.estimate.physical_model", metadata.PhysicalModel, 200); err != nil {
		return err
	}
	if metadata.DeploymentVersion < 1 {
		return invalid("usage.estimate.deployment_version", "must be positive")
	}
	if err := validateIdentifier("usage.estimate.provider_protocol_version", metadata.ProviderProtocolVersion, 64); err != nil {
		return err
	}
	return nil
}

// Validate checks safe normalized error facts without inspecting a raw provider body.
func (normalizedError NormalizedError) Validate() error {
	if !errorCodePattern.MatchString(normalizedError.Code) {
		return invalid("error.code", "must be an uppercase canonical code")
	}
	if !validErrorCategory(normalizedError.Category) {
		return invalid("error.category", "must be a supported normalized category")
	}
	if normalizedError.Category == ErrorUnknown && normalizedError.Retryable {
		return invalid("error.retryable", "must be false when category is unknown")
	}
	if normalizedError.RetryAfter != nil {
		if !normalizedError.Retryable {
			return invalid("error.retry_after", "requires retryable=true")
		}
		if *normalizedError.RetryAfter <= 0 || *normalizedError.RetryAfter > 24*time.Hour {
			return invalid("error.retry_after", "must be greater than zero and at most 24 hours")
		}
	}
	if normalizedError.ProviderStatus != 0 && (normalizedError.ProviderStatus < 100 || normalizedError.ProviderStatus > 599) {
		return invalid("error.provider_status", "must be zero or a valid HTTP status")
	}
	if err := validateSafeMessage(normalizedError.SafeMessage); err != nil {
		return err
	}
	return validateOptionalIdentifier("error.provider_request_id", normalizedError.ProviderRequestID, 256)
}

func validateMessage(field string, message Message) error {
	if !validRole(message.Role) {
		return invalid(field+".role", "must be a supported normalized role")
	}
	if err := validateOptionalIdentifier(field+".name", message.Name, 128); err != nil {
		return err
	}
	if len(message.Parts) > maxMessageParts {
		return invalid(field+".parts", fmt.Sprintf("must contain at most %d entries", maxMessageParts))
	}
	for index := range message.Parts {
		if err := validateContentPart(fmt.Sprintf("%s.parts[%d]", field, index), message.Parts[index]); err != nil {
			return err
		}
	}
	for index := range message.ToolCalls {
		if err := validateToolCall(fmt.Sprintf("%s.tool_calls[%d]", field, index), message.ToolCalls[index]); err != nil {
			return err
		}
	}
	if message.Role == RoleTool {
		if err := validateIdentifier(field+".tool_call_id", message.ToolCallID, 256); err != nil {
			return err
		}
		if len(message.ToolCalls) > 0 {
			return invalid(field+".tool_calls", "tool result messages cannot contain tool calls")
		}
		if len(message.Parts) == 0 {
			return invalid(field+".parts", "tool result messages require content")
		}
		return nil
	}
	if message.ToolCallID != "" {
		return invalid(field+".tool_call_id", "is only allowed for tool result messages")
	}
	if len(message.ToolCalls) > 0 && message.Role != RoleAssistant {
		return invalid(field+".tool_calls", "are only allowed for assistant messages")
	}
	if len(message.Parts) == 0 && len(message.ToolCalls) == 0 {
		return invalid(field, "must contain content or assistant tool calls")
	}
	return nil
}

func validateContentPart(field string, part ContentPart) error {
	switch part.Kind {
	case ContentText:
		if part.Text == "" || len(part.Text) > maxTextBytes || !utf8.ValidString(part.Text) {
			return invalid(field+".text", "must be non-empty valid UTF-8 within the size limit")
		}
		if part.Reference != "" || part.MediaType != "" || part.Detail != "" {
			return invalid(field, "text parts cannot contain media fields")
		}
	case ContentImageReference:
		if part.Text != "" {
			return invalid(field+".text", "media reference parts cannot contain text")
		}
		if part.Reference == "" || part.Reference != strings.TrimSpace(part.Reference) || len(part.Reference) > maxMediaReferenceBytes || !utf8.ValidString(part.Reference) {
			return invalid(field+".reference", "must be a non-empty trimmed reference within the size limit")
		}
		if err := validateOptionalIdentifier(field+".media_type", part.MediaType, 128); err != nil {
			return err
		}
		if part.Detail != "" && part.Detail != "auto" && part.Detail != "low" && part.Detail != "high" {
			return invalid(field+".detail", "must be auto, low, or high when present")
		}
	case ContentAudioReference:
		if part.Text != "" {
			return invalid(field+".text", "media reference parts cannot contain text")
		}
		if part.Reference == "" || part.Reference != strings.TrimSpace(part.Reference) || len(part.Reference) > maxMediaReferenceBytes || !utf8.ValidString(part.Reference) {
			return invalid(field+".reference", "must be a non-empty trimmed reference within the size limit")
		}
		if err := validateOptionalIdentifier(field+".media_type", part.MediaType, 128); err != nil {
			return err
		}
		if part.Detail != "" {
			return invalid(field+".detail", "is only allowed for image references")
		}
	default:
		return invalid(field+".kind", "must be text, image_reference, or audio_reference")
	}
	return nil
}

func validateToolCall(field string, call ToolCall) error {
	if err := validateIdentifier(field+".id", call.ID, 256); err != nil {
		return err
	}
	if !toolNamePattern.MatchString(call.Name) {
		return invalid(field+".name", "must be a canonical tool name")
	}
	return validateJSONObject(field+".arguments", call.Arguments, maxSchemaBytes)
}

func validateTools(tools []ToolDefinition) error {
	if len(tools) > maxTools {
		return invalid("tools", fmt.Sprintf("must contain at most %d entries", maxTools))
	}
	seen := make(map[string]struct{}, len(tools))
	for index := range tools {
		tool := tools[index]
		field := fmt.Sprintf("tools[%d]", index)
		if !toolNamePattern.MatchString(tool.Name) {
			return invalid(field+".name", "must be a canonical tool name")
		}
		if _, exists := seen[tool.Name]; exists {
			return invalid(field+".name", "must be unique")
		}
		seen[tool.Name] = struct{}{}
		if tool.Description != strings.TrimSpace(tool.Description) || len(tool.Description) > 4096 || !utf8.ValidString(tool.Description) {
			return invalid(field+".description", "must be trimmed valid UTF-8 within 4096 bytes")
		}
		if err := validateJSONObject(field+".input_schema", tool.InputSchema, maxSchemaBytes); err != nil {
			return err
		}
	}
	return nil
}

func validateToolChoice(choice *ToolChoice, tools []ToolDefinition) error {
	if choice == nil {
		return nil
	}
	switch choice.Mode {
	case ToolChoiceAuto, ToolChoiceNone:
		if choice.Name != "" {
			return invalid("tool_choice.name", "must be empty for auto or none")
		}
	case ToolChoiceRequired:
		if choice.Name != "" {
			return invalid("tool_choice.name", "must be empty for required")
		}
		if len(tools) == 0 {
			return invalid("tool_choice", "required mode needs at least one tool")
		}
	case ToolChoiceNamed:
		if !toolNamePattern.MatchString(choice.Name) {
			return invalid("tool_choice.name", "must be a canonical tool name for named mode")
		}
		found := false
		for _, tool := range tools {
			found = found || tool.Name == choice.Name
		}
		if !found {
			return invalid("tool_choice.name", "must reference a declared tool")
		}
	default:
		return invalid("tool_choice.mode", "must be auto, none, required, or named")
	}
	return nil
}

func validateResponseFormat(format *ResponseFormat) error {
	if format == nil {
		return nil
	}
	switch format.Type {
	case ResponseFormatText, ResponseFormatJSONObject:
		if format.Name != "" || format.Description != "" || len(format.Schema) > 0 || format.Strict != nil {
			return invalid("response_format", "text and json_object cannot contain schema fields")
		}
	case ResponseFormatJSONSchema:
		if !toolNamePattern.MatchString(format.Name) {
			return invalid("response_format.name", "must be a canonical schema name")
		}
		if format.Description != strings.TrimSpace(format.Description) || len(format.Description) > 4096 || !utf8.ValidString(format.Description) {
			return invalid("response_format.description", "must be trimmed valid UTF-8 within 4096 bytes")
		}
		if err := validateJSONObject("response_format.schema", format.Schema, maxSchemaBytes); err != nil {
			return err
		}
	default:
		return invalid("response_format.type", "must be text, json_object, or json_schema")
	}
	return nil
}

func validateStop(stop []string) error {
	if len(stop) > 16 {
		return invalid("stop", "must contain at most 16 entries")
	}
	seen := make(map[string]struct{}, len(stop))
	for index, value := range stop {
		if value == "" || len(value) > 256 || !utf8.ValidString(value) {
			return invalid(fmt.Sprintf("stop[%d]", index), "must be non-empty valid UTF-8 within 256 bytes")
		}
		if _, exists := seen[value]; exists {
			return invalid("stop", "must not contain duplicates")
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateLabels(labels []string) error {
	if len(labels) > 64 {
		return invalid("policy_labels", "must contain at most 64 entries")
	}
	if !slices.IsSorted(labels) {
		return invalid("policy_labels", "must be sorted")
	}
	for index, label := range labels {
		if !labelPattern.MatchString(label) {
			return invalid(fmt.Sprintf("policy_labels[%d]", index), "must be a canonical policy label")
		}
		if index > 0 && labels[index-1] == label {
			return invalid("policy_labels", "must be unique")
		}
	}
	return nil
}

func validateToolDelta(delta *ToolCallDelta) error {
	if delta == nil {
		return invalid("tool_delta", "must be present for tool_delta")
	}
	if delta.Index < 0 {
		return invalid("tool_delta.index", "must not be negative")
	}
	if delta.ID == "" && delta.Name == "" && delta.ArgumentsFragment == "" {
		return invalid("tool_delta", "must contain an id, name, or arguments fragment")
	}
	if err := validateOptionalIdentifier("tool_delta.id", delta.ID, 256); err != nil {
		return err
	}
	if delta.Name != "" && !toolNamePattern.MatchString(delta.Name) {
		return invalid("tool_delta.name", "must be a canonical tool name")
	}
	if len(delta.ArgumentsFragment) > maxSchemaBytes || !utf8.ValidString(delta.ArgumentsFragment) {
		return invalid("tool_delta.arguments_fragment", "must be valid UTF-8 within the size limit")
	}
	return nil
}

func validateTerminalUsage(status UsageStatus, usage *NormalizedUsage) error {
	switch status {
	case UsageStatusPresent:
		if usage == nil || !usage.Complete {
			return invalid("usage", "present terminal usage must be complete")
		}
	case UsageStatusPartial:
		if usage == nil || usage.Complete {
			return invalid("usage", "partial terminal usage must be incomplete")
		}
	case UsageStatusMissing:
		if usage != nil {
			return invalid("usage", "must be nil when terminal usage is missing")
		}
	default:
		return invalid("usage_status", "must be present, partial, or missing for message_end")
	}
	if usage != nil {
		return usage.Validate()
	}
	return nil
}

func validateFinishReason(field string, reason FinishReason, providerReason string) error {
	switch reason {
	case FinishStop, FinishLength, FinishToolCalls, FinishContentPolicy, FinishCancelled, FinishError:
		return validateOptionalIdentifier(field+".provider_finish_reason", providerReason, 256)
	case FinishUnknown:
		return validateIdentifier(field+".provider_finish_reason", providerReason, 256)
	default:
		return invalid(field+".finish_reason", "must be a supported normalized finish reason")
	}
}

func validateJSONObject(field string, raw json.RawMessage, maxBytes int) error {
	if len(raw) == 0 {
		return invalid(field, "must be a JSON object")
	}
	if len(raw) > maxBytes {
		return invalid(field, fmt.Sprintf("must not exceed %d bytes", maxBytes))
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]json.RawMessage
	if err := decoder.Decode(&object); err != nil || object == nil {
		return invalid(field, "must be a valid JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return invalid(field, "must contain exactly one JSON object")
	}
	return nil
}

func validateIdentifier(field, value string, maxBytes int) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maxBytes || !utf8.ValidString(value) || !identifierPattern.MatchString(value) {
		return invalid(field, fmt.Sprintf("must be a canonical identifier within %d bytes", maxBytes))
	}
	return nil
}

func validateOptionalIdentifier(field, value string, maxBytes int) error {
	if value == "" {
		return nil
	}
	return validateIdentifier(field, value, maxBytes)
}

func validateEndUserReference(value string) error {
	if value == "" {
		return nil
	}
	if value != strings.TrimSpace(value) || len(value) > 256 || !utf8.ValidString(value) || containsControl(value) {
		return invalid("end_user_reference", "must be trimmed printable UTF-8 within 256 bytes")
	}
	return nil
}

func validateFinitePointer(field string, value *float64) error {
	if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0)) {
		return invalid(field, "must be finite when present")
	}
	return nil
}

func validateProbability(field string, value *float64) error {
	if err := validateFinitePointer(field, value); err != nil {
		return err
	}
	if value != nil && (*value < 0 || *value > 1) {
		return invalid(field, "must be between zero and one")
	}
	return nil
}

func validateSafeMessage(message string) error {
	if message == "" || message != strings.TrimSpace(message) || len(message) > 512 || !utf8.ValidString(message) || containsControl(message) {
		return invalid("error.safe_message", "must be 1-512 trimmed printable UTF-8 bytes")
	}
	return nil
}

func containsControl(value string) bool {
	return strings.ContainsFunc(value, unicode.IsControl)
}

func validRole(role MessageRole) bool {
	switch role {
	case RoleSystem, RoleDeveloper, RoleUser, RoleAssistant, RoleTool:
		return true
	default:
		return false
	}
}

func validUsageSource(source UsageSource) bool {
	switch source {
	case UsageSourceProvider, UsageSourceEstimated, UsageSourceReconciled, UsageSourceAdjustment:
		return true
	default:
		return false
	}
}

func validErrorCategory(category ErrorCategory) bool {
	switch category {
	case ErrorAuth, ErrorPermission, ErrorInvalidRequest, ErrorRateLimit, ErrorCapacity, ErrorTimeout,
		ErrorProvider5xx, ErrorContentPolicy, ErrorContextLength, ErrorProtocol, ErrorCancelled, ErrorUnknown:
		return true
	default:
		return false
	}
}

type chunkPayload uint8

const (
	payloadRole chunkPayload = 1 << iota
	payloadContent
	payloadReasoning
	payloadTool
	payloadFinish
	payloadUsage
	payloadExtension
)

func validateChunkEmptyPayload(chunk NormalizedChunk, kind string, allowed chunkPayload) error {
	present := chunkPayload(0)
	if chunk.Role != "" {
		present |= payloadRole
	}
	if chunk.ContentDelta != "" {
		present |= payloadContent
	}
	if chunk.ReasoningDelta != "" {
		present |= payloadReasoning
	}
	if chunk.ToolDelta != nil {
		present |= payloadTool
	}
	if chunk.FinishReason != "" || chunk.ProviderFinishReason != "" {
		present |= payloadFinish
	}
	if chunk.Usage != nil || chunk.UsageStatus != "" {
		present |= payloadUsage
	}
	if len(chunk.ProviderExtension) > 0 {
		present |= payloadExtension
	}
	if present & ^allowed != 0 {
		return invalid("chunk", kind+" contains a payload owned by another chunk kind")
	}
	return nil
}

func invalid(field, reason string) error {
	return &ValidationError{Field: field, Reason: reason}
}
