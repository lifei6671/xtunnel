package managementapi

import (
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"
)

func TestLoginFailureLimiterPairRollingWindowAndNormalization(t *testing.T) {
	clock := newLoginLimiterClock()
	limiter := newLoginFailureLimiter(clock.Now)
	address := netip.MustParseAddr("::ffff:192.0.2.10")

	for range loginPairFailureLimit - 1 {
		if retryAfter := limiter.RecordFailure(address, "  AdMiN  "); retryAfter != 0 {
			t.Fatalf("RecordFailure() Retry-After = %d, want 0 before threshold", retryAfter)
		}
	}
	if retryAfter := limiter.RecordFailure(netip.MustParseAddr("192.0.2.10"), "admin"); retryAfter != 60 {
		t.Fatalf("RecordFailure() Retry-After = %d, want 60", retryAfter)
	}
	clock.Advance(59*time.Second + 500*time.Millisecond)
	if retryAfter := limiter.RetryAfter(address, "ADMIN"); retryAfter != 1 {
		t.Fatalf("RetryAfter() = %d, want ceiling 1", retryAfter)
	}
	clock.Advance(500 * time.Millisecond)
	if retryAfter := limiter.RetryAfter(address, "admin"); retryAfter != 0 {
		t.Fatalf("RetryAfter() at cooldown boundary = %d, want 0", retryAfter)
	}

	for range loginPairFailureLimit - 1 {
		limiter.RecordFailure(address, "admin")
	}
	clock.Advance(loginFailureWindow)
	if retryAfter := limiter.RecordFailure(address, "admin"); retryAfter != 0 {
		t.Fatalf("RecordFailure() after rolling-window expiry = %d, want 0", retryAfter)
	}
}

func TestLoginFailureLimiterEscalatesAndCapsCooldown(t *testing.T) {
	clock := newLoginLimiterClock()
	limiter := newLoginFailureLimiter(clock.Now)
	address := netip.MustParseAddr("198.51.100.20")
	want := []int{60, 120, 240, 480, 900, 900}

	for index, wantSeconds := range want {
		for range loginPairFailureLimit {
			limiter.RecordFailure(address, "admin")
		}
		if retryAfter := limiter.RetryAfter(address, "admin"); retryAfter != wantSeconds {
			t.Fatalf("cooldown %d Retry-After = %d, want %d", index, retryAfter, wantSeconds)
		}
		clock.Advance(time.Duration(wantSeconds) * time.Second)
	}
}

func TestLoginFailureLimiterGlobalRollingWindow(t *testing.T) {
	clock := newLoginLimiterClock()
	limiter := newLoginFailureLimiter(clock.Now)
	address := netip.MustParseAddr("203.0.113.30")

	for index := range loginGlobalFailureLimit {
		if retryAfter := limiter.RecordFailure(address, fmt.Sprintf("user-%d", index)); index < loginGlobalFailureLimit-1 && retryAfter != 0 {
			t.Fatalf("failure %d Retry-After = %d, want 0", index+1, retryAfter)
		}
	}
	if retryAfter := limiter.RetryAfter(address, "another-user"); retryAfter != 60 {
		t.Fatalf("global Retry-After = %d, want 60", retryAfter)
	}
	clock.Advance(59*time.Second + 500*time.Millisecond)
	if retryAfter := limiter.RetryAfter(address, "another-user"); retryAfter != 1 {
		t.Fatalf("global Retry-After ceiling = %d, want 1", retryAfter)
	}
	clock.Advance(500 * time.Millisecond)
	if retryAfter := limiter.RetryAfter(address, "another-user"); retryAfter != 0 {
		t.Fatalf("global Retry-After at window boundary = %d, want 0", retryAfter)
	}
}

func TestLoginFailureLimiterGlobalWindowKeepsMostRecentFailures(t *testing.T) {
	clock := newLoginLimiterClock()
	limiter := newLoginFailureLimiter(clock.Now)
	address := netip.MustParseAddr("203.0.113.31")

	for index := range loginGlobalFailureLimit {
		limiter.RecordFailure(address, fmt.Sprintf("user-%d", index))
	}
	clock.Advance(30 * time.Second)
	for index := range loginGlobalFailureLimit {
		limiter.RecordFailure(address, fmt.Sprintf("late-user-%d", index))
	}
	clock.Advance(30 * time.Second)
	if retryAfter := limiter.RetryAfter(address, "another-user"); retryAfter != 30 {
		t.Fatalf("global Retry-After after recent failure = %d, want 30", retryAfter)
	}
}

