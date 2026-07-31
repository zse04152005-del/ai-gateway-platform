package limits

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/limitpolicy"
)

const (
	defaultRedisTPMKeyPrefix     = "agw:limits:tpm:v1:{tpm}"
	defaultRedisTPMRetention     = time.Hour
	maximumRedisTPMRetention     = 24 * time.Hour
	defaultRedisTPMClockRetries  = 3
	maximumRedisTPMClockRetries  = 5
	maximumRedisTPMPrefixLength  = 128
	maximumTPMReservationIDBytes = 128

	redisTPMReserveDenied         = int64(0)
	redisTPMReserveAllowed        = int64(1)
	redisTPMReserveWindowMismatch = int64(2)
	redisTPMReserveCorrupt        = int64(3)
	redisTPMReserveConflict       = int64(4)
	redisTPMReserveDeniedLength   = 7
	redisTPMReserveAllowedLength  = 10
	redisTPMReserveMismatchLength = 4
	redisTPMReserveCorruptLength  = 2
	redisTPMReserveCountsOffset   = 6

	redisTPMSettleSucceeded = int64(0)
	redisTPMSettleMissing   = int64(1)
	redisTPMSettleConflict  = int64(2)
	redisTPMSettleCorrupt   = int64(3)
	redisTPMSettleResultLen = 10

	redisTPMWindowSeconds = int64(60)
)

