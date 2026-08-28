package limits

import (
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"
)

func TestSourceRateLimiterRefillsTokenBucket(t *testing.T) {
	now := time.Unix(100, 0)
	limiter := newSourceRateLimiter(2, 2, 4, func() time.Time { return now })
	address := netip.MustParseAddr("192.0.2.1")

	if !limiter.allow(address) || !limiter.allow(address) {
		t.Fatal("initial burst was not available")
	}
	if limiter.allow(address) {
		t.Fatal("third request unexpectedly exceeded the burst")
	}
	now = now.Add(500 * time.Millisecond)
	if !limiter.allow(address) {
		t.Fatal("one token was not refilled after half a second")
	}
	if limiter.allow(address) {
		t.Fatal("bucket refilled more than one token")
	}
}

func TestSourceRateLimiterExpiresFullyRefilledEntry(t *testing.T) {
	now := time.Unix(100, 0)
	limiter := newSourceRateLimiter(1, 1, 1, func() time.Time { return now })
	first := netip.MustParseAddr("192.0.2.1")
	second := netip.MustParseAddr("192.0.2.2")

	if !limiter.allow(first) {
		t.Fatal("first source was rejected")
	}
	now = now.Add(time.Second)
	if !limiter.allow(second) {
		t.Fatal("second source was rejected after TTL expiry")
	}
	shard := &limiter.shards[0]
	if len(shard.entries) != 1 || shard.entries[second] == nil {
		t.Fatalf("entries after expiry = %#v, want only second source", shard.entries)
	}
}

func TestSourceRateLimiterEvictsLeastRecentlyUsedEntry(t *testing.T) {
	now := time.Unix(100, 0)
	limiter := newSourceRateLimiter(1, 1, 64, func() time.Time { return now })
	addresses := addressesForShard(t, limiter, 0, 3)

	for _, address := range addresses[:2] {
		if !limiter.allow(address) {
			t.Fatalf("allow(%s) = false, want true", address)
		}
		now = now.Add(time.Millisecond)
	}
	if limiter.allow(addresses[0]) {
		t.Fatal("drained source unexpectedly received a token")
	}
	now = now.Add(time.Millisecond)
	if !limiter.allow(addresses[2]) {
		t.Fatalf("allow(%s) = false, want true", addresses[2])
	}
	shard := &limiter.shards[0]
	if shard.entries[addresses[0]] == nil || shard.entries[addresses[1]] != nil || shard.entries[addresses[2]] == nil {
		t.Fatalf("LRU entries = %#v, want first and third sources", shard.entries)
	}
}

func TestSourceRateLimiterUsesBoundedShardCapacity(t *testing.T) {
	limiter := newSourceRateLimiter(1, 1, 65, time.Now)
	if got := len(limiter.shards); got != maxSourceRateShards {
		t.Fatalf("shard count = %d, want %d", got, maxSourceRateShards)
	}
	var total uint64
	for index := range limiter.shards {
		total += limiter.shards[index].capacity
	}
	if total != 65 {
		t.Fatalf("total shard capacity = %d, want 65", total)
	}
}

func TestManagerRateLimitsReturnStableErrors(t *testing.T) {
	manager := newTestManager(t, openOptions(1, 1, 1, 1))
	address := netip.MustParseAddr("::ffff:192.0.2.10")

	for range manager.options.MaxOpenBurstPerSourceIP {
		if err := manager.AllowOpen(address); err != nil {
			t.Fatalf("AllowOpen() error = %v", err)
		}
	}
	if err := manager.AllowOpen(address); !errors.Is(err, ErrOpenRateExceeded) {
		t.Fatalf("AllowOpen() error = %v, want ErrOpenRateExceeded", err)
	}
	for range manager.options.MaxHTTPRequestsPerSourceIPPerSecond {
		if err := manager.AllowHTTPRequest(address); err != nil {
			t.Fatalf("AllowHTTPRequest() error = %v", err)
		}
	}
	if err := manager.AllowHTTPRequest(address); !errors.Is(err, ErrHTTPRequestRateExceeded) {
		t.Fatalf("AllowHTTPRequest() error = %v, want ErrHTTPRequestRateExceeded", err)
	}
	if err := manager.AllowOpen(netip.Addr{}); !errors.Is(err, ErrInvalidConnectionKey) {
		t.Fatalf("AllowOpen(invalid address) error = %v, want ErrInvalidConnectionKey", err)
	}
}

func addressesForShard(t *testing.T, limiter *sourceRateLimiter, wanted, count uint64) []netip.Addr {
	t.Helper()
	addresses := make([]netip.Addr, 0, count)
	for index := 1; len(addresses) < int(count) && index < 255; index++ {
		address := netip.MustParseAddr(fmt.Sprintf("192.0.2.%d", index))
		if hashAddress(address)%uint64(len(limiter.shards)) == wanted {
			addresses = append(addresses, address)
		}
	}
	if len(addresses) != int(count) {
		t.Fatalf("found %d addresses for shard %d, want %d", len(addresses), wanted, count)
	}
	return addresses
}
