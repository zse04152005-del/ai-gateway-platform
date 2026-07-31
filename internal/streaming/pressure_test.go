package streaming

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/mockadapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/provideradapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/upstreamhttp"
)

const (
	pressureBatches          = 3
	pressureSessionsPerBatch = 50
	pressureSlowChunks       = 64
)

var (
	errPressureClientCancelled = errors.New("pressure fixture client cancelled")
	errPressureSessionTimeout  = errors.New("pressure fixture session timeout")
)

type pressureScenario string

const (
	pressureNormal     pressureScenario = "normal"
	pressureSlowClient pressureScenario = "slow_client"
	pressureCancel     pressureScenario = "random_cancel"
	pressureDisconnect pressureScenario = "upstream_disconnect"
	pressureLongTTFT   pressureScenario = "long_ttft"
)

type pressureSpec struct {
	id          int
	scenario    pressureScenario
	cancelDelay time.Duration
}

type pressureResult struct {
	spec        pressureSpec
	setupErr    error
	producerErr error
	consumerErr error
	modelChunks int
	buffer      BufferStats
	timeout     TimeoutSnapshot
	failover    FailoverSnapshot
	usageOrigin UsageOrigin
	usage       *adapter.NormalizedUsage
	duration    time.Duration
}

type pressureServerMetrics struct {
	active atomic.Int64
	peak   atomic.Int64
}

func TestStreamingMixedPressureReleasesConnectionsGoroutinesAndBuffers(t *testing.T) {
	connections := newCancellationConnections()
	serverMetrics := &pressureServerMetrics{}
	server := httptest.NewUnstartedServer(pressureProviderHandler(serverMetrics))
	server.Config.ConnState = connections.observe
	server.Start()
	serverClosed := false
	t.Cleanup(func() {
		if !serverClosed {
			server.Close()
		}
	})

	client := newPressureHTTPClient(t)
	factory, err := mockadapter.NewFactory(mockadapter.FactoryOptions{})
	if err != nil {
		t.Fatalf("mockadapter.NewFactory() error = %v", err)
	}
	built := newCancellationAdapter(context.Background(), t, factory, server.URL)

	runtime.GC()
	baselineGoroutines := runtime.NumGoroutine()
	var baselineMemory runtime.MemStats
	runtime.ReadMemStats(&baselineMemory)
	random := rand.New(rand.NewSource(20260731)) //nolint:gosec // deterministic scheduling fixture, not security.
	totals := make(map[pressureScenario]int)
	maxBufferChunks := 0
	maxBufferBytes := 0
	backpressureWaits := uint64(0)
	startedAt := time.Now()

	for batch := range pressureBatches {
		specs := makePressureSpecs(batch, random)
		results := runPressureBatch(t, client, built, specs)
		for _, result := range results {
			assertPressureResult(t, result)
			totals[result.spec.scenario]++
			maxBufferChunks = max(maxBufferChunks, result.buffer.MaxObservedChunks)
			maxBufferBytes = max(maxBufferBytes, result.buffer.MaxObservedBytes)
			backpressureWaits += result.buffer.BackpressureWaits
		}
		assertEventually(t, 2*time.Second, func() bool { return serverMetrics.active.Load() == 0 },
			fmt.Sprintf("batch %d retained upstream handlers", batch))
	}

	client.CloseIdleConnections()
	assertEventually(t, 3*time.Second, func() bool { return connections.count() == 0 },
		"mixed pressure retained upstream connections")
	assertEventually(t, 3*time.Second, func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= baselineGoroutines+12
	}, "mixed pressure leaked goroutines")
	runtime.GC()
	var finalMemory runtime.MemStats
	runtime.ReadMemStats(&finalMemory)
	const retainedHeapAllowance = 32 << 20
	if finalMemory.HeapAlloc > baselineMemory.HeapAlloc+retainedHeapAllowance {
		t.Fatalf("retained heap grew from %d to %d bytes, allowance=%d",
			baselineMemory.HeapAlloc, finalMemory.HeapAlloc, retainedHeapAllowance)
	}
	if serverMetrics.peak.Load() < 10 {
		t.Fatalf("peak concurrent upstream handlers = %d, want at least 10", serverMetrics.peak.Load())
	}
	if maxBufferChunks > 4 || maxBufferBytes > 16<<10 || backpressureWaits == 0 {
		t.Fatalf("aggregate buffer evidence chunks=%d bytes=%d waits=%d", maxBufferChunks, maxBufferBytes, backpressureWaits)
	}
	for _, scenario := range []pressureScenario{
		pressureNormal, pressureSlowClient, pressureCancel, pressureDisconnect, pressureLongTTFT,
	} {
		if totals[scenario] != pressureBatches*(pressureSessionsPerBatch/5) {
			t.Fatalf("scenario %s sessions = %d", scenario, totals[scenario])
		}
	}

	server.Close()
	serverClosed = true
	t.Logf(
		"sessions=%d duration=%s peak_handlers=%d max_buffer_chunks=%d max_buffer_bytes=%d backpressure_waits=%d goroutines=%d->%d heap=%d->%d",
		pressureBatches*pressureSessionsPerBatch, time.Since(startedAt), serverMetrics.peak.Load(),
		maxBufferChunks, maxBufferBytes, backpressureWaits, baselineGoroutines, runtime.NumGoroutine(),
		baselineMemory.HeapAlloc, finalMemory.HeapAlloc,
	)
}

