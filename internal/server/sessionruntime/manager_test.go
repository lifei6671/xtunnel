package sessionruntime

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/protocol/deterministic"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/state"
	servercontrolauth "github.com/lifei6671/xtunnel/internal/server/controlauth"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
	serverworkauth "github.com/lifei6671/xtunnel/internal/server/workauth"
	serverworkpool "github.com/lifei6671/xtunnel/internal/server/workpool"
)

const (
	testTunnelID      = "tun_01J00000000000000000000000"
	testConnectorID   = "con_01J00000000000000000000000"
	testConnectorTwo  = "con_01J00000000000000000000001"
	testSessionID     = "sess_01J00000000000000000000000"
	testSessionTwo    = "sess_01J00000000000000000000001"
	testProtocol      = uint32(1)
	testWait          = 2 * time.Second
	testWriteTimeout  = 250 * time.Millisecond
	testReplayEntries = 32
)

func TestServePublishesAndCleansCurrentSession(t *testing.T) {
	registry := serverruntime.NewRegistry()
	session := commitSession(t, registry, testTunnelID, testConnectorID)
	manager := newTestManager(t, registry)
	serverConnection, clientConnection := net.Pipe()
	t.Cleanup(func() { _ = clientConnection.Close() })
	established := establishedSession(t, session, 0x2a)
	result := make(chan error, 1)
	go func() { result <- manager.Serve(context.Background(), serverConnection, &established) }()
	readInitialDemand(t, clientConnection)

	waitFor(t, func() bool {
		_, exists := manager.Resolve(session.SessionID)
		return exists
	})
	for index, value := range established.SessionSecret {
		if value != 0 {
			t.Fatalf("SessionSecret[%d] = %d, want cleared", index, value)
		}
	}
	if _, exists := registry.Current(testTunnelID, testConnectorID); !exists {
		t.Fatal("Registry Current disappeared while Control Session was running")
	}

	if err := clientConnection.Close(); err != nil {
		t.Fatalf("close peer: %v", err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(testWait):
		t.Fatal("Serve() did not exit after peer close")
	}
	if _, exists := manager.Resolve(session.SessionID); exists {
		t.Fatal("Resolve() found Session after Control Owner exited")
	}
	if _, exists := registry.Current(testTunnelID, testConnectorID); exists {
		t.Fatal("Registry Current remained after Control Owner exited")
	}
}

func TestServeReplacementImmediatelyFencesOldWorkAuthentication(t *testing.T) {
	registry := serverruntime.NewRegistry()
	manager := newTestManager(t, registry)
	oldSession := commitSession(t, registry, testTunnelID, testConnectorID)
	oldServer, oldClient := net.Pipe()
	defer oldClient.Close()
	oldEstablished := establishedSession(t, oldSession, 0x11)
	oldResult := make(chan error, 1)
	go func() { oldResult <- manager.Serve(context.Background(), oldServer, &oldEstablished) }()
	readInitialDemand(t, oldClient)
	waitFor(t, func() bool {
		_, exists := manager.Resolve(oldSession.SessionID)
		return exists
	})

	newSession := commitSession(t, registry, testTunnelID, testConnectorID)
	if newSession.Generation <= oldSession.Generation {
		t.Fatalf("replacement generation = %d, want greater than %d", newSession.Generation, oldSession.Generation)
	}
	newServer, newClient := net.Pipe()
	defer newClient.Close()
	newEstablished := establishedSession(t, newSession, 0x22)
	newResult := make(chan error, 1)
	go func() { newResult <- manager.Serve(context.Background(), newServer, &newEstablished) }()
	readInitialDemand(t, newClient)
	waitFor(t, func() bool {
		_, exists := manager.Resolve(newSession.SessionID)
		return exists
	})
	if _, exists := manager.Resolve(oldSession.SessionID); exists {
		t.Fatal("old Session remained resolvable after replacement was published")
	}
	select {
	case err := <-oldResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("old Serve() error = %v, want context cancellation", err)
		}
	case <-time.After(testWait):
		t.Fatal("old Serve() did not exit after replacement cancellation")
	}
	if current, exists := registry.Current(testTunnelID, testConnectorID); !exists || current != newSession {
		t.Fatalf("Registry Current = %#v, %v, want replacement %#v", current, exists, newSession)
	}

	_ = newClient.Close()
	select {
	case err := <-newResult:
		if err != nil {
			t.Fatalf("new Serve() error = %v", err)
		}
	case <-time.After(testWait):
		t.Fatal("new Serve() did not exit after peer close")
	}
}