var (
	// ErrRedisTPMProtocol means Redis returned an unsafe or corrupt TPM fact.
	ErrRedisTPMProtocol = errors.New("redis TPM response is invalid")
	// ErrRedisTPMClockBoundary means bounded reservation retries could not agree
	// with the Redis server minute before mutation.
	ErrRedisTPMClockBoundary = errors.New("redis TPM clock boundary did not stabilize")
	// ErrRedisTPMReservationConflict means an ID was reused with different
	// scopes, token values, or terminal actual usage.
	ErrRedisTPMReservationConflict = errors.New("redis TPM reservation conflicts with existing state")
	// ErrRedisTPMReservationExpired means the original minute key no longer
	// exists, so settlement cannot safely target another window.
	ErrRedisTPMReservationExpired = errors.New("redis TPM reservation is missing or expired")

	redisTPMPrefixPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._{}-]*$`)
	redisTPMIDPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
)

// RedisTPMOptions defines the versioned namespace, settlement grace and
// bounded Redis-clock retries. Retention is added to the original reset time;
// it never slides on retry or settlement.
type RedisTPMOptions struct {
	KeyPrefix    string
	Retention    time.Duration
	ClockRetries int
}

// DefaultRedisTPMOptions permits terminal settlement for one hour after the
// original minute reset.
func DefaultRedisTPMOptions() RedisTPMOptions {
	return RedisTPMOptions{
		KeyPrefix: defaultRedisTPMKeyPrefix, Retention: defaultRedisTPMRetention,
		ClockRetries: defaultRedisTPMClockRetries,
	}
}

// RedisTPMReserveRequest carries one uniquely identified four-scope plan.
type RedisTPMReserveRequest struct {
	ReservationID string
	Bindings      []Binding
	Plan          TPMReservationPlan
}

// RedisTPMCount reports one distributed TPM counter after a mutation.
type RedisTPMCount struct {
	Scope        Scope
	Count        uint64
	Soft         uint64
	SoftExceeded bool
}

// RedisTPMRejection identifies the first canonical hard boundary that denied
// the all-or-nothing reservation.
type RedisTPMRejection struct {
	Scope      Scope
	Count      uint64
	Requested  uint64
	Hard       uint64
	RetryAfter time.Duration
	ResetAt    time.Time
}

// RedisTPMHandle binds settlement to the original ID, minute and hierarchy.
// Redis verifies a fingerprint of these fields before changing counters.
type RedisTPMHandle struct {
	ReservationID  string
	Window         int64
	Scopes         []Scope
	ReservedTokens uint64
}

// RedisTPMReservation is the outcome of one atomic reserve operation.
type RedisTPMReservation struct {
	Handle     RedisTPMHandle
	WindowKey  string
	ServerTime time.Time
	ResetAt    time.Time
	ExpiresAt  time.Time
	Counts     []RedisTPMCount
	Idempotent bool
	Rejection  *RedisTPMRejection
}

// Allowed reports whether the reservation owns tokens at all four scopes.
func (reservation RedisTPMReservation) Allowed() bool {
	return reservation.Rejection == nil && len(reservation.Counts) == requiredScopeCount &&
		reservation.Handle.ReservationID != ""
}

// RedisTPMSettlement reports the exact signed correction applied to the
// original minute. ReleasedTokens and OverageTokens are mutually exclusive.
type RedisTPMSettlement struct {
	Handle         RedisTPMHandle
	WindowKey      string
	ServerTime     time.Time
	Actual         TPMActual
	ReleasedTokens uint64
	OverageTokens  uint64
	Counts         []RedisTPMCount
	Idempotent     bool
}

// RedisTPMLimiter atomically reserves and settles TPM against one server-time
// minute Hash shared by Platform, Tenant, Project and VirtualKey.
type RedisTPMLimiter struct {
	evaluator    RedisEvaluator
	now          func() time.Time
	keyPrefix    string
	retentionMS  int64
	clockRetries int
}

// NewRedisTPMLimiter validates immutable dependencies and bounds.
func NewRedisTPMLimiter(
	evaluator RedisEvaluator,
	options RedisTPMOptions,
	now func() time.Time,
) (*RedisTPMLimiter, error) {
	retentionMilliseconds := options.Retention.Milliseconds()
	if evaluator == nil || now == nil || !validRedisTPMKeyPrefix(options.KeyPrefix) ||
		retentionMilliseconds < 1 || options.Retention > maximumRedisTPMRetention ||
		options.ClockRetries < 1 || options.ClockRetries > maximumRedisTPMClockRetries {
		return nil, ErrInvalid
	}
	return &RedisTPMLimiter{
		evaluator: evaluator, now: now, keyPrefix: options.KeyPrefix,
		retentionMS: retentionMilliseconds, clockRetries: options.ClockRetries,
	}, nil
}

// ReserveTPM checks every hard limit before incrementing any counter. Retrying
// the exact same ID/plan returns the original reservation without double use.
func (limiter *RedisTPMLimiter) ReserveTPM(
	ctx context.Context,
	request RedisTPMReserveRequest,
) (RedisTPMReservation, error) {
	if limiter == nil || ctx == nil || !validTPMReservationID(request.ReservationID) ||
		request.Plan.validate() != nil {
		return RedisTPMReservation{}, ErrInvalid
	}
	scopes, policies, err := normalizeRPMBindings(request.Bindings)
	if err != nil {
		return RedisTPMReservation{}, err
	}
	window := limiter.now().UTC().Unix() / redisTPMWindowSeconds
	fingerprint := redisTPMScopeFingerprint(scopes)
	for range limiter.clockRetries {
		key := limiter.windowKey(window)
		arguments := redisTPMReserveArguments(
			window, limiter.retentionMS, request.ReservationID, fingerprint,
			request.Plan.ReservedTokens, scopes, policies,
		)
		raw, evalErr := limiter.evaluator.Eval(ctx, redisTPMReserveScript, []string{key}, arguments...)
		if evalErr != nil {
			return RedisTPMReservation{}, fmt.Errorf("evaluate Redis TPM reserve script: %w", evalErr)
		}
		response, responseErr := parseRedisTPMReserveResponse(raw, request.Plan.ReservedTokens)
		if responseErr != nil {
			return RedisTPMReservation{}, responseErr
		}
		switch response.code {
		case redisTPMReserveWindowMismatch:
			window = response.window
			continue
		case redisTPMReserveDenied:
			if response.window != window {
				return RedisTPMReservation{}, ErrRedisTPMProtocol
			}
			return deniedRedisTPMReservation(key, request, scopes, policies, response)
		case redisTPMReserveAllowed:
			if response.window != window || response.resetMS > int64(^uint64(0)>>1)-limiter.retentionMS ||
				response.expiresMS != response.resetMS+limiter.retentionMS {
				return RedisTPMReservation{}, ErrRedisTPMProtocol
			}
			return allowedRedisTPMReservation(key, request, scopes, policies, response)
		case redisTPMReserveCorrupt:
			return RedisTPMReservation{}, redisTPMCorruptError(response.scopeIndex)
		case redisTPMReserveConflict:
			return RedisTPMReservation{}, ErrRedisTPMReservationConflict
		default:
			return RedisTPMReservation{}, ErrRedisTPMProtocol
		}
	}
	return RedisTPMReservation{}, ErrRedisTPMClockBoundary
}

// SettleTPM replaces reserved tokens with terminal actual input+output in the
// original minute. Duplicate identical settlement is idempotent; a different
// actual value for the same ID fails closed.
func (limiter *RedisTPMLimiter) SettleTPM(
	ctx context.Context,
	handle RedisTPMHandle,
	actual TPMActual,
) (RedisTPMSettlement, error) {
	if limiter == nil || ctx == nil || actual.validate() != nil {
		return RedisTPMSettlement{}, ErrInvalid
	}
	scopes, err := validateRedisTPMHandle(handle)
	if err != nil {
		return RedisTPMSettlement{}, err
	}
	key := limiter.windowKey(handle.Window)
	fingerprint := redisTPMScopeFingerprint(scopes)
	arguments := redisTPMSettleArguments(handle, actual, fingerprint, scopes)
	raw, evalErr := limiter.evaluator.Eval(ctx, redisTPMSettleScript, []string{key}, arguments...)
	if evalErr != nil {
		return RedisTPMSettlement{}, fmt.Errorf("evaluate Redis TPM settle script: %w", evalErr)
	}
	response, responseErr := parseRedisTPMSettleResponse(raw, handle, actual)
	if responseErr != nil {
		return RedisTPMSettlement{}, responseErr
	}
	switch response.code {
	case redisTPMSettleSucceeded:
		return redisTPMSettlement(key, handle, scopes, actual, response), nil
	case redisTPMSettleMissing:
		return RedisTPMSettlement{}, ErrRedisTPMReservationExpired
	case redisTPMSettleConflict:
		return RedisTPMSettlement{}, ErrRedisTPMReservationConflict
	case redisTPMSettleCorrupt:
		return RedisTPMSettlement{}, redisTPMCorruptError(response.scopeIndex)
	default:
		return RedisTPMSettlement{}, ErrRedisTPMProtocol
	}
}

type parsedRedisTPMReserve struct {
	code       int64
	window     int64
	nowMS      int64
	resetMS    int64
	expiresMS  int64
	scopeIndex int
	count      uint64
	hard       uint64
	idempotent bool
	counts     [requiredScopeCount]uint64
}

type parsedRedisTPMSettlement struct {
	code       int64
	nowMS      int64
	delta      int64
	scopeIndex int
	idempotent bool
	counts     [requiredScopeCount]uint64
}

func redisTPMReserveArguments(
	window int64,
	retentionMS int64,
	reservationID string,
	fingerprint string,
	requested uint64,
	scopes [requiredScopeCount]Scope,
	policies [requiredScopeCount]limitpolicy.Effective,
) []any {
	arguments := make([]any, 0, 5+requiredScopeCount*3)
	arguments = append(arguments,
		window, retentionMS, redisTPMReservationField(reservationID),
		redisTPMPendingValue(requested, fingerprint), requested,
	)
	for index, scope := range scopes {
		arguments = append(arguments, redisRPMField(scope), policies[index].TPM.Hard, policies[index].TPM.Soft)
	}
	return arguments
}

func redisTPMSettleArguments(
	handle RedisTPMHandle,
	actual TPMActual,
	fingerprint string,
	scopes [requiredScopeCount]Scope,
) []any {
	arguments := make([]any, 0, 5+requiredScopeCount)
	arguments = append(arguments,
		redisTPMReservationField(handle.ReservationID),
		redisTPMPendingValue(handle.ReservedTokens, fingerprint),
		redisTPMSettledValue(handle.ReservedTokens, actual.Tokens, fingerprint),
		handle.ReservedTokens, actual.Tokens,
	)
	for _, scope := range scopes {
		arguments = append(arguments, redisRPMField(scope))
	}
	return arguments
}

func parseRedisTPMReserveResponse(raw any, requested uint64) (parsedRedisTPMReserve, error) {
	values, ok := raw.([]any)
	if !ok || len(values) == 0 {
		return parsedRedisTPMReserve{}, ErrRedisTPMProtocol
	}
	code, ok := redisRPMInt64(values[0])
	if !ok {
		return parsedRedisTPMReserve{}, ErrRedisTPMProtocol
	}
	response := parsedRedisTPMReserve{code: code}
	switch code {
	case redisTPMReserveWindowMismatch:
		if len(values) != redisTPMReserveMismatchLength ||
			!assignRedisTPMReserveTime(values[1], values[2], values[3], nil, &response) {
			return parsedRedisTPMReserve{}, ErrRedisTPMProtocol
		}
	case redisTPMReserveDenied:
		if !assignRedisTPMDenial(values, requested, &response) {
			return parsedRedisTPMReserve{}, ErrRedisTPMProtocol
		}
	case redisTPMReserveAllowed:
		if !assignRedisTPMAllowed(values, &response) {
			return parsedRedisTPMReserve{}, ErrRedisTPMProtocol
		}
	case redisTPMReserveCorrupt:
		if !assignRedisTPMCorrupt(values, &response.scopeIndex) {
			return parsedRedisTPMReserve{}, ErrRedisTPMProtocol
		}
	case redisTPMReserveConflict:
		if len(values) != 1 {
			return parsedRedisTPMReserve{}, ErrRedisTPMProtocol
		}
	default:
		return parsedRedisTPMReserve{}, ErrRedisTPMProtocol
	}
	return response, nil
}

func assignRedisTPMDenial(values []any, requested uint64, response *parsedRedisTPMReserve) bool {
	if len(values) != redisTPMReserveDeniedLength {
		return false
	}
	index, indexOK := redisRPMInt64(values[1])
	count, countOK := redisRPMUint64(values[2])
	hard, hardOK := redisRPMUint64(values[3])
	if !indexOK || index < 1 || index > requiredScopeCount || !countOK || !hardOK ||
		hard == 0 || hard > limitpolicy.MaximumValue || !exceeds(count, requested, hard) {
		return false
	}
	response.scopeIndex = int(index - 1)
	response.count = count
	response.hard = hard
	return assignRedisTPMReserveTime(values[4], values[5], values[6], nil, response)
}

func assignRedisTPMAllowed(values []any, response *parsedRedisTPMReserve) bool {
	if len(values) != redisTPMReserveAllowedLength ||
		!assignRedisTPMReserveTime(values[1], values[2], values[3], values[4], response) {
		return false
	}
	idempotent, ok := redisRPMInt64(values[5])
	if !ok || (idempotent != 0 && idempotent != 1) {
		return false
	}
	response.idempotent = idempotent == 1
	for index := range requiredScopeCount {
		count, countOK := redisRPMUint64(values[redisTPMReserveCountsOffset+index])
		if !countOK || count == 0 || count > limitpolicy.MaximumValue {
			return false
		}
		response.counts[index] = count
	}
	return true
}

func assignRedisTPMReserveTime(
	windowValue any,
	nowValue any,
	resetValue any,
	expiresValue any,
	response *parsedRedisTPMReserve,
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
	if expiresValue == nil {
		return true
	}
	expiresMilliseconds, expiresOK := redisRPMInt64(expiresValue)
	if !expiresOK || expiresMilliseconds <= resetMilliseconds {
		return false
	}
	response.expiresMS = expiresMilliseconds
	return true
}

func assignRedisTPMCorrupt(values []any, scopeIndex *int) bool {
	if len(values) != redisTPMReserveCorruptLength {
		return false
	}
	index, ok := redisRPMInt64(values[1])
	if !ok || index < 0 || index > requiredScopeCount {
		return false
	}
	*scopeIndex = int(index - 1)
	return true
}

func parseRedisTPMSettleResponse(
	raw any,
	handle RedisTPMHandle,
	actual TPMActual,
) (parsedRedisTPMSettlement, error) {
	values, ok := raw.([]any)
	if !ok || len(values) == 0 {
		return parsedRedisTPMSettlement{}, ErrRedisTPMProtocol
	}
	code, ok := redisRPMInt64(values[0])
	if !ok {
		return parsedRedisTPMSettlement{}, ErrRedisTPMProtocol
	}
	response := parsedRedisTPMSettlement{code: code}
	switch code {
	case redisTPMSettleSucceeded:
		if !assignRedisTPMSettlement(values, handle, actual, &response) {
			return parsedRedisTPMSettlement{}, ErrRedisTPMProtocol
		}
	case redisTPMSettleMissing, redisTPMSettleConflict:
		if len(values) != 1 {
			return parsedRedisTPMSettlement{}, ErrRedisTPMProtocol
		}
	case redisTPMSettleCorrupt:
		if !assignRedisTPMCorrupt(values, &response.scopeIndex) {
			return parsedRedisTPMSettlement{}, ErrRedisTPMProtocol
		}
	default:
		return parsedRedisTPMSettlement{}, ErrRedisTPMProtocol
	}
	return response, nil
}

func assignRedisTPMSettlement(
	values []any,
	handle RedisTPMHandle,
	actual TPMActual,
	response *parsedRedisTPMSettlement,
) bool {
	if len(values) != redisTPMSettleResultLen {
		return false
	}
	nowMilliseconds, nowOK := redisRPMInt64(values[1])
	reserved, reservedOK := redisRPMUint64(values[2])
	echoedActual, actualOK := redisRPMUint64(values[3])
	delta, deltaOK := redisRPMInt64(values[4])
	idempotent, idempotentOK := redisRPMInt64(values[5])
	wantDelta, wantDeltaOK := redisTPMDelta(handle.ReservedTokens, actual.Tokens)
	if !nowOK || nowMilliseconds < 0 || !reservedOK || reserved != handle.ReservedTokens ||
		!actualOK || echoedActual != actual.Tokens || !deltaOK || delta != wantDelta ||
		!wantDeltaOK || !idempotentOK || (idempotent != 0 && idempotent != 1) {
		return false
	}
	response.nowMS = nowMilliseconds
	response.delta = delta
	response.idempotent = idempotent == 1
	for index := range requiredScopeCount {
		count, countOK := redisRPMUint64(values[redisTPMReserveCountsOffset+index])
		if !countOK || count > limitpolicy.MaximumValue {
			return false
		}
		response.counts[index] = count
	}
	return true
}

func deniedRedisTPMReservation(
	key string,
	request RedisTPMReserveRequest,
	scopes [requiredScopeCount]Scope,
	policies [requiredScopeCount]limitpolicy.Effective,
	response parsedRedisTPMReserve,
) (RedisTPMReservation, error) {
	if response.hard != policies[response.scopeIndex].TPM.Hard {
		return RedisTPMReservation{}, ErrRedisTPMProtocol
	}
	serverTime := time.UnixMilli(response.nowMS).UTC()
	resetAt := time.UnixMilli(response.resetMS).UTC()
	return RedisTPMReservation{
		WindowKey: key, ServerTime: serverTime, ResetAt: resetAt,
		Rejection: &RedisTPMRejection{
			Scope: scopes[response.scopeIndex], Count: response.count,
			Requested: request.Plan.ReservedTokens, Hard: response.hard,
			RetryAfter: max(resetAt.Sub(serverTime), 0), ResetAt: resetAt,
		},
	}, nil
}

func allowedRedisTPMReservation(
	key string,
	request RedisTPMReserveRequest,
	scopes [requiredScopeCount]Scope,
	policies [requiredScopeCount]limitpolicy.Effective,
	response parsedRedisTPMReserve,
) (RedisTPMReservation, error) {
	counts := make([]RedisTPMCount, 0, requiredScopeCount)
	for index, count := range response.counts {
		if !response.idempotent && count > policies[index].TPM.Hard {
			return RedisTPMReservation{}, ErrRedisTPMProtocol
		}
		counts = append(counts, RedisTPMCount{
			Scope: scopes[index], Count: count, Soft: policies[index].TPM.Soft,
			SoftExceeded: count > policies[index].TPM.Soft,
		})
	}
	return RedisTPMReservation{
		Handle: RedisTPMHandle{
			ReservationID: request.ReservationID, Window: response.window,
			Scopes: append([]Scope(nil), scopes[:]...), ReservedTokens: request.Plan.ReservedTokens,
		},
		WindowKey: key, ServerTime: time.UnixMilli(response.nowMS).UTC(),
		ResetAt: time.UnixMilli(response.resetMS).UTC(), ExpiresAt: time.UnixMilli(response.expiresMS).UTC(),
		Counts: counts, Idempotent: response.idempotent,
	}, nil
}

func redisTPMSettlement(
	key string,
	handle RedisTPMHandle,
	scopes [requiredScopeCount]Scope,
	actual TPMActual,
	response parsedRedisTPMSettlement,
) RedisTPMSettlement {
	counts := make([]RedisTPMCount, 0, requiredScopeCount)
	for index, count := range response.counts {
		counts = append(counts, RedisTPMCount{Scope: scopes[index], Count: count})
	}
	settlement := RedisTPMSettlement{
		Handle: cloneRedisTPMHandle(handle), WindowKey: key,
		ServerTime: time.UnixMilli(response.nowMS).UTC(), Actual: actual,
		Counts: counts, Idempotent: response.idempotent,
	}
	if response.delta < 0 {
		settlement.ReleasedTokens = uint64(-response.delta)
	} else {
		settlement.OverageTokens = uint64(response.delta)
	}
	return settlement
}

func validateRedisTPMHandle(handle RedisTPMHandle) ([requiredScopeCount]Scope, error) {
	if !validTPMReservationID(handle.ReservationID) || handle.Window < 0 ||
		handle.ReservedTokens == 0 || handle.ReservedTokens > limitpolicy.MaximumValue {
		return [requiredScopeCount]Scope{}, ErrInvalid
	}
	scopes, err := normalizeScopes(handle.Scopes)
	if err != nil {
		return [requiredScopeCount]Scope{}, err
	}
	return scopes, nil
}

func (limiter *RedisTPMLimiter) windowKey(window int64) string {
	return limiter.keyPrefix + ":" + strconv.FormatInt(window, 10)
}

func redisTPMScopeFingerprint(scopes [requiredScopeCount]Scope) string {
	fields := make([]string, 0, requiredScopeCount)
	for _, scope := range scopes {
		fields = append(fields, redisRPMField(scope))
	}
	digest := sha256.Sum256([]byte(strings.Join(fields, "\x00")))
	return fmt.Sprintf("%x", digest)
}

func redisTPMReservationField(reservationID string) string {
	return "reservation:" + reservationID
}

func redisTPMPendingValue(reserved uint64, fingerprint string) string {
	return "r:" + strconv.FormatUint(reserved, 10) + ":" + fingerprint
}

func redisTPMSettledValue(reserved, actual uint64, fingerprint string) string {
	return "s:" + strconv.FormatUint(reserved, 10) + ":" + strconv.FormatUint(actual, 10) + ":" + fingerprint
}

func validRedisTPMKeyPrefix(prefix string) bool {
	return len(prefix) >= 1 && len(prefix) <= maximumRedisTPMPrefixLength &&
		redisTPMPrefixPattern.MatchString(prefix) && strings.Count(prefix, "{tpm}") == 1
}

func validTPMReservationID(value string) bool {
	return len(value) >= 1 && len(value) <= maximumTPMReservationIDBytes && redisTPMIDPattern.MatchString(value)
}

func cloneRedisTPMHandle(handle RedisTPMHandle) RedisTPMHandle {
	handle.Scopes = append([]Scope(nil), handle.Scopes...)
	return handle
}

func redisTPMDelta(reserved, actual uint64) (int64, bool) {
	if actual >= reserved {
		difference := actual - reserved
		if difference > limitpolicy.MaximumValue {
			return 0, false
		}
		return int64(difference), true
	}
	difference := reserved - actual
	if difference > limitpolicy.MaximumValue {
		return 0, false
	}
	return -int64(difference), true
}

func redisTPMCorruptError(scopeIndex int) error {
	if scopeIndex < 0 {
		return fmt.Errorf("%w: corrupt reservation record", ErrRedisTPMProtocol)
	}
	return fmt.Errorf("%w: corrupt %s scope counter", ErrRedisTPMProtocol, scopeIndexName(scopeIndex))
}

func scopeIndexName(scopeIndex int) ScopeKind {
	switch scopeIndex {
	case 0:
		return ScopePlatform
	case 1:
		return ScopeTenant
	case 2:
		return ScopeProject
	case 3:
		return ScopeKey
	default:
		return ScopeKind("unknown")
	}
}

const redisTPMReserveScript = `
local expected_window = tonumber(ARGV[1])
local retention_ms = tonumber(ARGV[2])
local reservation_field = ARGV[3]
local reservation_value = ARGV[4]
local requested = tonumber(ARGV[5])
local redis_time = redis.call('TIME')
local seconds = tonumber(redis_time[1])
local microseconds = tonumber(redis_time[2])
local now_ms = seconds * 1000 + math.floor(microseconds / 1000)
local server_window = math.floor(seconds / 60)
local reset_ms = (server_window + 1) * 60 * 1000
local expires_ms = reset_ms + retention_ms

