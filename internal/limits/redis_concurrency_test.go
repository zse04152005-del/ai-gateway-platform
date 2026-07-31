package limits

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRedisConcurrencyAcquireReturnsCanonicalLeaseAndSoftFacts(t *testing.T) {
	serverTime := time.Date(2026, time.July, 31, 13, 0, 0, 250_000_000, time.UTC)
	expiresAt := serverTime.Add(defaultRedisConcurrencyLease)
	evaluator := &scriptedRedisEvaluator{responses: []scriptedRedisResponse{{value: []any{
		redisConcurrencyAcquireAllowed, serverTime.UnixMilli(), expiresAt.UnixMilli(), int64(0),
		int64(1), int64(2), int64(3), int64(4),
	}}}}
	options := DefaultRedisConcurrencyOptions()
	options.KeyPrefix = "agw:test:p09t05:{concurrency}"
	limiter, err := NewRedisConcurrencyLimiter(evaluator, options)
	if err != nil {
		t.Fatalf("NewRedisConcurrencyLimiter() error = %v", err)
	}
	bindings := reverseLocalBindings(localBindings(localEffective(10, 20, 100, 200, 2, 5)))
	request := RedisConcurrencyAcquireRequest{LeaseID: "lease_req_p09_t05", Bindings: bindings}

	admission, err := limiter.Acquire(context.Background(), request)
	if err != nil || !admission.Allowed() {
		t.Fatalf("Acquire() = %+v, %v", admission, err)
	}
	if admission.Handle.LeaseID != request.LeaseID || len(admission.Handle.Scopes) != requiredScopeCount ||
		!admission.ServerTime.Equal(serverTime) || !admission.ExpiresAt.Equal(expiresAt) ||
		admission.Idempotent || admission.Rejection != nil {
		t.Fatalf("admission metadata = %+v", admission)
	}
	for index, count := range admission.Counts {
		want := uint64(index + 1)
		if count.Scope.Kind != localScopes()[index].Kind || count.Count != want || count.Soft != 2 ||
			count.SoftExceeded != (want > 2) {
			t.Fatalf("count[%d] = %+v", index, count)
		}
	}
	if len(evaluator.calls) != 1 || evaluator.calls[0].script != redisConcurrencyAcquireScript ||
		len(evaluator.calls[0].keys) != 5 || len(evaluator.calls[0].arguments) != 8 {
		t.Fatalf("Eval call = %+v", evaluator.calls)
	}
	keys := evaluator.calls[0].keys
	if keys[0] != options.KeyPrefix+":platform" ||
		keys[1] != options.KeyPrefix+":tenant:"+localTenantID ||
		keys[2] != options.KeyPrefix+":project:"+localTenantID+":"+localProjectID ||
		keys[3] != options.KeyPrefix+":key:"+localTenantID+":"+localProjectID+":"+localKeyID ||
		keys[4] != options.KeyPrefix+":lease:"+request.LeaseID {
		t.Fatalf("keys = %#v", keys)
	}
	arguments := evaluator.calls[0].arguments
	if arguments[0] != int64(defaultRedisConcurrencyLease/time.Millisecond) ||
		arguments[1] != int64(defaultRedisConcurrencyRetention/time.Millisecond) ||
		arguments[2] != request.LeaseID || len(arguments[3].(string)) != 64 ||
		arguments[4] != uint64(5) {
		t.Fatalf("arguments = %#v", arguments)
	}
}

