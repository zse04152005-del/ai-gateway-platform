package streaming

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/mockadapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/provideradapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/upstreamhttp"
)

const cancellationIterations = 25

func TestStreamingClientCancellationReleasesRealUpstreamConnectionsAndGoroutines(t *testing.T) {
	sessions := make(chan *cancellationSession, cancellationIterations)
	connections := newCancellationConnections()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		session := &cancellationSession{released: make(chan time.Time, 1)}
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(writer, "data: %s\n\n", cancellationStartEvent)
		_, _ = fmt.Fprintf(writer, "data: %s\n\n", cancellationContentEvent)
		writer.(http.Flusher).Flush()
		sessions <- session
		<-request.Context().Done()
		session.released <- time.Now()
	}))
	server.Config.ConnState = connections.observe
	server.Start()
	t.Cleanup(server.Close)

	client := newCancellationHTTPClient(t)
	factory, err := mockadapter.NewFactory(mockadapter.FactoryOptions{})
	if err != nil {
		t.Fatalf("mockadapter.NewFactory() error = %v", err)
	}
	baselineGoroutines := runtime.NumGoroutine()
	for iteration := range cancellationIterations {
		clientCtx, cancelClient := context.WithCancelCause(context.Background())
		controller, controllerErr := NewTimeoutController(clientCtx, TimeoutOptions{
			FirstTokenTimeout: time.Second, NoProgressTimeout: 5 * time.Second, TotalTimeout: 10 * time.Second,
		})
		if controllerErr != nil {
			t.Fatalf("iteration %d NewTimeoutController() error = %v", iteration, controllerErr)
		}
		built := newCancellationAdapter(controller.Context(), t, factory, server.URL)
		request, requestErr := built.BuildRequest(controller.Context(), cancellationRequest(iteration))
		if requestErr != nil {
			t.Fatalf("iteration %d BuildRequest() error = %v", iteration, requestErr)
		}
		response, requestErr := client.DoStream(request)
		if requestErr != nil {
			t.Fatalf("iteration %d DoStream() error = %v", iteration, requestErr)
		}
		t.Cleanup(func() { _ = response.Body.Close() })
		opened, openErr := built.OpenStream(controller.Context(), response)
		if openErr != nil {
			t.Fatalf("iteration %d OpenStream() error = %v", iteration, openErr)
		}
		stream, attachErr := controller.Attach(opened)
		if attachErr != nil {
			t.Fatalf("iteration %d Attach() error = %v", iteration, attachErr)
		}
		first, nextErr := stream.Next(clientCtx)
		if nextErr != nil || first.Kind != adapter.ChunkMessageStart {
			t.Fatalf("iteration %d first Next() = %+v/%v", iteration, first, nextErr)
		}
		second, nextErr := stream.Next(clientCtx)
		if nextErr != nil || second.Kind != adapter.ChunkContentDelta || second.ContentDelta != "visible" {
			t.Fatalf("iteration %d second Next() = %+v/%v", iteration, second, nextErr)
		}
		session := <-sessions
		result := make(chan error, 1)
		go func() {
			_, blockedErr := stream.Next(clientCtx)
			result <- blockedErr
		}()
		privateCause := fmt.Errorf("client cancellation fixture %d", iteration)
		cancelledAt := time.Now()
		cancelClient(privateCause)

		select {
		case nextErr = <-result:
			if !errors.Is(nextErr, context.Canceled) || !errors.Is(nextErr, privateCause) {
				t.Fatalf("iteration %d cancelled Next() error = %v", iteration, nextErr)
			}
		case <-time.After(time.Second):
			t.Fatalf("iteration %d guarded stream did not unblock", iteration)
		}
		select {
		case releasedAt := <-session.released:
			if propagation := releasedAt.Sub(cancelledAt); propagation < 0 || propagation > time.Second {
				t.Fatalf("iteration %d provider cancellation propagation = %s", iteration, propagation)
			}
		case <-time.After(time.Second):
			t.Fatalf("iteration %d provider request Context was not released", iteration)
		}
		snapshot := controller.Snapshot()
		if snapshot.CancellationObservedAt == nil || snapshot.UpstreamReleasedAt == nil ||
			snapshot.CancellationPropagation < 0 || snapshot.CancellationPropagation > time.Second ||
			!snapshot.ModelOutputStarted || snapshot.PartialFailure {
			t.Fatalf("iteration %d cancellation snapshot = %+v", iteration, snapshot)
		}
		if closeErr := stream.Close(); closeErr != nil {
			t.Fatalf("iteration %d stream.Close() error = %v", iteration, closeErr)
		}
	}

	client.CloseIdleConnections()
	assertEventually(t, 2*time.Second, func() bool { return connections.count() == 0 }, "upstream connections did not close")
	assertEventually(t, 2*time.Second, func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= baselineGoroutines+6
	}, "stream cancellation leaked goroutines")
}

