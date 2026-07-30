// Package config loads and validates process configuration from environment variables.
// It intentionally does not load .env files; local launch tools may export them before startup.
package config

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// EnvironmentDevelopment is the local developer environment.
	EnvironmentDevelopment = "development"
	// EnvironmentTest is the automated test environment.
	EnvironmentTest = "test"
	// EnvironmentStaging is the pre-production environment.
	EnvironmentStaging = "staging"
	// EnvironmentProduction is the production environment.
	EnvironmentProduction = "production"
)

// LookupEnv matches os.LookupEnv and makes configuration tests deterministic.
type LookupEnv func(string) (string, bool)

// Config contains process-independent MVP configuration.
// Provider credentials and tenant policies are versioned business configuration,
// so they deliberately do not belong in this runtime structure.
type Config struct {
	Environment EnvironmentConfig
	HTTP        HTTPConfig
	Postgres    PostgresConfig
	Redis       RedisConfig
	Kafka       KafkaConfig
	ClickHouse  ClickHouseConfig
	Telemetry   TelemetryConfig
	Security    SecurityConfig
}

// EnvironmentConfig contains deployment environment and logging settings.
type EnvironmentConfig struct {
	Name     string
	LogLevel string
}

// HTTPConfig contains service listener and lifecycle timeout settings.
type HTTPConfig struct {
	GatewayAddr       string
	ControlPlaneAddr  string
	MetricsAddr       string
	ShutdownTimeout   time.Duration
	ReadHeaderTimeout time.Duration
}

// PostgresConfig contains the PostgreSQL connection setting.
type PostgresConfig struct {
	URL string
}

// RedisConfig contains Redis connectivity and logical database settings.
type RedisConfig struct {
	Addr     string
	Password string
	DB       int
}

// KafkaConfig contains Kafka-compatible broker endpoints.
type KafkaConfig struct {
	Brokers []string
}

// ClickHouseConfig contains the ClickHouse HTTP endpoint.
type ClickHouseConfig struct {
	HTTPURL string
}

// TelemetryConfig contains OpenTelemetry export settings.
type TelemetryConfig struct {
	OTLPEndpoint string
}

// SecurityConfig contains development-only local cryptographic settings.
type SecurityConfig struct {
	LocalEnvelopeKey       []byte
	VirtualKeyHashKey      []byte
	VirtualKeyHashVersion  string
	VirtualKeyAuthCacheTTL time.Duration
}

var virtualKeyHashVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

