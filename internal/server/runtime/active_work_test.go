package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/healthbudget"
)

const (
	runtimeWorkID        = "work_01J00000000000000000000000"
	runtimeWorkIDTwo     = "work_01J00000000000000000000001"
	runtimeConnectionID  = "conn_01J00000000000000000000000"
	runtimeConnectionTwo = "conn_01J00000000000000000000001"
)

func TestTunnelRuntimeRegisterAndFinishExactlyOnce(t *testing.T) {
	fixture := newActiveWorkFixture(t, runtimeTunnelID, runtimeConnectorID)
	work, err := fixture.tunnel.RegisterActiveWork(fixture.spec(runtimeWorkID, runtimeConnectionID))
	if err != nil {
		t.Fatalf("RegisterActiveWork() error = %v", err)
	}
	if got := work.Identity(); got.TunnelID != runtimeTunnelID || got.ConnectorID != runtimeConnectorID ||
		got.SessionID != fixture.session.SessionID || got.Generation != fixture.session.Generation ||
		got.WorkID != runtimeWorkID || got.ConnectionID != runtimeConnectionID {
		t.Fatalf("Identity() = %#v, want complete immutable identity", got)
	}
	if count := fixture.tunnel.ActiveCount(); count != 1 {
		t.Fatalf("ActiveCount() = %d, want 1", count)
	}

	var wait sync.WaitGroup
	errorsCh := make(chan error, 64)
	for index := range 64 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if index%2 == 0 {
				errorsCh <- work.Finish()
				return
			}
			errorsCh <- work.Close()
		}()
	}
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Errorf("concurrent Finish/Close error = %v", err)
		}
	}
	if count := fixture.tunnel.ActiveCount(); count != 0 {
		t.Fatalf("ActiveCount() = %d, want 0", count)
	}
	fixture.recorder.assertEvents(t, []string{"cancel", "work:deadline", "peer:deadline", "work:close", "peer:close"})
	if fixture.lease.Release() {
		t.Fatal("termination did not release ConnectorLease exactly once")
	}
}

