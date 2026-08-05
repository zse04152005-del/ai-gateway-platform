//go:build integration

package integration_test

import "github.com/zse04152005-del/ai-gateway-platform/internal/adapter"

const integrationEstimationAlgorithm = "utf8-byte-bound"

func integrationEstimateMetadata() *adapter.UsageEstimateMetadata {
	return integrationEstimateMetadataFor("model-a-physical")
}

func integrationEstimateMetadataFor(physicalModel string) *adapter.UsageEstimateMetadata {
	return &adapter.UsageEstimateMetadata{
		Estimated: true, Tokenizer: integrationEstimationAlgorithm, TokenizerVersion: "v1",
		PhysicalModel: physicalModel, DeploymentVersion: 1,
		ProviderProtocolVersion: "v1",
	}
}
