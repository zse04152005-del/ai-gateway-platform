// Package sse implements a bounded, provider-neutral Server-Sent Events decoder.
package sse

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

var (
	// ErrLineTooLarge means one physical SSE line exceeded its configured bound.
	ErrLineTooLarge = errors.New("SSE line exceeds limit")
	// ErrEventTooLarge means one event block exceeded its aggregate bound.
	ErrEventTooLarge = errors.New("SSE event exceeds limit")
	// ErrUnknownField means an event contained a field outside the SSE grammar.
	ErrUnknownField = errors.New("SSE field is unknown")
	// ErrInvalidField means a standard SSE field contained an invalid value.
	ErrInvalidField = errors.New("SSE field is invalid")
)

// Limits bound physical lines and complete event blocks. Event bytes include
// comments and transport metadata, not only data fields.
type Limits struct {
	MaxLineBytes  int
	MaxEventBytes int
}

// Event is one decoded SSE block. A comment-only block has Comment=true and
// empty Data. Multiple data fields are joined with exactly one newline.
type Event struct {
	Type    string
	Data    []byte
	Comment bool
}

// IsDone recognizes the OpenAI-compatible terminal sentinel without treating
// surrounding transport whitespace as model content.
func (event Event) IsDone() bool {
	return bytes.Equal(bytes.TrimSpace(event.Data), []byte("[DONE]"))
}

// Decoder incrementally parses one stream. Callers must serialize Next calls.
type Decoder struct {
	reader *bufio.Reader
	limits Limits
	first  bool
}

// NewDecoder validates fixed resource bounds and creates an incremental reader.
func NewDecoder(reader io.Reader, limits Limits) (*Decoder, error) {
	if reader == nil {
		return nil, errors.New("SSE reader must not be nil")
	}
	if limits.MaxLineBytes < 1 || limits.MaxLineBytes > 1<<20 {
		return nil, errors.New("SSE line limit must be between 1 and 1048576 bytes")
	}
	if limits.MaxEventBytes < limits.MaxLineBytes || limits.MaxEventBytes > 4<<20 {
		return nil, errors.New("SSE event limit must cover the line limit and not exceed 4194304 bytes")
	}
	return &Decoder{
		reader: bufio.NewReaderSize(reader, limits.MaxLineBytes+2), limits: limits, first: true,
	}, nil
}

// Next skips empty separators and returns one data or comment event. EOF after
// a non-empty final block dispatches that block, matching the SSE algorithm.
func (decoder *Decoder) Next() (Event, error) {
	if decoder == nil || decoder.reader == nil {
		return Event{}, io.ErrClosedPipe
	}
	var data bytes.Buffer
	eventType := "message"
	comment := false
	started := false
	dataSeen := false
	eventBytes := 0

	for {
		line, err := decoder.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) && started {
				return buildEvent(eventType, data.Bytes(), comment, dataSeen), nil
			}
			return Event{}, err
		}
		if decoder.first {
			decoder.first = false
			line = bytes.TrimPrefix(line, []byte{0xef, 0xbb, 0xbf})
		}
		if len(line) == 0 {
			if !started {
				continue
			}
			return buildEvent(eventType, data.Bytes(), comment, dataSeen), nil
		}
		started = true
		if eventBytes > decoder.limits.MaxEventBytes-len(line) {
			return Event{}, ErrEventTooLarge
		}
		eventBytes += len(line)

		if line[0] == ':' {
			comment = true
			continue
		}
		field, value, found := bytes.Cut(line, []byte{':'})
		if !found {
			field, value = line, nil
		}
		value = bytes.TrimPrefix(value, []byte{' '})
		switch string(field) {
		case "data":
			separator := 0
			if dataSeen {
				separator = 1
			}
			if data.Len() > decoder.limits.MaxEventBytes-separator-len(value) {
				return Event{}, ErrEventTooLarge
			}
			if dataSeen {
				data.WriteByte('\n')
			}
			data.Write(value)
			dataSeen = true
		case "event":
			if !utf8.Valid(value) || bytes.IndexByte(value, 0) >= 0 {
				return Event{}, fmt.Errorf("%w: event", ErrInvalidField)
			}
			eventType = string(value)
		case "id":
			if bytes.IndexByte(value, 0) >= 0 {
				return Event{}, fmt.Errorf("%w: id", ErrInvalidField)
			}
		case "retry":
			if !asciiDigits(value) {
				return Event{}, fmt.Errorf("%w: retry", ErrInvalidField)
			}
		default:
			return Event{}, ErrUnknownField
		}
	}
}

func (decoder *Decoder) readLine() ([]byte, error) {
	line, err := decoder.reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return nil, ErrLineTooLarge
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	line = bytes.TrimSuffix(line, []byte{'\n'})
	line = bytes.TrimSuffix(line, []byte{'\r'})
	if len(line) > decoder.limits.MaxLineBytes {
		return nil, ErrLineTooLarge
	}
	if errors.Is(err, io.EOF) && len(line) == 0 {
		return nil, io.EOF
	}
	return append([]byte(nil), line...), nil
}

func buildEvent(eventType string, data []byte, comment, dataSeen bool) Event {
	return Event{
		Type: eventType, Data: append([]byte(nil), data...), Comment: comment && !dataSeen,
	}
}

func asciiDigits(value []byte) bool {
	if len(value) == 0 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}
