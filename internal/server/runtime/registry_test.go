package runtime

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/identity"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
)

const (
	runtimeTunnelID       = "tun_01J00000000000000000000000"
	runtimeTunnelIDTwo    = "tun_01J00000000000000000000001"
	runtimeConnectorID    = "con_01J00000000000000000000000"
	runtimeConnectorIDTwo = "con_01J00000000000000000000001"
)

func TestRegistryReserveAndCommitAuthenticated(t *testing.T) {
	tests := []struct {
		name        string
		tunnelID    string
		connectorID string
		newSession  func() (string, error)
		wantErr     error
	}{
		{name: "合法", tunnelID: runtimeTunnelID, connectorID: runtimeConnectorID, newSession: sessionGenerator(1)},
		{name: "错误 Tunnel ID", tunnelID: "tun_invalid", connectorID: runtimeConnectorID, newSession: sessionGenerator(1), wantErr: ErrInvalidTunnelID},
		{name: "超出 Tunnel ULID 范围", tunnelID: "tun_81J00000000000000000000000", connectorID: runtimeConnectorID, newSession: sessionGenerator(1), wantErr: ErrInvalidTunnelID},
		{name: "错误 Connector ID", tunnelID: runtimeTunnelID, connectorID: "con_invalid", newSession: sessionGenerator(1), wantErr: ErrInvalidConnectorID},
		{name: "错误 Session ID", tunnelID: runtimeTunnelID, connectorID: runtimeConnectorID, newSession: func() (string, error) { return "sess_invalid", nil }, wantErr: identity.ErrInvalidSessionID},
		{name: "Session 生成失败", tunnelID: runtimeTunnelID, connectorID: runtimeConnectorID, newSession: func() (string, error) { return "", errRandomSourceFailed }, wantErr: errRandomSourceFailed},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := newRegistry(test.newSession)
			session, err := installAuthenticated(registry, test.tunnelID, test.connectorID)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("installAuthenticated() error = %v, want %v", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("installAuthenticated() error = %v", err)
			}
			if session.TunnelID != runtimeTunnelID || session.ConnectorID != runtimeConnectorID || session.Generation != 1 || !identity.ValidSessionID(session.SessionID) {
				t.Fatalf("installAuthenticated() = %#v, want valid first Session", session)
			}
		})
	}
}

func TestRegistryReconnectAndDifferentConnectors(t *testing.T) {
	registry := newRegistry(sessionGenerator(1))
	first, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatalf("installAuthenticated(first) error = %v", err)
	}
	reconnected, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatalf("installAuthenticated(reconnect) error = %v", err)
	}
	if first.ConnectorID != reconnected.ConnectorID || first.SessionID == reconnected.SessionID || reconnected.Generation != first.Generation+1 {
		t.Fatalf("reconnect sessions = %#v, %#v, want same Connector and next generation", first, reconnected)
	}

	otherConnector, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorIDTwo)
	if err != nil {
		t.Fatalf("installAuthenticated(other connector) error = %v", err)
	}
	if otherConnector.Generation != 1 {
		t.Fatalf("other connector generation = %d, want 1", otherConnector.Generation)
	}
	currentFirst, foundFirst := registry.Current(runtimeTunnelID, runtimeConnectorID)
	currentSecond, foundSecond := registry.Current(runtimeTunnelID, runtimeConnectorIDTwo)
	if !foundFirst || !foundSecond || currentFirst != reconnected || currentSecond != otherConnector {
		t.Fatalf("Current() = (%#v, %v), (%#v, %v), want both Connectors online", currentFirst, foundFirst, currentSecond, foundSecond)
	}
}