func TestServeRejectsLateLowerGenerationWithoutDisturbingCurrent(t *testing.T) {
	registry := serverruntime.NewRegistry()
	manager := newTestManager(t, registry)
	older := commitSession(t, registry, testTunnelID, testConnectorTwo)
	newer := commitSession(t, registry, testTunnelID, testConnectorTwo)

	newServer, newClient := net.Pipe()
	defer newClient.Close()
	newEstablished := establishedSession(t, newer, 0x44)
	newResult := make(chan error, 1)
	go func() { newResult <- manager.Serve(context.Background(), newServer, &newEstablished) }()
	readInitialDemand(t, newClient)
	waitFor(t, func() bool {
		_, exists := manager.Resolve(newer.SessionID)
		return exists
	})

	oldServer, oldClient := net.Pipe()
	defer oldClient.Close()
	oldEstablished := establishedSession(t, older, 0x33)
	oldResult := make(chan error, 1)
	go func() { oldResult <- manager.Serve(context.Background(), oldServer, &oldEstablished) }()
	select {
	case err := <-oldResult:
		if !errors.Is(err, ErrSessionSuperseded) {
			t.Fatalf("late old Serve() error = %v, want ErrSessionSuperseded", err)
		}
	case <-time.After(testWait):
		t.Fatal("late old Serve() did not reject superseded generation")
	}
	if _, exists := manager.Resolve(newer.SessionID); !exists {
		t.Fatal("late old cleanup removed current Session runtime")
	}
	if current, exists := registry.Current(testTunnelID, testConnectorTwo); !exists || current != newer {
		t.Fatalf("Registry Current = %#v, %v, want newer %#v", current, exists, newer)
	}

	_ = newClient.Close()
	select {
	case <-newResult:
	case <-time.After(testWait):
		t.Fatal("newer Serve() did not stop")
	}
}

func TestGrantLeaseRequiresPublishedCurrentSession(t *testing.T) {
	registry := serverruntime.NewRegistry()
	manager := newTestManager(t, registry)
	if err := manager.GrantLease(testSessionID, "lease_01J00000000000000000000000", 1, time.Second); !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("GrantLease(offline) error = %v, want ErrSessionUnavailable", err)
	}
}

func TestRegisterIdlePublishesWorkIntoCurrentSessionPool(t *testing.T) {
	registry := serverruntime.NewRegistry()
	session := commitSession(t, registry, testTunnelID, testConnectorID)
	manager := newTestManager(t, registry)
	controlServer, controlClient := net.Pipe()
	defer controlClient.Close()
	established := establishedSession(t, session, 0x55)
	serveResult := make(chan error, 1)
	go func() { serveResult <- manager.Serve(context.Background(), controlServer, &established) }()
	readInitialDemand(t, controlClient)
	waitFor(t, func() bool {
		_, exists := manager.Resolve(session.SessionID)
		return exists
	})

	workServer, workPeer := net.Pipe()
	defer workPeer.Close()
	idle := serverIdle(t, session)
	work, err := manager.RegisterIdle(workServer, idle)
	if err != nil {
		t.Fatalf("RegisterIdle() error = %v", err)
	}
	pool, exists := manager.Pool(session)
	if !exists {
		t.Fatal("Pool() did not find current Session")
	}
	counts := pool.Snapshot()
	if counts.Idle != 1 || counts.Total != 1 {
		t.Fatalf("Pool counts = %#v, want one IDLE", counts)
	}
	if err := work.Close(); err != nil {
		t.Fatalf("Work.Close() error = %v", err)
	}
	select {
	case <-work.Done():
	default:
		t.Fatal("Work.Done() remained open after close")
	}

	_ = controlClient.Close()
	select {
	case <-serveResult:
	case <-time.After(testWait):
		t.Fatal("Serve() did not stop")
	}
}

