package runtime

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestConnectorLifecycleGenerationFencingAndSortedSnapshots(t *testing.T) {
	now := time.Date(2026, 8, 26, 9, 0, 0, 0, time.FixedZone("test", 8*60*60))
	registry := newRegistryWithClock(sessionGenerator(1), func() time.Time { return now })
	metadata := ConnectorMetadata{Hostname: "edge-a", OS: "linux", Arch: "arm64", Version: "0.1.0"}
	oldSession, err := installAuthenticated(registry, runtimeTunnelIDTwo, runtimeConnectorIDTwo)
	if err != nil {
		t.Fatalf("installAuthenticated(old) error = %v", err)
	}
	event, observed := registry.ObserveConnected(oldSession, metadata)
	if !observed || event.Name != ConnectorEventConnected || event.Snapshot.Status != ConnectorStatusOnline {
		t.Fatalf("ObserveConnected(old) = %#v, %v", event, observed)
	}
	if event.Snapshot.ConnectedAt.Location() != time.UTC || event.Snapshot.LastHeartbeatAt != now.UTC() {
		t.Fatalf("initial timestamps = %v/%v, want Server UTC %v", event.Snapshot.ConnectedAt, event.Snapshot.LastHeartbeatAt, now.UTC())
	}

	now = now.Add(time.Second)
	if !registry.ObserveHeartbeat(oldSession) {
		t.Fatal("ObserveHeartbeat(old current) = false")
	}
	newSession, err := installAuthenticated(registry, runtimeTunnelIDTwo, runtimeConnectorIDTwo)
	if err != nil {
		t.Fatalf("installAuthenticated(replacement) error = %v", err)
	}
	now = now.Add(time.Second)
	event, observed = registry.ObserveConnected(newSession, metadata)
	if !observed || event.Name != ConnectorEventSessionReplaced || event.Snapshot.Generation != 2 {
		t.Fatalf("ObserveConnected(replacement) = %#v, %v", event, observed)
	}
	if event.Snapshot.ConnectedAt != now.Add(-2*time.Second).UTC() {
		t.Fatalf("replacement connected_at = %v, want original process observation", event.Snapshot.ConnectedAt)
	}
	if registry.ObserveHeartbeat(oldSession) {
		t.Fatal("old generation Heartbeat changed replacement")
	}
	if _, changed := registry.ObserveDraining(oldSession); changed {
		t.Fatal("old generation Drain changed replacement")
	}
	drainEvent, changed := registry.ObserveDraining(newSession)
	if !changed || drainEvent.Name != ConnectorEventDraining || drainEvent.Snapshot.Status != ConnectorStatusDraining {
		t.Fatalf("ObserveDraining(current) = %#v, %v", drainEvent, changed)
	}

	firstSession, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatalf("installAuthenticated(first sorted) error = %v", err)
	}
	if _, observed := registry.ObserveConnected(firstSession, ConnectorMetadata{Hostname: "edge-b"}); !observed {
		t.Fatal("ObserveConnected(first sorted) = false")
	}
	snapshots := registry.ConnectorSnapshots()
	if len(snapshots) != 2 || snapshots[0].TunnelID != runtimeTunnelID || snapshots[1].TunnelID != runtimeTunnelIDTwo {
		t.Fatalf("ConnectorSnapshots() order = %#v", snapshots)
	}
}

func TestConnectorLifecycleDisconnectUsesActiveWorkTombstoneUntilFinish(t *testing.T) {
	fixture := newActiveWorkFixture(t, runtimeTunnelID, runtimeConnectorID)
	metadata := ConnectorMetadata{Hostname: "edge-a", OS: "linux", Arch: "amd64", Version: "0.1.0"}
	if _, observed := fixture.registry.ObserveConnected(fixture.session, metadata); !observed {
		t.Fatal("ObserveConnected() = false")
	}
	work, err := fixture.tunnel.RegisterActiveWork(fixture.spec(runtimeWorkID, runtimeConnectionID))
	if err != nil {
		t.Fatalf("RegisterActiveWork() error = %v", err)
	}
	event, cleared := fixture.registry.DisconnectIfCurrent(fixture.session, "control_session_closed")
	if !cleared || event.Name != ConnectorEventDisconnected || !event.Snapshot.Tombstone ||
		event.Snapshot.ActiveWork != 1 || !event.TunnelBecameOffline {
		t.Fatalf("DisconnectIfCurrent() = %#v, %v", event, cleared)
	}
	snapshots := fixture.registry.ConnectorSnapshots()
	if len(snapshots) != 1 || !snapshots[0].Tombstone || snapshots[0].Status != "" || snapshots[0].ActiveWork != 1 {
		t.Fatalf("Tombstone snapshots = %#v", snapshots)
	}
	if _, err := fixture.registry.AcquireConnector(runtimeTunnelID); !errors.Is(err, ErrNoAvailableConnector) {
		t.Fatalf("AcquireConnector(tombstone) error = %v", err)
	}
	if err := work.Finish(); err != nil {
		t.Fatalf("Finish() error = %v", err)
	}
	if snapshots := fixture.registry.ConnectorSnapshots(); len(snapshots) != 0 {
		t.Fatalf("ConnectorSnapshots() after final Active = %#v", snapshots)
	}
}

