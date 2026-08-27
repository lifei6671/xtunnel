package sessionruntime

import (
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/controlsession"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
)

const (
	testServiceID     = "svc_01J00000000000000000000000"
	testServiceIDTwo  = "svc_01J00000000000000000000001"
	testServiceIDGone = "svc_01J00000000000000000000002"
)

func TestAcceptHealthBatchValidatesBeforeMutationAndFencesGeneration(t *testing.T) {
	managed := healthReadySession(7, testHealthRequirement(7, time.Second))
	managed.serviceRequirements[testServiceIDTwo] = testHealthRequirement(8, time.Second)

	invalid := healthBatch(1,
		healthItem(testServiceID, 7, protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY, 20),
		&protocolv1.ServiceHealth{ServiceId: "invalid", ServiceRevision: 7},
	)
	if err := validateHealthBatch(invalid); err == nil {
		t.Fatal("validateHealthBatch() accepted an invalid item")
	}
	if managed.healthGeneration != 0 || len(managed.serviceHealth) != 0 {
		t.Fatal("invalid batch partially mutated Health state")
	}

	unknownService := "svc_01J00000000000000000000009"
	valid := healthBatch(2,
		healthItem(testServiceID, 7, protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY, 20),
		healthItem(testServiceIDTwo, 7, protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY, 21),
		healthItem(unknownService, 7, protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY, 22),
	)
	if err := validateHealthBatch(valid); err != nil {
		t.Fatalf("validateHealthBatch() error = %v", err)
	}
	managed.acceptHealthBatch(valid, 3*time.Second)
	if managed.healthGeneration != 2 || len(managed.serviceHealth) != 1 {
		t.Fatalf("accepted Health state = generation %d items %d, want 2/1", managed.healthGeneration, len(managed.serviceHealth))
	}
	got := managed.serviceHealth[testServiceID]
	if got.status != protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY || got.checkedAtMS != 20 || got.receivedAt != 3*time.Second {
		t.Fatalf("stored Health = %#v, want current matching item", got)
	}

	managed.acceptHealthBatch(healthBatch(1,
		healthItem(testServiceID, 7, protocolv1.HealthStatus_HEALTH_STATUS_UNHEALTHY, 99),
	), 4*time.Second)
	if got := managed.serviceHealth[testServiceID]; got.status != protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY || got.checkedAtMS != 20 {
		t.Fatalf("older generation overwrote Health: %#v", got)
	}

	managed.acceptHealthBatch(healthBatch(3,
		healthItem(testServiceID, 7, protocolv1.HealthStatus_HEALTH_STATUS_UNHEALTHY, 1),
	), 5*time.Second)
	if got := managed.serviceHealth[testServiceID]; got.status != protocolv1.HealthStatus_HEALTH_STATUS_UNHEALTHY || got.checkedAtMS != 1 {
		t.Fatalf("checked_at incorrectly controlled ordering: %#v", got)
	}
}

func TestHealthBatchItemLimitIsAtomic(t *testing.T) {
	registry := serverruntime.NewRegistry()
	session := commitSession(t, registry, testTunnelID, testConnectorID)
	managed := healthReadySession(7, testHealthRequirement(7, time.Second))
	managed.session = session
	managed.serviceRequirements = make(map[string]serviceRequirement, maxAcceptedHealthBatchItems+1)
	items := make([]*protocolv1.ServiceHealth, 0, maxAcceptedHealthBatchItems+1)
	for index := range maxAcceptedHealthBatchItems + 1 {
		serviceID := indexedServiceID(index)
		managed.serviceRequirements[serviceID] = testHealthRequirement(7, time.Second)
		items = append(items, healthItem(serviceID, 7, protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY, uint64(index)))
	}
	manager := &Manager{
		registry: registry, startedAt: time.Now(),
		bySession: map[string]*managedSession{session.SessionID: managed},
	}

	accepted := controlsession.Inbound{Envelope: &protocolv1.ControlEnvelope{
		Payload: &protocolv1.ControlEnvelope_ServiceHealthBatch{ServiceHealthBatch: healthBatch(1, items[:maxAcceptedHealthBatchItems]...)},
	}}
	if err := manager.handleHealthBatch(managed, accepted); err != nil {
		t.Fatalf("handleHealthBatch(128 items) error = %v", err)
	}
	if managed.healthGeneration != 1 || len(managed.serviceHealth) != maxAcceptedHealthBatchItems {
		t.Fatalf("128-item batch state = generation %d items %d", managed.healthGeneration, len(managed.serviceHealth))
	}

	for _, item := range items {
		item.Status = protocolv1.HealthStatus_HEALTH_STATUS_UNHEALTHY
	}
	rejected := controlsession.Inbound{Envelope: &protocolv1.ControlEnvelope{
		Payload: &protocolv1.ControlEnvelope_ServiceHealthBatch{ServiceHealthBatch: healthBatch(2, items...)},
	}}
	if err := manager.handleHealthBatch(managed, rejected); err == nil {
		t.Fatal("handleHealthBatch(129 items) accepted an oversized batch")
	}
	if managed.healthGeneration != 1 || len(managed.serviceHealth) != maxAcceptedHealthBatchItems {
		t.Fatalf("oversized batch mutated state = generation %d items %d", managed.healthGeneration, len(managed.serviceHealth))
	}
	for serviceID, health := range managed.serviceHealth {
		if health.status != protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY {
			t.Fatalf("oversized batch partially changed %s to %s", serviceID, health.status)
		}
	}
}

