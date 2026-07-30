package correlation

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testTraceID      = "11111111111111111111111111111111"
	testParentSpanID = "2222222222222222"
	testSpanIDOne    = "3333333333333333"
	testSpanIDTwo    = "4444444444444444"
)

type sequenceGenerator struct {
	mu         sync.Mutex
	requestIDs []string
	traceIDs   []string
	spanIDs    []string
	err        error
}

func (g *sequenceGenerator) RequestID() (string, error) {
	return g.next(&g.requestIDs)
}

func (g *sequenceGenerator) TraceID() (string, error) {
	return g.next(&g.traceIDs)
}

func (g *sequenceGenerator) SpanID() (string, error) {
	return g.next(&g.spanIDs)
}

func (g *sequenceGenerator) next(values *[]string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil {
		return "", g.err
	}
	if len(*values) == 0 {
		return "", errors.New("test generator sequence exhausted")
	}
	value := (*values)[0]
	*values = (*values)[1:]
	return value, nil
}

func TestMiddlewareAcceptsValidClientRequestAndTraceContext(t *testing.T) {
	generator := &sequenceGenerator{spanIDs: []string{testSpanIDOne}}
	manager := newTestManager(t, generator, nil)
	var captured Fields
	handler := manager.Middleware(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var ok bool
		captured, ok = FromContext(request.Context())
		if !ok {
			t.Fatal("correlation context missing")
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "/test", nil)
	request.Header.Set(requestIDHeader, "client-request-123")
	request.Header.Set(traceparentHeader, "00-"+testTraceID+"-"+testParentSpanID+"-01")
	request.Header.Set(tracestateHeader, "vendor=value,tenant@system=opaque")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if captured.RequestID != "client-request-123" || captured.TraceID != testTraceID ||
		captured.ParentSpanID != testParentSpanID || captured.SpanID != testSpanIDOne || captured.TraceFlags != "01" {
		t.Fatalf("captured fields = %+v", captured)
	}
	if captured.TraceState != "vendor=value,tenant@system=opaque" {
		t.Fatalf("tracestate = %q", captured.TraceState)
	}
	if got := recorder.Header().Get(requestIDHeader); got != captured.RequestID {
		t.Fatalf("response request ID = %q", got)
	}
	wantTraceparent := "00-" + testTraceID + "-" + testSpanIDOne + "-01"
	if got := recorder.Header().Get(traceparentHeader); got != wantTraceparent {
		t.Fatalf("response traceparent = %q, want %q", got, wantTraceparent)
	}
}

func TestMiddlewareRegeneratesInvalidAndRecentlyUsedRequestIDs(t *testing.T) {
	generator := &sequenceGenerator{
		requestIDs: []string{"req_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "req_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		traceIDs:   []string{testTraceID, "55555555555555555555555555555555"},
		spanIDs:    []string{testSpanIDOne, testSpanIDTwo},
	}
	manager := newTestManager(t, generator, nil)
	handler := manager.Middleware(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))

	invalid := httptest.NewRequest(http.MethodGet, "/", nil)
	invalid.Header.Set(requestIDHeader, "bad id with spaces")
	invalid.Header.Set(traceparentHeader, "00-invalid-parent")
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, invalid)
	if got := first.Header().Get(requestIDHeader); got != "req_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("generated request ID = %q", got)
	}
	if !strings.Contains(first.Header().Get(traceparentHeader), testTraceID) {
		t.Fatalf("generated traceparent = %q", first.Header().Get(traceparentHeader))
	}

	replay := httptest.NewRequest(http.MethodGet, "/", nil)
	replay.Header.Set(requestIDHeader, "req_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, replay)
	if got := second.Header().Get(requestIDHeader); got != "req_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("recent collision request ID = %q", got)
	}
}

