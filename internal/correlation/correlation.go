// Package correlation provides bounded request identity and W3C Trace Context propagation.
package correlation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/apierror"
)

const (
	requestIDHeader     = "X-Request-Id"
	traceparentHeader   = "traceparent"
	tracestateHeader    = "tracestate"
	defaultRecentTTL    = 10 * time.Minute
	defaultMaxRecent    = 10000
	maxGenerationTries  = 8
	maxTracestateLength = 512
	maxTracestateItems  = 32
)

var (
	requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`)
	traceIDPattern   = regexp.MustCompile(`^[0-9a-f]{32}$`)
	spanIDPattern    = regexp.MustCompile(`^[0-9a-f]{16}$`)
	flagsPattern     = regexp.MustCompile(`^[0-9a-f]{2}$`)
	stateKeyPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9_*/-]{0,240}(?:@[a-z0-9][a-z0-9_*/-]{0,13})?$`)
)

type contextKey struct{}

// Fields are the correlation values attached to one inbound request.
type Fields struct {
	RequestID    string
	TraceID      string
	SpanID       string
	ParentSpanID string
	TraceFlags   string
	TraceState   string
}

// Generator provides collision-testable cryptographically random identifiers.
type Generator interface {
	RequestID() (string, error)
	TraceID() (string, error)
	SpanID() (string, error)
}

// Options configures one service-local correlation manager.
type Options struct {
	Generator  Generator
	Now        func() time.Time
	RecentTTL  time.Duration
	MaxRecent  int
	ErrorType  string
	SampleFlag string
}

// Manager owns request-ID conflict state and the HTTP middleware.
type Manager struct {
	generator  Generator
	now        func() time.Time
	recentTTL  time.Duration
	maxRecent  int
	errorType  string
	sampleFlag string

	mu     sync.Mutex
	active map[string]struct{}
	recent map[string]time.Time
}