func TestSnapshotRequirementsOnlyInvalidateChangedServices(t *testing.T) {
	managed := &managedSession{serviceHealth: map[string]serviceHealthState{
		testServiceID:     {status: protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY, serviceRevision: 7},
		testServiceIDTwo:  {status: protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY, serviceRevision: 7},
		testServiceIDGone: {status: protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY, serviceRevision: 7},
	}}
	first := healthSnapshot(7,
		healthService(testServiceID, 7, true, protocolv1.HealthType_HEALTH_TYPE_TCP, time.Second),
		healthService(testServiceIDTwo, 7, true, protocolv1.HealthType_HEALTH_TYPE_HTTP, time.Second),
		healthService(testServiceIDGone, 7, true, protocolv1.HealthType_HEALTH_TYPE_TCP, time.Second),
	)
	if _, err := managed.stageSnapshot(&snapshotCandidate{snapshot: first, revision: 7}); err != nil {
		t.Fatalf("stageSnapshot(first) error = %v", err)
	}
	// 初次发布没有可继承的权威要求，全部旧状态必须被清除。
	if len(managed.serviceHealth) != 0 {
		t.Fatalf("first requirement install retained %d Health items", len(managed.serviceHealth))
	}
	managed.serviceHealth[testServiceID] = serviceHealthState{status: protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY, serviceRevision: 7}
	managed.serviceHealth[testServiceIDTwo] = serviceHealthState{status: protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY, serviceRevision: 7}
	managed.serviceHealth[testServiceIDGone] = serviceHealthState{status: protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY, serviceRevision: 7}

	second := healthSnapshot(8,
		healthService(testServiceID, 7, true, protocolv1.HealthType_HEALTH_TYPE_TCP, time.Second),
		healthService(testServiceIDTwo, 7, true, protocolv1.HealthType_HEALTH_TYPE_HTTP, 2*time.Second),
	)
	if _, err := managed.stageSnapshot(&snapshotCandidate{snapshot: second, revision: 8}); err != nil {
		t.Fatalf("stageSnapshot(second) error = %v", err)
	}
	if _, exists := managed.serviceHealth[testServiceID]; !exists {
		t.Fatal("unchanged Service lost current Health")
	}
	if _, exists := managed.serviceHealth[testServiceIDTwo]; exists {
		t.Fatal("Health Policy change retained stale Health")
	}
	if _, exists := managed.serviceHealth[testServiceIDGone]; exists {
		t.Fatal("removed Service retained stale Health")
	}
}

func TestManagerEligibleAppliesRevisionHealthAndFreshnessGates(t *testing.T) {
	registry := serverruntime.NewRegistry()
	session := commitSession(t, registry, testTunnelID, testConnectorID)
	manager := &Manager{registry: registry, startedAt: time.Now(), bySession: make(map[string]*managedSession)}
	managed := healthReadySession(7, testHealthRequirement(7, time.Second))
	managed.session = session
	manager.bySession[session.SessionID] = managed
	if _, observed := registry.ObserveConnected(session, serverruntime.ConnectorMetadata{}); !observed {
		t.Fatal("ObserveConnected() rejected current Session")
	}

	now := time.Since(manager.startedAt)
	managed.serviceHealth[testServiceID] = serviceHealthState{
		status: protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY, serviceRevision: 7, receivedAt: now,
	}
	manager.publishEligibility(managed)
	if !manager.Eligible(session, testServiceID) {
		t.Fatal("Eligible() rejected fresh HEALTHY current Revision")
	}

	managed.configMu.Lock()
	requirement := managed.serviceRequirements[testServiceID]
	requirement.requiredRevision = 8
	managed.serviceRequirements[testServiceID] = requirement
	managed.configMu.Unlock()
	manager.publishEligibility(managed)
	if manager.Eligible(session, testServiceID) {
		t.Fatal("Eligible() accepted an unobserved required Revision")
	}

	managed.configMu.Lock()
	requirement.requiredRevision = 7
	requirement.health = &protocolv1.HealthCheckConfig{Type: protocolv1.HealthType_HEALTH_TYPE_DISABLED}
	managed.serviceRequirements[testServiceID] = requirement
	managed.serviceHealth[testServiceID] = serviceHealthState{}
	managed.configMu.Unlock()
	manager.publishEligibility(managed)
	if !manager.Eligible(session, testServiceID) {
		t.Fatal("Eligible() rejected a Health-disabled Service")
	}

	managed.configMu.Lock()
	requirement.enabled = false
	managed.serviceRequirements[testServiceID] = requirement
	managed.configMu.Unlock()
	manager.publishEligibility(managed)
	if manager.Eligible(session, testServiceID) {
		t.Fatal("Eligible() accepted a disabled Service")
	}

	replaced := session
	replaced.Generation++
	if manager.Eligible(replaced, testServiceID) {
		t.Fatal("Eligible() accepted a non-current Session identity")
	}
}

