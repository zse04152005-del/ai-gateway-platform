package limits

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/limitpolicy"
)

func TestRedisTPMLimiterRetriesServerWindowAndReservesAllScopes(t *testing.T) {
	localTime := time.Date(2026, time.July, 31, 10, 59, 59, 0, time.UTC)
	serverTime := time.Date(2026, time.July, 31, 11, 0, 1, 250_000_000, time.UTC)
	serverWindow := serverTime.Unix() / redisTPMWindowSeconds
	resetAt := time.Date(2026, time.July, 31, 11, 1, 0, 0, time.UTC)
	expiresAt := resetAt.Add(time.Hour)
	evaluator := &scriptedRedisEvaluator{responses: []scriptedRedisResponse{
		{value: []any{
			redisTPMReserveWindowMismatch, serverWindow, serverTime.UnixMilli(), resetAt.UnixMilli(),
		}},
		{value: []any{
			redisTPMReserveAllowed, serverWindow, serverTime.UnixMilli(), resetAt.UnixMilli(),
			expiresAt.UnixMilli(), int64(0), int64(100), int64(200), int64(300), int64(400),
		}},
	}}
	options := DefaultRedisTPMOptions()
	options.KeyPrefix = "agw:test:p09t04:{tpm}"
	limiter, err := NewRedisTPMLimiter(evaluator, options, func() time.Time { return localTime })
	if err != nil {
		t.Fatalf("NewRedisTPMLimiter() error = %v", err)
	}
	bindings := reverseLocalBindings(localBindings(localEffective(10, 20, 250, 500, 5, 10)))
	request := RedisTPMReserveRequest{
		ReservationID: "reserve_req_p09_t04_1", Bindings: bindings, Plan: validTPMPlan(100),
	}

	reservation, err := limiter.ReserveTPM(context.Background(), request)
	if err != nil || !reservation.Allowed() {
		t.Fatalf("ReserveTPM() = %+v, %v", reservation, err)
	}
	if reservation.Handle.ReservationID != request.ReservationID ||
		reservation.Handle.Window != serverWindow || reservation.Handle.ReservedTokens != 100 ||
		len(reservation.Handle.Scopes) != requiredScopeCount || reservation.Idempotent ||
		reservation.WindowKey != options.KeyPrefix+":"+strconv.FormatInt(serverWindow, 10) ||
		!reservation.ServerTime.Equal(serverTime) || !reservation.ResetAt.Equal(resetAt) ||
		!reservation.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("reservation metadata = %+v", reservation)
	}
	for index, count := range reservation.Counts {
		want := uint64((index + 1) * 100)
		if count.Scope.Kind != localScopes()[index].Kind || count.Count != want || count.Soft != 250 ||
			count.SoftExceeded != (want > 250) {
			t.Fatalf("count[%d] = %+v", index, count)
		}
	}
	if len(evaluator.calls) != 2 || evaluator.calls[1].script != redisTPMReserveScript ||
		len(evaluator.calls[1].arguments) != 17 {
		t.Fatalf("Eval calls = %+v", evaluator.calls)
	}
	arguments := evaluator.calls[1].arguments
	if arguments[0] != serverWindow || arguments[1] != int64(time.Hour/time.Millisecond) ||
		arguments[2] != "reservation:"+request.ReservationID || arguments[4] != uint64(100) ||
		arguments[5] != "platform" || arguments[8] != "tenant:"+localTenantID ||
		arguments[11] != "project:"+localTenantID+":"+localProjectID ||
		arguments[14] != "key:"+localTenantID+":"+localProjectID+":"+localKeyID {
		t.Fatalf("reserve arguments = %#v", arguments)
	}
	if value, ok := arguments[3].(string); !ok || !strings.HasPrefix(value, "r:100:") || len(value) != 6+64 {
		t.Fatalf("reservation value = %#v", arguments[3])
	}
	reservation.Handle.Scopes[0].TenantID = "mutated"
	if localScopes()[0].TenantID != "" {
		t.Fatal("test fixture unexpectedly mutated")
	}
}

