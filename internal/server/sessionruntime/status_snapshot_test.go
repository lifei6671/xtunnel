package sessionruntime

import (
	"net"
	"testing"
	"time"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
	serverstatus "github.com/lifei6671/xtunnel/internal/server/status"
	serverworkpool "github.com/lifei6671/xtunnel/internal/server/workpool"
)

func TestRuntimeStatusSnapshotsFenceReplacementAndOwnReturnedState(t *testing.T) {
	registry := serverruntime.NewRegistry()
	oldSession := commitSession(t, registry, testTunnelID, testConnectorID)
	oldManaged := healthReadySession(7, testHealthRequirement(7, time.Second))
	oldManaged.session = oldSession
	oldManaged.metadata = serverruntime.ConnectorMetadata{Hostname: "old"}
	oldManaged.lastHeartbeatAt = 9 * time.Second
	oldManaged.serviceHealth[testServiceID] = serviceHealthState{
		status: protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY, serviceRevision: 7, receivedAt: 9 * time.Second,
	}
	oldManaged.pool = statusTestPool(t, oldSession)
	if _, observed := registry.ObserveConnected(oldSession, oldManaged.metadata); !observed {
		t.Fatal("ObserveConnected(old) = false")
	}
	manager := &Manager{
		registry: registry, startedAt: time.Now().Add(-10 * time.Second),
		options: Options{HeartbeatTimeout: 5 * time.Second},
		byConnector: map[connectorKey]*managedSession{
			{tunnelID: testTunnelID, connectorID: testConnectorID}: oldManaged,
		},
	}
	if !registry.PublishEligibility(oldSession, oldManaged.eligibilitySnapshot(manager.startedAt)) {
		t.Fatal("PublishEligibility(old) = false")
	}

	snapshots := manager.RuntimeStatusSnapshots()
	if len(snapshots) != 1 {
		t.Fatalf("RuntimeStatusSnapshots() len = %d, want 1", len(snapshots))
	}
	old := snapshots[0]
	service := old.Config.Services[testServiceID]
	if old.Session != oldSession || old.Hostname != "old" || !old.CurrentControlSession ||
		!old.HeartbeatFresh || !old.Config.ConfigReady || old.Config.ObservedRevision != 7 ||
		!service.HealthHealthy || service.HealthRevision != 7 || service.HealthyUntil.IsZero() {
		t.Fatalf("old Runtime status snapshot = %#v", old)
	}
	delete(old.Config.Services, testServiceID)
	if len(manager.RuntimeStatusSnapshots()[0].Config.Services) != 1 {
		t.Fatal("caller mutation changed Manager-owned Service status")
	}

	replacement := commitSession(t, registry, testTunnelID, testConnectorID)
	replacementManaged := &managedSession{
		session: replacement, metadata: serverruntime.ConnectorMetadata{Hostname: "new"},
		lastHeartbeatAt: 0, pool: statusTestPool(t, replacement),
	}
	manager.mu.Lock()
	manager.byConnector[connectorKey{tunnelID: testTunnelID, connectorID: testConnectorID}] = replacementManaged
	manager.mu.Unlock()

	snapshots = manager.RuntimeStatusSnapshots()
	if len(snapshots) != 1 || snapshots[0].Session != replacement || snapshots[0].Hostname != "new" {
		t.Fatalf("replacement Runtime status snapshots = %#v", snapshots)
	}
	if snapshots[0].Config.ConfigReady || snapshots[0].HeartbeatFresh || len(snapshots[0].Config.Services) != 0 {
		t.Fatalf("replacement inherited old Config/Health/Heartbeat: %#v", snapshots[0])
	}
}

