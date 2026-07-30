package adapterconformance_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/adapterconformance"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/provideradapter"
)

var errScriptProtocol = errors.New("scripted provider protocol error")

func TestSuiteExecutesEveryRegisteredFixtureOverHTTP(t *testing.T) {
	adapterconformance.Run(t, scriptedRegistration(t))
}

func scriptedRegistration(t *testing.T) adapterconformance.Registration {
	t.Helper()
	registration := validRegistration(t)
	responseFixtures := make([]*adapterconformance.ResponseFixture, 0, 3+len(registration.Fixtures.FinishReasons))
	responseFixtures = append(responseFixtures,
		&registration.Fixtures.Ordinary,
		&registration.Fixtures.CachedUsage,
		&registration.Fixtures.ToolCall,
	)
	for index := range registration.Fixtures.FinishReasons {
		responseFixtures = append(responseFixtures, &registration.Fixtures.FinishReasons[index])
	}
	responses := make(map[string]adapter.NormalizedResponse, len(responseFixtures))
	for _, fixture := range responseFixtures {
		setScriptCase(&fixture.Request, fixture.Name)
		fixture.NewHandler = newScriptedHandler(fixture.Name, http.StatusOK)
		responses[fixture.Name] = fixture.Want.Clone()
	}

	streamFixtures := []*adapterconformance.StreamFixture{
		&registration.Fixtures.Stream,
		&registration.Fixtures.UnknownStream,
	}
	streams := make(map[string][]adapter.NormalizedChunk, len(streamFixtures))
	for _, fixture := range streamFixtures {
		setScriptCase(&fixture.Request, fixture.Name)
		fixture.NewHandler = newScriptedHandler(fixture.Name, http.StatusOK)
		streams[fixture.Name] = cloneChunks(fixture.Want)
	}

	errorFixtures := []*adapterconformance.ErrorFixture{
		&registration.Fixtures.RateLimit,
		&registration.Fixtures.ProviderFailure,
	}
	errorsByCase := make(map[string]adapter.NormalizedError, len(errorFixtures))
	for _, fixture := range errorFixtures {
		setScriptCase(&fixture.Request, fixture.Name)
		status := fixture.Want.ProviderStatus
		fixture.NewHandler = newScriptedHandler(fixture.Name, status)
		errorsByCase[fixture.Name] = fixture.Want.Clone()
	}

	setScriptCase(&registration.Fixtures.UnknownOrdinary.Request, registration.Fixtures.UnknownOrdinary.Name)
	registration.Fixtures.UnknownOrdinary.NewHandler = newScriptedHandler(
		registration.Fixtures.UnknownOrdinary.Name,
		http.StatusOK,
	)
	registration.Fixtures.UnknownOrdinary.Want = errScriptProtocol
	setScriptCase(&registration.Fixtures.Cancellation.Request, registration.Fixtures.Cancellation.Name)
	registration.Fixtures.Cancellation.NewHandler = newScriptedCancellationHandler

	registration.Name = "scripted"
	registration.NewAdapter = func(_ context.Context, endpoint string) (provideradapter.Adapter, error) {
		parsed, err := url.Parse(endpoint)
		if err != nil {
			return nil, err
		}
		return &scriptedAdapter{
			endpoint: parsed, responses: responses, streams: streams, errors: errorsByCase,
			protocolCase: registration.Fixtures.UnknownOrdinary.Name,
		}, nil
	}
	return registration
}

func setScriptCase(request *adapter.NormalizedRequest, name string) {
	request.ProviderOptions = json.RawMessage(`{"case":"` + name + `"}`)
}

func newScriptedHandler(name string, status int) adapterconformance.HandlerFactory {
	return func() http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("X-Conformance-Case", name)
			writer.Header().Set("Content-Type", "application/octet-stream")
			writer.WriteHeader(status)
			_, _ = io.WriteString(writer, "real-http-fixture")
		})
	}
}

func newScriptedCancellationHandler(cancelled chan<- struct{}) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Conformance-Case", "cancellation")
		writer.Header().Set("Content-Type", "application/octet-stream")
		writer.WriteHeader(http.StatusOK)
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		<-request.Context().Done()
		close(cancelled)
	})
}

type scriptedAdapter struct {
	endpoint     *url.URL
	responses    map[string]adapter.NormalizedResponse
	streams      map[string][]adapter.NormalizedChunk
	errors       map[string]adapter.NormalizedError
	protocolCase string
}

func (*scriptedAdapter) Type() provideradapter.Type {
	return "scripted"
}

