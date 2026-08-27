package healthbudget

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

const (
	testTunnelID     = "tun_01J00000000000000000000000"
	testTunnelIDTwo  = "tun_01J00000000000000000000001"
	testConnectorID  = "con_01J00000000000000000000000"
	testConnectorTwo = "con_01J00000000000000000000001"
)

func TestNewRejectsInvalidOptions(t *testing.T) {
	tests := []struct {
		name    string
		options Options
	}{
		{name: "zero per tunnel", options: Options{MaxTargetsGlobal: 1}},
		{name: "zero global", options: Options{MaxTargetsPerTunnel: 1}},
		{name: "per tunnel exceeds global", options: Options{MaxTargetsPerTunnel: 2, MaxTargetsGlobal: 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := New(test.options); !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("New(%+v) error = %v, want ErrInvalidOptions", test.options, err)
			}
		})
	}
}

func TestInitializeTunnelIsIdempotentOnlyForSameBaseline(t *testing.T) {
	manager := mustManager(t, 10, 20)
	if err := manager.InitializeTunnel(testTunnelID, 3, 2); err != nil {
		t.Fatal(err)
	}
	if err := manager.InitializeTunnel(testTunnelID, 3, 2); err != nil {
		t.Fatalf("InitializeTunnel(same baseline) error = %v", err)
	}
	for _, test := range []struct {
		name         string
		tunnelID     string
		revision     uint64
		enabledCount uint64
		want         error
	}{
		{name: "different revision", tunnelID: testTunnelID, revision: 4, enabledCount: 2, want: ErrTunnelAlreadyInitialized},
		{name: "different count", tunnelID: testTunnelID, revision: 3, enabledCount: 3, want: ErrTunnelAlreadyInitialized},
		{name: "invalid tunnel", tunnelID: "tun-invalid", revision: 1, enabledCount: 1, want: ErrInvalidTunnelID},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := manager.InitializeTunnel(test.tunnelID, test.revision, test.enabledCount); !errors.Is(err, test.want) {
				t.Fatalf("InitializeTunnel() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestConfigurationReservationCommitAndRelease(t *testing.T) {
	manager := mustManager(t, 10, 20)
	mustInitialize(t, manager, testTunnelID, 1, 2)
	connector := mustAcquire(t, manager, testTunnelID, testConnectorID)

	released, err := manager.ReserveConfiguration(testTunnelID, 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	assertTunnel(t, manager, testTunnelID, TunnelSnapshot{
		Revision: 1, EnabledCount: 2, ConnectorCount: 1, Targets: 4,
		ReservationActive: true, ReservationRevision: 2, ReservationCandidateCount: 4,
	})
	if !released.Release() || released.Release() {
		t.Fatal("Release() did not finalize exactly once")
	}
	assertTunnel(t, manager, testTunnelID, TunnelSnapshot{
		Revision: 1, EnabledCount: 2, ConnectorCount: 1, Targets: 2,
	})

	committed, err := manager.ReserveConfiguration(testTunnelID, 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	if released.Commit() || released.Release() {
		t.Fatal("old Reservation changed a newer Reservation")
	}
	if snapshot := manager.Snapshot().Tunnels[testTunnelID]; !snapshot.ReservationActive ||
		snapshot.ReservationRevision != 2 || snapshot.Targets != 4 {
		t.Fatalf("newer Reservation after old cleanup = %+v", snapshot)
	}
	if !committed.Commit() || committed.Commit() || committed.Release() {
		t.Fatal("Commit/Release did not finalize exactly once")
	}
	assertTunnel(t, manager, testTunnelID, TunnelSnapshot{
		Revision: 2, EnabledCount: 4, ConnectorCount: 1, Targets: 4,
	})
	connector.Release()
}

func TestConfigurationReservationFencesSameTunnel(t *testing.T) {
	manager := mustManager(t, 10, 20)
	mustInitialize(t, manager, testTunnelID, 5, 2)
	lease, err := manager.ReserveConfiguration(testTunnelID, 6, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ReserveConfiguration(testTunnelID, 7, 4); !errors.Is(err, ErrConfigurationConflict) {
		t.Fatalf("concurrent ReserveConfiguration() error = %v, want ErrConfigurationConflict", err)
	}
	if !lease.Release() {
		t.Fatal("Release() = false, want true")
	}
	for _, revision := range []uint64{4, 5} {
		if _, err := manager.ReserveConfiguration(testTunnelID, revision, 3); !errors.Is(err, ErrConfigurationRevision) {
			t.Fatalf("ReserveConfiguration(revision %d) error = %v, want ErrConfigurationRevision", revision, err)
		}
	}
	if _, err := manager.ReserveConfiguration(testTunnelIDTwo, 1, 1); !errors.Is(err, ErrTunnelNotInitialized) {
		t.Fatalf("ReserveConfiguration(uninitialized) error = %v, want ErrTunnelNotInitialized", err)
	}
}

func TestConnectorReplacementUsesReferencesWithoutDoubleCharging(t *testing.T) {
	manager := mustManager(t, 5, 5)
	mustInitialize(t, manager, testTunnelID, 1, 5)
	oldGeneration := mustAcquire(t, manager, testTunnelID, testConnectorID)
	newGeneration := mustAcquire(t, manager, testTunnelID, testConnectorID)
	key := ConnectorKey{TunnelID: testTunnelID, ConnectorID: testConnectorID}
	if snapshot := manager.Snapshot(); snapshot.TargetsGlobal != 5 || snapshot.ConnectorReferences[key] != 2 {
		t.Fatalf("replacement Snapshot = %+v, want targets 5 refs 2", snapshot)
	}
	if !oldGeneration.Release() {
		t.Fatal("old generation Release() = false")
	}
	if snapshot := manager.Snapshot(); snapshot.TargetsGlobal != 5 || snapshot.ConnectorReferences[key] != 1 {
		t.Fatalf("old cleanup Snapshot = %+v, want targets 5 refs 1", snapshot)
	}
	if !newGeneration.Release() || newGeneration.Release() {
		t.Fatal("new generation Release() did not run exactly once")
	}
	if snapshot := manager.Snapshot(); snapshot.TargetsGlobal != 0 || len(snapshot.ConnectorReferences) != 0 {
		t.Fatalf("final Snapshot = %+v, want empty connector budget", snapshot)
	}
}

func TestConfigurationAndAuthUseOneProjectedTarget(t *testing.T) {
	tests := []struct {
		name          string
		current       uint64
		candidate     uint64
		wantReserved  uint64
		wantAfterAuth uint64
		commit        bool
		wantFinal     uint64
	}{
		{name: "increase release", current: 2, candidate: 4, wantReserved: 4, wantAfterAuth: 8, wantFinal: 4},
		{name: "decrease release", current: 4, candidate: 2, wantReserved: 4, wantAfterAuth: 8, wantFinal: 8},
		{name: "decrease commit", current: 4, candidate: 2, wantReserved: 4, wantAfterAuth: 8, commit: true, wantFinal: 4},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := mustManager(t, 8, 8)
			mustInitialize(t, manager, testTunnelID, 1, test.current)
			first := mustAcquire(t, manager, testTunnelID, testConnectorID)
			reservation, err := manager.ReserveConfiguration(testTunnelID, 2, test.candidate)
			if err != nil {
				t.Fatal(err)
			}
			if got := manager.Snapshot().TargetsGlobal; got != test.wantReserved {
				t.Fatalf("reserved targets = %d, want %d", got, test.wantReserved)
			}
			second := mustAcquire(t, manager, testTunnelID, testConnectorTwo)
			if got := manager.Snapshot().TargetsGlobal; got != test.wantAfterAuth {
				t.Fatalf("targets after Auth = %d, want %d", got, test.wantAfterAuth)
			}
			if test.commit {
				reservation.Commit()
			} else {
				reservation.Release()
			}
			if got := manager.Snapshot().TargetsGlobal; got != test.wantFinal {
				t.Fatalf("final targets = %d, want %d", got, test.wantFinal)
			}
			first.Release()
			second.Release()
		})
	}
}

func TestTunnelAndGlobalCapacityRejectAtomically(t *testing.T) {
	manager := mustManager(t, 6, 8)
	mustInitialize(t, manager, testTunnelID, 1, 3)
	mustInitialize(t, manager, testTunnelIDTwo, 1, 2)
	first := mustAcquire(t, manager, testTunnelID, testConnectorID)
	second := mustAcquire(t, manager, testTunnelID, testConnectorTwo)
	if _, err := manager.AcquireConnector(testTunnelID, "con_01J00000000000000000000002"); !errors.Is(err, ErrTargetCapacity) {
		t.Fatalf("AcquireConnector(per-tunnel overflow) error = %v, want ErrTargetCapacity", err)
	}
	third := mustAcquire(t, manager, testTunnelIDTwo, testConnectorID)
	if _, err := manager.AcquireConnector(testTunnelIDTwo, testConnectorTwo); !errors.Is(err, ErrTargetCapacity) {
		t.Fatalf("AcquireConnector(global overflow) error = %v, want ErrTargetCapacity", err)
	}
	if _, err := manager.ReserveConfiguration(testTunnelID, 2, 4); !errors.Is(err, ErrTargetCapacity) {
		t.Fatalf("ReserveConfiguration(over capacity) error = %v, want ErrTargetCapacity", err)
	}
	if snapshot := manager.Snapshot(); snapshot.TargetsGlobal != 8 || snapshot.Tunnels[testTunnelID].ReservationActive {
		t.Fatalf("failed reservations changed Snapshot: %+v", snapshot)
	}
	first.Release()
	second.Release()
	third.Release()
}

func TestTargetMultiplicationOverflowFailsClosed(t *testing.T) {
	maximum := ^uint64(0)
	manager := mustManager(t, maximum, maximum)
	mustInitialize(t, manager, testTunnelID, 1, maximum)
	first := mustAcquire(t, manager, testTunnelID, testConnectorID)
	if _, err := manager.AcquireConnector(testTunnelID, testConnectorTwo); !errors.Is(err, ErrTargetCapacity) {
		t.Fatalf("AcquireConnector(overflow) error = %v, want ErrTargetCapacity", err)
	}
	snapshot := manager.Snapshot()
	if snapshot.TargetsGlobal != maximum || snapshot.Tunnels[testTunnelID].ConnectorCount != 1 {
		t.Fatalf("overflow changed Snapshot: %+v", snapshot)
	}
	first.Release()
}

func TestConfigurationLeaseFinalizesOnceUnderConcurrency(t *testing.T) {
	manager := mustManager(t, 10, 20)
	mustInitialize(t, manager, testTunnelID, 1, 2)
	connector := mustAcquire(t, manager, testTunnelID, testConnectorID)
	lease, err := manager.ReserveConfiguration(testTunnelID, 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	var successes atomic.Uint64
	var wait sync.WaitGroup
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if lease.Commit() {
				successes.Add(1)
			}
		}()
	}
	wait.Wait()
	if successes.Load() != 1 {
		t.Fatalf("successful Commit calls = %d, want 1", successes.Load())
	}
	assertTunnel(t, manager, testTunnelID, TunnelSnapshot{
		Revision: 2, EnabledCount: 4, ConnectorCount: 1, Targets: 4,
	})
	connector.Release()
}

func TestConcurrentDifferentTunnels(t *testing.T) {
	const tunnelCount = 32
	manager := mustManager(t, tunnelCount, tunnelCount)
	for index := range tunnelCount {
		mustInitialize(t, manager, tunnelID(index), 1, 1)
	}
	leases := make([]*ConnectorLease, tunnelCount)
	var wait sync.WaitGroup
	for index := range tunnelCount {
		wait.Add(1)
		go func() {
			defer wait.Done()
			lease, err := manager.AcquireConnector(tunnelID(index), connectorID(index))
			if err != nil {
				t.Errorf("AcquireConnector(%d) error = %v", index, err)
				return
			}
			leases[index] = lease
		}()
	}
	wait.Wait()
	if got := manager.Snapshot().TargetsGlobal; got != tunnelCount {
		t.Fatalf("concurrent targets = %d, want %d", got, tunnelCount)
	}
	for _, lease := range leases {
		wait.Add(1)
		go func() {
			defer wait.Done()
			lease.Release()
		}()
	}
	wait.Wait()
	if got := manager.Snapshot().TargetsGlobal; got != 0 {
		t.Fatalf("targets after concurrent Release = %d, want 0", got)
	}
}

func TestSnapshotIsDeepCopy(t *testing.T) {
	manager := mustManager(t, 10, 20)
	mustInitialize(t, manager, testTunnelID, 1, 2)
	lease := mustAcquire(t, manager, testTunnelID, testConnectorID)
	first := manager.Snapshot()
	first.TargetsGlobal = 99
	first.Tunnels[testTunnelID] = TunnelSnapshot{Targets: 99}
	first.ConnectorReferences[ConnectorKey{TunnelID: testTunnelID, ConnectorID: testConnectorID}] = 99
	second := manager.Snapshot()
	if second.TargetsGlobal != 2 || second.Tunnels[testTunnelID].Targets != 2 ||
		second.ConnectorReferences[ConnectorKey{TunnelID: testTunnelID, ConnectorID: testConnectorID}] != 1 {
		t.Fatalf("mutated Snapshot polluted Manager: %+v", second)
	}
	lease.Release()
}

func mustManager(t *testing.T, perTunnel, global uint64) *Manager {
	t.Helper()
	manager, err := New(Options{MaxTargetsPerTunnel: perTunnel, MaxTargetsGlobal: global})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func mustInitialize(t *testing.T, manager *Manager, tunnelID string, revision, enabledCount uint64) {
	t.Helper()
	if err := manager.InitializeTunnel(tunnelID, revision, enabledCount); err != nil {
		t.Fatal(err)
	}
}

func mustAcquire(t *testing.T, manager *Manager, tunnelID, connectorID string) *ConnectorLease {
	t.Helper()
	lease, err := manager.AcquireConnector(tunnelID, connectorID)
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func assertTunnel(t *testing.T, manager *Manager, tunnelID string, want TunnelSnapshot) {
	t.Helper()
	if got := manager.Snapshot().Tunnels[tunnelID]; got != want {
		t.Fatalf("Tunnel Snapshot = %+v, want %+v", got, want)
	}
}

func tunnelID(index int) string {
	return fmt.Sprintf("tun_01J00000000000000000000%03d", index)
}

func connectorID(index int) string {
	return fmt.Sprintf("con_01J00000000000000000000%03d", index)
}
