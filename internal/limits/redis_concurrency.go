package limits

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/limitpolicy"
)

const (
	defaultRedisConcurrencyKeyPrefix    = "agw:limits:concurrency:v1:{concurrency}"
	defaultRedisConcurrencyLease        = 30 * time.Second
	defaultRedisConcurrencyRetention    = 5 * time.Minute
	minimumRedisConcurrencyLease        = 100 * time.Millisecond
	maximumRedisConcurrencyLease        = 10 * time.Minute
	maximumRedisConcurrencyRetention    = time.Hour
	maximumRedisConcurrencyPrefixLength = 128
	maximumRedisConcurrencyLeaseIDBytes = 128

	redisConcurrencyAcquireDenied   = int64(0)
	redisConcurrencyAcquireAllowed  = int64(1)
	redisConcurrencyAcquireConflict = int64(2)
	redisConcurrencyAcquireCorrupt  = int64(3)
	redisConcurrencyAcquireExpired  = int64(4)
	redisConcurrencyAllowedLength   = 8
	redisConcurrencyDeniedLength    = 6
	redisConcurrencyCorruptLength   = 2
	redisConcurrencyCountsOffset    = 4

	redisConcurrencyLifecycleSucceeded = int64(0)
	redisConcurrencyLifecycleMissing   = int64(1)
	redisConcurrencyLifecycleConflict  = int64(2)
	redisConcurrencyLifecycleCorrupt   = int64(3)
	redisConcurrencyReleaseLength      = 9
	redisConcurrencyRenewLength        = 7
)

