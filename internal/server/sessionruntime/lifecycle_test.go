package sessionruntime

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/safego"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
)

func TestConnectorSnapshotsMetricsHeartbeatDrainAndLifecycleLogs(t *testing.T) {
	registry := serverruntime.NewRegistry()
	session := commitSession(t, registry, testTunnelID, testConnectorID)
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	manager, err := New(registry, Options{
		HighPriorityCapacity: 8, NormalCapacity: 8, InboundCapacity: 8,
		WriteTimeout: testWriteTimeout, MaxReplayEntries: testReplayEntries,
		MaxWorkTotal: 64, MaxWorkConnecting: 16, HeartbeatTimeout: 5 * time.Second,
		SnapshotProvider: testSnapshotProvider{},
		Logger:           logger,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	startTestManager(t, manager)
	controlServer, controlClient := net.Pipe()
	established := establishedSession(t, session, 0x7a)
	serveResult := make(chan error, 1)
	go func() { serveResult <- manager.Serve(context.Background(), controlServer, &established) }()
	readInitialDemand(t, controlClient)

	var connectedAt, firstHeartbeat time.Time
	waitFor(t, func() bool {
		snapshots := manager.ConnectorSnapshots()
		if len(snapshots) != 1 {
			return false
		}
		connectedAt = snapshots[0].ConnectedAt
		firstHeartbeat = snapshots[0].LastHeartbeatAt
		return snapshots[0].Status == serverruntime.ConnectorStatusOnline &&
			snapshots[0].Hostname == "edge-test" && snapshots[0].Session == session
	})
	time.Sleep(2 * time.Millisecond)
	writeHeartbeat(t, controlClient, ^uint64(0))
	waitFor(t, func() bool {
		snapshots := manager.ConnectorSnapshots()
		return len(snapshots) == 1 && snapshots[0].LastHeartbeatAt.After(firstHeartbeat)
	})
	if connectedAt.IsZero() || firstHeartbeat.Before(connectedAt) {
		t.Fatalf("Server lifecycle timestamps = connected %v heartbeat %v", connectedAt, firstHeartbeat)
	}

	workServer, workClient := net.Pipe()
	defer workClient.Close()
	if _, err := manager.RegisterIdle(workServer, serverIdle(t, session)); err != nil {
		t.Fatalf("RegisterIdle() error = %v", err)
	}
	waitFor(t, func() bool {
		metrics := manager.MetricsSnapshot()
		return metrics.XTunnelConnectorsOnline == 1 && metrics.XTunnelControlSessionsOnline == 1 &&
			metrics.XTunnelTCPIdleWorkConnections == 1 && metrics.XTunnelActiveConnections == 0 &&
			metrics.XTunnelTCPActiveWorkConnections == 0
	})

	manager.mu.Lock()
	managed := manager.byConnector[connectorKey{tunnelID: testTunnelID, connectorID: testConnectorID}]
	manager.mu.Unlock()
	if managed == nil {
		t.Fatal("managed Session missing before drain")
	}
	manager.markDraining(managed)
	snapshots := manager.ConnectorSnapshots()
	if len(snapshots) != 1 || snapshots[0].Status != serverruntime.ConnectorStatusDraining {
		t.Fatalf("ConnectorSnapshots() after drain = %#v", snapshots)
	}
	metrics := manager.MetricsSnapshot()
	if metrics.XTunnelConnectorsOnline != 0 || metrics.XTunnelControlSessionsOnline != 1 {
		t.Fatalf("MetricsSnapshot() during drain = %#v", metrics)
	}

	if err := controlClient.Close(); err != nil {
		t.Fatalf("close Control peer: %v", err)
	}
	select {
	case err := <-serveResult:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(testWait):
		t.Fatal("Serve() did not stop after disconnect")
	}
	if snapshots := manager.ConnectorSnapshots(); len(snapshots) != 0 {
		t.Fatalf("ConnectorSnapshots() after disconnected idle-only Session = %#v", snapshots)
	}
	logs := output.String()
	for _, event := range []string{
		serverruntime.ConnectorEventConnected,
		serverruntime.ConnectorEventDraining,
		serverruntime.ConnectorEventDisconnected,
	} {
		if !strings.Contains(logs, `"msg":"`+event+`"`) {
			t.Fatalf("lifecycle logs missing %q: %s", event, logs)
		}
	}
	if strings.Contains(logs, "session_secret") || strings.Contains(logs, "xta_") {
		t.Fatalf("lifecycle logs exposed a Secret: %s", logs)
	}
}

func TestRevokeTunnelFencesLateServeAndCleansRunningSession(t *testing.T) {
	registry := serverruntime.NewRegistry()
	manager := newTestManager(t, registry)
	session := commitSession(t, registry, testTunnelID, testConnectorID)
	controlServer, controlClient := net.Pipe()
	established := establishedSession(t, session, 0x5a)
	serveResult := make(chan error, 1)
	go func() { serveResult <- manager.Serve(context.Background(), controlServer, &established) }()
	readInitialDemand(t, controlClient)
	waitFor(t, func() bool {
		_, exists := manager.Resolve(session.SessionID)
		return exists
	})

	if err := manager.RevokeTunnel(testTunnelID); err != nil {
		t.Fatalf("RevokeTunnel() error = %v", err)
	}
	select {
	case err := <-serveResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Serve() after revoke error = %v, want context cancellation", err)
		}
	case <-time.After(testWait):
		t.Fatal("Serve() did not stop after RevokeTunnel")
	}
	_ = controlClient.Close()
	if _, exists := manager.Resolve(session.SessionID); exists {
		t.Fatal("revoked Session remained resolvable")
	}
	if snapshots := manager.ConnectorSnapshots(); len(snapshots) != 0 {
		t.Fatalf("revoked Connector snapshots = %#v", snapshots)
	}

	lateServer, lateClient := net.Pipe()
	defer lateClient.Close()
	lateEstablished := establishedSession(t, session, 0x6a)
	if err := manager.Serve(context.Background(), lateServer, &lateEstablished); !errors.Is(err, serverruntime.ErrTunnelRuntimeRevoked) {
		t.Fatalf("late Serve() error = %v, want ErrTunnelRuntimeRevoked", err)
	}
	if _, exists := manager.Resolve(session.SessionID); exists {
		t.Fatal("late Serve republished a revoked Session")
	}
}

func TestConvergenceWaitsForStartedOwnerBeforeInstall(t *testing.T) {
	for _, operation := range []string{"revoke", "shutdown"} {
		t.Run(operation, func(t *testing.T) {
			registry := serverruntime.NewRegistry()
			manager := newTestManager(t, registry)
			session := commitSession(t, registry, testTunnelID, testConnectorID)
			installStarted := make(chan struct{})
			releaseInstall := make(chan struct{})
			var releaseInstallOnce sync.Once
			unblockInstall := func() { releaseInstallOnce.Do(func() { close(releaseInstall) }) }
			t.Cleanup(unblockInstall)
			manager.beforeInstallForTest = func(serverruntime.Session) {
				close(installStarted)
				<-releaseInstall
			}

			controlServer, controlClient := net.Pipe()
			t.Cleanup(func() { _ = controlClient.Close() })
			closeStarted := make(chan struct{})
			releaseClose := make(chan struct{})
			var releaseCloseOnce sync.Once
			unblockClose := func() { releaseCloseOnce.Do(func() { close(releaseClose) }) }
			t.Cleanup(unblockClose)
			blockingControl := &blockingCloseConn{
				Conn: controlServer, started: closeStarted, release: releaseClose,
			}
			established := establishedSession(t, session, 0x4a)
			serveResult := make(chan error, 1)
			go func() { serveResult <- manager.Serve(context.Background(), blockingControl, &established) }()
			select {
			case <-installStarted:
			case <-time.After(testWait):
				t.Fatal("Serve() did not pause before install")
			}

			operationResult := make(chan error, 1)
			switch operation {
			case "revoke":
				go func() { operationResult <- manager.RevokeTunnel(testTunnelID) }()
				waitFor(t, func() bool {
					manager.mu.Lock()
					_, revoked := manager.revokedTunnels[testTunnelID]
					manager.mu.Unlock()
					return revoked
				})
			case "shutdown":
				shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), testWait)
				t.Cleanup(cancelShutdown)
				go func() { operationResult <- manager.Shutdown(shutdownContext) }()
				waitFor(t, func() bool {
					manager.mu.Lock()
					started := manager.shutdownStarted
					manager.mu.Unlock()
					return started
				})
			}
			select {
			case err := <-operationResult:
				t.Fatalf("%s returned before startup reservation completed: %v", operation, err)
			default:
			}

			unblockInstall()
			select {
			case <-closeStarted:
			case <-time.After(testWait):
				t.Fatal("failed Serve() did not reach Control close")
			}
			select {
			case err := <-operationResult:
				t.Fatalf("%s returned before started Owner cleanup completed: %v", operation, err)
			default:
			}

			unblockClose()
			select {
			case err := <-operationResult:
				if err != nil {
					t.Fatalf("%s error = %v", operation, err)
				}
			case <-time.After(testWait):
				t.Fatalf("%s did not finish after Control close", operation)
			}
			select {
			case err := <-serveResult:
				want := ErrSessionUnavailable
				if operation == "revoke" {
					want = serverruntime.ErrTunnelRuntimeRevoked
				}
				if !errors.Is(err, want) {
					t.Fatalf("Serve() error = %v, want %v", err, want)
				}
			case <-time.After(testWait):
				t.Fatal("Serve() did not finish after convergence")
			}
			manager.mu.Lock()
			starting := len(manager.startingByTunnel)
			live := len(manager.liveSessions)
			manager.mu.Unlock()
			if starting != 0 || live != 0 {
				t.Fatalf("Manager ownership after convergence = starting %d live %d", starting, live)
			}
		})
	}
}