const (
	cancellationStartEvent   = `{"id":"cancel-test","object":"chat.completion.chunk","created":0,"model":"mock-chat-v1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`
	cancellationContentEvent = `{"id":"cancel-test","object":"chat.completion.chunk","created":0,"model":"mock-chat-v1","choices":[{"index":0,"delta":{"content":"visible"},"finish_reason":null}]}`
)

type cancellationSession struct {
	released chan time.Time
}

type cancellationConnections struct {
	mu     sync.Mutex
	active map[net.Conn]struct{}
}

func newCancellationConnections() *cancellationConnections {
	return &cancellationConnections{active: make(map[net.Conn]struct{})}
}

func (connections *cancellationConnections) observe(connection net.Conn, state http.ConnState) {
	connections.mu.Lock()
	defer connections.mu.Unlock()
	switch state {
	case http.StateNew:
		connections.active[connection] = struct{}{}
	case http.StateClosed, http.StateHijacked:
		delete(connections.active, connection)
	case http.StateActive, http.StateIdle:
		// Existing connection remains tracked.
	}
}

func (connections *cancellationConnections) count() int {
	connections.mu.Lock()
	defer connections.mu.Unlock()
	return len(connections.active)
}

func newCancellationHTTPClient(t *testing.T) *upstreamhttp.Client {
	t.Helper()
	client, err := upstreamhttp.NewClient(upstreamhttp.Options{
		ConnectTimeout: time.Second, KeepAlive: time.Second,
		TLSHandshakeTimeout: time.Second, ResponseHeaderTimeout: time.Second,
		TotalTimeout: 3 * time.Second, IdleConnTimeout: time.Second,
		ExpectContinueTimeout: time.Second, MaxIdleConns: 10,
		MaxIdleConnsPerHost: 5, MaxConnsPerHost: 10, MaxResponseHeaderBytes: 64 << 10,
	})
	if err != nil {
		t.Fatalf("upstreamhttp.NewClient() error = %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func newCancellationAdapter(
	ctx context.Context,
	t *testing.T,
	factory *mockadapter.Factory,
	endpoint string,
) provideradapter.Adapter {
	t.Helper()
	now := time.Now().UTC()
	provider := catalog.Provider{
		ID: "11111111-1111-4111-8111-111111111111", Code: "cancel-mock", Name: "Cancel Mock",
		AdapterType: string(mockadapter.Type), Status: catalog.StatusActive, Version: 1,
		CreatedAt: now, CreatedBy: "test", UpdatedAt: now, UpdatedBy: "test",
	}
	deployment := catalog.Deployment{
		ID: "22222222-2222-4222-8222-222222222222", ProviderID: provider.ID,
		Code: "cancel", PhysicalModel: "mock-chat-v1", EndpointURL: endpoint, Region: "local",
		Capabilities: catalog.CapabilitySet{
			Chat: true, Stream: true, Tools: true, UsageInStream: true,
			MaxContextTokens: 8192, MaxOutputTokens: 2048,
			DataRetentionMode: catalog.RetentionSelfHosted, ProviderProtocolVersion: "cancel-v1",
		},
		Status: catalog.StatusActive, Version: 1,
		CreatedAt: now, CreatedBy: "test", UpdatedAt: now, UpdatedBy: "test",
	}
	built, err := factory.New(ctx, provider, deployment)
	if err != nil {
		t.Fatalf("mock adapter Factory.New() error = %v", err)
	}
	return built
}

func cancellationRequest(iteration int) adapter.NormalizedRequest {
	return adapter.NormalizedRequest{
		RequestID: fmt.Sprintf("request-stream-cancel-%04d", iteration), LogicalModel: "logical-chat", Stream: true,
		Messages: []adapter.Message{{
			Role: adapter.RoleUser, Parts: []adapter.ContentPart{{Kind: adapter.ContentText, Text: "fixture"}},
		}},
		ProviderOptions: json.RawMessage(`{"mock_scenario":"sse"}`),
	}
}

func assertEventually(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !condition() {
		t.Fatal(message)
	}
}