// Load creates a validated configuration. Defaults are intended only for local development;
// production still requires explicit dependency and encryption settings.
func Load(lookup LookupEnv) (Config, error) {
	if lookup == nil {
		return Config{}, errors.New("environment lookup must not be nil")
	}

	environment := valueOrDefault(lookup, "APP_ENV", EnvironmentDevelopment)
	redisDB, err := intValue(lookup, "REDIS_DB", 0)
	if err != nil {
		return Config{}, err
	}
	shutdownTimeout, err := durationValue(lookup, "SHUTDOWN_TIMEOUT", 15*time.Second)
	if err != nil {
		return Config{}, err
	}
	readHeaderTimeout, err := durationValue(lookup, "HTTP_READ_HEADER_TIMEOUT", 5*time.Second)
	if err != nil {
		return Config{}, err
	}
	envelopeKey, err := decodeEnvelopeKey(valueOrDefault(lookup, "LOCAL_ENVELOPE_KEY", ""))
	if err != nil {
		return Config{}, err
	}
	virtualKeyHashKey, err := decodeFixedHexKey(
		"VIRTUAL_KEY_HASH_KEY",
		valueOrDefault(lookup, "VIRTUAL_KEY_HASH_KEY", ""),
	)
	if err != nil {
		return Config{}, err
	}
	virtualKeyAuthCacheTTL, err := durationValue(lookup, "VIRTUAL_KEY_AUTH_CACHE_TTL", 2*time.Second)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Environment: EnvironmentConfig{
			Name:     environment,
			LogLevel: valueOrDefault(lookup, "LOG_LEVEL", "info"),
		},
		HTTP: HTTPConfig{
			GatewayAddr:       valueOrDefault(lookup, "GATEWAY_HTTP_ADDR", ":8080"),
			ControlPlaneAddr:  valueOrDefault(lookup, "CONTROL_PLANE_HTTP_ADDR", ":8081"),
			MetricsAddr:       valueOrDefault(lookup, "METRICS_ADDR", ":9091"),
			ShutdownTimeout:   shutdownTimeout,
			ReadHeaderTimeout: readHeaderTimeout,
		},
		Postgres: PostgresConfig{
			URL: valueOrDefault(lookup, "DATABASE_URL", ""),
		},
		Redis: RedisConfig{
			Addr:     valueOrDefault(lookup, "REDIS_ADDR", "localhost:6379"),
			Password: valueOrDefault(lookup, "REDIS_PASSWORD", ""),
			DB:       redisDB,
		},
		Kafka: KafkaConfig{
			Brokers: splitCSV(valueOrDefault(lookup, "KAFKA_BROKERS", "localhost:19092")),
		},
		ClickHouse: ClickHouseConfig{
			HTTPURL: valueOrDefault(lookup, "CLICKHOUSE_HTTP_URL", "http://localhost:8123"),
		},
		Telemetry: TelemetryConfig{
			OTLPEndpoint: valueOrDefault(lookup, "OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318"),
		},
		Security: SecurityConfig{
			LocalEnvelopeKey:       envelopeKey,
			VirtualKeyHashKey:      virtualKeyHashKey,
			VirtualKeyHashVersion:  valueOrDefault(lookup, "VIRTUAL_KEY_HASH_KEY_VERSION", "local-v1"),
			VirtualKeyAuthCacheTTL: virtualKeyAuthCacheTTL,
		},
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks the complete configuration and returns all detected problems.
func (c Config) Validate() error {
	var problems []string

	if !oneOf(c.Environment.Name, EnvironmentDevelopment, EnvironmentTest, EnvironmentStaging, EnvironmentProduction) {
		problems = append(problems, "APP_ENV must be development, test, staging, or production")
	}
	if !oneOf(c.Environment.LogLevel, "debug", "info", "warn", "error") {
		problems = append(problems, "LOG_LEVEL must be debug, info, warn, or error")
	}
	for name, addr := range map[string]string{
		"GATEWAY_HTTP_ADDR":       c.HTTP.GatewayAddr,
		"CONTROL_PLANE_HTTP_ADDR": c.HTTP.ControlPlaneAddr,
		"METRICS_ADDR":            c.HTTP.MetricsAddr,
	} {
		if err := validateListenAddr(addr); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
		}
	}
	if c.HTTP.ShutdownTimeout <= 0 {
		problems = append(problems, "SHUTDOWN_TIMEOUT must be greater than zero")
	}
	if c.HTTP.ReadHeaderTimeout <= 0 {
		problems = append(problems, "HTTP_READ_HEADER_TIMEOUT must be greater than zero")
	}
	if err := validateURL("DATABASE_URL", c.Postgres.URL, "postgres", "postgresql"); err != nil {
		problems = append(problems, err.Error())
	}
	if _, _, err := net.SplitHostPort(c.Redis.Addr); err != nil {
		problems = append(problems, fmt.Sprintf("REDIS_ADDR must be host:port: %v", err))
	}
	if c.Redis.DB < 0 {
		problems = append(problems, "REDIS_DB must not be negative")
	}
	if len(c.Kafka.Brokers) == 0 {
		problems = append(problems, "KAFKA_BROKERS must contain at least one broker")
	}
	for _, broker := range c.Kafka.Brokers {
		if _, _, err := net.SplitHostPort(broker); err != nil {
			problems = append(problems, fmt.Sprintf("KAFKA_BROKERS contains invalid host:port %q", broker))
		}
	}
	if err := validateURL("CLICKHOUSE_HTTP_URL", c.ClickHouse.HTTPURL, "http", "https"); err != nil {
		problems = append(problems, err.Error())
	}
	if err := validateURL("OTEL_EXPORTER_OTLP_ENDPOINT", c.Telemetry.OTLPEndpoint, "http", "https"); err != nil {
		problems = append(problems, err.Error())
	}
	if c.Environment.Name == EnvironmentProduction && len(c.Security.LocalEnvelopeKey) > 0 {
		problems = append(problems, "LOCAL_ENVELOPE_KEY is development-only and must not be used in production")
	}
	if c.Environment.Name != EnvironmentProduction && len(c.Security.LocalEnvelopeKey) != 0 && len(c.Security.LocalEnvelopeKey) != 32 {
		problems = append(problems, "LOCAL_ENVELOPE_KEY must decode to exactly 32 bytes")
	}
	if len(c.Security.VirtualKeyHashKey) != 0 && len(c.Security.VirtualKeyHashKey) != 32 {
		problems = append(problems, "VIRTUAL_KEY_HASH_KEY must decode to exactly 32 bytes")
	}
	if !virtualKeyHashVersionPattern.MatchString(c.Security.VirtualKeyHashVersion) {
		problems = append(problems, "VIRTUAL_KEY_HASH_KEY_VERSION has an invalid format")
	}
	if c.Security.VirtualKeyAuthCacheTTL < 0 || c.Security.VirtualKeyAuthCacheTTL > 30*time.Second {
		problems = append(problems, "VIRTUAL_KEY_AUTH_CACHE_TTL must be between 0s and 30s")
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid configuration: %s", strings.Join(problems, "; "))
	}
	return nil
}

