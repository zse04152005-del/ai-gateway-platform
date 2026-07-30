package adapterconformance

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/httpserver"
	"github.com/zse04152005-del/ai-gateway-platform/internal/provideradapter"
)

const fixtureTimeout = 5 * time.Second

type runtime struct {
	adapter provideradapter.Adapter
	client  *http.Client
	origin  *url.URL
}

func newRuntime(t *testing.T, build AdapterBuilder, handler http.Handler) runtime {
	t.Helper()
	if handler == nil {
		t.Fatal("fixture handler factory returned nil")
	}
	shared, err := httpserver.NewServer(httpserver.Options{
		ServiceName: "adapter-conformance", Version: "test",
		NotReadyCode: "CONFORMANCE_NOT_READY", NotReadyMessage: "Conformance fixture is not ready",
		ErrorType: "conformance_error", ReadHeaderTimeout: time.Second, ShutdownTimeout: time.Second,
		ApplicationHandler: handler,
	})
	if err != nil {
		t.Fatalf("create shared fixture server: %v", err)
	}
	server := httptest.NewServer(shared.Handler())
	t.Cleanup(server.Close)
	origin, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse fixture server origin: %v", err)
	}
	built, err := build(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("build adapter for fixture endpoint: %v", err)
	}
	if built == nil {
		t.Fatal("adapter builder returned nil without an error")
	}
	return runtime{adapter: built, client: server.Client(), origin: origin}
}

func (rt runtime) execute(t *testing.T, request adapter.NormalizedRequest) (*http.Response, context.Context) {
	t.Helper()
	before := request.Clone()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	t.Cleanup(cancel)
	httpRequest, err := rt.adapter.BuildRequest(ctx, request)
	if err != nil {
		t.Fatalf("build fixture request: %v", err)
	}
	if httpRequest == nil {
		t.Fatal("BuildRequest returned nil without an error")
	}
	if httpRequest.URL == nil || httpRequest.URL.Scheme != rt.origin.Scheme || httpRequest.URL.Host != rt.origin.Host {
		t.Fatalf("BuildRequest escaped the isolated fixture origin: %v", httpRequest.URL)
	}
	if !reflect.DeepEqual(before, request) {
		t.Fatal("BuildRequest mutated the registered normalized request")
	}
	response, err := rt.client.Do(httpRequest)
	if err != nil {
		t.Fatalf("execute fixture request over HTTP: %v", err)
	}
	return response, ctx
}

func assertResponse(t *testing.T, got, want adapter.NormalizedResponse) {
	t.Helper()
	if err := got.Validate(); err != nil {
		t.Fatalf("adapter returned an invalid normalized response: %v", err)
	}
	if got.ObservedAt.IsZero() {
		t.Fatal("normalized response omitted observed_at")
	}
	got = canonicalResponse(got)
	want = canonicalResponse(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized response mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func assertChunks(t *testing.T, got, want []adapter.NormalizedChunk) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("normalized chunk count = %d, want %d\nchunks: %#v", len(got), len(want), got)
	}
	for index := range got {
		if err := got[index].Validate(); err != nil {
			t.Fatalf("normalized chunk %d is invalid: %v", index, err)
		}
		if got[index].Sequence != uint64(index) {
			t.Fatalf("normalized chunk %d sequence = %d", index, got[index].Sequence)
		}
		actual := canonicalChunk(got[index])
		expected := canonicalChunk(want[index])
		if !reflect.DeepEqual(actual, expected) {
			t.Fatalf("normalized chunk %d mismatch\n got: %#v\nwant: %#v", index, actual, expected)
		}
	}
}

func assertNormalizedError(t *testing.T, err error, want adapter.NormalizedError, forbidden []string) {
	t.Helper()
	var got adapter.NormalizedError
	if !errors.As(err, &got) {
		t.Fatalf("error %T is not a NormalizedError: %v", err, err)
	}
	if validateErr := got.Validate(); validateErr != nil {
		t.Fatalf("adapter returned an invalid normalized error: %v", validateErr)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized error mismatch\n got: %#v\nwant: %#v", got, want)
	}
	assertForbidden(t, err.Error(), forbidden)
	assertForbidden(t, got.Error(), forbidden)
}

func assertForbidden(t *testing.T, value string, forbidden []string) {
	t.Helper()
	for _, marker := range forbidden {
		if strings.Contains(value, marker) {
			t.Fatalf("error exposed forbidden fixture marker %q", marker)
		}
	}
}

func canonicalResponse(response adapter.NormalizedResponse) adapter.NormalizedResponse {
	response = response.Clone()
	response.ObservedAt = time.Time{}
	for index := range response.Choices {
		canonicalMessage(&response.Choices[index].Message)
	}
	if response.Usage != nil && len(response.Usage.UnmappedFields) == 0 {
		response.Usage.UnmappedFields = nil
	}
	return response
}

func canonicalChunk(chunk adapter.NormalizedChunk) adapter.NormalizedChunk {
	chunk = chunk.Clone()
	chunk.ObservedAt = time.Time{}
	if chunk.Usage != nil && len(chunk.Usage.UnmappedFields) == 0 {
		chunk.Usage.UnmappedFields = nil
	}
	if len(chunk.ProviderExtension) == 0 {
		chunk.ProviderExtension = nil
	}
	return chunk
}

func canonicalMessage(message *adapter.Message) {
	if len(message.Parts) == 0 {
		message.Parts = nil
	}
	if len(message.ToolCalls) == 0 {
		message.ToolCalls = nil
	}
}

func unexpectedStreamTermination(err error) error {
	if err == nil {
		return errors.New("stream returned no error and no chunk")
	}
	return fmt.Errorf("stream terminated unexpectedly: %w", err)
}
