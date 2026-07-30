package streaming

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"time"
)

const (
	minimumHeartbeatInterval = 10 * time.Millisecond
	maximumHeartbeatInterval = 5 * time.Minute

	// HeartbeatPreferenceHeader is the bounded client on/off control. Clients
	// cannot choose an interval and therefore cannot amplify write frequency.
	HeartbeatPreferenceHeader = "X-AI-Gateway-SSE-Heartbeat"
	// GatewayHeartbeatComment is a content-free SSE comment, never a data event.
	GatewayHeartbeatComment = "gateway-heartbeat"
)

var (
	// ErrHeartbeatConfiguration means the scheduler input is invalid.
	ErrHeartbeatConfiguration = errors.New("SSE heartbeat configuration is invalid")
	// ErrHeartbeatState means a scheduler lifecycle or recorder invariant failed.
	ErrHeartbeatState = errors.New("SSE heartbeat lifecycle state is invalid")
)

// HeartbeatOptions defines one request-scoped gateway heartbeat policy.
type HeartbeatOptions struct {
	Enabled  bool
	Interval time.Duration
}

// HeartbeatSnapshot contains only liveness timing and counts.
type HeartbeatSnapshot struct {
	Enabled    bool
	Running    bool
	Finished   bool
	StartedAt  *time.Time
	LastSentAt *time.Time
	Sent       uint64
}

// HeartbeatSink is implemented by sse.Writer. Calls are safe alongside model
// event writes because that writer serializes complete events.
type HeartbeatSink interface {
	WriteComment(context.Context, string) error
}

// HeartbeatRecorder records a successful downstream heartbeat without
// advancing model-output or upstream-progress clocks.
type HeartbeatRecorder interface {
	RecordGatewayHeartbeat() error
}

type heartbeatTicker interface {
	C() <-chan time.Time
	Stop()
}

type heartbeatTickerFactory func(time.Duration) heartbeatTicker

// Heartbeat emits fixed, content-free SSE comments at a platform-controlled
// interval. Run blocks and creates no unowned goroutine.
type Heartbeat struct {
	options   HeartbeatOptions
	now       func() time.Time
	newTicker heartbeatTickerFactory

	mu         sync.Mutex
	running    bool
	finished   bool
	startedAt  *time.Time
	lastSentAt *time.Time
	sent       uint64
}

// ResolveHeartbeatOptions applies the client on/off preference to a trusted
// platform interval. Empty and "on" enable; "off" disables. Other values fail
// closed and clients can never provide a custom frequency.
func ResolveHeartbeatOptions(platformInterval time.Duration, preference string) (HeartbeatOptions, error) {
	if !validHeartbeatInterval(platformInterval) {
		return HeartbeatOptions{}, ErrHeartbeatConfiguration
	}
	switch preference {
	case "", "on":
		return HeartbeatOptions{Enabled: true, Interval: platformInterval}, nil
	case "off":
		return HeartbeatOptions{}, nil
	default:
		return HeartbeatOptions{}, ErrHeartbeatConfiguration
	}
}

// NewHeartbeat validates one resolved policy. Disabled heartbeats require a
// zero interval and allocate no ticker when Run is called.
func NewHeartbeat(options HeartbeatOptions) (*Heartbeat, error) {
	return newHeartbeat(options, time.Now, func(interval time.Duration) heartbeatTicker {
		return &systemHeartbeatTicker{ticker: time.NewTicker(interval)}
	})
}

func newHeartbeat(options HeartbeatOptions, now func() time.Time, factory heartbeatTickerFactory) (*Heartbeat, error) {
	if now == nil || factory == nil || (!options.Enabled && options.Interval != 0) ||
		(options.Enabled && !validHeartbeatInterval(options.Interval)) {
		return nil, ErrHeartbeatConfiguration
	}
	if now().IsZero() {
		return nil, ErrHeartbeatConfiguration
	}
	return &Heartbeat{options: options, now: now, newTicker: factory}, nil
}

// Enabled reports whether Run will create a ticker and emit comments.
func (heartbeat *Heartbeat) Enabled() bool {
	return heartbeat != nil && heartbeat.options.Enabled
}

// Run emits heartbeats until Context cancellation or the first sink/recorder
// failure. A disabled scheduler returns immediately without touching its ports.
func (heartbeat *Heartbeat) Run(ctx context.Context, sink HeartbeatSink, recorder HeartbeatRecorder) error {
	if heartbeat == nil || ctx == nil || isNilHeartbeatPort(sink) || isNilHeartbeatPort(recorder) {
		return ErrHeartbeatConfiguration
	}
	started := heartbeat.now().UTC()
	heartbeat.mu.Lock()
	if heartbeat.running || heartbeat.finished {
		heartbeat.mu.Unlock()
		return ErrHeartbeatState
	}
	heartbeat.running = heartbeat.options.Enabled
	heartbeat.finished = !heartbeat.options.Enabled
	heartbeat.startedAt = &started
	enabled := heartbeat.options.Enabled
	heartbeat.mu.Unlock()
	if !enabled {
		return nil
	}

	ticker := heartbeat.newTicker(heartbeat.options.Interval)
	if ticker == nil {
		heartbeat.finish()
		return ErrHeartbeatConfiguration
	}
	defer func() {
		ticker.Stop()
		heartbeat.finish()
	}()
	for {
		select {
		case <-ctx.Done():
			return contextCancellation(ctx)
		case <-ticker.C():
			if err := sink.WriteComment(ctx, GatewayHeartbeatComment); err != nil {
				return err
			}
			if err := recorder.RecordGatewayHeartbeat(); err != nil {
				return ErrHeartbeatState
			}
			heartbeat.recordSent()
		}
	}
}

// Snapshot returns a copy safe for metrics and lifecycle assertions.
func (heartbeat *Heartbeat) Snapshot() HeartbeatSnapshot {
	if heartbeat == nil {
		return HeartbeatSnapshot{}
	}
	heartbeat.mu.Lock()
	defer heartbeat.mu.Unlock()
	return HeartbeatSnapshot{
		Enabled: heartbeat.options.Enabled, Running: heartbeat.running, Finished: heartbeat.finished,
		StartedAt: cloneTime(heartbeat.startedAt), LastSentAt: cloneTime(heartbeat.lastSentAt), Sent: heartbeat.sent,
	}
}

func (heartbeat *Heartbeat) recordSent() {
	now := heartbeat.now().UTC()
	heartbeat.mu.Lock()
	heartbeat.sent++
	heartbeat.lastSentAt = &now
	heartbeat.mu.Unlock()
}

func (heartbeat *Heartbeat) finish() {
	heartbeat.mu.Lock()
	heartbeat.running = false
	heartbeat.finished = true
	heartbeat.mu.Unlock()
}

func validHeartbeatInterval(interval time.Duration) bool {
	return interval >= minimumHeartbeatInterval && interval <= maximumHeartbeatInterval
}

func isNilHeartbeatPort(port any) bool {
	if port == nil {
		return true
	}
	value := reflect.ValueOf(port)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	case reflect.Array, reflect.Bool, reflect.Complex128, reflect.Complex64, reflect.Float32, reflect.Float64,
		reflect.Int, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Int8, reflect.Invalid, reflect.String,
		reflect.Struct, reflect.Uint, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uint8, reflect.Uintptr,
		reflect.UnsafePointer:
		return false
	}
	return false
}

type systemHeartbeatTicker struct {
	ticker *time.Ticker
}

func (ticker *systemHeartbeatTicker) C() <-chan time.Time {
	return ticker.ticker.C
}

func (ticker *systemHeartbeatTicker) Stop() {
	ticker.ticker.Stop()
}