var (
	// ErrRedisConcurrencyProtocol means Redis returned corrupt lease state or
	// an unsafe protocol shape.
	ErrRedisConcurrencyProtocol = errors.New("redis concurrency response is invalid")
	// ErrRedisConcurrencyLeaseConflict means one ID was reused with another
	// hierarchy or after terminal release.
	ErrRedisConcurrencyLeaseConflict = errors.New("redis concurrency lease conflicts with existing state")
	// ErrRedisConcurrencyLeaseExpired means the lease no longer owns capacity.
	ErrRedisConcurrencyLeaseExpired = errors.New("redis concurrency lease is missing or expired")

	redisConcurrencyPrefixPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._{}-]*$`)
	redisConcurrencyIDPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
)

// RedisConcurrencyOptions bounds lease duration and terminal idempotency
// retention. Heartbeats must renew before LeaseDuration elapses.
type RedisConcurrencyOptions struct {
	KeyPrefix     string
	LeaseDuration time.Duration
	Retention     time.Duration
}

// DefaultRedisConcurrencyOptions returns a 30-second lease with five minutes
// of released/expired metadata retention.
func DefaultRedisConcurrencyOptions() RedisConcurrencyOptions {
	return RedisConcurrencyOptions{
		KeyPrefix:     defaultRedisConcurrencyKeyPrefix,
		LeaseDuration: defaultRedisConcurrencyLease, Retention: defaultRedisConcurrencyRetention,
	}
}

// RedisConcurrencyAcquireRequest identifies one four-scope distributed lease.
type RedisConcurrencyAcquireRequest struct {
	LeaseID  string
	Bindings []Binding
}

// RedisConcurrencyCount reports one scope count after admission.
type RedisConcurrencyCount struct {
	Scope        Scope
	Count        uint64
	Soft         uint64
	SoftExceeded bool
}

// RedisConcurrencyUsage reports a scope count after renew or release.
type RedisConcurrencyUsage struct {
	Scope Scope
	Count uint64
}

// RedisConcurrencyRejection identifies the first canonical full scope. The
// earliest live lease supplies a real retry bound rather than a guessed TTL.
type RedisConcurrencyRejection struct {
	Scope          Scope
	Count          uint64
	Hard           uint64
	RetryAfter     time.Duration
	EarliestExpiry time.Time
}

// RedisConcurrencyHandle binds lifecycle operations to the acquired hierarchy.
type RedisConcurrencyHandle struct {
	LeaseID string
	Scopes  []Scope
}

// RedisConcurrencyAdmission is one all-or-nothing distributed acquisition.
type RedisConcurrencyAdmission struct {
	Handle     RedisConcurrencyHandle
	ServerTime time.Time
	ExpiresAt  time.Time
	Counts     []RedisConcurrencyCount
	Idempotent bool
	Rejection  *RedisConcurrencyRejection
}

// Allowed reports whether all four scopes contain the lease.
func (admission RedisConcurrencyAdmission) Allowed() bool {
	return admission.Rejection == nil && admission.Handle.LeaseID != "" &&
		len(admission.Counts) == requiredScopeCount
}

// RedisConcurrencyRenewal reports a server-time heartbeat extension.
type RedisConcurrencyRenewal struct {
	Handle     RedisConcurrencyHandle
	ServerTime time.Time
	ExpiresAt  time.Time
	Usage      []RedisConcurrencyUsage
}

// RedisConcurrencyRelease reports explicit terminal cleanup. Expired=true
// means the slot had already timed out and was reclaimed during this call.
type RedisConcurrencyRelease struct {
	Handle     RedisConcurrencyHandle
	ServerTime time.Time
	ExpiresAt  time.Time
	Usage      []RedisConcurrencyUsage
	Idempotent bool
	Expired    bool
}

// RedisConcurrencyLimiter stores one lease member in four same-slot ZSETs.
// Lua serializes cleanup, hard checks and lifecycle mutations.
type RedisConcurrencyLimiter struct {
	evaluator RedisEvaluator
	keyPrefix string
	leaseMS   int64
	retainMS  int64
}

// NewRedisConcurrencyLimiter validates dependencies and immutable lease bounds.
func NewRedisConcurrencyLimiter(
	evaluator RedisEvaluator,
	options RedisConcurrencyOptions,
) (*RedisConcurrencyLimiter, error) {
	leaseMilliseconds := options.LeaseDuration.Milliseconds()
	retentionMilliseconds := options.Retention.Milliseconds()
	if evaluator == nil || !validRedisConcurrencyPrefix(options.KeyPrefix) ||
		options.LeaseDuration < minimumRedisConcurrencyLease ||
		options.LeaseDuration > maximumRedisConcurrencyLease ||
		retentionMilliseconds < 1 || options.Retention > maximumRedisConcurrencyRetention {
		return nil, ErrInvalid
	}
	return &RedisConcurrencyLimiter{
		evaluator: evaluator, keyPrefix: options.KeyPrefix,
		leaseMS: leaseMilliseconds, retainMS: retentionMilliseconds,
	}, nil
}

// Acquire atomically removes expired members, checks every hard limit and adds
// the lease to all four ZSETs. Exact active retries are idempotent.
func (limiter *RedisConcurrencyLimiter) Acquire(
	ctx context.Context,
	request RedisConcurrencyAcquireRequest,
) (RedisConcurrencyAdmission, error) {
	if limiter == nil || ctx == nil || !validRedisConcurrencyLeaseID(request.LeaseID) {
		return RedisConcurrencyAdmission{}, ErrInvalid
	}
	scopes, policies, err := normalizeRPMBindings(request.Bindings)
	if err != nil {
		return RedisConcurrencyAdmission{}, err
	}
	keys := limiter.keys(scopes, request.LeaseID)
	arguments := redisConcurrencyAcquireArguments(
		limiter.leaseMS, limiter.retainMS, request.LeaseID, redisScopeFingerprint(scopes), policies,
	)
	raw, evalErr := limiter.evaluator.Eval(ctx, redisConcurrencyAcquireScript, keys, arguments...)
	if evalErr != nil {
		return RedisConcurrencyAdmission{}, fmt.Errorf("evaluate Redis concurrency acquire script: %w", evalErr)
	}
	response, responseErr := parseRedisConcurrencyAcquire(raw)
	if responseErr != nil {
		return RedisConcurrencyAdmission{}, responseErr
	}
	switch response.code {
	case redisConcurrencyAcquireDenied:
		return deniedRedisConcurrencyAdmission(scopes, policies, response)
	case redisConcurrencyAcquireAllowed:
		return allowedRedisConcurrencyAdmission(request.LeaseID, scopes, policies, response)
	case redisConcurrencyAcquireConflict:
		return RedisConcurrencyAdmission{}, ErrRedisConcurrencyLeaseConflict
	case redisConcurrencyAcquireCorrupt:
		return RedisConcurrencyAdmission{}, redisConcurrencyCorruptError(response.scopeIndex)
	case redisConcurrencyAcquireExpired:
		return RedisConcurrencyAdmission{}, ErrRedisConcurrencyLeaseExpired
	default:
		return RedisConcurrencyAdmission{}, ErrRedisConcurrencyProtocol
	}
}

// Renew extends one active lease from Redis server time. An expired or released
// lease cannot be resurrected.
func (limiter *RedisConcurrencyLimiter) Renew(
	ctx context.Context,
	handle RedisConcurrencyHandle,
) (RedisConcurrencyRenewal, error) {
	if limiter == nil || ctx == nil {
		return RedisConcurrencyRenewal{}, ErrInvalid
	}
	scopes, err := validateRedisConcurrencyHandle(handle)
	if err != nil {
		return RedisConcurrencyRenewal{}, err
	}
	arguments := []any{
		limiter.leaseMS, limiter.retainMS, handle.LeaseID, redisScopeFingerprint(scopes),
	}
	raw, evalErr := limiter.evaluator.Eval(
		ctx, redisConcurrencyRenewScript, limiter.keys(scopes, handle.LeaseID), arguments...,
	)
	if evalErr != nil {
		return RedisConcurrencyRenewal{}, fmt.Errorf("evaluate Redis concurrency renew script: %w", evalErr)
	}
	response, responseErr := parseRedisConcurrencyRenew(raw)
	if responseErr != nil {
		return RedisConcurrencyRenewal{}, responseErr
	}
	switch response.code {
	case redisConcurrencyLifecycleSucceeded:
		return RedisConcurrencyRenewal{
			Handle:     cloneRedisConcurrencyHandle(handle),
			ServerTime: time.UnixMilli(response.nowMS).UTC(),
			ExpiresAt:  time.UnixMilli(response.expiresMS).UTC(),
			Usage:      redisConcurrencyUsage(scopes, response.counts),
		}, nil
	case redisConcurrencyLifecycleMissing:
		return RedisConcurrencyRenewal{}, ErrRedisConcurrencyLeaseExpired
	case redisConcurrencyLifecycleConflict:
		return RedisConcurrencyRenewal{}, ErrRedisConcurrencyLeaseConflict
	case redisConcurrencyLifecycleCorrupt:
		return RedisConcurrencyRenewal{}, redisConcurrencyCorruptError(response.scopeIndex)
	default:
		return RedisConcurrencyRenewal{}, ErrRedisConcurrencyProtocol
	}
}

// Release removes all four members for normal completion, failure or caller
// cancellation. Repeated terminal release is idempotent while metadata remains.
func (limiter *RedisConcurrencyLimiter) Release(
	ctx context.Context,
	handle RedisConcurrencyHandle,
) (RedisConcurrencyRelease, error) {
	if limiter == nil || ctx == nil {
		return RedisConcurrencyRelease{}, ErrInvalid
	}
	scopes, err := validateRedisConcurrencyHandle(handle)
	if err != nil {
		return RedisConcurrencyRelease{}, err
	}
	arguments := []any{handle.LeaseID, redisScopeFingerprint(scopes)}
	raw, evalErr := limiter.evaluator.Eval(
		ctx, redisConcurrencyReleaseScript, limiter.keys(scopes, handle.LeaseID), arguments...,
	)
	if evalErr != nil {
		return RedisConcurrencyRelease{}, fmt.Errorf("evaluate Redis concurrency release script: %w", evalErr)
	}
	response, responseErr := parseRedisConcurrencyRelease(raw)
	if responseErr != nil {
		return RedisConcurrencyRelease{}, responseErr
	}
	switch response.code {
	case redisConcurrencyLifecycleSucceeded:
		return RedisConcurrencyRelease{
			Handle:     cloneRedisConcurrencyHandle(handle),
			ServerTime: time.UnixMilli(response.nowMS).UTC(),
			ExpiresAt:  time.UnixMilli(response.expiresMS).UTC(),
			Usage:      redisConcurrencyUsage(scopes, response.counts),
			Idempotent: response.idempotent, Expired: response.expired,
		}, nil
	case redisConcurrencyLifecycleMissing:
		return RedisConcurrencyRelease{}, ErrRedisConcurrencyLeaseExpired
	case redisConcurrencyLifecycleConflict:
		return RedisConcurrencyRelease{}, ErrRedisConcurrencyLeaseConflict
	case redisConcurrencyLifecycleCorrupt:
		return RedisConcurrencyRelease{}, redisConcurrencyCorruptError(response.scopeIndex)
	default:
		return RedisConcurrencyRelease{}, ErrRedisConcurrencyProtocol
	}
}

type parsedRedisConcurrency struct {
	code       int64
	nowMS      int64
	expiresMS  int64
	earliestMS int64
	scopeIndex int
	count      uint64
	hard       uint64
	idempotent bool
	expired    bool
	counts     [requiredScopeCount]uint64
}

func redisConcurrencyAcquireArguments(
	leaseMS int64,
	retainMS int64,
	leaseID string,
	fingerprint string,
	policies [requiredScopeCount]limitpolicy.Effective,
) []any {
	arguments := make([]any, 0, 4+requiredScopeCount)
	arguments = append(arguments, leaseMS, retainMS, leaseID, fingerprint)
	for _, policy := range policies {
		arguments = append(arguments, policy.Concurrency.Hard)
	}
	return arguments
}

func parseRedisConcurrencyAcquire(raw any) (parsedRedisConcurrency, error) {
	values, ok := raw.([]any)
	if !ok || len(values) == 0 {
		return parsedRedisConcurrency{}, ErrRedisConcurrencyProtocol
	}
	code, ok := redisRPMInt64(values[0])
	if !ok {
		return parsedRedisConcurrency{}, ErrRedisConcurrencyProtocol
	}
	response := parsedRedisConcurrency{code: code}
	switch code {
	case redisConcurrencyAcquireDenied:
		if !assignRedisConcurrencyDenied(values, &response) {
			return parsedRedisConcurrency{}, ErrRedisConcurrencyProtocol
		}
	case redisConcurrencyAcquireAllowed:
		if !assignRedisConcurrencyAllowed(values, &response) {
			return parsedRedisConcurrency{}, ErrRedisConcurrencyProtocol
		}
	case redisConcurrencyAcquireConflict, redisConcurrencyAcquireExpired:
		if len(values) != 1 {
			return parsedRedisConcurrency{}, ErrRedisConcurrencyProtocol
		}
	case redisConcurrencyAcquireCorrupt:
		if !assignRedisConcurrencyCorrupt(values, &response.scopeIndex) {
			return parsedRedisConcurrency{}, ErrRedisConcurrencyProtocol
		}
	default:
		return parsedRedisConcurrency{}, ErrRedisConcurrencyProtocol
	}
	return response, nil
}

func assignRedisConcurrencyDenied(values []any, response *parsedRedisConcurrency) bool {
	if len(values) != redisConcurrencyDeniedLength {
		return false
	}
	index, indexOK := redisRPMInt64(values[1])
	count, countOK := redisRPMUint64(values[2])
	hard, hardOK := redisRPMUint64(values[3])
	earliest, earliestOK := redisRPMInt64(values[4])
	now, nowOK := redisRPMInt64(values[5])
	if !indexOK || index < 1 || index > requiredScopeCount || !countOK || !hardOK ||
		hard == 0 || hard > limitpolicy.MaximumValue || count < hard ||
		!earliestOK || !nowOK || now < 0 || earliest <= now {
		return false
	}
	response.scopeIndex = int(index - 1)
	response.count = count
	response.hard = hard
	response.earliestMS = earliest
	response.nowMS = now
	return true
}

func assignRedisConcurrencyAllowed(values []any, response *parsedRedisConcurrency) bool {
	if len(values) != redisConcurrencyAllowedLength {
		return false
	}
	now, nowOK := redisRPMInt64(values[1])
	expires, expiresOK := redisRPMInt64(values[2])
	idempotent, idempotentOK := redisRPMInt64(values[3])
	if !nowOK || now < 0 || !expiresOK || expires <= now ||
		!idempotentOK || (idempotent != 0 && idempotent != 1) {
		return false
	}
	response.nowMS = now
	response.expiresMS = expires
	response.idempotent = idempotent == 1
	for index := range requiredScopeCount {
		count, countOK := redisRPMUint64(values[redisConcurrencyCountsOffset+index])
		if !countOK || count == 0 || count > limitpolicy.MaximumValue {
			return false
		}
		response.counts[index] = count
	}
	return true
}

func parseRedisConcurrencyRenew(raw any) (parsedRedisConcurrency, error) {
	return parseRedisConcurrencyLifecycle(raw, redisConcurrencyRenewLength, false)
}

func parseRedisConcurrencyRelease(raw any) (parsedRedisConcurrency, error) {
	return parseRedisConcurrencyLifecycle(raw, redisConcurrencyReleaseLength, true)
}

func parseRedisConcurrencyLifecycle(
	raw any,
	successLength int,
	release bool,
) (parsedRedisConcurrency, error) {
	values, ok := raw.([]any)
	if !ok || len(values) == 0 {
		return parsedRedisConcurrency{}, ErrRedisConcurrencyProtocol
	}
	code, ok := redisRPMInt64(values[0])
	if !ok {
		return parsedRedisConcurrency{}, ErrRedisConcurrencyProtocol
	}
	response := parsedRedisConcurrency{code: code}
	switch code {
	case redisConcurrencyLifecycleSucceeded:
		if !assignRedisConcurrencyLifecycleSuccess(values, successLength, release, &response) {
			return parsedRedisConcurrency{}, ErrRedisConcurrencyProtocol
		}
	case redisConcurrencyLifecycleMissing, redisConcurrencyLifecycleConflict:
		if len(values) != 1 {
			return parsedRedisConcurrency{}, ErrRedisConcurrencyProtocol
		}
	case redisConcurrencyLifecycleCorrupt:
		if !assignRedisConcurrencyCorrupt(values, &response.scopeIndex) {
			return parsedRedisConcurrency{}, ErrRedisConcurrencyProtocol
		}
	default:
		return parsedRedisConcurrency{}, ErrRedisConcurrencyProtocol
	}
	return response, nil
}

func assignRedisConcurrencyLifecycleSuccess(
	values []any,
	successLength int,
	release bool,
	response *parsedRedisConcurrency,
) bool {
	if len(values) != successLength {
		return false
	}
	now, nowOK := redisRPMInt64(values[1])
	expires, expiresOK := redisRPMInt64(values[2])
	if !nowOK || now < 0 || !expiresOK || expires < 0 {
		return false
	}
	response.nowMS = now
	response.expiresMS = expires
	offset := 3
	if release {
		idempotent, idempotentOK := redisRPMInt64(values[3])
		expired, expiredOK := redisRPMInt64(values[4])
		if !idempotentOK || (idempotent != 0 && idempotent != 1) ||
			!expiredOK || (expired != 0 && expired != 1) {
			return false
		}
		response.idempotent = idempotent == 1
		response.expired = expired == 1
		offset = 5
	} else if expires <= now {
		return false
	}
	for index := range requiredScopeCount {
		count, countOK := redisRPMUint64(values[offset+index])
		if !countOK || count > limitpolicy.MaximumValue {
			return false
		}
		response.counts[index] = count
	}
	return true
}

func assignRedisConcurrencyCorrupt(values []any, scopeIndex *int) bool {
	if len(values) != redisConcurrencyCorruptLength {
		return false
	}
	index, ok := redisRPMInt64(values[1])
	if !ok || index < 0 || index > requiredScopeCount {
		return false
	}
	*scopeIndex = int(index - 1)
	return true
}

func deniedRedisConcurrencyAdmission(
	scopes [requiredScopeCount]Scope,
	policies [requiredScopeCount]limitpolicy.Effective,
	response parsedRedisConcurrency,
) (RedisConcurrencyAdmission, error) {
	if response.hard != policies[response.scopeIndex].Concurrency.Hard {
		return RedisConcurrencyAdmission{}, ErrRedisConcurrencyProtocol
	}
	serverTime := time.UnixMilli(response.nowMS).UTC()
	earliest := time.UnixMilli(response.earliestMS).UTC()
	return RedisConcurrencyAdmission{
		ServerTime: serverTime,
		Rejection: &RedisConcurrencyRejection{
			Scope: scopes[response.scopeIndex], Count: response.count, Hard: response.hard,
			RetryAfter: earliest.Sub(serverTime), EarliestExpiry: earliest,
		},
	}, nil
}

func allowedRedisConcurrencyAdmission(
	leaseID string,
	scopes [requiredScopeCount]Scope,
	policies [requiredScopeCount]limitpolicy.Effective,
	response parsedRedisConcurrency,
) (RedisConcurrencyAdmission, error) {
	counts := make([]RedisConcurrencyCount, 0, requiredScopeCount)
	for index, count := range response.counts {
		if !response.idempotent && count > policies[index].Concurrency.Hard {
			return RedisConcurrencyAdmission{}, ErrRedisConcurrencyProtocol
		}
		counts = append(counts, RedisConcurrencyCount{
			Scope: scopes[index], Count: count, Soft: policies[index].Concurrency.Soft,
			SoftExceeded: count > policies[index].Concurrency.Soft,
		})
	}
	return RedisConcurrencyAdmission{
		Handle: RedisConcurrencyHandle{
			LeaseID: leaseID, Scopes: append([]Scope(nil), scopes[:]...),
		},
		ServerTime: time.UnixMilli(response.nowMS).UTC(),
		ExpiresAt:  time.UnixMilli(response.expiresMS).UTC(),
		Counts:     counts, Idempotent: response.idempotent,
	}, nil
}

func validateRedisConcurrencyHandle(
	handle RedisConcurrencyHandle,
) ([requiredScopeCount]Scope, error) {
	if !validRedisConcurrencyLeaseID(handle.LeaseID) {
		return [requiredScopeCount]Scope{}, ErrInvalid
	}
	scopes, err := normalizeScopes(handle.Scopes)
	if err != nil {
		return [requiredScopeCount]Scope{}, err
	}
	return scopes, nil
}

func redisConcurrencyUsage(
	scopes [requiredScopeCount]Scope,
	counts [requiredScopeCount]uint64,
) []RedisConcurrencyUsage {
	usage := make([]RedisConcurrencyUsage, 0, requiredScopeCount)
	for index, count := range counts {
		usage = append(usage, RedisConcurrencyUsage{Scope: scopes[index], Count: count})
	}
	return usage
}

func (limiter *RedisConcurrencyLimiter) keys(
	scopes [requiredScopeCount]Scope,
	leaseID string,
) []string {
	keys := make([]string, 0, requiredScopeCount+1)
	for _, scope := range scopes {
		keys = append(keys, limiter.keyPrefix+":"+redisRPMField(scope))
	}
	return append(keys, limiter.keyPrefix+":lease:"+leaseID)
}

func validRedisConcurrencyPrefix(prefix string) bool {
	return len(prefix) >= 1 && len(prefix) <= maximumRedisConcurrencyPrefixLength &&
		redisConcurrencyPrefixPattern.MatchString(prefix) && strings.Count(prefix, "{concurrency}") == 1
}

func validRedisConcurrencyLeaseID(value string) bool {
	return len(value) >= 1 && len(value) <= maximumRedisConcurrencyLeaseIDBytes &&
		redisConcurrencyIDPattern.MatchString(value)
}

func cloneRedisConcurrencyHandle(handle RedisConcurrencyHandle) RedisConcurrencyHandle {
	handle.Scopes = append([]Scope(nil), handle.Scopes...)
	return handle
}

func redisConcurrencyCorruptError(scopeIndex int) error {
	if scopeIndex < 0 {
		return fmt.Errorf("%w: corrupt lease metadata", ErrRedisConcurrencyProtocol)
	}
	return fmt.Errorf("%w: corrupt %s scope lease set", ErrRedisConcurrencyProtocol, scopeIndexName(scopeIndex))
}

const redisConcurrencyAcquireScript = `
local lease_ms = tonumber(ARGV[1])
local retention_ms = tonumber(ARGV[2])
local member = ARGV[3]
local fingerprint = ARGV[4]
local maximum = 9007199254740991
local redis_time = redis.call('TIME')
local now_ms = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)