// New creates an isolated manager. Recent IDs remain reserved for a bounded TTL,
// so sequential replay as well as concurrent collisions receive a server ID.
func New(options Options) (*Manager, error) {
	if options.Generator == nil {
		options.Generator = randomGenerator{reader: rand.Reader}
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.RecentTTL == 0 {
		options.RecentTTL = defaultRecentTTL
	}
	if options.MaxRecent == 0 {
		options.MaxRecent = defaultMaxRecent
	}
	if options.RecentTTL < 0 {
		return nil, errors.New("correlation recent TTL must not be negative")
	}
	if options.MaxRecent < 1 {
		return nil, errors.New("correlation maximum recent IDs must be greater than zero")
	}
	options.ErrorType = strings.TrimSpace(options.ErrorType)
	if options.ErrorType == "" {
		options.ErrorType = "internal_error"
	}
	if options.SampleFlag == "" {
		options.SampleFlag = "00"
	}
	if !flagsPattern.MatchString(options.SampleFlag) {
		return nil, errors.New("correlation sample flag must be two lowercase hexadecimal characters")
	}
	if _, err := apierror.New(apierror.Definition{
		Status:  http.StatusInternalServerError,
		Code:    "CORRELATION_CONTEXT_FAILED",
		Message: "Unable to initialize request correlation",
		Type:    options.ErrorType,
	}, nil); err != nil {
		return nil, fmt.Errorf("validate correlation error type: %w", err)
	}

	return &Manager{
		generator:  options.Generator,
		now:        options.Now,
		recentTTL:  options.RecentTTL,
		maxRecent:  options.MaxRecent,
		errorType:  options.ErrorType,
		sampleFlag: options.SampleFlag,
		active:     make(map[string]struct{}),
		recent:     make(map[string]time.Time),
	}, nil
}

// Middleware validates inbound correlation, reserves a unique request ID, and
// returns canonical identifiers in response headers.
func (m *Manager) Middleware(next http.Handler) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fields, err := m.newFields(request)
		if err != nil {
			publicError := apierror.MustNew(apierror.Definition{
				Status:  http.StatusInternalServerError,
				Code:    "CORRELATION_CONTEXT_FAILED",
				Message: "Unable to initialize request correlation",
				Type:    m.errorType,
			}, err)
			apierror.WriteHTTP(writer, publicError, "", m.errorType)
			return
		}
		defer m.release(fields.RequestID)

		writer.Header().Set(requestIDHeader, fields.RequestID)
		writer.Header().Set(traceparentHeader, formatTraceparent(fields.TraceID, fields.SpanID, fields.TraceFlags))
		if fields.TraceState != "" {
			writer.Header().Set(tracestateHeader, fields.TraceState)
		}
		ctx := context.WithValue(request.Context(), contextKey{}, fields)
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

// FromContext returns the immutable correlation fields for a request.
func FromContext(ctx context.Context) (Fields, bool) {
	if ctx == nil {
		return Fields{}, false
	}
	fields, ok := ctx.Value(contextKey{}).(Fields)
	return fields, ok
}

// RequestID returns the current request ID or an empty string outside middleware.
func RequestID(ctx context.Context) string {
	fields, _ := FromContext(ctx)
	return fields.RequestID
}

// TraceID returns the current trace ID or an empty string outside middleware.
func TraceID(ctx context.Context) string {
	fields, _ := FromContext(ctx)
	return fields.TraceID
}

// InjectHTTP propagates correlation to a downstream HTTP request. The current
// server span becomes the remote parent; the downstream server creates its own span.
func InjectHTTP(request *http.Request) error {
	if request == nil {
		return errors.New("downstream HTTP request must not be nil")
	}
	fields, ok := FromContext(request.Context())
	if !ok {
		return errors.New("downstream HTTP request has no correlation context")
	}
	request.Header.Set(requestIDHeader, fields.RequestID)
	request.Header.Set(traceparentHeader, formatTraceparent(fields.TraceID, fields.SpanID, fields.TraceFlags))
	if fields.TraceState != "" {
		request.Header.Set(tracestateHeader, fields.TraceState)
	} else {
		request.Header.Del(tracestateHeader)
	}
	return nil
}

func (m *Manager) newFields(request *http.Request) (Fields, error) {
	requestID, err := m.reserve(clientRequestID(request.Header.Values(requestIDHeader)))
	if err != nil {
		return Fields{}, err
	}
	releaseOnError := true
	defer func() {
		if releaseOnError {
			m.release(requestID)
		}
	}()

	traceID, parentSpanID, flags, validParent := parseTraceparent(request.Header.Values(traceparentHeader))
	if !validParent {
		traceID, err = m.generator.TraceID()
		if err != nil || !validTraceID(traceID) {
			return Fields{}, fmt.Errorf("generate trace ID: %w", normalizeGeneratorError(err))
		}
		parentSpanID = ""
		flags = m.sampleFlag
	}
	spanID, err := m.generator.SpanID()
	if err != nil || !validSpanID(spanID) {
		return Fields{}, fmt.Errorf("generate span ID: %w", normalizeGeneratorError(err))
	}

	traceState := ""
	if validParent {
		traceState = parseTracestate(request.Header.Values(tracestateHeader))
	}
	releaseOnError = false
	return Fields{
		RequestID:    requestID,
		TraceID:      traceID,
		SpanID:       spanID,
		ParentSpanID: parentSpanID,
		TraceFlags:   flags,
		TraceState:   traceState,
	}, nil
}

func (m *Manager) reserve(candidate string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.purgeExpired(now)

	if validRequestID(candidate) && !m.used(candidate) {
		m.active[candidate] = struct{}{}
		return candidate, nil
	}
	for range maxGenerationTries {
		generated, err := m.generator.RequestID()
		if err != nil {
			return "", fmt.Errorf("generate request ID: %w", err)
		}
		if validRequestID(generated) && !m.used(generated) {
			m.active[generated] = struct{}{}
			return generated, nil
		}
	}
	return "", errors.New("generate request ID: collision or invalid generator output limit reached")
}

func (m *Manager) release(requestID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.active[requestID]; !ok {
		return
	}
	delete(m.active, requestID)
	if m.recentTTL == 0 {
		return
	}
	now := m.now()
	m.purgeExpired(now)
	if len(m.recent) >= m.maxRecent {
		m.evictOldest()
	}
	m.recent[requestID] = now.Add(m.recentTTL)
}

func (m *Manager) used(requestID string) bool {
	if _, ok := m.active[requestID]; ok {
		return true
	}
	_, ok := m.recent[requestID]
	return ok
}

func (m *Manager) purgeExpired(now time.Time) {
	for requestID, expiresAt := range m.recent {
		if !expiresAt.After(now) {
			delete(m.recent, requestID)
		}
	}
}

func (m *Manager) evictOldest() {
	var oldestID string
	var oldestExpiry time.Time
	for requestID, expiresAt := range m.recent {
		if oldestID == "" || expiresAt.Before(oldestExpiry) {
			oldestID = requestID
			oldestExpiry = expiresAt
		}
	}
	if oldestID != "" {
		delete(m.recent, oldestID)
	}
}

func clientRequestID(values []string) string {
	if len(values) != 1 {
		return ""
	}
	return strings.TrimSpace(values[0])
}

func validRequestID(value string) bool {
	return requestIDPattern.MatchString(value)
}

func parseTraceparent(values []string) (traceID, parentSpanID, flags string, ok bool) {
	if len(values) != 1 {
		return "", "", "", false
	}
	parts := strings.Split(strings.TrimSpace(values[0]), "-")
	if len(parts) != 4 || parts[0] != "00" || !validTraceID(parts[1]) || !validSpanID(parts[2]) ||
		!flagsPattern.MatchString(parts[3]) {
		return "", "", "", false
	}
	return parts[1], parts[2], parts[3], true
}

func validTraceID(value string) bool {
	return traceIDPattern.MatchString(value) && value != strings.Repeat("0", 32)
}

func validSpanID(value string) bool {
	return spanIDPattern.MatchString(value) && value != strings.Repeat("0", 16)
}

func parseTracestate(values []string) string {
	if len(values) != 1 {
		return ""
	}
	raw := strings.TrimSpace(values[0])
	if raw == "" || len(raw) > maxTracestateLength {
		return ""
	}
	members := strings.Split(raw, ",")
	if len(members) > maxTracestateItems {
		return ""
	}
	seen := make(map[string]struct{}, len(members))
	canonical := make([]string, 0, len(members))
	for _, member := range members {
		parts := strings.SplitN(strings.TrimSpace(member), "=", 2)
		if len(parts) != 2 || !stateKeyPattern.MatchString(parts[0]) || !validStateValue(parts[1]) {
			return ""
		}
		if _, duplicate := seen[parts[0]]; duplicate {
			return ""
		}
		seen[parts[0]] = struct{}{}
		canonical = append(canonical, parts[0]+"="+parts[1])
	}
	return strings.Join(canonical, ",")
}

func validStateValue(value string) bool {
	if value == "" || value[len(value)-1] == ' ' {
		return false
	}
	for _, character := range []byte(value) {
		if character < 0x20 || character > 0x7e || character == ',' || character == '=' {
			return false
		}
	}
	return true
}

func formatTraceparent(traceID, spanID, flags string) string {
	return "00-" + traceID + "-" + spanID + "-" + flags
}

func normalizeGeneratorError(err error) error {
	if err != nil {
		return err
	}
	return errors.New("generator returned an invalid identifier")
}

type randomGenerator struct {
	reader io.Reader
}

func (g randomGenerator) RequestID() (string, error) {
	value, err := randomHex(g.reader, 16)
	return "req_" + value, err
}

func (g randomGenerator) TraceID() (string, error) {
	return randomHex(g.reader, 16)
}

func (g randomGenerator) SpanID() (string, error) {
	return randomHex(g.reader, 8)
}

func randomHex(reader io.Reader, byteCount int) (string, error) {
	value := make([]byte, byteCount)
	if _, err := io.ReadFull(reader, value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}