func TestDrainActiveWaitsForBlockingFinishCleanup(t *testing.T) {
	fixture := newActiveWorkFixture(t, runtimeTunnelID, runtimeConnectorID)
	closeStarted := make(chan struct{})
	allowClose := make(chan struct{})
	var allowCloseOnce sync.Once
	unblock := func() { allowCloseOnce.Do(func() { close(allowClose) }) }
	t.Cleanup(unblock)
	fixture.workConn = &recordingConn{
		name: "work", recorder: fixture.recorder,
		closeStarted: closeStarted, allowClose: allowClose,
	}
	work, err := fixture.tunnel.RegisterActiveWork(fixture.spec(runtimeWorkID, runtimeConnectionID))
	if err != nil {
		t.Fatalf("RegisterActiveWork() error = %v", err)
	}

	finishResult := make(chan error, 1)
	go func() { finishResult <- work.Finish() }()
	select {
	case <-closeStarted:
	case <-time.After(time.Second):
		t.Fatal("Finish() did not enter blocking WorkConn.Close")
	}
	if count := fixture.tunnel.ActiveCount(); count != 1 {
		t.Fatalf("ActiveCount() while Close blocks = %d, want 1", count)
	}
	if state := fixture.lease.lifecycle.Load(); state != connectorLeaseActiveOwned {
		t.Fatalf("ConnectorLease state while Close blocks = %d, want active-owned", state)
	}

	drainResult := make(chan error, 1)
	go func() { drainResult <- fixture.registry.DrainActive(context.Background()) }()
	select {
	case err := <-drainResult:
		t.Fatalf("DrainActive() returned before WorkConn.Close completed: %v", err)
	case <-time.After(4 * activeDrainPollInterval):
	}

	unblock()
	select {
	case err := <-finishResult:
		if err != nil {
			t.Fatalf("Finish() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Finish() did not return after WorkConn.Close unblocked")
	}
	select {
	case err := <-drainResult:
		if err != nil {
			t.Fatalf("DrainActive() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("DrainActive() did not return after cleanup completed")
	}
	if count := fixture.tunnel.ActiveCount(); count != 0 {
		t.Fatalf("ActiveCount() after cleanup = %d, want 0", count)
	}
	if fixture.lease.Release() {
		t.Fatal("Finish() did not release ConnectorLease exactly once")
	}
	fixture.recorder.assertEvents(t, []string{
		"cancel", "work:deadline", "peer:deadline", "work:close", "peer:close",
	})
}

func TestTunnelRuntimeRejectsInvalidAndDuplicateActiveWork(t *testing.T) {
	fixture := newActiveWorkFixture(t, runtimeTunnelID, runtimeConnectorID)
	tests := []struct {
		name   string
		mutate func(*ActiveWorkSpec)
	}{
		{name: "tunnel mismatch", mutate: func(spec *ActiveWorkSpec) { spec.Session.TunnelID = runtimeTunnelIDTwo }},
		{name: "connector", mutate: func(spec *ActiveWorkSpec) { spec.Session.ConnectorID = "con_invalid" }},
		{name: "session", mutate: func(spec *ActiveWorkSpec) { spec.Session.SessionID = "sess_invalid" }},
		{name: "generation", mutate: func(spec *ActiveWorkSpec) { spec.Session.Generation = 0 }},
		{name: "work", mutate: func(spec *ActiveWorkSpec) { spec.WorkID = "work_invalid" }},
		{name: "connection", mutate: func(spec *ActiveWorkSpec) { spec.ConnectionID = "conn_invalid" }},
		{name: "cancel", mutate: func(spec *ActiveWorkSpec) { spec.Cancel = nil }},
		{name: "work conn", mutate: func(spec *ActiveWorkSpec) { spec.WorkConn = nil }},
		{name: "lease", mutate: func(spec *ActiveWorkSpec) { spec.Lease = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := fixture.spec(runtimeWorkID, runtimeConnectionID)
			test.mutate(&spec)
			if _, err := fixture.tunnel.RegisterActiveWork(spec); !errors.Is(err, ErrInvalidActiveWork) {
				t.Fatalf("RegisterActiveWork() error = %v, want ErrInvalidActiveWork", err)
			}
		})
	}
	released := newActiveWorkFixtureWithRegistry(t, fixture.registry, runtimeTunnelIDTwo, runtimeConnectorID)
	if !released.lease.Release() {
		t.Fatal("released fixture Lease.Release() = false")
	}
	if _, err := released.tunnel.RegisterActiveWork(released.spec(runtimeWorkID, runtimeConnectionID)); !errors.Is(err, ErrInvalidActiveWork) {
		t.Fatalf("RegisterActiveWork(released lease) error = %v, want ErrInvalidActiveWork", err)
	}

	work, err := fixture.tunnel.RegisterActiveWork(fixture.spec(runtimeWorkID, runtimeConnectionID))
	if err != nil {
		t.Fatalf("RegisterActiveWork(first) error = %v", err)
	}
	second := newActiveWorkFixtureWithRegistry(t, fixture.registry, runtimeTunnelID, runtimeConnectorIDTwo)
	duplicateSpec := second.spec(runtimeWorkIDTwo, runtimeConnectionID)
	if _, err := fixture.tunnel.RegisterActiveWork(duplicateSpec); !errors.Is(err, ErrActiveWorkExists) {
		t.Fatalf("RegisterActiveWork(duplicate) error = %v, want ErrActiveWorkExists", err)
	}
	if !second.lease.Release() {
		t.Fatal("caller could not release ownership after duplicate registration")
	}
	if err := work.Finish(); err != nil {
		t.Fatalf("Finish() error = %v", err)
	}
}

func TestTunnelRuntimeActiveWorkWithoutPublicPeer(t *testing.T) {
	fixture := newActiveWorkFixture(t, runtimeTunnelID, runtimeConnectorID)
	spec := fixture.spec(runtimeWorkID, runtimeConnectionID)
	spec.PeerConn = nil

	work, err := fixture.tunnel.RegisterActiveWork(spec)
	if err != nil {
		t.Fatalf("RegisterActiveWork() error = %v", err)
	}
	if err := work.Finish(); err != nil {
		t.Fatalf("Finish() error = %v", err)
	}
	fixture.recorder.assertEvents(t, []string{"cancel", "work:deadline", "work:close"})
	if fixture.lease.Release() {
		t.Fatal("Finish() did not release ConnectorLease")
	}
}

func TestRegistryDrainActiveRejectsLateRegistration(t *testing.T) {
	fixture := newActiveWorkFixture(t, runtimeTunnelID, runtimeConnectorID)
	if err := fixture.registry.DrainActive(context.Background()); err != nil {
		t.Fatalf("DrainActive() error = %v", err)
	}
	if _, err := fixture.tunnel.RegisterActiveWork(
		fixture.spec(runtimeWorkID, runtimeConnectionID),
	); !errors.Is(err, ErrServerRuntimeDraining) {
		t.Fatalf("RegisterActiveWork(after drain) error = %v, want ErrServerRuntimeDraining", err)
	}
	if !fixture.lease.Release() {
		t.Fatal("caller could not release ConnectorLease after drained registration rejection")
	}
	fixture.recorder.assertEventCounts(t, 0)
}

func TestTunnelRuntimeRegisterAndReleaseHaveSingleLeaseWinner(t *testing.T) {
	// 每轮都让 Register 与公共 Release 同时争抢 acquired 状态。若 Register 获胜，
	// Lease 必须由 ACTIVE 终止路径释放；若 Release 获胜，注册必须快速失败。
	const iterations = 256
	for iteration := range iterations {
		fixture := newActiveWorkFixture(t, runtimeTunnelID, runtimeConnectorID)
		start := make(chan struct{})
		registerResult := make(chan struct {
			work *ActiveWork
			err  error
		}, 1)
		releaseResult := make(chan bool, 1)

		go func() {
			<-start
			work, err := fixture.tunnel.RegisterActiveWork(fixture.spec(runtimeWorkID, runtimeConnectionID))
			registerResult <- struct {
				work *ActiveWork
				err  error
			}{work: work, err: err}
		}()
		go func() {
			<-start
			releaseResult <- fixture.lease.Release()
		}()
		close(start)

		registered := <-registerResult
		released := <-releaseResult
		switch {
		case registered.err == nil:
			if released {
				t.Fatalf("iteration %d: Register and Release both acquired Lease ownership", iteration)
			}
			if got := fixture.lease.lifecycle.Load(); got != connectorLeaseActiveOwned {
				t.Fatalf("iteration %d: Lease state = %d, want active-owned", iteration, got)
			}
			if err := registered.work.Finish(); err != nil {
				t.Fatalf("iteration %d: Finish() error = %v", iteration, err)
			}
		case errors.Is(registered.err, ErrInvalidActiveWork):
			if !released {
				t.Fatalf("iteration %d: Release did not win after Register rejected Lease", iteration)
			}
		default:
			t.Fatalf("iteration %d: RegisterActiveWork() error = %v", iteration, registered.err)
		}
		if entries := connectorActiveEntries(fixture.registry, runtimeTunnelID); entries != 0 {
			t.Fatalf("iteration %d: connector active entries = %d, want 0", iteration, entries)
		}
	}
}

func TestRetiredSessionIDRemainsReservedUntilActiveWorkFinishes(t *testing.T) {
	const (
		firstSessionID  = "sess_01J00000000000000000000000"
		secondSessionID = "sess_01J00000000000000000000001"
	)
	generated := []string{firstSessionID, secondSessionID, firstSessionID, firstSessionID}
	var generatorMu sync.Mutex
	next := 0
	registry := newRegistry(func() (string, error) {
		generatorMu.Lock()
		defer generatorMu.Unlock()
		if next >= len(generated) {
			return "", errors.New("test session generator exhausted")
		}
		sessionID := generated[next]
		next++
		return sessionID, nil
	})
	fixture := newActiveWorkFixtureWithRegistry(t, registry, runtimeTunnelID, runtimeConnectorID)
	work, err := fixture.tunnel.RegisterActiveWork(fixture.spec(runtimeWorkID, runtimeConnectionID))
	if err != nil {
		t.Fatalf("RegisterActiveWork() error = %v", err)
	}
	replacement, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatalf("installAuthenticated(replacement) error = %v", err)
	}
	if replacement.SessionID != secondSessionID {
		t.Fatalf("replacement SessionID = %q, want %q", replacement.SessionID, secondSessionID)
	}

	// 旧 Session 已离开 Current，但 ActiveWork 与其 Lease 仍运行，因此全局 ID
	// 必须继续保留，不能被另一个 Connector 的认证预留复用。
	if _, err := registry.ReserveAuthenticated(runtimeTunnelID, runtimeConnectorIDTwo); !errors.Is(err, ErrSessionIDCollision) {
		t.Fatalf("ReserveAuthenticated(retired collision) error = %v, want ErrSessionIDCollision", err)
	}
	if err := work.Finish(); err != nil {
		t.Fatalf("Finish() error = %v", err)
	}

	reused, err := registry.ReserveAuthenticated(runtimeTunnelID, runtimeConnectorIDTwo)
	if err != nil {
		t.Fatalf("ReserveAuthenticated(after Finish) error = %v", err)
	}
	if reused.SessionID() != firstSessionID {
		t.Fatalf("reused SessionID = %q, want released retired ID %q", reused.SessionID(), firstSessionID)
	}
	if !registry.CancelAuthenticated(reused) {
		t.Fatal("CancelAuthenticated(reused) = false")
	}
}

func TestRetiredSessionHealthTargetRemainsReservedUntilActiveWorkFinishes(t *testing.T) {
	budget := newRuntimeHealthBudget(t, 1, 1)
	registry := NewRegistryWithLimitsAndHealthBudget(nil, budget)
	registry.newSession = sessionGenerator(1)
	fixture := newActiveWorkFixtureWithRegistry(t, registry, runtimeTunnelID, runtimeConnectorID)
	work, err := fixture.tunnel.RegisterActiveWork(fixture.spec(runtimeWorkID, runtimeConnectionID))
	if err != nil {
		t.Fatalf("RegisterActiveWork() error = %v", err)
	}
	replacement, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatalf("installAuthenticated(replacement) error = %v", err)
	}
	if !registry.ClearIfCurrent(replacement) {
		t.Fatal("ClearIfCurrent(replacement) = false")
	}

	key := healthbudget.ConnectorKey{TunnelID: runtimeTunnelID, ConnectorID: runtimeConnectorID}
	snapshot := budget.Snapshot()
	if snapshot.TargetsGlobal != 1 || snapshot.ConnectorReferences[key] != 1 {
		t.Fatalf("budget while old ActiveWork is alive = %#v, want old generation reference retained", snapshot)
	}
	pending, err := registry.ReserveAuthenticated(runtimeTunnelID, runtimeConnectorIDTwo)
	if err != nil {
		t.Fatalf("ReserveAuthenticated(second Connector) error = %v", err)
	}
	if _, err := registry.InstallAuthenticated(pending); !errors.Is(err, healthbudget.ErrTargetCapacity) {
		t.Fatalf("InstallAuthenticated(before old Finish) error = %v, want ErrTargetCapacity", err)
	}
	if !registry.CancelAuthenticated(pending) {
		t.Fatal("CancelAuthenticated(second Connector) = false")
	}

	if err := work.Finish(); err != nil {
		t.Fatalf("Finish() error = %v", err)
	}
	snapshot = budget.Snapshot()
	if snapshot.TargetsGlobal != 0 || len(snapshot.ConnectorReferences) != 0 {
		t.Fatalf("budget after old ActiveWork Finish = %#v, want fully released", snapshot)
	}
	if _, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorIDTwo); err != nil {
		t.Fatalf("installAuthenticated(after old Finish) error = %v", err)
	}
}