local state = redis.call('HGET', KEYS[5], 'state')
if state then
    local stored_fingerprint = redis.call('HGET', KEYS[5], 'fingerprint')
    local raw_expiry = redis.call('HGET', KEYS[5], 'expiry_ms')
    if not stored_fingerprint or not raw_expiry or not string.match(raw_expiry, '^%d+$') then
        return {3, 0}
    end
    local expiry = tonumber(raw_expiry)
    if not expiry or expiry < 0 or expiry > maximum or expiry % 1 ~= 0 then
        return {3, 0}
    end
    if stored_fingerprint ~= fingerprint then
        return {2}
    end
    if state ~= 'active' then
        if state == 'released' or state == 'expired' then
            return {2}
        end
        return {3, 0}
    end
    if expiry <= now_ms then
        for index = 1, 4 do
            redis.call('ZREM', KEYS[index], member)
        end
        redis.call('HSET', KEYS[5], 'state', 'expired')
        return {4}
    end
    local counts = {}
    for index = 1, 4 do
        redis.call('ZREMRANGEBYSCORE', KEYS[index], '-inf', now_ms)
        local score = redis.call('ZSCORE', KEYS[index], member)
        if not score or tonumber(score) ~= expiry then
            return {3, index}
        end
        counts[index] = redis.call('ZCARD', KEYS[index])
    end
    return {1, now_ms, expiry, 1, counts[1], counts[2], counts[3], counts[4]}