func TestRedisConcurrencyAcquireDenialAndIdempotency(t *testing.T) {
	serverTime := time.Date(2026, time.July, 31, 13, 5, 0, 0, time.UTC)
	earliest := serverTime.Add(5 * time.Second)
	deniedEvaluator := &scriptedRedisEvaluator{responses: []scriptedRedisResponse{{value: []any{
		redisConcurrencyAcquireDenied, int64(2), int64(3), int64(3),
		earliest.UnixMilli(), serverTime.UnixMilli(),
	}}}}
	limiter := mustRedisConcurrencyLimiter(t, deniedEvaluator)
	bindings := localBindings(localEffective(10, 20, 100, 200, 2, 3))

	denied, err := limiter.Acquire(context.Background(), RedisConcurrencyAcquireRequest{
		LeaseID: "lease_denied", Bindings: bindings,
	})
	if err != nil || denied.Allowed() || denied.Rejection == nil {
		t.Fatalf("Acquire(denied) = %+v, %v", denied, err)
	}
	if denied.Rejection.Scope.Kind != ScopeTenant || denied.Rejection.Count != 3 ||
		denied.Rejection.Hard != 3 || denied.Rejection.RetryAfter != 5*time.Second ||
		!denied.Rejection.EarliestExpiry.Equal(earliest) {
		t.Fatalf("rejection = %+v", denied.Rejection)
	}

	expires := serverTime.Add(30 * time.Second)
	idempotentEvaluator := &scriptedRedisEvaluator{responses: []scriptedRedisResponse{{value: []any{
		redisConcurrencyAcquireAllowed, serverTime.UnixMilli(), expires.UnixMilli(), int64(1),
		int64(4), int64(4), int64(4), int64(4),
	}}}}
	idempotent, err := mustRedisConcurrencyLimiter(t, idempotentEvaluator).Acquire(
		context.Background(), RedisConcurrencyAcquireRequest{LeaseID: "lease_retry", Bindings: bindings},
	)
	if err != nil || !idempotent.Allowed() || !idempotent.Idempotent {
		t.Fatalf("Acquire(idempotent) = %+v, %v", idempotent, err)
	}
}

func TestRedisConcurrencyRenewAndReleaseLifecycle(t *testing.T) {
	serverTime := time.Date(2026, time.July, 31, 13, 10, 0, 0, time.UTC)
	expires := serverTime.Add(30 * time.Second)
	handle := RedisConcurrencyHandle{LeaseID: "lease_lifecycle", Scopes: localScopes()}
	evaluator := &scriptedRedisEvaluator{responses: []scriptedRedisResponse{
		{value: []any{
			redisConcurrencyLifecycleSucceeded, serverTime.UnixMilli(), expires.UnixMilli(),
			int64(1), int64(2), int64(3), int64(4),
		}},
		{value: []any{
			redisConcurrencyLifecycleSucceeded, serverTime.Add(time.Second).UnixMilli(), expires.UnixMilli(),
			int64(0), int64(0), int64(0), int64(0), int64(0), int64(0),
		}},
		{value: []any{
			redisConcurrencyLifecycleSucceeded, serverTime.Add(2 * time.Second).UnixMilli(), expires.UnixMilli(),
			int64(1), int64(0), int64(0), int64(0), int64(0), int64(0),
		}},
	}}
	limiter := mustRedisConcurrencyLimiter(t, evaluator)

	renewal, err := limiter.Renew(context.Background(), handle)
	if err != nil || !renewal.ServerTime.Equal(serverTime) || !renewal.ExpiresAt.Equal(expires) ||
		len(renewal.Usage) != requiredScopeCount || renewal.Usage[3].Count != 4 {
		t.Fatalf("Renew() = %+v, %v", renewal, err)
	}
	release, err := limiter.Release(context.Background(), handle)
	if err != nil || release.Idempotent || release.Expired || len(release.Usage) != requiredScopeCount ||
		release.Usage[0].Count != 0 {
		t.Fatalf("Release() = %+v, %v", release, err)
	}
	duplicate, err := limiter.Release(context.Background(), handle)
	if err != nil || !duplicate.Idempotent || duplicate.Expired {
		t.Fatalf("Release(duplicate) = %+v, %v", duplicate, err)
	}
	if len(evaluator.calls) != 3 || evaluator.calls[0].script != redisConcurrencyRenewScript ||
		evaluator.calls[1].script != redisConcurrencyReleaseScript ||
		len(evaluator.calls[0].arguments) != 4 || len(evaluator.calls[1].arguments) != 2 {
		t.Fatalf("lifecycle calls = %+v", evaluator.calls)
	}
	renewal.Handle.Scopes[0].TenantID = "mutated"
	if handle.Scopes[0].TenantID != "" {
		t.Fatal("Renew() returned an aliased handle")
	}

	expiredEvaluator := &scriptedRedisEvaluator{responses: []scriptedRedisResponse{{value: []any{
		redisConcurrencyLifecycleSucceeded, serverTime.UnixMilli(), expires.UnixMilli(),
		int64(0), int64(1), int64(0), int64(0), int64(0), int64(0),
	}}}}
	expired, err := mustRedisConcurrencyLimiter(t, expiredEvaluator).Release(context.Background(), handle)
	if err != nil || !expired.Expired || expired.Idempotent {
		t.Fatalf("Release(expired) = %+v, %v", expired, err)
	}
}

