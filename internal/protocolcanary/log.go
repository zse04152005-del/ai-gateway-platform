package protocolcanary

import "log/slog"

// LogValue emits only bounded structural metadata and never provider content.
func (result Result) LogValue() slog.Value {
	attributes := []slog.Attr{
		slog.String("probe_id", result.ProbeID),
		slog.String("provider_id", result.ProviderID),
		slog.String("deployment_id", result.DeploymentID),
		slog.String("adapter_type", string(result.AdapterType)),
		slog.String("provider_protocol_version", result.ProviderProtocolVersion),
		slog.String("outcome", string(result.Outcome)),
		slog.Duration("duration", result.Duration),
		slog.Int("finding_count", len(result.Findings)),
	}
	if result.Failure != nil {
		attributes = append(attributes,
			slog.String("failure_code", result.Failure.Code),
			slog.String("failure_category", string(result.Failure.Category)),
			slog.Bool("failure_retryable", result.Failure.Retryable),
			slog.Int("provider_status", result.Failure.ProviderStatus),
		)
	}
	return slog.GroupValue(attributes...)
}

var _ slog.LogValuer = Result{}
