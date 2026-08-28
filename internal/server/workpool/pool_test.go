package workpool

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
)

const (
	testTunnelID    = "tun_01J00000000000000000000000"
	testConnectorID = "con_01J00000000000000000000000"
	testSessionID   = "sess_01J00000000000000000000000"
)

func TestNewValidatesOptions(t *testing.T) {
	valid := testOptions(8, 4)
	tests := []struct {
		name   string
		mutate func(*Options)
	}{
		{name: "Tunnel ID", mutate: func(options *Options) { options.Session.TunnelID = "tun_invalid" }},
		{name: "Connector ID", mutate: func(options *Options) { options.Session.ConnectorID = "con_invalid" }},
		{name: "Session ID", mutate: func(options *Options) { options.Session.SessionID = "sess_invalid" }},
		{name: "Generation", mutate: func(options *Options) { options.Session.Generation = 0 }},
		{name: "MaxTotal", mutate: func(options *Options) { options.MaxTotal = 0 }},
		{name: "MaxConnecting", mutate: func(options *Options) { options.MaxConnecting = 0 }},
		{name: "Connecting exceeds total", mutate: func(options *Options) { options.MaxConnecting = 9 }},
		{name: "Clock", mutate: func(options *Options) { options.Clock = nil }},
		{name: "DeadlineNow", mutate: func(options *Options) { options.DeadlineNow = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := valid
			test.mutate(&options)
			if _, err := New(options); !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("New() error = %v, want ErrInvalidOptions", err)
			}
		})
	}
}

