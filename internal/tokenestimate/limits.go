package tokenestimate

import (
	"context"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/limits"
)

// BoundInputEstimator adapts the shared local estimator to the TPM admission
// port while preserving the same tokenizer and model identity.
type BoundInputEstimator struct {
	estimator  *Estimator
	deployment catalog.Deployment
}

// BindInput validates and freezes one Deployment for repeated TPM estimates.
func (estimator *Estimator) BindInput(deployment catalog.Deployment) (*BoundInputEstimator, error) {
	if estimator == nil || deployment.Validate() != nil {
		return nil, ErrInvalid
	}
	return &BoundInputEstimator{estimator: estimator, deployment: deployment}, nil
}

// EstimateInputTokens implements limits.InputTokenEstimator.
func (bound *BoundInputEstimator) EstimateInputTokens(
	ctx context.Context,
	request adapter.NormalizedRequest,
) (limits.InputTokenEstimate, error) {
	if bound == nil || bound.estimator == nil {
		return limits.InputTokenEstimate{}, ErrInvalid
	}
	tokens, metadata, err := bound.estimator.EstimateInput(ctx, bound.deployment, request)
	if err != nil {
		return limits.InputTokenEstimate{}, err
	}
	return limits.InputTokenEstimate{
		Tokens: tokens, Tokenizer: metadata.Tokenizer,
		TokenizerVersion: metadata.TokenizerVersion, PhysicalModel: metadata.PhysicalModel,
		DeploymentVersion:       metadata.DeploymentVersion,
		ProviderProtocolVersion: metadata.ProviderProtocolVersion, Estimated: true,
	}, nil
}

var _ limits.InputTokenEstimator = (*BoundInputEstimator)(nil)