func TestRedisTPMLimiterHardRejectionIsAtomic(t *testing.T) {
	serverTime := time.Date(2026, time.July, 31, 11, 5, 2, 0, time.UTC)
	window := serverTime.Unix() / redisTPMWindowSeconds
	resetAt := time.Date(2026, time.July, 31, 11, 6, 0, 0, time.UTC)
	evaluator := &scriptedRedisEvaluator{responses: []scriptedRedisResponse{{value: []any{
		redisTPMReserveDenied, int64(2), int64(150), int64(200),
		window, serverTime.UnixMilli(), resetAt.UnixMilli(),
	}}}}
	limiter := mustRedisTPMLimiter(t, evaluator, serverTime)
	bindings := localBindings(localEffective(10, 20, 100, 200, 5, 10))
	request := RedisTPMReserveRequest{
		ReservationID: "reserve_denied", Bindings: bindings, Plan: validTPMPlan(75),
	}

	reservation, err := limiter.ReserveTPM(context.Background(), request)
	if err != nil || reservation.Allowed() || reservation.Rejection == nil {
		t.Fatalf("ReserveTPM() = %+v, %v", reservation, err)
	}
	if reservation.Rejection.Scope.Kind != ScopeTenant || reservation.Rejection.Count != 150 ||
		reservation.Rejection.Requested != 75 || reservation.Rejection.Hard != 200 ||
		reservation.Rejection.RetryAfter != 58*time.Second ||
		!reservation.Rejection.ResetAt.Equal(resetAt) || len(reservation.Counts) != 0 {
		t.Fatalf("rejection = %+v", reservation.Rejection)
	}
}