func TestDrainStopsNewAcquireAndWaitsForOpening(t *testing.T) {
	pool := newTestPool(t, 4, 4)
	openingConnection := &recordingConn{}
	opening := registerTestWork(t, pool, 1, openingConnection)
	if err := opening.MarkIdle(); err != nil {
		t.Fatalf("opening.MarkIdle() error = %v", err)
	}
	acquired, err := pool.Acquire(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	idleConnection := &recordingConn{}
	idle := registerTestWork(t, pool, 2, idleConnection)
	if err := idle.MarkIdle(); err != nil {
		t.Fatalf("idle.MarkIdle() error = %v", err)
	}
	connectingConnection := &recordingConn{}
	registerTestWork(t, pool, 3, connectingConnection)
	if err := pool.BeginDrain(); err != nil {
		t.Fatalf("BeginDrain() error = %v", err)
	}
	if _, err := pool.Acquire(context.Background(), time.Second); !errors.Is(err, ErrPoolDraining) {
		t.Fatalf("Acquire(drain) error = %v, want ErrPoolDraining", err)
	}
	rejectedConnection := &recordingConn{}
	if _, err := pool.RegisterConnecting(testWorkID(4), rejectedConnection); !errors.Is(err, ErrPoolDraining) {
		t.Fatalf("RegisterConnecting(drain) error = %v, want ErrPoolDraining", err)
	}

	result := make(chan struct {
		active uint32
		err    error
	}, 1)
	go func() {
		active, err := pool.WaitOpeningAndCloseNonActive(context.Background())
		result <- struct {
			active uint32
			err    error
		}{active: active, err: err}
	}()
	select {
	case <-result:
		t.Fatal("drain returned before OPENING settled")
	case <-time.After(20 * time.Millisecond):
	}
	if err := acquired.MarkActive(); err != nil {
		t.Fatalf("MarkActive() error = %v", err)
	}
	select {
	case got := <-result:
		if got.err != nil || got.active != 1 {
			t.Fatalf("WaitOpeningAndCloseNonActive() = %d, %v, want 1, nil", got.active, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("drain did not finish after OPENING became ACTIVE")
	}
	assertCounts(t, pool.Snapshot(), Counts{Active: 1, Total: 1, Draining: true})
	idleConnection.assertCalls(t, 1, 1)
	connectingConnection.assertCalls(t, 1, 1)
	rejectedConnection.assertCalls(t, 0, 0)
	if err := acquired.Close(); err != nil {
		t.Fatalf("active Close() error = %v", err)
	}
}

func TestDrainDeadlineForceClosesOpening(t *testing.T) {
	pool := newTestPool(t, 1, 1)
	connection := &recordingConn{}
	work := registerTestWork(t, pool, 1, connection)
	if err := work.MarkIdle(); err != nil {
		t.Fatalf("MarkIdle() error = %v", err)
	}
	if _, err := pool.Acquire(context.Background(), time.Second); err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	active, err := pool.WaitOpeningAndCloseNonActive(ctx)
	if err != nil || active != 0 {
		t.Fatalf("WaitOpeningAndCloseNonActive(timeout) = %d, %v, want 0, nil", active, err)
	}
	assertCounts(t, pool.Snapshot(), Counts{Draining: true})
	connection.assertCalls(t, 1, 1)
}

func TestPoolsShareProcessWorkLimits(t *testing.T) {
	limitManager, err := serverlimits.New(serverlimits.Options{
		MaxConnectors: 2, MaxConnectorsPerTunnel: 2,
		MaxWorkConnections: 2, MaxIdleWorkConnections: 1, MaxConnectingWorkConnections: 1,
		MaxPendingOpens: 1, MaxActiveConnections: 1, MaxConnectionsPerTunnel: 1,
		MaxConnectionsPerService: 1, MaxConnectionsPerSourceIP: 1,
		MaxOpenRatePerSourceIP: 1, MaxOpenBurstPerSourceIP: 1,
		MaxHTTPRequestsPerSourceIPPerSecond: 1,
	})
	if err != nil {
		t.Fatalf("limits.New() error = %v", err)
	}
	firstOptions := testOptions(2, 2)
	firstOptions.LimitManager = limitManager
	first, err := New(firstOptions)
	if err != nil {
		t.Fatalf("New(first) error = %v", err)
	}
	secondOptions := testOptions(2, 2)
	secondOptions.Session.ConnectorID = "con_01J00000000000000000000001"
	secondOptions.Session.SessionID = "sess_01J00000000000000000000001"
	secondOptions.LimitManager = limitManager
	second, err := New(secondOptions)
	if err != nil {
		t.Fatalf("New(second) error = %v", err)
	}
	firstWork := registerTestWork(t, first, 1, &recordingConn{})
	if _, err := second.RegisterConnecting(testWorkID(2), &recordingConn{}); !errors.Is(err, serverlimits.ErrConnectingWorkCapacity) {
		t.Fatalf("second RegisterConnecting() error = %v, want global connecting capacity", err)
	}
	if err := firstWork.MarkIdle(); err != nil {
		t.Fatalf("first MarkIdle() error = %v", err)
	}
	secondWork := registerTestWork(t, second, 2, &recordingConn{})
	if err := secondWork.MarkIdle(); !errors.Is(err, serverlimits.ErrIdleWorkCapacity) {
		t.Fatalf("second MarkIdle() error = %v, want global idle capacity", err)
	}
	if err := secondWork.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if err := firstWork.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if got := limitManager.Snapshot(); got.WorkTotal != 0 || got.WorkConnecting != 0 || got.WorkIdle != 0 {
		t.Fatalf("global Work snapshot after close = %#v", got)
	}
}

func TestWorkLifecycleAndExactlyOnceClose(t *testing.T) {
	pool := newTestPool(t, 4, 2)
	deadlineErr := errors.New("deadline failed")
	closeErr := errors.New("close failed")
	connection := &recordingConn{deadlineErr: deadlineErr, closeErr: closeErr}
	work, err := pool.RegisterConnecting(testWorkID(1), connection)
	if err != nil {
		t.Fatalf("RegisterConnecting() error = %v", err)
	}
	assertCounts(t, pool.Snapshot(), Counts{Connecting: 1, Total: 1})
	if work.ID() != testWorkID(1) || work.Conn() != connection || work.State() != StateConnecting {
		t.Fatalf("registered Work = (%q, %T, %s), want stable identity and CONNECTING", work.ID(), work.Conn(), work.State())
	}
	if err := work.MarkIdle(); err != nil {
		t.Fatalf("MarkIdle() error = %v", err)
	}
	assertCounts(t, pool.Snapshot(), Counts{Idle: 1, Total: 1})
	acquired, err := pool.Acquire(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if acquired != work || work.State() != StateOpening {
		t.Fatalf("Acquire() = %p state=%s, want original Work in OPENING", acquired, work.State())
	}
	assertCounts(t, pool.Snapshot(), Counts{Opening: 1, Total: 1})
	if err := work.MarkActive(); err != nil {
		t.Fatalf("MarkActive() error = %v", err)
	}
	assertCounts(t, pool.Snapshot(), Counts{Active: 1, Total: 1})

	firstErr := work.Close()
	select {
	case <-work.Done():
	default:
		t.Fatal("Done() remained open after Work connection close completed")
	}
	secondErr := work.Close()
	for _, target := range []error{deadlineErr, closeErr} {
		if !errors.Is(firstErr, target) || !errors.Is(secondErr, target) {
			t.Fatalf("Close() errors = %v / %v, missing %v", firstErr, secondErr, target)
		}
	}
	if work.State() != StateClosed {
		t.Fatalf("State() = %s, want CLOSED", work.State())
	}
	assertCounts(t, pool.Snapshot(), Counts{})
	connection.assertCalls(t, 1, 1)
}

func TestRegisterConnectingEnforcesOwnershipAndCapacity(t *testing.T) {
	pool := newTestPool(t, 2, 1)
	firstConn := &recordingConn{}
	first, err := pool.RegisterConnecting(testWorkID(1), firstConn)
	if err != nil {
		t.Fatalf("RegisterConnecting(first) error = %v", err)
	}
	duplicateConn := &recordingConn{}
	if _, err := pool.RegisterConnecting(testWorkID(1), duplicateConn); !errors.Is(err, ErrDuplicateWork) {
		t.Fatalf("RegisterConnecting(duplicate) error = %v, want ErrDuplicateWork", err)
	}
	secondConn := &recordingConn{}
	if _, err := pool.RegisterConnecting(testWorkID(2), secondConn); !errors.Is(err, ErrConnectingCapacity) {
		t.Fatalf("RegisterConnecting(connecting capacity) error = %v, want ErrConnectingCapacity", err)
	}
	if err := first.MarkIdle(); err != nil {
		t.Fatalf("MarkIdle(first) error = %v", err)
	}
	second, err := pool.RegisterConnecting(testWorkID(2), secondConn)
	if err != nil {
		t.Fatalf("RegisterConnecting(second) error = %v", err)
	}
	thirdConn := &recordingConn{}
	if _, err := pool.RegisterConnecting(testWorkID(3), thirdConn); !errors.Is(err, ErrPoolCapacity) {
		t.Fatalf("RegisterConnecting(total capacity) error = %v, want ErrPoolCapacity", err)
	}
	// 所有失败注册都必须保留调用方所有权，Pool 不能擅自关闭连接。
	duplicateConn.assertCalls(t, 0, 0)
	thirdConn.assertCalls(t, 0, 0)
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("Close(second) error = %v", err)
	}
}

func TestConcurrentDuplicateRegisterHasOneOwner(t *testing.T) {
	const callers = 64
	pool := newTestPool(t, callers, callers)
	results := make(chan struct {
		work *Work
		err  error
	}, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			work, err := pool.RegisterConnecting(testWorkID(1), &recordingConn{})
			results <- struct {
				work *Work
				err  error
			}{work: work, err: err}
		}()
	}
	wait.Wait()
	close(results)
	var owner *Work
	for result := range results {
		if result.err == nil {
			if owner != nil {
				t.Fatal("more than one concurrent RegisterConnecting call acquired ownership")
			}
			owner = result.work
			continue
		}
		if !errors.Is(result.err, ErrDuplicateWork) {
			t.Errorf("RegisterConnecting() error = %v, want ErrDuplicateWork", result.err)
		}
	}
	if owner == nil {
		t.Fatal("no concurrent RegisterConnecting call acquired ownership")
	}
	assertCounts(t, pool.Snapshot(), Counts{Connecting: 1, Total: 1})
	if err := owner.Close(); err != nil {
		t.Fatalf("Close(owner) error = %v", err)
	}
}

func TestAcquireWaitsForIdleAndHonorsContextAndTimeout(t *testing.T) {
	pool := newTestPool(t, 4, 2)
	result := make(chan struct {
		work *Work
		err  error
	}, 1)
	go func() {
		work, err := pool.Acquire(context.Background(), time.Second)
		result <- struct {
			work *Work
			err  error
		}{work: work, err: err}
	}()
	work, err := pool.RegisterConnecting(testWorkID(1), &recordingConn{})
	if err != nil {
		t.Fatalf("RegisterConnecting() error = %v", err)
	}
	if err := work.MarkIdle(); err != nil {
		t.Fatalf("MarkIdle() error = %v", err)
	}
	select {
	case acquired := <-result:
		if acquired.err != nil || acquired.work != work {
			t.Fatalf("Acquire() = %p, %v, want registered Work", acquired.work, acquired.err)
		}
	case <-time.After(time.Second):
		t.Fatal("Acquire() was not awakened by IDLE transition")
	}
	if err := work.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pool.Acquire(cancelled, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire(cancelled) error = %v, want context.Canceled", err)
	}
	if _, err := pool.Acquire(context.Background(), 20*time.Millisecond); !errors.Is(err, ErrAcquireTimeout) ||
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire(timeout) error = %v, want timeout and context deadline", err)
	}
}

func TestTryAcquireNeverWaitsAndClaimsIdleExactlyOnce(t *testing.T) {
	pool := newTestPool(t, 2, 2)
	if work, ok, err := pool.TryAcquire(); err != nil || ok || work != nil {
		t.Fatalf("TryAcquire(empty) = %p, %t, %v, want nil, false, nil", work, ok, err)
	}
	work := registerTestWork(t, pool, 1, &recordingConn{})
	if err := work.MarkIdle(); err != nil {
		t.Fatalf("MarkIdle() error = %v", err)
	}
	acquired, ok, err := pool.TryAcquire()
	if err != nil || !ok || acquired != work || work.State() != StateOpening {
		t.Fatalf("TryAcquire(idle) = %p, %t, %v state=%s", acquired, ok, err, work.State())
	}
	if second, secondOK, secondErr := pool.TryAcquire(); secondErr != nil || secondOK || second != nil {
		t.Fatalf("TryAcquire(claimed) = %p, %t, %v, want nil, false, nil", second, secondOK, secondErr)
	}
	if err := work.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestConcurrentRegisterAndAcquireRemainUnique(t *testing.T) {
	const workCount = 64
	pool := newTestPool(t, workCount, workCount)
	registerErrors := make(chan error, workCount)
	var registerWait sync.WaitGroup
	for index := 1; index <= workCount; index++ {
		registerWait.Add(1)
		go func(workID string) {
			defer registerWait.Done()
			work, err := pool.RegisterConnecting(workID, &recordingConn{})
			if err != nil {
				registerErrors <- err
				return
			}
			if err := work.MarkIdle(); err != nil {
				registerErrors <- err
				return
			}
		}(testWorkID(index))
	}
	registerWait.Wait()
	close(registerErrors)
	for err := range registerErrors {
		t.Errorf("concurrent registration error = %v", err)
	}
	if t.Failed() {
		return
	}
	assertCounts(t, pool.Snapshot(), Counts{Idle: workCount, Total: workCount})

	acquiredIDs := make(chan string, workCount)
	acquireErrors := make(chan error, workCount)
	var acquireWait sync.WaitGroup
	for range workCount {
		acquireWait.Add(1)
		go func() {
			defer acquireWait.Done()
			work, err := pool.Acquire(context.Background(), time.Second)
			if err != nil {
				acquireErrors <- err
				return
			}
			acquiredIDs <- work.ID()
			// OPEN 失败必须从 OPENING 直接 CLOSED，不能归还 IDLE。
			if err := work.Close(); err != nil {
				acquireErrors <- err
			}
		}()
	}
	acquireWait.Wait()
	close(acquiredIDs)
	close(acquireErrors)
	seen := make(map[string]struct{}, workCount)
	for workID := range acquiredIDs {
		if _, duplicate := seen[workID]; duplicate {
			t.Errorf("Work ID %q was acquired more than once", workID)
		}
		seen[workID] = struct{}{}
	}
	for err := range acquireErrors {
		t.Errorf("concurrent acquire/close error = %v", err)
	}
	if len(seen) != workCount {
		t.Fatalf("unique acquired Work count = %d, want %d", len(seen), workCount)
	}
	assertCounts(t, pool.Snapshot(), Counts{})
}

func TestCloseNonActivePreservesActiveAndCloseAggregatesErrors(t *testing.T) {
	pool := newTestPool(t, 4, 4)
	connectingConn := &recordingConn{}
	connecting := registerTestWork(t, pool, 1, connectingConn)
	idleConn := &recordingConn{}
	idle := registerTestWork(t, pool, 2, idleConn)
	if err := idle.MarkIdle(); err != nil {
		t.Fatalf("MarkIdle(idle) error = %v", err)
	}
	openingConn := &recordingConn{}
	opening := registerTestWork(t, pool, 3, openingConn)
	if err := opening.MarkIdle(); err != nil {
		t.Fatalf("MarkIdle(opening) error = %v", err)
	}
	if acquired, err := pool.Acquire(context.Background(), time.Second); err != nil || acquired != idle {
		t.Fatalf("Acquire(opening) = %p, %v, want first IDLE", acquired, err)
	}
	activeConn := &recordingConn{}
	active := registerTestWork(t, pool, 4, activeConn)
	if err := active.MarkIdle(); err != nil {
		t.Fatalf("MarkIdle(active) error = %v", err)
	}
	// FIFO 中 opening 是下一条 IDLE。
	acquired, err := pool.Acquire(context.Background(), time.Second)
	if err != nil || acquired != opening {
		t.Fatalf("Acquire(active candidate) = %p, %v, want opening Work", acquired, err)
	}
	if err := opening.MarkActive(); err != nil {
		t.Fatalf("MarkActive() error = %v", err)
	}

	if err := pool.CloseNonActive(); err != nil {
		t.Fatalf("CloseNonActive() error = %v", err)
	}
	assertCounts(t, pool.Snapshot(), Counts{Active: 1, Total: 1, Closed: true})
	connectingConn.assertCalls(t, 1, 1)
	idleConn.assertCalls(t, 1, 1)
	activeConn.assertCalls(t, 1, 1)
	openingConn.assertCalls(t, 0, 0)
	if _, err := pool.RegisterConnecting(testWorkID(5), &recordingConn{}); !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("RegisterConnecting(after cleanup) error = %v, want ErrPoolClosed", err)
	}
	if _, err := pool.Acquire(context.Background(), time.Second); !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("Acquire(after cleanup) error = %v, want ErrPoolClosed", err)
	}
	if err := pool.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	openingConn.assertCalls(t, 1, 1)
	assertCounts(t, pool.Snapshot(), Counts{Closed: true})
	_ = connecting
}

