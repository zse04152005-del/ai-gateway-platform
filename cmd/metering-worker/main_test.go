package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zse04152005-del/ai-gateway-platform/internal/config"
	"github.com/zse04152005-del/ai-gateway-platform/internal/meteringworker"
)

type connectorFunc func(context.Context, []string) (meteringworker.Session, error)

func (function connectorFunc) Connect(ctx context.Context, brokers []string) (meteringworker.Session, error) {
	return function(ctx, brokers)
}

type sessionFunc func(context.Context) error

func (function sessionFunc) Close(ctx context.Context) error {
	return function(ctx)
}

func TestRunRejectsInvalidConfigurationBeforeConnecting(t *testing.T) {
	connectorCalled := false
	connector := connectorFunc(func(context.Context, []string) (meteringworker.Session, error) {
		connectorCalled = true
		return nil, errors.New("must not be called")
	})

	err := run(context.Background(), mapLookup(nil), connector)
	if err == nil || !strings.Contains(err.Error(), "load metering-worker configuration") {
		t.Fatalf("run() error = %v, want configuration error", err)
	}
	if connectorCalled {
		t.Fatal("connector called for invalid configuration")
	}
}

func TestRunConnectsConfiguredBrokersAndStopsCleanly(t *testing.T) {
	connector := connectorFunc(func(_ context.Context, brokers []string) (meteringworker.Session, error) {
		if strings.Join(brokers, ",") != "broker-one:9092,broker-two:9092" {
			t.Fatalf("brokers = %v", brokers)
		}
		return sessionFunc(func(context.Context) error { return nil }), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := run(ctx, validLookup(), connector); err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}
}

func TestRunWithLogsRecordsBrokerCountWithoutBrokerAddresses(t *testing.T) {
	connector := connectorFunc(func(_ context.Context, _ []string) (meteringworker.Session, error) {
		return sessionFunc(func(context.Context) error { return nil }), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var output bytes.Buffer

	if err := runWithLogs(ctx, validLookup(), connector, &output); err != nil {
		t.Fatalf("runWithLogs() error = %v", err)
	}
	raw := output.String()
	for _, required := range []string{`"service":"metering-worker"`, `"brokerCount":2`, `"level":"INFO"`} {
		if !strings.Contains(raw, required) {
			t.Errorf("logs do not contain %q: %s", required, raw)
		}
	}
	for _, forbidden := range []string{"broker-one:9092", "broker-two:9092", "postgres://"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("logs contain forbidden value %q: %s", forbidden, raw)
		}
	}
}

func TestRunPropagatesConnectorFailure(t *testing.T) {
	wantErr := errors.New("synthetic event-bus failure")
	connector := connectorFunc(func(context.Context, []string) (meteringworker.Session, error) {
		return nil, wantErr
	})

	err := run(context.Background(), validLookup(), connector)
	if !errors.Is(err, wantErr) {
		t.Fatalf("run() error = %v, want connector failure", err)
	}
}

func TestRunRejectsNilLifecycleInputs(t *testing.T) {
	connector := connectorFunc(func(context.Context, []string) (meteringworker.Session, error) { return nil, nil })
	var nilContext context.Context
	if err := run(nilContext, validLookup(), connector); err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("run(nil context) error = %v", err)
	}
	if err := run(context.Background(), validLookup(), nil); err == nil || !strings.Contains(err.Error(), "connector") {
		t.Fatalf("run(nil connector) error = %v", err)
	}
}

func validLookup() config.LookupEnv {
	return mapLookup(map[string]string{
		"APP_ENV":       config.EnvironmentTest,
		"DATABASE_URL":  "postgres://localhost:5432/ai_gateway_test?sslmode=disable",
		"KAFKA_BROKERS": "broker-one:9092,broker-two:9092",
	})
}

func mapLookup(values map[string]string) config.LookupEnv {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}