func TestRegistryConnectorLimitsCountReplacementOnce(t *testing.T) {
	limitManager, err := serverlimits.New(serverlimits.Options{
		MaxConnectors: 2, MaxConnectorsPerTunnel: 2,
		MaxWorkConnections: 1, MaxIdleWorkConnections: 1, MaxConnectingWorkConnections: 1,
		MaxPendingOpens: 1, MaxActiveConnections: 1, MaxConnectionsPerTunnel: 1,
		MaxConnectionsPerService: 1, MaxConnectionsPerSourceIP: 1,
	})
	if err != nil {
		t.Fatalf("limits.New() error = %v", err)
	}
	registry := NewRegistryWithLimits(limitManager)
	registry.newSession = sessionGenerator(1)
	first, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatalf("installAuthenticated(first) error = %v", err)
	}
	failedPending, err := registry.ReserveAuthenticated(runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatalf("ReserveAuthenticated(failed replacement) error = %v", err)
	}
	failedReplacement, err := registry.InstallAuthenticated(failedPending)
	if err != nil {
		t.Fatalf("InstallAuthenticated(failed replacement) error = %v", err)
	}
	if !failedReplacement.Rollback() {
		t.Fatal("Rollback(failed replacement) did not restore first Session")
	}
	if current, exists := registry.Current(runtimeTunnelID, runtimeConnectorID); !exists || current != first {
		t.Fatalf("Current() = %#v, %v, want restored first Session %#v", current, exists, first)
	}
	if got := limitManager.Snapshot().Connectors; got != 1 {
		t.Fatalf("Connectors after replacement rollback = %d, want retained original identity", got)
	}
	replacement, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatalf("installAuthenticated(replacement) error = %v", err)
	}
	if got := limitManager.Snapshot().Connectors; got != 1 {
		t.Fatalf("Connectors after replacement = %d, want 1", got)
	}
	second, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorIDTwo)
	if err != nil {
		t.Fatalf("installAuthenticated(second Connector) error = %v", err)
	}
	pending, err := registry.ReserveAuthenticated(runtimeTunnelIDTwo, "con_01J00000000000000000000002")
	if !errors.Is(err, serverlimits.ErrConnectorCapacity) {
		t.Fatalf("ReserveAuthenticated(over global limit) = %#v, %v, want ErrConnectorCapacity", pending, err)
	}
	if !registry.ClearIfCurrent(replacement) || !registry.ClearIfCurrent(second) {
		t.Fatal("ClearIfCurrent() did not clear current limited Connectors")
	}
	if registry.ClearIfCurrent(first) {
		t.Fatal("old generation unexpectedly cleared replacement")
	}
	if got := limitManager.Snapshot().Connectors; got != 0 {
		t.Fatalf("Connectors after clear = %d, want 0", got)
	}
}

func TestAcquireConnectorWhereSkipsConnectorWithoutIdleCapacity(t *testing.T) {
	registry := newRegistry(sessionGenerator(1))
	withoutIdle, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatalf("installAuthenticated(without idle) error = %v", err)
	}
	withIdle, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorIDTwo)
	if err != nil {
		t.Fatalf("installAuthenticated(with idle) error = %v", err)
	}
	idle := map[Session]uint32{withoutIdle: 0, withIdle: 1}
	lease, err := registry.AcquireConnectorWhere(runtimeTunnelID, func(session Session) bool {
		return idle[session] > 0
	})
	if err != nil {
		t.Fatalf("AcquireConnectorWhere() error = %v", err)
	}
	if got := lease.Session(); got != withIdle {
		t.Fatalf("selected Session = %#v, want only IDLE-capable %#v", got, withIdle)
	}
	lease.Release()
}

func TestRegistryReservationIsInvisibleUntilCommit(t *testing.T) {
	registry := newRegistry(sessionGenerator(1))
	pending, err := registry.ReserveAuthenticated(runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatalf("ReserveAuthenticated() error = %v", err)
	}
	if !identity.ValidSessionID(pending.SessionID()) {
		t.Fatalf("Pending SessionID() = %q, want valid Session ID", pending.SessionID())
	}
	if _, exists := registry.Current(runtimeTunnelID, runtimeConnectorID); exists {
		t.Fatal("Current() published a Session before the AUTH success frame was committed")
	}

	session, err := registry.CommitAuthenticated(pending)
	if err != nil {
		t.Fatalf("CommitAuthenticated() error = %v", err)
	}
	if session.SessionID != pending.SessionID() || session.Generation != 1 {
		t.Fatalf("CommitAuthenticated() = %#v, want reserved ID and generation 1", session)
	}
	if current, exists := registry.Current(runtimeTunnelID, runtimeConnectorID); !exists || current != session {
		t.Fatalf("Current() = %#v, %v, want committed Session", current, exists)
	}
}

