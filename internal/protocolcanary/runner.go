package protocolcanary

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/provideradapter"
)

const (
	defaultProbeTimeout  = 5 * time.Second
	defaultMaximumChunks = 1024
	maximumChunks        = 4096
)

var (
	// ErrConfiguration means the probe could not be safely assembled. The
	// underlying builder/request error is deliberately not exposed.
	ErrConfiguration = errors.New("protocol canary configuration failed")
)

// AdapterBuilder is satisfied by provideradapter.Registry.
type AdapterBuilder interface {
	Build(
		ctx context.Context,
		provider catalog.Provider,
		deployment catalog.Deployment,
	) (provideradapter.Adapter, error)
}

// HTTPDoer is the constrained egress client injected by process assembly.
type HTTPDoer interface {
	Do(request *http.Request) (*http.Response, error)
}

// Executor is the scheduling port used by a later periodic worker. Schedulers
// provide due Probes and persist only the returned content-free Result.
type Executor interface {
	Run(ctx context.Context, probe Probe) (Result, error)
}

// Options contains bounded runner dependencies.
type Options struct {
	Builder        AdapterBuilder
	Client         HTTPDoer
	Now            func() time.Time
	DefaultTimeout time.Duration
	MaximumChunks  int
}

// Runner executes one minimal probe without retaining provider content.
type Runner struct {
	builder        AdapterBuilder
	client         HTTPDoer
	now            func() time.Time
	defaultTimeout time.Duration
	maximumChunks  int
}

