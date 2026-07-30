package upstreamhttp

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const testBody = "ok"

func TestNewClientAppliesHardenedPoolConfiguration(t *testing.T) {
	options := testOptions()
	client, err := NewClient(options)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)

	if client.httpClient.Timeout != options.TotalTimeout {
		t.Errorf("client timeout = %v, want %v", client.httpClient.Timeout, options.TotalTimeout)
	}
	transport := client.transport
	if transport.Proxy != nil {
		t.Error("transport inherits an ambient proxy")
	}
	if !transport.ForceAttemptHTTP2 || !transport.DisableCompression {
		t.Errorf("HTTP/2/compression flags = %v/%v", transport.ForceAttemptHTTP2, transport.DisableCompression)
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("TLS minimum version = %#v, want TLS 1.2", transport.TLSClientConfig)
	}
	if transport.ResponseHeaderTimeout != options.ResponseHeaderTimeout ||
		transport.TLSHandshakeTimeout != options.TLSHandshakeTimeout ||
		transport.IdleConnTimeout != options.IdleConnTimeout ||
		transport.MaxIdleConns != options.MaxIdleConns ||
		transport.MaxIdleConnsPerHost != options.MaxIdleConnsPerHost ||
		transport.MaxConnsPerHost != options.MaxConnsPerHost ||
		transport.MaxResponseHeaderBytes != options.MaxResponseHeaderBytes {
		t.Fatalf("transport configuration does not match options: %#v", transport)
	}
}

func TestClientStripsInboundSensitiveAndHopByHopHeadersWithoutMutatingInput(t *testing.T) {
	received := make(chan struct {
		header http.Header
		host   string
	}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received <- struct {
			header http.Header
			host   string
		}{header: request.Header.Clone(), host: request.Host}
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, testBody)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t)
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	request.Host = "client-controlled.invalid"
	request.Header.Set("Authorization", "Bearer provider-fixture")
	request.Header.Set("X-Provider-Feature", "enabled")
	request.Header.Set("Cookie", "gateway_session=must-not-leak")
	request.Header.Set("Proxy-Authorization", "must-not-leak")
	request.Header.Set("X-Forwarded-For", "203.0.113.4")
	request.Header.Set("Connection", "X-Connection-Secret")
	request.Header.Set("X-Connection-Secret", "must-not-leak")

	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil {
			t.Errorf("response body close error = %v", closeErr)
		}
	}()
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	captured := <-received
	header := captured.header
	if header.Get("Authorization") != "Bearer provider-fixture" || header.Get("X-Provider-Feature") != "enabled" {
		t.Errorf("adapter-owned headers were lost: %#v", header)
	}
	for _, name := range []string{"Cookie", "Proxy-Authorization", "X-Forwarded-For", "Connection", "X-Connection-Secret"} {
		if header.Get(name) != "" {
			t.Errorf("unsafe header %q reached provider", name)
		}
	}
	if request.Header.Get("Cookie") == "" || request.Header.Get("X-Connection-Secret") == "" {
		t.Error("Do() mutated the caller request headers")
	}
	if captured.host == request.Host {
		t.Fatalf("client-controlled Host reached provider: %q", captured.host)
	}
}

func TestClientReusesConnectionsAndRefusesRedirects(t *testing.T) {
	var connections atomic.Int64
	targetHits := make(chan struct{}, 1)
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetHits <- struct{}{}
	}))
	t.Cleanup(target.Close)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/redirect" {
			http.Redirect(writer, request, target.URL, http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.WriteString(writer, testBody)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)
	client := newTestClient(t)

	for range 2 {
		response := doGet(t, client, server.URL)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", response.StatusCode)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	if connections.Load() != 1 {
		t.Fatalf("new connections = %d, want one reused connection", connections.Load())
	}
	redirect := doGet(t, client, server.URL+"/redirect")
	defer func() {
		if closeErr := redirect.Body.Close(); closeErr != nil {
			t.Errorf("redirect body close error = %v", closeErr)
		}
	}()
	if redirect.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("redirect status = %d, want 307", redirect.StatusCode)
	}
	select {
	case <-targetHits:
		t.Fatal("client followed a cross-origin redirect")
	default:
	}
}

