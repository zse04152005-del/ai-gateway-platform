package limits

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/limitpolicy"
)

const (
	maximumTPMEstimatorLabelLength  = 64
	normalizedJSONEstimatorOverhead = uint64(32)
	normalizedJSONEstimatorMethod   = "normalized-json-byte-bound"
	normalizedJSONEstimatorVersion  = "v1"
)

var (
	// ErrTPMEstimateUnavailable means admission cannot obtain a safe input
	// estimate. Callers must fail closed instead of reserving only output.
	ErrTPMEstimateUnavailable = errors.New("TPM input estimate is unavailable")
	// ErrTPMUsageUnavailable means terminal usage does not contain both primary
	// token dimensions needed for settlement.
	ErrTPMUsageUnavailable = errors.New("TPM actual usage is unavailable")

	tpmEstimatorLabelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
)

// InputTokenEstimate is an explicitly estimated input count. Method and
// Version make the estimate reproducible without claiming tokenizer accuracy.
type InputTokenEstimate struct {
	Tokens  uint64
	Method  string
	Version string
}

// InputTokenEstimator is the policy-neutral port used before admission. A
// tokenizer-backed implementation is intentionally deferred to P10-T07.
type InputTokenEstimator interface {
	EstimateInputTokens(ctx context.Context, request adapter.NormalizedRequest) (InputTokenEstimate, error)
}

// NormalizedJSONByteEstimator is the bounded built-in fallback before P10-T07
// provides model tokenizers. It counts the validated normalized JSON bytes plus
// fixed framing overhead, so its method/version remain visibly approximate.
type NormalizedJSONByteEstimator struct {
	maximumInputTokens uint64
}

// NewNormalizedJSONByteEstimator binds the fallback to a selected model's
// maximum admissible input. Values beyond this bound fail closed.
func NewNormalizedJSONByteEstimator(maximumInputTokens uint64) (*NormalizedJSONByteEstimator, error) {
	if maximumInputTokens <= normalizedJSONEstimatorOverhead ||
		maximumInputTokens > limitpolicy.MaximumValue {
		return nil, ErrInvalid
	}
	return &NormalizedJSONByteEstimator{maximumInputTokens: maximumInputTokens}, nil
}

// EstimateInputTokens returns a deterministic, versioned approximation. The
// serialized representation deliberately includes normalized field names and
// empty structural fields as conservative framing overhead; it is not an exact
// provider tokenizer result.
func (estimator *NormalizedJSONByteEstimator) EstimateInputTokens(
	ctx context.Context,
	request adapter.NormalizedRequest,
) (InputTokenEstimate, error) {
	if estimator == nil || ctx == nil || estimator.maximumInputTokens <= normalizedJSONEstimatorOverhead {
		return InputTokenEstimate{}, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return InputTokenEstimate{}, err
	}
	if err := request.Validate(); err != nil {
		return InputTokenEstimate{}, fmt.Errorf("%w: normalized request: %w", ErrInvalid, err)
	}
	payload, err := json.Marshal(request.Clone())
	if err != nil {
		return InputTokenEstimate{}, fmt.Errorf("%w: serialize normalized request: %w", ErrTPMEstimateUnavailable, err)
	}
	bytes := uint64(len(payload))
	if bytes > estimator.maximumInputTokens-normalizedJSONEstimatorOverhead {
		return InputTokenEstimate{}, ErrTPMEstimateUnavailable
	}
	if err := ctx.Err(); err != nil {
		return InputTokenEstimate{}, err
	}
	return InputTokenEstimate{
		Tokens: bytes + normalizedJSONEstimatorOverhead,
		Method: normalizedJSONEstimatorMethod, Version: normalizedJSONEstimatorVersion,
	}, nil
}

// TPMReservationPlan combines estimated input with the largest output the
// selected deployment can emit. Estimated is always true for this planning
// path and prevents the value from being mistaken for provider billing usage.
type TPMReservationPlan struct {
	InputTokens         uint64
	MaximumOutputTokens uint64
	ReservedTokens      uint64
	EstimatorMethod     string
	EstimatorVersion    string
	Estimated           bool
}

// TPMActual is the only default TPM settlement metric: primary input plus
// primary output. Cache, reasoning and audio meters remain independent.
type TPMActual struct {
	InputTokens  uint64
	OutputTokens uint64
	Tokens       uint64
	Source       adapter.UsageSource
	Complete     bool
}