func TestRuntimeStatusSnapshotsUseOnlyPublishedEligibility(t *testing.T) {
	registry := serverruntime.NewRegistry()
	session := commitSession(t, registry, testTunnelID, testConnectorID)
	managed := healthReadySession(7, testHealthRequirement(7, time.Second))
	managed.session = session
	managed.lastHeartbeatAt = time.Second
	managed.serviceHealth[testServiceID] = serviceHealthState{
		status:          protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY,
		serviceRevision: 7, receivedAt: time.Second,
	}
	managed.pool = statusTestPool(t, session)
	if _, observed := registry.ObserveConnected(session, serverruntime.ConnectorMetadata{}); !observed {
		t.Fatal("ObserveConnected() = false")
	}
	manager := &Manager{
		registry: registry, startedAt: time.Now().Add(-2 * time.Second),
		options: Options{HeartbeatTimeout: 5 * time.Second},
		byConnector: map[connectorKey]*managedSession{
			{tunnelID: testTunnelID, connectorID: testConnectorID}: managed,
		},
	}

	unpublished := manager.RuntimeStatusSnapshots()
	if len(unpublished) != 1 || unpublished[0].Config.ConfigReady || len(unpublished[0].Config.Services) != 0 {
		t.Fatalf("unpublished eligibility leaked into status: %#v", unpublished)
	}
	if !registry.PublishEligibility(session, managed.eligibilitySnapshot(manager.startedAt)) {
		t.Fatal("PublishEligibility() = false")
	}
	published := manager.RuntimeStatusSnapshots()
	if len(published) != 1 || !published[0].Config.ConfigReady || len(published[0].Config.Services) != 1 {
		t.Fatalf("published eligibility missing from status: %#v", published)
	}
}

func TestRuntimeStatusSnapshotsUseLifecycleDrainBeforePoolDrain(t *testing.T) {
	registry := serverruntime.NewRegistry()
	session := commitSession(t, registry, testTunnelID, testConnectorID)
	managed := healthReadySession(7, testHealthRequirement(7, time.Second))
	managed.session = session
	managed.lastHeartbeatAt = time.Second
	managed.serviceHealth[testServiceID] = serviceHealthState{
		status:          protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY,
		serviceRevision: 7, receivedAt: time.Second,
	}
	managed.pool = statusTestPool(t, session)
	if _, observed := registry.ObserveConnected(session, serverruntime.ConnectorMetadata{}); !observed {
		t.Fatal("ObserveConnected() = false")
	}
	manager := &Manager{
		registry: registry, startedAt: time.Now().Add(-2 * time.Second),
		options: Options{HeartbeatTimeout: 5 * time.Second},
		byConnector: map[connectorKey]*managedSession{
			{tunnelID: testTunnelID, connectorID: testConnectorID}: managed,
		},
	}
	if !registry.PublishEligibility(session, managed.eligibilitySnapshot(manager.startedAt)) {
		t.Fatal("PublishEligibility() = false")
	}
	manager.beforeStatusFenceForTest = func(serverruntime.Session) {
		manager.beforeStatusFenceForTest = nil
		if _, changed := registry.ObserveDraining(session); !changed {
			t.Fatal("ObserveDraining() = false")
		}
	}

	snapshots := manager.RuntimeStatusSnapshots()
	if len(snapshots) != 1 || snapshots[0].LifecycleStatus != serverruntime.ConnectorStatusDraining ||
		snapshots[0].WorkPool.Draining || !snapshots[0].Config.ConfigReady {
		t.Fatalf("lifecycle/pool drain window snapshot = %#v", snapshots)
	}
	connector := serverstatus.ServiceConnectorFromRuntime(snapshots[0], testServiceID, 7, time.Now())
	if got := serverstatus.CalculateService(serverstatus.ServiceInput{
		Enabled: true, RequiredRevision: 7, HealthEnabled: true,
		Connectors: []serverstatus.ServiceConnector{connector},
	}); got != serverstatus.ServiceStatusNoCapacity {
		t.Fatalf("CalculateService(real drain chain) = %q, want %q", got, serverstatus.ServiceStatusNoCapacity)
	}
}

