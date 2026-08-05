// Package meteringworker provides the metering process lifecycle and event-bus bootstrap connection.
package meteringworker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"time"
)

// Session is an established event-bus consumer session that runs until
// cancellation and can be closed gracefully.
type Session interface {
	Run(context.Context) error
	Close(context.Context) error
}

// Connector establishes an event-bus bootstrap session using one of the configured brokers.
type Connector interface {
	Connect(context.Context, []string) (Session, error)
}

// Options defines the metering worker's process-level connection lifecycle.
type Options struct {
	Brokers         []string
	ConnectTimeout  time.Duration
	ShutdownTimeout time.Duration
	Connector       Connector
}

// Worker owns a single event-bus session for one process lifetime.
type Worker struct {
	brokers         []string
	connectTimeout  time.Duration
	shutdownTimeout time.Duration
	connector       Connector
	connected       atomic.Bool
	started         atomic.Bool
}

// New creates a single-use metering worker.
func New(options Options) (*Worker, error) {
	if options.Connector == nil {
		return nil, errors.New("event-bus connector must not be nil")
	}
	if options.ConnectTimeout <= 0 {
		return nil, errors.New("event-bus connect timeout must be greater than zero")
	}
	if options.ShutdownTimeout <= 0 {
		return nil, errors.New("worker shutdown timeout must be greater than zero")
	}
	if len(options.Brokers) == 0 {
		return nil, errors.New("event-bus brokers must not be empty")
	}

	brokers := make([]string, 0, len(options.Brokers))
	for _, raw := range options.Brokers {
		broker := strings.TrimSpace(raw)
		if _, _, err := net.SplitHostPort(broker); err != nil {
			return nil, fmt.Errorf("invalid event-bus broker %q: %w", broker, err)
		}
		brokers = append(brokers, broker)
	}

	return &Worker{
		brokers:         brokers,
		connectTimeout:  options.ConnectTimeout,
		shutdownTimeout: options.ShutdownTimeout,
		connector:       options.Connector,
	}, nil
}

// Connected reports whether the bootstrap event-bus session is established.
func (w *Worker) Connected() bool {
	return w.connected.Load()
}

// Run connects to the event bus, waits for cancellation, then closes the session
// within the configured shutdown timeout.
func (w *Worker) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("worker context must not be nil")
	}
	if !w.started.CompareAndSwap(false, true) {
		return errors.New("metering worker can only be run once")
	}

	connectCtx, cancelConnect := context.WithTimeout(ctx, w.connectTimeout)
	session, err := w.connector.Connect(connectCtx, append([]string(nil), w.brokers...))
	cancelConnect()
	if err != nil {
		return normalizeConnectError(ctx, err)
	}
	if session == nil {
		return errors.New("event-bus connector returned a nil session")
	}

	w.connected.Store(true)
	runErr := session.Run(ctx)
	w.connected.Store(false)

	closeErr := closeSession(session, w.shutdownTimeout)
	if ctx.Err() != nil && (runErr == nil || errors.Is(runErr, context.Canceled)) {
		runErr = nil
	}
	if runErr != nil && closeErr != nil {
		return errors.Join(
			fmt.Errorf("run metering worker event-bus session: %w", runErr),
			fmt.Errorf("close metering worker event-bus session: %w", closeErr),
		)
	}
	if runErr != nil {
		return fmt.Errorf("run metering worker event-bus session: %w", runErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close metering worker event-bus session: %w", closeErr)
	}
	return nil
}

func normalizeConnectError(ctx context.Context, connectErr error) error {
	select {
	case <-ctx.Done():
		return nil
	default:
		return fmt.Errorf("connect metering worker to event bus: %w", connectErr)
	}
}

func closeSession(session Session, timeout time.Duration) error {
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), timeout)
	defer cancelShutdown()
	result := make(chan error, 1)
	go func() {
		result <- session.Close(shutdownCtx)
	}()

	select {
	case err := <-result:
		return err
	case <-shutdownCtx.Done():
		return shutdownCtx.Err()
	}
}

// TCPConnector verifies bootstrap transport connectivity without claiming to
// implement Kafka consumption. Protocol consumers are added in later phases.
type TCPConnector struct {
	dialer net.Dialer
}

// NewTCPConnector creates a connector with TCP keepalive enabled.
func NewTCPConnector() *TCPConnector {
	return &TCPConnector{dialer: net.Dialer{KeepAlive: 30 * time.Second}}
}

// Connect tries brokers in order and returns the first successful TCP session.
func (c *TCPConnector) Connect(ctx context.Context, brokers []string) (Session, error) {
	if ctx == nil {
		return nil, errors.New("connect context must not be nil")
	}
	if len(brokers) == 0 {
		return nil, errors.New("event-bus brokers must not be empty")
	}

	attemptErrors := make([]error, 0, len(brokers))
	for _, broker := range brokers {
		connection, err := c.dialer.DialContext(ctx, "tcp", broker)
		if err == nil {
			return tcpSession{connection: connection}, nil
		}
		attemptErrors = append(attemptErrors, fmt.Errorf("broker %q: %w", broker, err))
		if ctx.Err() != nil {
			break
		}
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("event-bus bootstrap canceled: %w", ctx.Err())
	}
	return nil, fmt.Errorf("no event-bus broker reachable: %w", errors.Join(attemptErrors...))
}

type tcpSession struct {
	connection net.Conn
}

func (s tcpSession) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("event-bus session context must not be nil")
	}
	<-ctx.Done()
	return nil
}

func (s tcpSession) Close(_ context.Context) error {
	if err := s.connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("close event-bus TCP connection: %w", err)
	}
	return nil
}