end

local counts = {}
local hard_limits = {}
local earliest = {}
for index = 1, 4 do
    local hard = tonumber(ARGV[4 + index])
    if not hard or hard < 1 or hard > maximum or hard % 1 ~= 0 then
        return {3, index}
    end
    redis.call('ZREMRANGEBYSCORE', KEYS[index], '-inf', now_ms)
    if redis.call('ZSCORE', KEYS[index], member) then
        return {3, index}
    end
    counts[index] = redis.call('ZCARD', KEYS[index])
    hard_limits[index] = hard
    if counts[index] >= hard then
        local first = redis.call('ZRANGE', KEYS[index], 0, 0, 'WITHSCORES')
        if not first[2] then
            return {3, index}
        end
        earliest[index] = tonumber(first[2])
        if not earliest[index] or earliest[index] <= now_ms then
            return {3, index}
        end
    end
end
for index = 1, 4 do
    if counts[index] >= hard_limits[index] then
        return {0, index, counts[index], hard_limits[index], earliest[index], now_ms}
    end
end

local expiry = now_ms + lease_ms
local absolute_ttl = expiry + retention_ms
for index = 1, 4 do
    redis.call('ZADD', KEYS[index], expiry, member)
    redis.call('PEXPIREAT', KEYS[index], absolute_ttl)
    counts[index] = counts[index] + 1