func TestRevokeTunnelReleasesHealthTargetAfterClosingActiveWork(t *testing.T) {
	budget := newRuntimeHealthBudget(t, 1, 1)
	registry := NewRegistryWithLimitsAndHealthBudget(nil, budget)
	registry.newSession = sessionGenerator(1)
	fixture := newActiveWorkFixtureWithRegistry(t, registry, runtimeTunnelID, runtimeConnectorID)
	if _, err := fixture.tunnel.RegisterActiveWork(fixture.spec(runtimeWorkID, runtimeConnectionID)); err != nil {
		t.Fatalf("RegisterActiveWork() error = %v", err)
	}
	if err := registry.RevokeTunnel(runtimeTunnelID); err != nil {
		t.Fatalf("RevokeTunnel() error = %v", err)
	}
	snapshot := budget.Snapshot()
	if snapshot.TargetsGlobal != 0 || len(snapshot.ConnectorReferences) != 0 {
		t.Fatalf("budget after Revoke = %#v, want exactly-once release after ActiveWork close", snapshot)
	}
	if err := registry.RevokeTunnel(runtimeTunnelID); err != nil {
		t.Fatalf("RevokeTunnel(repeated) error = %v", err)
	}
	if snapshot = budget.Snapshot(); snapshot.TargetsGlobal != 0 || len(snapshot.ConnectorReferences) != 0 {
		t.Fatalf("budget after repeated Revoke = %#v, want unchanged", snapshot)
	}
}