func TestRuntimeStatusSnapshotsExposeDrainAndPoolCounts(t *testing.T) {
	registry := serverruntime.NewRegistry()
	session := commitSession(t, registry, testTunnelID, testConnectorID)
	managed := healthReadySession(7, testHealthRequirement(7, time.Second))
	managed.session = session
	managed.lastHeartbeatAt = time.Second
	managed.pool = statusTestPool(t, session)
	workServer, workClient := net.Pipe()
	defer workClient.Close()
	if _, err := managed.pool.RegisterConnecting("work_01J00000000000000000000000", workServer); err != nil {
		t.Fatalf("RegisterConnecting() error = %v", err)
	}
	if _, observed := registry.ObserveConnected(session, serverruntime.ConnectorMetadata{}); !observed {
		t.Fatal("ObserveConnected() = false")
	}
	manager := &Manager{
		registry: registry, startedAt: time.Now().Add(-2 * time.Second),
		options: Options{HeartbeatTimeout: 5 * time.Second},
		byConnector: map[connectorKey]*managedSession{
			{tunnelID: testTunnelID, connectorID: testConnectorID}: managed,
		},
	}
	beforeDrain := manager.RuntimeStatusSnapshots()
	if len(beforeDrain) != 1 || beforeDrain[0].WorkPool.Connecting != 1 || beforeDrain[0].WorkPool.Total != 1 {
		t.Fatalf("connecting Runtime status snapshot = %#v", beforeDrain)
	}
	if err := managed.pool.BeginDrain(); err != nil {
		t.Fatalf("BeginDrain() error = %v", err)
	}
	snapshots := manager.RuntimeStatusSnapshots()
	if len(snapshots) != 1 || !snapshots[0].WorkPool.Draining ||
		snapshots[0].WorkPool.Connecting != 1 || snapshots[0].WorkPool.Total != 1 {
		t.Fatalf("draining Runtime status snapshot = %#v", snapshots)
	}
}

func TestRuntimeStatusSnapshotsFinalCurrentFenceRejectsReplacementAndRevoke(t *testing.T) {
	for _, operation := range []string{"replacement", "revoke"} {
		t.Run(operation, func(t *testing.T) {
			registry := serverruntime.NewRegistry()
			session := commitSession(t, registry, testTunnelID, testConnectorID)
			managed := healthReadySession(7, testHealthRequirement(7, time.Second))
			managed.session = session
			managed.lastHeartbeatAt = time.Second
			managed.pool = statusTestPool(t, session)
			if _, observed := registry.ObserveConnected(session, serverruntime.ConnectorMetadata{}); !observed {
				t.Fatal("ObserveConnected() = false")
			}
			manager := &Manager{
				registry: registry, startedAt: time.Now().Add(-2 * time.Second),
				options: Options{HeartbeatTimeout: 5 * time.Second},
				byConnector: map[connectorKey]*managedSession{
					{tunnelID: testTunnelID, connectorID: testConnectorID}: managed,
				},
			}
			manager.beforeStatusFenceForTest = func(serverruntime.Session) {
				manager.beforeStatusFenceForTest = nil
				switch operation {
				case "replacement":
					commitSession(t, registry, testTunnelID, testConnectorID)
				case "revoke":
					if err := registry.RevokeTunnel(testTunnelID); err != nil {
						t.Fatalf("RevokeTunnel() error = %v", err)
					}
				}
			}
			if snapshots := manager.RuntimeStatusSnapshots(); len(snapshots) != 0 {
				t.Fatalf("RuntimeStatusSnapshots() after %s fence = %#v, want none", operation, snapshots)
			}
		})
	}
}

func statusTestPool(t *testing.T, session serverruntime.Session) *serverworkpool.Pool {
	t.Helper()
	pool, err := serverworkpool.New(serverworkpool.Options{
		Session: serverworkpool.Session{
			TunnelID: session.TunnelID, ConnectorID: session.ConnectorID,
			SessionID: session.SessionID, Generation: session.Generation,
		},
		MaxTotal: 8, MaxConnecting: 4,
		Clock: func() time.Duration { return 0 }, DeadlineNow: time.Now,
	})
	if err != nil {
		t.Fatalf("workpool.New() error = %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}
