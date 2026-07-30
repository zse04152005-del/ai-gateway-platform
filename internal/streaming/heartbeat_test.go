package streaming

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestResolveHeartbeatOptionsAllowsOnlyPlatformFrequencyAndClientOnOff(t *testing.T) {
	for _, preference := range []string{"", "on"} {
		options, err := ResolveHeartbeatOptions(15*time.Second, preference)
		if err != nil || !options.Enabled || options.Interval != 15*time.Second {
			t.Fatalf("ResolveHeartbeatOptions(%q) = %+v/%v", preference, options, err)
		}
	}
	disabled, err := ResolveHeartbeatOptions(15*time.Second, "off")
	if err != nil || disabled != (HeartbeatOptions{}) {
		t.Fatalf("disabled options = %+v/%v", disabled, err)
	}
	for _, test := range []struct {
		interval   time.Duration
		preference string
	}{
		{interval: minimumHeartbeatInterval - 1},
		{interval: maximumHeartbeatInterval + 1},
		{interval: time.Second, preference: "false"},
		{interval: time.Second, preference: "OFF"},
		{interval: time.Second, preference: " off"},
		{interval: time.Second, preference: "10ms"},
	} {
		if options, resolveErr := ResolveHeartbeatOptions(test.interval, test.preference); options != (HeartbeatOptions{}) ||
			!errors.Is(resolveErr, ErrHeartbeatConfiguration) {
			t.Fatalf("invalid ResolveHeartbeatOptions(%s, %q) = %+v/%v", test.interval, test.preference, options, resolveErr)
		}
	}
}

func TestHeartbeatDisabledAllocatesNoTickerAndTouchesNoPorts(t *testing.T) {
	created := 0
	heartbeat, err := newHeartbeat(HeartbeatOptions{}, time.Now, func(time.Duration) heartbeatTicker {
		created++
		return newFakeHeartbeatTicker()
	})
	if err != nil {
		t.Fatalf("newHeartbeat() error = %v", err)
	}
	sink := &fakeHeartbeatSink{}
	recorder := &fakeHeartbeatRecorder{}
	if runErr := heartbeat.Run(context.Background(), sink, recorder); runErr != nil {
		t.Fatalf("disabled Run() error = %v", runErr)
	}
	if created != 0 || len(sink.comments()) != 0 || recorder.count() != 0 {
		t.Fatalf("disabled side effects = ticker:%d comments:%v records:%d", created, sink.comments(), recorder.count())
	}
	snapshot := heartbeat.Snapshot()
	if snapshot.Enabled || snapshot.Running || !snapshot.Finished || snapshot.StartedAt == nil || snapshot.Sent != 0 {
		t.Fatalf("disabled snapshot = %+v", snapshot)
	}
	if err := heartbeat.Run(context.Background(), sink, recorder); !errors.Is(err, ErrHeartbeatState) {
		t.Fatalf("second disabled Run() error = %v", err)
	}
}

