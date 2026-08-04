package meteringoutbox

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

type sinkFunc func(context.Context, string, []byte) error

func (function sinkFunc) Publish(ctx context.Context, key string, payload []byte) error {
	return function(ctx, key, payload)
}

func TestRelayConstructorBoundsAndRetryDelay(t *testing.T) {
	database := &sql.DB{}
	sink := sinkFunc(func(context.Context, string, []byte) error { return nil })
	now := time.Date(2026, time.August, 4, 1, 2, 3, 0, time.UTC)
	valid := DefaultOptions()
	valid.Now = func() time.Time { return now }
	valid.Random = bytes.NewReader(make([]byte, 32))
	if _, err := New(database, sink, valid); err != nil {
		t.Fatalf("New(valid) error = %v", err)
	}
	invalid := []Options{
		{},
		mutateOptions(valid, func(value *Options) { value.BatchSize = 0 }),
		mutateOptions(valid, func(value *Options) { value.BatchSize = maximumBatchSize + 1 }),
		mutateOptions(valid, func(value *Options) { value.PollInterval = 0 }),
		mutateOptions(valid, func(value *Options) { value.LeaseDuration = value.PublishTimeout }),
		mutateOptions(valid, func(value *Options) { value.PublishTimeout = 0 }),
		mutateOptions(valid, func(value *Options) { value.MinimumRetry = 0 }),
		mutateOptions(valid, func(value *Options) { value.MaximumRetry = value.MinimumRetry / 2 }),
		mutateOptions(valid, func(value *Options) { value.Now = nil }),
		mutateOptions(valid, func(value *Options) { value.Now = func() time.Time { return time.Time{} } }),
		mutateOptions(valid, func(value *Options) { value.Random = nil }),
	}
	for index, options := range invalid {
		if _, err := New(database, sink, options); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid options[%d] error = %v", index, err)
		}
	}
	if _, err := New(nil, sink, valid); !errors.Is(err, ErrInvalid) {
		t.Fatalf("New(nil database) error = %v", err)
	}
	if _, err := New(database, nil, valid); !errors.Is(err, ErrInvalid) {
		t.Fatalf("New(nil sink) error = %v", err)
	}
	relay, err := New(database, sink, valid)
	if err != nil {
		t.Fatalf("New(retry relay) error = %v", err)
	}
	for attempts, want := range map[int]time.Duration{
		0: time.Second, 1: 2 * time.Second, 2: 4 * time.Second,
		5: 32 * time.Second, 6: time.Minute, 100: time.Minute,
	} {
		if got := relay.retryDelay(attempts); got != want {
			t.Errorf("retryDelay(%d) = %s, want %s", attempts, got, want)
		}
	}
}

func TestRelayInvalidCallsUUIDAndSafeErrors(t *testing.T) {
	var nilRelay *Relay
	if err := nilRelay.Run(context.Background()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil Relay.Run() error = %v", err)
	}
	if _, err := nilRelay.RelayOnce(context.Background()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil Relay.RelayOnce() error = %v", err)
	}
	id, err := newUUID(bytes.NewReader(make([]byte, 16)))
	if err != nil || id != "00000000-0000-4000-8000-000000000000" {
		t.Fatalf("newUUID() = %q, %v", id, err)
	}
	private := errors.New("private postgres or broker detail")
	failure := newRelayError(ErrStoreUnavailable, private)
	if failure.Error() != ErrStoreUnavailable.Error() || strings.Contains(failure.Error(), "private") ||
		!errors.Is(failure, ErrStoreUnavailable) || !errors.Is(failure, private) {
		t.Fatalf("relay error safety/unwrap = %q/%v", failure, failure)
	}
	var nilFailure *relayError
	if nilFailure.Error() != "usage event relay failed" || nilFailure.Unwrap() != nil {
		t.Fatalf("nil relay error methods = %q/%#v", nilFailure.Error(), nilFailure.Unwrap())
	}
	if err := expectOneTransition(fakeResult(0), nil); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("zero transition error = %v", err)
	}
	if err := expectOneTransition(fakeResult(1), nil); err != nil {
		t.Fatalf("one transition error = %v", err)
	}
}

type fakeResult int64

func (fakeResult) LastInsertId() (int64, error)        { return 0, nil }
func (result fakeResult) RowsAffected() (int64, error) { return int64(result), nil }

func mutateOptions(input Options, mutate func(*Options)) Options {
	mutate(&input)
	return input
}
