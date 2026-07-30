package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestJSONLoggerEmitsStableCorrelationFieldsAndLevel(t *testing.T) {
	var output bytes.Buffer
	logger, err := NewJSON(&output, "gateway", "test-version", "info")
	if err != nil {
		t.Fatalf("NewJSON() error = %v", err)
	}
	logger.Debug(context.Background(), "hidden", Fields{})
	logger.Info(context.Background(), "request accepted", Fields{
		RequestID: "req_test",
		TraceID:   "trace_test",
		TenantID:  "tenant_test",
		ProjectID: "project_test",
	}, slog.String("route", "primary"))

	lines := nonemptyLines(output.String())
	if len(lines) != 1 {
		t.Fatalf("log line count = %d, want 1; output = %q", len(lines), output.String())
	}
	record := decodeLogRecord(t, lines[0])
	want := map[string]any{
		"level":     "INFO",
		"msg":       "request accepted",
		"service":   "gateway",
		"version":   "test-version",
		"requestId": "req_test",
		"traceId":   "trace_test",
		"tenantId":  "tenant_test",
		"projectId": "project_test",
		"route":     "primary",
	}
	for key, value := range want {
		if record[key] != value {
			t.Errorf("record[%q] = %v, want %v", key, record[key], value)
		}
	}
}

func TestJSONLoggerRedactsSensitiveTopLevelAndNestedFields(t *testing.T) {
	var output bytes.Buffer
	logger, err := NewJSON(&output, "control-plane", "test-version", "debug")
	if err != nil {
		t.Fatalf("NewJSON() error = %v", err)
	}
	logger.Warn(context.Background(), "redaction test", Fields{},
		slog.String("Authorization", "Bearer highly-sensitive"),
		slog.String("provider_key", "provider-secret-value"),
		slog.String("access-token", "access-token-value"),
		slog.Int("input_tokens", 42),
		slog.Group("request",
			slog.String("system_prompt", "private prompt"),
			slog.Group("headers", slog.String("cookie", "session=private")),
		),
		slog.Any("payload", map[string]any{"response": "must not leak"}),
	)

	raw := output.String()
	for _, secret := range []string{
		"highly-sensitive", "provider-secret-value", "access-token-value", "private prompt", "session=private", "must not leak",
	} {
		if strings.Contains(raw, secret) {
			t.Errorf("log output contains sensitive value %q: %s", secret, raw)
		}
	}
	record := decodeLogRecord(t, nonemptyLines(raw)[0])
	if record["Authorization"] != RedactedValue || record["provider_key"] != RedactedValue || record["access-token"] != RedactedValue {
		t.Fatalf("top-level redaction failed: %v", record)
	}
	if record["input_tokens"] != float64(42) {
		t.Fatalf("safe token count = %v, want 42", record["input_tokens"])
	}
	request := record["request"].(map[string]any)
	if request["system_prompt"] != RedactedValue {
		t.Fatalf("nested prompt = %v", request["system_prompt"])
	}
	if record["payload"] != redactedAny {
		t.Fatalf("complex payload = %v", record["payload"])
	}
}

func TestJSONLoggerPreservesErrorsWithoutExpandingComplexValues(t *testing.T) {
	var output bytes.Buffer
	logger := MustNewJSON(&output, "metering-worker", "test", "error")
	logger.Error(context.Background(), "worker failed", Fields{}, slog.Any("error", errors.New("broker unavailable")))
	record := decodeLogRecord(t, nonemptyLines(output.String())[0])
	if record["error"] != "broker unavailable" {
		t.Fatalf("error = %v", record["error"])
	}
}

func TestNewJSONValidatesOptions(t *testing.T) {
	tests := []struct {
		name    string
		writer  *bytes.Buffer
		service string
		version string
		level   string
	}{
		{name: "nil writer", service: "gateway", version: "v1", level: "info"},
		{name: "service", writer: &bytes.Buffer{}, version: "v1", level: "info"},
		{name: "version", writer: &bytes.Buffer{}, service: "gateway", level: "info"},
		{name: "level", writer: &bytes.Buffer{}, service: "gateway", version: "v1", level: "verbose"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewJSON(test.writer, test.service, test.version, test.level); err == nil {
				t.Fatal("NewJSON() error = nil")
			}
		})
	}
}

func decodeLogRecord(t *testing.T, line string) map[string]any {
	t.Helper()
	var record map[string]any
	if err := json.Unmarshal([]byte(line), &record); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; line = %q", err, line)
	}
	return record
}

func nonemptyLines(value string) []string {
	parts := strings.Split(value, "\n")
	lines := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			lines = append(lines, part)
		}
	}
	return lines
}
