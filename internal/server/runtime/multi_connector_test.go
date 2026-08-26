package runtime

import (
	"fmt"
	"sync"
	"testing"
)

func TestAcquireConnectorWhereAdvancesPastUnavailableRoundRobinCursor(t *testing.T) {
	registry := newRegistry(sessionGenerator(1))
	connectorIDs := multiConnectorIDs(3)
	for _, connectorID := range connectorIDs {
		if _, err := installAuthenticated(registry, runtimeTunnelID, connectorID); err != nil {
			t.Fatalf("installAuthenticated(%s) error = %v", connectorID, err)
		}
	}

	acquireFrom := func(eligible map[string]bool) *ConnectorLease {
		t.Helper()
		lease, err := registry.AcquireConnectorWhere(runtimeTunnelID, func(session Session) bool {
			return eligible[session.ConnectorID]
		})
		if err != nil {
			t.Fatalf("AcquireConnectorWhere() error = %v", err)
		}
		return lease
	}

	selected := make(map[string]int, len(connectorIDs))
	for iteration := range 32 {
		eligible := map[string]bool{connectorIDs[1]: true, connectorIDs[2]: true}
		want := connectorIDs[1]
		if iteration%2 == 1 {
			eligible = map[string]bool{connectorIDs[0]: true, connectorIDs[2]: true}
			want = connectorIDs[2]
		}
		lease := acquireFrom(eligible)
		got := lease.Session().ConnectorID
		selected[got]++
		if got != want {
			t.Fatalf("selection %d ConnectorID = %q, want ordered successor %q", iteration, got, want)
		}
		if !lease.Release() {
			t.Fatalf("selection %d ConnectorLease.Release() = false", iteration)
		}
	}
	if selected[connectorIDs[2]] != 16 {
		t.Fatalf("continuously eligible Connector %q selections = %d, want 16", connectorIDs[2], selected[connectorIDs[2]])
	}
}

func TestRegistryMultiConnectorGenerationIsolationUnderChurn(t *testing.T) {
	tests := []struct {
		name             string
		connectors       int
		replacementsEach int
	}{
		{name: "three connectors", connectors: 3, replacementsEach: 16},
		{name: "eight connectors", connectors: 8, replacementsEach: 8},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := newRegistry(sessionGenerator(1))
			connectorIDs := multiConnectorIDs(test.connectors)
			initialSessions := make([]Session, 0, test.connectors)
			for _, connectorID := range connectorIDs {
				session, err := installAuthenticated(registry, runtimeTunnelID, connectorID)
				if err != nil {
					t.Fatalf("installAuthenticated(%s) error = %v", connectorID, err)
				}
				if session.Generation != 1 {
					t.Fatalf("initial generation for %s = %d, want 1", connectorID, session.Generation)
				}
				initialSessions = append(initialSessions, session)
			}

			type replacementResult struct {
				session Session
				err     error
			}
			start := make(chan struct{})
			results := make(chan replacementResult, test.connectors*test.replacementsEach)
			var replacements sync.WaitGroup
			for _, connectorID := range connectorIDs {
				replacements.Add(1)
				go func() {
					defer replacements.Done()
					<-start
					for range test.replacementsEach {
						session, err := installAuthenticated(registry, runtimeTunnelID, connectorID)
						results <- replacementResult{session: session, err: err}
						if err != nil {
							return
						}
					}
				}()
			}
			close(start)
			replacements.Wait()
			close(results)

			generations := make(map[string]map[uint64]struct{}, test.connectors)
			for result := range results {
				if result.err != nil {
					t.Fatalf("concurrent installAuthenticated() error = %v", result.err)
				}
				byConnector := generations[result.session.ConnectorID]
				if byConnector == nil {
					byConnector = make(map[uint64]struct{}, test.replacementsEach)
					generations[result.session.ConnectorID] = byConnector
				}
				if _, exists := byConnector[result.session.Generation]; exists {
					t.Fatalf("Connector %s repeated generation %d", result.session.ConnectorID, result.session.Generation)
				}
				byConnector[result.session.Generation] = struct{}{}
			}

			wantGeneration := uint64(test.replacementsEach + 1)
			for _, oldSession := range initialSessions {
				if registry.ClearIfCurrent(oldSession) {
					t.Fatalf("old generation cleanup cleared current Connector %s", oldSession.ConnectorID)
				}
			}
			for _, connectorID := range connectorIDs {
				if got := len(generations[connectorID]); got != test.replacementsEach {
					t.Fatalf("Connector %s replacement count = %d, want %d", connectorID, got, test.replacementsEach)
				}
				for generation := uint64(2); generation <= wantGeneration; generation++ {
					if _, exists := generations[connectorID][generation]; !exists {
						t.Fatalf("Connector %s missing generation %d", connectorID, generation)
					}
				}
				current, exists := registry.Current(runtimeTunnelID, connectorID)
				if !exists || current.Generation != wantGeneration {
					t.Fatalf("Current(%s) = %#v, %v, want generation %d", connectorID, current, exists, wantGeneration)
				}
				if !registry.ClearIfCurrent(current) {
					t.Fatalf("ClearIfCurrent(%s) = false", connectorID)
				}
			}
			assertRegistryConnectorStateReleased(t, registry, runtimeTunnelID)
		})
	}
}