func TestPoolCloseAggregatesErrorsAndTransitionsNeverUnderflow(t *testing.T) {
	pool := newTestPool(t, 2, 2)
	deadlineErr := errors.New("first deadline")
	closeErr := errors.New("second close")
	first := registerTestWork(t, pool, 1, &recordingConn{deadlineErr: deadlineErr})
	second := registerTestWork(t, pool, 2, &recordingConn{closeErr: closeErr})
	if err := first.MarkActive(); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("MarkActive(CONNECTING) error = %v, want ErrInvalidTransition", err)
	}
	if err := first.MarkIdle(); err != nil {
		t.Fatalf("MarkIdle(first) error = %v", err)
	}
	if err := first.MarkIdle(); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("MarkIdle(IDLE) error = %v, want ErrInvalidTransition", err)
	}
	err := pool.Close()
	if !errors.Is(err, deadlineErr) || !errors.Is(err, closeErr) {
		t.Fatalf("Close() error = %v, want both connection errors", err)
	}
	if err := pool.Close(); err != nil {
		t.Fatalf("second Close() error = %v, want nil after resources detached", err)
	}
	if err := second.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("Work.Close(after Pool.Close) error = %v, want stable close error", err)
	}
	assertCounts(t, pool.Snapshot(), Counts{Closed: true})
}

