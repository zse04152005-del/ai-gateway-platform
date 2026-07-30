package sse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	maximumDownstreamDataBytes    = 256 * 1024
	maximumDownstreamCommentBytes = 4096
)

var (
	// ErrWriterInvalid means construction or event input violated the contract.
	ErrWriterInvalid = errors.New("SSE writer input is invalid")
	// ErrWriterUnsupported means the ResponseWriter cannot flush and enforce deadlines.
	ErrWriterUnsupported = errors.New("SSE response writer capabilities are unavailable")
	// ErrWriterClosed means a terminal event or prior failure made the writer unusable.
	ErrWriterClosed = errors.New("SSE writer is closed")
	// ErrWriteTimeout means the bounded downstream write deadline expired.
	ErrWriteTimeout = errors.New("SSE downstream write timed out")
	// ErrClientDisconnected means the request Context or connection closed.
	ErrClientDisconnected = errors.New("SSE client disconnected")
	// ErrWriteFailed means an otherwise unclassified downstream write failed.
	ErrWriteFailed = errors.New("SSE downstream write failed")

	downstreamRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`)
)

type responseController interface {
	Flush() error
	SetWriteDeadline(time.Time) error
}

// Writer emits complete, immediately flushed SSE events with a fresh bounded
// deadline per event. It is safe for serialized use by multiple goroutines.
type Writer struct {
	response   http.ResponseWriter
	controller responseController
	writeLimit time.Duration
	now        func() time.Time

	mu       sync.Mutex
	terminal bool
	failed   bool
}

// NewWriter validates streaming capabilities before mutating response headers.
func NewWriter(response http.ResponseWriter, requestID string, writeTimeout time.Duration) (*Writer, error) {
	if isNilResponseWriter(response) {
		return nil, ErrWriterInvalid
	}
	return newWriter(response, requestID, writeTimeout, time.Now, http.NewResponseController(response))
}

func newWriter(
	response http.ResponseWriter,
	requestID string,
	writeTimeout time.Duration,
	now func() time.Time,
	controller responseController,
) (*Writer, error) {
	if isNilResponseWriter(response) || controller == nil || now == nil {
		return nil, ErrWriterInvalid
	}
	if !downstreamRequestIDPattern.MatchString(requestID) {
		return nil, ErrWriterInvalid
	}
	if writeTimeout <= 0 || writeTimeout > time.Minute {
		return nil, ErrWriterInvalid
	}
	if now().IsZero() {
		return nil, ErrWriterInvalid
	}
	if !supportsResponseControl(response) {
		return nil, ErrWriterUnsupported
	}

	header := response.Header()
	header.Set("Cache-Control", "no-cache, no-store")
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("X-Accel-Buffering", "no")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Request-Id", requestID)
	header.Del("Content-Length")
	return &Writer{response: response, controller: controller, writeLimit: writeTimeout, now: now}, nil
}

// WriteJSON marshals one bounded JSON value as an SSE data event.
func (writer *Writer) WriteJSON(ctx context.Context, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ErrWriterInvalid
	}
	return writer.WriteData(ctx, encoded)
}

// WriteData emits one data event, prefixing every logical line as required by SSE.
func (writer *Writer) WriteData(ctx context.Context, data []byte) error {
	if len(data) == 0 || len(data) > maximumDownstreamDataBytes || !utf8.Valid(data) {
		return ErrWriterInvalid
	}
	return writer.writeEvent(ctx, encodeDataEvent(data), false)
}

// WriteComment emits one gateway-owned heartbeat/comment event.
func (writer *Writer) WriteComment(ctx context.Context, comment string) error {
	if comment == "" || len(comment) > maximumDownstreamCommentBytes || !utf8.ValidString(comment) || strings.IndexByte(comment, 0) >= 0 {
		return ErrWriterInvalid
	}
	return writer.writeEvent(ctx, encodeCommentEvent(comment), false)
}

// WriteDone emits the OpenAI-compatible terminal sentinel exactly once.
func (writer *Writer) WriteDone(ctx context.Context) error {
	return writer.writeEvent(ctx, []byte("data: [DONE]\n\n"), true)
}

func (writer *Writer) writeEvent(ctx context.Context, encoded []byte, terminal bool) error {
	if writer == nil || writer.response == nil || writer.controller == nil || writer.now == nil || ctx == nil {
		return ErrWriterInvalid
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.terminal || writer.failed {
		return ErrWriterClosed
	}
	if err := contextWriteError(ctx); err != nil {
		writer.failed = true
		return err
	}
	deadline := writer.now().Add(writer.writeLimit)
	if deadline.IsZero() {
		writer.failed = true
		return ErrWriterInvalid
	}
	if err := writer.controller.SetWriteDeadline(deadline); err != nil {
		writer.failed = true
		return classifyWriteError(ctx, err)
	}
	if err := writeAll(writer.response, encoded); err != nil {
		writer.failed = true
		return classifyWriteError(ctx, err)
	}
	if err := writer.controller.Flush(); err != nil {
		writer.failed = true
		return classifyWriteError(ctx, err)
	}
	if err := writer.controller.SetWriteDeadline(time.Time{}); err != nil {
		writer.failed = true
		return classifyWriteError(ctx, err)
	}
	writer.terminal = terminal
	return nil
}

func encodeDataEvent(data []byte) []byte {
	normalized := bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	normalized = bytes.ReplaceAll(normalized, []byte("\r"), []byte("\n"))
	lines := bytes.Split(normalized, []byte("\n"))
	encoded := make([]byte, 0, len(normalized)+len(lines)*6+1)
	for _, line := range lines {
		encoded = append(encoded, "data: "...)
		encoded = append(encoded, line...)
		encoded = append(encoded, '\n')
	}
	return append(encoded, '\n')
}

func encodeCommentEvent(comment string) []byte {
	normalized := strings.ReplaceAll(strings.ReplaceAll(comment, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(normalized, "\n")
	var encoded strings.Builder
	encoded.Grow(len(normalized) + len(lines)*3 + 1)
	for _, line := range lines {
		encoded.WriteString(": ")
		encoded.WriteString(line)
		encoded.WriteByte('\n')
	}
	encoded.WriteByte('\n')
	return []byte(encoded.String())
}

func writeAll(writer io.Writer, encoded []byte) error {
	for len(encoded) > 0 {
		written, err := writer.Write(encoded)
		if written < 0 || written > len(encoded) {
			return io.ErrShortWrite
		}
		encoded = encoded[written:]
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func contextWriteError(ctx context.Context) error {
	if ctx == nil || ctx.Err() == nil {
		return nil
	}
	cause := context.Cause(ctx)
	if cause == nil {
		cause = ctx.Err()
	}
	return &writerError{kind: ErrClientDisconnected, cause: errors.Join(ctx.Err(), cause)}
}

func classifyWriteError(ctx context.Context, cause error) error {
	if contextErr := contextWriteError(ctx); contextErr != nil {
		return contextErr
	}
	kind := ErrWriteFailed
	var networkError net.Error
	switch {
	case errors.Is(cause, http.ErrNotSupported):
		kind = ErrWriterUnsupported
	case errors.Is(cause, os.ErrDeadlineExceeded), errors.As(cause, &networkError) && networkError.Timeout():
		kind = ErrWriteTimeout
	case errors.Is(cause, net.ErrClosed), errors.Is(cause, syscall.EPIPE), errors.Is(cause, syscall.ECONNRESET):
		kind = ErrClientDisconnected
	}
	return &writerError{kind: kind, cause: cause}
}

type writerError struct {
	kind  error
	cause error
}

func (failure *writerError) Error() string {
	if failure == nil || failure.kind == nil {
		return ErrWriteFailed.Error()
	}
	return failure.kind.Error()
}

func (failure *writerError) Unwrap() []error {
	if failure == nil {
		return nil
	}
	return []error{failure.kind, failure.cause}
}

func supportsResponseControl(response http.ResponseWriter) bool {
	hasFlush, hasDeadline := false, false
	for depth := 0; response != nil && depth < 32; depth++ {
		if _, ok := response.(interface{ FlushError() error }); ok {
			hasFlush = true
		}
		if _, ok := response.(http.Flusher); ok {
			hasFlush = true
		}
		if _, ok := response.(interface{ SetWriteDeadline(time.Time) error }); ok {
			hasDeadline = true
		}
		if hasFlush && hasDeadline {
			return true
		}
		unwrapper, ok := response.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		response = unwrapper.Unwrap()
	}
	return false
}

func isNilResponseWriter(response http.ResponseWriter) bool {
	if response == nil {
		return true
	}
	value := reflect.ValueOf(response)
	kind := value.Kind()
	if kind >= reflect.Chan && kind <= reflect.Slice {
		return value.IsNil()
	}
	return false
}

var _ error = (*writerError)(nil)
