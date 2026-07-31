package limits

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/limitpolicy"
)

func TestNormalizedJSONByteEstimatorIsBoundedAndVersioned(t *testing.T) {
	request := validTPMRequest()
	payload, err := json.Marshal(request.Clone())
	if err != nil {
		t.Fatalf("json.Marshal(request) error = %v", err)
	}
	estimator, err := NewNormalizedJSONByteEstimator(10_000)
	if err != nil {
		t.Fatalf("NewNormalizedJSONByteEstimator() error = %v", err)
	}
	estimate, err := estimator.EstimateInputTokens(context.Background(), request)
	if err != nil {
		t.Fatalf("EstimateInputTokens() error = %v", err)
	}
	if estimate.Tokens != uint64(len(payload))+normalizedJSONEstimatorOverhead ||
		estimate.Method != normalizedJSONEstimatorMethod || estimate.Version != normalizedJSONEstimatorVersion {
		t.Fatalf("estimate = %+v", estimate)
	}

	plan, err := PlanTPMReservation(context.Background(), estimator, request, 512)
	if err != nil || plan.InputTokens != estimate.Tokens || plan.ReservedTokens != estimate.Tokens+128 {
		t.Fatalf("PlanTPMReservation(built-in estimator) = %+v, %v", plan, err)
	}
}