func TestConvergenceFreezesLifecycleReasonBeforeUnlock(t *testing.T) {
	for _, operation := range []string{"revoke", "shutdown"} {
		t.Run(operation, func(t *testing.T) {
			registry := serverruntime.NewRegistry()
			var output bytes.Buffer
			manager, err := New(registry, Options{
				HighPriorityCapacity: 8, NormalCapacity: 8, InboundCapacity: 8,
				WriteTimeout: testWriteTimeout, MaxReplayEntries: testReplayEntries,
				MaxWorkTotal: 64, MaxWorkConnecting: 16, HeartbeatTimeout: 5 * time.Second,
				SnapshotProvider: testSnapshotProvider{},
				Logger:           slog.New(slog.NewJSONHandler(&output, nil)),
			})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			startTestManager(t, manager)

			removed := make(chan struct{})
			releaseRemove := make(chan struct{})
			var releaseRemoveOnce sync.Once
			unblockRemove := func() { releaseRemoveOnce.Do(func() { close(releaseRemove) }) }
			t.Cleanup(unblockRemove)
			manager.afterRemoveForTest = func(serverruntime.Session) {
				close(removed)
				<-releaseRemove
			}
			fenced := make(chan struct{})
			releaseFence := make(chan struct{})
			var releaseFenceOnce sync.Once
			unblockFence := func() { releaseFenceOnce.Do(func() { close(releaseFence) }) }
			t.Cleanup(unblockFence)
			manager.afterConvergenceFenceForTest = func(got string) {
				if got != operation {
					return
				}
				close(fenced)
				<-releaseFence
			}

			session := commitSession(t, registry, testTunnelID, testConnectorID)
			controlServer, controlClient := net.Pipe()
			established := establishedSession(t, session, 0x5a)
			serveResult := make(chan error, 1)
			go func() { serveResult <- manager.Serve(context.Background(), controlServer, &established) }()
			readInitialDemand(t, controlClient)
			_ = controlClient.Close()
			select {
			case <-removed:
			case <-time.After(testWait):
				t.Fatal("Serve() did not pause before cleanup")
			}

			operationResult := make(chan error, 1)
			switch operation {
			case "revoke":
				go func() { operationResult <- manager.RevokeTunnel(testTunnelID) }()
			case "shutdown":
				shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), testWait)
				t.Cleanup(cancelShutdown)
				go func() { operationResult <- manager.Shutdown(shutdownContext) }()
			}
			select {
			case <-fenced:
			case <-time.After(testWait):
				t.Fatalf("%s did not establish convergence fence", operation)
			}
			unblockRemove()
			select {
			case <-serveResult:
			case <-time.After(testWait):
				t.Fatal("Serve() did not clean up while convergence paused")
			}
			unblockFence()
			select {
			case err := <-operationResult:
				if err != nil {
					t.Fatalf("%s error = %v", operation, err)
				}
			case <-time.After(testWait):
				t.Fatalf("%s did not finish after fence release", operation)
			}

			wantReason := "server_shutdown"
			if operation == "revoke" {
				wantReason = "tunnel_revoked"
			}
			logs := output.String()
			if !strings.Contains(logs, `"msg":"`+serverruntime.ConnectorEventDisconnected+`"`) ||
				!strings.Contains(logs, `"reason":"`+wantReason+`"`) {
				t.Fatalf("%s lifecycle logs = %s", operation, logs)
			}
			if strings.Contains(logs, `"reason":"control_session_closed"`) {
				t.Fatalf("%s lifecycle reason was overwritten by natural cleanup: %s", operation, logs)
			}
		})
	}
}