end
redis.call('HSET', KEYS[5], 'state', 'active', 'fingerprint', fingerprint, 'expiry_ms', expiry)
redis.call('PEXPIREAT', KEYS[5], absolute_ttl)
return {1, now_ms, expiry, 0, counts[1], counts[2], counts[3], counts[4]}
`

const redisConcurrencyRenewScript = `
local lease_ms = tonumber(ARGV[1])
local retention_ms = tonumber(ARGV[2])
local member = ARGV[3]
local fingerprint = ARGV[4]
local maximum = 9007199254740991
local redis_time = redis.call('TIME')
local now_ms = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)

local state = redis.call('HGET', KEYS[5], 'state')
if not state then
    return {1}
end
local stored_fingerprint = redis.call('HGET', KEYS[5], 'fingerprint')
local raw_expiry = redis.call('HGET', KEYS[5], 'expiry_ms')
if not stored_fingerprint or not raw_expiry or not string.match(raw_expiry, '^%d+$') then
    return {3, 0}
end
local expiry = tonumber(raw_expiry)
if not expiry or expiry < 0 or expiry > maximum or expiry % 1 ~= 0 then
    return {3, 0}
end
if stored_fingerprint ~= fingerprint then
    return {2}
end
if state ~= 'active' then
    if state == 'released' then
        return {2}
    end
    if state == 'expired' then
        return {1}
    end
    return {3, 0}