func valueOrDefault(lookup LookupEnv, key, fallback string) string {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

func intValue(lookup LookupEnv, key string, fallback int) (int, error) {
	raw := valueOrDefault(lookup, key, strconv.Itoa(fallback))
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return value, nil
}

func durationValue(lookup LookupEnv, key string, fallback time.Duration) (time.Duration, error) {
	raw := valueOrDefault(lookup, key, fallback.String())
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration: %w", key, err)
	}
	return value, nil
}

func decodeEnvelopeKey(raw string) ([]byte, error) {
	if raw == "" {
		return nil, nil
	}
	decoded, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("LOCAL_ENVELOPE_KEY must be hexadecimal: %w", err)
	}
	return decoded, nil
}

func decodeFixedHexKey(name, raw string) ([]byte, error) {
	if raw == "" {
		return nil, nil
	}
	decoded, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%s must be hexadecimal: %w", name, err)
	}
	if len(decoded) != sha256.Size {
		return nil, fmt.Errorf("%s must decode to exactly 32 bytes", name)
	}
	return decoded, nil
}

// ResolveVirtualKeyHashKey returns a copied explicit key or a development-only,
// domain-separated key derived from LOCAL_ENVELOPE_KEY. Production never falls back.
func (c Config) ResolveVirtualKeyHashKey() ([]byte, error) {
	if len(c.Security.VirtualKeyHashKey) == sha256.Size {
		return append([]byte(nil), c.Security.VirtualKeyHashKey...), nil
	}
	if c.Environment.Name == EnvironmentDevelopment && len(c.Security.LocalEnvelopeKey) == sha256.Size {
		mac := hmac.New(sha256.New, c.Security.LocalEnvelopeKey)
		_, _ = mac.Write([]byte("ai-gateway/virtual-key-hmac/" + c.Security.VirtualKeyHashVersion))
		return mac.Sum(nil), nil
	}
	return nil, errors.New("VIRTUAL_KEY_HASH_KEY is required outside local development")
}

func splitCSV(raw string) []string {
	parts := strings.Split(raw, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := strings.TrimSpace(part); value != "" {
			values = append(values, value)
		}
	}
	return values
}

func validateListenAddr(addr string) error {
	if strings.TrimSpace(addr) == "" {
		return errors.New("must not be empty")
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("must be host:port: %w", err)
	}
	parsedPort, err := strconv.Atoi(port)
	if err != nil || parsedPort < 1 || parsedPort > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	return nil
}

func validateURL(name, raw string, allowedSchemes ...string) error {
	if raw == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s must be a valid URL: %w", name, err)
	}
	if parsed.Host == "" || !oneOf(parsed.Scheme, allowedSchemes...) {
		return fmt.Errorf("%s must use one of %v and include a host", name, allowedSchemes)
	}
	return nil
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