func TestRedisTPMLimiterSettlesReleaseOverageAndDuplicate(t *testing.T) {
	reservedAt := time.Date(2026, time.July, 31, 11, 59, 59, 0, time.UTC)
	settledAt := time.Date(2026, time.July, 31, 12, 1, 5, 0, time.UTC)
	window := reservedAt.Unix() / redisTPMWindowSeconds
	handle := RedisTPMHandle{
		ReservationID: "reserve_settle", Window: window,
		Scopes: localScopes(), ReservedTokens: 100,
	}
	actual := TPMActual{
		InputTokens: 40, OutputTokens: 20, Tokens: 60,
		Source: adapter.UsageSourceProvider, Complete: true,
	}
	evaluator := &scriptedRedisEvaluator{responses: []scriptedRedisResponse{
		{value: []any{
			redisTPMSettleSucceeded, settledAt.UnixMilli(), int64(100), int64(60), int64(-40), int64(0),
			int64(60), int64(160), int64(260), int64(360),
		}},
		{value: []any{
			redisTPMSettleSucceeded, settledAt.Add(time.Second).UnixMilli(), int64(100), int64(60), int64(-40), int64(1),
			int64(60), int64(160), int64(260), int64(360),
		}},
	}}
	limiter := mustRedisTPMLimiter(t, evaluator, reservedAt)

	settlement, err := limiter.SettleTPM(context.Background(), handle, actual)
	if err != nil {
		t.Fatalf("SettleTPM(release) error = %v", err)
	}
	if settlement.ReleasedTokens != 40 || settlement.OverageTokens != 0 || settlement.Idempotent ||
		!settlement.ServerTime.Equal(settledAt) || settlement.Actual != actual ||
		settlement.WindowKey != DefaultRedisTPMOptions().KeyPrefix+":"+strconv.FormatInt(window, 10) ||
		len(settlement.Counts) != requiredScopeCount || settlement.Counts[0].Count != 60 {
		t.Fatalf("release settlement = %+v", settlement)
	}
	duplicate, err := limiter.SettleTPM(context.Background(), handle, actual)
	if err != nil || !duplicate.Idempotent || duplicate.ReleasedTokens != 40 {
		t.Fatalf("SettleTPM(duplicate) = %+v, %v", duplicate, err)
	}
	if len(evaluator.calls) != 2 || evaluator.calls[0].script != redisTPMSettleScript ||
		len(evaluator.calls[0].arguments) != 9 || evaluator.calls[0].arguments[3] != uint64(100) ||
		evaluator.calls[0].arguments[4] != uint64(60) {
		t.Fatalf("settle calls = %+v", evaluator.calls)
	}
	pending := evaluator.calls[0].arguments[1].(string)
	settled := evaluator.calls[0].arguments[2].(string)
	if !strings.HasPrefix(pending, "r:100:") || !strings.HasPrefix(settled, "s:100:60:") {
		t.Fatalf("settlement markers = %q / %q", pending, settled)
	}

	overageActual := TPMActual{
		InputTokens: 70, OutputTokens: 50, Tokens: 120,
		Source: adapter.UsageSourceEstimated, Complete: false,
	}
	overageEvaluator := &scriptedRedisEvaluator{responses: []scriptedRedisResponse{{value: []any{
		redisTPMSettleSucceeded, settledAt.UnixMilli(), int64(100), int64(120), int64(20), int64(0),
		int64(120), int64(120), int64(120), int64(120),
	}}}}
	overageLimiter := mustRedisTPMLimiter(t, overageEvaluator, reservedAt)
	overage, err := overageLimiter.SettleTPM(context.Background(), handle, overageActual)
	if err != nil || overage.OverageTokens != 20 || overage.ReleasedTokens != 0 {
		t.Fatalf("SettleTPM(overage) = %+v, %v", overage, err)
	}

	exactActual := TPMActual{
		InputTokens: 60, OutputTokens: 40, Tokens: 100,
		Source: adapter.UsageSourceReconciled, Complete: true,
	}
	exactEvaluator := &scriptedRedisEvaluator{responses: []scriptedRedisResponse{{value: []any{
		redisTPMSettleSucceeded, settledAt.UnixMilli(), int64(100), int64(100), int64(0), int64(0),
		int64(100), int64(100), int64(100), int64(100),
	}}}}
	exact, err := mustRedisTPMLimiter(t, exactEvaluator, reservedAt).SettleTPM(
		context.Background(), handle, exactActual,
	)
	if err != nil || exact.OverageTokens != 0 || exact.ReleasedTokens != 0 {
		t.Fatalf("SettleTPM(exact) = %+v, %v", exact, err)
	}
}