func TestOldSessionCleanupPreservesActiveWork(t *testing.T) {
	fixture := newActiveWorkFixture(t, runtimeTunnelID, runtimeConnectorID)
	work, err := fixture.tunnel.RegisterActiveWork(fixture.spec(runtimeWorkID, runtimeConnectionID))
	if err != nil {
		t.Fatalf("RegisterActiveWork() error = %v", err)
	}
	if !fixture.registry.ClearIfCurrent(fixture.session) {
		t.Fatal("ClearIfCurrent(old) = false")
	}
	newSession, err := installAuthenticated(fixture.registry, runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatalf("installAuthenticated(new) error = %v", err)
	}
	if newSession.Generation != fixture.session.Generation+1 {
		t.Fatalf("new generation = %d, want %d", newSession.Generation, fixture.session.Generation+1)
	}
	if count := fixture.tunnel.ActiveCount(); count != 1 {
		t.Fatalf("ActiveCount() after old Session cleanup = %d, want ACTIVE preserved", count)
	}
	if got := fixture.recorder.eventsSnapshot(); len(got) != 0 {
		t.Fatalf("old Session cleanup performed IO: %v", got)
	}
	if err := work.Finish(); err != nil {
		t.Fatalf("Finish() error = %v", err)
	}
}

func TestRevokeTunnelClosesAllGenerationsOutsideLock(t *testing.T) {
	fixture := newActiveWorkFixture(t, runtimeTunnelID, runtimeConnectorID)
	oldWork, err := fixture.tunnel.RegisterActiveWork(fixture.spec(runtimeWorkID, runtimeConnectionID))
	if err != nil {
		t.Fatalf("RegisterActiveWork(old) error = %v", err)
	}
	if !fixture.registry.ClearIfCurrent(fixture.session) {
		t.Fatal("ClearIfCurrent(old) = false")
	}
	newFixture := newActiveWorkFixtureWithRegistry(t, fixture.registry, runtimeTunnelID, runtimeConnectorID)
	newWork, err := newFixture.tunnel.RegisterActiveWork(newFixture.spec(runtimeWorkIDTwo, runtimeConnectionTwo))
	if err != nil {
		t.Fatalf("RegisterActiveWork(new) error = %v", err)
	}

	var wait sync.WaitGroup
	wait.Add(2)
	errorsCh := make(chan error, 2)
	go func() {
		defer wait.Done()
		errorsCh <- fixture.registry.RevokeTunnel(runtimeTunnelID)
	}()
	go func() {
		defer wait.Done()
		errorsCh <- oldWork.Finish()
	}()
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Errorf("concurrent Finish/Revoke error = %v", err)
		}
	}
	if err := newWork.Finish(); err != nil {
		t.Fatalf("Finish(after revoke) error = %v", err)
	}
	if count := fixture.tunnel.ActiveCount(); count != 0 {
		t.Fatalf("ActiveCount() after revoke = %d, want 0", count)
	}
	// Revoke 已释放原 Lease；这里构造尚未释放的同 Session Lease 只用于证明 revoked
	// 检查发生在 Runtime 锁内，且不会接管或关闭调用方的新资源。
	revokedSpec := fixture.spec(runtimeWorkID, runtimeConnectionID)
	revokedSpec.Lease = &ConnectorLease{runtime: fixture.tunnel, session: fixture.session, lifecycle: new(atomic.Uint32)}
	if _, err := fixture.tunnel.RegisterActiveWork(revokedSpec); !errors.Is(err, ErrTunnelRuntimeRevoked) {
		t.Fatalf("RegisterActiveWork(after revoke) error = %v, want ErrTunnelRuntimeRevoked", err)
	}
	fixture.recorder.assertEventCounts(t, 1)
	newFixture.recorder.assertEventCounts(t, 1)
}

