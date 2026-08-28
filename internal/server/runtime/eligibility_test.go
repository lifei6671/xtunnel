package runtime

import (
	"errors"
	"testing"
	"time"
)

const runtimeEligibilityServiceID = "svc_01J00000000000000000000000"

func TestEligibilityAppliesRevisionHealthAndStrictFreshnessGates(t *testing.T) {
	base := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	now := base
	registry := newRegistryWithClock(sessionGenerator(1), func() time.Time { return now })
	session, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatal(err)
	}
	if _, observed := registry.ObserveConnected(session, ConnectorMetadata{}); !observed {
		t.Fatal("ObserveConnected() rejected current Session")
	}

	baseService := ServiceEligibility{
		RequiredRevision: 7, HealthRevision: 7, Enabled: true,
		HealthHealthy: true, HealthyUntil: base.Add(2 * time.Second),
	}
	tests := []struct {
		name     string
		now      time.Time
		state    SessionEligibility
		eligible bool
	}{
		{
			name: "fresh healthy current revision", now: base,
			state: SessionEligibility{ConfigReady: true, HasObserved: true, ObservedRevision: 7,
				Services: map[string]ServiceEligibility{runtimeEligibilityServiceID: baseService}},
			eligible: true,
		},
		{
			name: "strict stale boundary remains eligible", now: base.Add(2 * time.Second),
			state: SessionEligibility{ConfigReady: true, HasObserved: true, ObservedRevision: 7,
				Services: map[string]ServiceEligibility{runtimeEligibilityServiceID: baseService}},
			eligible: true,
		},
		{
			name: "past stale boundary", now: base.Add(2*time.Second + time.Nanosecond),
			state: SessionEligibility{ConfigReady: true, HasObserved: true, ObservedRevision: 7,
				Services: map[string]ServiceEligibility{runtimeEligibilityServiceID: baseService}},
		},
		{
			name: "unknown health", now: base,
			state: SessionEligibility{ConfigReady: true, HasObserved: true, ObservedRevision: 7,
				Services: map[string]ServiceEligibility{runtimeEligibilityServiceID: func() ServiceEligibility {
					service := baseService
					service.HealthHealthy = false
					return service
				}()}},
		},
		{
			name: "old health revision", now: base,
			state: SessionEligibility{ConfigReady: true, HasObserved: true, ObservedRevision: 7,
				Services: map[string]ServiceEligibility{runtimeEligibilityServiceID: func() ServiceEligibility {
					service := baseService
					service.HealthRevision = 6
					return service
				}()}},
		},
		{
			name: "required revision not observed", now: base,
			state: SessionEligibility{ConfigReady: true, HasObserved: true, ObservedRevision: 6,
				Services: map[string]ServiceEligibility{runtimeEligibilityServiceID: baseService}},
		},
		{
			name: "health disabled", now: base,
			state: SessionEligibility{ConfigReady: true, HasObserved: true, ObservedRevision: 7,
				Services: map[string]ServiceEligibility{runtimeEligibilityServiceID: {
					RequiredRevision: 7, Enabled: true, HealthDisabled: true,
				}}},
			eligible: true,
		},
		{
			name: "service disabled", now: base,
			state: SessionEligibility{ConfigReady: true, HasObserved: true, ObservedRevision: 7,
				Services: map[string]ServiceEligibility{runtimeEligibilityServiceID: {
					RequiredRevision: 7, Enabled: false, HealthDisabled: true,
				}}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now = test.now
			if !registry.PublishEligibility(session, test.state) {
				t.Fatal("PublishEligibility() rejected current Session")
			}
			if got := registry.Eligible(session, runtimeEligibilityServiceID); got != test.eligible {
				t.Fatalf("Eligible() = %t, want %t", got, test.eligible)
			}
		})
	}
}