func TestRedisTPMLimiterValidationAndInfrastructureFailures(t *testing.T) {
	clock := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	evaluator := &scriptedRedisEvaluator{}
	validOptions := DefaultRedisTPMOptions()
	invalidOptions := []RedisTPMOptions{
		{},
		{KeyPrefix: "bad prefix {tpm}", Retention: time.Hour, ClockRetries: 1},
		{KeyPrefix: "agw:no-tag", Retention: time.Hour, ClockRetries: 1},
		{KeyPrefix: "agw:{tpm}", Retention: 0, ClockRetries: 1},
		{KeyPrefix: "agw:{tpm}", Retention: maximumRedisTPMRetention + time.Millisecond, ClockRetries: 1},
		{KeyPrefix: "agw:{tpm}", Retention: time.Hour, ClockRetries: 0},
		{KeyPrefix: "agw:{tpm}", Retention: time.Hour, ClockRetries: maximumRedisTPMClockRetries + 1},
	}
	for index, options := range invalidOptions {
		if _, err := NewRedisTPMLimiter(evaluator, options, func() time.Time { return clock }); !errors.Is(err, ErrInvalid) {
			t.Fatalf("NewRedisTPMLimiter(invalid %d) error = %v", index, err)
		}
	}
	if _, err := NewRedisTPMLimiter(nil, validOptions, func() time.Time { return clock }); !errors.Is(err, ErrInvalid) {
		t.Fatalf("NewRedisTPMLimiter(nil evaluator) error = %v", err)
	}
	if _, err := NewRedisTPMLimiter(evaluator, validOptions, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("NewRedisTPMLimiter(nil clock) error = %v", err)
	}

	limiter := mustRedisTPMLimiter(t, evaluator, clock)
	validRequest := RedisTPMReserveRequest{
		ReservationID: "reserve_valid", Bindings: localBindings(localEffective(1, 2, 100, 200, 1, 2)),
		Plan: validTPMPlan(50),
	}
	var nilContext context.Context
	invalidRequests := []RedisTPMReserveRequest{
		{},
		{ReservationID: "bad id", Bindings: validRequest.Bindings, Plan: validRequest.Plan},
		{ReservationID: "reserve_bad_plan", Bindings: validRequest.Bindings},
		{ReservationID: "reserve_bad_bindings", Bindings: validRequest.Bindings[:3], Plan: validRequest.Plan},
	}
	if _, err := limiter.ReserveTPM(nilContext, validRequest); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ReserveTPM(nil context) error = %v", err)
	}
	for index, request := range invalidRequests {
		if _, err := limiter.ReserveTPM(context.Background(), request); !errors.Is(err, ErrInvalid) {
			t.Fatalf("ReserveTPM(invalid %d) error = %v", index, err)
		}
	}
	if _, err := (*RedisTPMLimiter)(nil).ReserveTPM(context.Background(), validRequest); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil ReserveTPM() error = %v", err)
	}

	handle := RedisTPMHandle{
		ReservationID: "reserve_valid", Window: clock.Unix() / 60,
		Scopes: localScopes(), ReservedTokens: 50,
	}
	actual := TPMActual{Tokens: 0, Source: adapter.UsageSourceEstimated, Complete: true}
	invalidHandles := []RedisTPMHandle{
		{},
		{ReservationID: "bad id", Window: handle.Window, Scopes: handle.Scopes, ReservedTokens: 50},
		{ReservationID: "reserve_valid", Window: -1, Scopes: handle.Scopes, ReservedTokens: 50},
		{ReservationID: "reserve_valid", Window: handle.Window, Scopes: handle.Scopes[:3], ReservedTokens: 50},
	}
	for index, invalidHandle := range invalidHandles {
		if _, err := limiter.SettleTPM(context.Background(), invalidHandle, actual); !errors.Is(err, ErrInvalid) {
			t.Fatalf("SettleTPM(invalid handle %d) error = %v", index, err)
		}
	}
	invalidActual := actual
	invalidActual.Tokens = 1
	if _, err := limiter.SettleTPM(context.Background(), handle, invalidActual); !errors.Is(err, ErrInvalid) {
		t.Fatalf("SettleTPM(invalid actual) error = %v", err)
	}
	if _, err := (*RedisTPMLimiter)(nil).SettleTPM(context.Background(), handle, actual); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil SettleTPM() error = %v", err)
	}

	transportError := errors.New("redis unavailable")
	reserveFailure := &scriptedRedisEvaluator{responses: []scriptedRedisResponse{{err: transportError}}}
	if _, err := mustRedisTPMLimiter(t, reserveFailure, clock).ReserveTPM(context.Background(), validRequest); !errors.Is(err, transportError) {
		t.Fatalf("ReserveTPM(transport) error = %v", err)
	}
	settleFailure := &scriptedRedisEvaluator{responses: []scriptedRedisResponse{{err: transportError}}}
	if _, err := mustRedisTPMLimiter(t, settleFailure, clock).SettleTPM(context.Background(), handle, actual); !errors.Is(err, transportError) {
		t.Fatalf("SettleTPM(transport) error = %v", err)
	}
}

