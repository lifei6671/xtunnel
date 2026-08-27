package runtime

import (
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