func TestAuthenticatedInstallNestedRollbackSkipsFailedReplacement(t *testing.T) {
	registry := newRegistry(sessionGenerator(1))
	stable, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatalf("installAuthenticated(stable) error = %v", err)
	}
	reserveInstall := func(name string) *AuthenticatedSessionInstall {
		t.Helper()
		pending, reserveErr := registry.ReserveAuthenticated(runtimeTunnelID, runtimeConnectorID)
		if reserveErr != nil {
			t.Fatalf("ReserveAuthenticated(%s) error = %v", name, reserveErr)
		}
		install, installErr := registry.InstallAuthenticated(pending)
		if installErr != nil {
			t.Fatalf("InstallAuthenticated(%s) error = %v", name, installErr)
		}
		return install
	}

	failedOlder := reserveInstall("failed older replacement")
	newer := reserveInstall("newer replacement")
	if failedOlder.Rollback() {
		t.Fatal("older Rollback() replaced a newer Current Session")
	}
	if current, exists := registry.Current(runtimeTunnelID, runtimeConnectorID); !exists || current != newer.Session() {
		t.Fatalf("Current() = %#v, %v, want newer replacement %#v", current, exists, newer.Session())
	}
	if !newer.Rollback() {
		t.Fatal("newer Rollback() did not restore the nearest healthy Session")
	}
	if current, exists := registry.Current(runtimeTunnelID, runtimeConnectorID); !exists || current != stable {
		t.Fatalf("Current() = %#v, %v, want stable Session %#v after skipping failed replacement", current, exists, stable)
	}
}

func TestAuthenticatedInstallRollbackDoesNotRestoreCleanedHistoricalSession(t *testing.T) {
	registry := newRegistry(sessionGenerator(1))
	stable, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatalf("installAuthenticated(stable) error = %v", err)
	}
	pending, err := registry.ReserveAuthenticated(runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatalf("ReserveAuthenticated(replacement) error = %v", err)
	}
	replacement, err := registry.InstallAuthenticated(pending)
	if err != nil {
		t.Fatalf("InstallAuthenticated(replacement) error = %v", err)
	}
	if !registry.ClearIfCurrent(stable) {
		t.Fatal("ClearIfCurrent(stable history) did not invalidate the rollback candidate")
	}
	if current, exists := registry.Current(runtimeTunnelID, runtimeConnectorID); !exists || current != replacement.Session() {
		t.Fatalf("Current() = %#v, %v, want replacement unchanged", current, exists)
	}
	if !replacement.Rollback() {
		t.Fatal("Rollback(replacement) did not resolve the transaction head")
	}
	if current, exists := registry.Current(runtimeTunnelID, runtimeConnectorID); exists {
		t.Fatalf("Rollback restored cleaned historical Session %#v", current)
	}
}

func TestAuthenticatedInstallRollbackDoesNotRestoreCleanedFinalizedPredecessor(t *testing.T) {
	registry := newRegistry(sessionGenerator(1))
	if _, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID); err != nil {
		t.Fatalf("installAuthenticated(stable) error = %v", err)
	}
	installReplacement := func(name string) *AuthenticatedSessionInstall {
		t.Helper()
		pending, reserveErr := registry.ReserveAuthenticated(runtimeTunnelID, runtimeConnectorID)
		if reserveErr != nil {
			t.Fatalf("ReserveAuthenticated(%s) error = %v", name, reserveErr)
		}
		install, installErr := registry.InstallAuthenticated(pending)
		if installErr != nil {
			t.Fatalf("InstallAuthenticated(%s) error = %v", name, installErr)
		}
		return install
	}

	finalized := installReplacement("finalized")
	if !finalized.Finalize() {
		t.Fatal("Finalize(first replacement) = false")
	}
	newer := installReplacement("newer")
	if !registry.ClearIfCurrent(finalized.Session()) {
		t.Fatal("ClearIfCurrent(finalized history) did not invalidate the rollback candidate")
	}
	if current, exists := registry.Current(runtimeTunnelID, runtimeConnectorID); !exists || current != newer.Session() {
		t.Fatalf("Current() = %#v, %v, want newer replacement unchanged", current, exists)
	}
	if !newer.Rollback() {
		t.Fatal("Rollback(newer) did not resolve the transaction head")
	}
	if current, exists := registry.Current(runtimeTunnelID, runtimeConnectorID); exists {
		t.Fatalf("Rollback restored cleaned finalized predecessor %#v", current)
	}
}

