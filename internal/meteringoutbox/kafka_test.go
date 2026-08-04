package meteringoutbox

import (
	"context"
	"errors"
	"testing"
)

func TestKafkaSinkConstructorAndInvalidRecords(t *testing.T) {
	for _, brokers := range [][]string{nil, {}, {""}, {" localhost:9092"}} {
		if _, err := NewKafkaSink(brokers); !errors.Is(err, ErrKafkaSinkInvalid) {
			t.Errorf("NewKafkaSink(%q) error = %v", brokers, err)
		}
	}
	sink, err := NewKafkaSink([]string{"127.0.0.1:1"})
	if err != nil {
		t.Fatalf("NewKafkaSink(valid) error = %v", err)
	}
	t.Cleanup(sink.Close)
	var nilContext context.Context
	if err := sink.Publish(nilContext, "event-id", []byte(`{}`)); !errors.Is(err, ErrKafkaSinkInvalid) {
		t.Fatalf("Publish(nil context) error = %v", err)
	}
	if err := sink.Publish(context.Background(), "", []byte(`{}`)); !errors.Is(err, ErrKafkaSinkInvalid) {
		t.Fatalf("Publish(empty id) error = %v", err)
	}
	if err := sink.Publish(context.Background(), "event-id", nil); !errors.Is(err, ErrKafkaSinkInvalid) {
		t.Fatalf("Publish(empty payload) error = %v", err)
	}
	var nilSink *KafkaSink
	if err := nilSink.Publish(context.Background(), "event-id", []byte(`{}`)); !errors.Is(err, ErrKafkaSinkInvalid) {
		t.Fatalf("nil sink Publish() error = %v", err)
	}
	nilSink.Close()
}
