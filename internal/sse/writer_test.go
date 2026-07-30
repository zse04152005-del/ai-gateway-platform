package sse

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWriterSetsSafeHeadersAndFlushesEveryEvent(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	response := newControlledResponse()
	writer, err := newWriter(response, "request-sse-0001", 2*time.Second, func() time.Time { return now }, response)
	if err != nil {
		t.Fatalf("newWriter() error = %v", err)
	}
	assertWriterHeaders(t, response.header, "request-sse-0001")
	if err := writer.WriteData(context.Background(), []byte("first\r\nsecond")); err != nil {
		t.Fatalf("WriteData() error = %v", err)
	}
	if err := writer.WriteComment(context.Background(), "gateway\rheartbeat"); err != nil {
		t.Fatalf("WriteComment() error = %v", err)
	}
	if err := writer.WriteJSON(context.Background(), map[string]string{"delta": "safe"}); err != nil {
		t.Fatalf("WriteJSON() error = %v", err)
	}
	if err := writer.WriteDone(context.Background()); err != nil {
		t.Fatalf("WriteDone() error = %v", err)
	}
	want := "data: first\ndata: second\n\n: gateway\n: heartbeat\n\ndata: {\"delta\":\"safe\"}\n\ndata: [DONE]\n\n"
	if got := response.body.String(); got != want {
		t.Fatalf("SSE body = %q, want %q", got, want)
	}
	if response.flushes != 4 {
		t.Fatalf("flush count = %d, want 4", response.flushes)
	}
	if len(response.deadlines) != 8 {
		t.Fatalf("deadline calls = %d, want 8", len(response.deadlines))
	}
	for index, deadline := range response.deadlines {
		if index%2 == 0 && !deadline.Equal(now.Add(2*time.Second)) {
			t.Errorf("event deadline[%d] = %v", index, deadline)
		}
		if index%2 == 1 && !deadline.IsZero() {
			t.Errorf("cleared deadline[%d] = %v", index, deadline)
		}
	}
	if err := writer.WriteData(context.Background(), []byte("after done")); !errors.Is(err, ErrWriterClosed) {
		t.Fatalf("write after done error = %v", err)
	}
}

func TestWriterRejectsInvalidConstructionAndInput(t *testing.T) {
	response := newControlledResponse()
	now := func() time.Time { return time.Unix(100, 0) }
	var typedNil *controlledResponse
	tests := []struct {
		name       string
		response   http.ResponseWriter
		requestID  string
		timeout    time.Duration
		clock      func() time.Time
		controller responseController
		want       error
	}{
		{name: "nil response", requestID: "request-sse-0001", timeout: time.Second, clock: now, controller: response, want: ErrWriterInvalid},
		{name: "typed nil response", response: typedNil, requestID: "request-sse-0001", timeout: time.Second, clock: now, controller: response, want: ErrWriterInvalid},
		{name: "invalid request id", response: response, requestID: "bad", timeout: time.Second, clock: now, controller: response, want: ErrWriterInvalid},
		{name: "zero timeout", response: response, requestID: "request-sse-0001", clock: now, controller: response, want: ErrWriterInvalid},
		{name: "excessive timeout", response: response, requestID: "request-sse-0001", timeout: time.Minute + 1, clock: now, controller: response, want: ErrWriterInvalid},
		{name: "nil clock", response: response, requestID: "request-sse-0001", timeout: time.Second, controller: response, want: ErrWriterInvalid},
		{name: "zero clock", response: response, requestID: "request-sse-0001", timeout: time.Second, clock: func() time.Time { return time.Time{} }, controller: response, want: ErrWriterInvalid},
		{name: "nil controller", response: response, requestID: "request-sse-0001", timeout: time.Second, clock: now, want: ErrWriterInvalid},
		{name: "unsupported", response: httptest.NewRecorder(), requestID: "request-sse-0001", timeout: time.Second, clock: now, controller: response, want: ErrWriterUnsupported},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			writer, err := newWriter(test.response, test.requestID, test.timeout, test.clock, test.controller)
			if !errors.Is(err, test.want) || writer != nil {
				t.Fatalf("newWriter() = %#v/%v, want nil/%v", writer, err, test.want)
			}
		})
	}

	writer := mustTestWriter(t, newControlledResponse())
	invalid := []func() error{
		func() error { return writer.WriteData(context.Background(), nil) },
		func() error { return writer.WriteData(context.Background(), []byte{0xff}) },
		func() error {
			return writer.WriteData(context.Background(), bytes.Repeat([]byte{'x'}, maximumDownstreamDataBytes+1))
		},
		func() error { return writer.WriteComment(context.Background(), "") },
		func() error { return writer.WriteComment(context.Background(), "bad\x00comment") },
		func() error {
			return writer.WriteComment(context.Background(), strings.Repeat("x", maximumDownstreamCommentBytes+1))
		},
		func() error { return writer.WriteJSON(context.Background(), make(chan int)) },
	}
	for index, invoke := range invalid {
		if err := invoke(); !errors.Is(err, ErrWriterInvalid) {
			t.Errorf("invalid operation %d error = %v", index, err)
		}
	}
	var nilWriter *Writer
	if err := nilWriter.WriteData(context.Background(), []byte("value")); !errors.Is(err, ErrWriterInvalid) {
		t.Fatalf("nil Writer error = %v", err)
	}
}

