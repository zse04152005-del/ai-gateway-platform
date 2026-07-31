package limits

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/limitpolicy"
)

const (
	localTenantID  = "73000000-0000-4000-8000-000000000001"
	localProjectID = "73000000-0000-4000-8000-000000000002"
	localKeyID     = "73000000-0000-4000-8000-000000000003"
	otherTenantID  = "73000000-0000-4000-8000-000000000004"
	workerCount    = 64
	permitCount    = 8
)

func TestLocalLimiterAdmissionSoftHardAndRelease(t *testing.T) {
	clock := newLocalTestClock(time.Date(2026, time.July, 31, 12, 34, 20, 0, time.UTC))
	limiter := mustLocalLimiter(t, clock)
	policy := localEffective(1, 3, 5, 12, 1, 2)
	if err := limiter.Replace(1, localBindings(policy)); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}

	first, err := limiter.Acquire(Request{Scopes: localScopes(), EstimatedTokens: 4})
	if err != nil || !first.Allowed() || len(first.SoftThresholds) != 0 {
		t.Fatalf("first admission = %+v, %v", first, err)
	}
	second, err := limiter.Acquire(Request{Scopes: reverseLocalScopes(), EstimatedTokens: 3})
	if err != nil || !second.Allowed() {
		t.Fatalf("second admission = %+v, %v", second, err)
	}
	if len(second.SoftThresholds) != requiredScopeCount*3 {
		t.Fatalf("soft threshold count = %d, want %d", len(second.SoftThresholds), requiredScopeCount*3)
	}
	for _, threshold := range second.SoftThresholds {
		if threshold.Usage <= threshold.Threshold {
			t.Fatalf("soft threshold = %+v", threshold)
		}
	}

	denied, err := limiter.Acquire(Request{Scopes: localScopes(), EstimatedTokens: 1})
	if err != nil || denied.Allowed() || denied.Rejection == nil {
		t.Fatalf("concurrency denial = %+v, %v", denied, err)
	}
	if denied.Rejection.Scope.Kind != ScopePlatform || denied.Rejection.Resource != ResourceConcurrency ||
		denied.Rejection.Usage != 2 || denied.Rejection.Hard != 2 || denied.Rejection.RetryAfter != 0 ||
		!denied.Rejection.ResetAt.IsZero() {
		t.Fatalf("concurrency rejection = %+v", denied.Rejection)
	}

	first.Lease.Release()
	third, err := limiter.Acquire(Request{Scopes: localScopes(), EstimatedTokens: 1})
	if err != nil || !third.Allowed() {
		t.Fatalf("third admission after release = %+v, %v", third, err)
	}
	third.Lease.Release()

	usage, err := limiter.Snapshot(localScopes()[0])
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if usage.RPM != 3 || usage.TPM != 8 || usage.Concurrency != 1 || usage.SnapshotVersion != 1 {
		t.Fatalf("usage = %+v", usage)
	}
	second.Lease.Release()
	second.Lease.Release()

	rpmDenied, err := limiter.Acquire(Request{Scopes: localScopes(), EstimatedTokens: 1})
	if err != nil || rpmDenied.Rejection == nil || rpmDenied.Rejection.Resource != ResourceRPM {
		t.Fatalf("RPM denial = %+v, %v", rpmDenied, err)
	}
	if rpmDenied.Rejection.Usage != 3 || rpmDenied.Rejection.Requested != 1 ||
		rpmDenied.Rejection.Hard != 3 || rpmDenied.Rejection.RetryAfter != 40*time.Second ||
		!rpmDenied.Rejection.ResetAt.Equal(time.Date(2026, time.July, 31, 12, 35, 0, 0, time.UTC)) {
		t.Fatalf("RPM rejection = %+v", rpmDenied.Rejection)
	}
}