func TestConvergenceReasonOverridesOnlyDefaultCleanup(t *testing.T) {
	managed := &managedSession{}
	managed.setTerminationReason("control_session_closed")
	managed.setConvergenceReason("tunnel_revoked")
	if got := managed.termination(); got != "tunnel_revoked" {
		t.Fatalf("default cleanup reason after Revoke fence = %q", got)
	}

	managed = &managedSession{}
	managed.setTerminationReason("heartbeat_timeout")
	managed.setConvergenceReason("server_shutdown")
	if got := managed.termination(); got != "heartbeat_timeout" {
		t.Fatalf("specific pre-convergence reason after Shutdown fence = %q", got)
	}
}

func TestSessionReplacementLogsNewGenerationWithoutOldCleanupPollution(t *testing.T) {
	registry := serverruntime.NewRegistry()
	var output bytes.Buffer
	manager, err := New(registry, Options{
		HighPriorityCapacity: 8, NormalCapacity: 8, InboundCapacity: 8,
		WriteTimeout: testWriteTimeout, MaxReplayEntries: testReplayEntries,
		MaxWorkTotal: 64, MaxWorkConnecting: 16, HeartbeatTimeout: 5 * time.Second,
		SnapshotProvider: testSnapshotProvider{},
		Logger:           slog.New(slog.NewJSONHandler(&output, nil)),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	startTestManager(t, manager)
	oldSession := commitSession(t, registry, testTunnelID, testConnectorID)
	oldServer, oldClient := net.Pipe()
	oldEstablished := establishedSession(t, oldSession, 0x11)
	oldResult := make(chan error, 1)
	go func() { oldResult <- manager.Serve(context.Background(), oldServer, &oldEstablished) }()
	readInitialDemand(t, oldClient)

	newSession := commitSession(t, registry, testTunnelID, testConnectorID)
	newServer, newClient := net.Pipe()
	newEstablished := establishedSession(t, newSession, 0x22)
	newResult := make(chan error, 1)
	go func() { newResult <- manager.Serve(context.Background(), newServer, &newEstablished) }()
	readInitialDemand(t, newClient)
	select {
	case err := <-oldResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("old Serve() error = %v", err)
		}
	case <-time.After(testWait):
		t.Fatal("old Serve() did not stop after replacement")
	}
	snapshots := manager.ConnectorSnapshots()
	if len(snapshots) != 1 || snapshots[0].Session != newSession || snapshots[0].Status != serverruntime.ConnectorStatusOnline {
		t.Fatalf("replacement snapshots = %#v", snapshots)
	}
	if !strings.Contains(output.String(), `"msg":"`+serverruntime.ConnectorEventSessionReplaced+`"`) {
		t.Fatalf("replacement lifecycle log missing: %s", output.String())
	}
	if strings.Contains(output.String(), `"msg":"`+serverruntime.ConnectorEventDisconnected+`"`) {
		t.Fatalf("old generation cleanup emitted a current disconnect event: %s", output.String())
	}
	_ = oldClient.Close()
	_ = newClient.Close()
	select {
	case <-newResult:
	case <-time.After(testWait):
		t.Fatal("new Serve() did not stop")
	}
}

func TestConcurrentReplacementKeepsDisplacedSessionsOwnedUntilCleanup(t *testing.T) {
	registry := serverruntime.NewRegistry()
	manager := newTestManager(t, registry)
	first := commitSession(t, registry, testTunnelID, testConnectorID)
	firstServer, firstClient := net.Pipe()
	firstEstablished := establishedSession(t, first, 0x31)
	firstResult := make(chan error, 1)
	go func() { firstResult <- manager.Serve(context.Background(), firstServer, &firstEstablished) }()
	readInitialDemand(t, firstClient)

	secondInstalled := make(chan struct{})
	releaseSecond := make(chan struct{})
	manager.afterInstallForTest = func(session serverruntime.Session) {
		if session.Generation != 2 {
			return
		}
		close(secondInstalled)
		<-releaseSecond
	}
	second := commitSession(t, registry, testTunnelID, testConnectorID)
	secondServer, secondClient := net.Pipe()
	secondEstablished := establishedSession(t, second, 0x32)
	secondResult := make(chan error, 1)
	go func() { secondResult <- manager.Serve(context.Background(), secondServer, &secondEstablished) }()
	select {
	case <-secondInstalled:
	case <-time.After(testWait):
		t.Fatal("second Serve() did not pause after install")
	}

	third := commitSession(t, registry, testTunnelID, testConnectorID)
	thirdServer, thirdClient := net.Pipe()
	thirdEstablished := establishedSession(t, third, 0x33)
	thirdResult := make(chan error, 1)
	go func() { thirdResult <- manager.Serve(context.Background(), thirdServer, &thirdEstablished) }()
	readInitialDemand(t, thirdClient)
	close(releaseSecond)

	for name, result := range map[string]<-chan error{"first": firstResult, "second": secondResult} {
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("%s Serve() error = %v, want context cancellation", name, err)
			}
		case <-time.After(testWait):
			t.Fatalf("%s displaced Serve() did not stop", name)
		}
	}
	manager.mu.Lock()
	liveCount := len(manager.liveSessions)
	current := manager.byConnector[connectorKey{tunnelID: testTunnelID, connectorID: testConnectorID}]
	manager.mu.Unlock()
	if liveCount != 1 || current == nil || current.session != third {
		t.Fatalf("live Sessions after concurrent replacement = %d, current=%#v, want only third", liveCount, current)
	}

	_ = firstClient.Close()
	_ = secondClient.Close()
	_ = thirdClient.Close()
	select {
	case <-thirdResult:
	case <-time.After(testWait):
		t.Fatal("third Serve() did not stop")
	}
}

