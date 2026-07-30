package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testTimeout = 3 * time.Second

type requestResult struct {
	status int
	err    error
}

func TestNewServerRejectsInvalidTimeouts(t *testing.T) {
	tests := []struct {
		name    string
		options Options
		want    string
	}{
		{
			name: "read header timeout",
			options: Options{
				ShutdownTimeout: time.Second,
			},
			want: "read header timeout",
		},
		{
			name: "shutdown timeout",
			options: Options{
				ReadHeaderTimeout: time.Second,
			},
			want: "shutdown timeout",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewServer(test.options)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewServer() error = %v, want text %q", err, test.want)
			}
		})
	}
}

func TestHealthEndpointsReflectLifecycle(t *testing.T) {
	server := newTestServer(t, nil, time.Second)

	live := serveRequest(server.Handler(), http.MethodGet, "/health/live")
	assertStatus(t, live, http.StatusOK)
	assertHeader(t, live, "Cache-Control", "no-store")
	assertHeader(t, live, "Content-Type", "application/json; charset=utf-8")
	var liveBody healthResponse
	decodeBody(t, live, &liveBody)
	if liveBody.Status != "ok" || liveBody.Version != "test-version" {
		t.Fatalf("live response = %+v, want status ok and test version", liveBody)
	}

	notReady := serveRequest(server.Handler(), http.MethodGet, "/health/ready")
	assertStatus(t, notReady, http.StatusServiceUnavailable)
	assertHeader(t, notReady, "Retry-After", "1")
	var unavailable errorEnvelope
	decodeBody(t, notReady, &unavailable)
	if unavailable.Error.Code != "GATEWAY_NOT_READY" || !unavailable.Error.Retryable {
		t.Fatalf("not-ready response = %+v", unavailable)
	}

	server.ready.Store(true)
	ready := serveRequest(server.Handler(), http.MethodGet, "/health/ready")
	assertStatus(t, ready, http.StatusOK)
	var readyBody healthResponse
	decodeBody(t, ready, &readyBody)
	if readyBody.Status != "ok" {
		t.Fatalf("ready status = %q, want ok", readyBody.Status)
	}

	methodNotAllowed := serveRequest(server.Handler(), http.MethodPost, "/health/live")
	assertStatus(t, methodNotAllowed, http.StatusMethodNotAllowed)
}

func TestServeGracefullyDrainsInFlightRequest(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	application := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-releaseRequest
		writer.WriteHeader(http.StatusNoContent)
	})
	server := newTestServer(t, application, time.Second)
	listener := newTestListener(t)
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(ctx, listener)
	}()

	client := &http.Client{Timeout: testTimeout}
	t.Cleanup(client.CloseIdleConnections)
	baseURL := "http://" + listener.Addr().String()
	waitForStatus(t, client, baseURL+"/health/ready", http.StatusOK)

	requestDone := make(chan requestResult, 1)
	go func() {
		response, err := client.Get(baseURL + "/work")
		if err != nil {
			requestDone <- requestResult{err: err}
			return
		}
		closeErr := response.Body.Close()
		requestDone <- requestResult{status: response.StatusCode, err: closeErr}
	}()
	waitForSignal(t, requestStarted, "application request to start")

	cancel()
	waitForCondition(t, func() bool { return !server.Ready() }, "readiness to turn false")
	select {
	case err := <-serveDone:
		t.Fatalf("Serve() returned before in-flight request drained: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	close(releaseRequest)
	result := waitForRequest(t, requestDone)
	if result.err != nil || result.status != http.StatusNoContent {
		t.Fatalf("in-flight request result = %+v, want status 204 without error", result)
	}
	if err := waitForError(t, serveDone); err != nil {
		t.Fatalf("Serve() error = %v, want nil", err)
	}
}

func TestServeForceClosesAfterShutdownDeadline(t *testing.T) {
	requestStarted := make(chan struct{})
	requestCanceled := make(chan struct{})
	application := http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(requestStarted)
		<-request.Context().Done()
		close(requestCanceled)
	})
	server := newTestServer(t, application, 50*time.Millisecond)
	listener := newTestListener(t)
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(ctx, listener)
	}()

	client := &http.Client{Timeout: testTimeout}
	t.Cleanup(client.CloseIdleConnections)
	baseURL := "http://" + listener.Addr().String()
	waitForStatus(t, client, baseURL+"/health/ready", http.StatusOK)
	requestDone := make(chan error, 1)
	go func() {
		response, err := client.Get(baseURL + "/blocked")
		if response != nil {
			err = errors.Join(err, response.Body.Close())
		}
		requestDone <- err
	}()
	waitForSignal(t, requestStarted, "blocked request to start")

	cancel()
	serveErr := waitForError(t, serveDone)
	if serveErr == nil || !strings.Contains(serveErr.Error(), "shutdown gateway HTTP server") {
		t.Fatalf("Serve() error = %v, want shutdown timeout", serveErr)
	}
	waitForSignal(t, requestCanceled, "forced close to cancel request context")
	select {
	case <-requestDone:
	case <-time.After(testTimeout):
		t.Fatal("client request did not finish after forced close")
	}
}

func newTestServer(t *testing.T, application http.Handler, shutdownTimeout time.Duration) *Server {
	t.Helper()
	server, err := NewServer(Options{
		Version:            "test-version",
		ReadHeaderTimeout:  time.Second,
		ShutdownTimeout:    shutdownTimeout,
		ApplicationHandler: application,
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	return server
}

func newTestListener(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("listener.Close() error = %v", err)
		}
	})
	return listener
}

func serveRequest(handler http.Handler, method, target string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(method, target, nil))
	return recorder
}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(recorder.Body.Bytes(), target); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; body = %q", err, recorder.Body.String())
	}
}

func assertStatus(t *testing.T, recorder *httptest.ResponseRecorder, want int) {
	t.Helper()
	if recorder.Code != want {
		t.Fatalf("status = %d, want %d; body = %q", recorder.Code, want, recorder.Body.String())
	}
}

func assertHeader(t *testing.T, recorder *httptest.ResponseRecorder, name, want string) {
	t.Helper()
	if got := recorder.Header().Get(name); got != want {
		t.Fatalf("header %s = %q, want %q", name, got, want)
	}
}

func waitForStatus(t *testing.T, client *http.Client, target string, want int) {
	t.Helper()
	waitForCondition(t, func() bool {
		response, err := client.Get(target)
		if err != nil {
			return false
		}
		closeErr := response.Body.Close()
		return closeErr == nil && response.StatusCode == want
	}, "HTTP status to become ready")
}

func waitForCondition(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func waitForSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(testTimeout):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func waitForError(t *testing.T, errors <-chan error) error {
	t.Helper()
	select {
	case err := <-errors:
		return err
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for server result")
		return nil
	}
}

func waitForRequest(t *testing.T, results <-chan requestResult) requestResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for request result")
		return requestResult{}
	}
}