func TestConnectorLifecycleDisconnectFreezesDrainingBeforeActiveWorkTombstone(t *testing.T) {
	fixture := newActiveWorkFixture(t, runtimeTunnelID, runtimeConnectorID)
	if _, observed := fixture.registry.ObserveConnected(fixture.session, ConnectorMetadata{Hostname: "edge-a"}); !observed {
		t.Fatal("ObserveConnected() = false")
	}
	work, err := fixture.tunnel.RegisterActiveWork(fixture.spec(runtimeWorkID, runtimeConnectionID))
	if err != nil {
		t.Fatalf("RegisterActiveWork() error = %v", err)
	}
	if _, changed := fixture.registry.ObserveDraining(fixture.session); !changed {
		t.Fatal("ObserveDraining() = false")
	}
	event, disconnected := fixture.registry.DisconnectIfCurrent(fixture.session, "control_session_closed")
	if !disconnected || !event.WasDraining || !event.Snapshot.Tombstone ||
		event.Snapshot.Status != "" || event.Snapshot.ActiveWork != 1 {
		t.Fatalf("DisconnectIfCurrent() = %#v/%t, want frozen Draining Active Work Tombstone", event, disconnected)
	}
	if err := work.Finish(); err != nil {
		t.Fatalf("Finish() error = %v", err)
	}
}

func TestDisconnectEventFreezesLastCurrentConnectorTransition(t *testing.T) {
	registry := newRegistry(sessionGenerator(1))
	first, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorIDTwo)
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range []Session{first, second} {
		if _, observed := registry.ObserveConnected(session, ConnectorMetadata{}); !observed {
			t.Fatalf("ObserveConnected(%s) = false", session.ConnectorID)
		}
	}
	firstEvent, disconnected := registry.DisconnectIfCurrent(first, "control_session_closed")
	if !disconnected || firstEvent.Name != ConnectorEventDisconnected || firstEvent.TunnelBecameOffline {
		t.Fatalf("first DisconnectIfCurrent() = %#v/%t, want Tunnel still online", firstEvent, disconnected)
	}
	secondEvent, disconnected := registry.DisconnectIfCurrent(second, "control_session_closed")
	if !disconnected || secondEvent.Name != ConnectorEventDisconnected || !secondEvent.TunnelBecameOffline {
		t.Fatalf("second DisconnectIfCurrent() = %#v/%t, want frozen Tunnel offline edge", secondEvent, disconnected)
	}
}

