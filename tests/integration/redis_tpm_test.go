//go:build integration

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/limitpolicy"
	"github.com/zse04152005-del/ai-gateway-platform/internal/limits"
)

const (
	redisTPMTenantID      = "75000000-0000-4000-8000-000000000001"
	redisTPMProjectID     = "75000000-0000-4000-8000-000000000002"
	redisTPMKeyID         = "75000000-0000-4000-8000-000000000003"
	redisTPMWorkers       = 64
	redisTPMReserved      = 25
	redisTPMHard          = 500
	redisTPMSoft          = 400
	redisTPMAllowed       = redisTPMHard / redisTPMReserved
	redisTPMTestRetention = 2 * time.Minute
)

type redisTPMResult struct {
	worker      int
	reservation limits.RedisTPMReservation
	err         error
}

func TestRedisTPMAtomicReservationAndSettlement(t *testing.T) {
	client := openRedisIntegrationClient(t)
	waitForRedisRPMWindow(t, client)
	evaluator, err := limits.NewGoRedisEvaluator(client)
	if err != nil {
		t.Fatalf("limits.NewGoRedisEvaluator() error = %v", err)
	}
	prefix := fmt.Sprintf("agw:test:p09t04:{tpm}:%d", time.Now().UnixNano())
	limiter := newRedisTPMIntegrationLimiter(t, evaluator, prefix)
	keys := make(map[string]struct{})
	t.Cleanup(func() { cleanupRedisTPMKeys(t, client, keys) })

	bindings := redisTPMBindings(redisTPMSoft, redisTPMHard)
	start := make(chan struct{})
	results := make(chan redisTPMResult, redisTPMWorkers)
	var workers sync.WaitGroup
	workers.Add(redisTPMWorkers)
	for worker := range redisTPMWorkers {
		go func() {
			defer workers.Done()
			<-start
			reservation, reserveErr := limiter.ReserveTPM(context.Background(), limits.RedisTPMReserveRequest{
				ReservationID: fmt.Sprintf("reserve_%03d", worker),
				Bindings:      bindings, Plan: redisTPMPlan(redisTPMReserved),
			})
			results <- redisTPMResult{worker: worker, reservation: reservation, err: reserveErr}
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	allowed := make([]redisTPMResult, 0, redisTPMAllowed)
	denied := 0
	softAdmissions := 0
	windowKey := ""
	for outcome := range results {
		if outcome.err != nil {
			t.Fatalf("ReserveTPM(worker %d) error = %v", outcome.worker, outcome.err)
		}
		if outcome.reservation.WindowKey == "" {
			t.Fatal("ReserveTPM() returned an empty WindowKey")
		}
		keys[outcome.reservation.WindowKey] = struct{}{}
		if windowKey == "" {
			windowKey = outcome.reservation.WindowKey
		}
		if outcome.reservation.WindowKey != windowKey {
			t.Fatalf("concurrent reservations crossed windows: %q / %q", windowKey, outcome.reservation.WindowKey)
		}
		if outcome.reservation.Allowed() {
			allowed = append(allowed, outcome)
			if outcome.reservation.Counts[0].SoftExceeded {
				softAdmissions++
			}
			continue
		}
		denied++
		if outcome.reservation.Rejection == nil ||
			outcome.reservation.Rejection.Scope.Kind != limits.ScopePlatform ||
			outcome.reservation.Rejection.Count != redisTPMHard ||
			outcome.reservation.Rejection.Requested != redisTPMReserved ||
			outcome.reservation.Rejection.Hard != redisTPMHard {
			t.Fatalf("denied reservation = %+v", outcome.reservation)
		}
	}
	if len(allowed) != redisTPMAllowed || denied != redisTPMWorkers-redisTPMAllowed ||
		softAdmissions != redisTPMAllowed-redisTPMSoft/redisTPMReserved {
		t.Fatalf("outcomes = allowed:%d denied:%d soft:%d", len(allowed), denied, softAdmissions)
	}
	assertRedisTPMFields(t, client, windowKey, redisTPMHard, redisTPMAllowed)

	first := allowed[0]
	duplicate, err := limiter.ReserveTPM(context.Background(), limits.RedisTPMReserveRequest{
		ReservationID: first.reservation.Handle.ReservationID,
		Bindings:      bindings, Plan: redisTPMPlan(redisTPMReserved),
	})
	if err != nil || !duplicate.Allowed() || !duplicate.Idempotent ||
		duplicate.Handle.Window != first.reservation.Handle.Window {
		t.Fatalf("ReserveTPM(duplicate) = %+v, %v", duplicate, err)
	}
	assertRedisTPMFields(t, client, windowKey, redisTPMHard, redisTPMAllowed)

	ttlBefore, err := client.PTTL(context.Background(), windowKey).Result()
	if err != nil || ttlBefore <= time.Minute || ttlBefore > time.Minute+redisTPMTestRetention {
		t.Fatalf("PTTL(before settlement) = %v, %v", ttlBefore, err)
	}
	settlements := settleRedisTPMReservations(t, limiter, allowed)
	if len(settlements) != redisTPMAllowed {
		t.Fatalf("settlement count = %d", len(settlements))
	}
	assertRedisTPMFields(t, client, windowKey, redisTPMAllowed*10, redisTPMAllowed)
	ttlAfter, err := client.PTTL(context.Background(), windowKey).Result()
	if err != nil || ttlAfter <= 0 || ttlAfter > ttlBefore {
		t.Fatalf("PTTL(after settlement) = %v, before %v, error %v", ttlAfter, ttlBefore, err)
	}

	firstActual := redisTPMActual(10)
	settledAgain, err := limiter.SettleTPM(context.Background(), first.reservation.Handle, firstActual)
	if err != nil || !settledAgain.Idempotent || settledAgain.ReleasedTokens != 15 {
		t.Fatalf("SettleTPM(duplicate) = %+v, %v", settledAgain, err)
	}
	conflictingActual := redisTPMActual(11)
	if _, err := limiter.SettleTPM(context.Background(), first.reservation.Handle, conflictingActual); !errors.Is(err, limits.ErrRedisTPMReservationConflict) {
		t.Fatalf("SettleTPM(conflicting actual) error = %v", err)
	}
	assertRedisTPMFields(t, client, windowKey, redisTPMAllowed*10, redisTPMAllowed)

	testRedisTPMOverageBlocksAdmission(t, client, evaluator, keys)
	testRedisTPMExpiredAndCorruptFailClosed(t, client, evaluator, keys)
}

func settleRedisTPMReservations(
	t *testing.T,
	limiter *limits.RedisTPMLimiter,
	reservations []redisTPMResult,
) []limits.RedisTPMSettlement {
	t.Helper()
	settlements := make(chan limits.RedisTPMSettlement, len(reservations))
	errorsOut := make(chan error, len(reservations))
	var workers sync.WaitGroup
	workers.Add(len(reservations))
	for _, outcome := range reservations {
		go func() {
			defer workers.Done()
			settlement, err := limiter.SettleTPM(
				context.Background(), outcome.reservation.Handle, redisTPMActual(10),
			)
			if err != nil {
				errorsOut <- err
				return
			}
			settlements <- settlement
		}()
	}
	workers.Wait()
	close(settlements)
	close(errorsOut)
	for err := range errorsOut {
		t.Fatalf("SettleTPM(concurrent) error = %v", err)
	}
	result := make([]limits.RedisTPMSettlement, 0, len(reservations))
	for settlement := range settlements {
		if settlement.ReleasedTokens != 15 || settlement.OverageTokens != 0 || settlement.Idempotent {
			t.Fatalf("concurrent settlement = %+v", settlement)
		}
		result = append(result, settlement)
	}
	return result
}

func testRedisTPMOverageBlocksAdmission(
	t *testing.T,
	client *redis.Client,
	evaluator *limits.GoRedisEvaluator,
	keys map[string]struct{},
) {
	t.Helper()
	prefix := fmt.Sprintf("agw:test:p09t04:overage:{tpm}:%d", time.Now().UnixNano())
	limiter := newRedisTPMIntegrationLimiter(t, evaluator, prefix)
	bindings := redisTPMBindings(80, 100)
	reservation, err := limiter.ReserveTPM(context.Background(), limits.RedisTPMReserveRequest{
		ReservationID: "reserve_overage", Bindings: bindings, Plan: redisTPMPlan(80),
	})
	if err != nil || !reservation.Allowed() {
		t.Fatalf("ReserveTPM(overage setup) = %+v, %v", reservation, err)
	}
	keys[reservation.WindowKey] = struct{}{}
	actual := limits.TPMActual{
		InputTokens: 70, OutputTokens: 50, Tokens: 120,
		Source: adapter.UsageSourceProvider, Complete: true,
	}
	settlement, err := limiter.SettleTPM(context.Background(), reservation.Handle, actual)
	if err != nil || settlement.OverageTokens != 40 || settlement.Counts[0].Count != 120 {
		t.Fatalf("SettleTPM(overage) = %+v, %v", settlement, err)
	}
	denied, err := limiter.ReserveTPM(context.Background(), limits.RedisTPMReserveRequest{
		ReservationID: "reserve_after_overage", Bindings: bindings, Plan: redisTPMPlan(2),
	})
	if err != nil || denied.Rejection == nil || denied.Rejection.Count != 120 || denied.Rejection.Hard != 100 {
		t.Fatalf("ReserveTPM(after overage) = %+v, %v", denied, err)
	}
	platform, err := client.HGet(context.Background(), reservation.WindowKey, "platform").Int64()
	if err != nil || platform != 120 {
		t.Fatalf("platform after overage denial = %d, %v", platform, err)
	}
}

func testRedisTPMExpiredAndCorruptFailClosed(
	t *testing.T,
	client *redis.Client,
	evaluator *limits.GoRedisEvaluator,
	keys map[string]struct{},
) {
	t.Helper()
	prefix := fmt.Sprintf("agw:test:p09t04:expiry:{tpm}:%d", time.Now().UnixNano())
	limiter := newRedisTPMIntegrationLimiter(t, evaluator, prefix)
	bindings := redisTPMBindings(100, 200)
	reservation, err := limiter.ReserveTPM(context.Background(), limits.RedisTPMReserveRequest{
		ReservationID: "reserve_expired", Bindings: bindings, Plan: redisTPMPlan(25),
	})
	if err != nil || !reservation.Allowed() {
		t.Fatalf("ReserveTPM(expiry setup) = %+v, %v", reservation, err)
	}
	keys[reservation.WindowKey] = struct{}{}
	if err := client.Del(context.Background(), reservation.WindowKey).Err(); err != nil {
		t.Fatalf("Del(expired reservation) error = %v", err)
	}
	if _, err := limiter.SettleTPM(context.Background(), reservation.Handle, redisTPMActual(10)); !errors.Is(err, limits.ErrRedisTPMReservationExpired) {
		t.Fatalf("SettleTPM(expired) error = %v", err)
	}

	corrupt, err := limiter.ReserveTPM(context.Background(), limits.RedisTPMReserveRequest{
		ReservationID: "reserve_corrupt", Bindings: bindings, Plan: redisTPMPlan(25),
	})
	if err != nil || !corrupt.Allowed() {
		t.Fatalf("ReserveTPM(corrupt setup) = %+v, %v", corrupt, err)
	}
	keys[corrupt.WindowKey] = struct{}{}
	if err := client.HSet(context.Background(), corrupt.WindowKey, "tenant:"+redisTPMTenantID, "1e3").Err(); err != nil {
		t.Fatalf("HSet(corrupt TPM) error = %v", err)
	}
	if _, err := limiter.SettleTPM(context.Background(), corrupt.Handle, redisTPMActual(10)); !errors.Is(err, limits.ErrRedisTPMProtocol) {
		t.Fatalf("SettleTPM(corrupt) error = %v", err)
	}
	platform, err := client.HGet(context.Background(), corrupt.WindowKey, "platform").Int64()
	if err != nil || platform != 25 {
		t.Fatalf("platform after corrupt settlement = %d, %v", platform, err)
	}
}

func newRedisTPMIntegrationLimiter(
	t *testing.T,
	evaluator *limits.GoRedisEvaluator,
	prefix string,
) *limits.RedisTPMLimiter {
	t.Helper()
	options := limits.DefaultRedisTPMOptions()
	options.KeyPrefix = prefix
	options.Retention = redisTPMTestRetention
	limiter, err := limits.NewRedisTPMLimiter(evaluator, options, time.Now)
	if err != nil {
		t.Fatalf("limits.NewRedisTPMLimiter() error = %v", err)
	}
	return limiter
}

func assertRedisTPMFields(
	t *testing.T,
	client *redis.Client,
	key string,
	wantCount int,
	wantReservations int,
) {
	t.Helper()
	fields, err := client.HGetAll(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("HGetAll(%q) error = %v", key, err)
	}
	if len(fields) != 4+wantReservations {
		t.Fatalf("field count = %d, want %d: %#v", len(fields), 4+wantReservations, fields)
	}
	for _, field := range []string{
		"platform", "tenant:" + redisTPMTenantID,
		"project:" + redisTPMTenantID + ":" + redisTPMProjectID,
		"key:" + redisTPMTenantID + ":" + redisTPMProjectID + ":" + redisTPMKeyID,
	} {
		if fields[field] != strconv.Itoa(wantCount) {
			t.Fatalf("field %q = %q, want %d", field, fields[field], wantCount)
		}
	}
}

func cleanupRedisTPMKeys(t *testing.T, client *redis.Client, keys map[string]struct{}) {
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
		t.Errorf("cleanup Redis TPM keys: %v", err)
	}
}

func redisTPMBindings(soft, hard uint64) []limits.Binding {
	policy := limitpolicy.Effective{
		RPM:         redisRPMThreshold(1_000, 2_000),
		TPM:         redisRPMThreshold(soft, hard),
		Concurrency: redisRPMThreshold(100, 200),
	}
	scopes := []limits.Scope{
		{Kind: limits.ScopePlatform},
		{Kind: limits.ScopeTenant, TenantID: redisTPMTenantID},
		{Kind: limits.ScopeProject, TenantID: redisTPMTenantID, ProjectID: redisTPMProjectID},
		{
			Kind: limits.ScopeKey, TenantID: redisTPMTenantID,
			ProjectID: redisTPMProjectID, KeyID: redisTPMKeyID,
		},
	}
	bindings := make([]limits.Binding, 0, len(scopes))
	for _, scope := range scopes {
		bindings = append(bindings, limits.Binding{Scope: scope, Policy: policy})
	}
	return bindings
}

func redisTPMPlan(reserved uint64) limits.TPMReservationPlan {
	return limits.TPMReservationPlan{
		InputTokens: 1, MaximumOutputTokens: reserved - 1, ReservedTokens: reserved,
		Tokenizer: "integration-estimator", TokenizerVersion: "v1", PhysicalModel: "model-fixture",
		DeploymentVersion: 1, ProviderProtocolVersion: "protocol-v1", Estimated: true,
	}
}

func redisTPMActual(tokens uint64) limits.TPMActual {
	return limits.TPMActual{
		InputTokens: tokens, OutputTokens: 0, Tokens: tokens,
		Source: adapter.UsageSourceProvider, Complete: true,
	}
}