func TestRegistryMultiConnectorConcurrentLeaseReleaseDoesNotLeakCounters(t *testing.T) {
	tests := []struct {
		name         string
		connectors   int
		acquisitions int
	}{
		{name: "three connectors", connectors: 3, acquisitions: 240},
		{name: "eight connectors", connectors: 8, acquisitions: 256},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := newRegistry(sessionGenerator(1))
			connectorIDs := multiConnectorIDs(test.connectors)
			for _, connectorID := range connectorIDs {
				if _, err := installAuthenticated(registry, runtimeTunnelID, connectorID); err != nil {
					t.Fatalf("installAuthenticated(%s) error = %v", connectorID, err)
				}
			}

			start := make(chan struct{})
			leases := make(chan *ConnectorLease, test.acquisitions)
			errorsCh := make(chan error, test.acquisitions)
			var acquisitions sync.WaitGroup
			for range test.acquisitions {
				acquisitions.Add(1)
				go func() {
					defer acquisitions.Done()
					<-start
					lease, err := registry.AcquireConnector(runtimeTunnelID)
					if err != nil {
						errorsCh <- err
						return
					}
					leases <- lease
				}()
			}
			close(start)
			acquisitions.Wait()
			close(leases)
			close(errorsCh)
			for err := range errorsCh {
				t.Errorf("AcquireConnector() error = %v", err)
			}

			selected := make(map[string]int, test.connectors)
			allLeases := make([]*ConnectorLease, 0, test.acquisitions)
			for lease := range leases {
				selected[lease.Session().ConnectorID]++
				allLeases = append(allLeases, lease)
			}
			minimum, maximum := test.acquisitions, 0
			for _, connectorID := range connectorIDs {
				count := selected[connectorID]
				if count < minimum {
					minimum = count
				}
				if count > maximum {
					maximum = count
				}
			}
			if maximum-minimum > 1 {
				t.Fatalf("concurrent selection = %#v, want maximum distribution difference 1", selected)
			}

			releaseResults := make(chan bool, len(allLeases)*2)
			var releases sync.WaitGroup
			for _, lease := range allLeases {
				for range 2 {
					releases.Add(1)
					go func() {
						defer releases.Done()
						releaseResults <- lease.Release()
					}()
				}
			}
			releases.Wait()
			close(releaseResults)
			successfulReleases := 0
			for released := range releaseResults {
				if released {
					successfulReleases++
				}
			}
			if successfulReleases != len(allLeases) {
				t.Fatalf("successful releases = %d, want exactly %d", successfulReleases, len(allLeases))
			}
			if entries := connectorActiveEntries(registry, runtimeTunnelID); entries != 0 {
				t.Fatalf("active counter entries after release = %d, want 0", entries)
			}

			for _, connectorID := range connectorIDs {
				current, exists := registry.Current(runtimeTunnelID, connectorID)
				if !exists || !registry.ClearIfCurrent(current) {
					t.Fatalf("clear current Connector %s = %#v, %v", connectorID, current, exists)
				}
			}
			assertRegistryConnectorStateReleased(t, registry, runtimeTunnelID)
		})
	}
}

