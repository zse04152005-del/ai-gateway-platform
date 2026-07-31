//go:build integration

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/zse04152005-del/ai-gateway-platform/internal/limitpolicy"
	"github.com/zse04152005-del/ai-gateway-platform/internal/limits"
)

const (
	redisConcurrencyTenantID  = "76000000-0000-4000-8000-000000000001"
	redisConcurrencyProjectID = "76000000-0000-4000-8000-000000000002"
	redisConcurrencyKeyID     = "76000000-0000-4000-8000-000000000003"
	redisConcurrencyWorkers   = 64
	redisConcurrencyHard      = 20
	redisConcurrencySoft      = 15
	redisConcurrencyLease     = 700 * time.Millisecond
	redisConcurrencyRetention = 2 * time.Second
)

type redisConcurrencyResult struct {
	worker    int
	admission limits.RedisConcurrencyAdmission
	err       error
}

func TestRedisConcurrencyLeaseLifecycleAndProcessExpiry(t *testing.T) {
	client := openRedisIntegrationClient(t)
	evaluator, err := limits.NewGoRedisEvaluator(client)
	if err != nil {
		t.Fatalf("limits.NewGoRedisEvaluator() error = %v", err)
	}
	prefix := fmt.Sprintf("agw:test:p09t05:{concurrency}:%d", time.Now().UnixNano())
	limiter := newRedisConcurrencyIntegrationLimiter(t, evaluator, prefix)
	t.Cleanup(func() { cleanupRedisConcurrencyPrefix(t, client, prefix) })
	bindings := redisConcurrencyBindings(redisConcurrencySoft, redisConcurrencyHard)

	start := make(chan struct{})
	results := make(chan redisConcurrencyResult, redisConcurrencyWorkers)
	var workers sync.WaitGroup
	workers.Add(redisConcurrencyWorkers)
	for worker := range redisConcurrencyWorkers {
		go func() {
			defer workers.Done()
			<-start
			admission, acquireErr := limiter.Acquire(context.Background(), limits.RedisConcurrencyAcquireRequest{
				LeaseID: fmt.Sprintf("lease_%03d", worker), Bindings: bindings,
			})
			results <- redisConcurrencyResult{worker: worker, admission: admission, err: acquireErr}
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	allowed := make([]redisConcurrencyResult, 0, redisConcurrencyHard)
	denied := 0
	softAdmissions := 0
	for outcome := range results {
		if outcome.err != nil {
			t.Fatalf("Acquire(worker %d) error = %v", outcome.worker, outcome.err)
		}
		if outcome.admission.Allowed() {
			allowed = append(allowed, outcome)
			if outcome.admission.Counts[0].SoftExceeded {
				softAdmissions++
			}
			continue
		}
		denied++
		if outcome.admission.Rejection == nil ||
			outcome.admission.Rejection.Scope.Kind != limits.ScopePlatform ||
			outcome.admission.Rejection.Count != redisConcurrencyHard ||
			outcome.admission.Rejection.Hard != redisConcurrencyHard ||
			outcome.admission.Rejection.RetryAfter <= 0 ||
			outcome.admission.Rejection.RetryAfter > redisConcurrencyLease {
			t.Fatalf("denied admission = %+v", outcome.admission)
		}
	}
	if len(allowed) != redisConcurrencyHard || denied != redisConcurrencyWorkers-redisConcurrencyHard ||
		softAdmissions != redisConcurrencyHard-redisConcurrencySoft {
		t.Fatalf("outcomes = allowed:%d denied:%d soft:%d", len(allowed), denied, softAdmissions)
	}
	assertRedisConcurrencyCounts(t, client, prefix, redisConcurrencyHard)

	first := allowed[0]
	duplicate, err := limiter.Acquire(context.Background(), limits.RedisConcurrencyAcquireRequest{
		LeaseID: first.admission.Handle.LeaseID, Bindings: bindings,
	})
	if err != nil || !duplicate.Allowed() || !duplicate.Idempotent ||
		!duplicate.ExpiresAt.Equal(first.admission.ExpiresAt) {
		t.Fatalf("Acquire(duplicate) = %+v, %v", duplicate, err)
	}
	assertRedisConcurrencyCounts(t, client, prefix, redisConcurrencyHard)
	assertRedisConcurrencyTTL(t, client, prefix, first.admission.Handle.LeaseID)

	time.Sleep(250 * time.Millisecond)
	renewal, err := limiter.Renew(context.Background(), first.admission.Handle)
	if err != nil || !renewal.ExpiresAt.After(first.admission.ExpiresAt) ||
		len(renewal.Usage) != 4 || renewal.Usage[0].Count != redisConcurrencyHard {
		t.Fatalf("Renew() = %+v, %v", renewal, err)
	}

	released := releaseRedisConcurrencyHalf(t, limiter, allowed[1:11])
	if released != 10 {
		t.Fatalf("released = %d, want 10", released)
	}
	assertRedisConcurrencyCounts(t, client, prefix, 10)
	firstReleased := allowed[1].admission.Handle
	duplicateRelease, err := limiter.Release(context.Background(), firstReleased)
	if err != nil || !duplicateRelease.Idempotent || duplicateRelease.Expired {
		t.Fatalf("Release(duplicate) = %+v, %v", duplicateRelease, err)
	}

	wait := time.Until(renewal.ExpiresAt) + 120*time.Millisecond
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		<-timer.C
	}
	recovered, err := limiter.Acquire(context.Background(), limits.RedisConcurrencyAcquireRequest{
		LeaseID: "lease_after_process_exit", Bindings: bindings,
	})
	if err != nil || !recovered.Allowed() || recovered.Counts[0].Count != 1 {
		t.Fatalf("Acquire(after process expiry) = %+v, %v", recovered, err)
	}
	assertRedisConcurrencyCounts(t, client, prefix, 1)
	if _, err := limiter.Renew(context.Background(), first.admission.Handle); !errors.Is(err, limits.ErrRedisConcurrencyLeaseExpired) {
		t.Fatalf("Renew(expired process lease) error = %v", err)
	}
	release, err := limiter.Release(context.Background(), recovered.Handle)
	if err != nil || release.Expired || release.Idempotent {
		t.Fatalf("Release(recovered) = %+v, %v", release, err)
	}
	assertRedisConcurrencyCounts(t, client, prefix, 0)

	testRedisConcurrencyCorruptionIsAtomic(t, client, evaluator)
}

func releaseRedisConcurrencyHalf(
	t *testing.T,
	limiter *limits.RedisConcurrencyLimiter,
	leases []redisConcurrencyResult,
) int {
	t.Helper()
	results := make(chan error, len(leases))
	var workers sync.WaitGroup
	workers.Add(len(leases))
	for _, lease := range leases {
		go func() {
			defer workers.Done()
			// A fresh cleanup context is intentional: downstream cancellation must
			// not prevent release of its distributed slot.
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			release, err := limiter.Release(ctx, lease.admission.Handle)
			if err == nil && (release.Idempotent || release.Expired) {
				err = errors.New("first terminal release was not active")
			}
			results <- err
		}()
	}
	workers.Wait()
	close(results)
	released := 0
	for err := range results {
		if err != nil {
			t.Fatalf("Release(concurrent terminal path) error = %v", err)
		}
		released++
	}
	return released
}

func testRedisConcurrencyCorruptionIsAtomic(
	t *testing.T,
	client *redis.Client,
	evaluator *limits.GoRedisEvaluator,
) {
	t.Helper()
	prefix := fmt.Sprintf("agw:test:p09t05:corrupt:{concurrency}:%d", time.Now().UnixNano())
	limiter := newRedisConcurrencyIntegrationLimiter(t, evaluator, prefix)
	t.Cleanup(func() { cleanupRedisConcurrencyPrefix(t, client, prefix) })
	admission, err := limiter.Acquire(context.Background(), limits.RedisConcurrencyAcquireRequest{
		LeaseID: "lease_corrupt", Bindings: redisConcurrencyBindings(1, 2),
	})
	if err != nil || !admission.Allowed() {
		t.Fatalf("Acquire(corrupt setup) = %+v, %v", admission, err)
	}
	projectKey := prefix + ":project:" + redisConcurrencyTenantID + ":" + redisConcurrencyProjectID
	if err := client.ZRem(context.Background(), projectKey, admission.Handle.LeaseID).Err(); err != nil {
		t.Fatalf("ZRem(corrupt member) error = %v", err)
	}
	if _, err := limiter.Release(context.Background(), admission.Handle); !errors.Is(err, limits.ErrRedisConcurrencyProtocol) {
		t.Fatalf("Release(corrupt member) error = %v", err)
	}
	platformKey := prefix + ":platform"
	platform, err := client.ZCard(context.Background(), platformKey).Result()
	if err != nil || platform != 1 {
		t.Fatalf("platform after corrupt release = %d, %v", platform, err)
	}
}

func newRedisConcurrencyIntegrationLimiter(
	t *testing.T,
	evaluator *limits.GoRedisEvaluator,
	prefix string,
) *limits.RedisConcurrencyLimiter {
	t.Helper()
	options := limits.DefaultRedisConcurrencyOptions()
	options.KeyPrefix = prefix
	options.LeaseDuration = redisConcurrencyLease
	options.Retention = redisConcurrencyRetention
	limiter, err := limits.NewRedisConcurrencyLimiter(evaluator, options)
	if err != nil {
		t.Fatalf("limits.NewRedisConcurrencyLimiter() error = %v", err)
	}
	return limiter
}

func assertRedisConcurrencyCounts(t *testing.T, client *redis.Client, prefix string, want int64) {
	t.Helper()
	keys := []string{
		prefix + ":platform",
		prefix + ":tenant:" + redisConcurrencyTenantID,
		prefix + ":project:" + redisConcurrencyTenantID + ":" + redisConcurrencyProjectID,
		prefix + ":key:" + redisConcurrencyTenantID + ":" + redisConcurrencyProjectID + ":" + redisConcurrencyKeyID,
	}
	for _, key := range keys {
		count, err := client.ZCard(context.Background(), key).Result()
		if err != nil || count != want {
			t.Fatalf("ZCARD(%q) = %d, %v, want %d", key, count, err, want)
		}
	}
}

func assertRedisConcurrencyTTL(t *testing.T, client *redis.Client, prefix, leaseID string) {
	t.Helper()
	for _, key := range []string{prefix + ":platform", prefix + ":lease:" + leaseID} {
		ttl, err := client.PTTL(context.Background(), key).Result()
		if err != nil || ttl <= redisConcurrencyLease || ttl > redisConcurrencyLease+redisConcurrencyRetention {
			t.Fatalf("PTTL(%q) = %v, %v", key, ttl, err)
		}
	}
}

func cleanupRedisConcurrencyPrefix(t *testing.T, client *redis.Client, prefix string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var cursor uint64
	for {
		keys, next, err := client.Scan(ctx, cursor, prefix+":*", 100).Result()
		if err != nil {
			t.Errorf("scan Redis concurrency keys: %v", err)
			return
		}
		if len(keys) > 0 {
			if err := client.Del(ctx, keys...).Err(); err != nil {
				t.Errorf("cleanup Redis concurrency keys: %v", err)
				return
			}
		}
		cursor = next
		if cursor == 0 {
			return
		}
	}
}

func redisConcurrencyBindings(soft, hard uint64) []limits.Binding {
	policy := limitpolicy.Effective{
		RPM:         redisRPMThreshold(1_000, 2_000),
		TPM:         redisRPMThreshold(100_000, 200_000),
		Concurrency: redisRPMThreshold(soft, hard),
	}
	scopes := []limits.Scope{
		{Kind: limits.ScopePlatform},
		{Kind: limits.ScopeTenant, TenantID: redisConcurrencyTenantID},
		{Kind: limits.ScopeProject, TenantID: redisConcurrencyTenantID, ProjectID: redisConcurrencyProjectID},
		{
			Kind: limits.ScopeKey, TenantID: redisConcurrencyTenantID,
			ProjectID: redisConcurrencyProjectID, KeyID: redisConcurrencyKeyID,
		},
	}
	bindings := make([]limits.Binding, 0, len(scopes))
	for _, scope := range scopes {
		bindings = append(bindings, limits.Binding{Scope: scope, Policy: policy})
	}
	return bindings
}