func TestMiddlewareRegeneratesConcurrentActiveCollision(t *testing.T) {
	generator := &sequenceGenerator{
		requestIDs: []string{"req_cccccccccccccccccccccccccccccccc"},
		traceIDs:   []string{testTraceID, "55555555555555555555555555555555"},
		spanIDs:    []string{testSpanIDOne, testSpanIDTwo},
	}
	manager := newTestManager(t, generator, nil)
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	handler := manager.Middleware(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if RequestID(request.Context()) == "concurrent-client-id" {
			close(firstEntered)
			<-releaseFirst
		}
		writer.WriteHeader(http.StatusNoContent)
	}))

	firstRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	firstRequest.Header.Set(requestIDHeader, "concurrent-client-id")
	firstRecorder := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(firstRecorder, firstRequest)
		close(firstDone)
	}()
	<-firstEntered

	secondRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	secondRequest.Header.Set(requestIDHeader, "concurrent-client-id")
	secondRecorder := httptest.NewRecorder()
	handler.ServeHTTP(secondRecorder, secondRequest)
	if got := secondRecorder.Header().Get(requestIDHeader); got != "req_cccccccccccccccccccccccccccccccc" {
		t.Fatalf("active collision request ID = %q", got)
	}
	close(releaseFirst)
	<-firstDone
}

func TestInjectHTTPPreservesRequestAndTraceAcrossManagers(t *testing.T) {
	upstreamGenerator := &sequenceGenerator{
		requestIDs: []string{"req_dddddddddddddddddddddddddddddddd"},
		traceIDs:   []string{testTraceID},
		spanIDs:    []string{testSpanIDOne},
	}
	downstreamGenerator := &sequenceGenerator{spanIDs: []string{testSpanIDTwo}}
	upstream := newTestManager(t, upstreamGenerator, nil)
	downstream := newTestManager(t, downstreamGenerator, nil)
	var upstreamFields, downstreamFields Fields
	downstreamHandler := downstream.Middleware(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		downstreamFields, _ = FromContext(request.Context())
		writer.WriteHeader(http.StatusNoContent)
	}))
	upstreamHandler := upstream.Middleware(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstreamFields, _ = FromContext(request.Context())
		downstreamRequest := httptest.NewRequest(http.MethodGet, "http://downstream.local/work", nil).WithContext(request.Context())
		if err := InjectHTTP(downstreamRequest); err != nil {
			t.Fatalf("InjectHTTP() error = %v", err)
		}
		downstreamHandler.ServeHTTP(httptest.NewRecorder(), downstreamRequest)
		writer.WriteHeader(http.StatusNoContent)
	}))

	upstreamHandler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if downstreamFields.RequestID != upstreamFields.RequestID || downstreamFields.TraceID != upstreamFields.TraceID {
		t.Fatalf("upstream = %+v; downstream = %+v", upstreamFields, downstreamFields)
	}
	if downstreamFields.ParentSpanID != upstreamFields.SpanID || downstreamFields.SpanID == upstreamFields.SpanID {
		t.Fatalf("span relationship upstream = %+v; downstream = %+v", upstreamFields, downstreamFields)
	}
}

func TestMiddlewareDropsInvalidTracestateAndMultipleHeaders(t *testing.T) {
	generator := &sequenceGenerator{
		requestIDs: []string{"req_eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"},
		spanIDs:    []string{testSpanIDOne},
	}
	manager := newTestManager(t, generator, nil)
	var captured Fields
	handler := manager.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		captured, _ = FromContext(request.Context())
	}))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Add(requestIDHeader, "client-request-one")
	request.Header.Add(requestIDHeader, "client-request-two")
	request.Header.Set(traceparentHeader, "00-"+testTraceID+"-"+testParentSpanID+"-00")
	request.Header.Set(tracestateHeader, "duplicate=one,duplicate=two")

	handler.ServeHTTP(httptest.NewRecorder(), request)

	if captured.RequestID != "req_eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee" {
		t.Fatalf("multiple client IDs were not replaced: %+v", captured)
	}
	if captured.TraceState != "" {
		t.Fatalf("invalid tracestate retained: %q", captured.TraceState)
	}
}

