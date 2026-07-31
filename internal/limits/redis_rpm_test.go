package limits

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRedisRPMLimiterRetriesServerWindowAndReturnsSoftFacts(t *testing.T) {
	localTime := time.Date(2026, time.July, 31, 10, 59, 59, 0, time.UTC)
	serverTime := time.Date(2026, time.July, 31, 11, 0, 1, 250_000_000, time.UTC)
	serverWindow := serverTime.Unix() / redisRPMWindowSeconds
	resetAt := time.Date(2026, time.July, 31, 11, 1, 0, 0, time.UTC)
	evaluator := &scriptedRedisEvaluator{responses: []scriptedRedisResponse{
		{value: []any{
			int64(redisRPMResultWindowMismatch), serverWindow,
			serverTime.UnixMilli(), resetAt.UnixMilli(),
		}},
		{value: []any{
			int64(redisRPMResultAllowed), serverWindow,
			serverTime.UnixMilli(), resetAt.UnixMilli(),
			int64(1), int64(2), int64(3), int64(4),
		}},
	}}
	options := DefaultRedisRPMOptions()
	options.KeyPrefix = "agw:test:p09t03:{rpm}"
	limiter, err := NewRedisRPMLimiter(evaluator, options, func() time.Time { return localTime })
	if err != nil {
		t.Fatalf("NewRedisRPMLimiter() error = %v", err)
	}
	policy := localEffective(2, 5, 10, 20, 2, 5)
	bindings := localBindings(policy)
	for left, right := 0, len(bindings)-1; left < right; left, right = left+1, right-1 {
		bindings[left], bindings[right] = bindings[right], bindings[left]
	}

	admission, err := limiter.AcquireRPM(context.Background(), bindings)
	if err != nil || !admission.Allowed() {
		t.Fatalf("AcquireRPM() = %+v, %v", admission, err)
	}
	if admission.Window != serverWindow ||
		admission.WindowKey != options.KeyPrefix+":"+strconv.FormatInt(serverWindow, 10) ||
		!admission.ServerTime.Equal(serverTime) || !admission.ResetAt.Equal(resetAt) ||
		len(admission.Counts) != requiredScopeCount {
		t.Fatalf("admission metadata = %+v", admission)
	}
	for index, count := range admission.Counts {
		wantCount := uint64(index + 1)
		if count.Scope.Kind != localScopes()[index].Kind || count.Count != wantCount || count.Soft != 2 ||
			count.SoftExceeded != (wantCount > 2) {
			t.Fatalf("count[%d] = %+v", index, count)
		}
	}
	if len(evaluator.calls) != 2 {
		t.Fatalf("Eval call count = %d, want 2", len(evaluator.calls))
	}
	localWindow := localTime.Unix() / redisRPMWindowSeconds
	if evaluator.calls[0].keys[0] != options.KeyPrefix+":"+strconv.FormatInt(localWindow, 10) ||
		evaluator.calls[1].keys[0] != options.KeyPrefix+":"+strconv.FormatInt(serverWindow, 10) {
		t.Fatalf("window keys = %#v / %#v", evaluator.calls[0].keys, evaluator.calls[1].keys)
	}
	if evaluator.calls[1].script != redisRPMAdmissionScript || len(evaluator.calls[1].arguments) != 10 ||
		evaluator.calls[1].arguments[0] != serverWindow ||
		evaluator.calls[1].arguments[2] != "platform" ||
		evaluator.calls[1].arguments[4] != "tenant:"+localTenantID ||
		evaluator.calls[1].arguments[6] != "project:"+localTenantID+":"+localProjectID ||
		evaluator.calls[1].arguments[8] != "key:"+localTenantID+":"+localProjectID+":"+localKeyID {
		t.Fatalf("second Eval call = %+v", evaluator.calls[1])
	}
}