func TestRedisConcurrencyValidationTransportAndStateErrors(t *testing.T) {
	validEvaluator := &scriptedRedisEvaluator{}
	validOptions := DefaultRedisConcurrencyOptions()
	invalidOptions := []RedisConcurrencyOptions{
		{},
		{KeyPrefix: "bad prefix {concurrency}", LeaseDuration: time.Second, Retention: time.Second},
		{KeyPrefix: "agw:no-tag", LeaseDuration: time.Second, Retention: time.Second},
		{KeyPrefix: "agw:{concurrency}", LeaseDuration: minimumRedisConcurrencyLease - time.Millisecond, Retention: time.Second},
		{KeyPrefix: "agw:{concurrency}", LeaseDuration: maximumRedisConcurrencyLease + time.Millisecond, Retention: time.Second},
		{KeyPrefix: "agw:{concurrency}", LeaseDuration: time.Second, Retention: 0},
		{KeyPrefix: "agw:{concurrency}", LeaseDuration: time.Second, Retention: maximumRedisConcurrencyRetention + time.Millisecond},
	}
	for index, options := range invalidOptions {
		if _, err := NewRedisConcurrencyLimiter(validEvaluator, options); !errors.Is(err, ErrInvalid) {
			t.Fatalf("NewRedisConcurrencyLimiter(invalid %d) error = %v", index, err)
		}
	}
	if _, err := NewRedisConcurrencyLimiter(nil, validOptions); !errors.Is(err, ErrInvalid) {
		t.Fatalf("NewRedisConcurrencyLimiter(nil evaluator) error = %v", err)
	}
	limiter := mustRedisConcurrencyLimiter(t, validEvaluator)
	bindings := localBindings(localEffective(1, 2, 1, 2, 1, 2))
	var nilContext context.Context
	invalidRequests := []RedisConcurrencyAcquireRequest{
		{},
		{LeaseID: "bad id", Bindings: bindings},
		{LeaseID: "lease_bad_bindings", Bindings: bindings[:3]},
	}
	if _, err := limiter.Acquire(nilContext, RedisConcurrencyAcquireRequest{LeaseID: "lease", Bindings: bindings}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Acquire(nil context) error = %v", err)
	}
	for index, request := range invalidRequests {
		if _, err := limiter.Acquire(context.Background(), request); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Acquire(invalid %d) error = %v", index, err)
		}
	}
	if _, err := (*RedisConcurrencyLimiter)(nil).Acquire(
		context.Background(), RedisConcurrencyAcquireRequest{LeaseID: "lease", Bindings: bindings},
	); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil Acquire() error = %v", err)
	}

	handle := RedisConcurrencyHandle{LeaseID: "lease_valid", Scopes: localScopes()}
	invalidHandles := []RedisConcurrencyHandle{
		{},
		{LeaseID: "bad id", Scopes: localScopes()},
		{LeaseID: "lease_valid", Scopes: localScopes()[:3]},
	}
	for index, invalid := range invalidHandles {
		if _, err := limiter.Renew(context.Background(), invalid); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Renew(invalid %d) error = %v", index, err)
		}
		if _, err := limiter.Release(context.Background(), invalid); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Release(invalid %d) error = %v", index, err)
		}
	}
	if _, err := (*RedisConcurrencyLimiter)(nil).Renew(context.Background(), handle); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil Renew() error = %v", err)
	}
	if _, err := (*RedisConcurrencyLimiter)(nil).Release(context.Background(), handle); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil Release() error = %v", err)
	}

	transportError := errors.New("redis unavailable")
	operations := []func(*RedisConcurrencyLimiter) error{
		func(selected *RedisConcurrencyLimiter) error {
			_, err := selected.Acquire(context.Background(), RedisConcurrencyAcquireRequest{LeaseID: "lease", Bindings: bindings})
			return err
		},
		func(selected *RedisConcurrencyLimiter) error {
			_, err := selected.Renew(context.Background(), handle)
			return err
		},
		func(selected *RedisConcurrencyLimiter) error {
			_, err := selected.Release(context.Background(), handle)
			return err
		},
	}
	for index, operation := range operations {
		failing := &scriptedRedisEvaluator{responses: []scriptedRedisResponse{{err: transportError}}}
		if err := operation(mustRedisConcurrencyLimiter(t, failing)); !errors.Is(err, transportError) {
			t.Fatalf("operation %d transport error = %v", index, err)
		}
	}
}

