package usage

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"
)

const (
	testTunnelA  = "tun_01J00000000000000000000000"
	testTunnelB  = "tun_01J00000000000000000000001"
	testServiceA = "svc_01J00000000000000000000000"
	testServiceB = "svc_01J00000000000000000000001"
)

func TestNewAndAddValidation(t *testing.T) {
	if _, err := New(Options{}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("New() error = %v, want ErrInvalidOptions", err)
	}
	repository := &fakeRepository{}
	owner := newTestOwner(t, repository)
	tests := []struct {
		name    string
		record  func() error
		wantErr error
	}{
		{name: "missing tunnel", record: func() error { return owner.ObserveOpen("", testServiceA, true) }, wantErr: ErrInvalidKey},
		{name: "missing service", record: func() error { return owner.AddIngressBytes(testTunnelA, "", 1) }, wantErr: ErrInvalidKey},
		{name: "invalid nonempty tunnel", record: func() error { return owner.ObserveOpen("tun-not-ulid", testServiceA, true) }, wantErr: ErrInvalidKey},
		{name: "invalid nonempty service", record: func() error { return owner.AddEgressBytes(testTunnelA, "svc-not-ulid", 1) }, wantErr: ErrInvalidKey},
		{name: "zero ingress", record: func() error { return owner.AddIngressBytes(testTunnelA, testServiceA, 0) }},
		{name: "zero egress", record: func() error { return owner.AddEgressBytes(testTunnelA, testServiceA, 0) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.record(); !errors.Is(err, test.wantErr) {
				t.Fatalf("record error = %v, want %v", err, test.wantErr)
			}
		})
	}
	if err := owner.Flush(t.Context()); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	if got := repository.flushCount(); got != 0 {
		t.Fatalf("Repository Flush calls = %d, want zero", got)
	}
}

func TestFlushBuildsSortedMinuteDeltas(t *testing.T) {
	repository := &fakeRepository{}
	owner := newTestOwner(t, repository)
	owner.now = func() time.Time { return time.Date(2026, 8, 30, 12, 34, 56, 0, time.FixedZone("test", 8*60*60)) }

	if err := owner.ObserveOpen(testTunnelB, testServiceA, true); err != nil {
		t.Fatalf("ObserveOpen() error = %v", err)
	}
	if err := owner.AddIngressBytes(testTunnelB, testServiceA, 2); err != nil {
		t.Fatalf("AddIngressBytes() error = %v", err)
	}
	if err := owner.ObserveOpen(testTunnelA, testServiceB, false); err != nil {
		t.Fatalf("failed ObserveOpen() error = %v", err)
	}
	if err := owner.AddEgressBytes(testTunnelA, testServiceB, 3); err != nil {
		t.Fatalf("AddEgressBytes() error = %v", err)
	}
	for range 2 {
		if err := owner.ObserveOpen(testTunnelA, testServiceB, true); err != nil {
			t.Fatalf("successful ObserveOpen() error = %v", err)
		}
	}
	if err := owner.AddIngressBytes(testTunnelA, testServiceB, 4); err != nil {
		t.Fatalf("second AddIngressBytes() error = %v", err)
	}
	if err := owner.Flush(t.Context()); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}

	batch := repository.lastBatch(t)
	if len(batch) != 2 {
		t.Fatalf("batch length = %d, want 2", len(batch))
	}
	wantBucket := time.Date(2026, 8, 30, 4, 34, 0, 0, time.UTC)
	if batch[0].BucketTime != wantBucket || batch[0].TunnelID != testTunnelA || batch[0].ServiceID != testServiceB ||
		batch[0].Connections != 2 || batch[0].IngressBytes != 4 || batch[0].EgressBytes != 3 || batch[0].Errors != 1 {
		t.Fatalf("first delta = %#v", batch[0])
	}
	if batch[1].BucketTime != wantBucket || batch[1].TunnelID != testTunnelB || batch[1].ServiceID != testServiceA ||
		batch[1].Connections != 1 || batch[1].IngressBytes != 2 {
		t.Fatalf("second delta = %#v", batch[1])
	}
}

