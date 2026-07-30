// Package observability provides shared telemetry primitives with safe defaults.
package observability

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
)

const (
	// RedactedValue replaces values whose attribute names identify sensitive data.
	RedactedValue = "[REDACTED]"
	redactedAny   = "[REDACTED_COMPLEX_VALUE]"
)

// Fields are the bounded correlation dimensions present on every process log record.
type Fields struct {
	RequestID string
	TraceID   string
	TenantID  string
	ProjectID string
}

// Logger emits schema-stable structured records through slog.
type Logger struct {
	logger  *slog.Logger
	service string
	version string
}

// NewJSON creates a JSON logger with recursive sensitive-attribute redaction.
func NewJSON(writer io.Writer, service, version, minimumLevel string) (*Logger, error) {
	if isNilWriter(writer) {
		return nil, errors.New("log writer must not be nil")
	}
	service = strings.TrimSpace(service)
	if service == "" {
		return nil, errors.New("log service must not be empty")
	}
	version = strings.TrimSpace(version)
	if version == "" {
		return nil, errors.New("log version must not be empty")
	}
	level, err := parseLevel(minimumLevel)
	if err != nil {
		return nil, err
	}

	handler := redactingHandler{
		next: slog.NewJSONHandler(writer, &slog.HandlerOptions{Level: level}),
	}
	return &Logger{
		logger:  slog.New(handler),
		service: service,
		version: version,
	}, nil
}

func isNilWriter(writer io.Writer) bool {
	if writer == nil {
		return true
	}
	value := reflect.ValueOf(writer)
	kind := value.Kind()
	if kind == reflect.Chan || kind == reflect.Func || kind == reflect.Interface || kind == reflect.Map ||
		kind == reflect.Pointer || kind == reflect.Slice {
		return value.IsNil()
	}
	return false
}

// MustNewJSON is for process bootstrap constants and panics on programmer error.
func MustNewJSON(writer io.Writer, service, version, minimumLevel string) *Logger {
	logger, err := NewJSON(writer, service, version, minimumLevel)
	if err != nil {
		panic(err)
	}
	return logger
}

// Debug emits a debug record when enabled.
func (l *Logger) Debug(ctx context.Context, message string, fields Fields, attributes ...slog.Attr) {
	l.log(ctx, slog.LevelDebug, message, fields, attributes...)
}

// Info emits an informational record.
func (l *Logger) Info(ctx context.Context, message string, fields Fields, attributes ...slog.Attr) {
	l.log(ctx, slog.LevelInfo, message, fields, attributes...)
}

// Warn emits a warning record.
func (l *Logger) Warn(ctx context.Context, message string, fields Fields, attributes ...slog.Attr) {
	l.log(ctx, slog.LevelWarn, message, fields, attributes...)
}

// Error emits an error record.
func (l *Logger) Error(ctx context.Context, message string, fields Fields, attributes ...slog.Attr) {
	l.log(ctx, slog.LevelError, message, fields, attributes...)
}

func (l *Logger) log(ctx context.Context, level slog.Level, message string, fields Fields, attributes ...slog.Attr) {
	if ctx == nil {
		ctx = context.Background()
	}
	base := make([]slog.Attr, 0, 6+len(attributes))
	base = append(base,
		slog.String("service", l.service),
		slog.String("version", l.version),
		slog.String("requestId", fields.RequestID),
		slog.String("traceId", fields.TraceID),
		slog.String("tenantId", fields.TenantID),
		slog.String("projectId", fields.ProjectID),
	)
	base = append(base, attributes...)
	l.logger.LogAttrs(ctx, level, message, base...)
}

type redactingHandler struct {
	next slog.Handler
}

func (h redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h redactingHandler) Handle(ctx context.Context, record slog.Record) error {
	clean := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	record.Attrs(func(attribute slog.Attr) bool {
		clean.AddAttrs(redactAttribute(attribute))
		return true
	})
	return h.next.Handle(ctx, clean)
}

func (h redactingHandler) WithAttrs(attributes []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, 0, len(attributes))
	for _, attribute := range attributes {
		clean = append(clean, redactAttribute(attribute))
	}
	return redactingHandler{next: h.next.WithAttrs(clean)}
}

func (h redactingHandler) WithGroup(name string) slog.Handler {
	return redactingHandler{next: h.next.WithGroup(name)}
}

func redactAttribute(attribute slog.Attr) slog.Attr {
	if isSensitiveLogKey(attribute.Key) {
		return slog.String(attribute.Key, RedactedValue)
	}

	value := attribute.Value.Resolve()
	switch value.Kind() {
	case slog.KindGroup:
		group := value.Group()
		clean := make([]slog.Attr, 0, len(group))
		for _, child := range group {
			clean = append(clean, redactAttribute(child))
		}
		return slog.Attr{Key: attribute.Key, Value: slog.GroupValue(clean...)}
	case slog.KindAny:
		if err, ok := value.Any().(error); ok {
			return slog.String(attribute.Key, err.Error())
		}
		return slog.String(attribute.Key, redactedAny)
	case slog.KindBool, slog.KindDuration, slog.KindFloat64, slog.KindInt64, slog.KindString, slog.KindTime, slog.KindUint64:
		return slog.Attr{Key: attribute.Key, Value: value}
	case slog.KindLogValuer:
		return slog.String(attribute.Key, redactedAny)
	}
	return slog.String(attribute.Key, redactedAny)
}

func isSensitiveLogKey(key string) bool {
	normalized := strings.NewReplacer("_", "", "-", "", ".", "").Replace(strings.ToLower(strings.TrimSpace(key)))
	for _, suffix := range []string{
		"accesstoken",
		"apikey",
		"authorization",
		"authheader",
		"authtoken",
		"bearertoken",
		"cookie",
		"cookieheader",
		"idtoken",
		"password",
		"prompt",
		"providerkey",
		"refreshtoken",
		"response",
		"secret",
		"sessiontoken",
		"setcookie",
	} {
		if strings.HasSuffix(normalized, suffix) {
			return true
		}
	}
	return normalized == "token"
}

func parseLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, errors.New("log minimum level must be debug, info, warn, or error")
	}
}
