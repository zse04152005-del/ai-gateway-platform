package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrStaleSnapshot indicates that a publisher attempted to move configuration backwards.
	ErrStaleSnapshot = errors.New("snapshot version is stale")
	// ErrSnapshotVersionConflict indicates different content for the same version.
	ErrSnapshotVersionConflict = errors.New("snapshot version has conflicting content")
)

// Snapshot is a validated immutable business-configuration document.
// Callers only receive copies of its canonical JSON bytes.
type Snapshot struct {
	version       int64
	publishedAt   time.Time
	checksum      [sha256.Size]byte
	canonicalJSON []byte
}

// NewSnapshot validates and canonicalizes a versioned JSON object.
func NewSnapshot(version int64, publishedAt time.Time, document []byte) (Snapshot, error) {
	if version <= 0 {
		return Snapshot{}, errors.New("snapshot version must be greater than zero")
	}
	if publishedAt.IsZero() {
		return Snapshot{}, errors.New("snapshot published time must not be zero")
	}

	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	var decoded map[string]any
	if err := decoder.Decode(&decoded); err != nil {
		return Snapshot{}, fmt.Errorf("decode snapshot JSON: %w", err)
	}
	if decoded == nil {
		return Snapshot{}, errors.New("snapshot JSON must be an object")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Snapshot{}, errors.New("snapshot JSON must contain exactly one document")
		}
		return Snapshot{}, fmt.Errorf("decode trailing snapshot JSON: %w", err)
	}
	if err := rejectSensitiveKeys(decoded, "$"); err != nil {
		return Snapshot{}, err
	}

	canonical, err := json.Marshal(decoded)
	if err != nil {
		return Snapshot{}, fmt.Errorf("canonicalize snapshot JSON: %w", err)
	}
	return Snapshot{
		version:       version,
		publishedAt:   publishedAt.UTC(),
		checksum:      sha256.Sum256(canonical),
		canonicalJSON: append([]byte(nil), canonical...),
	}, nil
}

// Version returns the monotonically increasing configuration version.
func (s Snapshot) Version() int64 {
	return s.version
}

// PublishedAt returns the publisher timestamp normalized to UTC.
func (s Snapshot) PublishedAt() time.Time {
	return s.publishedAt
}

// Checksum returns the SHA-256 checksum of the canonical JSON document.
func (s Snapshot) Checksum() string {
	return hex.EncodeToString(s.checksum[:])
}

// JSON returns a copy of the canonical JSON document.
func (s Snapshot) JSON() []byte {
	return append([]byte(nil), s.canonicalJSON...)
}

// Decode decodes a copy of the canonical document into target.
func (s Snapshot) Decode(target any) error {
	if target == nil {
		return errors.New("snapshot decode target must not be nil")
	}
	if err := json.Unmarshal(s.canonicalJSON, target); err != nil {
		return fmt.Errorf("decode canonical snapshot: %w", err)
	}
	return nil
}

func (s Snapshot) clone() Snapshot {
	s.canonicalJSON = append([]byte(nil), s.canonicalJSON...)
	return s
}

func (s Snapshot) validate() error {
	if s.version <= 0 || s.publishedAt.IsZero() || len(s.canonicalJSON) == 0 {
		return errors.New("snapshot is not initialized")
	}
	if sha256.Sum256(s.canonicalJSON) != s.checksum {
		return errors.New("snapshot checksum does not match its document")
	}
	return nil
}

// SnapshotReader supplies lock-free current reads and notification-based waits.
type SnapshotReader interface {
	Current() (Snapshot, bool)
	WaitForVersion(context.Context, int64) (Snapshot, error)
}

// SnapshotPublisher atomically publishes validated, monotonic snapshots.
type SnapshotPublisher interface {
	Publish(Snapshot) error
}

// SnapshotStore is an in-process immutable snapshot cache. Reads are lock-free;
// publication is serialized to enforce version monotonicity and wake waiters.
type SnapshotStore struct {
	current atomic.Pointer[Snapshot]
	mu      sync.Mutex
	changed chan struct{}
}

// NewSnapshotStore creates an empty store.
func NewSnapshotStore() *SnapshotStore {
	return &SnapshotStore{changed: make(chan struct{})}
}

// Current returns an isolated copy of the most recently published snapshot.
func (s *SnapshotStore) Current() (Snapshot, bool) {
	current := s.current.Load()
	if current == nil {
		return Snapshot{}, false
	}
	return current.clone(), true
}

// Publish atomically advances the current version. Re-publishing identical
// content at the same version is idempotent; conflicting or stale versions fail.
func (s *SnapshotStore) Publish(snapshot Snapshot) error {
	if err := snapshot.validate(); err != nil {
		return fmt.Errorf("publish snapshot: %w", err)
	}
	candidate := snapshot.clone()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.initializeChangedLocked()
	current := s.current.Load()
	if current != nil {
		switch {
		case candidate.version < current.version:
			return fmt.Errorf("%w: current=%d candidate=%d", ErrStaleSnapshot, current.version, candidate.version)
		case candidate.version == current.version && candidate.checksum != current.checksum:
			return fmt.Errorf("%w: version=%d", ErrSnapshotVersionConflict, candidate.version)
		case candidate.version == current.version:
			return nil
		}
	}

	s.current.Store(&candidate)
	close(s.changed)
	s.changed = make(chan struct{})
	return nil
}

// WaitForVersion returns immediately when the current version is newer than afterVersion;
// otherwise it waits for a publication or context cancellation.
func (s *SnapshotStore) WaitForVersion(ctx context.Context, afterVersion int64) (Snapshot, error) {
	if ctx == nil {
		return Snapshot{}, errors.New("snapshot wait context must not be nil")
	}
	if afterVersion < 0 {
		return Snapshot{}, errors.New("snapshot wait version must not be negative")
	}

	for {
		if current, ok := s.Current(); ok && current.version > afterVersion {
			return current, nil
		}

		s.mu.Lock()
		s.initializeChangedLocked()
		current := s.current.Load()
		if current != nil && current.version > afterVersion {
			result := current.clone()
			s.mu.Unlock()
			return result, nil
		}
		changed := s.changed
		s.mu.Unlock()

		select {
		case <-changed:
		case <-ctx.Done():
			return Snapshot{}, fmt.Errorf("wait for snapshot after version %d: %w", afterVersion, ctx.Err())
		}
	}
}

func (s *SnapshotStore) initializeChangedLocked() {
	if s.changed == nil {
		s.changed = make(chan struct{})
	}
}

var forbiddenSnapshotKeys = map[string]struct{}{
	"apikey":        {},
	"authorization": {},
	"credential":    {},
	"credentials":   {},
	"password":      {},
	"providerkey":   {},
	"secret":        {},
	"secretvalue":   {},
	"token":         {},
}

func rejectSensitiveKeys(value any, path string) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalized := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(strings.TrimSpace(key)))
			if _, forbidden := forbiddenSnapshotKeys[normalized]; forbidden {
				return fmt.Errorf("snapshot contains forbidden sensitive key at %s.%s", path, key)
			}
			if err := rejectSensitiveKeys(child, path+"."+key); err != nil {
				return err
			}
		}
	case []any:
		for index, child := range typed {
			if err := rejectSensitiveKeys(child, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
	}
	return nil
}