func TestClientEnforcesResponseHeaderAndTotalTimeouts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case <-time.After(250 * time.Millisecond):
			writer.WriteHeader(http.StatusOK)
		case <-request.Context().Done():
		}
	}))
	t.Cleanup(server.Close)

	for _, test := range []struct {
		name            string
		totalTimeout    time.Duration
		responseTimeout time.Duration
	}{
		{name: "response header", totalTimeout: time.Second, responseTimeout: 20 * time.Millisecond},
		{name: "total", totalTimeout: 20 * time.Millisecond, responseTimeout: time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := testOptions()
			options.TotalTimeout = test.totalTimeout
			options.ResponseHeaderTimeout = test.responseTimeout
			client, err := NewClient(options)
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}
			t.Cleanup(client.CloseIdleConnections)
			request, requestErr := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
			if requestErr != nil {
				t.Fatalf("NewRequestWithContext() error = %v", requestErr)
			}
			response, requestErr := client.Do(request)
			closeUnexpectedResponse(t, response)
			if !errors.Is(requestErr, ErrTimeout) || !errors.Is(requestErr, ErrTransport) {
				t.Fatalf("Do() error = %v, want ErrTimeout and ErrTransport", requestErr)
			}
		})
	}
}

func TestClientPreservesCancellationAndRejectsUnsafeRequests(t *testing.T) {
	client := newTestClient(t)
	response, err := client.Do(nil)
	closeUnexpectedResponse(t, response)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Do(nil) error = %v", err)
	}
	unsafe, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "ftp://provider.invalid/file", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	response, err = client.Do(unsafe)
	closeUnexpectedResponse(t, response)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Do(unsafe) error = %v", err)
	}
	withUserInfo, err := http.NewRequestWithContext(
		context.Background(), http.MethodGet, "https://fixture-user:fixture-value@provider.invalid", nil,
	)
	if err != nil {
		t.Fatalf("NewRequestWithContext(userinfo) error = %v", err)
	}
	response, err = client.Do(withUserInfo)
	closeUnexpectedResponse(t, response)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Do(userinfo) error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	request, err := http.NewRequestWithContext(cancelled, http.MethodGet, "https://provider.invalid", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	response, err = client.Do(request)
	closeUnexpectedResponse(t, response)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrTransport) {
		t.Fatalf("Do(cancelled) error = %v", err)
	}
}

func TestNewClientRejectsInvalidOptions(t *testing.T) {
	valid := testOptions()
	tests := []struct {
		name   string
		mutate func(*Options)
	}{
		{name: "connect", mutate: func(value *Options) { value.ConnectTimeout = 0 }},
		{name: "keepalive", mutate: func(value *Options) { value.KeepAlive = 0 }},
		{name: "TLS", mutate: func(value *Options) { value.TLSHandshakeTimeout = 0 }},
		{name: "response", mutate: func(value *Options) { value.ResponseHeaderTimeout = 0 }},
		{name: "total", mutate: func(value *Options) { value.TotalTimeout = 0 }},
		{name: "idle timeout", mutate: func(value *Options) { value.IdleConnTimeout = 0 }},
		{name: "expect continue", mutate: func(value *Options) { value.ExpectContinueTimeout = -1 }},
		{name: "idle global", mutate: func(value *Options) { value.MaxIdleConns = 0 }},
		{name: "idle host", mutate: func(value *Options) { value.MaxIdleConnsPerHost = 0 }},
		{name: "host total", mutate: func(value *Options) { value.MaxConnsPerHost = 1 }},
		{name: "header bytes", mutate: func(value *Options) { value.MaxResponseHeaderBytes = 1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := valid
			test.mutate(&options)
			if _, err := NewClient(options); err == nil {
				t.Fatal("NewClient() error = nil, want validation error")
			}
		})
	}
}

func newTestClient(t *testing.T) *Client {
	t.Helper()
	client, err := NewClient(testOptions())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func doGet(t *testing.T, client *Client, target string) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	return response
}

func closeUnexpectedResponse(t *testing.T, response *http.Response) {
	t.Helper()
	if response != nil && response.Body != nil {
		if err := response.Body.Close(); err != nil {
			t.Errorf("unexpected response body close error = %v", err)
		}
	}
}

func testOptions() Options {
	return Options{
		ConnectTimeout: 100 * time.Millisecond, KeepAlive: time.Second,
		TLSHandshakeTimeout: time.Second, ResponseHeaderTimeout: time.Second,
		TotalTimeout: 2 * time.Second, IdleConnTimeout: time.Second,
		ExpectContinueTimeout: 100 * time.Millisecond, MaxIdleConns: 20,
		MaxIdleConnsPerHost: 10, MaxConnsPerHost: 20, MaxResponseHeaderBytes: 4096,
	}
}
