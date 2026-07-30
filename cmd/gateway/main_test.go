package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/zse04152005-del/ai-gateway-platform/internal/config"
	"github.com/zse04152005-del/ai-gateway-platform/internal/providersecret"
	"github.com/zse04152005-del/ai-gateway-platform/internal/upstreamhttp"
)

func TestRunRejectsInvalidConfigurationBeforeListening(t *testing.T) {
	listenCalled := false
	listen := func(_, _ string) (net.Listener, error) {
		listenCalled = true
		return nil, errors.New("must not be called")
	}

	err := run(context.Background(), mapLookup(nil), listen)
	if err == nil || !strings.Contains(err.Error(), "load gateway configuration") {
		t.Fatalf("run() error = %v, want configuration error", err)
	}
	if listenCalled {
		t.Fatal("listen called for invalid configuration")
	}
}

func TestRunPropagatesListenerFailure(t *testing.T) {
	wantErr := errors.New("synthetic bind failure")
	listen := func(network, address string) (net.Listener, error) {
		if network != "tcp" || address != "127.0.0.1:18080" {
			t.Fatalf("listen arguments = %q, %q", network, address)
		}
		return nil, wantErr
	}

	err := run(context.Background(), validLookup(), listen)
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "listen for gateway HTTP") {
		t.Fatalf("run() error = %v, want wrapped listener error", err)
	}
}

func TestRunStopsCleanlyWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	listen := func(network, _ string) (net.Listener, error) {
		return net.Listen(network, "127.0.0.1:0")
	}

	if err := run(ctx, validLookup(), listen); err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}
}

func TestRunWithLogsEmitsSafeStructuredLifecycleRecords(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var output bytes.Buffer
	listen := func(network, _ string) (net.Listener, error) {
		return net.Listen(network, "127.0.0.1:0")
	}

	if err := runWithLogs(ctx, validLookup(), listen, &output); err != nil {
		t.Fatalf("runWithLogs() error = %v", err)
	}
	raw := output.String()
	for _, required := range []string{`"service":"gateway"`, `"requestId":""`, `"traceId":""`, `"level":"INFO"`} {
		if !strings.Contains(raw, required) {
			t.Errorf("logs do not contain %q: %s", required, raw)
		}
	}
	if strings.Contains(raw, "postgres://") {
		t.Fatalf("logs contain database URL: %s", raw)
	}
}

func TestRunRejectsNilLifecycleInputs(t *testing.T) {
	var nilContext context.Context
	if err := run(nilContext, validLookup(), net.Listen); err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("run(nil context) error = %v", err)
	}
	if err := run(context.Background(), validLookup(), nil); err == nil || !strings.Contains(err.Error(), "listen function") {
		t.Fatalf("run(nil listen) error = %v", err)
	}
}

func TestNewChatExecutorBuildsRegisteredRuntimeWithOptionalLocalSecrets(t *testing.T) {
	cfg, err := config.Load(validLookup())
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	client, err := upstreamhttp.NewClient(upstreamhttp.Options{
		ConnectTimeout:         cfg.UpstreamHTTP.ConnectTimeout,
		KeepAlive:              cfg.UpstreamHTTP.KeepAlive,
		TLSHandshakeTimeout:    cfg.UpstreamHTTP.TLSHandshakeTimeout,
		ResponseHeaderTimeout:  cfg.UpstreamHTTP.ResponseHeaderTimeout,
		TotalTimeout:           cfg.UpstreamHTTP.TotalTimeout,
		IdleConnTimeout:        cfg.UpstreamHTTP.IdleConnTimeout,
		ExpectContinueTimeout:  cfg.UpstreamHTTP.ExpectContinueTimeout,
		MaxIdleConns:           cfg.UpstreamHTTP.MaxIdleConns,
		MaxIdleConnsPerHost:    cfg.UpstreamHTTP.MaxIdleConnsPerHost,
		MaxConnsPerHost:        cfg.UpstreamHTTP.MaxConnsPerHost,
		MaxResponseHeaderBytes: cfg.UpstreamHTTP.MaxResponseHeaderBytes,
	})
	if err != nil {
		t.Fatalf("upstreamhttp.NewClient() error = %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)
	database, err := sql.Open("postgres", cfg.Postgres.URL)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := database.Close(); closeErr != nil {
			t.Errorf("database.Close() error = %v", closeErr)
		}
	})
	if executor, buildErr := newChatExecutor(database, cfg, client); buildErr != nil || executor == nil {
		t.Fatalf("newChatExecutor(no local key) = %#v, %v", executor, buildErr)
	}
	cfg.Security.LocalEnvelopeKey = bytes.Repeat([]byte{0x42}, 32)
	if executor, buildErr := newChatExecutor(database, cfg, client); buildErr != nil || executor == nil {
		t.Fatalf("newChatExecutor(local key) = %#v, %v", executor, buildErr)
	}
	if _, buildErr := newChatExecutor(nil, cfg, client); buildErr == nil {
		t.Fatal("newChatExecutor(nil database) error = nil")
	}
	if _, buildErr := newChatExecutor(database, cfg, nil); buildErr == nil {
		t.Fatal("newChatExecutor(nil client) error = nil")
	}
}

func TestOptionalProviderSecretResolverFailsClosedWithoutManager(t *testing.T) {
	_, err := (optionalProviderSecretResolver{}).Resolve(context.Background(), providersecret.Locator{
		ProviderID: "11111111-1111-4111-8111-111111111111",
		ID:         "22222222-2222-4222-8222-222222222222",
	})
	if !errors.Is(err, providersecret.ErrBackendUnavailable) {
		t.Fatalf("Resolve() error = %v", err)
	}
}

func validLookup() config.LookupEnv {
	return mapLookup(map[string]string{
		"APP_ENV":              config.EnvironmentTest,
		"GATEWAY_HTTP_ADDR":    "127.0.0.1:18080",
		"DATABASE_URL":         "postgres://localhost:5432/ai_gateway_test?sslmode=disable",
		"VIRTUAL_KEY_HASH_KEY": strings.Repeat("11", 32),
	})
}

func mapLookup(values map[string]string) config.LookupEnv {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}
