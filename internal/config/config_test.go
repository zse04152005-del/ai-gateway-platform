package config

import (
	"strings"
	"testing"
	"time"
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
	virtualKeyHashKey, err := cfg.ResolveVirtualKeyHashKey()
	if err != nil || len(virtualKeyHashKey) != 32 {
		t.Fatalf("ResolveVirtualKeyHashKey() length/error = %d/%v", len(virtualKeyHashKey), err)
	}
	if len(cfg.Kafka.Brokers) != 2 {
		t.Fatalf("brokers = %v, want two brokers", cfg.Kafka.Brokers)
	}
	if cfg.Security.VirtualKeyAuthCacheTTL != 2*time.Second {
		t.Fatalf("virtual key auth cache TTL = %v, want 2s", cfg.Security.VirtualKeyAuthCacheTTL)
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

func TestResolveVirtualKeyHashKeyRequiresExplicitKeyOutsideDevelopment(t *testing.T) {
	values := validEnvironment()
	values["APP_ENV"] = EnvironmentTest
	values["LOCAL_ENVELOPE_KEY"] = ""
	cfg, err := Load(mapLookup(values))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if _, err := cfg.ResolveVirtualKeyHashKey(); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("ResolveVirtualKeyHashKey() error = %v, want required-key error", err)
	}

	values["VIRTUAL_KEY_HASH_KEY"] = strings.Repeat("11", 32)
	cfg, err = Load(mapLookup(values))
	if err != nil {
		t.Fatalf("Load(explicit key) error = %v", err)
	}
	resolved, err := cfg.ResolveVirtualKeyHashKey()
	if err != nil || len(resolved) != 32 || resolved[0] != 0x11 {
		t.Fatalf("resolved explicit key length/first/error = %d/%x/%v", len(resolved), resolved[0], err)
	}
}

func TestLoadRejectsInvalidVirtualKeyHashConfiguration(t *testing.T) {
	values := validEnvironment()
	values["VIRTUAL_KEY_HASH_KEY"] = "abcd"
	values["VIRTUAL_KEY_HASH_KEY_VERSION"] = "bad version"

	_, err := Load(mapLookup(values))
	if err == nil || !strings.Contains(err.Error(), "VIRTUAL_KEY_HASH_KEY") {
		t.Fatalf("Load() error = %v, want virtual-key hash validation", err)
	}
}

func TestLoadRejectsUnsafeVirtualKeyAuthCacheTTL(t *testing.T) {
	values := validEnvironment()
	values["VIRTUAL_KEY_AUTH_CACHE_TTL"] = "31s"

	_, err := Load(mapLookup(values))
	if err == nil || !strings.Contains(err.Error(), "VIRTUAL_KEY_AUTH_CACHE_TTL") {
		t.Fatalf("Load() error = %v, want cache TTL validation", err)
	}
}

func validEnvironment() map[string]string {
	return map[string]string{
		"APP_ENV":                      EnvironmentDevelopment,
		"LOG_LEVEL":                    "debug",
		"GATEWAY_HTTP_ADDR":            ":8080",
		"CONTROL_PLANE_HTTP_ADDR":      ":8081",
		"METRICS_ADDR":                 ":9091",
		"DATABASE_URL":                 "postgres://ai_gateway:synthetic-test-only@localhost:5432/ai_gateway?sslmode=disable", // #nosec G101 -- deterministic non-production test fixture.
		"REDIS_ADDR":                   "localhost:6379",
		"KAFKA_BROKERS":                "localhost:19092,localhost:29092",
		"CLICKHOUSE_HTTP_URL":          "http://localhost:8123",
		"OTEL_EXPORTER_OTLP_ENDPOINT":  "http://localhost:4318",
		"LOCAL_ENVELOPE_KEY":           strings.Repeat("00", 32),
		"VIRTUAL_KEY_HASH_KEY_VERSION": "local-v1",
	}
}

func mapLookup(values map[string]string) LookupEnv {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}