func TestRedisConcurrencyProtocolAndLifecycleErrors(t *testing.T) {
	serverTime := time.Date(2026, time.July, 31, 13, 20, 0, 0, time.UTC)
	expires := serverTime.Add(30 * time.Second)
	bindings := localBindings(localEffective(1, 2, 1, 2, 1, 2))
	request := RedisConcurrencyAcquireRequest{LeaseID: "lease_protocol", Bindings: bindings}
	acquireCases := []struct {
		response any
		want     error
	}{
		{[]any{redisConcurrencyAcquireConflict}, ErrRedisConcurrencyLeaseConflict},
		{[]any{redisConcurrencyAcquireExpired}, ErrRedisConcurrencyLeaseExpired},
		{[]any{redisConcurrencyAcquireCorrupt, int64(0)}, ErrRedisConcurrencyProtocol},
		{"bad", ErrRedisConcurrencyProtocol},
		{[]any{}, ErrRedisConcurrencyProtocol},
		{[]any{"bad code"}, ErrRedisConcurrencyProtocol},
		{[]any{int64(99)}, ErrRedisConcurrencyProtocol},
		{[]any{redisConcurrencyAcquireAllowed, serverTime.UnixMilli()}, ErrRedisConcurrencyProtocol},
		{[]any{
			redisConcurrencyAcquireDenied, int64(1), int64(1), int64(2),
			expires.UnixMilli(), serverTime.UnixMilli(),
		}, ErrRedisConcurrencyProtocol},
		{[]any{
			redisConcurrencyAcquireDenied, int64(1), int64(2), int64(3),
			expires.UnixMilli(), serverTime.UnixMilli(),
		}, ErrRedisConcurrencyProtocol},
	}
	for index, test := range acquireCases {
		evaluator := &scriptedRedisEvaluator{responses: []scriptedRedisResponse{{value: test.response}}}
		_, err := mustRedisConcurrencyLimiter(t, evaluator).Acquire(context.Background(), request)
		if !errors.Is(err, test.want) {
			t.Fatalf("Acquire(protocol %d) error = %v, want %v", index, err, test.want)
		}
	}

	handle := RedisConcurrencyHandle{LeaseID: request.LeaseID, Scopes: localScopes()}
	for _, operation := range []struct {
		name string
		run  func(*RedisConcurrencyLimiter) error
	}{
		{"renew", func(selected *RedisConcurrencyLimiter) error {
			_, err := selected.Renew(context.Background(), handle)
			return err
		}},
		{"release", func(selected *RedisConcurrencyLimiter) error {
			_, err := selected.Release(context.Background(), handle)
			return err
		}},
	} {
		cases := []struct {
			response any
			want     error
		}{
			{[]any{redisConcurrencyLifecycleMissing}, ErrRedisConcurrencyLeaseExpired},
			{[]any{redisConcurrencyLifecycleConflict}, ErrRedisConcurrencyLeaseConflict},
			{[]any{redisConcurrencyLifecycleCorrupt, int64(2)}, ErrRedisConcurrencyProtocol},
			{"bad", ErrRedisConcurrencyProtocol},
			{[]any{}, ErrRedisConcurrencyProtocol},
			{[]any{int64(99)}, ErrRedisConcurrencyProtocol},
		}
		for index, test := range cases {
			evaluator := &scriptedRedisEvaluator{responses: []scriptedRedisResponse{{value: test.response}}}
			if err := operation.run(mustRedisConcurrencyLimiter(t, evaluator)); !errors.Is(err, test.want) {
				t.Fatalf("%s(protocol %d) error = %v, want %v", operation.name, index, err, test.want)
			}
		}
	}
	if !strings.Contains(redisConcurrencyCorruptError(-1).Error(), "metadata") ||
		!strings.Contains(redisConcurrencyCorruptError(3).Error(), "key") {
		t.Fatal("corruption errors do not identify metadata/scope")
	}
}

func mustRedisConcurrencyLimiter(
	t *testing.T,
	evaluator RedisEvaluator,
) *RedisConcurrencyLimiter {
	t.Helper()
	limiter, err := NewRedisConcurrencyLimiter(evaluator, DefaultRedisConcurrencyOptions())
	if err != nil {
		t.Fatalf("NewRedisConcurrencyLimiter() error = %v", err)
	}
	return limiter
}