if server_window ~= expected_window then
    return {2, server_window, now_ms, reset_ms}
end

local maximum = 9007199254740991
if not requested or requested < 1 or requested > maximum or requested % 1 ~= 0 then
    return {3, 0}
end

local fields = {}
local hard_limits = {}
local counts = {}
for index = 1, 4 do
    local argument = 6 + (index - 1) * 3
    local field = ARGV[argument]
    local hard = tonumber(ARGV[argument + 1])
    local soft = tonumber(ARGV[argument + 2])
    if not hard or not soft or hard < 1 or hard > maximum or soft < 1 or soft > hard or
       hard % 1 ~= 0 or soft % 1 ~= 0 then
        return {3, index}
    end
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

local existing = redis.call('HGET', KEYS[1], reservation_field)
if existing then
    if existing ~= reservation_value then
        return {4}
    end
    return {1, server_window, now_ms, reset_ms, expires_ms, 1,
            counts[1], counts[2], counts[3], counts[4]}
end

for index = 1, 4 do
    if requested > hard_limits[index] or counts[index] > hard_limits[index] - requested then
        return {0, index, counts[index], hard_limits[index], server_window, now_ms, reset_ms}
    end
end

for index = 1, 4 do
    counts[index] = redis.call('HINCRBY', KEYS[1], fields[index], requested)