func TestAuthenticatedInstallFinalizeMakesReplacementIrreversible(t *testing.T) {
	registry := newRegistry(sessionGenerator(1))
	stable, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatalf("installAuthenticated(stable) error = %v", err)
	}
	pending, err := registry.ReserveAuthenticated(runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatalf("ReserveAuthenticated(replacement) error = %v", err)
	}
	replacement, err := registry.InstallAuthenticated(pending)
	if err != nil {
		t.Fatalf("InstallAuthenticated(replacement) error = %v", err)
	}
	if !replacement.Finalize() {
		t.Fatal("Finalize() = false, want first finalization to succeed")
	}
	if replacement.Rollback() {
		t.Fatal("Rollback() restored old Session after finalization")
	}
	if current, exists := registry.Current(runtimeTunnelID, runtimeConnectorID); !exists || current != replacement.Session() || current == stable {
		t.Fatalf("Current() = %#v, %v, want finalized replacement %#v", current, exists, replacement.Session())
	}
}

func TestRegistryCancelledReservationDoesNotConsumeGeneration(t *testing.T) {
	registry := newRegistry(sessionGenerator(1))
	cancelled, err := registry.ReserveAuthenticated(runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatalf("ReserveAuthenticated(cancelled) error = %v", err)
	}
	if !registry.CancelAuthenticated(cancelled) {
		t.Fatal("CancelAuthenticated() = false, want true")
	}
	if _, err := registry.CommitAuthenticated(cancelled); !errors.Is(err, ErrPendingSessionNotFound) {
		t.Fatalf("CommitAuthenticated(cancelled) error = %v, want ErrPendingSessionNotFound", err)
	}

	committed, err := registry.ReserveAuthenticated(runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatalf("ReserveAuthenticated(committed) error = %v", err)
	}
	session, err := registry.CommitAuthenticated(committed)
	if err != nil {
		t.Fatalf("CommitAuthenticated() error = %v", err)
	}
	if session.Generation != 1 {
		t.Fatalf("Generation = %d, want failed flush reservation not to consume generation", session.Generation)
	}
}

func TestRegistryClearIfCurrentFencesOldSession(t *testing.T) {
	registry := newRegistry(sessionGenerator(1))
	oldSession, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatalf("installAuthenticated(old) error = %v", err)
	}
	newSession, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatalf("installAuthenticated(new) error = %v", err)
	}
	if registry.ClearIfCurrent(oldSession) {
		t.Fatal("ClearIfCurrent(old) cleared the replacement Session")
	}
	current, found := registry.Current(runtimeTunnelID, runtimeConnectorID)
	if !found || current != newSession {
		t.Fatalf("Current() = %#v, %v, want new Session %#v", current, found, newSession)
	}
	if !registry.ClearIfCurrent(newSession) {
		t.Fatal("ClearIfCurrent(new) = false, want true")
	}
	if _, found := registry.Current(runtimeTunnelID, runtimeConnectorID); found {
		t.Fatal("Current() found a Session after its current cleanup")
	}
}