func TestConcurrentAddIsExactlyCounted(t *testing.T) {
	repository := &fakeRepository{}
	owner := newTestOwner(t, repository)
	owner.now = func() time.Time { return time.Date(2026, 8, 30, 12, 34, 56, 0, time.UTC) }
	const goroutines = 24
	const increments = 2_000
	var wait sync.WaitGroup
	for range goroutines {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range increments {
				if err := owner.ObserveOpen(testTunnelA, testServiceA, true); err != nil {
					t.Errorf("ObserveOpen(success) error = %v", err)
					return
				}
				if err := owner.ObserveOpen(testTunnelA, testServiceA, false); err != nil {
					t.Errorf("ObserveOpen(failure) error = %v", err)
					return
				}
				if err := owner.AddIngressBytes(testTunnelA, testServiceA, 2); err != nil {
					t.Errorf("AddIngressBytes() error = %v", err)
					return
				}
				if err := owner.AddEgressBytes(testTunnelA, testServiceA, 3); err != nil {
					t.Errorf("AddEgressBytes() error = %v", err)
					return
				}
			}
		}()
	}
	wait.Wait()
	if err := owner.Flush(t.Context()); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	batch := repository.lastBatch(t)
	if len(batch) != 1 {
		t.Fatalf("batch length = %d, want 1", len(batch))
	}
	delta := batch[0]
	want := uint64(goroutines * increments)
	if delta.Connections != want || delta.IngressBytes != want*2 || delta.EgressBytes != want*3 || delta.Errors != want {
		t.Fatalf("concurrent delta = %#v, want base %d", delta, want)
	}
}

func TestFailedFlushMergesConcurrentIncrementForRetry(t *testing.T) {
	flushStarted := make(chan struct{})
	releaseFailure := make(chan struct{})
	var calls int
	repository := &fakeRepository{flushFunc: func(context.Context, []Delta) error {
		calls++
		if calls == 1 {
			close(flushStarted)
			<-releaseFailure
			return errors.New("temporary repository failure")
		}
		return nil
	}}
	owner := newTestOwner(t, repository)
	if err := owner.AddIngressBytes(testTunnelA, testServiceA, 5); err != nil {
		t.Fatalf("AddIngressBytes() error = %v", err)
	}
	flushResult := make(chan error, 1)
	go func() { flushResult <- owner.Flush(context.Background()) }()
	<-flushStarted
	if err := owner.AddIngressBytes(testTunnelA, testServiceA, 7); err != nil {
		t.Fatalf("concurrent AddIngressBytes() error = %v", err)
	}
	close(releaseFailure)
	if err := <-flushResult; err == nil {
		t.Fatal("first Flush() error = nil, want repository failure")
	}
	if err := owner.Flush(t.Context()); err != nil {
		t.Fatalf("retry Flush() error = %v", err)
	}
	batches := repository.batchesSnapshot()
	if len(batches) != 2 || batches[0][0].IngressBytes != 5 || batches[1][0].IngressBytes != 12 {
		t.Fatalf("flush batches = %#v, want failed 5 then retried 12", batches)
	}
}

func TestPendingBucketCapacityIncludesInFlightBatch(t *testing.T) {
	flushStarted := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	repository := &fakeRepository{flushFunc: func(context.Context, []Delta) error {
		once.Do(func() {
			close(flushStarted)
			<-release
		})
		return nil
	}}
	owner := newTestOwner(t, repository)
	owner.maxBuckets = 1
	if err := owner.AddIngressBytes(testTunnelA, testServiceA, 1); err != nil {
		t.Fatalf("first AddIngressBytes() error = %v", err)
	}
	flushResult := make(chan error, 1)
	go func() { flushResult <- owner.Flush(context.Background()) }()
	<-flushStarted
	if err := owner.AddIngressBytes(testTunnelA, testServiceA, 2); err != nil {
		t.Fatalf("AddIngressBytes() for in-flight key error = %v", err)
	}
	if err := owner.AddIngressBytes(testTunnelB, testServiceB, 1); !errors.Is(err, ErrCapacity) {
		t.Fatalf("AddIngressBytes() beyond capacity error = %v, want ErrCapacity", err)
	}
	close(release)
	if err := <-flushResult; err != nil {
		t.Fatalf("first Flush() error = %v", err)
	}
	if err := owner.Flush(t.Context()); err != nil {
		t.Fatalf("second Flush() error = %v", err)
	}
	if err := owner.AddIngressBytes(testTunnelB, testServiceB, 1); err != nil {
		t.Fatalf("AddIngressBytes() after capacity released error = %v", err)
	}
}

