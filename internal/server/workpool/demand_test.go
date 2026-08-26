package workpool

import (
	"errors"
	"math"
	"sync"
	"testing"
	"time"
)

const (
	testLeaseID    = "lease_01J00000000000000000000000"
	testLeaseIDTwo = "lease_01J00000000000000000000001"
)

func TestDecideDemandClampsCoalescesExpiresAndCancels(t *testing.T) {
	clock := &fakeClock{}
	options := testOptions(8, 3)
	options.Clock = clock.Now
	pool, err := New(options)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	request := DemandRequest{
		DesiredNonActive: 10, BudgetSlots: 6, LeaseID: testLeaseID, LeaseTTL: 1500 * time.Millisecond,
	}
	first, emitted, err := pool.DecideDemand(request)
	if err != nil || !emitted {
		t.Fatalf("DecideDemand(first) = %#v, %v, %v, want emitted", first, emitted, err)
	}
	if first.Session != pool.Session() || first.Demand.BudgetLeaseID != testLeaseID ||
		first.Demand.DesiredNonActive != 8 || first.Demand.MaxNewConnections != 3 ||
		first.Demand.LeaseTTLMillis != 1500 || first.Demand.Generation != 1 ||
		first.Grant == nil || first.Grant.Session != pool.Session() || first.Grant.LeaseID != testLeaseID ||
		first.Grant.Slots != 3 || first.Grant.TTL != 1500*time.Millisecond || first.ReplacedLeaseID != "" {
		t.Fatalf("first DemandHandoff = %#v, want clamped generation 1 with matching grant", first)
	}

	request.LeaseID = testLeaseIDTwo
	if handoff, emitted, err := pool.DecideDemand(request); err != nil || emitted || handoff != (DemandHandoff{}) {
		t.Fatalf("DecideDemand(coalesced) = %#v, %v, %v, want no handoff", handoff, emitted, err)
	}
	clock.Advance(1500 * time.Millisecond)
	second, emitted, err := pool.DecideDemand(request)
	if err != nil || !emitted {
		t.Fatalf("DecideDemand(expired) = %#v, %v, %v, want replacement", second, emitted, err)
	}
	if second.Demand.Generation != 2 || second.Demand.BudgetLeaseID != testLeaseIDTwo ||
		second.ReplacedLeaseID != testLeaseID {
		t.Fatalf("expired replacement = %#v, want generation 2 replacing first Lease", second)
	}

	cancelled, emitted, err := pool.DecideDemand(DemandRequest{DesiredNonActive: 0})
	if err != nil || !emitted {
		t.Fatalf("DecideDemand(cancel) = %#v, %v, %v, want emitted cancellation", cancelled, emitted, err)
	}
	if cancelled.Demand.Generation != 3 || cancelled.Demand.DesiredNonActive != 0 ||
		cancelled.Grant != nil || cancelled.ReplacedLeaseID != testLeaseIDTwo {
		t.Fatalf("cancel handoff = %#v, want generation 3 without Grant", cancelled)
	}
	if _, emitted, err := pool.DecideDemand(DemandRequest{DesiredNonActive: 0}); err != nil || emitted {
		t.Fatalf("DecideDemand(repeated cancel) emitted=%v error=%v, want coalesced", emitted, err)
	}
}

func TestDecideDemandCanGrantWhenBudgetBecomesAvailable(t *testing.T) {
	pool := newTestPool(t, 8, 4)
	withoutBudget, emitted, err := pool.DecideDemand(DemandRequest{DesiredNonActive: 5})
	if err != nil || emitted || withoutBudget != (DemandHandoff{}) {
		t.Fatalf("DecideDemand(no budget) = %#v, %v, %v, want no premature Demand", withoutBudget, emitted, err)
	}
	withBudget, emitted, err := pool.DecideDemand(DemandRequest{
		DesiredNonActive: 5, BudgetSlots: 2, LeaseID: testLeaseID, LeaseTTL: time.Second,
	})
	if err != nil || !emitted || withBudget.Grant == nil || withBudget.Grant.Slots != 2 ||
		withBudget.Demand.Generation != 1 {
		t.Fatalf("DecideDemand(with budget) = %#v, %v, %v, want generation 1 grant", withBudget, emitted, err)
	}
}

func TestDecideDemandUsesLivePoolCapacity(t *testing.T) {
	pool := newTestPool(t, 4, 3)
	for index := 1; index <= 2; index++ {
		work := registerTestWork(t, pool, index, &recordingConn{})
		if err := work.MarkIdle(); err != nil {
			t.Fatalf("MarkIdle(%d) error = %v", index, err)
		}
		acquired, err := pool.Acquire(t.Context(), time.Second)
		if err != nil {
			t.Fatalf("Acquire(%d) error = %v", index, err)
		}
		if err := acquired.MarkActive(); err != nil {
			t.Fatalf("MarkActive(%d) error = %v", index, err)
		}
	}
	connecting := registerTestWork(t, pool, 3, &recordingConn{})
	handoff, emitted, err := pool.DecideDemand(DemandRequest{
		DesiredNonActive: 10, BudgetSlots: 10, LeaseID: testLeaseID, LeaseTTL: time.Second,
	})
	if err != nil || !emitted {
		t.Fatalf("DecideDemand() = %#v, %v, %v", handoff, emitted, err)
	}
	// 两条 ACTIVE 已占总上限，因此非 ACTIVE 目标钳制为 2；当前 CONNECTING=1，
	// 总容量也只剩 1，本轮最多只能再发放一个槽位。
	if handoff.Demand.DesiredNonActive != 2 || handoff.Demand.MaxNewConnections != 1 ||
		handoff.Grant == nil || handoff.Grant.Slots != 1 {
		t.Fatalf("capacity-clamped handoff = %#v, want desired 2 and one slot", handoff)
	}
	if err := connecting.Close(); err != nil {
		t.Fatalf("Close(connecting) error = %v", err)
	}
	if err := pool.Close(); err != nil {
		t.Fatalf("Close(pool) error = %v", err)
	}
}