func TestNormalizedJSONByteEstimatorFailsClosed(t *testing.T) {
	if _, err := NewNormalizedJSONByteEstimator(0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("NewNormalizedJSONByteEstimator(zero) error = %v", err)
	}
	if _, err := NewNormalizedJSONByteEstimator(limitpolicy.MaximumValue + 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("NewNormalizedJSONByteEstimator(large) error = %v", err)
	}
	if _, err := (*NormalizedJSONByteEstimator)(nil).EstimateInputTokens(
		context.Background(), validTPMRequest(),
	); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil EstimateInputTokens() error = %v", err)
	}
	var nilContext context.Context
	estimator, err := NewNormalizedJSONByteEstimator(10_000)
	if err != nil {
		t.Fatalf("NewNormalizedJSONByteEstimator() error = %v", err)
	}
	if _, err := estimator.EstimateInputTokens(nilContext, validTPMRequest()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("EstimateInputTokens(nil context) error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := estimator.EstimateInputTokens(cancelled, validTPMRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("EstimateInputTokens(cancelled) error = %v", err)
	}
	if _, err := estimator.EstimateInputTokens(context.Background(), adapter.NormalizedRequest{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("EstimateInputTokens(invalid request) error = %v", err)
	}
	payload, err := json.Marshal(validTPMRequest().Clone())
	if err != nil {
		t.Fatalf("json.Marshal(request) error = %v", err)
	}
	small, err := NewNormalizedJSONByteEstimator(uint64(len(payload)) + normalizedJSONEstimatorOverhead - 1)
	if err != nil {
		t.Fatalf("NewNormalizedJSONByteEstimator(small) error = %v", err)
	}
	if _, err := small.EstimateInputTokens(context.Background(), validTPMRequest()); !errors.Is(err, ErrTPMEstimateUnavailable) {
		t.Fatalf("EstimateInputTokens(over bound) error = %v", err)
	}
}

func TestPlanTPMReservationUsesRequestMaximumAndExplicitFallback(t *testing.T) {
	request := validTPMRequest()
	estimator := &recordingTPMEstimator{estimate: InputTokenEstimate{
		Tokens: 37, Method: "bytes-upper-bound", Version: "v1",
	}}

	plan, err := PlanTPMReservation(context.Background(), estimator, request, 512)
	if err != nil {
		t.Fatalf("PlanTPMReservation(request maximum) error = %v", err)
	}
	if plan.InputTokens != 37 || plan.MaximumOutputTokens != 128 || plan.ReservedTokens != 165 ||
		plan.EstimatorMethod != "bytes-upper-bound" || plan.EstimatorVersion != "v1" || !plan.Estimated {
		t.Fatalf("request maximum plan = %+v", plan)
	}
	if len(estimator.requests) != 1 || estimator.requests[0].RequestID != request.RequestID {
		t.Fatalf("estimator requests = %+v", estimator.requests)
	}
	estimator.requests[0].Messages[0].Parts[0].Text = "mutated"
	if request.Messages[0].Parts[0].Text == "mutated" {
		t.Fatal("estimator received an aliased request")
	}

	request.MaxOutputTokens = nil
	plan, err = PlanTPMReservation(context.Background(), estimator, request, 512)
	if err != nil || plan.MaximumOutputTokens != 512 || plan.ReservedTokens != 549 {
		t.Fatalf("fallback plan = %+v, %v", plan, err)
	}
}

func TestPlanTPMReservationFailsClosed(t *testing.T) {
	request := validTPMRequest()
	validEstimator := &recordingTPMEstimator{estimate: InputTokenEstimate{
		Tokens: 1, Method: "safe", Version: "v1",
	}}
	var nilContext context.Context
	invalidCalls := []struct {
		name       string
		ctx        context.Context
		estimator  InputTokenEstimator
		request    adapter.NormalizedRequest
		capability uint64
	}{
		{"nil context", nilContext, validEstimator, request, 512},
		{"nil estimator", context.Background(), nil, request, 512},
		{"zero capability", context.Background(), validEstimator, request, 0},
		{"large capability", context.Background(), validEstimator, request, limitpolicy.MaximumValue + 1},
		{"invalid request", context.Background(), validEstimator, adapter.NormalizedRequest{}, 512},
	}
	for _, test := range invalidCalls {
		t.Run(test.name, func(t *testing.T) {
			if _, err := PlanTPMReservation(test.ctx, test.estimator, test.request, test.capability); !errors.Is(err, ErrInvalid) {
				t.Fatalf("PlanTPMReservation() error = %v", err)
			}
		})
	}

	tooLarge := int64(513)
	request.MaxOutputTokens = &tooLarge
	if _, err := PlanTPMReservation(context.Background(), validEstimator, request, 512); !errors.Is(err, ErrInvalid) {
		t.Fatalf("PlanTPMReservation(output beyond capability) error = %v", err)
	}

	request = validTPMRequest()
	estimatorError := errors.New("estimator offline")
	failures := []struct {
		name      string
		estimator *recordingTPMEstimator
	}{
		{"dependency", &recordingTPMEstimator{err: estimatorError}},
		{"zero", &recordingTPMEstimator{estimate: InputTokenEstimate{Method: "safe", Version: "v1"}}},
		{"method", &recordingTPMEstimator{estimate: InputTokenEstimate{Tokens: 1, Method: "bad method", Version: "v1"}}},
		{"version", &recordingTPMEstimator{estimate: InputTokenEstimate{Tokens: 1, Method: "safe", Version: ""}}},
	}
	for _, test := range failures {
		t.Run(test.name, func(t *testing.T) {
			_, err := PlanTPMReservation(context.Background(), test.estimator, request, 512)
			if !errors.Is(err, ErrTPMEstimateUnavailable) {
				t.Fatalf("PlanTPMReservation() error = %v", err)
			}
		})
	}

	overflowEstimator := &recordingTPMEstimator{estimate: InputTokenEstimate{
		Tokens: limitpolicy.MaximumValue, Method: "safe", Version: "v1",
	}}
	if _, err := PlanTPMReservation(context.Background(), overflowEstimator, request, 512); !errors.Is(err, ErrInvalid) {
		t.Fatalf("PlanTPMReservation(overflow) error = %v", err)
	}
}

func TestActualTPMUsesOnlyPrimaryDimensions(t *testing.T) {
	usage := adapter.NormalizedUsage{
		InputTokens: adapter.Tokens(11), OutputTokens: adapter.Tokens(7),
		CacheReadTokens: adapter.Tokens(5), ReasoningTokens: adapter.Tokens(3),
		AudioInputTokens: adapter.Tokens(2), AudioOutputTokens: adapter.Tokens(1),
		Source: adapter.UsageSourceEstimated, Complete: false,
	}
	actual, err := ActualTPM(usage)
	if err != nil {
		t.Fatalf("ActualTPM() error = %v", err)
	}
	if actual.InputTokens != 11 || actual.OutputTokens != 7 || actual.Tokens != 18 ||
		actual.Source != adapter.UsageSourceEstimated || actual.Complete {
		t.Fatalf("actual = %+v", actual)
	}
}

func TestActualTPMRequiresBothPrimaryDimensions(t *testing.T) {
	invalid := []adapter.NormalizedUsage{
		{InputTokens: adapter.Tokens(1), Source: adapter.UsageSourceEstimated},
		{OutputTokens: adapter.Tokens(1), Source: adapter.UsageSourceEstimated},
		{InputTokens: adapter.Tokens(-1), OutputTokens: adapter.Tokens(1), Source: adapter.UsageSourceEstimated},
		{InputTokens: adapter.Tokens(1), OutputTokens: adapter.Tokens(1), Source: adapter.UsageSourceAdjustment},
	}
	for index, usage := range invalid {
		if _, err := ActualTPM(usage); !errors.Is(err, ErrTPMUsageUnavailable) {
			t.Fatalf("ActualTPM(invalid %d) error = %v", index, err)
		}
	}
}

type recordingTPMEstimator struct {
	estimate InputTokenEstimate
	err      error
	requests []adapter.NormalizedRequest
}

func (estimator *recordingTPMEstimator) EstimateInputTokens(
	_ context.Context,
	request adapter.NormalizedRequest,
) (InputTokenEstimate, error) {
	estimator.requests = append(estimator.requests, request)
	return estimator.estimate, estimator.err
}

func validTPMRequest() adapter.NormalizedRequest {
	maximum := int64(128)
	return adapter.NormalizedRequest{
		RequestID: "req_p09_t04", LogicalModel: "support-chat", MaxOutputTokens: &maximum,
		Messages: []adapter.Message{{
			Role:  adapter.RoleUser,
			Parts: []adapter.ContentPart{{Kind: adapter.ContentText, Text: "hello"}},
		}},
	}
}
