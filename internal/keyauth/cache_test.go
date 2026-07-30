package keyauth

import (
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/virtualkey"
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

func TestMemoryCacheBindsScopedRecordToItsCredentialPrefix(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.July, 30, 13, 0, 0, 0, time.UTC)
	cache, err := NewMemoryCache(time.Minute, 2, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewMemoryCache() error = %v", err)
	}
	models := []string{"tenant-one-chat"}
	rpm := int64(100)
	record := Record{
		ID:            "37000000-0000-4000-8000-000000000001",
		TenantID:      "17000000-0000-4000-8000-000000000001",
		ProjectID:     "27000000-0000-4000-8000-000000000001",
		Prefix:        "agw_live_tenant01",
		SecretHash:    []byte{1, 2, 3},
		AllowedModels: &models,
		Limits:        &virtualkey.Limits{RPM: &rpm},
	}

	cache.Set("agw_live_tenant02", record)
	if _, ok := cache.Get("agw_live_tenant02"); ok {
		t.Fatal("cache accepted a scoped record under a different credential prefix")
	}

	cache.Set(record.Prefix, record)
	loaded, ok := cache.Get(record.Prefix)
	if !ok {
		t.Fatal("scoped record missing from cache")
	}
	loaded.TenantID = "17000000-0000-4000-8000-000000000002"
	loaded.ProjectID = "27000000-0000-4000-8000-000000000002"
	(*loaded.AllowedModels)[0] = "tenant-two-chat"
	*loaded.Limits.RPM = 999

	reloaded, ok := cache.Get(record.Prefix)
	if !ok {
		t.Fatal("scoped record missing after caller mutation")
	}
	if reloaded.TenantID != record.TenantID || reloaded.ProjectID != record.ProjectID {
		t.Fatalf("cached scope changed to tenant/project %s/%s", reloaded.TenantID, reloaded.ProjectID)
	}
	if got := (*reloaded.AllowedModels)[0]; got != "tenant-one-chat" {
		t.Fatalf("cached model scope = %q, want tenant-one-chat", got)
	}
	if got := *reloaded.Limits.RPM; got != 100 {
		t.Fatalf("cached RPM = %d, want 100", got)
	}
}