func TestDecideDemandRejectsInvalidLeaseAndGenerationOverflow(t *testing.T) {
	tests := []struct {
		name    string
		request DemandRequest
	}{
		{name: "Lease ID", request: DemandRequest{DesiredNonActive: 1, BudgetSlots: 1, LeaseID: "lease_invalid", LeaseTTL: time.Second}},
		{name: "Zero TTL", request: DemandRequest{DesiredNonActive: 1, BudgetSlots: 1, LeaseID: testLeaseID}},
		{name: "Fractional millisecond", request: DemandRequest{DesiredNonActive: 1, BudgetSlots: 1, LeaseID: testLeaseID, LeaseTTL: time.Millisecond + time.Nanosecond}},
		{name: "TTL exceeds wire uint32", request: DemandRequest{DesiredNonActive: 1, BudgetSlots: 1, LeaseID: testLeaseID, LeaseTTL: (time.Duration(math.MaxUint32) + 1) * time.Millisecond}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pool := newTestPool(t, 4, 2)
			if _, emitted, err := pool.DecideDemand(test.request); !errors.Is(err, ErrInvalidDemand) || emitted {
				t.Fatalf("DecideDemand() emitted=%v error=%v, want ErrInvalidDemand", emitted, err)
			}
		})
	}

	pool := newTestPool(t, 4, 2)
	if _, emitted, err := pool.DecideDemand(DemandRequest{
		DesiredNonActive: 1, BudgetSlots: 1, LeaseID: testLeaseID, LeaseTTL: time.Second,
	}); err != nil || !emitted {
		t.Fatalf("DecideDemand(initial Lease) emitted=%v error=%v", emitted, err)
	}
	if _, emitted, err := pool.DecideDemand(DemandRequest{
		DesiredNonActive: 2, BudgetSlots: 1, LeaseID: testLeaseID, LeaseTTL: time.Second,
	}); !errors.Is(err, ErrInvalidDemand) || emitted {
		t.Fatalf("DecideDemand(reused active Lease ID) emitted=%v error=%v, want ErrInvalidDemand", emitted, err)
	}

	clock := &fakeClock{now: time.Duration(math.MaxInt64) - time.Millisecond}
	options := testOptions(4, 2)
	options.Clock = clock.Now
	overflowPool, err := New(options)
	if err != nil {
		t.Fatalf("New(overflow clock) error = %v", err)
	}
	if _, emitted, err := overflowPool.DecideDemand(DemandRequest{
		DesiredNonActive: 1, BudgetSlots: 1, LeaseID: testLeaseID, LeaseTTL: 2 * time.Millisecond,
	}); !errors.Is(err, ErrInvalidDemand) || emitted {
		t.Fatalf("DecideDemand(deadline overflow) emitted=%v error=%v, want ErrInvalidDemand", emitted, err)
	}

	pool = newTestPool(t, 4, 2)
	pool.mu.Lock()
	pool.demand.generation = math.MaxUint64
	pool.mu.Unlock()
	if _, emitted, err := pool.DecideDemand(DemandRequest{
		DesiredNonActive: 1, BudgetSlots: 1, LeaseID: testLeaseID, LeaseTTL: time.Second,
	}); !errors.Is(err, ErrDemandGenerationExhausted) || emitted {
		t.Fatalf("DecideDemand(generation overflow) emitted=%v error=%v", emitted, err)
	}
}

func TestConcurrentDemandIsCoalescedToOneGeneration(t *testing.T) {
	const callers = 64
	pool := newTestPool(t, 16, 8)
	results := make(chan struct {
		emitted bool
		err     error
	}, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, emitted, err := pool.DecideDemand(DemandRequest{
				DesiredNonActive: 8, BudgetSlots: 8, LeaseID: testLeaseID, LeaseTTL: time.Second,
			})
			results <- struct {
				emitted bool
				err     error
			}{emitted: emitted, err: err}
		}()
	}
	wait.Wait()
	close(results)
	emittedCount := 0
	for result := range results {
		if result.err != nil {
			t.Errorf("DecideDemand() error = %v", result.err)
		}
		if result.emitted {
			emittedCount++
		}
	}
	if emittedCount != 1 {
		t.Fatalf("emitted Demand count = %d, want 1", emittedCount)
	}
	pool.mu.Lock()
	generation := pool.demand.generation
	pool.mu.Unlock()
	if generation != 1 {
		t.Fatalf("Demand generation = %d, want 1", generation)
	}
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Duration
}

func (clock *fakeClock) Now() time.Duration {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now += duration
}