end
if expiry <= now_ms then
    for index = 1, 4 do
        redis.call('ZREM', KEYS[index], member)
    end
    redis.call('HSET', KEYS[5], 'state', 'expired')
    return {1}
end

local counts = {}
for index = 1, 4 do
    redis.call('ZREMRANGEBYSCORE', KEYS[index], '-inf', now_ms)
    local score = redis.call('ZSCORE', KEYS[index], member)
    if not score or tonumber(score) ~= expiry then
        return {3, index}
    end
    counts[index] = redis.call('ZCARD', KEYS[index])
end
local next_expiry = now_ms + lease_ms
local absolute_ttl = next_expiry + retention_ms
for index = 1, 4 do
    redis.call('ZADD', KEYS[index], 'XX', next_expiry, member)
    redis.call('PEXPIREAT', KEYS[index], absolute_ttl)
end
redis.call('HSET', KEYS[5], 'expiry_ms', next_expiry)
redis.call('PEXPIREAT', KEYS[5], absolute_ttl)
return {0, now_ms, next_expiry, counts[1], counts[2], counts[3], counts[4]}
`

const redisConcurrencyReleaseScript = `
local member = ARGV[1]
local fingerprint = ARGV[2]
local maximum = 9007199254740991
local redis_time = redis.call('TIME')
local now_ms = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)