func TestRevokeClosesCurrentAndRetiringSessionsDuringInstallTransition(t *testing.T) {
	registry := serverruntime.NewRegistry()
	var output bytes.Buffer
	manager, err := New(registry, Options{
		HighPriorityCapacity: 8, NormalCapacity: 8, InboundCapacity: 8,
		WriteTimeout: testWriteTimeout, MaxReplayEntries: testReplayEntries,
		MaxWorkTotal: 64, MaxWorkConnecting: 16, HeartbeatTimeout: 5 * time.Second,
		SnapshotProvider: testSnapshotProvider{},
		Logger:           slog.New(slog.NewJSONHandler(&output, nil)),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	startTestManager(t, manager)
	first := commitSession(t, registry, testTunnelID, testConnectorID)
	firstServer, firstClient := net.Pipe()
	firstEstablished := establishedSession(t, first, 0x41)
	firstResult := make(chan error, 1)
	go func() { firstResult <- manager.Serve(context.Background(), firstServer, &firstEstablished) }()
	readInitialDemand(t, firstClient)

	secondInstalled := make(chan struct{})
	releaseSecond := make(chan struct{})
	manager.afterInstallForTest = func(session serverruntime.Session) {
		if session.Generation != 2 {
			return
		}
		close(secondInstalled)
		<-releaseSecond
	}
	second := commitSession(t, registry, testTunnelID, testConnectorID)
	secondServer, secondClient := net.Pipe()
	secondEstablished := establishedSession(t, second, 0x42)
	secondResult := make(chan error, 1)
	go func() { secondResult <- manager.Serve(context.Background(), secondServer, &secondEstablished) }()
	select {
	case <-secondInstalled:
	case <-time.After(testWait):
		t.Fatal("second Serve() did not pause after install")
	}

	if err := manager.RevokeTunnel(testTunnelID); err != nil {
		t.Fatalf("RevokeTunnel() error = %v", err)
	}
	close(releaseSecond)
	for name, result := range map[string]<-chan error{"first": firstResult, "second": secondResult} {
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("%s Serve() after revoke error = %v, want context cancellation", name, err)
			}
		case <-time.After(testWait):
			t.Fatalf("%s Serve() did not stop after revoke", name)
		}
	}
	manager.mu.Lock()
	liveCount := len(manager.liveSessions)
	currentCount := len(manager.byConnector)
	resolvableCount := len(manager.bySession)
	manager.mu.Unlock()
	if liveCount != 0 || currentCount != 0 || resolvableCount != 0 {
		t.Fatalf("Manager resources after revoke = live %d current %d resolvable %d", liveCount, currentCount, resolvableCount)
	}
	logs := output.String()
	if strings.Count(logs, `"msg":"`+serverruntime.ConnectorEventDisconnected+`"`) != 1 ||
		!strings.Contains(logs, `"reason":"tunnel_revoked"`) {
		t.Fatalf("revoke lifecycle logs = %s", logs)
	}
	_ = firstClient.Close()
	_ = secondClient.Close()
}