func makePressureSpecs(batch int, random *rand.Rand) []pressureSpec {
	scenarios := []pressureScenario{
		pressureNormal, pressureSlowClient, pressureCancel, pressureDisconnect, pressureLongTTFT,
	}
	specs := make([]pressureSpec, 0, pressureSessionsPerBatch)
	for index := range pressureSessionsPerBatch {
		specs = append(specs, pressureSpec{
			id: batch*pressureSessionsPerBatch + index, scenario: scenarios[index%len(scenarios)],
			cancelDelay: time.Duration(1+random.Intn(15)) * time.Millisecond, //nolint:gosec // deterministic fixture.
		})
	}
	random.Shuffle(len(specs), func(left, right int) { specs[left], specs[right] = specs[right], specs[left] })
	return specs
}

func runPressureBatch(
	t *testing.T,
	client *upstreamhttp.Client,
	built provideradapter.Adapter,
	specs []pressureSpec,
) []pressureResult {
	t.Helper()
	batchCtx, cancelBatch := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancelBatch()
	results := make(chan pressureResult, len(specs))
	var wait sync.WaitGroup
	wait.Add(len(specs))
	for _, spec := range specs {
		go func() {
			defer wait.Done()
			results <- runPressureSession(batchCtx, client, built, spec)
		}()
	}
	wait.Wait()
	close(results)
	collected := make([]pressureResult, 0, len(specs))
	for result := range results {
		collected = append(collected, result)
	}
	if len(collected) != len(specs) {
		t.Fatalf("pressure results = %d, want %d", len(collected), len(specs))
	}
	return collected
}

