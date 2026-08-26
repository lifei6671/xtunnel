package limits

import (
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
)

const (
	testTunnelID  = "tun_01J00000000000000000000000"
	testServiceID = "svc_01J00000000000000000000000"
)

var testSourceIP = netip.MustParseAddr("192.0.2.10")

func TestConnectorLeaseCountsIdentityOnceAcrossReplacement(t *testing.T) {
	manager := newTestManager(t, Options{
		MaxConnectors: 2, MaxConnectorsPerTunnel: 1,
		MaxWorkConnections: 1, MaxIdleWorkConnections: 1, MaxConnectingWorkConnections: 1,
		MaxPendingOpens: 1, MaxActiveConnections: 1, MaxConnectionsPerTunnel: 1,
		MaxConnectionsPerService: 1, MaxConnectionsPerSourceIP: 1,
	})
	first, err := manager.AcquireConnector(testTunnelID, "con_01J00000000000000000000000")
	if err != nil {
		t.Fatalf("AcquireConnector(first generation) error = %v", err)
	}
	replacement, err := manager.AcquireConnector(testTunnelID, "con_01J00000000000000000000000")
	if err != nil {
		t.Fatalf("AcquireConnector(replacement) error = %v", err)
	}
	if _, err := manager.AcquireConnector(testTunnelID, "con_01J00000000000000000000001"); !errors.Is(err, ErrConnectorCapacity) {
		t.Fatalf("AcquireConnector(second identity) error = %v, want ErrConnectorCapacity", err)
	}
	if got := manager.Snapshot(); got.Connectors != 1 || got.ConnectorsByTunnel[testTunnelID] != 1 {
		t.Fatalf("Snapshot() during replacement = %#v", got)
	}
	first.Release()
	if got := manager.Snapshot().Connectors; got != 1 {
		t.Fatalf("Connectors after old generation release = %d, want 1", got)
	}
	replacement.Release()
	replacement.Release()
	assertEmpty(t, manager.Snapshot())
}

func TestWorkLeaseTracksEveryLifecycleStateAndReleasesExactlyOnce(t *testing.T) {
	manager := newTestManager(t, Options{
		MaxConnectors: 2, MaxConnectorsPerTunnel: 2,
		MaxWorkConnections: 2, MaxIdleWorkConnections: 1, MaxConnectingWorkConnections: 1,
		MaxPendingOpens: 2, MaxActiveConnections: 2, MaxConnectionsPerTunnel: 2,
		MaxConnectionsPerService: 2, MaxConnectionsPerSourceIP: 2,
	})
	first, err := manager.AcquireWork()
	if err != nil {
		t.Fatalf("AcquireWork(first) error = %v", err)
	}
	if _, err := manager.AcquireWork(); !errors.Is(err, ErrConnectingWorkCapacity) {
		t.Fatalf("AcquireWork() error = %v, want ErrConnectingWorkCapacity", err)
	}
	if err := first.MarkIdle(); err != nil {
		t.Fatalf("first.MarkIdle() error = %v", err)
	}
	second, err := manager.AcquireWork()
	if err != nil {
		t.Fatalf("AcquireWork(second) error = %v", err)
	}
	if _, err := manager.AcquireWork(); !errors.Is(err, ErrWorkCapacity) {
		t.Fatalf("AcquireWork() error = %v, want ErrWorkCapacity", err)
	}
	if err := second.MarkIdle(); !errors.Is(err, ErrIdleWorkCapacity) {
		t.Fatalf("second.MarkIdle() error = %v, want ErrIdleWorkCapacity", err)
	}
	if err := first.MarkOpening(); err != nil {
		t.Fatalf("first.MarkOpening() error = %v", err)
	}
	if err := second.MarkIdle(); err != nil {
		t.Fatalf("second.MarkIdle() after IDLE release error = %v", err)
	}
	if err := first.MarkActive(); err != nil {
		t.Fatalf("first.MarkActive() error = %v", err)
	}

	assertSnapshot(t, manager.Snapshot(), Snapshot{
		WorkTotal: 2, WorkIdle: 1,
		ActiveByTunnel: map[string]uint64{}, ActiveByService: map[ConnectionService]uint64{},
		ActiveBySource: map[netip.Addr]uint64{},
	})
	first.Release()
	first.Release()
	second.Release()
	second.Release()
	assertEmpty(t, manager.Snapshot())
}

