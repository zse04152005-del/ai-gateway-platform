package keyauth

import (
	"errors"
	"sync"
	"time"
)

// Cache stores only positive, already scoped authentication records.
type Cache interface {
	Get(prefix string) (Record, bool)
	Set(prefix string, record Record)
	Invalidate(prefix string)
}

type cacheEntry struct {
	record    Record
	expiresAt time.Time
	inserted  uint64
}

// MemoryCache is a bounded TTL cache with explicit prefix invalidation.
// A zero TTL safely disables cross-request caching.
type MemoryCache struct {
	mutex      sync.Mutex
	entries    map[string]cacheEntry
	ttl        time.Duration
	maxEntries int
	clock      func() time.Time
	sequence   uint64
}

// NewMemoryCache creates a bounded cache.
func NewMemoryCache(ttl time.Duration, maxEntries int, clock func() time.Time) (*MemoryCache, error) {
	if ttl < 0 {
		return nil, errors.New("authentication cache TTL must not be negative")
	}
	if maxEntries <= 0 {
		return nil, errors.New("authentication cache capacity must be positive")
	}
	if clock == nil {
		return nil, errors.New("authentication cache clock must not be nil")
	}
	return &MemoryCache{entries: make(map[string]cacheEntry), ttl: ttl, maxEntries: maxEntries, clock: clock}, nil
}

// Get returns a deep copy and removes expired entries.
func (cache *MemoryCache) Get(prefix string) (Record, bool) {
	if cache == nil || cache.ttl == 0 {
		return Record{}, false
	}
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	entry, ok := cache.entries[prefix]
	if !ok {
		return Record{}, false
	}
	if !cache.clock().Before(entry.expiresAt) {
		delete(cache.entries, prefix)
		return Record{}, false
	}
	return cloneRecord(entry.record), true
}

// Set stores a deep copy and evicts the oldest entry at capacity.
func (cache *MemoryCache) Set(prefix string, record Record) {
	if cache == nil || cache.ttl == 0 {
		return
	}
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	cache.sequence++
	if _, replacing := cache.entries[prefix]; !replacing && len(cache.entries) >= cache.maxEntries {
		cache.evictOldest()
	}
	cache.entries[prefix] = cacheEntry{
		record: cloneRecord(record), expiresAt: cache.clock().Add(cache.ttl), inserted: cache.sequence,
	}
}

// Invalidate removes one safe-prefix entry immediately.
func (cache *MemoryCache) Invalidate(prefix string) {
	if cache == nil {
		return
	}
	cache.mutex.Lock()
	delete(cache.entries, prefix)
	cache.mutex.Unlock()
}

func (cache *MemoryCache) evictOldest() {
	var (
		oldestPrefix string
		oldestOrder  uint64
		found        bool
	)
	for prefix, entry := range cache.entries {
		if !found || entry.inserted < oldestOrder {
			oldestPrefix = prefix
			oldestOrder = entry.inserted
			found = true
		}
	}
	if found {
		delete(cache.entries, oldestPrefix)
	}
}
