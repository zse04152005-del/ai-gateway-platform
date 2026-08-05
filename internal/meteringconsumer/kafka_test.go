package meteringconsumer

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

type handlerFunc func(context.Context, string, []byte) (Result, error)

func (function handlerFunc) Process(ctx context.Context, key string, payload []byte) (Result, error) {
	return function(ctx, key, payload)
}

func TestKafkaSessionCancellationClosesBootstrapConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- connection
		}
	}()
	handler := handlerFunc(func(context.Context, string, []byte) (Result, error) {
		return Result{}, nil
	})
	connector, err := NewKafkaConnector(handler, KafkaOptions{ConsumerGroup: DefaultConsumerGroup})
	if err != nil {
		t.Fatalf("NewKafkaConnector() error = %v", err)
	}
	session, err := connector.Connect(context.Background(), []string{listener.Addr().String()})
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- session.Run(ctx) }()
	var connection net.Conn
	select {
	case connection = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Kafka bootstrap connection")
	}
	t.Cleanup(func() { _ = connection.Close() })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Session.Run() cancellation error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Kafka session did not stop after cancellation")
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("Session.Close() error = %v", err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("SetReadDeadline() error = %v", err)
	}
	buffer := make([]byte, 1024)
	for {
		if _, err := connection.Read(buffer); err != nil {
			var netError net.Error
			if errors.As(err, &netError) && netError.Timeout() {
				t.Fatal("Kafka bootstrap connection remained open after Close")
			}
			break
		}
	}
}

func TestKafkaConnectorValidatesGroupBrokersAndSession(t *testing.T) {
	handler := handlerFunc(func(context.Context, string, []byte) (Result, error) {
		return Result{}, nil
	})
	if _, err := NewKafkaConnector(nil, KafkaOptions{ConsumerGroup: DefaultConsumerGroup}); !errors.Is(err, ErrKafkaConsumerInvalid) {
		t.Fatalf("NewKafkaConnector(nil) error = %v", err)
	}
	if _, err := NewKafkaConnector(handler, KafkaOptions{ConsumerGroup: "Bad Group"}); !errors.Is(err, ErrKafkaConsumerInvalid) {
		t.Fatalf("NewKafkaConnector(bad group) error = %v", err)
	}
	connector, err := NewKafkaConnector(handler, KafkaOptions{ConsumerGroup: DefaultConsumerGroup})
	if err != nil {
		t.Fatalf("NewKafkaConnector(valid) error = %v", err)
	}
	var nilContext context.Context
	if _, err := connector.Connect(nilContext, []string{"localhost:19092"}); !errors.Is(err, ErrKafkaConsumerInvalid) {
		t.Fatalf("Connect(nil context) error = %v", err)
	}
	for _, brokers := range [][]string{nil, {"missing-port"}, {" localhost:19092"}} {
		if _, err := connector.Connect(context.Background(), brokers); !errors.Is(err, ErrKafkaConsumerInvalid) {
			t.Errorf("Connect(%v) error = %v", brokers, err)
		}
	}
	var nilSession *kafkaSession
	if err := nilSession.Run(context.Background()); !errors.Is(err, ErrKafkaConsumerInvalid) {
		t.Fatalf("nil kafkaSession.Run() error = %v", err)
	}
	if err := nilSession.Close(context.Background()); err != nil {
		t.Fatalf("nil kafkaSession.Close() error = %v", err)
	}
}
