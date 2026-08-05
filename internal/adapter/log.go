package adapter

import "log/slog"

// LogValue exposes evidence integrity metadata and never raw provider JSON.
func (evidence UsageEvidence) LogValue() slog.Value {
	if !evidence.Present() {
		return slog.GroupValue(slog.Bool("present", false))
	}
	return slog.GroupValue(
		slog.Bool("present", true),
		slog.String("sha256", evidence.Hash()),
		slog.Int("bytes", evidence.Size()),
	)
}

// LogValue exposes request metadata but never messages, schemas, stop strings,
// provider options, or other caller content.
func (request NormalizedRequest) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("requestId", request.RequestID),
		slog.String("logicalModel", request.LogicalModel),
		slog.Bool("stream", request.Stream),
		slog.Int("messageCount", len(request.Messages)),
		slog.Int("toolCount", len(request.Tools)),
	)
}

// LogValue exposes response metadata but never message content or tool arguments.
func (response NormalizedResponse) LogValue() slog.Value {
	attributes := []slog.Attr{
		slog.String("responseId", response.ResponseID),
		slog.String("model", response.Model),
		slog.Int("choiceCount", len(response.Choices)),
	}
	if response.Usage != nil {
		attributes = append(attributes, slog.Any("usage", *response.Usage))
	}
	return slog.GroupValue(attributes...)
}

// LogValue exposes stream control facts but never content, reasoning, tool
// arguments, or provider extension bytes.
func (chunk NormalizedChunk) LogValue() slog.Value {
	attributes := []slog.Attr{
		slog.Uint64("sequence", chunk.Sequence),
		slog.String("kind", string(chunk.Kind)),
		slog.Int("choiceIndex", chunk.ChoiceIndex),
	}
	if chunk.UsageStatus != "" {
		attributes = append(attributes, slog.String("usageStatus", string(chunk.UsageStatus)))
	}
	return slog.GroupValue(attributes...)
}

// LogValue exposes token facts and the evidence digest, never raw evidence.
func (usage NormalizedUsage) LogValue() slog.Value {
	attributes := []slog.Attr{
		slog.String("source", string(usage.Source)),
		slog.Bool("complete", usage.Complete),
	}
	attributes = appendTokenCount(attributes, "inputTokens", usage.InputTokens)
	attributes = appendTokenCount(attributes, "outputTokens", usage.OutputTokens)
	attributes = appendTokenCount(attributes, "cacheReadTokens", usage.CacheReadTokens)
	attributes = appendTokenCount(attributes, "cacheWriteTokens", usage.CacheWriteTokens)
	attributes = appendTokenCount(attributes, "reasoningTokens", usage.ReasoningTokens)
	attributes = appendTokenCount(attributes, "audioInputTokens", usage.AudioInputTokens)
	attributes = appendTokenCount(attributes, "audioOutputTokens", usage.AudioOutputTokens)
	if usage.RawEvidence.Present() {
		attributes = append(attributes,
			slog.String("rawEvidenceHash", usage.RawEvidenceHash()),
			slog.Int("rawEvidenceBytes", usage.RawEvidence.Size()),
		)
	}
	if usage.Estimate != nil {
		attributes = append(attributes,
			slog.Bool("estimated", usage.Estimate.Estimated),
			slog.String("tokenizer", usage.Estimate.Tokenizer),
			slog.String("tokenizerVersion", usage.Estimate.TokenizerVersion),
			slog.String("physicalModel", usage.Estimate.PhysicalModel),
			slog.Int64("deploymentVersion", usage.Estimate.DeploymentVersion),
			slog.String("providerProtocolVersion", usage.Estimate.ProviderProtocolVersion),
		)
	}
	if len(usage.UnmappedFields) > 0 {
		attributes = append(attributes, slog.Int("unmappedFieldCount", len(usage.UnmappedFields)))
	}
	return slog.GroupValue(attributes...)
}

// LogValue exposes only the already-safe normalized error facts.
func (normalizedError NormalizedError) LogValue() slog.Value {
	attributes := []slog.Attr{
		slog.String("code", normalizedError.Code),
		slog.String("category", string(normalizedError.Category)),
		slog.Bool("retryable", normalizedError.Retryable),
		slog.Int("providerStatus", normalizedError.ProviderStatus),
		slog.String("safeMessage", normalizedError.SafeMessage),
	}
	if normalizedError.RetryAfter != nil {
		attributes = append(attributes, slog.Duration("retryAfter", *normalizedError.RetryAfter))
	}
	return slog.GroupValue(attributes...)
}

func appendTokenCount(attributes []slog.Attr, name string, count TokenCount) []slog.Attr {
	if !count.Present {
		return attributes
	}
	return append(attributes, slog.Int64(name, count.Value))
}

var _ slog.LogValuer = NormalizedRequest{}
var _ slog.LogValuer = NormalizedResponse{}
var _ slog.LogValuer = NormalizedChunk{}
var _ slog.LogValuer = NormalizedUsage{}
var _ slog.LogValuer = NormalizedError{}
var _ slog.LogValuer = UsageEvidence{}