func TestRegistryRejectsCurrentSessionIDCollision(t *testing.T) {
	duplicate := "sess_01J00000000000000000000000"
	registry := newRegistry(func() (string, error) { return duplicate, nil })
	if _, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID); err != nil {
		t.Fatalf("installAuthenticated(first) error = %v", err)
	}
	if _, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorIDTwo); !errors.Is(err, ErrSessionIDCollision) {
		t.Fatalf("installAuthenticated(collision) error = %v, want ErrSessionIDCollision", err)
	}
}

func TestRegistryRejectsPendingSessionIDCollision(t *testing.T) {
	duplicate := "sess_01J00000000000000000000000"
	registry := newRegistry(func() (string, error) { return duplicate, nil })
	pending, err := registry.ReserveAuthenticated(runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatalf("ReserveAuthenticated(first) error = %v", err)
	}
	if _, err := registry.ReserveAuthenticated(runtimeTunnelIDTwo, runtimeConnectorIDTwo); !errors.Is(err, ErrSessionIDCollision) {
		t.Fatalf("ReserveAuthenticated(collision) error = %v, want ErrSessionIDCollision", err)
	}
	if !registry.CancelAuthenticated(pending) {
		t.Fatal("CancelAuthenticated(first) = false, want true")
	}
}

func TestRegistryDifferentTunnelSessionLocksAreIndependent(t *testing.T) {
	registry := newRegistry(sessionGenerator(1))
	first, err := registry.Tunnel(runtimeTunnelID)
	if err != nil {
		t.Fatalf("Tunnel(first) error = %v", err)
	}
	first.mu.Lock()
	locked := true
	t.Cleanup(func() {
		if locked {
			first.mu.Unlock()
		}
	})

	completed := make(chan error, 1)
	go func() {
		_, installErr := installAuthenticated(registry, runtimeTunnelIDTwo, runtimeConnectorID)
		completed <- installErr
	}()
	select {
	case err := <-completed:
		if err != nil {
			t.Fatalf("installAuthenticated(other Tunnel) error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("other Tunnel Session operation was blocked by first TunnelRuntime.mu")
	}
	first.mu.Unlock()
	locked = false
}

func TestRegistryConcurrentReplacement(t *testing.T) {
	const replacements = 64
	registry := newRegistry(sessionGenerator(1))
	var wait sync.WaitGroup
	errorsCh := make(chan error, replacements)
	sessionsCh := make(chan Session, replacements)

	for range replacements {
		wait.Add(1)
		go func() {
			defer wait.Done()
			session, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID)
			if err != nil {
				errorsCh <- err
				return
			}
			sessionsCh <- session
		}()
	}
	wait.Wait()
	close(errorsCh)
	close(sessionsCh)
	for err := range errorsCh {
		t.Errorf("installAuthenticated() error = %v", err)
	}

	seenSessionIDs := make(map[string]struct{}, replacements)
	for session := range sessionsCh {
		if _, exists := seenSessionIDs[session.SessionID]; exists {
			t.Errorf("Session ID %q was generated more than once", session.SessionID)
		}
		seenSessionIDs[session.SessionID] = struct{}{}
	}
	if len(seenSessionIDs) != replacements {
		t.Fatalf("unique Session count = %d, want %d", len(seenSessionIDs), replacements)
	}
	current, found := registry.Current(runtimeTunnelID, runtimeConnectorID)
	if !found || current.Generation != replacements {
		t.Fatalf("Current() = %#v, %v, want generation %d", current, found, replacements)
	}
}

func TestRegistryAcquireConnectorRequiresCurrentSession(t *testing.T) {
	registry := newRegistry(sessionGenerator(1))
	if _, err := registry.AcquireConnector(runtimeTunnelID); !errors.Is(err, ErrNoAvailableConnector) {
		t.Fatalf("AcquireConnector(empty) error = %v, want ErrNoAvailableConnector", err)
	}
	if _, err := registry.AcquireConnector("tun_invalid"); !errors.Is(err, ErrInvalidTunnelID) {
		t.Fatalf("AcquireConnector(invalid ID) error = %v, want ErrInvalidTunnelID", err)
	}
	if _, err := installAuthenticated(registry, runtimeTunnelIDTwo, runtimeConnectorID); err != nil {
		t.Fatalf("installAuthenticated(other tunnel) error = %v", err)
	}
	if _, err := registry.AcquireConnector(runtimeTunnelID); !errors.Is(err, ErrNoAvailableConnector) {
		t.Fatalf("AcquireConnector(wrong tunnel) error = %v, want ErrNoAvailableConnector", err)
	}
}

func TestRegistryAcquireConnectorBalancesTwoConnectorsByDefault(t *testing.T) {
	registry := newRegistry(sessionGenerator(1))
	// 反向安装故意打乱 map 插入顺序；第一次选择仍必须从较小 ConnectorID 开始。
	if _, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorIDTwo); err != nil {
		t.Fatalf("installAuthenticated(second) error = %v", err)
	}
	if _, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID); err != nil {
		t.Fatalf("installAuthenticated(first) error = %v", err)
	}

	want := []string{
		runtimeConnectorID,
		runtimeConnectorIDTwo,
		runtimeConnectorID,
		runtimeConnectorIDTwo,
		runtimeConnectorID,
		runtimeConnectorIDTwo,
	}
	leases := make([]*ConnectorLease, 0, len(want))
	for index, wantConnector := range want {
		lease, err := registry.AcquireConnector(runtimeTunnelID)
		if err != nil {
			t.Fatalf("AcquireConnector(%d) error = %v", index, err)
		}
		leases = append(leases, lease)
		if got := lease.Session().ConnectorID; got != wantConnector {
			t.Fatalf("AcquireConnector(%d) ConnectorID = %q, want %q", index, got, wantConnector)
		}
	}
	for _, lease := range leases {
		if !lease.Release() {
			t.Fatal("ConnectorLease.Release() = false, want first release to decrement")
		}
	}
}