func TestCurrentConnectorSnapshotRejectsReplacementAndTombstone(t *testing.T) {
	registry := newRegistry(sessionGenerator(1))
	oldSession, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatal(err)
	}
	if _, observed := registry.ObserveConnected(oldSession, ConnectorMetadata{Hostname: "old"}); !observed {
		t.Fatal("ObserveConnected(old) = false")
	}
	replacement, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, current, _ := registry.CurrentConnectorSnapshot(oldSession); current {
		t.Fatal("CurrentConnectorSnapshot() exposed replaced generation")
	}
	if _, _, current, observed := registry.CurrentConnectorSnapshot(replacement); !current || observed {
		t.Fatalf("CurrentConnectorSnapshot() before observation = current %t observed %t", current, observed)
	}
	if _, observed := registry.ObserveConnected(replacement, ConnectorMetadata{Hostname: "new"}); !observed {
		t.Fatal("ObserveConnected(replacement) = false")
	}
	snapshot, _, current, observed := registry.CurrentConnectorSnapshot(replacement)
	if !current || !observed || snapshot.Session != replacement || snapshot.Hostname != "new" || snapshot.Tombstone {
		t.Fatalf("CurrentConnectorSnapshot(replacement) = %#v, %t, %t", snapshot, current, observed)
	}

	tunnel, err := registry.Tunnel(runtimeTunnelID)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := registry.AcquireConnector(runtimeTunnelID)
	if err != nil {
		t.Fatal(err)
	}
	_, cancel := context.WithCancel(context.Background())
	workConn, workPeer := net.Pipe()
	defer workPeer.Close()
	peerConn, peerClient := net.Pipe()
	defer peerClient.Close()
	work, err := tunnel.RegisterActiveWork(ActiveWorkSpec{
		Session: replacement, WorkID: runtimeWorkID, ConnectionID: runtimeConnectionID,
		Cancel: cancel, WorkConn: workConn, PeerConn: peerConn, Lease: lease,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, disconnected := registry.DisconnectIfCurrent(replacement, "test"); !disconnected {
		t.Fatal("DisconnectIfCurrent() = false")
	}
	if _, _, current, _ := registry.CurrentConnectorSnapshot(replacement); current {
		t.Fatal("CurrentConnectorSnapshot() exposed ActiveWork Tombstone")
	}
	if err := work.Finish(); err != nil {
		t.Fatal(err)
	}
}

func TestRevokeTunnelDoesNotRepeatDisconnectForActiveTombstone(t *testing.T) {
	fixture := newActiveWorkFixture(t, runtimeTunnelID, runtimeConnectorID)
	if _, observed := fixture.registry.ObserveConnected(fixture.session, ConnectorMetadata{Hostname: "edge-a"}); !observed {
		t.Fatal("ObserveConnected() = false")
	}
	work, err := fixture.tunnel.RegisterActiveWork(fixture.spec(runtimeWorkID, runtimeConnectionID))
	if err != nil {
		t.Fatalf("RegisterActiveWork() error = %v", err)
	}
	first, disconnected := fixture.registry.DisconnectIfCurrent(fixture.session, "control_session_closed")
	if !disconnected || first.Name != ConnectorEventDisconnected || !first.Snapshot.Tombstone {
		t.Fatalf("DisconnectIfCurrent() = %#v, %v", first, disconnected)
	}
	revokeEvents, err := fixture.registry.RevokeTunnelWithLifecycle(runtimeTunnelID)
	if err != nil {
		t.Fatalf("RevokeTunnelWithLifecycle() error = %v", err)
	}
	if len(revokeEvents) != 0 {
		t.Fatalf("Revoke repeated an already emitted Tombstone disconnect: %#v", revokeEvents)
	}
	if err := work.Finish(); err != nil {
		t.Fatalf("Finish(after revoke) error = %v", err)
	}
	if snapshots := fixture.registry.ConnectorSnapshots(); len(snapshots) != 0 {
		t.Fatalf("ConnectorSnapshots() after revoke = %#v", snapshots)
	}
}

func TestRevokeTunnelClearsSessionMapsAndLateInstallResources(t *testing.T) {
	registry := newRegistry(sessionGenerator(1))
	oldSession, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatalf("installAuthenticated(old) error = %v", err)
	}
	lease, err := registry.AcquireConnector(runtimeTunnelID)
	if err != nil {
		t.Fatalf("AcquireConnector(old) error = %v", err)
	}
	if _, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID); err != nil {
		t.Fatalf("installAuthenticated(replacement) error = %v", err)
	}
	pending, err := registry.ReserveAuthenticated(runtimeTunnelID, runtimeConnectorIDTwo)
	if err != nil {
		t.Fatalf("ReserveAuthenticated(pending) error = %v", err)
	}
	if err := registry.RevokeTunnel(runtimeTunnelID); err != nil {
		t.Fatalf("RevokeTunnel() error = %v", err)
	}
	runtime := registry.tunnel(runtimeTunnelID, false)
	runtime.mu.Lock()
	if len(runtime.current) != 0 || len(runtime.pending) != 0 || len(runtime.currentConnectorLimits) != 0 ||
		len(runtime.pendingConnectorLimits) != 0 || len(runtime.connectors) != 0 || !runtime.revoked {
		runtime.mu.Unlock()
		t.Fatalf("revoked Runtime retained selectable/session resources")
	}
	runtime.mu.Unlock()
	if _, err := registry.InstallAuthenticated(pending); !errors.Is(err, ErrTunnelRuntimeRevoked) {
		t.Fatalf("InstallAuthenticated(revoked pending) error = %v, want ErrTunnelRuntimeRevoked", err)
	}
	if registry.CancelAuthenticated(pending) {
		t.Fatal("CancelAuthenticated(revoked pending) released twice")
	}
	if _, err := registry.ReserveAuthenticated(runtimeTunnelID, runtimeConnectorIDTwo); !errors.Is(err, ErrTunnelRuntimeRevoked) {
		t.Fatalf("ReserveAuthenticated(after revoke) error = %v", err)
	}
	if !lease.Release() {
		t.Fatal("old generation Lease did not retain exactly-once ownership through revoke")
	}
	registry.sessionIDsMu.Lock()
	remainingSessionIDs := len(registry.sessionIDs)
	registry.sessionIDsMu.Unlock()
	if remainingSessionIDs != 0 {
		t.Fatalf("session ID reservations after final release = %d, want 0 (old=%s)", remainingSessionIDs, oldSession.SessionID)
	}
}