func TestCountersSaturateWithoutWrapping(t *testing.T) {
	tests := []struct {
		name      string
		first     counters
		second    counters
		assertion func(Delta) uint64
	}{
		{name: "connections", first: counters{Connections: maxCounter}, second: counters{Connections: 1}, assertion: func(delta Delta) uint64 { return delta.Connections }},
		{name: "ingress", first: counters{IngressBytes: math.MaxUint64}, second: counters{IngressBytes: 1}, assertion: func(delta Delta) uint64 { return delta.IngressBytes }},
		{name: "egress", first: counters{EgressBytes: maxCounter - 1}, second: counters{EgressBytes: 2}, assertion: func(delta Delta) uint64 { return delta.EgressBytes }},
		{name: "errors", first: counters{Errors: maxCounter}, second: counters{Errors: maxCounter}, assertion: func(delta Delta) uint64 { return delta.Errors }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := &fakeRepository{}
			owner := newTestOwner(t, repository)
			_ = owner.add(testTunnelA, testServiceA, test.first)
			if err := owner.add(testTunnelA, testServiceA, test.second); !errors.Is(err, ErrCounterOverflow) {
				t.Fatalf("overflow add() error = %v, want ErrCounterOverflow", err)
			}
			if err := owner.Flush(t.Context()); err != nil {
				t.Fatalf("Flush() error = %v", err)
			}
			if got := test.assertion(repository.lastBatch(t)[0]); got != maxCounter {
				t.Fatalf("saturated value = %d, want %d", got, maxCounter)
			}
		})
	}
}

func TestFailedFlushMergeReportsOverflow(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	repository := &fakeRepository{flushFunc: func(context.Context, []Delta) error {
		close(started)
		<-release
		return errors.New("flush failed")
	}}
	owner := newTestOwner(t, repository)
	if err := owner.add(testTunnelA, testServiceA, counters{Errors: maxCounter}); err != nil {
		t.Fatalf("add() error = %v", err)
	}
	result := make(chan error, 1)
	go func() { result <- owner.Flush(context.Background()) }()
	<-started
	if err := owner.add(testTunnelA, testServiceA, counters{Errors: 1}); err != nil {
		t.Fatalf("concurrent add() error = %v", err)
	}
	close(release)
	if err := <-result; !errors.Is(err, ErrCounterOverflow) {
		t.Fatalf("Flush() error = %v, want ErrCounterOverflow", err)
	}
}

func TestStartPeriodicallyFlushesAndParentCancellationStopsOwner(t *testing.T) {
	flushed := make(chan struct{})
	var once sync.Once
	repository := &fakeRepository{flushFunc: func(context.Context, []Delta) error {
		once.Do(func() { close(flushed) })
		return nil
	}}
	owner := newTestOwner(t, repository)
	owner.flushEvery = 5 * time.Millisecond
	parent, cancelParent := context.WithCancel(context.Background())
	if err := owner.Start(parent); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := owner.Start(parent); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("second Start() error = %v, want ErrAlreadyStarted", err)
	}
	if err := owner.ObserveOpen(testTunnelA, testServiceA, true); err != nil {
		t.Fatalf("ObserveOpen() error = %v", err)
	}
	select {
	case <-flushed:
	case <-time.After(time.Second):
		t.Fatal("periodic Flush did not run")
	}
	cancelParent()
	select {
	case <-owner.done:
	case <-time.After(time.Second):
		t.Fatal("Usage owner did not stop after parent cancellation")
	}
	if err := owner.AddEgressBytes(testTunnelA, testServiceA, 2); err != nil {
		t.Fatalf("AddEgressBytes during graceful drain error = %v", err)
	}
	if err := owner.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if got := repository.totalEgress(); got != 2 {
		t.Fatalf("persisted egress after parent cancellation = %d, want 2", got)
	}
}

func TestShutdownFlushesAndRollsUpExactlyOnce(t *testing.T) {
	repository := &fakeRepository{}
	owner := newTestOwner(t, repository)
	owner.now = func() time.Time { return time.Date(2026, 8, 30, 12, 34, 56, 0, time.UTC) }
	if err := owner.ObserveOpen(testTunnelA, testServiceA, true); err != nil {
		t.Fatalf("ObserveOpen() error = %v", err)
	}
	if err := owner.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if err := owner.Shutdown(t.Context()); err != nil {
		t.Fatalf("second Shutdown() error = %v", err)
	}
	if repository.flushCount() != 1 || repository.rollupCount() != 1 {
		t.Fatalf("Repository calls = flush:%d rollup:%d, want 1/1", repository.flushCount(), repository.rollupCount())
	}
	if got := repository.lastRollup(t); got != time.Date(2026, 8, 30, 12, 34, 0, 0, time.UTC) {
		t.Fatalf("Rollup completedBefore = %s", got)
	}
	if err := owner.ObserveOpen(testTunnelA, testServiceA, true); !errors.Is(err, ErrStopped) {
		t.Fatalf("ObserveOpen after Shutdown() error = %v, want ErrStopped", err)
	}
	if err := owner.Start(t.Context()); !errors.Is(err, ErrStopped) {
		t.Fatalf("Start after Shutdown() error = %v, want ErrStopped", err)
	}
}