func TestServeDrainRequestFencesSessionTimesOutOpeningAndAcksDuplicates(t *testing.T) {
	registry := serverruntime.NewRegistry()
	session := commitSession(t, registry, testTunnelID, testConnectorID)
	manager := newTestManager(t, registry)
	controlServer, controlClient := net.Pipe()
	defer controlClient.Close()
	established := establishedSession(t, session, 0x66)
	serveResult := make(chan error, 1)
	go func() { serveResult <- manager.Serve(context.Background(), controlServer, &established) }()
	readInitialDemand(t, controlClient)
	waitFor(t, func() bool {
		_, exists := manager.Resolve(session.SessionID)
		return exists
	})

	workServer, workPeer := net.Pipe()
	defer workPeer.Close()
	if _, err := manager.RegisterIdle(workServer, serverIdle(t, session)); err != nil {
		t.Fatalf("RegisterIdle() error = %v", err)
	}
	pool, exists := manager.Pool(session)
	if !exists {
		t.Fatal("Pool() did not find Session before drain")
	}
	if _, err := pool.Acquire(context.Background(), time.Second); err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	request := &protocolv1.ControlEnvelope{
		ProtocolVersion: testProtocol,
		Payload: &protocolv1.ControlEnvelope_DrainRequest{DrainRequest: &protocolv1.DrainRequest{
			DrainId: "drain_01J00000000000000000000000", DrainTimeoutMs: 20,
		}},
	}
	if err := frame.WriteControl(controlClient, request); err != nil {
		t.Fatalf("write DrainRequest: %v", err)
	}
	ack := readDrainAck(t, controlClient)
	if ack.GetDrainId() != request.GetDrainRequest().GetDrainId() || ack.GetRemainingActive() != 0 {
		t.Fatalf("DrainAck = %#v, want matching ID and zero active", ack)
	}
	if _, exists := manager.Resolve(session.SessionID); exists {
		t.Fatal("draining Session remained available to WorkAuth")
	}
	if _, exists := manager.Pool(session); exists {
		t.Fatal("draining Session remained eligible to Tunnel Proxy")
	}
	select {
	case <-workPeerDone(workPeer):
	case <-time.After(testWait):
		t.Fatal("OPENING Work was not force-closed at drain timeout")
	}

	if err := frame.WriteControl(controlClient, request); err != nil {
		t.Fatalf("write duplicate DrainRequest: %v", err)
	}
	duplicate := readDrainAck(t, controlClient)
	if duplicate.GetDrainId() != ack.GetDrainId() || duplicate.GetRemainingActive() != ack.GetRemainingActive() {
		t.Fatalf("duplicate DrainAck = %#v, want %#v", duplicate, ack)
	}
	_ = controlClient.Close()
	select {
	case <-serveResult:
	case <-time.After(testWait):
		t.Fatal("Serve() did not stop after drained peer close")
	}
}

func TestServeOwnerExitCancelsUnboundedDrainWait(t *testing.T) {
	registry := serverruntime.NewRegistry()
	session := commitSession(t, registry, testTunnelID, testConnectorID)
	manager := newTestManager(t, registry)
	controlServer, controlClient := net.Pipe()
	established := establishedSession(t, session, 0x67)
	serveResult := make(chan error, 1)
	go func() { serveResult <- manager.Serve(context.Background(), controlServer, &established) }()
	readInitialDemand(t, controlClient)

	workServer, workPeer := net.Pipe()
	defer workPeer.Close()
	if _, err := manager.RegisterIdle(workServer, serverIdle(t, session)); err != nil {
		t.Fatalf("RegisterIdle() error = %v", err)
	}
	pool, exists := manager.Pool(session)
	if !exists {
		t.Fatal("Pool() did not find Session before drain")
	}
	if _, err := pool.Acquire(context.Background(), time.Second); err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	request := &protocolv1.ControlEnvelope{
		ProtocolVersion: testProtocol,
		Payload: &protocolv1.ControlEnvelope_DrainRequest{DrainRequest: &protocolv1.DrainRequest{
			DrainId: "drain_01J00000000000000000000002", DrainTimeoutMs: ^uint32(0),
		}},
	}
	if err := frame.WriteControl(controlClient, request); err != nil {
		t.Fatalf("write unbounded DrainRequest: %v", err)
	}
	waitFor(t, func() bool {
		_, available := manager.Resolve(session.SessionID)
		return !available
	})
	if err := controlClient.Close(); err != nil {
		t.Fatalf("close Control peer during drain: %v", err)
	}
	select {
	case <-serveResult:
	case <-time.After(testWait):
		t.Fatal("Serve() remained blocked on peer drain_timeout after Owner exit")
	}
	select {
	case <-workPeerDone(workPeer):
	case <-time.After(testWait):
		t.Fatal("Owner exit did not close the canceled OPENING WorkConn")
	}
}