func TestDifferentTunnelLocksAreIndependent(t *testing.T) {
	registry := newRegistry(sessionGenerator(1))
	first, err := registry.Tunnel(runtimeTunnelID)
	if err != nil {
		t.Fatalf("Tunnel(first) error = %v", err)
	}
	secondFixture := newActiveWorkFixtureWithRegistry(t, registry, runtimeTunnelIDTwo, runtimeConnectorID)

	first.mu.Lock()
	locked := true
	t.Cleanup(func() {
		if locked {
			first.mu.Unlock()
		}
	})
	completed := make(chan error, 1)
	go func() {
		work, registerErr := secondFixture.tunnel.RegisterActiveWork(secondFixture.spec(runtimeWorkID, runtimeConnectionID))
		if registerErr == nil {
			registerErr = work.Finish()
		}
		completed <- registerErr
	}()
	select {
	case err := <-completed:
		if err != nil {
			t.Fatalf("other Tunnel operation error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("other Tunnel operation was blocked by first Tunnel lock")
	}
	first.mu.Unlock()
	locked = false
}

func TestActiveWorkAggregatesCloseErrors(t *testing.T) {
	fixture := newActiveWorkFixture(t, runtimeTunnelID, runtimeConnectorID)
	workDeadlineErr := errors.New("work deadline")
	peerDeadlineErr := errors.New("peer deadline")
	workCloseErr := errors.New("work close")
	peerCloseErr := errors.New("peer close")
	fixture.workConn.deadlineErr = workDeadlineErr
	fixture.peerConn.deadlineErr = peerDeadlineErr
	fixture.workConn.closeErr = workCloseErr
	fixture.peerConn.closeErr = peerCloseErr
	work, err := fixture.tunnel.RegisterActiveWork(fixture.spec(runtimeWorkID, runtimeConnectionID))
	if err != nil {
		t.Fatalf("RegisterActiveWork() error = %v", err)
	}
	firstErr := work.Finish()
	secondErr := work.Close()
	for _, target := range []error{workDeadlineErr, peerDeadlineErr, workCloseErr, peerCloseErr} {
		if !errors.Is(firstErr, target) || !errors.Is(secondErr, target) {
			t.Fatalf("close errors = %v / %v, missing %v", firstErr, secondErr, target)
		}
	}
	fixture.recorder.assertEventCounts(t, 1)
}

func TestRevokeTunnelAggregatesErrorsFromAllActiveWork(t *testing.T) {
	registry := newRegistry(sessionGenerator(1))
	first := newActiveWorkFixtureWithRegistry(t, registry, runtimeTunnelID, runtimeConnectorID)
	second := newActiveWorkFixtureWithRegistry(t, registry, runtimeTunnelID, runtimeConnectorIDTwo)
	firstErr := errors.New("first work close")
	secondErr := errors.New("second peer close")
	first.workConn.closeErr = firstErr
	second.peerConn.closeErr = secondErr
	if _, err := first.tunnel.RegisterActiveWork(first.spec(runtimeWorkID, runtimeConnectionID)); err != nil {
		t.Fatalf("RegisterActiveWork(first) error = %v", err)
	}
	if _, err := second.tunnel.RegisterActiveWork(second.spec(runtimeWorkIDTwo, runtimeConnectionTwo)); err != nil {
		t.Fatalf("RegisterActiveWork(second) error = %v", err)
	}

	err := registry.RevokeTunnel(runtimeTunnelID)
	if !errors.Is(err, firstErr) || !errors.Is(err, secondErr) {
		t.Fatalf("RevokeTunnel() error = %v, want both close errors", err)
	}
	first.recorder.assertEventCounts(t, 1)
	second.recorder.assertEventCounts(t, 1)
}

type activeWorkFixture struct {
	registry *Registry
	tunnel   *TunnelRuntime
	session  Session
	lease    *ConnectorLease
	recorder *eventRecorder
	workConn *recordingConn
	peerConn *recordingConn
	cancel   func()
}

func newActiveWorkFixture(t *testing.T, tunnelID, connectorID string) *activeWorkFixture {
	t.Helper()
	return newActiveWorkFixtureWithRegistry(t, newRegistry(sessionGenerator(1)), tunnelID, connectorID)
}

func newActiveWorkFixtureWithRegistry(t *testing.T, registry *Registry, tunnelID, connectorID string) *activeWorkFixture {
	t.Helper()
	session, err := installAuthenticated(registry, tunnelID, connectorID)
	if err != nil {
		t.Fatalf("installAuthenticated() error = %v", err)
	}
	lease, err := registry.AcquireConnector(tunnelID)
	if err != nil {
		t.Fatalf("AcquireConnector() error = %v", err)
	}
	if lease.Session() != session {
		t.Fatalf("AcquireConnector() Session = %#v, want %#v", lease.Session(), session)
	}
	tunnel, err := registry.Tunnel(tunnelID)
	if err != nil {
		t.Fatalf("Tunnel() error = %v", err)
	}
	recorder := &eventRecorder{}
	return &activeWorkFixture{
		registry: registry, tunnel: tunnel, session: session, lease: lease,
		recorder: recorder,
		workConn: &recordingConn{name: "work", recorder: recorder},
		peerConn: &recordingConn{name: "peer", recorder: recorder},
		cancel:   func() { recorder.append("cancel") },
	}
}

func (fixture *activeWorkFixture) spec(workID, connectionID string) ActiveWorkSpec {
	return ActiveWorkSpec{
		Session: fixture.session, WorkID: workID, ConnectionID: connectionID,
		Cancel: fixture.cancel, WorkConn: fixture.workConn, PeerConn: fixture.peerConn, Lease: fixture.lease,
	}
}

type eventRecorder struct {
	mu     sync.Mutex
	events []string
}

func (recorder *eventRecorder) append(event string) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.events = append(recorder.events, event)
}

func (recorder *eventRecorder) eventsSnapshot() []string {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]string(nil), recorder.events...)
}