func newTestPool(t *testing.T, maxTotal, maxConnecting uint32) *Pool {
	t.Helper()
	pool, err := New(testOptions(maxTotal, maxConnecting))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return pool
}

func testOptions(maxTotal, maxConnecting uint32) Options {
	return Options{
		Session: Session{
			TunnelID: testTunnelID, ConnectorID: testConnectorID, SessionID: testSessionID, Generation: 1,
		},
		MaxTotal: maxTotal, MaxConnecting: maxConnecting,
		Clock: func() time.Duration { return 0 }, DeadlineNow: time.Now,
	}
}

func registerTestWork(t *testing.T, pool *Pool, index int, connection net.Conn) *Work {
	t.Helper()
	work, err := pool.RegisterConnecting(testWorkID(index), connection)
	if err != nil {
		t.Fatalf("RegisterConnecting(%d) error = %v", index, err)
	}
	return work
}

func testWorkID(index int) string {
	return fmt.Sprintf("work_%026d", index)
}

func assertCounts(t *testing.T, got, want Counts) {
	t.Helper()
	if got != want {
		t.Fatalf("Snapshot() = %#v, want %#v", got, want)
	}
	if got.Total != got.Connecting+got.Idle+got.Opening+got.Active {
		t.Fatalf("Snapshot total invariant violated: %#v", got)
	}
}