func TestLocalLimiterRejectsHierarchyAtomically(t *testing.T) {
	clock := newLocalTestClock(time.Date(2026, time.July, 31, 13, 0, 0, 0, time.UTC))
	limiter := mustLocalLimiter(t, clock)
	bindings := localBindings(localEffective(5, 10, 50, 100, 5, 10))
	bindings[1].Policy = localEffective(1, 1, 50, 100, 5, 10)
	if err := limiter.Replace(1, bindings); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}

	admission, err := limiter.Acquire(Request{Scopes: localScopes(), EstimatedTokens: 2})
	if err != nil || !admission.Allowed() {
		t.Fatalf("first admission = %+v, %v", admission, err)
	}
	admission.Lease.Release()
	denied, err := limiter.Acquire(Request{Scopes: localScopes(), EstimatedTokens: 2})
	if err != nil || denied.Rejection == nil || denied.Rejection.Scope.Kind != ScopeTenant ||
		denied.Rejection.Resource != ResourceRPM {
		t.Fatalf("tenant denial = %+v, %v", denied, err)
	}
	platformUsage, err := limiter.Snapshot(localScopes()[0])
	if err != nil {
		t.Fatalf("Snapshot(platform) error = %v", err)
	}
	tenantUsage, err := limiter.Snapshot(localScopes()[1])
	if err != nil {
		t.Fatalf("Snapshot(tenant) error = %v", err)
	}
	if platformUsage.RPM != 1 || platformUsage.TPM != 2 || tenantUsage.RPM != 1 || tenantUsage.TPM != 2 {
		t.Fatalf("partial consumption: platform=%+v tenant=%+v", platformUsage, tenantUsage)
	}
}

func TestLocalLimiterMinuteWindowAndClockRegression(t *testing.T) {
	clock := newLocalTestClock(time.Date(2026, time.July, 31, 13, 10, 30, 0, time.UTC))
	limiter := mustLocalLimiter(t, clock)
	policy := localEffective(1, 1, 100, 200, 5, 10)
	if err := limiter.Replace(1, localBindings(policy)); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	first, err := limiter.Acquire(Request{Scopes: localScopes(), EstimatedTokens: 1})
	if err != nil || !first.Allowed() {
		t.Fatalf("first admission = %+v, %v", first, err)
	}
	first.Lease.Release()
	denied, err := limiter.Acquire(Request{Scopes: localScopes(), EstimatedTokens: 1})
	if err != nil || denied.Rejection == nil || denied.Rejection.RetryAfter != 30*time.Second {
		t.Fatalf("window denial = %+v, %v", denied, err)
	}

	clock.Advance(30 * time.Second)
	nextWindow, err := limiter.Acquire(Request{Scopes: localScopes(), EstimatedTokens: 1})
	if err != nil || !nextWindow.Allowed() {
		t.Fatalf("next-window admission = %+v, %v", nextWindow, err)
	}
	nextWindow.Lease.Release()

	clock.Set(time.Date(2026, time.July, 31, 13, 10, 45, 0, time.UTC))
	regressed, err := limiter.Acquire(Request{Scopes: localScopes(), EstimatedTokens: 1})
	if err != nil || regressed.Rejection == nil || regressed.Rejection.Resource != ResourceRPM {
		t.Fatalf("regressed-clock admission = %+v, %v", regressed, err)
	}
	usage, err := limiter.Snapshot(localScopes()[0])
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if !usage.WindowStart.Equal(time.Date(2026, time.July, 31, 13, 11, 0, 0, time.UTC)) || usage.RPM != 1 {
		t.Fatalf("regressed-clock usage = %+v", usage)
	}
}

func TestLocalLimiterTPMRejectionAndSnapshotWindowReset(t *testing.T) {
	clock := newLocalTestClock(time.Date(2026, time.July, 31, 13, 20, 10, 0, time.UTC))
	limiter := mustLocalLimiter(t, clock)
	policy := localEffective(50, 100, 3, 5, 5, 10)
	if err := limiter.Replace(1, localBindings(policy)); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	first, err := limiter.Acquire(Request{Scopes: localScopes(), EstimatedTokens: 4})
	if err != nil || !first.Allowed() {
		t.Fatalf("first admission = %+v, %v", first, err)
	}
	first.Lease.Release()
	denied, err := limiter.Acquire(Request{Scopes: localScopes(), EstimatedTokens: 2})
	if err != nil || denied.Rejection == nil || denied.Rejection.Resource != ResourceTPM ||
		denied.Rejection.Usage != 4 || denied.Rejection.Requested != 2 || denied.Rejection.Hard != 5 ||
		denied.Rejection.RetryAfter != 50*time.Second {
		t.Fatalf("TPM denial = %+v, %v", denied, err)
	}

	clock.Advance(50 * time.Second)
	usage, err := limiter.Snapshot(localScopes()[0])
	if err != nil || usage.RPM != 0 || usage.TPM != 0 || usage.Concurrency != 0 ||
		!usage.WindowStart.Equal(time.Date(2026, time.July, 31, 13, 21, 0, 0, time.UTC)) {
		t.Fatalf("reset usage = %+v, %v", usage, err)
	}
}

