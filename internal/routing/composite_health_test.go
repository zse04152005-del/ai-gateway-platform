package routing

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestCompositeHealthRequiresEverySignalInOrder(t *testing.T) {
	t.Parallel()
	events := make([]string, 0, 2)
	first := compositeHealthReader{name: "passive", healthy: true, events: &events}
	second := compositeHealthReader{name: "active", healthy: true, events: &events}
	combined, err := NewCompositeHealth(first, second)
	if err != nil {
		t.Fatalf("NewCompositeHealth() error = %v", err)
	}
	if healthy, readErr := combined.Healthy(context.Background(), routeUUID(9, 1)); readErr != nil || !healthy {
		t.Fatalf("Healthy() = %v, %v", healthy, readErr)
	}
	if !reflect.DeepEqual(events, []string{"passive", "active"}) {
		t.Fatalf("events = %#v", events)
	}

	events = events[:0]
	first.healthy = false
	combined, _ = NewCompositeHealth(first, second)
	if healthy, readErr := combined.Healthy(context.Background(), routeUUID(9, 1)); readErr != nil || healthy {
		t.Fatalf("unhealthy result = %v, %v", healthy, readErr)
	}
	if !reflect.DeepEqual(events, []string{"passive"}) {
		t.Fatalf("short-circuit events = %#v", events)
	}
}

func TestCompositeHealthFailsClosedOnReaderErrorAndInvalidConstruction(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("synthetic active health read failure")
	combined, err := NewCompositeHealth(
		compositeHealthReader{healthy: true},
		compositeHealthReader{err: wantErr},
	)
	if err != nil {
		t.Fatalf("NewCompositeHealth() error = %v", err)
	}
	if healthy, readErr := combined.Healthy(context.Background(), routeUUID(9, 2)); healthy || !errors.Is(readErr, wantErr) {
		t.Fatalf("Healthy() = %v, %v", healthy, readErr)
	}
	if _, err := NewCompositeHealth(compositeHealthReader{healthy: true}); err == nil {
		t.Fatal("NewCompositeHealth(one reader) error = nil")
	}
	if _, err := NewCompositeHealth(compositeHealthReader{healthy: true}, nil); err == nil {
		t.Fatal("NewCompositeHealth(nil reader) error = nil")
	}
	if _, err := (*CompositeHealth)(nil).Healthy(context.Background(), routeUUID(9, 2)); err == nil {
		t.Fatal("nil CompositeHealth error = nil")
	}
}

type compositeHealthReader struct {
	name    string
	healthy bool
	err     error
	events  *[]string
}

func (reader compositeHealthReader) Healthy(context.Context, string) (bool, error) {
	if reader.events != nil {
		*reader.events = append(*reader.events, reader.name)
	}
	return reader.healthy, reader.err
}