type recordingConn struct {
	mu sync.Mutex

	deadlineCalls int
	closeCalls    int
	deadlineErr   error
	closeErr      error
}

func (*recordingConn) Read([]byte) (int, error)  { return 0, net.ErrClosed }
func (*recordingConn) Write([]byte) (int, error) { return 0, net.ErrClosed }
func (connection *recordingConn) Close() error {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	connection.closeCalls++
	return connection.closeErr
}
func (*recordingConn) LocalAddr() net.Addr  { return workPoolAddr("local") }
func (*recordingConn) RemoteAddr() net.Addr { return workPoolAddr("remote") }
func (connection *recordingConn) SetDeadline(time.Time) error {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	connection.deadlineCalls++
	return connection.deadlineErr
}
func (*recordingConn) SetReadDeadline(time.Time) error  { return nil }
func (*recordingConn) SetWriteDeadline(time.Time) error { return nil }

func (connection *recordingConn) assertCalls(t *testing.T, deadline, close int) {
	t.Helper()
	connection.mu.Lock()
	defer connection.mu.Unlock()
	if connection.deadlineCalls != deadline || connection.closeCalls != close {
		t.Fatalf("connection calls = deadline:%d close:%d, want deadline:%d close:%d",
			connection.deadlineCalls, connection.closeCalls, deadline, close)
	}
}

type workPoolAddr string

func (address workPoolAddr) Network() string { return "test" }
func (address workPoolAddr) String() string  { return string(address) }