func TestOpenLeaseFailedActivationRemainsPendingAndCanBeReleased(t *testing.T) {
	manager := newTestManager(t, Options{
		MaxConnectors: 2, MaxConnectorsPerTunnel: 2,
		MaxWorkConnections: 1, MaxIdleWorkConnections: 1, MaxConnectingWorkConnections: 1,
		MaxPendingOpens: 2, MaxActiveConnections: 2, MaxConnectionsPerTunnel: 2,
		MaxConnectionsPerService: 1, MaxConnectionsPerSourceIP: 2,
	})
	first, err := manager.AcquirePendingOpen(testConnectionKey())
	if err != nil {
		t.Fatalf("AcquirePendingOpen(first) error = %v", err)
	}
	second, err := manager.AcquirePendingOpen(testConnectionKey())
	if err != nil {
		t.Fatalf("AcquirePendingOpen(second) error = %v", err)
	}
	if _, err := manager.AcquirePendingOpen(testConnectionKey()); !errors.Is(err, ErrPendingOpenCapacity) {
		t.Fatalf("AcquirePendingOpen() error = %v, want ErrPendingOpenCapacity", err)
	}
	if err := first.Activate(); err != nil {
		t.Fatalf("first.Activate() error = %v", err)
	}
	if err := second.Activate(); !errors.Is(err, ErrActiveConnectionCapacity) {
		t.Fatalf("second.Activate() error = %v, want ErrActiveConnectionCapacity", err)
	}
	got := manager.Snapshot()
	if got.PendingOpens != 1 || got.ActiveTotal != 1 ||
		got.ActiveByTunnel[testTunnelID] != 1 ||
		got.ActiveByService[ConnectionService{TunnelID: testTunnelID, ServiceID: testServiceID}] != 1 ||
		got.ActiveBySource[testSourceIP] != 1 {
		t.Fatalf("Snapshot() after rejected activation = %#v", got)
	}
	second.Release()
	second.Release()
	first.Release()
	first.Release()
	assertEmpty(t, manager.Snapshot())
}

func TestOpenLeaseActivationChecksEveryActiveDimension(t *testing.T) {
	tests := []struct {
		name    string
		options Options
		second  ConnectionKey
	}{
		{
			name: "global", options: openOptions(1, 2, 2, 2),
			second: ConnectionKey{TunnelID: "tun_01J00000000000000000000001", ServiceID: "svc_01J00000000000000000000001", SourceIP: netip.MustParseAddr("192.0.2.11")},
		},
		{
			name: "tunnel", options: openOptions(2, 1, 2, 2),
			second: ConnectionKey{TunnelID: testTunnelID, ServiceID: "svc_01J00000000000000000000001", SourceIP: netip.MustParseAddr("192.0.2.11")},
		},
		{
			name: "service", options: openOptions(2, 2, 1, 2),
			second: ConnectionKey{TunnelID: testTunnelID, ServiceID: testServiceID, SourceIP: netip.MustParseAddr("192.0.2.11")},
		},
		{
			name: "source", options: openOptions(2, 2, 2, 1),
			second: ConnectionKey{TunnelID: "tun_01J00000000000000000000001", ServiceID: "svc_01J00000000000000000000001", SourceIP: testSourceIP},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := newTestManager(t, test.options)
			first, err := manager.AcquirePendingOpen(testConnectionKey())
			if err != nil {
				t.Fatalf("AcquirePendingOpen(first) error = %v", err)
			}
			second, err := manager.AcquirePendingOpen(test.second)
			if err != nil {
				t.Fatalf("AcquirePendingOpen(second) error = %v", err)
			}
			if err := first.Activate(); err != nil {
				t.Fatalf("first.Activate() error = %v", err)
			}
			if err := second.Activate(); !errors.Is(err, ErrActiveConnectionCapacity) {
				t.Fatalf("second.Activate() error = %v, want ErrActiveConnectionCapacity", err)
			}
			first.Release()
			second.Release()
			assertEmpty(t, manager.Snapshot())
		})
	}
}

func TestConcurrentWorkAcquireNeverExceedsHardLimit(t *testing.T) {
	manager := newTestManager(t, Options{
		MaxConnectors: 2, MaxConnectorsPerTunnel: 2,
		MaxWorkConnections: 7, MaxIdleWorkConnections: 7, MaxConnectingWorkConnections: 7,
		MaxPendingOpens: 1, MaxActiveConnections: 1, MaxConnectionsPerTunnel: 1,
		MaxConnectionsPerService: 1, MaxConnectionsPerSourceIP: 1,
	})
	start := make(chan struct{})
	release := make(chan struct{})
	var acquired atomic.Int64
	var wait sync.WaitGroup
	results := make(chan bool, 100)
	for range 100 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			lease, err := manager.AcquireWork()
			if err != nil {
				if !errors.Is(err, ErrWorkCapacity) && !errors.Is(err, ErrConnectingWorkCapacity) {
					t.Errorf("AcquireWork() unexpected error = %v", err)
				}
				results <- false
				return
			}
			acquired.Add(1)
			results <- true
			<-release
			lease.Release()
		}()
	}
	close(start)
	var successful int
	for range 100 {
		if <-results {
			successful++
		}
	}
	if got := manager.Snapshot().WorkTotal; got != 7 {
		t.Fatalf("concurrent WorkTotal = %d, want 7", got)
	}
	if successful != 7 {
		t.Fatalf("successful concurrent acquisitions = %d, want 7", successful)
	}
	close(release)
	wait.Wait()
	if got := acquired.Load(); got != 7 {
		t.Fatalf("successful concurrent acquisitions = %d, want 7", got)
	}
	assertEmpty(t, manager.Snapshot())
}