func runPressureSession(
	parent context.Context,
	client *upstreamhttp.Client,
	built provideradapter.Adapter,
	spec pressureSpec,
) (result pressureResult) {
	result.spec = spec
	startedAt := time.Now()
	defer func() { result.duration = time.Since(startedAt) }()
	clientCtx, cancelClient := context.WithCancelCause(parent)
	defer cancelClient(nil)

	buffer, err := NewBuffer(clientCtx, BufferOptions{
		MaxChunks: 4, MaxBytes: 16 << 10, BackpressureTimeout: 250 * time.Millisecond,
	})
	if err != nil {
		result.setupErr = err
		return result
	}
	firstTokenTimeout := time.Second
	if spec.scenario == pressureLongTTFT {
		firstTokenTimeout = 50 * time.Millisecond
	}
	controller, err := NewTimeoutController(buffer.Context(), TimeoutOptions{
		FirstTokenTimeout: firstTokenTimeout, NoProgressTimeout: 750 * time.Millisecond, TotalTimeout: 4 * time.Second,
	})
	if err != nil {
		result.setupErr = err
		return result
	}
	defer func() { _ = controller.Close() }()

	request, err := built.BuildRequest(controller.Context(), cancellationRequest(spec.id))
	if err != nil {
		result.setupErr = err
		return result
	}
	request.Header.Set("X-Pressure-Scenario", string(spec.scenario))
	response, err := client.DoStream(request)
	if err != nil {
		result.setupErr = err
		return result
	}
	defer func() { _ = response.Body.Close() }()
	opened, err := built.OpenStream(controller.Context(), response)
	if err != nil {
		result.setupErr = err
		return result
	}
	stream, err := controller.Attach(opened)
	if err != nil {
		result.setupErr = err
		return result
	}
	defer func() { _ = stream.Close() }()
	aggregator, err := NewUsageAggregator(UsageAggregatorOptions{MaxUsageEvents: 8})
	if err != nil {
		result.setupErr = err
		return result
	}
	if err := aggregator.SetLocalEstimate(adapter.NormalizedUsage{
		InputTokens: adapter.Tokens(1), Source: adapter.UsageSourceEstimated,
	}); err != nil {
		result.setupErr = err
		return result
	}
	gate, err := NewFailoverGate(FailoverOptions{MaxAttempts: 1})
	if err != nil {
		result.setupErr = err
		return result
	}
	token, err := gate.StartInitial(controller.Context(), "22222222-2222-4222-8222-222222222222", func(context.Context, AttemptStart) error {
		return nil
	})
	if err != nil {
		result.setupErr = err
		return result
	}

	producerDone := make(chan error, 1)
	go func() {
		for {
			chunk, nextErr := stream.Next(controller.Context())
			if nextErr != nil {
				_ = aggregator.ClosePartial()
				if nextErr == io.EOF { //nolint:errorlint // wrapped EOF is a protocol failure, not clean completion.
					buffer.Finish(nil)
				} else {
					buffer.Finish(nextErr)
				}
				producerDone <- nextErr
				return
			}
			if observeErr := aggregator.Observe(chunk); observeErr != nil {
				buffer.Abort(observeErr)
				producerDone <- observeErr
				return
			}
			if pushErr := buffer.Push(controller.Context(), chunk); pushErr != nil {
				producerDone <- pushErr
				return
			}
		}
	}()

	cancelled := false
	for {
		chunk, nextErr := buffer.Next(clientCtx)
		if nextErr != nil {
			result.consumerErr = nextErr
			break
		}
		forwardErr := gate.Forward(clientCtx, token, chunk, func(_ context.Context, forwarded adapter.NormalizedChunk) error {
			if isModelOutput(forwarded.Kind) {
				result.modelChunks++
			}
			return nil
		})
		if forwardErr != nil {
			result.consumerErr = forwardErr
			cancelClient(forwardErr)
			break
		}
		if spec.scenario == pressureSlowClient {
			time.Sleep(5 * time.Millisecond)
		}
		if spec.scenario == pressureCancel && result.modelChunks > 0 && !cancelled {
			cancelled = true
			time.Sleep(spec.cancelDelay)
			cancelClient(errPressureClientCancelled)
		}
	}

	select {
	case result.producerErr = <-producerDone:
	case <-time.After(2 * time.Second):
		cancelClient(errPressureSessionTimeout)
		select {
		case result.producerErr = <-producerDone:
		case <-time.After(time.Second):
			result.setupErr = errPressureSessionTimeout
		}
	}
	_ = gate.Close(token)
	result.buffer = buffer.Stats()
	result.timeout = controller.Snapshot()
	result.failover = gate.Snapshot()
	result.usage, result.usageOrigin = aggregator.KnownUsage()
	return result
}

func assertPressureResult(t *testing.T, result pressureResult) {
	t.Helper()
	if result.setupErr != nil {
		t.Fatalf("session %d (%s) setup error = %v", result.spec.id, result.spec.scenario, result.setupErr)
	}
	if result.duration > 5*time.Second || result.buffer.MaxObservedChunks > 4 || result.buffer.MaxObservedBytes > 16<<10 ||
		result.failover.AttemptsStarted != 1 || !result.failover.Closed {
		t.Fatalf("session %d (%s) bounds = duration:%s buffer:%+v failover:%+v",
			result.spec.id, result.spec.scenario, result.duration, result.buffer, result.failover)
	}
	switch result.spec.scenario {
	case pressureNormal:
		assertPressureComplete(t, result, 4, false)
	case pressureSlowClient:
		assertPressureComplete(t, result, pressureSlowChunks, true)
	case pressureCancel:
		if !errors.Is(result.producerErr, context.Canceled) || !errors.Is(result.consumerErr, context.Canceled) ||
			result.modelChunks != 1 || result.timeout.CancellationObservedAt == nil ||
			result.timeout.UpstreamReleasedAt == nil || !result.timeout.ModelOutputStarted ||
			result.usageOrigin != UsageOriginLocalEstimate {
			t.Fatalf("cancel session %d result = %+v", result.spec.id, result)
		}
	case pressureDisconnect:
		producerProtocol := errors.Is(result.producerErr, mockadapter.ErrProtocol)
		consumerProtocol := errors.Is(result.consumerErr, mockadapter.ErrProtocol)
		if !producerProtocol || !consumerProtocol || result.modelChunks != 1 ||
			!result.timeout.ModelOutputStarted || result.timeout.TimedOut ||
			result.usageOrigin != UsageOriginLocalEstimate {
			t.Fatalf("disconnect session %d producer=%T %v protocol=%t consumer=%T %v protocol=%t result=%+v",
				result.spec.id, result.producerErr, result.producerErr, producerProtocol,
				result.consumerErr, result.consumerErr, consumerProtocol, result)
		}
	case pressureLongTTFT:
		if !errors.Is(result.producerErr, ErrFirstTokenTimeout) ||
			!errors.Is(result.consumerErr, ErrFirstTokenTimeout) || result.modelChunks != 0 ||
			!result.timeout.TimedOut || result.timeout.TimeoutKind != TimeoutFirstToken ||
			!result.timeout.RetryEligibleBeforeOutput || result.timeout.PartialFailure ||
			result.usageOrigin != UsageOriginLocalEstimate {
			t.Fatalf("TTFT session %d result = %+v", result.spec.id, result)
		}
	default:
		t.Fatalf("session %d unknown scenario %s", result.spec.id, result.spec.scenario)
	}
}