func TestEligibilityPinsServiceRequiredRevisionWithoutPinningObservedRevision(t *testing.T) {
	registry := newRegistryWithClock(sessionGenerator(1), time.Now)
	session, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatal(err)
	}
	if _, observed := registry.ObserveConnected(session, ConnectorMetadata{}); !observed {
		t.Fatal("ObserveConnected() rejected current Session")
	}
	if !registry.PublishEligibility(session, SessionEligibility{
		ConfigReady: true, HasObserved: true, ObservedRevision: 8,
		Services: map[string]ServiceEligibility{runtimeEligibilityServiceID: {
			RequiredRevision: 7, Enabled: true, HealthDisabled: true,
		}},
	}) {
		t.Fatal("PublishEligibility() rejected current Session")
	}

	if !registry.EligibleAtRevision(session, runtimeEligibilityServiceID, 7) {
		t.Fatal("Eligible() rejected exact Service revision when a newer Tunnel revision was observed")
	}
	if registry.EligibleAtRevision(session, runtimeEligibilityServiceID, 8) {
		t.Fatal("Eligible() accepted a different Service revision")
	}
	if _, eligible := registry.WatchEligibilityAtRevision(session, runtimeEligibilityServiceID, 7); !eligible {
		t.Fatal("WatchEligibility() rejected the exact Service revision")
	}
	if _, eligible := registry.WatchEligibilityAtRevision(session, runtimeEligibilityServiceID, 8); eligible {
		t.Fatal("WatchEligibility() accepted a different Service revision")
	}
	lease, err := registry.AcquireEligibleConnectorAtRevisionWhere(
		runtimeTunnelID, runtimeEligibilityServiceID, 7, func(Session) bool { return true },
	)
	if err != nil {
		t.Fatalf("AcquireEligibleConnectorWhere(exact revision) error = %v", err)
	}
	lease.Release()
	if _, err := registry.AcquireEligibleConnectorAtRevisionWhere(
		runtimeTunnelID, runtimeEligibilityServiceID, 8, func(Session) bool { return true },
	); !errors.Is(err, ErrNoAvailableConnector) {
		t.Fatalf("AcquireEligibleConnectorWhere(different revision) error = %v, want ErrNoAvailableConnector", err)
	}
}

func TestEligibilityTreatsZeroAsExactRevision(t *testing.T) {
	registry := newRegistryWithClock(sessionGenerator(1), time.Now)
	session, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatal(err)
	}
	if _, observed := registry.ObserveConnected(session, ConnectorMetadata{}); !observed {
		t.Fatal("ObserveConnected() rejected current Session")
	}
	publish := func(requiredRevision uint64) {
		t.Helper()
		if !registry.PublishEligibility(session, SessionEligibility{
			ConfigReady: true, HasObserved: true, ObservedRevision: 1,
			Services: map[string]ServiceEligibility{runtimeEligibilityServiceID: {
				RequiredRevision: requiredRevision, Enabled: true, HealthDisabled: true,
			}},
		}) {
			t.Fatal("PublishEligibility() rejected current Session")
		}
	}

	publish(1)
	if !registry.Eligible(session, runtimeEligibilityServiceID) {
		t.Fatal("Eligible() rejected the current Service revision")
	}
	if registry.EligibleAtRevision(session, runtimeEligibilityServiceID, 0) {
		t.Fatal("EligibleAtRevision(0) accepted Service revision 1")
	}
	if _, eligible := registry.WatchEligibilityAtRevision(session, runtimeEligibilityServiceID, 0); eligible {
		t.Fatal("WatchEligibilityAtRevision(0) accepted Service revision 1")
	}
	if _, err := registry.AcquireEligibleConnectorAtRevisionWhere(
		runtimeTunnelID, runtimeEligibilityServiceID, 0, func(Session) bool { return true },
	); !errors.Is(err, ErrNoAvailableConnector) {
		t.Fatalf("AcquireEligibleConnectorAtRevisionWhere(0) error = %v, want ErrNoAvailableConnector", err)
	}

	publish(0)
	if !registry.EligibleAtRevision(session, runtimeEligibilityServiceID, 0) {
		t.Fatal("EligibleAtRevision(0) rejected Service revision 0")
	}
	if _, eligible := registry.WatchEligibilityAtRevision(session, runtimeEligibilityServiceID, 0); !eligible {
		t.Fatal("WatchEligibilityAtRevision(0) rejected Service revision 0")
	}
	lease, err := registry.AcquireEligibleConnectorAtRevisionWhere(
		runtimeTunnelID, runtimeEligibilityServiceID, 0, func(Session) bool { return true },
	)
	if err != nil {
		t.Fatalf("AcquireEligibleConnectorAtRevisionWhere(0) error = %v", err)
	}
	lease.Release()
}

