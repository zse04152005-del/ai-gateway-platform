package keyauth

import (
	"testing"
	"time"
)

func TestMemoryCacheBoundsCopiesAndEviction(t *testing.T) {
	now := time.Date(2026, time.July, 30, 13, 0, 0, 0, time.UTC)
	cache, err := NewMemoryCache(time.Minute, 1, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewMemoryCache() error = %v", err)
	}
	first := Record{Prefix: "agw_live_00000001", SecretHash: []byte{1}}
	cache.Set(first.Prefix, first)
	first.SecretHash[0] = 9
	loaded, ok := cache.Get(first.Prefix)
	if !ok || loaded.SecretHash[0] != 1 {
		t.Fatalf("cached deep copy = %#v/%t", loaded.SecretHash, ok)
	}
	loaded.SecretHash[0] = 8
	reloaded, _ := cache.Get(first.Prefix)
	if reloaded.SecretHash[0] != 1 {
		t.Fatalf("Get() returned mutable cache storage: %#v", reloaded.SecretHash)
	}

	second := Record{Prefix: "agw_live_00000002", SecretHash: []byte{2}}
	cache.Set(second.Prefix, second)
	if _, ok := cache.Get(first.Prefix); ok {
		t.Fatal("oldest entry was not evicted at capacity")
	}
	if _, ok := cache.Get(second.Prefix); !ok {
		t.Fatal("newest entry missing after eviction")
	}
}