func TestHandleHealthBatchRequiresCurrentConfigReadySession(t *testing.T) {
	registry := serverruntime.NewRegistry()
	session := commitSession(t, registry, testTunnelID, testConnectorID)
	managed := healthReadySession(7, testHealthRequirement(7, time.Second))
	managed.session = session
	managed.configReady = false
	manager := &Manager{
		registry: registry, startedAt: time.Now(),
		bySession: map[string]*managedSession{session.SessionID: managed},
	}
	inbound := controlsession.Inbound{Envelope: &protocolv1.ControlEnvelope{
		Payload: &protocolv1.ControlEnvelope_ServiceHealthBatch{ServiceHealthBatch: healthBatch(1,
			healthItem(testServiceID, 7, protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY, 1),
		)},
	}}
	if err := manager.handleHealthBatch(managed, inbound); err != nil {
		t.Fatalf("handleHealthBatch(not ready) error = %v", err)
	}
	if managed.healthGeneration != 0 {
		t.Fatalf("not-ready Session advanced generation to %d", managed.healthGeneration)
	}

	managed.configReady = true
	replacement := commitSession(t, registry, testTunnelID, testConnectorID)
	if err := manager.handleHealthBatch(managed, inbound); err != nil {
		t.Fatalf("handleHealthBatch(non-current) error = %v", err)
	}
	if managed.healthGeneration != 0 {
		t.Fatalf("non-current Session advanced generation to %d", managed.healthGeneration)
	}

	managed = healthReadySession(7, testHealthRequirement(7, time.Second))
	managed.session = replacement
	manager.bySession = map[string]*managedSession{replacement.SessionID: managed}
	heartbeatTimer := time.NewTimer(time.Hour)
	defer heartbeatTimer.Stop()
	if _, err := manager.handleInbound(t.Context(), managed, heartbeatTimer, nil, inbound); err != nil {
		t.Fatalf("handleInbound(Health Batch) error = %v", err)
	}
	if managed.healthGeneration != 1 || managed.serviceHealth[testServiceID].status != protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY {
		t.Fatalf("handleInbound() did not publish Health: generation=%d state=%#v", managed.healthGeneration, managed.serviceHealth[testServiceID])
	}
}

func healthReadySession(observed uint64, requirement serviceRequirement) *managedSession {
	return &managedSession{
		configReady: true, hasObserved: true, observedRevision: observed,
		serviceRequirements: map[string]serviceRequirement{testServiceID: requirement},
		serviceHealth:       make(map[string]serviceHealthState),
	}
}

func testHealthRequirement(revision uint64, interval time.Duration) serviceRequirement {
	return serviceRequirement{requiredRevision: revision, enabled: true, health: &protocolv1.HealthCheckConfig{
		Type: protocolv1.HealthType_HEALTH_TYPE_TCP, IntervalMs: uint32(interval / time.Millisecond),
	}}
}

func healthSnapshot(revision uint64, services ...*protocolv1.ServiceConfig) *protocolv1.TunnelSnapshot {
	return &protocolv1.TunnelSnapshot{TunnelId: testTunnelID, Revision: revision, Services: services}
}

func healthService(id string, revision uint64, enabled bool, healthType protocolv1.HealthType, interval time.Duration) *protocolv1.ServiceConfig {
	return &protocolv1.ServiceConfig{
		ServiceId: id, RequiredRevision: revision, Enabled: enabled,
		Health: &protocolv1.HealthCheckConfig{Type: healthType, IntervalMs: uint32(interval / time.Millisecond)},
	}
}

func healthBatch(generation uint64, items ...*protocolv1.ServiceHealth) *protocolv1.ServiceHealthBatch {
	return &protocolv1.ServiceHealthBatch{Generation: generation, Items: items}
}

func healthItem(id string, revision uint64, status protocolv1.HealthStatus, checkedAt uint64) *protocolv1.ServiceHealth {
	return &protocolv1.ServiceHealth{
		ServiceId: id, ServiceRevision: revision, Status: status, CheckedAtMs: checkedAt,
	}
}

func indexedServiceID(index int) string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	serviceID := []byte(testServiceID)
	serviceID[len(serviceID)-2] = alphabet[index/len(alphabet)]
	serviceID[len(serviceID)-1] = alphabet[index%len(alphabet)]
	return string(serviceID)
}