func TestRegistryAcquireConnectorPrefersLeastActive(t *testing.T) {
	registry := newRegistry(sessionGenerator(1))
	firstSession, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatalf("installAuthenticated(first) error = %v", err)
	}
	secondSession, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorIDTwo)
	if err != nil {
		t.Fatalf("installAuthenticated(second) error = %v", err)
	}
	firstLease, err := registry.AcquireConnector(runtimeTunnelID)
	if err != nil {
		t.Fatalf("AcquireConnector(first) error = %v", err)
	}
	secondLease, err := registry.AcquireConnector(runtimeTunnelID)
	if err != nil {
		t.Fatalf("AcquireConnector(second) error = %v", err)
	}
	if firstLease.Session() != firstSession || secondLease.Session() != secondSession {
		t.Fatalf("initial leases = %#v, %#v, want stable Connector order", firstLease.Session(), secondLease.Session())
	}
	if !secondLease.Release() {
		t.Fatal("second Connector Release() = false")
	}
	leastActive, err := registry.AcquireConnector(runtimeTunnelID)
	if err != nil {
		t.Fatalf("AcquireConnector(least active) error = %v", err)
	}
	if got := leastActive.Session(); got != secondSession {
		t.Fatalf("least-active Session = %#v, want %#v", got, secondSession)
	}
	if !firstLease.Release() || !leastActive.Release() {
		t.Fatal("remaining ConnectorLease release failed")
	}
}