func TestShutdownHonorsContextAndCachesResult(t *testing.T) {
	repository := &fakeRepository{flushFunc: func(ctx context.Context, _ []Delta) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	owner := newTestOwner(t, repository)
	owner.operationLimit = time.Second
	if err := owner.ObserveOpen(testTunnelA, testServiceA, true); err != nil {
		t.Fatalf("ObserveOpen() error = %v", err)
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := owner.Shutdown(shutdownContext)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown() error = %v, want deadline exceeded", err)
	}
	if second := owner.Shutdown(context.Background()); !errors.Is(second, context.DeadlineExceeded) {
		t.Fatalf("second Shutdown() error = %v, want cached deadline exceeded", second)
	}
	if repository.flushCount() != 1 {
		t.Fatalf("Repository Flush calls = %d, want 1", repository.flushCount())
	}
}

func TestPeriodicFailureIsReportedAndRetried(t *testing.T) {
	reported := make(chan error, 1)
	var calls int
	repository := &fakeRepository{flushFunc: func(context.Context, []Delta) error {
		calls++
		if calls == 1 {
			return errors.New("temporary failure")
		}
		return nil
	}}
	owner, err := New(Options{Repository: repository, ReportError: func(err error) { reported <- err }})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	owner.flushEvery = 5 * time.Millisecond
	if err := owner.ObserveOpen(testTunnelA, testServiceA, true); err != nil {
		t.Fatalf("ObserveOpen() error = %v", err)
	}
	if err := owner.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case err := <-reported:
		if err == nil {
			t.Fatal("reported error = nil")
		}
	case <-time.After(time.Second):
		t.Fatal("periodic failure was not reported")
	}
	deadline := time.Now().Add(time.Second)
	for repository.flushCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if repository.flushCount() < 2 {
		t.Fatal("failed periodic batch was not retried")
	}
	if err := owner.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

type fakeRepository struct {
	mu         sync.Mutex
	batches    [][]Delta
	rollups    []time.Time
	flushFunc  func(context.Context, []Delta) error
	rollupFunc func(context.Context, time.Time) error
}

func (repository *fakeRepository) Flush(ctx context.Context, deltas []Delta) error {
	copyOfDeltas := append([]Delta(nil), deltas...)
	repository.mu.Lock()
	repository.batches = append(repository.batches, copyOfDeltas)
	function := repository.flushFunc
	repository.mu.Unlock()
	if function != nil {
		return function(ctx, copyOfDeltas)
	}
	return nil
}

func (repository *fakeRepository) Rollup(ctx context.Context, completedBefore time.Time) error {
	repository.mu.Lock()
	repository.rollups = append(repository.rollups, completedBefore)
	function := repository.rollupFunc
	repository.mu.Unlock()
	if function != nil {
		return function(ctx, completedBefore)
	}
	return nil
}

func (repository *fakeRepository) flushCount() int {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	return len(repository.batches)
}

func (repository *fakeRepository) rollupCount() int {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	return len(repository.rollups)
}

func (repository *fakeRepository) batchesSnapshot() [][]Delta {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	batches := make([][]Delta, len(repository.batches))
	for index := range repository.batches {
		batches[index] = append([]Delta(nil), repository.batches[index]...)
	}
	return batches
}

func (repository *fakeRepository) lastBatch(t *testing.T) []Delta {
	t.Helper()
	batches := repository.batchesSnapshot()
	if len(batches) == 0 {
		t.Fatal("Repository has no Flush batch")
	}
	return batches[len(batches)-1]
}

func (repository *fakeRepository) lastRollup(t *testing.T) time.Time {
	t.Helper()
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if len(repository.rollups) == 0 {
		t.Fatal("Repository has no Rollup call")
	}
	return repository.rollups[len(repository.rollups)-1]
}

func (repository *fakeRepository) totalEgress() uint64 {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	var total uint64
	for _, batch := range repository.batches {
		for _, delta := range batch {
			total += delta.EgressBytes
		}
	}
	return total
}

func newTestOwner(t *testing.T, repository Repository) *Owner {
	t.Helper()
	owner, err := New(Options{Repository: repository})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return owner
}