func TestLocalLimiterHotUpdateRetainsUsageAndLiveLeases(t *testing.T) {
	clock := newLocalTestClock(time.Date(2026, time.July, 31, 14, 0, 0, 0, time.UTC))
	limiter := mustLocalLimiter(t, clock)
	if err := limiter.Replace(1, localBindings(localEffective(50, 100, 500, 1_000, 1, 1))); err != nil {
		t.Fatalf("Replace(v1) error = %v", err)
	}
	first, err := limiter.Acquire(Request{Scopes: localScopes(), EstimatedTokens: 5})
	if err != nil || !first.Allowed() {
		t.Fatalf("first admission = %+v, %v", first, err)
	}
	denied, err := limiter.Acquire(Request{Scopes: localScopes(), EstimatedTokens: 5})
	if err != nil || denied.Rejection == nil || denied.Rejection.Resource != ResourceConcurrency {
		t.Fatalf("v1 denial = %+v, %v", denied, err)
	}

	if err := limiter.Replace(2, localBindings(localEffective(50, 100, 500, 1_000, 1, 2))); err != nil {
		t.Fatalf("Replace(v2) error = %v", err)
	}
	second, err := limiter.Acquire(Request{Scopes: localScopes(), EstimatedTokens: 5})
	if err != nil || !second.Allowed() || second.SnapshotVersion != 2 {
		t.Fatalf("v2 admission = %+v, %v", second, err)
	}
	if err := limiter.Replace(2, localBindings(localEffective(50, 100, 500, 1_000, 1, 2))); !errors.Is(err, ErrStaleSnapshot) {
		t.Fatalf("Replace(stale) error = %v", err)
	}

	if err := limiter.Replace(3, localBindings(localEffective(50, 100, 500, 1_000, 1, 1))); err != nil {
		t.Fatalf("Replace(v3) error = %v", err)
	}
	lowered, err := limiter.Acquire(Request{Scopes: localScopes(), EstimatedTokens: 5})
	if err != nil || lowered.Rejection == nil || lowered.Rejection.Resource != ResourceConcurrency {
		t.Fatalf("lowered-limit admission = %+v, %v", lowered, err)
	}
	usage, err := limiter.Snapshot(localScopes()[0])
	if err != nil || usage.SnapshotVersion != 3 || usage.RPM != 2 || usage.TPM != 10 || usage.Concurrency != 2 {
		t.Fatalf("usage after hot updates = %+v, %v", usage, err)
	}

	first.Lease.Release()
	stillFull, err := limiter.Acquire(Request{Scopes: localScopes(), EstimatedTokens: 5})
	if err != nil || stillFull.Rejection == nil || stillFull.Rejection.Resource != ResourceConcurrency {
		t.Fatalf("one-live-lease admission = %+v, %v", stillFull, err)
	}
	second.Lease.Release()
	resumed, err := limiter.Acquire(Request{Scopes: localScopes(), EstimatedTokens: 5})
	if err != nil || !resumed.Allowed() {
		t.Fatalf("resumed admission = %+v, %v", resumed, err)
	}
	if err := limiter.Replace(4, localBindings(localEffective(50, 100, 500, 1_000, 1, 1))[:3]); err != nil {
		t.Fatalf("Replace(v4 without key) error = %v", err)
	}
	resumed.Lease.Release()
	if err := limiter.Replace(5, localBindings(localEffective(50, 100, 500, 1_000, 1, 1))); err != nil {
		t.Fatalf("Replace(v5) error = %v", err)
	}
	keyUsage, err := limiter.Snapshot(localScopes()[3])
	if err != nil || keyUsage.RPM != 0 || keyUsage.TPM != 0 || keyUsage.Concurrency != 0 {
		t.Fatalf("re-added key usage = %+v, %v", keyUsage, err)
	}
	if limiter.Version() != 5 {
		t.Fatalf("Version() = %d, want 5", limiter.Version())
	}
}