func TestWriterClassifiesTimeoutDisconnectAndPermanentFailure(t *testing.T) {
	t.Run("context cancellation", func(t *testing.T) {
		response := newControlledResponse()
		writer := mustTestWriter(t, response)
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(errors.New("test disconnect cause"))
		err := writer.WriteData(ctx, []byte("value"))
		if !errors.Is(err, ErrClientDisconnected) || !errors.Is(err, context.Canceled) || response.body.Len() != 0 {
			t.Fatalf("cancelled write = %v, bytes=%d", err, response.body.Len())
		}
		if err := writer.WriteData(context.Background(), []byte("again")); !errors.Is(err, ErrWriterClosed) {
			t.Fatalf("write after cancellation error = %v", err)
		}
	})

	tests := []struct {
		name string
		err  error
		want error
	}{
		{name: "timeout", err: timeoutError{}, want: ErrWriteTimeout},
		{name: "closed", err: net.ErrClosed, want: ErrClientDisconnected},
		{name: "other", err: errors.New("private writer detail"), want: ErrWriteFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := newControlledResponse()
			response.writeErr = test.err
			writer := mustTestWriter(t, response)
			err := writer.WriteData(context.Background(), []byte("value"))
			if !errors.Is(err, test.want) || strings.Contains(err.Error(), "private") {
				t.Fatalf("WriteData() error = %v, want safe %v", err, test.want)
			}
		})
	}

	response := newControlledResponse()
	response.flushErr = net.ErrClosed
	writer := mustTestWriter(t, response)
	if err := writer.WriteData(context.Background(), []byte("value")); !errors.Is(err, ErrClientDisconnected) {
		t.Fatalf("flush disconnect error = %v", err)
	}
}

func TestWriterFlushesBeforeHandlerReturnsAndDetectsRealDisconnect(t *testing.T) {
	firstFlushed := make(chan struct{})
	disconnected := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		writer, err := NewWriter(response, "request-sse-real", time.Second)
		if err != nil {
			disconnected <- err
			return
		}
		if err := writer.WriteData(request.Context(), []byte("first")); err != nil {
			disconnected <- err
			return
		}
		close(firstFlushed)
		<-request.Context().Done()
		disconnected <- writer.WriteData(request.Context(), []byte("second"))
	}))
	t.Cleanup(server.Close)

	// #nosec G107 -- the URL belongs to this in-process httptest Server.
	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatalf("GET streaming server: %v", err)
	}
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("first flushed line = %q/%v", line, err)
	}
	select {
	case <-firstFlushed:
	case <-time.After(time.Second):
		t.Fatal("first event was not flushed before handler completion")
	}
	assertWriterHeaders(t, response.Header, "request-sse-real")
	_ = response.Body.Close()
	select {
	case err := <-disconnected:
		if !errors.Is(err, ErrClientDisconnected) {
			t.Fatalf("real disconnect error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not observe client disconnect")
	}
}

func mustTestWriter(t *testing.T, response *controlledResponse) *Writer {
	t.Helper()
	writer, err := newWriter(
		response, "request-sse-0001", time.Second, func() time.Time { return time.Unix(100, 0) }, response,
	)
	if err != nil {
		t.Fatalf("newWriter() error = %v", err)
	}
	return writer
}

func assertWriterHeaders(t *testing.T, header http.Header, requestID string) {
	t.Helper()
	want := map[string]string{
		"Cache-Control": "no-cache, no-store", "Content-Type": "text/event-stream; charset=utf-8",
		"X-Accel-Buffering": "no", "X-Content-Type-Options": "nosniff", "X-Request-Id": requestID,
	}
	for name, value := range want {
		if got := header.Get(name); got != value {
			t.Errorf("header %s = %q, want %q", name, got, value)
		}
	}
	if header.Get("Content-Length") != "" || header.Get("Connection") != "" {
		t.Errorf("unsafe framing headers = %#v", header)
	}
}

type controlledResponse struct {
	header    http.Header
	body      bytes.Buffer
	deadlines []time.Time
	flushes   int
	writeErr  error
	flushErr  error
}

func newControlledResponse() *controlledResponse {
	return &controlledResponse{header: make(http.Header)}
}

func (response *controlledResponse) Header() http.Header { return response.header }
func (*controlledResponse) WriteHeader(int)              {}

func (response *controlledResponse) Write(value []byte) (int, error) {
	if response.writeErr != nil {
		return 0, response.writeErr
	}
	return response.body.Write(value)
}

func (response *controlledResponse) FlushError() error {
	response.flushes++
	return response.flushErr
}

func (response *controlledResponse) Flush() error {
	return response.FlushError()
}

func (response *controlledResponse) SetWriteDeadline(deadline time.Time) error {
	response.deadlines = append(response.deadlines, deadline)
	return nil
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "private timeout detail" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