func TestRegistryReplacementKeepsOldGenerationInConnectorLoad(t *testing.T) {
	registry := newRegistry(sessionGenerator(1))
	connectorIDs := multiConnectorIDs(3)
	initial := make(map[string]Session, len(connectorIDs))
	for _, connectorID := range connectorIDs {
		session, err := installAuthenticated(registry, runtimeTunnelID, connectorID)
		if err != nil {
			t.Fatalf("installAuthenticated(%s) error = %v", connectorID, err)
		}
		initial[connectorID] = session
	}

	oldLease, err := registry.AcquireConnector(runtimeTunnelID)
	if err != nil {
		t.Fatalf("AcquireConnector(old generation) error = %v", err)
	}
	if got := oldLease.Session(); got != initial[connectorIDs[0]] {
		t.Fatalf("old generation lease = %#v, want %#v", got, initial[connectorIDs[0]])
	}
	replacement, err := installAuthenticated(registry, runtimeTunnelID, connectorIDs[0])
	if err != nil {
		t.Fatalf("installAuthenticated(replacement) error = %v", err)
	}
	if count := connectorActiveCount(registry, initial[connectorIDs[0]]); count != 1 {
		t.Fatalf("old generation active count = %d, want 1", count)
	}
	runtime := registry.tunnel(runtimeTunnelID, false)
	runtime.mu.Lock()
	_, retired := runtime.retired[initial[connectorIDs[0]]]
	runtime.mu.Unlock()
	if !retired {
		t.Fatal("old generation with an active Lease was not retained as a tombstone")
	}

	leastActive, err := registry.AcquireConnector(runtimeTunnelID)
	if err != nil {
		t.Fatalf("AcquireConnector(least active) error = %v", err)
	}
	if got := leastActive.Session().ConnectorID; got != connectorIDs[1] {
		t.Fatalf("least-active ConnectorID = %q, want %q while reconnected %q still carries old load", got, connectorIDs[1], replacement.ConnectorID)
	}
	if !leastActive.Release() || !oldLease.Release() {
		t.Fatal("Connector leases did not release exactly once")
	}
	if _, exists := connectorActiveValue(registry, initial[connectorIDs[0]]); exists {
		t.Fatal("old generation counter remained after final Release")
	}

	for _, connectorID := range connectorIDs {
		current, exists := registry.Current(runtimeTunnelID, connectorID)
		if !exists || !registry.ClearIfCurrent(current) {
			t.Fatalf("clear current Connector %s = %#v, %v", connectorID, current, exists)
		}
	}
	assertRegistryConnectorStateReleased(t, registry, runtimeTunnelID)
}

func multiConnectorIDs(count int) []string {
	connectorIDs := make([]string, count)
	for index := range count {
		connectorIDs[index] = fmt.Sprintf("con_01J%023d", index)
	}
	return connectorIDs
}

func assertRegistryConnectorStateReleased(t *testing.T, registry *Registry, tunnelID string) {
	t.Helper()
	runtime := registry.tunnel(tunnelID, false)
	if runtime == nil {
		t.Fatal("TunnelRuntime disappeared before final state inspection")
	}
	runtime.mu.Lock()
	current := len(runtime.current)
	activeCounters := len(runtime.connectorActive)
	retired := len(runtime.retired)
	pending := len(runtime.pending)
	currentLimits := len(runtime.currentConnectorLimits)
	pendingLimits := len(runtime.pendingConnectorLimits)
	runtime.mu.Unlock()
	if current != 0 || activeCounters != 0 || retired != 0 || pending != 0 || currentLimits != 0 || pendingLimits != 0 {
		t.Fatalf(
			"final TunnelRuntime state = current:%d active:%d retired:%d pending:%d current_limits:%d pending_limits:%d, want all zero",
			current, activeCounters, retired, pending, currentLimits, pendingLimits,
		)
	}
	registry.sessionIDsMu.Lock()
	sessionIDs := len(registry.sessionIDs)
	registry.sessionIDsMu.Unlock()
	if sessionIDs != 0 {
		t.Fatalf("reserved Session IDs after cleanup = %d, want 0", sessionIDs)
	}
}