func TestRedisRPMLimiterHardRejectionIsDeterministic(t *testing.T) {
	serverTime := time.Date(2026, time.July, 31, 11, 5, 2, 0, time.UTC)
	serverWindow := serverTime.Unix() / redisRPMWindowSeconds
	resetAt := time.Date(2026, time.July, 31, 11, 6, 0, 0, time.UTC)
	evaluator := &scriptedRedisEvaluator{responses: []scriptedRedisResponse{{value: []any{
		int64(redisRPMResultDenied), int64(3), int64(3), int64(3),
		serverWindow, serverTime.UnixMilli(), resetAt.UnixMilli(),
	}}}}
	options := DefaultRedisRPMOptions()
	options.KeyPrefix = "agw:test:denied:{rpm}"
	limiter, err := NewRedisRPMLimiter(evaluator, options, func() time.Time { return serverTime })
	if err != nil {
		t.Fatalf("NewRedisRPMLimiter() error = %v", err)
	}
	bindings := localBindings(localEffective(2, 5, 10, 20, 2, 5))
	bindings[2].Policy = localEffective(2, 3, 10, 20, 2, 5)

	admission, err := limiter.AcquireRPM(context.Background(), bindings)
	if err != nil || admission.Allowed() || admission.Rejection == nil {
		t.Fatalf("AcquireRPM() = %+v, %v", admission, err)
	}
	if admission.Rejection.Scope.Kind != ScopeProject || admission.Rejection.Count != 3 ||
		admission.Rejection.Hard != 3 || admission.Rejection.RetryAfter != 58*time.Second ||
		!admission.Rejection.ResetAt.Equal(resetAt) || len(admission.Counts) != 0 {
		t.Fatalf("rejection = %+v", admission.Rejection)
	}
}

func TestRedisRPMLimiterValidationAndProtocolFailures(t *testing.T) {
	clock := func() time.Time { return time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC) }
	validEvaluator := &scriptedRedisEvaluator{}
	validOptions := DefaultRedisRPMOptions()
	invalidOptions := []RedisRPMOptions{
		{},
		{KeyPrefix: "bad prefix {rpm}", Retention: time.Second, ClockRetries: 1},
		{KeyPrefix: "agw:no-tag", Retention: time.Second, ClockRetries: 1},
		{KeyPrefix: "agw:{rpm}", Retention: 0, ClockRetries: 1},
		{KeyPrefix: "agw:{rpm}", Retention: time.Minute + time.Millisecond, ClockRetries: 1},
		{KeyPrefix: "agw:{rpm}", Retention: time.Second, ClockRetries: 0},
		{KeyPrefix: "agw:{rpm}", Retention: time.Second, ClockRetries: maximumRedisRPMClockRetries + 1},
	}
	for index, options := range invalidOptions {
		if _, err := NewRedisRPMLimiter(validEvaluator, options, clock); !errors.Is(err, ErrInvalid) {
			t.Fatalf("NewRedisRPMLimiter(invalid[%d]) error = %v", index, err)
		}
	}
	if _, err := NewRedisRPMLimiter(nil, validOptions, clock); !errors.Is(err, ErrInvalid) {
		t.Fatalf("NewRedisRPMLimiter(nil evaluator) error = %v", err)
	}
	if _, err := NewRedisRPMLimiter(validEvaluator, validOptions, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("NewRedisRPMLimiter(nil clock) error = %v", err)
	}
	if _, err := NewGoRedisEvaluator(nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("NewGoRedisEvaluator(nil) error = %v", err)
	}
	if _, err := (*GoRedisEvaluator)(nil).Eval(context.Background(), "return 1", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil GoRedisEvaluator.Eval() error = %v", err)
	}

	limiter, err := NewRedisRPMLimiter(validEvaluator, validOptions, clock)
	if err != nil {
		t.Fatalf("NewRedisRPMLimiter(valid) error = %v", err)
	}
	if _, err := (*RedisRPMLimiter)(nil).AcquireRPM(context.Background(), localBindings(
		localEffective(1, 2, 1, 2, 1, 2),
	)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil RedisRPMLimiter.AcquireRPM() error = %v", err)
	}
	var nilContext context.Context
	if _, err := limiter.AcquireRPM(nilContext, localBindings(localEffective(1, 2, 1, 2, 1, 2))); !errors.Is(err, ErrInvalid) {
		t.Fatalf("AcquireRPM(nil context) error = %v", err)
	}
	invalidBindings := [][]Binding{
		nil,
		localBindings(localEffective(1, 2, 1, 2, 1, 2))[:3],
		append(
			localBindings(localEffective(1, 2, 1, 2, 1, 2))[:3],
			localBindings(localEffective(1, 2, 1, 2, 1, 2))[0],
		),
		localBindings(localEffective(2, 1, 1, 2, 1, 2)),
	}
	for index, bindings := range invalidBindings {
		if _, err := limiter.AcquireRPM(context.Background(), bindings); !errors.Is(err, ErrInvalid) {
			t.Fatalf("AcquireRPM(invalid bindings %d) error = %v", index, err)
		}
	}

	transportError := errors.New("Redis unavailable")
	failing := &scriptedRedisEvaluator{responses: []scriptedRedisResponse{{err: transportError}}}
	failingLimiter, err := NewRedisRPMLimiter(failing, validOptions, clock)
	if err != nil {
		t.Fatalf("NewRedisRPMLimiter(failing) error = %v", err)
	}
	if _, err := failingLimiter.AcquireRPM(
		context.Background(), localBindings(localEffective(1, 2, 1, 2, 1, 2)),
	); !errors.Is(err, transportError) {
		t.Fatalf("AcquireRPM(transport error) = %v", err)
	}
}

