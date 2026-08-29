package managementapi

import (
	"container/list"
	"crypto/sha256"
	"encoding/binary"
	"net/netip"
	"strings"
	"sync"
	"time"
)

const (
	loginFailureWindow          = time.Minute
	loginPairFailureLimit       = 5
	loginGlobalFailureLimit     = 100
	loginFailureEntryCapacity   = 4096
	loginFailureEntryInactivity = 30 * time.Minute
)

var loginCooldowns = [...]time.Duration{
	time.Minute,
	2 * time.Minute,
	4 * time.Minute,
	8 * time.Minute,
	15 * time.Minute,
}

// loginFailureLimiter 只记录已经完成密码校验的失败请求。RetryAfter 本身不消费
// 预算，成功登录只清理对应 Client/Username 的连续失败状态，不改变 Server 全局窗口。
// 所有状态由同一把锁线性化；锁内只操作内存，禁止日志、数据库或其他外部调用。
type loginFailureLimiter struct {
	mu sync.Mutex

	now func() time.Time

	entries map[[sha256.Size]byte]*loginFailureEntry
	lru     list.List

	globalFailures []time.Time
}

type loginFailureEntry struct {
	key [sha256.Size]byte

	failures      []time.Time
	cooldownStep  int
	cooldownUntil time.Time
	lastActive    time.Time

	element *list.Element
}

func newLoginFailureLimiter(now func() time.Time) *loginFailureLimiter {
	return &loginFailureLimiter{
		now:            now,
		entries:        make(map[[sha256.Size]byte]*loginFailureEntry),
		globalFailures: make([]time.Time, 0, loginGlobalFailureLimit),
	}
}

// RetryAfter 返回当前请求必须等待的整秒数；0 表示可以继续认证。组合冷却与
// Server 全局窗口同时生效时返回较长者，保证调用方等待后不会立刻再次命中另一层限制。
func (limiter *loginFailureLimiter) RetryAfter(clientIP netip.Addr, username string) int {
	now := limiter.now()
	key := loginFailureKey(clientIP, username)

	limiter.mu.Lock()
	defer limiter.mu.Unlock()

	limiter.expire(now)
	retryUntil := limiter.globalRetryUntil(now)
	if entry := limiter.entries[key]; entry != nil {
		limiter.touch(entry, now)
		if entry.cooldownUntil.After(retryUntil) {
			retryUntil = entry.cooldownUntil
		}
	}
	return retryAfterSeconds(now, retryUntil)
}

// RecordFailure 记录一次已经确认的认证失败，并返回记录后生效的 Retry-After 秒数。
// 达到组合阈值时才推进一次冷却级别；冷却期间并发完成的失败不会重复升级同一轮冷却。
func (limiter *loginFailureLimiter) RecordFailure(clientIP netip.Addr, username string) int {
	now := limiter.now()
	key := loginFailureKey(clientIP, username)

	limiter.mu.Lock()
	defer limiter.mu.Unlock()

	limiter.expire(now)
	if len(limiter.globalFailures) == loginGlobalFailureLimit {
		copy(limiter.globalFailures, limiter.globalFailures[1:])
		limiter.globalFailures[len(limiter.globalFailures)-1] = now
	} else {
		limiter.globalFailures = append(limiter.globalFailures, now)
	}

	entry := limiter.entries[key]
	if entry == nil {
		if len(limiter.entries) == loginFailureEntryCapacity {
			limiter.remove(limiter.lru.Back())
		}
		entry = &loginFailureEntry{
			key:        key,
			failures:   make([]time.Time, 0, loginPairFailureLimit),
			lastActive: now,
		}
		entry.element = limiter.lru.PushFront(entry)
		limiter.entries[key] = entry
	} else {
		limiter.touch(entry, now)
	}

	if !now.Before(entry.cooldownUntil) {
		entry.failures = pruneLoginFailures(entry.failures, now)
		entry.failures = append(entry.failures, now)
		if len(entry.failures) == loginPairFailureLimit {
			entry.cooldownUntil = now.Add(loginCooldowns[entry.cooldownStep])
			if entry.cooldownStep < len(loginCooldowns)-1 {
				entry.cooldownStep++
			}
			entry.failures = entry.failures[:0]
		}
	}

	retryUntil := limiter.globalRetryUntil(now)
	if entry.cooldownUntil.After(retryUntil) {
		retryUntil = entry.cooldownUntil
	}
	return retryAfterSeconds(now, retryUntil)
}

// RecordSuccess 清理对应组合的连续失败与冷却状态。全局失败预算用于抵御大量随机
// 用户名攻击，成功登录不得将它清零。
func (limiter *loginFailureLimiter) RecordSuccess(clientIP netip.Addr, username string) {
	now := limiter.now()
	key := loginFailureKey(clientIP, username)

	limiter.mu.Lock()
	defer limiter.mu.Unlock()

	limiter.expire(now)
	if entry := limiter.entries[key]; entry != nil {
		limiter.remove(entry.element)
	}
}

func (limiter *loginFailureLimiter) expire(now time.Time) {
	limiter.globalFailures = pruneLoginFailures(limiter.globalFailures, now)
	cutoff := now.Add(-loginFailureEntryInactivity)
	for element := limiter.lru.Back(); element != nil; element = limiter.lru.Back() {
		entry := element.Value.(*loginFailureEntry)
		if entry.lastActive.After(cutoff) {
			return
		}
		limiter.remove(element)
	}
}

func (limiter *loginFailureLimiter) globalRetryUntil(now time.Time) time.Time {
	if len(limiter.globalFailures) < loginGlobalFailureLimit {
		return time.Time{}
	}
	return limiter.globalFailures[0].Add(loginFailureWindow)
}

func (limiter *loginFailureLimiter) touch(entry *loginFailureEntry, now time.Time) {
	entry.lastActive = now
	limiter.lru.MoveToFront(entry.element)
}

func (limiter *loginFailureLimiter) remove(element *list.Element) {
	entry := element.Value.(*loginFailureEntry)
	delete(limiter.entries, entry.key)
	limiter.lru.Remove(element)
}

func pruneLoginFailures(failures []time.Time, now time.Time) []time.Time {
	cutoff := now.Add(-loginFailureWindow)
	first := 0
	for first < len(failures) && !failures[first].After(cutoff) {
		first++
	}
	if first == len(failures) {
		return failures[:0]
	}
	return failures[first:]
}

func loginFailureKey(clientIP netip.Addr, username string) [sha256.Size]byte {
	clientIP = clientIP.WithZone("").Unmap()
	normalizedUsername := strings.ToLower(strings.TrimSpace(username))
	hash := sha256.New()
	hash.Write([]byte("xtunnel-management-login-failure-v1\x00"))
	address := clientIP.AsSlice()
	var addressLength [2]byte
	binary.BigEndian.PutUint16(addressLength[:], uint16(len(address)))
	hash.Write(addressLength[:])
	hash.Write(address)
	hash.Write([]byte(normalizedUsername))
	var key [sha256.Size]byte
	copy(key[:], hash.Sum(nil))
	return key
}

func retryAfterSeconds(now, retryUntil time.Time) int {
	if !retryUntil.After(now) {
		return 0
	}
	remaining := retryUntil.Sub(now)
	seconds := int((remaining + time.Second - 1) / time.Second)
	return max(1, seconds)
}