func TestLoginFailureLimiterOnlyFailuresConsumeAndSuccessPreservesGlobal(t *testing.T) {
	clock := newLoginLimiterClock()
	limiter := newLoginFailureLimiter(clock.Now)
	address := netip.MustParseAddr("192.0.2.40")

	for range 200 {
		if retryAfter := limiter.RetryAfter(address, "admin"); retryAfter != 0 {
			t.Fatalf("RetryAfter() without failures = %d, want 0", retryAfter)
		}
	}
	if len(limiter.entries) != 0 || len(limiter.globalFailures) != 0 {
		t.Fatalf("checks consumed state: entries=%d global=%d", len(limiter.entries), len(limiter.globalFailures))
	}

	for range loginPairFailureLimit {
		limiter.RecordFailure(address, "admin")
	}
	limiter.RecordSuccess(address, " ADMIN ")
	if retryAfter := limiter.RetryAfter(address, "admin"); retryAfter != 0 {
		t.Fatalf("RetryAfter() after success = %d, want cleared pair state", retryAfter)
	}
	if len(limiter.globalFailures) != loginPairFailureLimit {
		t.Fatalf("global failures after success = %d, want %d", len(limiter.globalFailures), loginPairFailureLimit)
	}

	for index := loginPairFailureLimit; index < loginGlobalFailureLimit; index++ {
		limiter.RecordFailure(address, fmt.Sprintf("other-%d", index))
	}
	if retryAfter := limiter.RetryAfter(address, "fresh-user"); retryAfter != 60 {
		t.Fatalf("global Retry-After after success = %d, want 60", retryAfter)
	}
}

func TestLoginFailureLimiterBoundsAndExpiresPairEntries(t *testing.T) {
	clock := newLoginLimiterClock()
	limiter := newLoginFailureLimiter(clock.Now)
	address := netip.MustParseAddr("198.51.100.50")

	for index := range loginFailureEntryCapacity {
		limiter.RecordFailure(address, fmt.Sprintf("user-%d", index))
	}
	limiter.RetryAfter(address, "user-0")
	limiter.RecordFailure(address, "overflow")
	if len(limiter.entries) != loginFailureEntryCapacity {
		t.Fatalf("entry count = %d, want %d", len(limiter.entries), loginFailureEntryCapacity)
	}
	if _, exists := limiter.entries[loginFailureKey(address, "user-0")]; !exists {
		t.Fatal("recently touched entry was evicted")
	}
	if _, exists := limiter.entries[loginFailureKey(address, "user-1")]; exists {
		t.Fatal("least recently used entry was not evicted")
	}

	clock.Advance(loginFailureEntryInactivity)
	limiter.RetryAfter(address, "not-recorded")
	if len(limiter.entries) != 0 {
		t.Fatalf("entry count after inactivity = %d, want 0", len(limiter.entries))
	}
}

func TestLoginFailureLimiterConcurrentFailuresAdvanceOneCooldown(t *testing.T) {
	clock := newLoginLimiterClock()
	limiter := newLoginFailureLimiter(clock.Now)
	address := netip.MustParseAddr("203.0.113.60")
	var wait sync.WaitGroup

	for range 200 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			limiter.RecordFailure(address, "admin")
		}()
	}
	wait.Wait()

	if retryAfter := limiter.RetryAfter(address, "admin"); retryAfter != 60 {
		t.Fatalf("RetryAfter() = %d, want one 60-second cooldown", retryAfter)
	}
	if len(limiter.entries) != 1 {
		t.Fatalf("entry count = %d, want 1", len(limiter.entries))
	}
	if len(limiter.globalFailures) != loginGlobalFailureLimit {
		t.Fatalf("global failures = %d, want bounded %d", len(limiter.globalFailures), loginGlobalFailureLimit)
	}
}

type loginLimiterClock struct {
	mu  sync.Mutex
	now time.Time
}

func newLoginLimiterClock() *loginLimiterClock {
	return &loginLimiterClock{now: time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)}
}

func (clock *loginLimiterClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *loginLimiterClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(duration)
}