// PlanTPMReservation validates a normalized request, estimates its input and
// reserves the request maximum output. capabilityMaximumOutputTokens is the
// explicit selected-deployment fallback when the request omits a maximum.
func PlanTPMReservation(
	ctx context.Context,
	estimator InputTokenEstimator,
	request adapter.NormalizedRequest,
	capabilityMaximumOutputTokens uint64,
) (TPMReservationPlan, error) {
	if ctx == nil || estimator == nil || capabilityMaximumOutputTokens == 0 ||
		capabilityMaximumOutputTokens > limitpolicy.MaximumValue {
		return TPMReservationPlan{}, ErrInvalid
	}
	if err := request.Validate(); err != nil {
		return TPMReservationPlan{}, fmt.Errorf("%w: normalized request: %w", ErrInvalid, err)
	}
	maximumOutput := capabilityMaximumOutputTokens
	if request.MaxOutputTokens != nil {
		if *request.MaxOutputTokens <= 0 || uint64(*request.MaxOutputTokens) > capabilityMaximumOutputTokens {
			return TPMReservationPlan{}, ErrInvalid
		}
		maximumOutput = uint64(*request.MaxOutputTokens)
	}
	estimate, err := estimator.EstimateInputTokens(ctx, request.Clone())
	if err != nil {
		return TPMReservationPlan{}, fmt.Errorf("%w: %w", ErrTPMEstimateUnavailable, err)
	}
	if err := estimate.validate(); err != nil {
		return TPMReservationPlan{}, err
	}
	if estimate.Tokens > limitpolicy.MaximumValue-maximumOutput {
		return TPMReservationPlan{}, ErrInvalid
	}
	return TPMReservationPlan{
		InputTokens: estimate.Tokens, MaximumOutputTokens: maximumOutput,
		ReservedTokens:  estimate.Tokens + maximumOutput,
		EstimatorMethod: estimate.Method, EstimatorVersion: estimate.Version,
		Estimated: true,
	}, nil
}

// ActualTPM converts normalized terminal usage without adding any independent
// billing dimension. Partial terminal facts remain marked Complete=false.
func ActualTPM(usage adapter.NormalizedUsage) (TPMActual, error) {
	if err := usage.Validate(); err != nil {
		return TPMActual{}, fmt.Errorf("%w: invalid usage: %w", ErrTPMUsageUnavailable, err)
	}
	if usage.Source == adapter.UsageSourceAdjustment {
		return TPMActual{}, ErrTPMUsageUnavailable
	}
	if !usage.InputTokens.Present || !usage.OutputTokens.Present ||
		usage.InputTokens.Value < 0 || usage.OutputTokens.Value < 0 {
		return TPMActual{}, ErrTPMUsageUnavailable
	}
	input := uint64(usage.InputTokens.Value)
	output := uint64(usage.OutputTokens.Value)
	if input > limitpolicy.MaximumValue || output > limitpolicy.MaximumValue ||
		input > limitpolicy.MaximumValue-output {
		return TPMActual{}, ErrTPMUsageUnavailable
	}
	return TPMActual{
		InputTokens: input, OutputTokens: output, Tokens: input + output,
		Source: usage.Source, Complete: usage.Complete,
	}, nil
}

func (estimate InputTokenEstimate) validate() error {
	if estimate.Tokens == 0 || estimate.Tokens > limitpolicy.MaximumValue ||
		!validTPMEstimatorLabel(estimate.Method) || !validTPMEstimatorLabel(estimate.Version) {
		return ErrTPMEstimateUnavailable
	}
	return nil
}

func (plan TPMReservationPlan) validate() error {
	if !plan.Estimated || plan.InputTokens == 0 || plan.MaximumOutputTokens == 0 ||
		plan.ReservedTokens == 0 || plan.ReservedTokens > limitpolicy.MaximumValue ||
		plan.InputTokens > limitpolicy.MaximumValue-plan.MaximumOutputTokens ||
		plan.ReservedTokens != plan.InputTokens+plan.MaximumOutputTokens ||
		!validTPMEstimatorLabel(plan.EstimatorMethod) || !validTPMEstimatorLabel(plan.EstimatorVersion) {
		return ErrInvalid
	}
	return nil
}

func (actual TPMActual) validate() error {
	if actual.InputTokens > limitpolicy.MaximumValue || actual.OutputTokens > limitpolicy.MaximumValue ||
		actual.InputTokens > limitpolicy.MaximumValue-actual.OutputTokens ||
		actual.Tokens != actual.InputTokens+actual.OutputTokens {
		return ErrInvalid
	}
	switch actual.Source {
	case adapter.UsageSourceProvider, adapter.UsageSourceEstimated, adapter.UsageSourceReconciled:
		return nil
	case adapter.UsageSourceAdjustment:
		return ErrInvalid
	default:
		return ErrInvalid
	}
}

func validTPMEstimatorLabel(value string) bool {
	return len(value) >= 1 && len(value) <= maximumTPMEstimatorLabelLength &&
		tpmEstimatorLabelPattern.MatchString(value)
}
