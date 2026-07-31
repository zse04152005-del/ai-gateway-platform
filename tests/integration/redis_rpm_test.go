//go:build integration

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/zse04152005-del/ai-gateway-platform/internal/limitpolicy"
	"github.com/zse04152005-del/ai-gateway-platform/internal/limits"
)

const (
	redisRPMTenantID      = "74000000-0000-4000-8000-000000000001"
	redisRPMProjectID     = "74000000-0000-4000-8000-000000000002"
	redisRPMKeyID         = "74000000-0000-4000-8000-000000000003"
	redisRPMWorkers       = 128
	redisRPMHardLimit     = 50
	redisRPMSoftLimit     = 40
	redisRPMTestRetention = 10 * time.Second
)

func TestRedisRPMAtomicConcurrencyAndTTL(t *testing.T) {
	client := openRedisIntegrationClient(t)
	waitForRedisRPMWindow(t, client)
	evaluator, err := limits.NewGoRedisEvaluator(client)
	if err != nil {
		t.Fatalf("limits.NewGoRedisEvaluator() error = %v", err)
	}
	prefix := fmt.Sprintf("agw:test:p09t03:{rpm}:%d", time.Now().UnixNano())
	options := limits.DefaultRedisRPMOptions()
	options.KeyPrefix = prefix
	options.Retention = redisRPMTestRetention
	limiter, err := limits.NewRedisRPMLimiter(evaluator, options, time.Now)
	if err != nil {
		t.Fatalf("limits.NewRedisRPMLimiter() error = %v", err)
	}
	keys := make(map[string]struct{})
	var keysMu sync.Mutex
	t.Cleanup(func() {
		keysMu.Lock()
		defer keysMu.Unlock()
		cleanupRedisRPMKeys(t, client, keys)
	})

	bindings := redisRPMBindings(redisRPMSoftLimit, redisRPMHardLimit)
	type result struct {
		admission limits.RedisRPMAdmission
		err       error
	}
	start := make(chan struct{})
	results := make(chan result, redisRPMWorkers)
	var workers sync.WaitGroup
	workers.Add(redisRPMWorkers)
	for range redisRPMWorkers {
		go func() {
			defer workers.Done()
			<-start
			admission, acquireErr := limiter.AcquireRPM(context.Background(), bindings)
			results <- result{admission: admission, err: acquireErr}
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	allowed := 0
	denied := 0
	softPlatformAdmissions := 0
	var windowKey string
	for outcome := range results {
		if outcome.err != nil {
			t.Fatalf("AcquireRPM() error = %v", outcome.err)
		}
		if outcome.admission.WindowKey == "" {
			t.Fatal("AcquireRPM() returned an empty WindowKey")
		}
		keysMu.Lock()
		keys[outcome.admission.WindowKey] = struct{}{}
		keysMu.Unlock()
		if windowKey == "" {
			windowKey = outcome.admission.WindowKey
		}
		if outcome.admission.WindowKey != windowKey {
			t.Fatalf("concurrent calls crossed minute windows: %q / %q", windowKey, outcome.admission.WindowKey)
		}
		if outcome.admission.Allowed() {
			allowed++
			if len(outcome.admission.Counts) != 4 {
				t.Fatalf("allowed counts = %+v", outcome.admission.Counts)
			}
			if outcome.admission.Counts[0].SoftExceeded {
				softPlatformAdmissions++
			}
			continue
		}
		denied++
		if outcome.admission.Rejection == nil ||
			outcome.admission.Rejection.Scope.Kind != limits.ScopePlatform ||
			outcome.admission.Rejection.Count != redisRPMHardLimit ||
			outcome.admission.Rejection.Hard != redisRPMHardLimit {
			t.Fatalf("denied admission = %+v", outcome.admission)
		}
	}
	if allowed != redisRPMHardLimit || denied != redisRPMWorkers-redisRPMHardLimit ||
		softPlatformAdmissions != redisRPMHardLimit-redisRPMSoftLimit {
		t.Fatalf(
			"outcomes = allowed:%d denied:%d soft:%d",
			allowed,
			denied,
			softPlatformAdmissions,
		)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fields, err := client.HGetAll(ctx, windowKey).Result()
	if err != nil {
		t.Fatalf("HGetAll(%q) error = %v", windowKey, err)
	}
	if len(fields) != 4 {
		t.Fatalf("field count = %d, want 4: %#v", len(fields), fields)
	}
	for field, value := range fields {
		if value != strconv.Itoa(redisRPMHardLimit) {
			t.Fatalf("field %q count = %q, want %d", field, value, redisRPMHardLimit)
		}
	}
	ttl, err := client.PTTL(ctx, windowKey).Result()
	if err != nil {
		t.Fatalf("PTTL(%q) error = %v", windowKey, err)
	}
	if ttl <= 0 || ttl > time.Minute+redisRPMTestRetention {
		t.Fatalf("PTTL(%q) = %v", windowKey, ttl)
	}

	tenantLimited := redisRPMBindings(redisRPMSoftLimit, redisRPMHardLimit)
	tenantLimited[0].Policy = redisRPMEffective(redisRPMSoftLimit, redisRPMHardLimit*2)
	deniedAtTenant, err := limiter.AcquireRPM(ctx, tenantLimited)
	if err != nil || deniedAtTenant.Rejection == nil ||
		deniedAtTenant.Rejection.Scope.Kind != limits.ScopeTenant {
		t.Fatalf("tenant hard denial = %+v, %v", deniedAtTenant, err)
	}
	platformCount, err := client.HGet(ctx, windowKey, "platform").Int64()
	if err != nil || platformCount != redisRPMHardLimit {
		t.Fatalf("platform count after tenant denial = %d, %v", platformCount, err)
	}

	testRedisRPMServerClockCorrection(t, client, evaluator, keys, &keysMu)
	testRedisRPMCorruptCounterFailsClosed(t, client, evaluator, keys, &keysMu)
}

func testRedisRPMServerClockCorrection(
	t *testing.T,
	client *redis.Client,
	evaluator *limits.GoRedisEvaluator,
	keys map[string]struct{},
	keysMu *sync.Mutex,
) {
	t.Helper()
	options := limits.DefaultRedisRPMOptions()
	options.KeyPrefix = fmt.Sprintf("agw:test:p09t03:skew:{rpm}:%d", time.Now().UnixNano())
	options.Retention = redisRPMTestRetention
	skewedClock := func() time.Time { return time.Now().Add(10 * time.Minute) }
	limiter, err := limits.NewRedisRPMLimiter(evaluator, options, skewedClock)
	if err != nil {
		t.Fatalf("limits.NewRedisRPMLimiter(skewed) error = %v", err)
	}
	admission, err := limiter.AcquireRPM(context.Background(), redisRPMBindings(1, 2))
	if err != nil || !admission.Allowed() {
		t.Fatalf("AcquireRPM(skewed clock) = %+v, %v", admission, err)
	}
	keysMu.Lock()
	keys[admission.WindowKey] = struct{}{}
	keysMu.Unlock()
	if admission.Window != admission.ServerTime.Unix()/60 ||
		admission.ResetAt.Sub(admission.ServerTime) <= 0 ||
		admission.ResetAt.Sub(admission.ServerTime) > time.Minute {
		t.Fatalf("server-clock admission = %+v", admission)
	}
	ttl, err := client.PTTL(context.Background(), admission.WindowKey).Result()
	if err != nil || ttl <= 0 || ttl > time.Minute+redisRPMTestRetention {
		t.Fatalf("skewed-clock PTTL = %v, %v", ttl, err)
	}
}

func testRedisRPMCorruptCounterFailsClosed(
	t *testing.T,
	client *redis.Client,
	evaluator *limits.GoRedisEvaluator,
	keys map[string]struct{},
	keysMu *sync.Mutex,
) {
	t.Helper()
	options := limits.DefaultRedisRPMOptions()
	options.KeyPrefix = fmt.Sprintf("agw:test:p09t03:corrupt:{rpm}:%d", time.Now().UnixNano())
	options.Retention = redisRPMTestRetention
	limiter, err := limits.NewRedisRPMLimiter(evaluator, options, time.Now)
	if err != nil {
		t.Fatalf("limits.NewRedisRPMLimiter(corrupt) error = %v", err)
	}
	first, err := limiter.AcquireRPM(context.Background(), redisRPMBindings(1, 2))
	if err != nil || !first.Allowed() {
		t.Fatalf("AcquireRPM(corrupt setup) = %+v, %v", first, err)
	}
	keysMu.Lock()
	keys[first.WindowKey] = struct{}{}
	keysMu.Unlock()
	if err := client.HSet(context.Background(), first.WindowKey, "platform", "1e3").Err(); err != nil {
		t.Fatalf("HSet(corrupt counter) error = %v", err)
	}
	if _, err := limiter.AcquireRPM(context.Background(), redisRPMBindings(1, 2)); !errors.Is(err, limits.ErrRedisRPMProtocol) {
		t.Fatalf("AcquireRPM(corrupt counter) error = %v", err)
	}
	tenantCount, err := client.HGet(context.Background(), first.WindowKey, "tenant:"+redisRPMTenantID).Int64()
	if err != nil || tenantCount != 1 {
		t.Fatalf("tenant count after corrupt rejection = %d, %v", tenantCount, err)
	}
}

func openRedisIntegrationClient(t *testing.T) *redis.Client {
	t.Helper()
	address := os.Getenv("REDIS_ADDR")
	if address == "" {
		t.Skip("REDIS_ADDR is not set")
	}
	databaseNumber, err := strconv.Atoi(os.Getenv("REDIS_DB"))
	if err != nil {
		t.Fatalf("parse REDIS_DB: %v", err)
	}
	client := redis.NewClient(&redis.Options{
		Addr: address, Password: os.Getenv("REDIS_PASSWORD"), DB: databaseNumber,
	})
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("Redis Ping() error = %v", err)
	}
	return client
}

func waitForRedisRPMWindow(t *testing.T, client *redis.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	serverTime, err := client.Time(ctx).Result()
	if err != nil {
		t.Fatalf("Redis TIME error = %v", err)
	}
	remaining := time.Minute - time.Duration(serverTime.Second())*time.Second -
		time.Duration(serverTime.Nanosecond())
	if remaining >= 10*time.Second {
		return
	}
	timer := time.NewTimer(remaining + 100*time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		t.Fatalf("wait for Redis minute boundary: %v", context.Cause(ctx))
	case <-timer.C:
	}
}

func cleanupRedisRPMKeys(t *testing.T, client *redis.Client, keys map[string]struct{}) {
	t.Helper()
	if len(keys) == 0 {
		return
	}
	keyList := make([]string, 0, len(keys))
	for key := range keys {
		keyList = append(keyList, key)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Del(ctx, keyList...).Err(); err != nil {
		t.Errorf("cleanup Redis RPM keys: %v", err)
	}
}

func redisRPMBindings(soft, hard uint64) []limits.Binding {
	policy := redisRPMEffective(soft, hard)
	scopes := []limits.Scope{
		{Kind: limits.ScopePlatform},
		{Kind: limits.ScopeTenant, TenantID: redisRPMTenantID},
		{Kind: limits.ScopeProject, TenantID: redisRPMTenantID, ProjectID: redisRPMProjectID},
		{
			Kind: limits.ScopeKey, TenantID: redisRPMTenantID,
			ProjectID: redisRPMProjectID, KeyID: redisRPMKeyID,
		},
	}
	bindings := make([]limits.Binding, 0, len(scopes))
	for _, scope := range scopes {
		bindings = append(bindings, limits.Binding{Scope: scope, Policy: policy})
	}
	return bindings
}

func redisRPMEffective(soft, hard uint64) limitpolicy.Effective {
	return limitpolicy.Effective{
		RPM:         redisRPMThreshold(soft, hard),
		TPM:         redisRPMThreshold(100, 200),
		Concurrency: redisRPMThreshold(10, 20),
	}
}

func redisRPMThreshold(soft, hard uint64) limitpolicy.EffectiveThreshold {
	return limitpolicy.EffectiveThreshold{
		Soft: soft, Hard: hard,
		SoftSource: limitpolicy.SourcePlatform, HardSource: limitpolicy.SourcePlatform,
	}
}