func TestConcurrentActivationLinearizesPerSourceLimit(t *testing.T) {
	manager := newTestManager(t, Options{
		MaxConnectors: 2, MaxConnectorsPerTunnel: 2,
		MaxWorkConnections: 1, MaxIdleWorkConnections: 1, MaxConnectingWorkConnections: 1,
		MaxPendingOpens: 64, MaxActiveConnections: 64, MaxConnectionsPerTunnel: 64,
		MaxConnectionsPerService: 64, MaxConnectionsPerSourceIP: 1,
	})
	leases := make([]*OpenLease, 64)
	for index := range leases {
		lease, err := manager.AcquirePendingOpen(testConnectionKey())
		if err != nil {
			t.Fatalf("AcquirePendingOpen(%d) error = %v", index, err)
		}
		leases[index] = lease
	}
	start := make(chan struct{})
	var active atomic.Int64
	var wait sync.WaitGroup
	for _, lease := range leases {
		wait.Add(1)
		go func(lease *OpenLease) {
			defer wait.Done()
			<-start
			if err := lease.Activate(); err == nil {
				active.Add(1)
			} else if !errors.Is(err, ErrActiveConnectionCapacity) {
				t.Errorf("Activate() unexpected error = %v", err)
			}
		}(lease)
	}
	close(start)
	wait.Wait()
	if got := active.Load(); got != 1 {
		t.Fatalf("concurrent active leases = %d, want 1", got)
	}
	if got := manager.Snapshot(); got.ActiveTotal != 1 || got.PendingOpens != 63 {
		t.Fatalf("Snapshot() = %#v, want one active and 63 pending", got)
	}
	for _, lease := range leases {
		lease.Release()
		lease.Release()
	}
	assertEmpty(t, manager.Snapshot())
}

func openOptions(global, tunnel, service, source uint64) Options {
	return Options{
		MaxConnectors: 2, MaxConnectorsPerTunnel: 2,
		MaxWorkConnections: 1, MaxIdleWorkConnections: 1, MaxConnectingWorkConnections: 1,
		MaxPendingOpens: 2, MaxActiveConnections: global, MaxConnectionsPerTunnel: tunnel,
		MaxConnectionsPerService: service, MaxConnectionsPerSourceIP: source,
	}
}

func testConnectionKey() ConnectionKey {
	return ConnectionKey{TunnelID: testTunnelID, ServiceID: testServiceID, SourceIP: testSourceIP}
}

func newTestManager(t *testing.T, options Options) *Manager {
	t.Helper()
	manager, err := New(options)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return manager
}

func assertEmpty(t *testing.T, got Snapshot) {
	t.Helper()
	assertSnapshot(t, got, Snapshot{
		ConnectorsByTunnel: map[string]uint64{},
		ActiveByTunnel:     map[string]uint64{}, ActiveByService: map[ConnectionService]uint64{},
		ActiveBySource: map[netip.Addr]uint64{},
	})
}

func assertSnapshot(t *testing.T, got, want Snapshot) {
	t.Helper()
	if got.Connectors != want.Connectors || got.WorkTotal != want.WorkTotal || got.WorkConnecting != want.WorkConnecting ||
		got.WorkIdle != want.WorkIdle || got.PendingOpens != want.PendingOpens ||
		got.ActiveTotal != want.ActiveTotal || len(got.ConnectorsByTunnel) != len(want.ConnectorsByTunnel) ||
		len(got.ActiveByTunnel) != len(want.ActiveByTunnel) ||
		len(got.ActiveByService) != len(want.ActiveByService) || len(got.ActiveBySource) != len(want.ActiveBySource) {
		t.Fatalf("Snapshot() = %#v, want %#v", got, want)
	}
	for key, count := range want.ConnectorsByTunnel {
		if got.ConnectorsByTunnel[key] != count {
			t.Fatalf("ConnectorsByTunnel[%q] = %d, want %d", key, got.ConnectorsByTunnel[key], count)
		}
	}
	for key, count := range want.ActiveByTunnel {
		if got.ActiveByTunnel[key] != count {
			t.Fatalf("ActiveByTunnel[%q] = %d, want %d", key, got.ActiveByTunnel[key], count)
		}
	}
	for key, count := range want.ActiveByService {
		if got.ActiveByService[key] != count {
			t.Fatalf("ActiveByService[%#v] = %d, want %d", key, got.ActiveByService[key], count)
		}
	}
	for key, count := range want.ActiveBySource {
		if got.ActiveBySource[key] != count {
			t.Fatalf("ActiveBySource[%s] = %d, want %d", key, got.ActiveBySource[key], count)
		}
	}
}
