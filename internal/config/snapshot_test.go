package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

var snapshotTime = time.Date(2026, time.July, 30, 12, 0, 0, 0, time.FixedZone("test", 8*60*60))

func TestNewSnapshotCanonicalizesAndProtectsDocument(t *testing.T) {
	original := []byte(`{ "models": [{"name":"gpt-test","max_tokens": 4096}], "secret_ref":"vault://provider/1" }`)
	snapshot, err := NewSnapshot(7, snapshotTime, original)
	if err != nil {
		t.Fatalf("NewSnapshot() error = %v", err)
	}
	original[2] = 'X'

	if snapshot.Version() != 7 {
		t.Fatalf("Version() = %d, want 7", snapshot.Version())
	}
	if snapshot.PublishedAt().Location() != time.UTC {
		t.Fatalf("PublishedAt() location = %v, want UTC", snapshot.PublishedAt().Location())
	}
	first := snapshot.JSON()
	first[0] = 'X'
	if !json.Valid(snapshot.JSON()) {
		t.Fatal("mutating JSON() result changed snapshot content")
	}
	if len(snapshot.Checksum()) != 64 {
		t.Fatalf("Checksum() length = %d, want 64", len(snapshot.Checksum()))
	}

	var decoded map[string]any
	if err := snapshot.Decode(&decoded); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if decoded["secret_ref"] != "vault://provider/1" {
		t.Fatalf("decoded secret_ref = %v", decoded["secret_ref"])
	}
	if err := snapshot.Decode(nil); err == nil {
		t.Fatal("Decode(nil) error = nil")
	}
}

func TestCanonicalChecksumIgnoresWhitespaceAndObjectKeyOrder(t *testing.T) {
	first, err := NewSnapshot(1, snapshotTime, []byte(`{"b":2,"a":1}`))
	if err != nil {
		t.Fatalf("first NewSnapshot() error = %v", err)
	}
	second, err := NewSnapshot(1, snapshotTime, []byte("{\n  \"a\": 1, \"b\": 2\n}"))
	if err != nil {
		t.Fatalf("second NewSnapshot() error = %v", err)
	}
	if first.Checksum() != second.Checksum() {
		t.Fatalf("checksums differ: %s != %s", first.Checksum(), second.Checksum())
	}
}

func TestNewSnapshotRejectsInvalidAndSensitiveDocuments(t *testing.T) {
	tests := []struct {
		name      string
		version   int64
		published time.Time
		document  string
		want      string
	}{
		{name: "version", version: 0, published: snapshotTime, document: `{}`, want: "version"},
		{name: "time", version: 1, document: `{}`, want: "published time"},
		{name: "syntax", version: 1, published: snapshotTime, document: `{`, want: "decode"},
		{name: "not object", version: 1, published: snapshotTime, document: `null`, want: "object"},
		{name: "trailing", version: 1, published: snapshotTime, document: `{} {}`, want: "exactly one"},
		{name: "secret nested", version: 1, published: snapshotTime, document: `{"providers":[{"api_key":"redacted"}]}`, want: "forbidden sensitive key"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewSnapshot(test.version, test.published, []byte(test.document))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewSnapshot() error = %v, want text %q", err, test.want)
			}
		})
	}
}

func TestSnapshotStorePublishesMonotonically(t *testing.T) {
	store := NewSnapshotStore()
	if _, ok := store.Current(); ok {
		t.Fatal("empty Current() returned a snapshot")
	}
	versionTwo := mustSnapshot(t, 2, `{"model":"v2"}`)
	if err := store.Publish(versionTwo); err != nil {
		t.Fatalf("Publish(v2) error = %v", err)
	}
	if err := store.Publish(versionTwo); err != nil {
		t.Fatalf("idempotent Publish(v2) error = %v", err)
	}
	if err := store.Publish(mustSnapshot(t, 1, `{"model":"v1"}`)); !errors.Is(err, ErrStaleSnapshot) {
		t.Fatalf("Publish(stale) error = %v", err)
	}
	if err := store.Publish(mustSnapshot(t, 2, `{"model":"conflict"}`)); !errors.Is(err, ErrSnapshotVersionConflict) {
		t.Fatalf("Publish(conflict) error = %v", err)
	}
	if err := store.Publish(Snapshot{}); err == nil {
		t.Fatal("Publish(zero) error = nil")
	}

	current, ok := store.Current()
	if !ok || current.Version() != 2 {
		t.Fatalf("Current() = version %d, %v", current.Version(), ok)
	}
}

