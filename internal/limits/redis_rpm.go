package limits

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/zse04152005-del/ai-gateway-platform/internal/limitpolicy"
)

const (
	defaultRedisRPMKeyPrefix      = "agw:limits:rpm:v1:{rpm}"
	defaultRedisRPMRetention      = 5 * time.Second
	defaultRedisRPMClockRetries   = 3
	maximumRedisRPMClockRetries   = 5
	maximumRedisRPMPrefixLength   = 128
	redisRPMResultDenied          = 0
	redisRPMResultAllowed         = 1
	redisRPMResultWindowMismatch  = 2
	redisRPMResultCorrupt         = 3
	redisRPMMismatchResultLength  = 4
	redisRPMDeniedResultLength    = 7
	redisRPMAllowedResultLength   = 8
	redisRPMCorruptResultLength   = 2
	redisRPMArgumentsBeforeScopes = 2
	redisRPMArgumentsPerScope     = 2
	redisRPMAllowedCountsOffset   = 4
	redisRPMWindowSeconds         = int64(60)
	redisRPMMillisecondsPerSecond = int64(1_000)
	redisRPMMicrosecondsPerSecond = int64(1_000_000)
	redisRPMMicrosecondsPerMillis = int64(1_000)
)

var (
	// ErrRedisRPMProtocol means Redis returned a shape or value the limiter
	// cannot safely interpret.
	ErrRedisRPMProtocol = errors.New("redis RPM response is invalid")
	// ErrRedisRPMClockBoundary means every bounded retry crossed a Redis minute
	// boundary before a counter mutation could occur.
	ErrRedisRPMClockBoundary = errors.New("redis RPM clock boundary did not stabilize")

	redisRPMPrefixPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._{}-]*$`)
)

// RedisRPMOptions defines key retention and bounded server-clock retries.
type RedisRPMOptions struct {
	KeyPrefix    string
	Retention    time.Duration
	ClockRetries int
}

// DefaultRedisRPMOptions returns the versioned production key namespace.
func DefaultRedisRPMOptions() RedisRPMOptions {
	return RedisRPMOptions{
		KeyPrefix: defaultRedisRPMKeyPrefix, Retention: defaultRedisRPMRetention,
		ClockRetries: defaultRedisRPMClockRetries,
	}
}

// RedisEvaluator is the minimal infrastructure port needed by the Lua limiter.
type RedisEvaluator interface {
	Eval(ctx context.Context, script string, keys []string, args ...any) (any, error)
}

// GoRedisEvaluator adapts a go-redis client without exposing commands to the
// limiter's policy logic.
type GoRedisEvaluator struct {
	client redis.Scripter
}

// NewGoRedisEvaluator validates a go-redis script-capable client.
func NewGoRedisEvaluator(client redis.Scripter) (*GoRedisEvaluator, error) {
	if client == nil {
		return nil, ErrInvalid
	}
	return &GoRedisEvaluator{client: client}, nil
}

// Eval runs one script and returns the decoded RESP value.
func (evaluator *GoRedisEvaluator) Eval(
	ctx context.Context,
	script string,
	keys []string,
	args ...any,
) (any, error) {
	if evaluator == nil || evaluator.client == nil || ctx == nil {
		return nil, ErrInvalid
	}
	return evaluator.client.Eval(ctx, script, keys, args...).Result()
}

// RedisRPMCount reports one distributed counter after an allowed admission.
type RedisRPMCount struct {
	Scope        Scope
	Count        uint64
	Soft         uint64
	SoftExceeded bool
}

// RedisRPMRejection identifies the first canonical hard scope that denied.
type RedisRPMRejection struct {
	Scope      Scope
	Count      uint64
	Hard       uint64
	RetryAfter time.Duration
	ResetAt    time.Time
}

// RedisRPMAdmission is the all-or-nothing result of one four-scope Lua call.
type RedisRPMAdmission struct {
	Window     int64
	WindowKey  string
	ServerTime time.Time
	ResetAt    time.Time
	Counts     []RedisRPMCount
	Rejection  *RedisRPMRejection
}

// Allowed reports whether all four distributed counters were incremented.
func (admission RedisRPMAdmission) Allowed() bool {
	return admission.Rejection == nil && len(admission.Counts) == requiredScopeCount
}

// RedisRPMLimiter performs one atomic hard check and increment across the
// Platform, Tenant, Project and Key fields of a server-time minute Hash.
type RedisRPMLimiter struct {
	evaluator    RedisEvaluator
	now          func() time.Time
	keyPrefix    string
	retentionMS  int64
	clockRetries int
}

// NewRedisRPMLimiter validates dependencies and immutable runtime options.
func NewRedisRPMLimiter(
	evaluator RedisEvaluator,
	options RedisRPMOptions,
	now func() time.Time,
) (*RedisRPMLimiter, error) {
	retentionMilliseconds := options.Retention.Milliseconds()
	if evaluator == nil || now == nil || !validRedisRPMKeyPrefix(options.KeyPrefix) ||
		retentionMilliseconds < 1 || options.Retention > time.Minute ||
		options.ClockRetries < 1 || options.ClockRetries > maximumRedisRPMClockRetries {
		return nil, ErrInvalid
	}
	return &RedisRPMLimiter{
		evaluator: evaluator, now: now, keyPrefix: options.KeyPrefix,
		retentionMS: retentionMilliseconds, clockRetries: options.ClockRetries,
	}, nil
}

// AcquireRPM uses Redis TIME as the final window authority. A local/server
// mismatch returns before any write and is retried with the server window.
func (limiter *RedisRPMLimiter) AcquireRPM(
	ctx context.Context,
	bindings []Binding,
) (RedisRPMAdmission, error) {
	if limiter == nil || ctx == nil {
		return RedisRPMAdmission{}, ErrInvalid
	}
	scopes, policies, err := normalizeRPMBindings(bindings)
	if err != nil {
		return RedisRPMAdmission{}, err
	}
	window := limiter.now().UTC().Unix() / redisRPMWindowSeconds
	for range limiter.clockRetries {
		key := limiter.windowKey(window)
		arguments := redisRPMArguments(window, limiter.retentionMS, scopes, policies)
		raw, evalErr := limiter.evaluator.Eval(ctx, redisRPMAdmissionScript, []string{key}, arguments...)
		if evalErr != nil {
			return RedisRPMAdmission{}, fmt.Errorf("evaluate Redis RPM script: %w", evalErr)
		}
		response, responseErr := redisRPMResponse(raw)
		if responseErr != nil {
			return RedisRPMAdmission{}, responseErr
		}
		switch response.code {
		case redisRPMResultWindowMismatch:
			window = response.window
			continue
		case redisRPMResultDenied:
			return deniedRedisRPMAdmission(key, scopes, policies, response)
		case redisRPMResultAllowed:
			return allowedRedisRPMAdmission(key, scopes, policies, response)
		case redisRPMResultCorrupt:
			return RedisRPMAdmission{}, fmt.Errorf(
				"%w: corrupt %s scope counter",
				ErrRedisRPMProtocol,
				scopes[response.scopeIndex].Kind,
			)
		default:
			return RedisRPMAdmission{}, ErrRedisRPMProtocol
		}
	}
	return RedisRPMAdmission{}, ErrRedisRPMClockBoundary
}

type parsedRedisRPMResponse struct {
	code       int64
	window     int64
	nowMS      int64
	resetMS    int64
	scopeIndex int
	count      uint64
	hard       uint64
	counts     [requiredScopeCount]uint64
}

func normalizeRPMBindings(
	bindings []Binding,
) ([requiredScopeCount]Scope, [requiredScopeCount]limitpolicy.Effective, error) {
	var policies [requiredScopeCount]limitpolicy.Effective
	if len(bindings) != requiredScopeCount {
		return [requiredScopeCount]Scope{}, policies, ErrInvalid
	}
	scopeInput := make([]Scope, 0, requiredScopeCount)
	policyByScope := make(map[Scope]limitpolicy.Effective, requiredScopeCount)
	for _, binding := range bindings {
		if binding.Scope.Validate() != nil || binding.Policy.Validate() != nil {
			return [requiredScopeCount]Scope{}, policies, ErrInvalid
		}
		if _, duplicate := policyByScope[binding.Scope]; duplicate {
			return [requiredScopeCount]Scope{}, policies, ErrInvalid
		}
		scopeInput = append(scopeInput, binding.Scope)
		policyByScope[binding.Scope] = binding.Policy
	}
	scopes, err := normalizeScopes(scopeInput)
	if err != nil {
		return [requiredScopeCount]Scope{}, policies, err
	}
	for index, scope := range scopes {
		policies[index] = policyByScope[scope]
	}
	return scopes, policies, nil
}

func redisRPMArguments(
	window int64,
	retentionMilliseconds int64,
	scopes [requiredScopeCount]Scope,
	policies [requiredScopeCount]limitpolicy.Effective,
) []any {
	arguments := make([]any, 0, redisRPMArgumentsBeforeScopes+requiredScopeCount*redisRPMArgumentsPerScope)
	arguments = append(arguments, window, retentionMilliseconds)
	for index, scope := range scopes {
		arguments = append(arguments, redisRPMField(scope), policies[index].RPM.Hard)
	}
	return arguments
}

func redisRPMResponse(raw any) (parsedRedisRPMResponse, error) {
	values, ok := raw.([]any)
	if !ok || len(values) == 0 {
		return parsedRedisRPMResponse{}, ErrRedisRPMProtocol
	}
	code, ok := redisRPMInt64(values[0])
	if !ok {
		return parsedRedisRPMResponse{}, ErrRedisRPMProtocol
	}
	response := parsedRedisRPMResponse{code: code}
	switch code {
	case redisRPMResultWindowMismatch:
		if len(values) != redisRPMMismatchResultLength ||
			!assignRedisRPMTimeFields(values[1], values[2], values[3], &response) {
			return parsedRedisRPMResponse{}, ErrRedisRPMProtocol
		}
	case redisRPMResultDenied:
		if len(values) != redisRPMDeniedResultLength || !assignRedisRPMDenial(values, &response) {
			return parsedRedisRPMResponse{}, ErrRedisRPMProtocol
		}
	case redisRPMResultAllowed:
		if len(values) != redisRPMAllowedResultLength ||
			!assignRedisRPMTimeFields(values[1], values[2], values[3], &response) {
			return parsedRedisRPMResponse{}, ErrRedisRPMProtocol
		}
		for index := range requiredScopeCount {
			count, countOK := redisRPMUint64(values[redisRPMAllowedCountsOffset+index])
			if !countOK || count == 0 || count > limitpolicy.MaximumValue {
				return parsedRedisRPMResponse{}, ErrRedisRPMProtocol
			}
			response.counts[index] = count
		}
	case redisRPMResultCorrupt:
		if len(values) != redisRPMCorruptResultLength {
			return parsedRedisRPMResponse{}, ErrRedisRPMProtocol
		}
		index, indexOK := redisRPMInt64(values[1])
		if !indexOK || index < 1 || index > requiredScopeCount {
			return parsedRedisRPMResponse{}, ErrRedisRPMProtocol
		}
		response.scopeIndex = int(index - 1)
	default:
		return parsedRedisRPMResponse{}, ErrRedisRPMProtocol
	}
	return response, nil
}

func assignRedisRPMTimeFields(
	windowValue any,
	nowValue any,
	resetValue any,
	response *parsedRedisRPMResponse,
) bool {
	window, windowOK := redisRPMInt64(windowValue)
	nowMilliseconds, nowOK := redisRPMInt64(nowValue)
	resetMilliseconds, resetOK := redisRPMInt64(resetValue)
	if !windowOK || !nowOK || !resetOK || window < 0 || nowMilliseconds < 0 ||
		resetMilliseconds <= nowMilliseconds {
		return false
	}
	response.window = window
	response.nowMS = nowMilliseconds
	response.resetMS = resetMilliseconds
	return true
}

func assignRedisRPMDenial(values []any, response *parsedRedisRPMResponse) bool {
	index, indexOK := redisRPMInt64(values[1])
	count, countOK := redisRPMUint64(values[2])
	hard, hardOK := redisRPMUint64(values[3])
	if !indexOK || index < 1 || index > requiredScopeCount || !countOK || !hardOK ||
		hard == 0 || hard > limitpolicy.MaximumValue || count < hard {
		return false
	}
	if !assignRedisRPMTimeFields(values[4], values[5], values[6], response) {
		return false
	}
	response.scopeIndex = int(index - 1)
	response.count = count
	response.hard = hard
	return true
}

func deniedRedisRPMAdmission(
	key string,
	scopes [requiredScopeCount]Scope,
	policies [requiredScopeCount]limitpolicy.Effective,
	response parsedRedisRPMResponse,
) (RedisRPMAdmission, error) {
	if response.hard != policies[response.scopeIndex].RPM.Hard {
		return RedisRPMAdmission{}, ErrRedisRPMProtocol
	}
	serverTime := time.UnixMilli(response.nowMS).UTC()
	resetAt := time.UnixMilli(response.resetMS).UTC()
	retryAfter := resetAt.Sub(serverTime)
	if retryAfter < 0 {
		retryAfter = 0
	}
	return RedisRPMAdmission{
		Window: response.window, WindowKey: key, ServerTime: serverTime, ResetAt: resetAt,
		Rejection: &RedisRPMRejection{
			Scope: scopes[response.scopeIndex], Count: response.count, Hard: response.hard,
			RetryAfter: retryAfter, ResetAt: resetAt,
		},
	}, nil
}

func allowedRedisRPMAdmission(
	key string,
	scopes [requiredScopeCount]Scope,
	policies [requiredScopeCount]limitpolicy.Effective,
	response parsedRedisRPMResponse,
) (RedisRPMAdmission, error) {
	counts := make([]RedisRPMCount, 0, requiredScopeCount)
	for index, count := range response.counts {
		if count > policies[index].RPM.Hard {
			return RedisRPMAdmission{}, ErrRedisRPMProtocol
		}
		counts = append(counts, RedisRPMCount{
			Scope: scopes[index], Count: count, Soft: policies[index].RPM.Soft,
			SoftExceeded: count > policies[index].RPM.Soft,
		})
	}
	return RedisRPMAdmission{
		Window: response.window, WindowKey: key,
		ServerTime: time.UnixMilli(response.nowMS).UTC(),
		ResetAt:    time.UnixMilli(response.resetMS).UTC(), Counts: counts,
	}, nil
}

func (limiter *RedisRPMLimiter) windowKey(window int64) string {
	return limiter.keyPrefix + ":" + strconv.FormatInt(window, 10)
}

func redisRPMField(scope Scope) string {
	switch scope.Kind {
	case ScopePlatform:
		return "platform"
	case ScopeTenant:
		return "tenant:" + scope.TenantID
	case ScopeProject:
		return "project:" + scope.TenantID + ":" + scope.ProjectID
	case ScopeKey:
		return "key:" + scope.TenantID + ":" + scope.ProjectID + ":" + scope.KeyID
	default:
		return ""
	}
}

func validRedisRPMKeyPrefix(prefix string) bool {
	return len(prefix) >= 1 && len(prefix) <= maximumRedisRPMPrefixLength &&
		redisRPMPrefixPattern.MatchString(prefix) && strings.Count(prefix, "{rpm}") == 1
}

func redisRPMInt64(value any) (int64, bool) {
	integer, ok := value.(int64)
	return integer, ok
}

func redisRPMUint64(value any) (uint64, bool) {
	integer, ok := redisRPMInt64(value)
	if !ok || integer < 0 {
		return 0, false
	}
	return uint64(integer), true
}

const redisRPMAdmissionScript = `
local expected_window = tonumber(ARGV[1])
local retention_ms = tonumber(ARGV[2])
local redis_time = redis.call('TIME')
local seconds = tonumber(redis_time[1])
local microseconds = tonumber(redis_time[2])
local now_ms = seconds * 1000 + math.floor(microseconds / 1000)
local server_window = math.floor(seconds / 60)
local reset_ms = (server_window + 1) * 60 * 1000

if server_window ~= expected_window then
    return {2, server_window, now_ms, reset_ms}
end

local maximum = 9007199254740991
local fields = {}
local hard_limits = {}
local counts = {}

for index = 1, 4 do
    local argument = 3 + (index - 1) * 2
    local field = ARGV[argument]
    local hard = tonumber(ARGV[argument + 1])
    local raw = redis.call('HGET', KEYS[1], field)
    local count = 0
    if raw then
        if not string.match(raw, '^%d+$') then
            return {3, index}
        end
        count = tonumber(raw)
        if not count or count < 0 or count > maximum or count % 1 ~= 0 then
            return {3, index}
        end
    end
    fields[index] = field
    hard_limits[index] = hard
    counts[index] = count
end

for index = 1, 4 do
    if counts[index] + 1 > hard_limits[index] then
        return {0, index, counts[index], hard_limits[index], server_window, now_ms, reset_ms}
    end
end

for index = 1, 4 do
    counts[index] = redis.call('HINCRBY', KEYS[1], fields[index], 1)
end

redis.call('PEXPIREAT', KEYS[1], reset_ms + retention_ms)
return {1, server_window, now_ms, reset_ms, counts[1], counts[2], counts[3], counts[4]}
`