func TestServiceConfigObservedSeparatesRevisionVisibilityFromHealthAndCapacity(t *testing.T) {
	registry := newRegistryWithClock(sessionGenerator(1), time.Now)
	session, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatal(err)
	}
	if registry.ServiceConfigObserved(runtimeTunnelID, runtimeEligibilityServiceID, 7) {
		t.Fatal("ServiceConfigObserved() accepted unpublished config")
	}
	state := SessionEligibility{
		ConfigReady: true, HasObserved: true, ObservedRevision: 7,
		Services: map[string]ServiceEligibility{runtimeEligibilityServiceID: {
			RequiredRevision: 7,
			// Health 仍为 UNKNOWN 且 Service disabled；本查询只裁决配置是否已观察，
			// 不能把可用性门禁混入稳定错误码判断。
			Enabled: false,
		}},
	}
	if !registry.PublishEligibility(session, state) {
		t.Fatal("PublishEligibility() rejected current Session")
	}
	if !registry.ServiceConfigObserved(runtimeTunnelID, runtimeEligibilityServiceID, 7) {
		t.Fatal("ServiceConfigObserved() rejected observed revision")
	}
	if registry.ServiceConfigObserved(runtimeTunnelID, runtimeEligibilityServiceID, 8) {
		t.Fatal("ServiceConfigObserved() accepted newer unobserved revision")
	}
}

func TestEligibilityCurrentFencingAndRollbackWakeWaiters(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	registry := newRegistryWithClock(sessionGenerator(1), func() time.Time { return now })
	oldSession, err := installAuthenticated(registry, runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatal(err)
	}
	if _, observed := registry.ObserveConnected(oldSession, ConnectorMetadata{}); !observed {
		t.Fatal("ObserveConnected(old) rejected current Session")
	}
	state := SessionEligibility{
		ConfigReady: true, HasObserved: true, ObservedRevision: 1,
		Services: map[string]ServiceEligibility{runtimeEligibilityServiceID: {
			RequiredRevision: 1, Enabled: true, HealthDisabled: true,
		}},
	}
	if !registry.PublishEligibility(oldSession, state) {
		t.Fatal("PublishEligibility(old) rejected current Session")
	}
	watch, eligible := registry.WatchEligibility(oldSession, runtimeEligibilityServiceID)
	if !eligible {
		t.Fatal("WatchEligibility(old) rejected eligible Session")
	}

	pending, err := registry.ReserveAuthenticated(runtimeTunnelID, runtimeConnectorID)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := registry.InstallAuthenticated(pending)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-watch.Changed:
	default:
		t.Fatal("replacement did not wake old eligibility watcher")
	}
	if registry.Eligible(oldSession, runtimeEligibilityServiceID) {
		t.Fatal("Eligible() accepted replaced Session")
	}
	if registry.PublishEligibility(oldSession, state) {
		t.Fatal("old Session overwrote replacement eligibility")
	}
	if !replacement.Rollback() {
		t.Fatal("Rollback() did not restore old Session")
	}
	if !registry.Eligible(oldSession, runtimeEligibilityServiceID) {
		t.Fatal("rollback did not restore preserved old eligibility")
	}
}