func TestRedisRPMLimiterBoundsClockRetriesAndRejectsCorruption(t *testing.T) {
	serverTime := time.Date(2026, time.July, 31, 12, 15, 1, 0, time.UTC)
	serverWindow := serverTime.Unix() / redisRPMWindowSeconds
	resetAt := time.Date(2026, time.July, 31, 12, 16, 0, 0, time.UTC)
	mismatch := scriptedRedisResponse{value: []any{
		int64(redisRPMResultWindowMismatch), serverWindow, serverTime.UnixMilli(), resetAt.UnixMilli(),
	}}
	evaluator := &scriptedRedisEvaluator{responses: []scriptedRedisResponse{mismatch, mismatch, mismatch}}
	options := DefaultRedisRPMOptions()
	limiter, err := NewRedisRPMLimiter(evaluator, options, func() time.Time { return serverTime })
	if err != nil {
		t.Fatalf("NewRedisRPMLimiter() error = %v", err)
	}
	bindings := localBindings(localEffective(1, 2, 1, 2, 1, 2))
	if _, err := limiter.AcquireRPM(context.Background(), bindings); !errors.Is(err, ErrRedisRPMClockBoundary) {
		t.Fatalf("AcquireRPM(clock boundary) error = %v", err)
	}
	if len(evaluator.calls) != defaultRedisRPMClockRetries {
		t.Fatalf("Eval call count = %d, want %d", len(evaluator.calls), defaultRedisRPMClockRetries)
	}

	protocolResponses := []any{
		"not an array",
		[]any{},
		[]any{"bad code"},
		[]any{int64(redisRPMResultAllowed), serverWindow, serverTime.UnixMilli(), resetAt.UnixMilli()},
		[]any{
			int64(redisRPMResultAllowed), serverWindow, serverTime.UnixMilli(), resetAt.UnixMilli(),
			int64(1), int64(1), int64(1), int64(3),
		},
		[]any{
			int64(redisRPMResultDenied), int64(1), int64(2), int64(1),
			serverWindow, serverTime.UnixMilli(), resetAt.UnixMilli(),
		},
		[]any{int64(redisRPMResultCorrupt), int64(0)},
		[]any{int64(99)},
	}
	for index, response := range protocolResponses {
		protocolEvaluator := &scriptedRedisEvaluator{responses: []scriptedRedisResponse{{value: response}}}
		protocolLimiter, buildErr := NewRedisRPMLimiter(protocolEvaluator, options, func() time.Time { return serverTime })
		if buildErr != nil {
			t.Fatalf("NewRedisRPMLimiter(protocol %d) error = %v", index, buildErr)
		}
		if _, err := protocolLimiter.AcquireRPM(context.Background(), bindings); !errors.Is(err, ErrRedisRPMProtocol) {
			t.Fatalf("AcquireRPM(protocol %d) error = %v", index, err)
		}
	}

	corruptEvaluator := &scriptedRedisEvaluator{responses: []scriptedRedisResponse{{
		value: []any{int64(redisRPMResultCorrupt), int64(2)},
	}}}
	corruptLimiter, err := NewRedisRPMLimiter(corruptEvaluator, options, func() time.Time { return serverTime })
	if err != nil {
		t.Fatalf("NewRedisRPMLimiter(corrupt) error = %v", err)
	}
	_, err = corruptLimiter.AcquireRPM(context.Background(), bindings)
	if !errors.Is(err, ErrRedisRPMProtocol) || !strings.Contains(err.Error(), "tenant") {
		t.Fatalf("AcquireRPM(corrupt) error = %v", err)
	}
}

type scriptedRedisResponse struct {
	value any
	err   error
}

type scriptedRedisCall struct {
	script    string
	keys      []string
	arguments []any
}

type scriptedRedisEvaluator struct {
	responses []scriptedRedisResponse
	calls     []scriptedRedisCall
}

func (evaluator *scriptedRedisEvaluator) Eval(
	_ context.Context,
	script string,
	keys []string,
	args ...any,
) (any, error) {
	evaluator.calls = append(evaluator.calls, scriptedRedisCall{
		script: script, keys: append([]string(nil), keys...), arguments: append([]any(nil), args...),
	})
	if len(evaluator.responses) == 0 {
		return nil, errors.New("unexpected Redis Eval call")
	}
	response := evaluator.responses[0]
	evaluator.responses = evaluator.responses[1:]
	return response.value, response.err
}
