package limits

import (
	"container/list"
	"net/netip"
	"sync"
	"time"
)

const maxSourceRateShards = 32

// sourceRateLimiter 将来源 IP 状态分散到最多 32 个分片，并在每个分片内用
// LRU + TTL 限制内存占用。每次 Allow 只持有一个分片锁；锁内不执行 IO，也不
// 回调其他 owner，因此不会把公网连接路径串行化到 Manager 的资源计数锁上。
type sourceRateLimiter struct {
	shards []sourceRateShard
	rate   float64
	burst  float64
	ttl    time.Duration
	now    func() time.Time
}

type sourceRateShard struct {
	mu       sync.Mutex
	capacity uint64
	entries  map[netip.Addr]*sourceRateEntry
	lru      list.List
}

type sourceRateEntry struct {
	address  netip.Addr
	tokens   float64
	updated  time.Time
	lastSeen time.Time
	element  *list.Element
}

func newSourceRateLimiter(rate, burst, capacity uint64, now func() time.Time) *sourceRateLimiter {
	shardCount := min(capacity, uint64(maxSourceRateShards))
	shards := make([]sourceRateShard, int(shardCount))
	baseCapacity := capacity / shardCount
	extraCapacity := capacity % shardCount
	for index := range shards {
		shards[index].capacity = baseCapacity
		if uint64(index) < extraCapacity {
			shards[index].capacity++
		}
		shards[index].entries = make(map[netip.Addr]*sourceRateEntry)
	}
	refillSeconds := burst / rate
	if burst%rate != 0 {
		refillSeconds++
	}
	if refillSeconds == 0 {
		refillSeconds = 1
	}
	return &sourceRateLimiter{
		shards: shards,
		rate:   float64(rate),
		burst:  float64(burst),
		ttl:    time.Duration(refillSeconds) * time.Second,
		now:    now,
	}
}

func (limiter *sourceRateLimiter) allow(address netip.Addr) bool {
	address = address.Unmap()
	shard := &limiter.shards[hashAddress(address)%uint64(len(limiter.shards))]
	now := limiter.now()

	shard.mu.Lock()
	defer shard.mu.Unlock()
	shard.expire(now, limiter.ttl)

	entry := shard.entries[address]
	if entry == nil {
		if uint64(len(shard.entries)) >= shard.capacity {
			shard.remove(shard.lru.Back())
		}
		entry = &sourceRateEntry{
			address:  address,
			tokens:   limiter.burst,
			updated:  now,
			lastSeen: now,
		}
		entry.element = shard.lru.PushFront(entry)
		shard.entries[address] = entry
	} else {
		elapsed := now.Sub(entry.updated)
		if elapsed > 0 {
			entry.tokens = min(limiter.burst, entry.tokens+elapsed.Seconds()*limiter.rate)
			entry.updated = now
		}
		entry.lastSeen = now
		shard.lru.MoveToFront(entry.element)
	}
	if entry.tokens < 1 {
		return false
	}
	entry.tokens--
	return true
}

func (shard *sourceRateShard) expire(now time.Time, ttl time.Duration) {
	for element := shard.lru.Back(); element != nil; element = shard.lru.Back() {
		entry := element.Value.(*sourceRateEntry)
		if now.Sub(entry.lastSeen) < ttl {
			return
		}
		shard.remove(element)
	}
}

func (shard *sourceRateShard) remove(element *list.Element) {
	if element == nil {
		return
	}
	entry := element.Value.(*sourceRateEntry)
	delete(shard.entries, entry.address)
	shard.lru.Remove(element)
}

func hashAddress(address netip.Addr) uint64 {
	bytes := address.As16()
	hash := uint64(14695981039346656037)
	for _, value := range bytes {
		hash ^= uint64(value)
		hash *= 1099511628211
	}
	return hash
}
