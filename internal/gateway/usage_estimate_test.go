package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/execution"
	"github.com/zse04152005-del/ai-gateway-platform/internal/retry"
)

const testEstimationAlgorithm = "utf8-byte-bound"

func TestFailoverUsesLocalEstimateOnlyWhenProviderUsageIsAbsent(t *testing.T) {
	responseWithoutUsage := gatewayNormalizedResponse(t)
	responseWithoutUsage.Usage = nil
	estimator := &recordingLocalUsageEstimator{usage: gatewayEstimatedUsage()}
	recorder := &stubExecutionRecorder{}
	coordinator := usageEstimateCoordinator(
		t, &sequenceFailoverSelector{steps: []selectorStep{{selection: failoverSelection(failoverDeploymentA)}}},
		&sequenceFailoverExecutor{steps: []executorStep{{response: responseWithoutUsage}}}, recorder, estimator,
	)

	projected, err := coordinator.Execute(
		context.Background(), failoverSelectionRequest(), failoverRecordedRequest(),
		failoverProviderRequest(), "req_failover_fixture",
	)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if estimator.calls != 1 || estimator.response == nil ||
		estimator.deployment.ID != failoverDeploymentA || projected.Usage == nil ||
		projected.Usage.Source != adapter.UsageSourceEstimated || projected.Gateway.UsageComplete ||
		recorder.outcome.Usage == nil || recorder.outcome.Usage.Estimate == nil ||
		!recorder.outcome.Usage.Estimate.Estimated {
		t.Fatalf("local estimate flow = calls=%d response=%+v projected=%+v outcome=%+v", estimator.calls, estimator.response, projected, recorder.outcome)
	}

	providerResponse := gatewayNormalizedResponse(t)
	providerEstimator := &recordingLocalUsageEstimator{usage: gatewayEstimatedUsage()}
	providerRecorder := &stubExecutionRecorder{}
	providerCoordinator := usageEstimateCoordinator(
		t, &sequenceFailoverSelector{steps: []selectorStep{{selection: failoverSelection(failoverDeploymentA)}}},
		&sequenceFailoverExecutor{steps: []executorStep{{response: providerResponse}}}, providerRecorder, providerEstimator,
	)
	projected, err = providerCoordinator.Execute(
		context.Background(), failoverSelectionRequest(), failoverRecordedRequest(),
		failoverProviderRequest(), "req_failover_fixture",
	)
	if err != nil || providerEstimator.calls != 0 || projected.Usage == nil ||
		projected.Usage.Source != adapter.UsageSourceProvider || providerRecorder.outcome.Usage == nil ||
		providerRecorder.outcome.Usage.Source != adapter.UsageSourceProvider || providerRecorder.outcome.Usage.Estimate != nil {
		t.Fatalf("provider priority = projected=%+v calls=%d outcome=%+v error=%v", projected, providerEstimator.calls, providerRecorder.outcome, err)
	}
}

func TestFailoverRejectsEstimatorThatImpersonatesProvider(t *testing.T) {
	response := gatewayNormalizedResponse(t)
	response.Usage = nil
	estimator := &recordingLocalUsageEstimator{usage: adapter.NormalizedUsage{
		InputTokens: adapter.Tokens(10), OutputTokens: adapter.Tokens(3),
		Source: adapter.UsageSourceProvider, Complete: true,
	}}
	recorder := &stubExecutionRecorder{}
	coordinator := usageEstimateCoordinator(
		t, &sequenceFailoverSelector{steps: []selectorStep{{selection: failoverSelection(failoverDeploymentA)}}},
		&sequenceFailoverExecutor{steps: []executorStep{{response: response}}}, recorder, estimator,
	)
	projected, err := coordinator.Execute(
		context.Background(), failoverSelectionRequest(), failoverRecordedRequest(),
		failoverProviderRequest(), "req_failover_fixture",
	)
	if err != nil || estimator.calls != 1 || projected.Usage != nil || projected.Gateway.UsageComplete ||
		recorder.outcome.Usage != nil {
		t.Fatalf("impersonating estimate = projected=%+v calls=%d outcome=%+v error=%v", projected, estimator.calls, recorder.outcome, err)
	}
}

type recordingLocalUsageEstimator struct {
	usage      adapter.NormalizedUsage
	err        error
	deployment catalog.Deployment
	request    adapter.NormalizedRequest
	response   *adapter.NormalizedResponse
	calls      int
}

func (estimator *recordingLocalUsageEstimator) EstimateUsage(
	_ context.Context,
	deployment catalog.Deployment,
	request adapter.NormalizedRequest,
	response *adapter.NormalizedResponse,
) (adapter.NormalizedUsage, error) {
	estimator.calls++
	estimator.deployment = deployment
	estimator.request = request.Clone()
	if response != nil {
		cloned := response.Clone()
		estimator.response = &cloned
	}
	return estimator.usage.Clone(), estimator.err
}

func gatewayEstimatedUsage() adapter.NormalizedUsage {
	return adapter.NormalizedUsage{
		InputTokens: adapter.Tokens(101), OutputTokens: adapter.Tokens(17),
		Source: adapter.UsageSourceEstimated, Complete: false,
		Estimate: &adapter.UsageEstimateMetadata{
			Estimated: true, Tokenizer: testEstimationAlgorithm, TokenizerVersion: "v1",
			PhysicalModel: "gateway-model-fixture", DeploymentVersion: 4,
			ProviderProtocolVersion: "protocol-v1",
		},
	}
}

func usageEstimateCoordinator(
	t *testing.T,
	selector RouteSelector,
	executor ChatExecutor,
	recorder execution.Recorder,
	estimator LocalUsageEstimator,
) *nonStreamFailover {
	t.Helper()
	coordinator, err := newNonStreamFailover(
		selector, executor, recorder, &stubRouteDecisionRecorder{},
		FailoverOptions{
			MaximumAttempts: 3, TotalTimeout: 5 * time.Second,
			MinimumAttemptWindow: 10 * time.Millisecond, AdditionalCost: retry.CostAllowed,
		},
		time.Now, noWait, estimator,
	)
	if err != nil {
		t.Fatalf("newNonStreamFailover() error = %v", err)
	}
	return coordinator
}

var _ LocalUsageEstimator = (*recordingLocalUsageEstimator)(nil)