func TestShutdownWaitsForActiveThenClosesControlSession(t *testing.T) {
	manager, registry, runtime, session, active, work := activeShutdownFixture(t)

	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
	defer cancelShutdown()
	shutdownResult := make(chan error, 1)
	go func() { shutdownResult <- manager.Shutdown(shutdownContext) }()
	select {
	case err := <-shutdownResult:
		t.Fatalf("Shutdown() returned before ACTIVE naturally finished: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if _, exists := manager.Pool(session); exists {
		t.Fatal("Shutdown left Session eligible for new Tunnel OPEN")
	}
	if err := active.Finish(); err != nil {
		t.Fatalf("ActiveWork.Finish() error = %v", err)
	}
	if err := work.Close(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Work.Close() error = %v, want nil or io.ErrClosedPipe", err)
	}
	select {
	case err := <-shutdownResult:
		if err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	case <-time.After(testWait):
		t.Fatal("Shutdown() did not finish after ACTIVE naturally ended")
	}
	if got := runtime.ActiveCount(); got != 0 {
		t.Fatalf("ActiveCount() after shutdown = %d, want 0", got)
	}
	if _, exists := registry.Current(testTunnelID, testConnectorID); exists {
		waitFor(t, func() bool {
			_, current := registry.Current(testTunnelID, testConnectorID)
			return !current
		})
	}
}

func TestShutdownDeadlineForceClosesActive(t *testing.T) {
	manager, _, runtime, _, active, work := activeShutdownFixture(t)
	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelShutdown()
	if err := manager.Shutdown(shutdownContext); err != nil {
		t.Fatalf("Shutdown(deadline) error = %v", err)
	}
	if got := runtime.ActiveCount(); got != 0 {
		t.Fatalf("ActiveCount() after forced shutdown = %d, want 0", got)
	}
	if err := active.Finish(); err != nil {
		t.Fatalf("ActiveWork.Finish(after force) error = %v", err)
	}
	if err := work.Close(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Work.Close() after forced shutdown error = %v, want nil or io.ErrClosedPipe", err)
	}
}

func TestServeHeartbeatReconcilesExpiredDemandWithoutRequestStorm(t *testing.T) {
	registry := serverruntime.NewRegistry()
	session := commitSession(t, registry, testTunnelID, testConnectorID)
	manager := newTestManager(t, registry)
	manager.demandLeaseTTL = 20 * time.Millisecond
	controlServer, controlClient := net.Pipe()
	defer controlClient.Close()
	established := establishedSession(t, session, 0x77)
	serveResult := make(chan error, 1)
	go func() { serveResult <- manager.Serve(context.Background(), controlServer, &established) }()
	first := readWorkDemand(t, controlClient)
	if first.GetDemandGeneration() != 1 {
		t.Fatalf("initial demand generation = %d, want 1", first.GetDemandGeneration())
	}

	time.Sleep(30 * time.Millisecond)
	writeHeartbeat(t, controlClient, 0)
	second := readWorkDemand(t, controlClient)
	if second.GetDemandGeneration() != 2 || second.GetBudgetLeaseId() == first.GetBudgetLeaseId() {
		t.Fatalf("reconciled WorkDemand = %#v, want new generation and lease", second)
	}

	writeHeartbeat(t, controlClient, ^uint64(0))
	if err := controlClient.SetReadDeadline(time.Now().Add(40 * time.Millisecond)); err != nil {
		t.Fatalf("set no-storm read deadline: %v", err)
	}
	unexpected := &protocolv1.ControlEnvelope{}
	err := frame.ReadControl(controlClient, unexpected)
	if err == nil {
		t.Fatalf("fresh lease emitted duplicate WorkDemand %#v", unexpected.GetWorkDemand())
	}
	var networkError net.Error
	if !errors.As(err, &networkError) || !networkError.Timeout() {
		t.Fatalf("fresh lease read error = %v, want timeout with no duplicate Demand", err)
	}
	if err := controlClient.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear no-storm read deadline: %v", err)
	}

	_ = controlClient.Close()
	select {
	case <-serveResult:
	case <-time.After(testWait):
		t.Fatal("Serve() did not stop after demand reconciliation test")
	}
}