// NewRunner validates dependencies and constructs a concurrency-safe runner.
func NewRunner(options Options) (*Runner, error) {
	if isNilInterface(options.Builder) {
		return nil, errors.New("protocol canary adapter builder must be present")
	}
	if isNilInterface(options.Client) {
		return nil, errors.New("protocol canary HTTP client must be present")
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	if now().IsZero() {
		return nil, errors.New("protocol canary clock must return a non-zero time")
	}
	timeout := options.DefaultTimeout
	if timeout == 0 {
		timeout = defaultProbeTimeout
	}
	if timeout < minimumProbeTimeout || timeout > maximumProbeTimeout {
		return nil, fmt.Errorf("protocol canary default timeout must be between %s and %s", minimumProbeTimeout, maximumProbeTimeout)
	}
	chunkLimit := options.MaximumChunks
	if chunkLimit == 0 {
		chunkLimit = defaultMaximumChunks
	}
	if chunkLimit < 1 || chunkLimit > maximumChunks {
		return nil, fmt.Errorf("protocol canary maximum chunks must be between 1 and %d", maximumChunks)
	}
	return &Runner{
		builder: options.Builder, client: options.Client, now: now,
		defaultTimeout: timeout, maximumChunks: chunkLimit,
	}, nil
}

// Run performs one ordinary or streaming probe. Invalid assembly returns the
// safe ErrConfiguration sentinel; provider observations are returned as Result.
func (runner *Runner) Run(ctx context.Context, probe Probe) (Result, error) {
	if runner == nil || isNilInterface(runner.builder) || isNilInterface(runner.client) || runner.now == nil {
		return Result{}, ErrConfiguration
	}
	if ctx == nil {
		return Result{}, fmt.Errorf("%w: context is nil", ErrConfiguration)
	}
	if err := probe.Validate(); err != nil {
		return Result{}, fmt.Errorf("%w: probe is invalid", ErrConfiguration)
	}
	timeout := probe.Timeout
	if timeout == 0 {
		timeout = runner.defaultTimeout
	}
	started := runner.now().UTC()
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	built, err := runner.builder.Build(probeCtx, probe.Provider, probe.Deployment)
	if err != nil || built == nil || built.Type() != provideradapter.Type(probe.Provider.AdapterType) {
		if outcome, observed := contextOutcome(ctx, probeCtx); observed {
			return runner.complete(probe, started, outcome, nil, nil)
		}
		return Result{}, fmt.Errorf("%w: adapter build failed", ErrConfiguration)
	}
	httpRequest, err := built.BuildRequest(probeCtx, probe.Request.Clone())
	if err != nil || httpRequest == nil {
		if outcome, observed := contextOutcome(ctx, probeCtx); observed {
			return runner.complete(probe, started, outcome, nil, nil)
		}
		return Result{}, fmt.Errorf("%w: minimal request build failed", ErrConfiguration)
	}
	response, err := runner.client.Do(httpRequest)
	if err != nil {
		if outcome, observed := contextOutcome(ctx, probeCtx); observed {
			return runner.complete(probe, started, outcome, nil, nil)
		}
		return runner.complete(probe, started, OutcomeTransportFailure, nil, nil)
	}
	if response == nil || response.Body == nil {
		return runner.complete(probe, started, OutcomeTransportFailure, nil, nil)
	}
	defer func() { _ = response.Body.Close() }()
	if probe.Request.Stream {
		return runner.runStream(probeCtx, ctx, probe, started, built, response)
	}
	return runner.runOrdinary(probeCtx, ctx, probe, started, built, response)
}

func (runner *Runner) runOrdinary(
	probeCtx context.Context,
	parentCtx context.Context,
	probe Probe,
	started time.Time,
	built provideradapter.Adapter,
	response *http.Response,
) (Result, error) {
	normalized, err := built.ParseResponse(probeCtx, response)
	if err != nil {
		return runner.completeError(parentCtx, probeCtx, probe, started, err)
	}
	findings := make([]Finding, 0)
	for index, choice := range normalized.Choices {
		findings = append(findings, inspectFinish(probe.Baseline, choice.FinishReason, choice.ProviderFinishReason,
			fmt.Sprintf("/choices/%d/finish_reason", index))...)
	}
	usageObserved := normalized.Usage != nil
	if normalized.Usage != nil {
		findings = append(findings, inspectUsage(*normalized.Usage, "/usage")...)
	}
	if probe.Baseline.RequireUsage && !usageObserved {
		findings = append(findings, Finding{Code: FindingMissingUsage, Path: "/usage"})
	}
	return runner.completeFindings(probe, started, findings)
}

func (runner *Runner) runStream(
	probeCtx context.Context,
	parentCtx context.Context,
	probe Probe,
	started time.Time,
	built provideradapter.Adapter,
	response *http.Response,
) (Result, error) {
	stream, err := built.OpenStream(probeCtx, response)
	if err != nil {
		return runner.completeError(parentCtx, probeCtx, probe, started, err)
	}
	defer func() { _ = stream.Close() }()
	findings := make([]Finding, 0)
	usageObserved := false
	terminalObserved := false
	for count := 0; ; count++ {
		chunk, nextErr := stream.Next(probeCtx)
		if nextErr != nil {
			if errors.Is(nextErr, io.EOF) {
				break
			}
			outcome, failure, drift := classifyObservation(parentCtx, probeCtx, nextErr)
			if drift != nil {
				findings = append(findings, *drift)
				return runner.completeFindings(probe, started, findings)
			}
			return runner.complete(probe, started, outcome, nil, failure)
		}
		if count >= runner.maximumChunks {
			findings = append(findings, Finding{Code: FindingChunkLimit, Path: "/stream/chunks"})
			return runner.completeFindings(probe, started, findings)
		}
		switch chunk.Kind {
		case adapter.ChunkProviderExtension:
			findings = append(findings, Finding{
				Code:        FindingProviderExtension,
				Path:        "/stream/extensions/" + escapePathSegment(chunk.ProviderEventType),
				Fingerprint: fingerprint(chunk.ProviderExtension),
			})
		case adapter.ChunkMessageEnd:
			terminalObserved = true
			findings = append(findings, inspectFinish(
				probe.Baseline, chunk.FinishReason, chunk.ProviderFinishReason, "/stream/message_end/finish_reason",
			)...)
			if chunk.Usage != nil {
				usageObserved = true
				findings = append(findings, inspectUsage(*chunk.Usage, "/stream/message_end/usage")...)
			}
		case adapter.ChunkUsageDelta:
			if chunk.Usage != nil {
				usageObserved = true
				findings = append(findings, inspectUsage(*chunk.Usage, "/stream/usage")...)
			}
		case adapter.ChunkMessageStart, adapter.ChunkContentDelta, adapter.ChunkReasoningDelta,
			adapter.ChunkToolDelta, adapter.ChunkHeartbeat:
			// Content is intentionally not retained by the canary.
		}
	}
	if !terminalObserved {
		findings = append(findings, Finding{Code: FindingProtocolViolation, Path: "/stream/missing_message_end"})
	}
	if probe.Baseline.RequireUsage && !usageObserved {
		findings = append(findings, Finding{Code: FindingMissingUsage, Path: "/stream/usage"})
	}
	return runner.completeFindings(probe, started, findings)
}

func (runner *Runner) completeError(
	parentCtx context.Context,
	probeCtx context.Context,
	probe Probe,
	started time.Time,
	err error,
) (Result, error) {
	outcome, failure, drift := classifyObservation(parentCtx, probeCtx, err)
	if drift != nil {
		return runner.completeFindings(probe, started, []Finding{*drift})
	}
	return runner.complete(probe, started, outcome, nil, failure)
}

func (runner *Runner) completeFindings(probe Probe, started time.Time, findings []Finding) (Result, error) {
	if len(findings) == 0 {
		return runner.complete(probe, started, OutcomeStable, nil, nil)
	}
	sort.Slice(findings, func(left, right int) bool { return compareFinding(findings[left], findings[right]) < 0 })
	findings = slices.CompactFunc(findings, func(left, right Finding) bool { return compareFinding(left, right) == 0 })
	if len(findings) > maximumFindings {
		findings = findings[:maximumFindings]
	}
	return runner.complete(probe, started, OutcomeDrift, findings, nil)
}

func (runner *Runner) complete(
	probe Probe,
	started time.Time,
	outcome Outcome,
	findings []Finding,
	failure *ProviderFailure,
) (Result, error) {
	finished := runner.now().UTC()
	result := Result{
		ProbeID: probe.ID, ProviderID: probe.Provider.ID, DeploymentID: probe.Deployment.ID,
		AdapterType:             provideradapter.Type(probe.Provider.AdapterType),
		ProviderProtocolVersion: probe.Deployment.Capabilities.ProviderProtocolVersion,
		Outcome:                 outcome, StartedAt: started, FinishedAt: finished, Duration: finished.Sub(started),
		Findings: append([]Finding(nil), findings...), Failure: cloneFailure(failure),
	}
	if err := result.Validate(); err != nil {
		return Result{}, fmt.Errorf("%w: result invariant failed", ErrConfiguration)
	}
	return result, nil
}

func inspectFinish(
	baseline Baseline,
	reason adapter.FinishReason,
	providerReason string,
	path string,
) []Finding {
	if slices.Contains(baseline.AllowedFinishReasons, reason) {
		return nil
	}
	observed := providerReason
	if observed == "" {
		observed = string(reason)
	}
	return []Finding{{Code: FindingUnexpectedFinishReason, Path: path, Fingerprint: fingerprint([]byte(observed))}}
}

func inspectUsage(usage adapter.NormalizedUsage, prefix string) []Finding {
	findings := make([]Finding, 0, len(usage.UnmappedFields))
	for _, path := range usage.UnmappedFields {
		findings = append(findings, Finding{Code: FindingUnmappedUsageField, Path: prefix + path})
	}
	return findings
}

func classifyObservation(
	parentCtx context.Context,
	probeCtx context.Context,
	err error,
) (Outcome, *ProviderFailure, *Finding) {
	if outcome, observed := contextOutcome(parentCtx, probeCtx); observed {
		return outcome, nil, nil
	}
	var violation provideradapter.ProtocolViolation
	if errors.As(err, &violation) {
		operation := violation.ProtocolOperation()
		code := violation.ProtocolCode()
		if !protocolTokenPattern.MatchString(operation) || !protocolTokenPattern.MatchString(code) {
			operation = "invalid_diagnostic"
			code = "invalid_diagnostic"
		}
		finding := Finding{
			Code: FindingProtocolViolation,
			Path: "/protocol/" + escapePathSegment(operation) + "/" + escapePathSegment(code),
		}
		return OutcomeDrift, nil, &finding
	}
	var normalized adapter.NormalizedError
	if errors.As(err, &normalized) {
		failure := &ProviderFailure{
			Code: normalized.Code, Category: normalized.Category,
			Retryable: normalized.Retryable, ProviderStatus: normalized.ProviderStatus,
		}
		return OutcomeProviderFailure, failure, nil
	}
	return OutcomeTransportFailure, nil, nil
}

func contextOutcome(parentCtx, probeCtx context.Context) (Outcome, bool) {
	if parentCtx.Err() != nil {
		return OutcomeCancelled, true
	}
	if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
		return OutcomeTimeout, true
	}
	if probeCtx.Err() != nil {
		return OutcomeCancelled, true
	}
	return "", false
}

func fingerprint(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func escapePathSegment(value string) string {
	if value == "" {
		return "unknown"
	}
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}

func cloneFailure(failure *ProviderFailure) *ProviderFailure {
	if failure == nil {
		return nil
	}
	cloned := *failure
	return &cloned
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	case reflect.Array, reflect.Bool, reflect.Complex128, reflect.Complex64, reflect.Float32, reflect.Float64,
		reflect.Int, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Int8, reflect.Invalid, reflect.String,
		reflect.Struct, reflect.Uint, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uint8, reflect.Uintptr,
		reflect.UnsafePointer:
		return false
	}
	return false
}

var _ Executor = (*Runner)(nil)