func TestRevokeOwnsCleanupAfterServeRemovesLookup(t *testing.T) {
	registry := serverruntime.NewRegistry()
	var output bytes.Buffer
	manager, err := New(registry, Options{
		HighPriorityCapacity: 8, NormalCapacity: 8, InboundCapacity: 8,
		WriteTimeout: testWriteTimeout, MaxReplayEntries: testReplayEntries,
		MaxWorkTotal: 64, MaxWorkConnecting: 16, HeartbeatTimeout: 5 * time.Second,
		SnapshotProvider: testSnapshotProvider{},
		Logger:           slog.New(slog.NewJSONHandler(&output, nil)),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	startTestManager(t, manager)
	session := commitSession(t, registry, testTunnelID, testConnectorID)
	server, client := net.Pipe()
	established := establishedSession(t, session, 0x51)
	serveResult := make(chan error, 1)
	removed := make(chan struct{})
	releaseCleanup := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCleanup) }) }
	t.Cleanup(release)
	cleanupCalls := make(chan struct{}, 2)
	manager.beforeCleanupForTest = func(serverruntime.Session) { cleanupCalls <- struct{}{} }
	manager.afterRemoveForTest = func(serverruntime.Session) {
		close(removed)
		<-releaseCleanup
	}
	go func() { serveResult <- manager.Serve(context.Background(), server, &established) }()
	readInitialDemand(t, client)
	pool, exists := manager.Pool(session)
	if !exists {
		t.Fatal("Pool() missing before cleanup race")
	}
	idleServer, idleClient := net.Pipe()
	t.Cleanup(func() { _ = idleClient.Close() })
	if _, err := manager.RegisterIdle(idleServer, serverIdleWithWorkID(t, session, "work_01J00000000000000000000051")); err != nil {
		t.Fatalf("RegisterIdle() error = %v", err)
	}
	connectingServer, connectingClient := net.Pipe()
	t.Cleanup(func() { _ = connectingClient.Close() })
	closeStarted := make(chan struct{})
	releaseClose := make(chan struct{})
	var releaseCloseOnce sync.Once
	unblockClose := func() { releaseCloseOnce.Do(func() { close(releaseClose) }) }
	t.Cleanup(unblockClose)
	blockingConnecting := &blockingCloseConn{Conn: connectingServer, started: closeStarted, release: releaseClose}
	if _, err := pool.RegisterConnecting("work_01J00000000000000000000052", blockingConnecting); err != nil {
		t.Fatalf("RegisterConnecting() error = %v", err)
	}
	idleDone := workPeerDone(idleClient)
	connectingDone := workPeerDone(connectingClient)
	_ = client.Close()
	select {
	case <-removed:
	case <-time.After(testWait):
		t.Fatal("Serve() did not pause after removing lookup ownership")
	}
	manager.mu.Lock()
	liveBeforeRevoke := len(manager.liveSessions)
	manager.mu.Unlock()
	if liveBeforeRevoke != 1 {
		t.Fatalf("live Sessions before Revoke = %d, want cleanup ownership retained", liveBeforeRevoke)
	}
	revokeResult := make(chan error, 1)
	go func() { revokeResult <- manager.RevokeTunnel(testTunnelID) }()
	select {
	case <-cleanupCalls:
	case <-time.After(testWait):
		t.Fatal("Revoke did not acquire shared cleanup")
	}
	select {
	case <-closeStarted:
	case <-time.After(testWait):
		t.Fatal("Revoke did not reach the blocked CONNECTING Close")
	}
	shutdownResult := make(chan error, 1)
	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), testWait)
	defer cancelShutdown()
	go func() { shutdownResult <- manager.Shutdown(shutdownContext) }()
	select {
	case <-cleanupCalls:
	case <-time.After(testWait):
		t.Fatal("Shutdown did not join the shared cleanup")
	}
	select {
	case err := <-revokeResult:
		t.Fatalf("Revoke returned before CONNECTING Close completed: %v", err)
	default:
	}
	select {
	case err := <-shutdownResult:
		t.Fatalf("Shutdown returned before shared cleanup completed: %v", err)
	default:
	}
	unblockClose()
	if err := <-revokeResult; err != nil {
		t.Fatalf("RevokeTunnel() error = %v", err)
	}
	if err := <-shutdownResult; err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	for name, done := range map[string]<-chan struct{}{"idle": idleDone, "connecting": connectingDone} {
		select {
		case <-done:
		case <-time.After(testWait):
			t.Fatalf("%s WorkConn remained open after Revoke returned", name)
		}
	}
	counts := pool.Snapshot()
	if !counts.Closed || counts.Total != 0 {
		t.Fatalf("WorkPool after Revoke = %#v, want closed and empty", counts)
	}
	manager.mu.Lock()
	liveAfterRevoke := len(manager.liveSessions)
	manager.mu.Unlock()
	if liveAfterRevoke != 0 {
		t.Fatalf("live Sessions after Revoke = %d, want cleanup complete", liveAfterRevoke)
	}
	release()
	select {
	case <-serveResult:
	case <-time.After(testWait):
		t.Fatal("Serve() did not finish after cleanup release")
	}
	logs := output.String()
	if strings.Count(logs, `"msg":"`+serverruntime.ConnectorEventDisconnected+`"`) != 1 ||
		!strings.Contains(logs, `"reason":"tunnel_revoked"`) {
		t.Fatalf("disconnect/revoke race logs = %s", logs)
	}
}

