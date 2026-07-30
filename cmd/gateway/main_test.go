package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/zse04152005-del/ai-gateway-platform/internal/config"
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

func TestRunRejectsNilLifecycleInputs(t *testing.T) {
	var nilContext context.Context
	if err := run(nilContext, validLookup(), net.Listen); err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("run(nil context) error = %v", err)
	}
	if err := run(context.Background(), validLookup(), nil); err == nil || !strings.Contains(err.Error(), "listen function") {
		t.Fatalf("run(nil listen) error = %v", err)
	}
}

func validLookup() config.LookupEnv {
	return mapLookup(map[string]string{
		"APP_ENV":           config.EnvironmentTest,
		"GATEWAY_HTTP_ADDR": "127.0.0.1:18080",
		"DATABASE_URL":      "postgres://localhost:5432/ai_gateway_test?sslmode=disable",
	})
}

func mapLookup(values map[string]string) config.LookupEnv {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}
