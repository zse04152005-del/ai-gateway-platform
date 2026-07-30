package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadMockProviderIsDependencyFreeAndLocalOnly(t *testing.T) {
	t.Parallel()
	cfg, err := LoadMockProvider(mapLookup(map[string]string{
		"APP_ENV":                  EnvironmentTest,
		"LOG_LEVEL":                "debug",
		"MOCK_PROVIDER_HTTP_ADDR":  "127.0.0.1:18082",
		"SHUTDOWN_TIMEOUT":         "3s",
		"HTTP_READ_HEADER_TIMEOUT": "2s",
	}))
	if err != nil {
		t.Fatalf("LoadMockProvider() error = %v", err)
	}
	if cfg.HTTP.Addr != "127.0.0.1:18082" || cfg.HTTP.ShutdownTimeout != 3*time.Second ||
		cfg.HTTP.ReadHeaderTimeout != 2*time.Second {
		t.Fatalf("mock-provider HTTP config = %+v", cfg.HTTP)
	}
}

func TestLoadMockProviderRejectsUnsafeEnvironmentAndExposure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		values map[string]string
		want   string
	}{
		{
			name: "production",
			values: map[string]string{
				"APP_ENV": EnvironmentProduction, "MOCK_PROVIDER_HTTP_ADDR": "127.0.0.1:18082",
			},
			want: "development or test",
		},
		{
			name: "non-loopback",
			values: map[string]string{
				"APP_ENV": EnvironmentTest, "MOCK_PROVIDER_HTTP_ADDR": "0.0.0.0:18082",
			},
			want: "loopback",
		},
		{
			name: "missing port",
			values: map[string]string{
				"APP_ENV": EnvironmentTest, "MOCK_PROVIDER_HTTP_ADDR": "localhost",
			},
			want: "host:port",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadMockProvider(mapLookup(test.values))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadMockProvider() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadMockProviderRejectsNilLookupAndInvalidDuration(t *testing.T) {
	t.Parallel()
	if _, err := LoadMockProvider(nil); err == nil {
		t.Fatal("LoadMockProvider(nil) error = nil")
	}
	_, err := LoadMockProvider(mapLookup(map[string]string{
		"APP_ENV": "test", "MOCK_PROVIDER_HTTP_ADDR": "[::1]:18082", "SHUTDOWN_TIMEOUT": "never",
	}))
	if err == nil || !strings.Contains(err.Error(), "SHUTDOWN_TIMEOUT") {
		t.Fatalf("LoadMockProvider(invalid duration) error = %v", err)
	}
}