local state = redis.call('HGET', KEYS[5], 'state')
if not state then
    return {1}
end
local stored_fingerprint = redis.call('HGET', KEYS[5], 'fingerprint')
local raw_expiry = redis.call('HGET', KEYS[5], 'expiry_ms')
if not stored_fingerprint or not raw_expiry or not string.match(raw_expiry, '^%d+$') then
    return {3, 0}
end
local expiry = tonumber(raw_expiry)
if not expiry or expiry < 0 or expiry > maximum or expiry % 1 ~= 0 then
    return {3, 0}
end
if stored_fingerprint ~= fingerprint then
    return {2}
end

for index = 1, 4 do
    redis.call('ZREMRANGEBYSCORE', KEYS[index], '-inf', now_ms)
end
local counts = {}
if state == 'released' or state == 'expired' then
    for index = 1, 4 do
        if redis.call('ZSCORE', KEYS[index], member) then
            return {3, index}
        end
        counts[index] = redis.call('ZCARD', KEYS[index])
    end
    local expired = 0
    if state == 'expired' then expired = 1 end
    return {0, now_ms, expiry, 1, expired, counts[1], counts[2], counts[3], counts[4]}
end
if state ~= 'active' then
    return {3, 0}
end
if expiry <= now_ms then
    for index = 1, 4 do
        redis.call('ZREM', KEYS[index], member)
        counts[index] = redis.call('ZCARD', KEYS[index])
    end
    redis.call('HSET', KEYS[5], 'state', 'expired')
    return {0, now_ms, expiry, 0, 1, counts[1], counts[2], counts[3], counts[4]}
end

for index = 1, 4 do
    local score = redis.call('ZSCORE', KEYS[index], member)
    if not score or tonumber(score) ~= expiry then
        return {3, index}
    end
end
for index = 1, 4 do
    redis.call('ZREM', KEYS[index], member)
    counts[index] = redis.call('ZCARD', KEYS[index])
end
redis.call('HSET', KEYS[5], 'state', 'released')
return {0, now_ms, expiry, 0, 0, counts[1], counts[2], counts[3], counts[4]}
`
