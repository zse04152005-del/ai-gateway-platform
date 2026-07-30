package config

import (
	"strings"
	"testing"
)

func TestLoadValidDevelopmentConfig(t *testing.T) {
	values := validEnvironment()

	cfg, err := Load(mapLookup(values))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Environment.Name != EnvironmentDevelopment {
		t.Fatalf("environment = %q, want %q", cfg.Environment.Name, EnvironmentDevelopment)
	}
	if len(cfg.Security.LocalEnvelopeKey) != 32 {
		t.Fatalf("local envelope key length = %d, want 32", len(cfg.Security.LocalEnvelopeKey))
	}
	if len(cfg.Kafka.Brokers) != 2 {
		t.Fatalf("brokers = %v, want two brokers", cfg.Kafka.Brokers)
	}
}

func TestLoadRejectsProductionLocalEnvelopeKey(t *testing.T) {
	values := validEnvironment()
	values["APP_ENV"] = EnvironmentProduction

	_, err := Load(mapLookup(values))
	if err == nil || !strings.Contains(err.Error(), "development-only") {
		t.Fatalf("Load() error = %v, want production local-key rejection", err)
	}
}

func TestLoadRejectsMissingDatabaseURL(t *testing.T) {
	values := validEnvironment()
	delete(values, "DATABASE_URL")

	_, err := Load(mapLookup(values))
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("Load() error = %v, want DATABASE_URL validation error", err)
	}
}

func TestLoadRejectsInvalidAddressesAndDurations(t *testing.T) {
	values := validEnvironment()
	values["GATEWAY_HTTP_ADDR"] = "missing-port"
	values["REDIS_ADDR"] = "redis"
	values["KAFKA_BROKERS"] = "broker-one,broker-two:abc"

	_, err := Load(mapLookup(values))
	if err == nil {
		t.Fatal("Load() error = nil, want validation error")
	}
	for _, fragment := range []string{"GATEWAY_HTTP_ADDR", "REDIS_ADDR", "KAFKA_BROKERS"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("Load() error %q does not contain %q", err, fragment)
		}
	}
}

func TestLoadRejectsInvalidDurationDuringParsing(t *testing.T) {
	values := validEnvironment()
	values["SHUTDOWN_TIMEOUT"] = "never"

	_, err := Load(mapLookup(values))
	if err == nil || !strings.Contains(err.Error(), "SHUTDOWN_TIMEOUT") {
		t.Fatalf("Load() error = %v, want SHUTDOWN_TIMEOUT parsing error", err)
	}
}

func TestLoadRejectsInvalidEnvironmentAndLogLevel(t *testing.T) {
	values := validEnvironment()
	values["APP_ENV"] = "prod"
	values["LOG_LEVEL"] = "verbose"

	_, err := Load(mapLookup(values))
	if err == nil {
		t.Fatal("Load() error = nil, want validation error")
	}
	for _, fragment := range []string{"APP_ENV", "LOG_LEVEL"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("Load() error %q does not contain %q", err, fragment)
		}
	}
}

func TestLoadRejectsNilLookup(t *testing.T) {
	if _, err := Load(nil); err == nil {
		t.Fatal("Load(nil) error = nil, want error")
	}
}

func validEnvironment() map[string]string {
	return map[string]string{
		"APP_ENV":                     EnvironmentDevelopment,
		"LOG_LEVEL":                   "debug",
		"GATEWAY_HTTP_ADDR":           ":8080",
		"CONTROL_PLANE_HTTP_ADDR":     ":8081",
		"METRICS_ADDR":                ":9091",
		"DATABASE_URL":                "postgres://ai_gateway:synthetic-test-only@localhost:5432/ai_gateway?sslmode=disable", // #nosec G101 -- deterministic non-production test fixture.
		"REDIS_ADDR":                  "localhost:6379",
		"KAFKA_BROKERS":               "localhost:19092,localhost:29092",
		"CLICKHOUSE_HTTP_URL":         "http://localhost:8123",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4318",
		"LOCAL_ENVELOPE_KEY":          strings.Repeat("00", 32),
	}
}

func mapLookup(values map[string]string) LookupEnv {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}