func TestLocalLimiterValidationAndMissingPolicyFailClosed(t *testing.T) {
	clock := newLocalTestClock(time.Date(2026, time.July, 31, 15, 0, 0, 0, time.UTC))
	if _, err := NewLocalLimiter(Options{}, clock.Now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("NewLocalLimiter(empty options) error = %v", err)
	}
	if _, err := NewLocalLimiter(DefaultOptions(), nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("NewLocalLimiter(nil clock) error = %v", err)
	}
	limiter := mustLocalLimiter(t, clock)
	if err := (*LocalLimiter)(nil).Replace(1, localBindings(localEffective(1, 2, 1, 2, 1, 2))); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil LocalLimiter.Replace() error = %v", err)
	}
	if _, err := limiter.Acquire(Request{Scopes: localScopes(), EstimatedTokens: 1}); !errors.Is(err, ErrPolicyUnavailable) {
		t.Fatalf("Acquire(unconfigured) error = %v", err)
	}
	if err := limiter.Replace(0, localBindings(localEffective(1, 2, 1, 2, 1, 2))); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Replace(version zero) error = %v", err)
	}
	duplicate := localBindings(localEffective(1, 2, 1, 2, 1, 2))
	duplicate = append(duplicate, duplicate[0])
	if err := limiter.Replace(1, duplicate); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Replace(duplicate) error = %v", err)
	}
	invalidPolicy := localEffective(2, 1, 1, 2, 1, 2)
	if err := limiter.Replace(1, localBindings(invalidPolicy)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Replace(invalid policy) error = %v", err)
	}

	missingKey := localBindings(localEffective(1, 2, 1, 2, 1, 2))[:3]
	if err := limiter.Replace(1, missingKey); err != nil {
		t.Fatalf("Replace(missing key) error = %v", err)
	}
	platformUsage, err := limiter.Snapshot(localScopes()[0])
	if err != nil || platformUsage.RPM != 0 || platformUsage.TPM != 0 || platformUsage.Concurrency != 0 {
		t.Fatalf("Snapshot(unused platform) = %+v, %v", platformUsage, err)
	}
	if _, err := limiter.Acquire(Request{Scopes: localScopes(), EstimatedTokens: 1}); !errors.Is(err, ErrPolicyUnavailable) {
		t.Fatalf("Acquire(missing key) error = %v", err)
	}
	if _, err := limiter.Snapshot(localScopes()[3]); !errors.Is(err, ErrPolicyUnavailable) {
		t.Fatalf("Snapshot(missing key) error = %v", err)
	}
	if _, err := (*LocalLimiter)(nil).Snapshot(localScopes()[0]); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil LocalLimiter.Snapshot() error = %v", err)
	}
	if _, err := limiter.Snapshot(Scope{Kind: ScopeTenant, TenantID: "bad"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Snapshot(invalid scope) error = %v", err)
	}

	invalidRequests := []Request{
		{},
		{Scopes: localScopes(), EstimatedTokens: 0},
		{Scopes: localScopes(), EstimatedTokens: limitpolicy.MaximumValue + 1},
		{Scopes: localScopes()[:3], EstimatedTokens: 1},
		{Scopes: []Scope{localScopes()[0], localScopes()[1], localScopes()[2], localScopes()[2]}, EstimatedTokens: 1},
		{
			Scopes: []Scope{
				localScopes()[0],
				{Kind: ScopeTenant, TenantID: otherTenantID},
				localScopes()[2],
				localScopes()[3],
			},
			EstimatedTokens: 1,
		},
	}
	for index, request := range invalidRequests {
		if _, err := limiter.Acquire(request); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Acquire(invalid[%d]) error = %v", index, err)
		}
	}
	if (Scope{Kind: ScopeTenant, TenantID: "bad"}).Validate() == nil {
		t.Fatal("Scope.Validate(bad UUID) error = nil")
	}
	if (*LocalLimiter)(nil).Version() != 0 {
		t.Fatal("nil LocalLimiter.Version() != 0")
	}
	var nilLease *Lease
	nilLease.Release()
	limiter.release([requiredScopeCount]Scope{})

	smallLimiter, err := NewLocalLimiter(Options{MaximumBindings: requiredScopeCount}, clock.Now)
	if err != nil {
		t.Fatalf("NewLocalLimiter(small) error = %v", err)
	}
	tooMany := append(localBindings(localEffective(1, 2, 1, 2, 1, 2)), Binding{
		Scope:  Scope{Kind: ScopeTenant, TenantID: otherTenantID},
		Policy: localEffective(1, 2, 1, 2, 1, 2),
	})
	if err := smallLimiter.Replace(1, tooMany); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Replace(over capacity) error = %v", err)
	}
}