func (*scriptedAdapter) Capabilities(context.Context) catalog.CapabilitySet {
	return catalog.CapabilitySet{
		Chat: true, Stream: true, Tools: true, UsageInStream: true, CacheUsage: true,
		MaxContextTokens: 1024, MaxOutputTokens: 256,
		DataRetentionMode: catalog.RetentionSelfHosted, ProviderProtocolVersion: "scripted-v1",
	}
}

func (script *scriptedAdapter) BuildRequest(
	ctx context.Context,
	request adapter.NormalizedRequest,
) (*http.Request, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var options struct {
		Case string `json:"case"`
	}
	if err := json.Unmarshal(request.ProviderOptions, &options); err != nil {
		return nil, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, script.endpoint.String(), bytes.NewReader([]byte("request")))
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("X-Conformance-Case", options.Case)
	return httpRequest, nil
}

func (script *scriptedAdapter) ParseResponse(
	_ context.Context,
	response *http.Response,
) (adapter.NormalizedResponse, error) {
	defer func() { _ = response.Body.Close() }()
	if _, err := io.ReadAll(response.Body); err != nil {
		return adapter.NormalizedResponse{}, err
	}
	selected := response.Header.Get("X-Conformance-Case")
	if selected == script.protocolCase {
		return adapter.NormalizedResponse{}, errScriptProtocol
	}
	if normalized, exists := script.errors[selected]; exists {
		return adapter.NormalizedResponse{}, normalized
	}
	normalized, exists := script.responses[selected]
	if !exists {
		return adapter.NormalizedResponse{}, errScriptProtocol
	}
	return normalized.Clone(), nil
}

func (script *scriptedAdapter) OpenStream(
	_ context.Context,
	response *http.Response,
) (provideradapter.ChunkStream, error) {
	selected := response.Header.Get("X-Conformance-Case")
	return &scriptedStream{
		body: response.Body, chunks: cloneChunks(script.streams[selected]), blocking: selected == "cancellation",
	}, nil
}

func (script *scriptedAdapter) NormalizeError(
	_ context.Context,
	response *http.Response,
	_ []byte,
) adapter.NormalizedError {
	return script.errors[response.Header.Get("X-Conformance-Case")].Clone()
}

func (*scriptedAdapter) EstimateUsage(
	context.Context,
	adapter.NormalizedRequest,
) (adapter.NormalizedUsage, error) {
	return adapter.NormalizedUsage{}, errors.New("scripted estimate unavailable")
}

type scriptedStream struct {
	body        io.ReadCloser
	chunks      []adapter.NormalizedChunk
	blocking    bool
	initialized bool
	closed      bool
	closeOnce   sync.Once
	closeErr    error
}

func (stream *scriptedStream) Next(ctx context.Context) (adapter.NormalizedChunk, error) {
	if stream.blocking {
		stopClose := context.AfterFunc(ctx, func() { _ = stream.Close() })
		defer stopClose()
		var buffer [1]byte
		_, err := stream.body.Read(buffer[:])
		if ctx.Err() != nil {
			return adapter.NormalizedChunk{}, ctx.Err()
		}
		return adapter.NormalizedChunk{}, err
	}
	if !stream.initialized {
		stream.initialized = true
		if _, err := io.ReadAll(stream.body); err != nil {
			return adapter.NormalizedChunk{}, err
		}
		if err := stream.Close(); err != nil {
			return adapter.NormalizedChunk{}, err
		}
	}
	if len(stream.chunks) == 0 {
		return adapter.NormalizedChunk{}, io.EOF
	}
	chunk := stream.chunks[0]
	stream.chunks = stream.chunks[1:]
	return chunk.Clone(), nil
}

func (stream *scriptedStream) Close() error {
	stream.closeOnce.Do(func() {
		stream.closed = true
		stream.closeErr = stream.body.Close()
	})
	return stream.closeErr
}

func cloneChunks(chunks []adapter.NormalizedChunk) []adapter.NormalizedChunk {
	cloned := make([]adapter.NormalizedChunk, len(chunks))
	for index := range chunks {
		cloned[index] = chunks[index].Clone()
	}
	return cloned
}

var (
	_ provideradapter.Adapter     = (*scriptedAdapter)(nil)
	_ provideradapter.ChunkStream = (*scriptedStream)(nil)
)

func TestScriptedAdapterCancellationAfterClosedBody(t *testing.T) {
	t.Parallel()

	stream := &scriptedStream{body: io.NopCloser(bytes.NewReader(nil)), blocking: true}
	if err := stream.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := stream.Next(ctx); err == nil {
		t.Fatal("closed scripted stream returned no error")
	}
}