func TestRedisTPMLimiterIdempotencyErrorsClockBoundAndProtocol(t *testing.T) {
	serverTime := time.Date(2026, time.July, 31, 12, 15, 1, 0, time.UTC)
	window := serverTime.Unix() / 60
	resetAt := time.Date(2026, time.July, 31, 12, 16, 0, 0, time.UTC)
	request := RedisTPMReserveRequest{
		ReservationID: "reserve_protocol", Bindings: localBindings(localEffective(1, 2, 100, 200, 1, 2)),
		Plan: validTPMPlan(50),
	}
	mismatch := scriptedRedisResponse{value: []any{
		redisTPMReserveWindowMismatch, window, serverTime.UnixMilli(), resetAt.UnixMilli(),
	}}
	bounded := &scriptedRedisEvaluator{responses: []scriptedRedisResponse{mismatch, mismatch, mismatch}}
	if _, err := mustRedisTPMLimiter(t, bounded, serverTime).ReserveTPM(context.Background(), request); !errors.Is(err, ErrRedisTPMClockBoundary) {
		t.Fatalf("ReserveTPM(clock boundary) error = %v", err)
	}

	reserveErrors := []struct {
		response any
		want     error
	}{
		{[]any{redisTPMReserveConflict}, ErrRedisTPMReservationConflict},
		{[]any{redisTPMReserveCorrupt, int64(0)}, ErrRedisTPMProtocol},
		{[]any{redisTPMReserveCorrupt, int64(3)}, ErrRedisTPMProtocol},
		{"bad", ErrRedisTPMProtocol},
		{[]any{}, ErrRedisTPMProtocol},
		{[]any{"bad code"}, ErrRedisTPMProtocol},
		{[]any{int64(99)}, ErrRedisTPMProtocol},
		{[]any{redisTPMReserveAllowed, window}, ErrRedisTPMProtocol},
		{[]any{redisTPMReserveDenied, int64(1), int64(0), int64(200), window, serverTime.UnixMilli(), resetAt.UnixMilli()}, ErrRedisTPMProtocol},
		{[]any{
			redisTPMReserveDenied, int64(1), int64(150), int64(199),
			window, serverTime.UnixMilli(), resetAt.UnixMilli(),
		}, ErrRedisTPMProtocol},
		{[]any{
			redisTPMReserveAllowed, window + 1, serverTime.UnixMilli(), resetAt.UnixMilli(),
			resetAt.Add(time.Hour).UnixMilli(), int64(0), int64(50), int64(50), int64(50), int64(50),
		}, ErrRedisTPMProtocol},
		{[]any{
			redisTPMReserveAllowed, window, serverTime.UnixMilli(), resetAt.UnixMilli(),
			resetAt.Add(2 * time.Hour).UnixMilli(), int64(0), int64(50), int64(50), int64(50), int64(50),
		}, ErrRedisTPMProtocol},
		{[]any{
			redisTPMReserveAllowed, window, serverTime.UnixMilli(), resetAt.UnixMilli(),
			resetAt.Add(time.Hour).UnixMilli(), int64(0), int64(201), int64(50), int64(50), int64(50),
		}, ErrRedisTPMProtocol},
	}
	for index, test := range reserveErrors {
		evaluator := &scriptedRedisEvaluator{responses: []scriptedRedisResponse{{value: test.response}}}
		_, err := mustRedisTPMLimiter(t, evaluator, serverTime).ReserveTPM(context.Background(), request)
		if !errors.Is(err, test.want) {
			t.Fatalf("ReserveTPM(protocol %d) error = %v, want %v", index, err, test.want)
		}
	}
	idempotentEvaluator := &scriptedRedisEvaluator{responses: []scriptedRedisResponse{{value: []any{
		redisTPMReserveAllowed, window, serverTime.UnixMilli(), resetAt.UnixMilli(),
		resetAt.Add(time.Hour).UnixMilli(), int64(1), int64(201), int64(201), int64(201), int64(201),
	}}}}
	idempotent, err := mustRedisTPMLimiter(t, idempotentEvaluator, serverTime).ReserveTPM(
		context.Background(), request,
	)
	if err != nil || !idempotent.Allowed() || !idempotent.Idempotent {
		t.Fatalf("ReserveTPM(idempotent over current hard) = %+v, %v", idempotent, err)
	}

	handle := RedisTPMHandle{
		ReservationID: request.ReservationID, Window: window, Scopes: localScopes(), ReservedTokens: 50,
	}
	actual := TPMActual{
		InputTokens: 20, OutputTokens: 10, Tokens: 30,
		Source: adapter.UsageSourceEstimated, Complete: false,
	}
	settleErrors := []struct {
		response any
		want     error
	}{
		{[]any{redisTPMSettleMissing}, ErrRedisTPMReservationExpired},
		{[]any{redisTPMSettleConflict}, ErrRedisTPMReservationConflict},
		{[]any{redisTPMSettleCorrupt, int64(4)}, ErrRedisTPMProtocol},
		{[]any{redisTPMSettleSucceeded, serverTime.UnixMilli()}, ErrRedisTPMProtocol},
		{[]any{int64(99)}, ErrRedisTPMProtocol},
		{"bad", ErrRedisTPMProtocol},
		{[]any{}, ErrRedisTPMProtocol},
		{[]any{"bad code"}, ErrRedisTPMProtocol},
		{[]any{redisTPMSettleMissing, int64(1)}, ErrRedisTPMProtocol},
		{[]any{redisTPMSettleConflict, int64(1)}, ErrRedisTPMProtocol},
		{[]any{redisTPMSettleCorrupt, int64(5)}, ErrRedisTPMProtocol},
	}
	for index, test := range settleErrors {
		evaluator := &scriptedRedisEvaluator{responses: []scriptedRedisResponse{{value: test.response}}}
		_, err := mustRedisTPMLimiter(t, evaluator, serverTime).SettleTPM(context.Background(), handle, actual)
		if !errors.Is(err, test.want) {
			t.Fatalf("SettleTPM(protocol %d) error = %v, want %v", index, err, test.want)
		}
	}
	for index, kind := range []ScopeKind{ScopePlatform, ScopeTenant, ScopeProject, ScopeKey} {
		if got := scopeIndexName(index); got != kind {
			t.Fatalf("scopeIndexName(%d) = %q, want %q", index, got, kind)
		}
	}
	if scopeIndexName(99) != ScopeKind("unknown") {
		t.Fatalf("scopeIndexName(99) = %q", scopeIndexName(99))
	}
	if delta, ok := redisTPMDelta(0, limitpolicy.MaximumValue+1); ok || delta != 0 {
		t.Fatalf("redisTPMDelta(overflow positive) = %d, %t", delta, ok)
	}
	if delta, ok := redisTPMDelta(limitpolicy.MaximumValue+1, 0); ok || delta != 0 {
		t.Fatalf("redisTPMDelta(overflow negative) = %d, %t", delta, ok)
	}
}

func mustRedisTPMLimiter(
	t *testing.T,
	evaluator RedisEvaluator,
	now time.Time,
) *RedisTPMLimiter {
	t.Helper()
	limiter, err := NewRedisTPMLimiter(evaluator, DefaultRedisTPMOptions(), func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewRedisTPMLimiter() error = %v", err)
	}
	return limiter
}

func validTPMPlan(reserved uint64) TPMReservationPlan {
	return TPMReservationPlan{
		InputTokens: 1, MaximumOutputTokens: reserved - 1, ReservedTokens: reserved,
		EstimatorMethod: "test-estimator", EstimatorVersion: "v1", Estimated: true,
	}
}

func reverseLocalBindings(bindings []Binding) []Binding {
	for left, right := 0, len(bindings)-1; left < right; left, right = left+1, right-1 {
		bindings[left], bindings[right] = bindings[right], bindings[left]
	}
	return bindings
}