func TestHeartbeatEmitsFixedCommentsAndDoesNotAdvanceModelProgress(t *testing.T) {
	clock := &stepHeartbeatClock{now: time.Unix(100, 0).UTC()}
	ticker := newFakeHeartbeatTicker()
	heartbeat, err := newHeartbeat(
		HeartbeatOptions{Enabled: true, Interval: time.Second}, clock.Now,
		func(interval time.Duration) heartbeatTicker {
			if interval != time.Second {
				t.Fatalf("ticker interval = %s", interval)
			}
			return ticker
		},
	)
	if err != nil {
		t.Fatalf("newHeartbeat() error = %v", err)
	}
	controller := newTestTimeoutController(t, testTimeoutOptions())
	attachTestStream(t, controller, newTimeoutTestStream())
	sink := &fakeHeartbeatSink{wrote: make(chan struct{}, 4)}
	runCtx, cancel := context.WithCancelCause(context.Background())
	runResult := make(chan error, 1)
	go func() { runResult <- heartbeat.Run(runCtx, sink, controller) }()

	for range 2 {
		clock.Advance(time.Second)
		ticker.tick <- clock.Now()
		select {
		case <-sink.wrote:
		case <-time.After(time.Second):
			t.Fatal("heartbeat comment was not written")
		}
	}
	cancel(errors.New("test heartbeat stop"))
	select {
	case runErr := <-runResult:
		if !errors.Is(runErr, context.Canceled) {
			t.Fatalf("Run cancellation error = %v", runErr)
		}
	case <-time.After(time.Second):
		t.Fatal("heartbeat Run did not stop")
	}
	comments := sink.comments()
	if len(comments) != 2 || comments[0] != GatewayHeartbeatComment || comments[1] != GatewayHeartbeatComment {
		t.Fatalf("heartbeat comments = %#v", comments)
	}
	snapshot := heartbeat.Snapshot()
	if !snapshot.Enabled || snapshot.Running || !snapshot.Finished || snapshot.Sent != 2 ||
		snapshot.StartedAt == nil || snapshot.LastSentAt == nil || !ticker.stopped() {
		t.Fatalf("heartbeat snapshot/ticker = %+v/%v", snapshot, ticker.stopped())
	}
	timeoutSnapshot := controller.Snapshot()
	if timeoutSnapshot.GatewayHeartbeats != 2 || timeoutSnapshot.ModelOutputStarted ||
		timeoutSnapshot.FirstModelEventAt != nil || timeoutSnapshot.UpstreamEvents != 0 {
		t.Fatalf("heartbeat changed model progress = %+v", timeoutSnapshot)
	}
}

func TestHeartbeatStopsOnSinkOrRecorderFailure(t *testing.T) {
	sinkFailure := errors.New("safe sink failure")
	tests := []struct {
		name       string
		sink       *fakeHeartbeatSink
		recorder   *fakeHeartbeatRecorder
		want       error
		wantWrites int
		wantRecord int
	}{
		{name: "sink", sink: &fakeHeartbeatSink{err: sinkFailure}, recorder: &fakeHeartbeatRecorder{}, want: sinkFailure, wantWrites: 1},
		{name: "recorder", sink: &fakeHeartbeatSink{}, recorder: &fakeHeartbeatRecorder{err: errors.New("private recorder failure")}, want: ErrHeartbeatState, wantWrites: 1, wantRecord: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ticker := newFakeHeartbeatTicker()
			heartbeat, err := newHeartbeat(
				HeartbeatOptions{Enabled: true, Interval: time.Second}, time.Now,
				func(time.Duration) heartbeatTicker { return ticker },
			)
			if err != nil {
				t.Fatalf("newHeartbeat() error = %v", err)
			}
			result := make(chan error, 1)
			go func() { result <- heartbeat.Run(context.Background(), test.sink, test.recorder) }()
			ticker.tick <- time.Now()
			select {
			case runErr := <-result:
				if !errors.Is(runErr, test.want) {
					t.Fatalf("Run() error = %v, want %v", runErr, test.want)
				}
			case <-time.After(time.Second):
				t.Fatal("failed heartbeat Run did not stop")
			}
			if len(test.sink.comments()) != test.wantWrites || test.recorder.count() != test.wantRecord || !ticker.stopped() {
				t.Fatalf("failure side effects = comments:%v records:%d stopped:%v", test.sink.comments(), test.recorder.count(), ticker.stopped())
			}
		})
	}
}