func TestScopeValidationMatrix(t *testing.T) {
	invalid := []Scope{
		{Kind: ScopePlatform, TenantID: localTenantID},
		{Kind: ScopeTenant, TenantID: localTenantID, ProjectID: localProjectID},
		{Kind: ScopeTenant, TenantID: localTenantID, KeyID: localKeyID},
		{Kind: ScopeProject, TenantID: localTenantID},
		{Kind: ScopeProject, TenantID: localTenantID, ProjectID: localProjectID, KeyID: localKeyID},
		{Kind: ScopeKey, TenantID: localTenantID, ProjectID: localProjectID, KeyID: "bad"},
		{Kind: "unknown"},
	}
	for index, scope := range invalid {
		if !errors.Is(scope.Validate(), ErrInvalid) {
			t.Fatalf("Scope.Validate(invalid[%d]) error = nil", index)
		}
	}
	if scopeRank("unknown") != requiredScopeCount {
		t.Fatalf("scopeRank(unknown) = %d", scopeRank("unknown"))
	}
}

func TestLocalLimiterConcurrentHardCapAndIdempotentRelease(t *testing.T) {
	clock := newLocalTestClock(time.Date(2026, time.July, 31, 16, 0, 0, 0, time.UTC))
	limiter := mustLocalLimiter(t, clock)
	policy := localEffective(500, 1_000, 500, 1_000, permitCount-1, permitCount)
	if err := limiter.Replace(1, localBindings(policy)); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}

	type result struct {
		admission Admission
		err       error
	}
	start := make(chan struct{})
	release := make(chan struct{})
	results := make(chan result, workerCount)
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			<-start
			admission, err := limiter.Acquire(Request{Scopes: localScopes(), EstimatedTokens: 1})
			results <- result{admission: admission, err: err}
			if admission.Allowed() {
				<-release
				admission.Lease.Release()
				admission.Lease.Release()
			}
		}()
	}
	close(start)
	admitted := 0
	for range workerCount {
		outcome := <-results
		if outcome.err != nil {
			t.Fatalf("Acquire() error = %v", outcome.err)
		}
		if outcome.admission.Allowed() {
			admitted++
			continue
		}
		if outcome.admission.Rejection == nil || outcome.admission.Rejection.Resource != ResourceConcurrency {
			t.Fatalf("denied admission = %+v", outcome.admission)
		}
	}
	if admitted != permitCount {
		t.Fatalf("admitted = %d, want %d", admitted, permitCount)
	}
	usage, err := limiter.Snapshot(localScopes()[0])
	if err != nil || usage.Concurrency != permitCount || usage.RPM != permitCount || usage.TPM != permitCount {
		t.Fatalf("held usage = %+v, %v", usage, err)
	}
	close(release)
	workers.Wait()
	usage, err = limiter.Snapshot(localScopes()[0])
	if err != nil || usage.Concurrency != 0 || usage.RPM != permitCount || usage.TPM != permitCount {
		t.Fatalf("released usage = %+v, %v", usage, err)
	}
}

func TestLocalLimiterConcurrentHotUpdates(t *testing.T) {
	clock := newLocalTestClock(time.Date(2026, time.July, 31, 17, 0, 0, 0, time.UTC))
	limiter := mustLocalLimiter(t, clock)
	policy := localEffective(10_000, 20_000, 10_000, 20_000, 31, 32)
	if err := limiter.Replace(1, localBindings(policy)); err != nil {
		t.Fatalf("Replace(v1) error = %v", err)
	}

	const (
		concurrentWorkers = 32
		iterations        = 100
		lastVersion       = 100
	)
	start := make(chan struct{})
	errorsFound := make(chan error, concurrentWorkers+1)
	var workers sync.WaitGroup
	workers.Add(concurrentWorkers + 1)
	for range concurrentWorkers {
		go func() {
			defer workers.Done()
			<-start
			for range iterations {
				admission, err := limiter.Acquire(Request{Scopes: localScopes(), EstimatedTokens: 1})
				if err != nil {
					errorsFound <- err
					return
				}
				if !admission.Allowed() {
					errorsFound <- errors.New("admission unexpectedly rejected")
					return
				}
				admission.Lease.Release()
			}
		}()
	}
	go func() {
		defer workers.Done()
		<-start
		for version := uint64(2); version <= lastVersion; version++ {
			updated := localEffective(10_000, 20_000, 10_000, 20_000, 31, 32)
			if err := limiter.Replace(version, localBindings(updated)); err != nil {
				errorsFound <- err
				return
			}
		}
	}()
	close(start)
	workers.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Errorf("concurrent operation: %v", err)
	}
	if limiter.Version() != lastVersion {
		t.Fatalf("Version() = %d, want %d", limiter.Version(), lastVersion)
	}
	usage, err := limiter.Snapshot(localScopes()[0])
	if err != nil || usage.Concurrency != 0 || usage.RPM != concurrentWorkers*iterations {
		t.Fatalf("final usage = %+v, %v", usage, err)
	}
}

