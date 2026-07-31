package activehealth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/mockadapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/mockprovider"
	"github.com/zse04152005-del/ai-gateway-platform/internal/provideradapter"
)

func TestAdapterProberUsesFixedOneTokenRequestAndTrafficClass(t *testing.T) {
	t.Parallel()
	var requests atomic.Uint64
	providerHandler := mockprovider.NewHandler()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.Header.Get(probeHeaderName) != probeHeaderValue {
			t.Errorf("probe header = %q", request.Header.Get(probeHeaderName))
		}
		var body struct {
			MaxTokens int64 `json:"max_tokens"`
			Messages  []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		rawBody, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			t.Errorf("read probe body: %v", readErr)
		}
		request.Body = io.NopCloser(bytes.NewReader(rawBody))
		if err := json.Unmarshal(rawBody, &body); err != nil {
			t.Errorf("decode probe body: %v", err)
		} else if body.MaxTokens != 1 || len(body.Messages) != 1 || body.Messages[0].Content != "ping" {
			t.Errorf("probe body = %+v", body)
		}
		providerHandler.ServeHTTP(writer, request)
	}))
	t.Cleanup(server.Close)
	target := activeTarget(1, server.URL)
	factory, err := mockadapter.NewFactory(mockadapter.FactoryOptions{})
	if err != nil {
		t.Fatalf("mockadapter.NewFactory() error = %v", err)
	}
	registry, err := provideradapter.NewRegistry(factory)
	if err != nil {
		t.Fatalf("provideradapter.NewRegistry() error = %v", err)
	}
	prober, err := NewAdapterProber(registry, server.Client(), time.Now)
	if err != nil {
		t.Fatalf("NewAdapterProber() error = %v", err)
	}
	result := prober.Probe(context.Background(), target)
	if result.Code != ResultSucceeded || requests.Load() != 1 {
		t.Fatalf("Probe() = %+v, requests = %d", result, requests.Load())
	}
}

func TestAdapterProberClassifiesProviderFailureAndCancellation(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "private provider body", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	target := activeTarget(2, server.URL)
	factory, _ := mockadapter.NewFactory(mockadapter.FactoryOptions{})
	registry, _ := provideradapter.NewRegistry(factory)
	prober, err := NewAdapterProber(registry, server.Client(), time.Now)
	if err != nil {
		t.Fatalf("NewAdapterProber() error = %v", err)
	}
	if result := prober.Probe(context.Background(), target); result.Code != ResultProviderFailure {
		t.Fatalf("provider failure = %+v", result)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if result := prober.Probe(cancelled, target); result.Code != ResultCancelled {
		t.Fatalf("cancelled result = %+v", result)
	}
	deadline, deadlineCancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer deadlineCancel()
	if result := prober.Probe(deadline, target); result.Code != ResultTimedOut {
		t.Fatalf("deadline result = %+v", result)
	}
	if _, err := NewAdapterProber(nil, server.Client(), time.Now); err == nil {
		t.Fatal("NewAdapterProber(nil registry) error = nil")
	}
}

func TestAdapterProberCollapsesTransportAndProtocolFailures(t *testing.T) {
	t.Parallel()
	providerHandler := mockprovider.NewHandler()
	server := httptest.NewServer(providerHandler)
	t.Cleanup(server.Close)
	target := activeTarget(3, server.URL)
	factory, _ := mockadapter.NewFactory(mockadapter.FactoryOptions{})
	registry, _ := provideradapter.NewRegistry(factory)
	transportProber, err := NewAdapterProber(registry, errorHTTPClient{}, time.Now)
	if err != nil {
		t.Fatalf("NewAdapterProber(transport) error = %v", err)
	}
	if result := transportProber.Probe(context.Background(), target); result.Code != ResultTransportFailure {
		t.Fatalf("transport result = %+v", result)
	}

	protocolServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"broken"}`))
	}))
	t.Cleanup(protocolServer.Close)
	protocolTarget := activeTarget(4, protocolServer.URL)
	protocolProber, _ := NewAdapterProber(registry, protocolServer.Client(), time.Now)
	if result := protocolProber.Probe(context.Background(), protocolTarget); result.Code != ResultProtocolFailure {
		t.Fatalf("protocol result = %+v", result)
	}
	if result := (*AdapterProber)(nil).Probe(context.Background(), target); result.Code != ResultAdapterUnavailable {
		t.Fatalf("nil prober result = %+v", result)
	}
}

type errorHTTPClient struct{}

func (errorHTTPClient) Do(*http.Request) (*http.Response, error) {
	return nil, errors.New("private transport failure")
}