func TestHeartbeatValidationAndNilContracts(t *testing.T) {
	invalid := []HeartbeatOptions{
		{Interval: time.Second},
		{Enabled: true},
		{Enabled: true, Interval: minimumHeartbeatInterval - 1},
		{Enabled: true, Interval: maximumHeartbeatInterval + 1},
	}
	for _, options := range invalid {
		if heartbeat, err := NewHeartbeat(options); heartbeat != nil || !errors.Is(err, ErrHeartbeatConfiguration) {
			t.Fatalf("NewHeartbeat(%+v) = %#v/%v", options, heartbeat, err)
		}
	}
	if heartbeat, err := newHeartbeat(HeartbeatOptions{}, nil, func(time.Duration) heartbeatTicker { return nil }); heartbeat != nil || !errors.Is(err, ErrHeartbeatConfiguration) {
		t.Fatalf("nil-clock newHeartbeat = %#v/%v", heartbeat, err)
	}
	if heartbeat, err := newHeartbeat(HeartbeatOptions{}, time.Now, nil); heartbeat != nil || !errors.Is(err, ErrHeartbeatConfiguration) {
		t.Fatalf("nil-factory newHeartbeat = %#v/%v", heartbeat, err)
	}
	zeroClock := func() time.Time { return time.Time{} }
	if heartbeat, err := newHeartbeat(HeartbeatOptions{}, zeroClock, func(time.Duration) heartbeatTicker { return nil }); heartbeat != nil || !errors.Is(err, ErrHeartbeatConfiguration) {
		t.Fatalf("zero-clock newHeartbeat = %#v/%v", heartbeat, err)
	}

	valid, err := NewHeartbeat(HeartbeatOptions{})
	if err != nil {
		t.Fatalf("NewHeartbeat(disabled) error = %v", err)
	}
	var nilSink *fakeHeartbeatSink
	var nilRecorder *fakeHeartbeatRecorder
	for _, runErr := range []error{
		(*Heartbeat)(nil).Run(context.Background(), &fakeHeartbeatSink{}, &fakeHeartbeatRecorder{}),
		valid.Run(nil, &fakeHeartbeatSink{}, &fakeHeartbeatRecorder{}), //nolint:staticcheck // explicit nil boundary
		valid.Run(context.Background(), nilSink, &fakeHeartbeatRecorder{}),
		valid.Run(context.Background(), &fakeHeartbeatSink{}, nilRecorder),
	} {
		if !errors.Is(runErr, ErrHeartbeatConfiguration) {
			t.Fatalf("invalid Run() error = %v", runErr)
		}
	}
	if (*Heartbeat)(nil).Enabled() || (*Heartbeat)(nil).Snapshot() != (HeartbeatSnapshot{}) {
		t.Fatal("nil heartbeat accessor contract failed")
	}

	missingTicker, err := newHeartbeat(
		HeartbeatOptions{Enabled: true, Interval: time.Second}, time.Now,
		func(time.Duration) heartbeatTicker { return nil },
	)
	if err != nil {
		t.Fatalf("newHeartbeat(missing ticker) error = %v", err)
	}
	if runErr := missingTicker.Run(context.Background(), &fakeHeartbeatSink{}, &fakeHeartbeatRecorder{}); !errors.Is(runErr, ErrHeartbeatConfiguration) {
		t.Fatalf("missing ticker Run() error = %v", runErr)
	}
}

type fakeHeartbeatTicker struct {
	tick     chan time.Time
	stopOnce sync.Once
	stopMu   sync.Mutex
	stop     bool
}

func newFakeHeartbeatTicker() *fakeHeartbeatTicker {
	return &fakeHeartbeatTicker{tick: make(chan time.Time, 4)}
}

func (ticker *fakeHeartbeatTicker) C() <-chan time.Time {
	return ticker.tick
}

func (ticker *fakeHeartbeatTicker) Stop() {
	ticker.stopOnce.Do(func() {
		ticker.stopMu.Lock()
		ticker.stop = true
		ticker.stopMu.Unlock()
	})
}

func (ticker *fakeHeartbeatTicker) stopped() bool {
	ticker.stopMu.Lock()
	defer ticker.stopMu.Unlock()
	return ticker.stop
}

type fakeHeartbeatSink struct {
	mu       sync.Mutex
	received []string
	err      error
	wrote    chan struct{}
}

func (sink *fakeHeartbeatSink) WriteComment(_ context.Context, comment string) error {
	sink.mu.Lock()
	sink.received = append(sink.received, comment)
	err := sink.err
	wrote := sink.wrote
	sink.mu.Unlock()
	if wrote != nil {
		wrote <- struct{}{}
	}
	return err
}

func (sink *fakeHeartbeatSink) comments() []string {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]string(nil), sink.received...)
}

type fakeHeartbeatRecorder struct {
	mu      sync.Mutex
	records int
	err     error
}

func (recorder *fakeHeartbeatRecorder) RecordGatewayHeartbeat() error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.records++
	return recorder.err
}

func (recorder *fakeHeartbeatRecorder) count() int {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.records
}

type stepHeartbeatClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *stepHeartbeatClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *stepHeartbeatClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(duration)
	clock.mu.Unlock()
}