func TestTraceContextParsersRejectAmbiguousAndUnsafeValues(t *testing.T) {
	valid := "00-" + testTraceID + "-" + testParentSpanID + "-01"
	invalidParents := [][]string{
		nil,
		{valid, valid},
		{"01-" + testTraceID + "-" + testParentSpanID + "-01"},
		{"00-" + strings.Repeat("0", 32) + "-" + testParentSpanID + "-01"},
		{"00-" + testTraceID + "-" + strings.Repeat("0", 16) + "-01"},
		{"00-" + strings.Repeat("A", 32) + "-" + testParentSpanID + "-01"},
	}
	for _, values := range invalidParents {
		if _, _, _, ok := parseTraceparent(values); ok {
			t.Errorf("parseTraceparent(%q) accepted invalid value", values)
		}
	}
	if traceID, parentID, flags, ok := parseTraceparent([]string{valid}); !ok ||
		traceID != testTraceID || parentID != testParentSpanID || flags != "01" {
		t.Fatalf("valid traceparent = %q, %q, %q, %v", traceID, parentID, flags, ok)
	}

	invalidStates := [][]string{
		{"duplicate=one,duplicate=two"},
		{"bad key=value"},
		{"valid=value=with-equals"},
		{strings.Repeat("a", maxTracestateLength+1) + "=value"},
	}
	for _, values := range invalidStates {
		if got := parseTracestate(values); got != "" {
			t.Errorf("parseTracestate(%q) = %q", values, got)
		}
	}
}

func TestMiddlewareReturnsSafeErrorWhenEntropyFails(t *testing.T) {
	generator := &sequenceGenerator{err: errors.New("entropy source /internal/device failed")}
	manager := newTestManager(t, generator, nil)
	handler := manager.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called after correlation failure")
	}))
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), "CORRELATION_CONTEXT_FAILED") {
		t.Fatalf("failure response = %d %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "/internal/device") {
		t.Fatalf("failure response leaked generator error: %s", recorder.Body.String())
	}
}

func TestRecentWindowExpiresAndRemainsBounded(t *testing.T) {
	now := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	generator := &sequenceGenerator{
		traceIDs: []string{testTraceID, "55555555555555555555555555555555", "66666666666666666666666666666666"},
		spanIDs:  []string{testSpanIDOne, testSpanIDTwo, "7777777777777777"},
	}
	manager := newTestManager(t, generator, func(options *Options) {
		options.Now = func() time.Time { return now }
		options.RecentTTL = time.Minute
		options.MaxRecent = 1
	})
	handler := manager.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	for _, requestID := range []string{"bounded-request-one", "bounded-request-two"} {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Header.Set(requestIDHeader, requestID)
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}
	if len(manager.recent) != 1 {
		t.Fatalf("recent size = %d, want 1", len(manager.recent))
	}
	now = now.Add(2 * time.Minute)
	reused := httptest.NewRequest(http.MethodGet, "/", nil)
	reused.Header.Set(requestIDHeader, "bounded-request-two")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, reused)
	if got := recorder.Header().Get(requestIDHeader); got != "bounded-request-two" {
		t.Fatalf("expired request ID = %q", got)
	}
}

func TestNewAndContextHelpersValidateInputs(t *testing.T) {
	tests := []Options{
		{RecentTTL: -time.Second},
		{MaxRecent: -1},
		{SampleFlag: "0G"},
		{ErrorType: "Invalid Type"},
	}
	for _, options := range tests {
		if _, err := New(options); err == nil {
			t.Fatalf("New(%+v) error = nil", options)
		}
	}
	var nilContext context.Context
	if _, ok := FromContext(nilContext); ok || RequestID(context.Background()) != "" || TraceID(context.Background()) != "" {
		t.Fatal("empty context helpers returned correlation")
	}
	if err := InjectHTTP(nil); err == nil {
		t.Fatal("InjectHTTP(nil) error = nil")
	}
	if err := InjectHTTP(httptest.NewRequest(http.MethodGet, "/", nil)); err == nil {
		t.Fatal("InjectHTTP without context error = nil")
	}
}

func newTestManager(t *testing.T, generator Generator, mutate func(*Options)) *Manager {
	t.Helper()
	options := Options{Generator: generator, ErrorType: "gateway_error"}
	if mutate != nil {
		mutate(&options)
	}
	manager, err := New(options)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return manager
}