func BenchmarkLocalLimiterAcquireRelease(b *testing.B) {
	clock := newLocalTestClock(time.Date(2026, time.July, 31, 18, 0, 0, 0, time.UTC))
	limiter, err := NewLocalLimiter(DefaultOptions(), clock.Now)
	if err != nil {
		b.Fatalf("NewLocalLimiter() error = %v", err)
	}
	policy := localEffective(
		limitpolicy.MaximumValue-1,
		limitpolicy.MaximumValue,
		limitpolicy.MaximumValue-1,
		limitpolicy.MaximumValue,
		limitpolicy.MaximumValue-1,
		limitpolicy.MaximumValue,
	)
	if err := limiter.Replace(1, localBindings(policy)); err != nil {
		b.Fatalf("Replace() error = %v", err)
	}
	request := Request{Scopes: localScopes(), EstimatedTokens: 1}
	b.ReportAllocs()
	for b.Loop() {
		admission, acquireErr := limiter.Acquire(request)
		if acquireErr != nil || !admission.Allowed() {
			b.Fatalf("Acquire() = %+v, %v", admission, acquireErr)
		}
		admission.Lease.Release()
	}
}

type localTestClock struct {
	mu      sync.Mutex
	current time.Time
}

func newLocalTestClock(current time.Time) *localTestClock {
	return &localTestClock{current: current}
}

func (clock *localTestClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.current
}

func (clock *localTestClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.current = clock.current.Add(duration)
}

func (clock *localTestClock) Set(current time.Time) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.current = current
}

func mustLocalLimiter(t *testing.T, clock *localTestClock) *LocalLimiter {
	t.Helper()
	limiter, err := NewLocalLimiter(DefaultOptions(), clock.Now)
	if err != nil {
		t.Fatalf("NewLocalLimiter() error = %v", err)
	}
	return limiter
}

func localScopes() []Scope {
	return []Scope{
		{Kind: ScopePlatform},
		{Kind: ScopeTenant, TenantID: localTenantID},
		{Kind: ScopeProject, TenantID: localTenantID, ProjectID: localProjectID},
		{Kind: ScopeKey, TenantID: localTenantID, ProjectID: localProjectID, KeyID: localKeyID},
	}
}

func reverseLocalScopes() []Scope {
	scopes := localScopes()
	return []Scope{scopes[3], scopes[2], scopes[1], scopes[0]}
}

func localBindings(policy limitpolicy.Effective) []Binding {
	scopes := localScopes()
	bindings := make([]Binding, 0, len(scopes))
	for _, scope := range scopes {
		bindings = append(bindings, Binding{Scope: scope, Policy: policy})
	}
	return bindings
}

func localEffective(
	rpmSoft uint64,
	rpmHard uint64,
	tpmSoft uint64,
	tpmHard uint64,
	concurrencySoft uint64,
	concurrencyHard uint64,
) limitpolicy.Effective {
	return limitpolicy.Effective{
		RPM:         localThreshold(rpmSoft, rpmHard),
		TPM:         localThreshold(tpmSoft, tpmHard),
		Concurrency: localThreshold(concurrencySoft, concurrencyHard),
	}
}

func localThreshold(soft, hard uint64) limitpolicy.EffectiveThreshold {
	return limitpolicy.EffectiveThreshold{
		Soft: soft, Hard: hard,
		SoftSource: limitpolicy.SourcePlatform, HardSource: limitpolicy.SourcePlatform,
	}
}