func TestServeHeartbeatRefillsAfterInitialDemandIsConsumed(t *testing.T) {
	registry := serverruntime.NewRegistry()
	session := commitSession(t, registry, testTunnelID, testConnectorID)
	manager := newTestManager(t, registry)
	controlServer, controlClient := net.Pipe()
	defer controlClient.Close()
	established := establishedSession(t, session, 0x78)
	serveResult := make(chan error, 1)
	go func() { serveResult <- manager.Serve(context.Background(), controlServer, &established) }()
	initial := readWorkDemand(t, controlClient)

	for range initialDesiredNonActive {
		workServer, workPeer := net.Pipe()
		t.Cleanup(func() { _ = workPeer.Close() })
		workID, err := identity.NewWorkID()
		if err != nil {
			t.Fatalf("NewWorkID() error = %v", err)
		}
		if _, err := manager.RegisterIdle(workServer, serverIdleWithWorkID(t, session, workID)); err != nil {
			t.Fatalf("RegisterIdle(%s) error = %v", workID, err)
		}
	}
	pool, exists := manager.Pool(session)
	if !exists {
		t.Fatal("Pool() did not find current Session")
	}
	if counts := pool.Snapshot(); counts.Idle != initialDesiredNonActive {
		t.Fatalf("Pool counts after consuming initial Demand = %#v, want 8 IDLE", counts)
	}
	consumed, err := pool.Acquire(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if err := consumed.Close(); err != nil {
		t.Fatalf("close consumed WorkConn: %v", err)
	}

	writeHeartbeat(t, controlClient, 0)
	refill := readWorkDemand(t, controlClient)
	if refill.GetDemandGeneration() <= initial.GetDemandGeneration() ||
		refill.GetBudgetLeaseId() == initial.GetBudgetLeaseId() ||
		refill.GetDesiredNonActive() != initialDesiredNonActive || refill.GetMaxNewConnections() != 1 {
		t.Fatalf("refill WorkDemand = %#v, want one new slot in a newer generation", refill)
	}

	_ = controlClient.Close()
	select {
	case <-serveResult:
	case <-time.After(testWait):
		t.Fatal("Serve() did not stop after consumed-demand reconciliation test")
	}
}

func TestPendingDemandDecreaseImmediatelyCancelsLeaseAndCanRiseAgain(t *testing.T) {
	registry := serverruntime.NewRegistry()
	session := commitSession(t, registry, testTunnelID, testConnectorID)
	manager := newTestManager(t, registry)
	controlServer, controlClient := net.Pipe()
	defer controlClient.Close()
	const secretByte = byte(0x79)
	established := establishedSession(t, session, secretByte)
	serveResult := make(chan error, 1)
	go func() { serveResult <- manager.Serve(context.Background(), controlServer, &established) }()
	initial := readWorkDemand(t, controlClient)

	for range initialDesiredNonActive {
		workServer, workPeer := net.Pipe()
		t.Cleanup(func() { _ = workPeer.Close() })
		workID, err := identity.NewWorkID()
		if err != nil {
			t.Fatalf("NewWorkID() error = %v", err)
		}
		work, err := manager.RegisterIdle(workServer, serverIdleWithWorkID(t, session, workID))
		if err != nil {
			t.Fatalf("RegisterIdle(%s) error = %v", workID, err)
		}
		if err := work.Close(); err != nil {
			t.Fatalf("close initial WorkConn: %v", err)
		}
	}
	if err := manager.SetPendingOpens(session, initialDesiredNonActive+1); err != nil {
		t.Fatalf("SetPendingOpens(rise) error = %v", err)
	}
	risen := readWorkDemand(t, controlClient)
	if risen.GetDemandGeneration() <= initial.GetDemandGeneration() ||
		risen.GetDesiredNonActive() != initialDesiredNonActive+1 ||
		risen.GetBudgetLeaseId() == "" || risen.GetMaxNewConnections() == 0 {
		t.Fatalf("risen WorkDemand = %#v, want a granted target of 9", risen)
	}

	if err := manager.SetPendingOpens(session, 0); err != nil {
		t.Fatalf("SetPendingOpens(decrease) error = %v", err)
	}
	lowered := readWorkDemand(t, controlClient)
	if lowered.GetDemandGeneration() <= risen.GetDemandGeneration() ||
		lowered.GetDesiredNonActive() != initialDesiredNonActive ||
		lowered.GetBudgetLeaseId() != "" || lowered.GetMaxNewConnections() != 0 || lowered.GetLeaseTtlMs() != 0 {
		t.Fatalf("lowered WorkDemand = %#v, want an immediate pure target of 8", lowered)
	}
	authenticator, exists := manager.Resolve(session.SessionID)
	if !exists {
		t.Fatal("Resolve() did not find current Session")
	}
	revokedHello := signedWorkHello(t, session, risen.GetBudgetLeaseId(), secretByte)
	var decision *serverworkauth.DecisionError
	if err := authenticator.ValidateAndConsume(revokedHello); !errors.As(err, &decision) ||
		decision.Reason != serverworkauth.ReasonLeaseInvalid {
		t.Fatalf("revoked Lease ValidateAndConsume() error = %v, want LeaseInvalid", err)
	}

	if err := manager.SetPendingOpens(session, initialDesiredNonActive+1); err != nil {
		t.Fatalf("SetPendingOpens(re-rise) error = %v", err)
	}
	regranted := readWorkDemand(t, controlClient)
	if regranted.GetDemandGeneration() <= lowered.GetDemandGeneration() ||
		regranted.GetDesiredNonActive() != initialDesiredNonActive+1 ||
		regranted.GetBudgetLeaseId() == "" || regranted.GetBudgetLeaseId() == risen.GetBudgetLeaseId() ||
		regranted.GetMaxNewConnections() == 0 {
		t.Fatalf("regranted WorkDemand = %#v, want a fresh granted target of 9", regranted)
	}
	if err := authenticator.ValidateAndConsume(
		signedWorkHello(t, session, regranted.GetBudgetLeaseId(), secretByte),
	); err != nil {
		t.Fatalf("new Lease ValidateAndConsume() error = %v", err)
	}

	_ = controlClient.Close()
	select {
	case <-serveResult:
	case <-time.After(testWait):
		t.Fatal("Serve() did not stop after Pending Demand decrease test")
	}
}

func TestServeHeartbeatTimeoutUsesLocalReceiptTimeAndCleansSession(t *testing.T) {
	registry := serverruntime.NewRegistry()
	session := commitSession(t, registry, testTunnelID, testConnectorID)
	manager, err := New(registry, Options{
		HighPriorityCapacity: 8, NormalCapacity: 8, InboundCapacity: 8,
		WriteTimeout: testWriteTimeout, MaxReplayEntries: testReplayEntries,
		MaxWorkTotal: 64, MaxWorkConnecting: 16, HeartbeatTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	controlServer, controlClient := net.Pipe()
	defer controlClient.Close()
	established := establishedSession(t, session, 0x88)
	serveResult := make(chan error, 1)
	go func() { serveResult <- manager.Serve(context.Background(), controlServer, &established) }()
	readInitialDemand(t, controlClient)

	time.Sleep(60 * time.Millisecond)
	writeHeartbeat(t, controlClient, ^uint64(0))
	time.Sleep(60 * time.Millisecond)
	select {
	case err := <-serveResult:
		t.Fatalf("Serve() exited %v before a full timeout since the locally received heartbeat", err)
	default:
	}

	select {
	case err := <-serveResult:
		if !errors.Is(err, ErrHeartbeatTimeout) {
			t.Fatalf("Serve() error = %v, want ErrHeartbeatTimeout", err)
		}
	case <-time.After(testWait):
		t.Fatal("Serve() did not close a heartbeat-stale Session")
	}
	if _, exists := manager.Resolve(session.SessionID); exists {
		t.Fatal("heartbeat-stale Session remained resolvable after cleanup")
	}
	if _, exists := registry.Current(testTunnelID, testConnectorID); exists {
		t.Fatal("heartbeat-stale Session remained current after cleanup")
	}
}

func newTestManager(t *testing.T, registry *serverruntime.Registry) *Manager {
	t.Helper()
	manager, err := New(registry, Options{
		HighPriorityCapacity: 8, NormalCapacity: 8, InboundCapacity: 8,
		WriteTimeout: testWriteTimeout, MaxReplayEntries: testReplayEntries,
		MaxWorkTotal: 64, MaxWorkConnecting: 16, HeartbeatTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return manager
}

func activeShutdownFixture(t *testing.T) (
	*Manager,
	*serverruntime.Registry,
	*serverruntime.TunnelRuntime,
	serverruntime.Session,
	*serverruntime.ActiveWork,
	*serverworkpool.Work,
) {
	t.Helper()
	registry := serverruntime.NewRegistry()
	session := commitSession(t, registry, testTunnelID, testConnectorID)
	manager := newTestManager(t, registry)
	controlServer, controlClient := net.Pipe()
	established := establishedSession(t, session, 0x68)
	serveResult := make(chan error, 1)
	go func() { serveResult <- manager.Serve(context.Background(), controlServer, &established) }()
	readInitialDemand(t, controlClient)
	t.Cleanup(func() {
		_ = controlClient.Close()
		select {
		case <-serveResult:
		case <-time.After(testWait):
			t.Error("Serve() did not stop after shutdown fixture cleanup")
		}
	})

	workServer, workPeer := net.Pipe()
	t.Cleanup(func() { _ = workPeer.Close() })
	work, err := manager.RegisterIdle(workServer, serverIdle(t, session))
	if err != nil {
		t.Fatalf("RegisterIdle() error = %v", err)
	}
	pool, exists := manager.Pool(session)
	if !exists {
		t.Fatal("Pool() did not find active shutdown Session")
	}
	acquired, err := pool.Acquire(context.Background(), time.Second)
	if err != nil || acquired != work {
		t.Fatalf("Acquire() = %p, %v, want registered Work", acquired, err)
	}
	if err := work.MarkActive(); err != nil {
		t.Fatalf("MarkActive() error = %v", err)
	}
	connectorLease, err := registry.AcquireConnector(testTunnelID)
	if err != nil {
		t.Fatalf("AcquireConnector() error = %v", err)
	}
	tunnelRuntime, err := registry.Tunnel(testTunnelID)
	if err != nil {
		t.Fatalf("Tunnel() error = %v", err)
	}
	peerServer, peerClient := net.Pipe()
	t.Cleanup(func() { _ = peerClient.Close() })
	_, cancelWork := context.WithCancel(context.Background())
	active, err := tunnelRuntime.RegisterActiveWork(serverruntime.ActiveWorkSpec{
		Session: session, WorkID: work.ID(), ConnectionID: "conn_01J00000000000000000000000",
		Cancel: cancelWork, WorkConn: workServer, PeerConn: peerServer, Lease: connectorLease,
	})
	if err != nil {
		connectorLease.Release()
		cancelWork()
		t.Fatalf("RegisterActiveWork() error = %v", err)
	}
	return manager, registry, tunnelRuntime, session, active, work
}

func commitSession(t *testing.T, registry *serverruntime.Registry, tunnelID, connectorID string) serverruntime.Session {
	t.Helper()
	pending, err := registry.ReserveAuthenticated(tunnelID, connectorID)
	if err != nil {
		t.Fatalf("ReserveAuthenticated() error = %v", err)
	}
	session, err := registry.CommitAuthenticated(pending)
	if err != nil {
		t.Fatalf("CommitAuthenticated() error = %v", err)
	}
	return session
}

func establishedSession(t *testing.T, session serverruntime.Session, secretByte byte) servercontrolauth.Established {
	t.Helper()
	control, err := state.NewControl(state.EndpointServer, testProtocol)
	if err != nil {
		t.Fatalf("NewControl() error = %v", err)
	}
	result := &protocolv1.ConnectorAuthResult{Result: &protocolv1.ConnectorAuthResult_Success{
		Success: &protocolv1.ConnectorAuthSuccess{SessionSecret: make([]byte, 32)},
	}}
	if _, err := control.AcceptOutbound(result); err != nil {
		t.Fatalf("AcceptOutbound(auth success) error = %v", err)
	}
	if err := control.CommitAuthSuccessAfterFlush(result); err != nil {
		t.Fatalf("CommitAuthSuccessAfterFlush() error = %v", err)
	}
	var secret [32]byte
	for index := range secret {
		secret[index] = secretByte
	}
	return servercontrolauth.Established{
		Session: session, SessionSecret: secret, ProtocolVersion: testProtocol, Control: control,
	}
}

func serverIdle(t *testing.T, session serverruntime.Session) serverworkauth.Idle {
	return serverIdleWithWorkID(t, session, "work_01J00000000000000000000000")
}

func serverIdleWithWorkID(t *testing.T, session serverruntime.Session, workID string) serverworkauth.Idle {
	t.Helper()
	workState, err := state.NewWork(state.EndpointServer)
	if err != nil {
		t.Fatalf("NewWork() error = %v", err)
	}
	hello := &protocolv1.WorkHello{
		TunnelId: session.TunnelID, ConnectorId: session.ConnectorID, SessionId: session.SessionID,
		WorkId: workID, Nonce: make([]byte, 32), Mac: make([]byte, 32),
		BudgetLeaseId: "lease_01J00000000000000000000000",
	}
	if err := workState.AcceptInbound(hello); err != nil {
		t.Fatalf("AcceptInbound(WorkHello) error = %v", err)
	}
	ready := &protocolv1.WorkReady{
		WorkId: hello.GetWorkId(), Status: protocolv1.WorkReadyStatus_WORK_READY_STATUS_READY,
		ErrorCode: protocolv1.ErrorCode_ERROR_CODE_OK,
	}
	if err := workState.AcceptOutbound(ready); err != nil {
		t.Fatalf("AcceptOutbound(WorkReady) error = %v", err)
	}
	return serverworkauth.Idle{
		TunnelID: session.TunnelID, ConnectorID: session.ConnectorID, SessionID: session.SessionID,
		WorkID: hello.GetWorkId(), State: workState,
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition was not satisfied before deadline")
		}
		time.Sleep(time.Millisecond)
	}
}

func readInitialDemand(t *testing.T, connection net.Conn) {
	t.Helper()
	demand := readWorkDemand(t, connection)
	if demand.GetDesiredNonActive() != initialDesiredNonActive ||
		demand.GetMaxNewConnections() != initialDesiredNonActive || demand.GetDemandGeneration() != 1 ||
		demand.GetLeaseTtlMs() != uint32(initialLeaseTTL/time.Millisecond) {
		t.Fatalf("initial WorkDemand = %#v", demand)
	}
}

func readWorkDemand(t *testing.T, connection net.Conn) *protocolv1.WorkDemand {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(testWait)); err != nil {
		t.Fatalf("set WorkDemand deadline: %v", err)
	}
	envelope := &protocolv1.ControlEnvelope{}
	if err := frame.ReadControl(connection, envelope); err != nil {
		t.Fatalf("read WorkDemand: %v", err)
	}
	if err := connection.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear WorkDemand deadline: %v", err)
	}
	demand := envelope.GetWorkDemand()
	if demand == nil {
		t.Fatalf("Control message = %#v, want WorkDemand", envelope)
	}
	return demand
}

func writeHeartbeat(t *testing.T, connection net.Conn, timestamp uint64) {
	t.Helper()
	envelope := &protocolv1.ControlEnvelope{
		ProtocolVersion: testProtocol,
		Payload:         &protocolv1.ControlEnvelope_Heartbeat{Heartbeat: &protocolv1.Heartbeat{TimestampMs: timestamp}},
	}
	if err := frame.WriteControl(connection, envelope); err != nil {
		t.Fatalf("write Heartbeat: %v", err)
	}
}

func readDrainAck(t *testing.T, connection net.Conn) *protocolv1.DrainAck {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(testWait)); err != nil {
		t.Fatalf("set DrainAck deadline: %v", err)
	}
	envelope := &protocolv1.ControlEnvelope{}
	if err := frame.ReadControl(connection, envelope); err != nil {
		t.Fatalf("read DrainAck: %v", err)
	}
	if err := connection.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear DrainAck deadline: %v", err)
	}
	if envelope.GetDrainAck() == nil {
		t.Fatalf("Control message = %#v, want DrainAck", envelope)
	}
	return envelope.GetDrainAck()
}

func workPeerDone(connection net.Conn) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		var one [1]byte
		_, _ = connection.Read(one[:])
	}()
	return done
}

func signedWorkHello(
	t *testing.T,
	session serverruntime.Session,
	leaseID string,
	secretByte byte,
) *protocolv1.WorkHello {
	t.Helper()
	workID, err := identity.NewWorkID()
	if err != nil {
		t.Fatalf("NewWorkID() error = %v", err)
	}
	hello := &protocolv1.WorkHello{
		TunnelId: session.TunnelID, ConnectorId: session.ConnectorID, SessionId: session.SessionID,
		WorkId: workID, BudgetLeaseId: leaseID, Nonce: make([]byte, 32),
	}
	secret := make([]byte, 32)
	for index := range secret {
		secret[index] = secretByte
	}
	mac, err := deterministic.ComputeWorkHelloMAC(secret, hello)
	clear(secret)
	if err != nil {
		t.Fatalf("ComputeWorkHelloMAC() error = %v", err)
	}
	hello.Mac = mac
	return hello
}