func (recorder *eventRecorder) assertEvents(t *testing.T, want []string) {
	t.Helper()
	got := recorder.eventsSnapshot()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func (recorder *eventRecorder) assertEventCounts(t *testing.T, want int) {
	t.Helper()
	counts := make(map[string]int)
	for _, event := range recorder.eventsSnapshot() {
		counts[event]++
	}
	for _, event := range []string{"cancel", "work:deadline", "peer:deadline", "work:close", "peer:close"} {
		if counts[event] != want {
			t.Fatalf("event %q count = %d, want %d; all=%v", event, counts[event], want, counts)
		}
	}
}

type recordingConn struct {
	name         string
	recorder     *eventRecorder
	deadlineErr  error
	closeErr     error
	closeStarted chan struct{}
	allowClose   <-chan struct{}
	closeOnce    sync.Once
}

func (connection *recordingConn) Read([]byte) (int, error)  { return 0, net.ErrClosed }
func (connection *recordingConn) Write([]byte) (int, error) { return 0, net.ErrClosed }
func (connection *recordingConn) Close() error {
	connection.recorder.append(connection.name + ":close")
	if connection.closeStarted != nil {
		connection.closeOnce.Do(func() { close(connection.closeStarted) })
	}
	if connection.allowClose != nil {
		<-connection.allowClose
	}
	return connection.closeErr
}
func (connection *recordingConn) LocalAddr() net.Addr  { return activeWorkAddr("local") }
func (connection *recordingConn) RemoteAddr() net.Addr { return activeWorkAddr("remote") }
func (connection *recordingConn) SetDeadline(time.Time) error {
	connection.recorder.append(connection.name + ":deadline")
	return connection.deadlineErr
}
func (connection *recordingConn) SetReadDeadline(time.Time) error  { return nil }
func (connection *recordingConn) SetWriteDeadline(time.Time) error { return nil }

type activeWorkAddr string

func (address activeWorkAddr) Network() string { return "test" }
func (address activeWorkAddr) String() string  { return string(address) }
