package meteringworker

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"testing"
	"time"
)

const workerTestTimeout = 3 * time.Second

type connectorFunc func(context.Context, []string) (Session, error)

func (function connectorFunc) Connect(ctx context.Context, brokers []string) (Session, error) {
	return function(ctx, brokers)
}

type sessionFunc func(context.Context) error

func (function sessionFunc) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (function sessionFunc) Close(ctx context.Context) error {
	return function(ctx)
}

type failingRunSession struct {
	runError error
}

func (session failingRunSession) Run(context.Context) error { return session.runError }
func (failingRunSession) Close(context.Context) error       { return nil }

func TestNewValidatesOptions(t *testing.T) {
	valid := Options{
		Brokers:         []string{"localhost:19092"},
		ConnectTimeout:  time.Second,
		ShutdownTimeout: time.Second,
		Connector: connectorFunc(func(context.Context, []string) (Session, error) {
			return sessionFunc(func(context.Context) error { return nil }), nil
		}),
	}
	tests := []struct {
		name   string
		mutate func(*Options)
		want   string
	}{
		{name: "connector", mutate: func(options *Options) { options.Connector = nil }, want: "connector"},
		{name: "connect timeout", mutate: func(options *Options) { options.ConnectTimeout = 0 }, want: "connect timeout"},
		{name: "shutdown timeout", mutate: func(options *Options) { options.ShutdownTimeout = 0 }, want: "shutdown timeout"},
		{name: "brokers", mutate: func(options *Options) { options.Brokers = nil }, want: "brokers"},
		{name: "broker address", mutate: func(options *Options) { options.Brokers = []string{"missing-port"} }, want: "invalid event-bus broker"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := valid
			test.mutate(&options)
			_, err := New(options)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("New() error = %v, want text %q", err, test.want)
			}
		})
	}
}

func TestWorkerConnectsWaitsAndCloses(t *testing.T) {
	connected := make(chan struct{})
	closed := make(chan struct{})
	connector := connectorFunc(func(_ context.Context, brokers []string) (Session, error) {
		if !slices.Equal(brokers, []string{"broker-one:9092", "broker-two:9092"}) {
			t.Fatalf("brokers = %v", brokers)
		}
		close(connected)
		return sessionFunc(func(ctx context.Context) error {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("session close context has no deadline")
			}
			close(closed)
			return nil
		}), nil
	})
	worker := newTestWorker(t, connector)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	waitForWorkerSignal(t, connected, "event-bus connection")
	waitForWorkerCondition(t, worker.Connected, "connected state")

	cancel()
	if err := waitForWorkerResult(t, done); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	waitForWorkerSignal(t, closed, "event-bus session close")
	if worker.Connected() {
		t.Fatal("Connected() = true after shutdown")
	}
}

func TestWorkerReturnsConnectionAndCloseFailures(t *testing.T) {
	connectErr := errors.New("synthetic connect failure")
	worker := newTestWorker(t, connectorFunc(func(context.Context, []string) (Session, error) {
		return nil, connectErr
	}))
	if err := worker.Run(context.Background()); !errors.Is(err, connectErr) {
		t.Fatalf("Run() error = %v, want connect failure", err)
	}

	closeErr := errors.New("synthetic close failure")
	worker = newTestWorker(t, connectorFunc(func(context.Context, []string) (Session, error) {
		return sessionFunc(func(context.Context) error { return closeErr }), nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := worker.Run(ctx); !errors.Is(err, closeErr) {
		t.Fatalf("Run() error = %v, want close failure", err)
	}

	runErr := errors.New("synthetic consume failure")
	worker = newTestWorker(t, connectorFunc(func(context.Context, []string) (Session, error) {
		return failingRunSession{runError: runErr}, nil
	}))
	if err := worker.Run(context.Background()); !errors.Is(err, runErr) {
		t.Fatalf("Run() error = %v, want consume failure", err)
	}
}

func TestWorkerEnforcesShutdownDeadlineWhenSessionIgnoresContext(t *testing.T) {
	closeStarted := make(chan struct{})
	releaseClose := make(chan struct{})
	worker, err := New(Options{
		Brokers:         []string{"broker-one:9092"},
		ConnectTimeout:  time.Second,
		ShutdownTimeout: 25 * time.Millisecond,
		Connector: connectorFunc(func(context.Context, []string) (Session, error) {
			return sessionFunc(func(context.Context) error {
				close(closeStarted)
				<-releaseClose
				return nil
			}), nil
		}),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	startedAt := time.Now()
	runErr := worker.Run(ctx)
	elapsed := time.Since(startedAt)
	waitForWorkerSignal(t, closeStarted, "session close to start")
	close(releaseClose)
	if !errors.Is(runErr, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want deadline exceeded", runErr)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Run() shutdown elapsed = %v, want finite deadline", elapsed)
	}
}

func TestWorkerTreatsStartupCancellationAsCleanStop(t *testing.T) {
	connector := connectorFunc(func(ctx context.Context, _ []string) (Session, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	worker := newTestWorker(t, connector)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := worker.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
}

func TestWorkerRejectsNilContextNilSessionAndSecondRun(t *testing.T) {
	connector := connectorFunc(func(context.Context, []string) (Session, error) { return nil, nil })
	worker := newTestWorker(t, connector)
	var nilContext context.Context
	if err := worker.Run(nilContext); err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("Run(nil) error = %v", err)
	}
	if err := worker.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "nil session") {
		t.Fatalf("Run() error = %v, want nil session", err)
	}
	if err := worker.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "only be run once") {
		t.Fatalf("second Run() error = %v", err)
	}
}

func TestTCPConnectorFallsBackToReachableBroker(t *testing.T) {
	closedListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("closed net.Listen() error = %v", err)
	}
	unreachable := closedListener.Addr().String()
	if err := closedListener.Close(); err != nil {
		t.Fatalf("closedListener.Close() error = %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("listener.Close() error = %v", err)
		}
	})
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- connection
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), workerTestTimeout)
	defer cancel()
	session, err := NewTCPConnector().Connect(ctx, []string{unreachable, listener.Addr().String()})
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	serverConnection := waitForConnection(t, accepted)
	t.Cleanup(func() {
		if err := serverConnection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("server connection close error = %v", err)
		}
	})
	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("session.Close() error = %v", err)
	}
}

func newTestWorker(t *testing.T, connector Connector) *Worker {
	t.Helper()
	worker, err := New(Options{
		Brokers:         []string{"broker-one:9092", "broker-two:9092"},
		ConnectTimeout:  time.Second,
		ShutdownTimeout: time.Second,
		Connector:       connector,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return worker
}

func waitForWorkerSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(workerTestTimeout):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func waitForWorkerCondition(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(workerTestTimeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func waitForWorkerResult(t *testing.T, results <-chan error) error {
	t.Helper()
	select {
	case err := <-results:
		return err
	case <-time.After(workerTestTimeout):
		t.Fatal("timed out waiting for worker result")
		return nil
	}
}

func waitForConnection(t *testing.T, connections <-chan net.Conn) net.Conn {
	t.Helper()
	select {
	case connection := <-connections:
		return connection
	case <-time.After(workerTestTimeout):
		t.Fatal("timed out waiting for TCP connection")
		return nil
	}
}