type blockingCloseConn struct {
	net.Conn
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
	err     error
}

func TestCleanupWaitsForControlOwnerBeforeConvergenceCompletes(t *testing.T) {
	for _, operation := range []string{"revoke", "shutdown", "replacement"} {
		t.Run(operation, func(t *testing.T) {
			registry := serverruntime.NewRegistry()
			manager := newTestManager(t, registry)
			first := commitSession(t, registry, testTunnelID, testConnectorID)
			firstServer, firstClient := net.Pipe()
			closeStarted := make(chan struct{})
			releaseClose := make(chan struct{})
			var releaseOnce sync.Once
			unblockClose := func() { releaseOnce.Do(func() { close(releaseClose) }) }
			t.Cleanup(unblockClose)
			blockingControl := &blockingCloseConn{Conn: firstServer, started: closeStarted, release: releaseClose}
			firstEstablished := establishedSession(t, first, 0x61)
			firstServeResult := make(chan error, 1)
			go func() { firstServeResult <- manager.Serve(context.Background(), blockingControl, &firstEstablished) }()
			readInitialDemand(t, firstClient)

			manager.mu.Lock()
			managed := manager.byConnector[connectorKey{tunnelID: testTunnelID, connectorID: testConnectorID}]
			manager.mu.Unlock()
			if managed == nil {
				t.Fatal("Current managed Session missing before cleanup")
			}

			operationResult := make(chan error, 1)
			var secondClient net.Conn
			var secondServeResult <-chan error
			switch operation {
			case "revoke":
				go func() { operationResult <- manager.RevokeTunnel(testTunnelID) }()
			case "shutdown":
				shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), testWait)
				t.Cleanup(cancelShutdown)
				go func() { operationResult <- manager.Shutdown(shutdownContext) }()
			case "replacement":
				second := commitSession(t, registry, testTunnelID, testConnectorID)
				secondServer, client := net.Pipe()
				secondClient = client
				secondEstablished := establishedSession(t, second, 0x62)
				serveResult := make(chan error, 1)
				secondServeResult = serveResult
				go func() { serveResult <- manager.Serve(context.Background(), secondServer, &secondEstablished) }()
				go func() {
					envelope := &protocolv1.ControlEnvelope{}
					if err := frame.ReadControl(secondClient, envelope); err != nil {
						operationResult <- err
						return
					}
					snapshot := envelope.GetConfigSnapshot()
					if snapshot == nil {
						operationResult <- errors.New("replacement initial Control message is not ConfigSnapshot")
						return
					}
					if err := frame.WriteControl(secondClient, &protocolv1.ControlEnvelope{
						ProtocolVersion: testProtocol,
						Payload: &protocolv1.ControlEnvelope_ConfigAck{ConfigAck: &protocolv1.ConfigAck{
							ObservedRevision: snapshot.GetRevision(),
							ApplyStatus:      protocolv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_APPLIED,
							ErrorCode:        protocolv1.ErrorCode_ERROR_CODE_OK,
						}},
					}); err != nil {
						operationResult <- err
						return
					}
					envelope.Reset()
					if err := frame.ReadControl(secondClient, envelope); err != nil {
						operationResult <- err
						return
					}
					if envelope.GetWorkDemand() == nil {
						operationResult <- errors.New("replacement post-Ack Control message is not WorkDemand")
						return
					}
					operationResult <- nil
				}()
			}

			select {
			case <-closeStarted:
			case <-time.After(testWait):
				t.Fatal("cleanup did not reach blocked Control Owner Close")
			}
			select {
			case err := <-operationResult:
				t.Fatalf("%s completed before Control Owner Close: %v", operation, err)
			default:
			}
			select {
			case <-managed.owner.Done():
				t.Fatal("Control Owner Done closed before blocked socket Close completed")
			default:
			}
			manager.mu.Lock()
			_, stillLive := manager.liveSessions[managed]
			manager.mu.Unlock()
			if !stillLive {
				t.Fatal("live Session deleted before Control Owner cleanup completed")
			}

			unblockClose()
			select {
			case err := <-operationResult:
				if err != nil {
					t.Fatalf("%s completion error = %v", operation, err)
				}
			case <-time.After(testWait):
				t.Fatalf("%s did not complete after Control Owner Close", operation)
			}
			select {
			case <-managed.owner.Done():
			default:
				t.Fatal("Control Owner Done remained open after cleanup returned")
			}
			manager.mu.Lock()
			_, stillLive = manager.liveSessions[managed]
			manager.mu.Unlock()
			if stillLive {
				t.Fatal("old live Session remained after cleanup returned")
			}
			select {
			case <-firstServeResult:
			case <-time.After(testWait):
				t.Fatal("old Serve did not finish after Control Owner cleanup")
			}
			_ = firstClient.Close()
			if secondClient != nil {
				_ = secondClient.Close()
				select {
				case <-secondServeResult:
				case <-time.After(testWait):
					t.Fatal("replacement Serve did not finish")
				}
			}
		})
	}
}