func TestRegistryAcquireConnectorUsesStableRoundRobinForTies(t *testing.T) {
	registry := newRegistry(sessionGenerator(1))
	if _, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID); err != nil {
		t.Fatalf("installAuthenticated(first) error = %v", err)
	}
	if _, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorIDTwo); err != nil {
		t.Fatalf("installAuthenticated(second) error = %v", err)
	}

	first, err := registry.AcquireConnector(runtimeTunnelID)
	if err != nil {
		t.Fatalf("AcquireConnector(first) error = %v", err)
	}
	second, err := registry.AcquireConnector(runtimeTunnelID)
	if err != nil {
		t.Fatalf("AcquireConnector(second) error = %v", err)
	}
	if !first.Release() || !second.Release() {
		t.Fatal("initial tie leases did not release")
	}
	third, err := registry.AcquireConnector(runtimeTunnelID)
	if err != nil {
		t.Fatalf("AcquireConnector(third) error = %v", err)
	}
	if got := third.Session().ConnectorID; got != runtimeConnectorID {
		t.Fatalf("third ConnectorID = %q, want round-robin %q", got, runtimeConnectorID)
	}
	if !third.Release() {
		t.Fatal("third Release() = false")
	}
	fourth, err := registry.AcquireConnector(runtimeTunnelID)
	if err != nil {
		t.Fatalf("AcquireConnector(fourth) error = %v", err)
	}
	if got := fourth.Session().ConnectorID; got != runtimeConnectorIDTwo {
		t.Fatalf("fourth ConnectorID = %q, want round-robin %q", got, runtimeConnectorIDTwo)
	}
	if !fourth.Release() {
		t.Fatal("fourth Release() = false")
	}
}

func TestRegistryConnectorLeaseIsFencedAcrossReconnectAndClear(t *testing.T) {
	registry := newRegistry(sessionGenerator(1))
	oldSession, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatalf("installAuthenticated(old) error = %v", err)
	}
	secondSession, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorIDTwo)
	if err != nil {
		t.Fatalf("installAuthenticated(second connector) error = %v", err)
	}
	oldLease, err := registry.AcquireConnector(runtimeTunnelID)
	if err != nil {
		t.Fatalf("AcquireConnector(old) error = %v", err)
	}
	if count := connectorActiveCount(registry, oldSession); count != 1 {
		t.Fatalf("old active count = %d, want 1", count)
	}

	newSession, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatalf("installAuthenticated(reconnect) error = %v", err)
	}
	if count := connectorActiveCount(registry, oldSession); count != 1 {
		t.Fatalf("old generation tombstone count = %d, want 1", count)
	}
	leastActive, err := registry.AcquireConnector(runtimeTunnelID)
	if err != nil {
		t.Fatalf("AcquireConnector(least active after reconnect) error = %v", err)
	}
	if leastActive.Session() != secondSession {
		t.Fatalf("least-active Session after reconnect = %#v, want %#v", leastActive.Session(), secondSession)
	}
	if !oldLease.Release() || oldLease.Release() {
		t.Fatal("old generation Release() did not decrement exactly once")
	}
	if _, exists := connectorActiveValue(registry, oldSession); exists {
		t.Fatal("old generation tombstone remained after exact Release reached zero")
	}
	if !leastActive.Release() {
		t.Fatal("least-active second Connector Release() = false")
	}
	recovered, err := registry.AcquireConnector(runtimeTunnelID)
	if err != nil {
		t.Fatalf("AcquireConnector(after old Release) error = %v", err)
	}
	if recovered.Session() != newSession {
		t.Fatalf("recovered Session = %#v, want reconnected %#v", recovered.Session(), newSession)
	}
	if !registry.ClearIfCurrent(newSession) {
		t.Fatal("ClearIfCurrent(reconnected) = false")
	}
	if count := connectorActiveCount(registry, newSession); count != 1 {
		t.Fatalf("cleared current tombstone count = %d, want 1", count)
	}
	remaining, err := registry.AcquireConnector(runtimeTunnelID)
	if err != nil {
		t.Fatalf("AcquireConnector(after first Connector Clear) error = %v", err)
	}
	if remaining.Session() != secondSession {
		t.Fatalf("AcquireConnector selected cleared Session = %#v", remaining.Session())
	}
	if !recovered.Release() {
		t.Fatal("Release() after Clear did not retire the ActiveWork tombstone")
	}
	if _, exists := connectorActiveValue(registry, newSession); exists {
		t.Fatal("cleared Session tombstone remained after Release reached zero")
	}
	if !remaining.Release() {
		t.Fatal("remaining Connector Release() = false")
	}
	if !registry.ClearIfCurrent(secondSession) {
		t.Fatal("ClearIfCurrent(second) = false")
	}
	if _, err := registry.AcquireConnector(runtimeTunnelID); !errors.Is(err, ErrNoAvailableConnector) {
		t.Fatalf("AcquireConnector(after all Clear) error = %v, want ErrNoAvailableConnector", err)
	}
}