func assertPressureComplete(t *testing.T, result pressureResult, modelChunks int, backpressure bool) {
	t.Helper()
	if !errors.Is(result.producerErr, io.EOF) || !errors.Is(result.consumerErr, io.EOF) ||
		result.modelChunks != modelChunks || !result.timeout.ModelOutputStarted || result.timeout.TimedOut ||
		result.usageOrigin != UsageOriginProviderFinal || result.usage == nil || !result.usage.Complete {
		t.Fatalf("complete session %d (%s) result = %+v", result.spec.id, result.spec.scenario, result)
	}
	if backpressure && result.buffer.BackpressureWaits == 0 {
		t.Fatalf("slow session %d did not apply backpressure: %+v", result.spec.id, result.buffer)
	}
}

func pressureProviderHandler(metrics *pressureServerMetrics) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		metrics.begin()
		defer metrics.active.Add(-1)
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		flusher, ok := writer.(http.Flusher)
		if !ok {
			return
		}
		scenario := pressureScenario(request.Header.Get("X-Pressure-Scenario"))
		pressureWriteSSE(writer, cancellationStartEvent)
		switch scenario {
		case pressureNormal:
			for index := range 4 {
				pressureWriteSSE(writer, pressureContentEvent(fmt.Sprintf("normal-%d", index)))
			}
			pressureWriteTerminal(writer)
			flusher.Flush()
		case pressureSlowClient:
			for index := range pressureSlowChunks {
				pressureWriteSSE(writer, pressureContentEvent(fmt.Sprintf("slow-%d", index)))
			}
			pressureWriteTerminal(writer)
			flusher.Flush()
		case pressureCancel:
			pressureWriteSSE(writer, pressureContentEvent("cancel-visible"))
			flusher.Flush()
			<-request.Context().Done()
		case pressureDisconnect:
			pressureWriteSSE(writer, pressureContentEvent("disconnect-visible"))
			flusher.Flush()
		case pressureLongTTFT:
			_, _ = io.WriteString(writer, ": provider-heartbeat\n\n")
			flusher.Flush()
			<-request.Context().Done()
		default:
			flusher.Flush()
		}
	})
}

func (metrics *pressureServerMetrics) begin() {
	active := metrics.active.Add(1)
	for {
		peak := metrics.peak.Load()
		if active <= peak || metrics.peak.CompareAndSwap(peak, active) {
			return
		}
	}
}

func pressureWriteSSE(writer io.Writer, event string) {
	_, _ = fmt.Fprintf(writer, "data: %s\n\n", event)
}

func pressureWriteTerminal(writer io.Writer) {
	pressureWriteSSE(writer, `{"id":"pressure","object":"chat.completion.chunk","created":0,"model":"mock-chat-v1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":6,"completion_tokens":4,"total_tokens":10}}`)
	_, _ = io.WriteString(writer, "data: [DONE]\n\n")
}

func pressureContentEvent(content string) string {
	return fmt.Sprintf(`{"id":"pressure","object":"chat.completion.chunk","created":0,"model":"mock-chat-v1","choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, content)
}

func newPressureHTTPClient(t *testing.T) *upstreamhttp.Client {
	t.Helper()
	client, err := upstreamhttp.NewClient(upstreamhttp.Options{
		ConnectTimeout: time.Second, KeepAlive: time.Second, TLSHandshakeTimeout: time.Second,
		ResponseHeaderTimeout: time.Second, TotalTimeout: 5 * time.Second, IdleConnTimeout: time.Second,
		ExpectContinueTimeout: time.Second, MaxIdleConns: 128, MaxIdleConnsPerHost: 128,
		MaxConnsPerHost: 128, MaxResponseHeaderBytes: 64 << 10,
	})
	if err != nil {
		t.Fatalf("upstreamhttp.NewClient() error = %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return client
}