func (connection *blockingCloseConn) Close() error {
	connection.once.Do(func() {
		close(connection.started)
		<-connection.release
		connection.err = connection.Conn.Close()
	})
	return connection.err
}

func TestInitialDemandFailureLogsPairedDisconnect(t *testing.T) {
	tests := []struct {
		name      string
		wantError string
		fail      func(*managedSession)
	}{
		{
			name: "grant lease", wantError: "grant WorkDemand lease",
			fail: func(managed *managedSession) { managed.authenticator.Close() },
		},
		{
			name: "owner enqueue", wantError: "enqueue WorkDemand",
			fail: func(managed *managedSession) {
				managed.cancel()
				<-managed.owner.Done()
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := serverruntime.NewRegistry()
			var output bytes.Buffer
			manager, err := New(registry, Options{
				HighPriorityCapacity: 8, NormalCapacity: 8, InboundCapacity: 8,
				WriteTimeout: testWriteTimeout, MaxReplayEntries: testReplayEntries,
				MaxWorkTotal: 64, MaxWorkConnecting: 16, HeartbeatTimeout: 5 * time.Second,
				SnapshotProvider: testSnapshotProvider{},
				Logger:           slog.New(slog.NewJSONHandler(&output, nil)),
			})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			startTestManager(t, manager)
			manager.beforeInitialDemandForTest = test.fail
			session := commitSession(t, registry, testTunnelID, testConnectorID)
			server, client := net.Pipe()
			t.Cleanup(func() { _ = client.Close() })
			established := establishedSession(t, session, 0x71)

			serveResult := make(chan error, 1)
			go func() { serveResult <- manager.Serve(context.Background(), server, &established) }()
			snapshot := readConfigSnapshot(t, client)
			writeConfigAck(t, client, appliedConfigAck(snapshot.GetRevision()))
			select {
			case err = <-serveResult:
			case <-time.After(testWait):
				t.Fatal("Serve() did not report initial Demand failure")
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("Serve() error = %v, want %q", err, test.wantError)
			}
			logs := output.String()
			if strings.Count(logs, `"msg":"`+serverruntime.ConnectorEventConnected+`"`) != 1 ||
				strings.Count(logs, `"msg":"`+serverruntime.ConnectorEventDisconnected+`"`) != 1 ||
				!strings.Contains(logs, `"reason":"`+initialDemandFailedReason+`"`) {
				t.Fatalf("initial Demand failure lifecycle logs = %s", logs)
			}
			manager.mu.Lock()
			liveCount := len(manager.liveSessions)
			lookupCount := len(manager.bySession)
			manager.mu.Unlock()
			if liveCount != 0 || lookupCount != 0 {
				t.Fatalf("Manager resources after initial Demand failure = live %d lookup %d", liveCount, lookupCount)
			}
		})
	}
}

