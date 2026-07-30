package config

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// MockProviderConfig is intentionally dependency-free. The local protocol
// simulator must be able to run without PostgreSQL, Redis, Kafka, or Internet access.
type MockProviderConfig struct {
	Environment EnvironmentConfig
	HTTP        MockProviderHTTPConfig
}

// MockProviderHTTPConfig contains the local-only listener and lifecycle limits.
type MockProviderHTTPConfig struct {
	Addr              string
	ShutdownTimeout   time.Duration
	ReadHeaderTimeout time.Duration
}

// LoadMockProvider loads only settings used by the offline protocol simulator.
func LoadMockProvider(lookup LookupEnv) (MockProviderConfig, error) {
	if lookup == nil {
		return MockProviderConfig{}, errors.New("environment lookup must not be nil")
	}
	shutdownTimeout, err := durationValue(lookup, "SHUTDOWN_TIMEOUT", 15*time.Second)
	if err != nil {
		return MockProviderConfig{}, err
	}
	readHeaderTimeout, err := durationValue(lookup, "HTTP_READ_HEADER_TIMEOUT", 5*time.Second)
	if err != nil {
		return MockProviderConfig{}, err
	}
	cfg := MockProviderConfig{
		Environment: EnvironmentConfig{
			Name:     valueOrDefault(lookup, "APP_ENV", EnvironmentDevelopment),
			LogLevel: valueOrDefault(lookup, "LOG_LEVEL", "info"),
		},
		HTTP: MockProviderHTTPConfig{
			Addr:              valueOrDefault(lookup, "MOCK_PROVIDER_HTTP_ADDR", "127.0.0.1:18082"),
			ShutdownTimeout:   shutdownTimeout,
			ReadHeaderTimeout: readHeaderTimeout,
		},
	}
	if err := cfg.Validate(); err != nil {
		return MockProviderConfig{}, err
	}
	return cfg, nil
}

// Validate rejects production use and non-loopback exposure.
func (cfg MockProviderConfig) Validate() error {
	var problems []string
	if !oneOf(cfg.Environment.Name, EnvironmentDevelopment, EnvironmentTest) {
		problems = append(problems, "APP_ENV must be development or test for mock-provider")
	}
	if !oneOf(cfg.Environment.LogLevel, "debug", "info", "warn", "error") {
		problems = append(problems, "LOG_LEVEL must be debug, info, warn, or error")
	}
	if err := validateLoopbackListenAddr(cfg.HTTP.Addr); err != nil {
		problems = append(problems, fmt.Sprintf("MOCK_PROVIDER_HTTP_ADDR: %v", err))
	}
	if cfg.HTTP.ShutdownTimeout <= 0 {
		problems = append(problems, "SHUTDOWN_TIMEOUT must be greater than zero")
	}
	if cfg.HTTP.ReadHeaderTimeout <= 0 {
		problems = append(problems, "HTTP_READ_HEADER_TIMEOUT must be greater than zero")
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid mock-provider configuration: %s", strings.Join(problems, "; "))
	}
	return nil
}

func validateLoopbackListenAddr(address string) error {
	if err := validateListenAddr(address); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("must bind to an explicit loopback address")
	}
	return nil
}