end
redis.call('HSET', KEYS[1], reservation_field, reservation_value)
redis.call('PEXPIREAT', KEYS[1], expires_ms)
return {1, server_window, now_ms, reset_ms, expires_ms, 0,
        counts[1], counts[2], counts[3], counts[4]}
`

const redisTPMSettleScript = `
local reservation_field = ARGV[1]
local pending_value = ARGV[2]
local settled_value = ARGV[3]
local reserved = tonumber(ARGV[4])
local actual = tonumber(ARGV[5])
local maximum = 9007199254740991

local existing = redis.call('HGET', KEYS[1], reservation_field)
if not existing then
    return {1}
end
if existing ~= pending_value and existing ~= settled_value then
    return {2}
end
if not reserved or not actual or reserved < 1 or reserved > maximum or actual < 0 or actual > maximum or
   reserved % 1 ~= 0 or actual % 1 ~= 0 then
    return {3, 0}
end

local fields = {}
local counts = {}
for index = 1, 4 do
    local field = ARGV[5 + index]
    local raw = redis.call('HGET', KEYS[1], field)
    if not raw or not string.match(raw, '^%d+$') then
        return {3, index}
    end
    local count = tonumber(raw)
    if not count or count < 0 or count > maximum or count % 1 ~= 0 then
        return {3, index}
    end
    fields[index] = field
    counts[index] = count
end

local delta = actual - reserved
if existing == settled_value then
    local redis_time = redis.call('TIME')
    local now_ms = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)
    return {0, now_ms, reserved, actual, delta, 1,
            counts[1], counts[2], counts[3], counts[4]}
end

for index = 1, 4 do
    if delta < 0 and counts[index] < -delta then
        return {3, index}
    end
    if delta > 0 and counts[index] > maximum - delta then
        return {3, index}
    end
end
for index = 1, 4 do
    counts[index] = redis.call('HINCRBY', KEYS[1], fields[index], delta)
end
redis.call('HSET', KEYS[1], reservation_field, settled_value)
local redis_time = redis.call('TIME')
local now_ms = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)
return {0, now_ms, reserved, actual, delta, 0,
        counts[1], counts[2], counts[3], counts[4]}
`
