package sse

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"testing"
)

func TestDecoderHandlesFragmentedMultilineEventsAndDone(t *testing.T) {
	input := "\xef\xbb\xbf\r\n\r\nevent: delta\r\nid: safe-id\r\nretry: 1000\r\ndata: first\r\ndata: second\r\n\r\n: heartbeat\n\ndata: [DONE]\n\n"
	decoder := newTestDecoder(t, oneByteReader{reader: bytes.NewBufferString(input)}, Limits{
		MaxLineBytes: 64, MaxEventBytes: 256,
	})

	first, err := decoder.Next()
	if err != nil || first.Type != "delta" || string(first.Data) != "first\nsecond" || first.Comment {
		t.Fatalf("first event = %+v/%v", first, err)
	}
	heartbeat, err := decoder.Next()
	if err != nil || !heartbeat.Comment || len(heartbeat.Data) != 0 {
		t.Fatalf("heartbeat = %+v/%v", heartbeat, err)
	}
	done, err := decoder.Next()
	if err != nil || !done.IsDone() {
		t.Fatalf("done event = %+v/%v", done, err)
	}
	if _, err := decoder.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal Next() error = %v, want EOF", err)
	}
}

func TestDecoderDispatchesFinalEventAtEOFAndCopiesData(t *testing.T) {
	decoder := newTestDecoder(t, bytes.NewBufferString("data: final"), Limits{MaxLineBytes: 32, MaxEventBytes: 64})
	event, err := decoder.Next()
	if err != nil || string(event.Data) != "final" {
		t.Fatalf("final event = %+v/%v", event, err)
	}
	event.Data[0] = 'F'
	if _, err := decoder.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("Next() after final event error = %v", err)
	}
}

func TestDecoderPreservesEmptyDataEventAndIgnoresCommentsMixedWithData(t *testing.T) {
	decoder := newTestDecoder(t, bytes.NewBufferString("data:\n\n: note\ndata: value\n\n"), Limits{
		MaxLineBytes: 32, MaxEventBytes: 64,
	})
	empty, err := decoder.Next()
	if err != nil || len(empty.Data) != 0 || empty.Comment {
		t.Fatalf("empty data event = %+v/%v", empty, err)
	}
	mixed, err := decoder.Next()
	if err != nil || string(mixed.Data) != "value" || mixed.Comment {
		t.Fatalf("mixed event = %+v/%v", mixed, err)
	}
}

func TestDecoderRejectsMalformedOrUnboundedInput(t *testing.T) {
	tests := []struct {
		name   string
		input  []byte
		limits Limits
		want   error
	}{
		{name: "line", input: []byte("data: 123456789\n"), limits: Limits{MaxLineBytes: 8, MaxEventBytes: 32}, want: ErrLineTooLarge},
		{name: "event", input: []byte(": 123456\n: 123456\n: 1\n\n"), limits: Limits{MaxLineBytes: 16, MaxEventBytes: 16}, want: ErrEventTooLarge},
		{name: "unknown field", input: []byte("payload: value\n\n"), limits: Limits{MaxLineBytes: 32, MaxEventBytes: 64}, want: ErrUnknownField},
		{name: "invalid retry", input: []byte("retry: soon\n\n"), limits: Limits{MaxLineBytes: 32, MaxEventBytes: 64}, want: ErrInvalidField},
		{name: "invalid id", input: []byte("id: bad\x00id\n\n"), limits: Limits{MaxLineBytes: 32, MaxEventBytes: 64}, want: ErrInvalidField},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decoder := newTestDecoder(t, bytes.NewReader(test.input), test.limits)
			if _, err := decoder.Next(); !errors.Is(err, test.want) {
				t.Fatalf("Next() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestNewDecoderValidatesDependenciesAndLimits(t *testing.T) {
	tests := []struct {
		name   string
		reader io.Reader
		limits Limits
	}{
		{name: "nil reader", limits: Limits{MaxLineBytes: 1, MaxEventBytes: 1}},
		{name: "zero line", reader: bytes.NewReader(nil), limits: Limits{MaxEventBytes: 1}},
		{name: "event below line", reader: bytes.NewReader(nil), limits: Limits{MaxLineBytes: 2, MaxEventBytes: 1}},
		{name: "line above maximum", reader: bytes.NewReader(nil), limits: Limits{MaxLineBytes: 1<<20 + 1, MaxEventBytes: 1<<20 + 1}},
		{name: "event above maximum", reader: bytes.NewReader(nil), limits: Limits{MaxLineBytes: 1, MaxEventBytes: 4<<20 + 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if decoder, err := NewDecoder(test.reader, test.limits); err == nil || decoder != nil {
				t.Fatalf("NewDecoder() = %#v/%v, want nil/error", decoder, err)
			}
		})
	}

	decoder := newTestDecoder(t, bytes.NewReader(nil), Limits{MaxLineBytes: 1, MaxEventBytes: 1})
	if got, err := decoder.Next(); !errors.Is(err, io.EOF) || !reflect.DeepEqual(got, Event{}) {
		t.Fatalf("empty stream = %+v/%v", got, err)
	}
	var nilDecoder *Decoder
	if _, err := nilDecoder.Next(); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("nil Decoder.Next() error = %v", err)
	}
}

func newTestDecoder(t *testing.T, reader io.Reader, limits Limits) *Decoder {
	t.Helper()
	decoder, err := NewDecoder(reader, limits)
	if err != nil {
		t.Fatalf("NewDecoder() error = %v", err)
	}
	return decoder
}

type oneByteReader struct {
	reader *bytes.Buffer
}

func (reader oneByteReader) Read(target []byte) (int, error) {
	if len(target) > 1 {
		target = target[:1]
	}
	return reader.reader.Read(target)
}