func TestInboundPanicIsReportedAndSessionIsCleanedUp(t *testing.T) {
	registry := serverruntime.NewRegistry()
	manager, err := New(registry, Options{
		HighPriorityCapacity: 8, NormalCapacity: 8, InboundCapacity: 8,
		WriteTimeout: testWriteTimeout, MaxReplayEntries: testReplayEntries,
		MaxWorkTotal: 64, MaxWorkConnecting: 16, HeartbeatTimeout: 5 * time.Second,
		SnapshotProvider: testSnapshotProvider{},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	startTestManager(t, manager)
	manager.beforeInitialDemandForTest = func(*managedSession) {
		panic("sensitive panic value")
	}
	session := commitSession(t, registry, testTunnelID, testConnectorID)
	server, client := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	established := establishedSession(t, session, 0x71)

	serveResult := make(chan error, 1)
	go func() { serveResult <- manager.Serve(context.Background(), server, &established) }()
	snapshot := readConfigSnapshot(t, client)
	writeConfigAck(t, client, appliedConfigAck(snapshot.GetRevision()))

	select {
	case err = <-serveResult:
	case <-time.After(testWait):
		t.Fatal("Serve() did not report inbound panic")
	}
	if !errors.Is(err, safego.ErrPanic) {
		t.Fatalf("Serve() error = %v, want safego.ErrPanic", err)
	}
	if strings.Contains(err.Error(), "sensitive panic value") {
		t.Fatalf("Serve() error exposed panic value: %v", err)
	}
	manager.mu.Lock()
	liveCount := len(manager.liveSessions)
	lookupCount := len(manager.bySession)
	manager.mu.Unlock()
	if liveCount != 0 || lookupCount != 0 {
		t.Fatalf("Manager resources after inbound panic = live %d lookup %d", liveCount, lookupCount)
	}
}

func TestShutdownUsesExplicitLifecycleReason(t *testing.T) {
	registry := serverruntime.NewRegistry()
	var output bytes.Buffer
	manager, err := New(registry, Options{
		HighPriorityCapacity: 8, NormalCapacity: 8, InboundCapacity: 8,
		WriteTimeout: testWriteTimeout, MaxReplayEntries: testReplayEntries,
		MaxWorkTotal: 64, MaxWorkConnecting: 16, HeartbeatTimeout: 5 * time.Second,
		SnapshotProvider: testSnapshotProvider{},
		Logger:           slog.New(slog.NewJSONHandler(&output, nil)),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	startTestManager(t, manager)
	session := commitSession(t, registry, testTunnelID, testConnectorID)
	server, client := net.Pipe()
	established := establishedSession(t, session, 0x61)
	serveResult := make(chan error, 1)
	go func() { serveResult <- manager.Serve(context.Background(), server, &established) }()
	readInitialDemand(t, client)

	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), testWait)
	defer cancelShutdown()
	if err := manager.Shutdown(shutdownContext); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	select {
	case <-serveResult:
	case <-time.After(testWait):
		t.Fatal("Serve() did not stop after Shutdown")
	}
	if !strings.Contains(output.String(), `"msg":"`+serverruntime.ConnectorEventDisconnected+`"`) ||
		!strings.Contains(output.String(), `"reason":"server_shutdown"`) {
		t.Fatalf("shutdown lifecycle logs = %s", output.String())
	}
	_ = client.Close()
}

func TestMetricsSnapshotCountsTombstoneActiveUntilFinalCleanup(t *testing.T) {
	manager, _, _, _, active, _ := activeShutdownFixture(t)
	waitFor(t, func() bool {
		metrics := manager.MetricsSnapshot()
		return metrics.XTunnelActiveConnections == 1 && metrics.XTunnelTCPActiveWorkConnections == 1
	})
	if err := active.Finish(); err != nil {
		t.Fatalf("Finish() error = %v", err)
	}
	metrics := manager.MetricsSnapshot()
	if metrics.XTunnelActiveConnections != 0 || metrics.XTunnelTCPActiveWorkConnections != 0 {
		t.Fatalf("MetricsSnapshot() after Finish = %#v", metrics)
	}
}