func TestRegistryConnectorLeaseRejectsDuplicateRelease(t *testing.T) {
	registry := newRegistry(sessionGenerator(1))
	session, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatalf("installAuthenticated() error = %v", err)
	}
	lease, err := registry.AcquireConnector(runtimeTunnelID)
	if err != nil {
		t.Fatalf("AcquireConnector() error = %v", err)
	}
	copiedLease := *lease
	if !lease.Release() {
		t.Fatal("first Release() = false")
	}
	if lease.Release() || copiedLease.Release() {
		t.Fatal("duplicate Release() = true")
	}
	if _, exists := connectorActiveValue(registry, session); exists {
		t.Fatal("duplicate Release() left or underflowed an active counter")
	}
}

func TestRegistryConcurrentAcquireAndRelease(t *testing.T) {
	const acquisitions = 256
	registry := newRegistry(sessionGenerator(1))
	if _, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID); err != nil {
		t.Fatalf("installAuthenticated(first) error = %v", err)
	}
	if _, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorIDTwo); err != nil {
		t.Fatalf("installAuthenticated(second) error = %v", err)
	}

	leases := make(chan *ConnectorLease, acquisitions)
	errorsCh := make(chan error, acquisitions)
	var acquireWait sync.WaitGroup
	for range acquisitions {
		acquireWait.Add(1)
		go func() {
			defer acquireWait.Done()
			lease, err := registry.AcquireConnector(runtimeTunnelID)
			if err != nil {
				errorsCh <- err
				return
			}
			leases <- lease
		}()
	}
	acquireWait.Wait()
	close(leases)
	close(errorsCh)
	for err := range errorsCh {
		t.Errorf("AcquireConnector() error = %v", err)
	}

	selected := make(map[string]int, 2)
	allLeases := make([]*ConnectorLease, 0, acquisitions)
	for lease := range leases {
		selected[lease.Session().ConnectorID]++
		allLeases = append(allLeases, lease)
	}
	if selected[runtimeConnectorID] != acquisitions/2 || selected[runtimeConnectorIDTwo] != acquisitions/2 {
		t.Fatalf("concurrent selection = %#v, want even least-active distribution", selected)
	}

	var releaseWait sync.WaitGroup
	for _, lease := range allLeases {
		for range 2 {
			releaseWait.Add(1)
			go func(lease *ConnectorLease) {
				defer releaseWait.Done()
				lease.Release()
			}(lease)
		}
	}
	releaseWait.Wait()
	if entries := connectorActiveEntries(registry, runtimeTunnelID); entries != 0 {
		t.Fatalf("active entries after concurrent releases = %d, want 0", entries)
	}
}

var errRandomSourceFailed = errors.New("random source failed")

func connectorActiveCount(registry *Registry, session Session) uint64 {
	count, _ := connectorActiveValue(registry, session)
	return count
}

func connectorActiveValue(registry *Registry, session Session) (uint64, bool) {
	runtime, err := registry.Tunnel(session.TunnelID)
	if err != nil {
		return 0, false
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	count, exists := runtime.connectorActive[session]
	return count, exists
}

func connectorActiveEntries(registry *Registry, tunnelID string) int {
	runtime, err := registry.Tunnel(tunnelID)
	if err != nil {
		return 0
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return len(runtime.connectorActive)
}

// installAuthenticated 仅供 Registry 单元测试跳过可回滚的网络写入阶段。
func installAuthenticated(registry *Registry, tunnelID, connectorID string) (Session, error) {
	pending, err := registry.ReserveAuthenticated(tunnelID, connectorID)
	if err != nil {
		return Session{}, err
	}
	return registry.CommitAuthenticated(pending)
}

func sessionGenerator(start int) func() (string, error) {
	var mu sync.Mutex
	next := start
	return func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		id := fmt.Sprintf("sess_%026d", next)
		next++
		return id, nil
	}
}