func TestSnapshotStoreZeroValueIsUsable(t *testing.T) {
	var store SnapshotStore
	if err := store.Publish(mustSnapshot(t, 1, `{"model":"zero-value"}`)); err != nil {
		t.Fatalf("zero-value Publish() error = %v", err)
	}
	current, ok := store.Current()
	if !ok || current.Version() != 1 {
		t.Fatalf("zero-value Current() = version %d, %v", current.Version(), ok)
	}
}

func TestSnapshotStoreWaitsForNewerVersionAndCancels(t *testing.T) {
	store := NewSnapshotStore()
	if err := store.Publish(mustSnapshot(t, 1, `{"model":"v1"}`)); err != nil {
		t.Fatalf("Publish(v1) error = %v", err)
	}
	if current, err := store.WaitForVersion(context.Background(), 0); err != nil || current.Version() != 1 {
		t.Fatalf("immediate WaitForVersion() = version %d, error %v", current.Version(), err)
	}

	result := make(chan Snapshot, 1)
	waitErr := make(chan error, 1)
	go func() {
		snapshot, err := store.WaitForVersion(context.Background(), 1)
		result <- snapshot
		waitErr <- err
	}()
	if err := store.Publish(mustSnapshot(t, 2, `{"model":"v2"}`)); err != nil {
		t.Fatalf("Publish(v2) error = %v", err)
	}
	if got := <-result; got.Version() != 2 {
		t.Fatalf("waited version = %d, want 2", got.Version())
	}
	if err := <-waitErr; err != nil {
		t.Fatalf("WaitForVersion() error = %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.WaitForVersion(canceled, 2); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled WaitForVersion() error = %v", err)
	}
	var nilContext context.Context
	if _, err := store.WaitForVersion(nilContext, 0); err == nil {
		t.Fatal("WaitForVersion(nil) error = nil")
	}
	if _, err := store.WaitForVersion(context.Background(), -1); err == nil {
		t.Fatal("WaitForVersion(-1) error = nil")
	}
}

func TestSnapshotStoreConcurrentPublishEndsAtHighestVersion(t *testing.T) {
	store := NewSnapshotStore()
	const publishers = 100
	snapshots := make([]Snapshot, 0, publishers)
	for version := int64(1); version <= publishers; version++ {
		snapshots = append(snapshots, mustSnapshot(t, version, fmt.Sprintf(`{"version":%d}`, version)))
	}
	unexpected := make(chan error, publishers)
	var group sync.WaitGroup
	for _, snapshot := range snapshots {
		group.Add(1)
		go func(snapshot Snapshot) {
			defer group.Done()
			err := store.Publish(snapshot)
			if err != nil && !errors.Is(err, ErrStaleSnapshot) {
				unexpected <- err
			}
		}(snapshot)
	}
	group.Wait()
	close(unexpected)
	for err := range unexpected {
		t.Errorf("unexpected concurrent Publish() error = %v", err)
	}
	current, ok := store.Current()
	if !ok || current.Version() != publishers {
		t.Fatalf("final Current() = version %d, %v; want %d", current.Version(), ok, publishers)
	}
}

func mustSnapshot(t *testing.T, version int64, document string) Snapshot {
	t.Helper()
	snapshot, err := NewSnapshot(version, snapshotTime, []byte(document))
	if err != nil {
		t.Fatalf("NewSnapshot(%d) error = %v", version, err)
	}
	return snapshot
}
