package connector

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/configruntime"
	agenthealth "github.com/lifei6671/xtunnel/internal/agent/health"
	"github.com/lifei6671/xtunnel/internal/controlsession"
	"github.com/lifei6671/xtunnel/internal/identity"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
)

const (
	firstHealthServiceID  = "svc_01J00000000000000000000000"
	secondHealthServiceID = "svc_01J00000000000000000000001"
	thirdHealthServiceID  = "svc_01J00000000000000000000002"
	fourthHealthServiceID = "svc_01J00000000000000000000003"
)

type fakeHealthSource struct {
	states  map[string]agenthealth.State
	changed chan struct{}
}

type outboxHealthSession struct {
	outbox  *controlsession.Outbox
	inbound chan controlsession.Inbound
	done    chan struct{}
}

func (session *outboxHealthSession) Enqueue(envelope *protocolv1.ControlEnvelope) error {
	return session.outbox.Enqueue(envelope)
}

func (session *outboxHealthSession) ReplaceHealth(items []*protocolv1.ServiceHealth) error {
	return session.outbox.ReplaceHealth(items)
}

func (session *outboxHealthSession) EnqueueConfigAckAndReplaceHealth(
	ack *protocolv1.ControlEnvelope,
	items []*protocolv1.ServiceHealth,
) error {
	return session.outbox.EnqueueConfigAckAndReplaceHealth(ack, items)
}

func (session *outboxHealthSession) Flush(context.Context) error { return nil }

func (session *outboxHealthSession) Inbound() <-chan controlsession.Inbound { return session.inbound }
func (session *outboxHealthSession) Done() <-chan struct{}                  { return session.done }

func newFakeHealthSource() *fakeHealthSource {
	return &fakeHealthSource{states: make(map[string]agenthealth.State), changed: make(chan struct{}, 1)}
}

func (source *fakeHealthSource) Snapshot() map[string]agenthealth.State {
	result := make(map[string]agenthealth.State, len(source.states))
	for serviceID, state := range source.states {
		result[serviceID] = state
	}
	return result
}

func (source *fakeHealthSource) Changed() <-chan struct{} { return source.changed }

func TestHealthReporterFullBatchUsesCurrentStateAndFailClosedDefaults(t *testing.T) {
	source := newFakeHealthSource()
	source.states[firstHealthServiceID] = agenthealth.State{
		Status: protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY, ServiceRevision: 7,
		CheckedAt: time.UnixMilli(1234), Latency: 1500 * time.Millisecond,
		OriginErrorCode: protocolv1.ErrorCode_ERROR_CODE_ORIGIN_REFUSED,
	}
	session := newHealthReporterSession(4)
	reporter := newHealthReporter(source, session)
	snapshot := healthReporterSnapshot(7,
		healthReporterService(firstHealthServiceID, 7, protocolv1.HealthType_HEALTH_TYPE_TCP),
		healthReporterService(secondHealthServiceID, 7, protocolv1.HealthType_HEALTH_TYPE_TCP),
		healthReporterService(thirdHealthServiceID, 7, protocolv1.HealthType_HEALTH_TYPE_DISABLED),
		healthReporterService(fourthHealthServiceID, 0, protocolv1.HealthType_HEALTH_TYPE_DISABLED),
	)
	if err := reporter.publishFull(snapshot); err != nil {
		t.Fatal(err)
	}

	batch := receiveEnqueued(t, session.enqueued).GetServiceHealthBatch()
	if batch.GetGeneration() != 0 || len(batch.GetItems()) != 4 {
		t.Fatalf("full Batch = %#v", batch)
	}
	items := healthItemsByID(batch.GetItems())
	current := items[firstHealthServiceID]
	if current.GetStatus() != protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY ||
		current.GetServiceRevision() != 7 || current.GetCheckedAtMs() != 1234 ||
		current.GetLatencyMs() != 1500 || current.GetErrorCode() != "ORIGIN_REFUSED" {
		t.Fatalf("current Health = %#v", current)
	}
	if got := items[secondHealthServiceID].GetStatus(); got != protocolv1.HealthStatus_HEALTH_STATUS_UNKNOWN {
		t.Fatalf("missing enabled Health = %s, want UNKNOWN", got)
	}
	if got := items[thirdHealthServiceID].GetStatus(); got != protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY {
		t.Fatalf("Health Disabled = %s, want HEALTHY", got)
	}
	if got := items[fourthHealthServiceID].GetStatus(); got != protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY {
		t.Fatalf("missing revision-zero Health Disabled = %s, want HEALTHY", got)
	}
}

func TestHealthReporterPassesFullCollectionAtomicallyToOutbox(t *testing.T) {
	source := newFakeHealthSource()
	session := newHealthReporterSession(4)
	reporter := newHealthReporter(source, session)
	snapshot := &protocolv1.TunnelSnapshot{Revision: 3}
	for index := range healthReportBatchSize + 1 {
		snapshot.Services = append(snapshot.Services, healthReporterService(
			fmt.Sprintf("svc_%03d", index), 3, protocolv1.HealthType_HEALTH_TYPE_DISABLED,
		))
	}
	if err := reporter.publishFull(snapshot); err != nil {
		t.Fatal(err)
	}
	full := receiveEnqueued(t, session.enqueued).GetServiceHealthBatch()
	if full.GetGeneration() != 0 || len(full.GetItems()) != healthReportBatchSize+1 {
		t.Fatalf("full collection generation/items = %d/%d, want 0/%d",
			full.GetGeneration(), len(full.GetItems()), healthReportBatchSize+1)
	}
}

func TestHealthReporterReplacesFullServiceLimitBeforeOldHealthDequeues(t *testing.T) {
	outbox, err := controlsession.NewOutbox(1, controlHighQueue, controlNormalQueue)
	if err != nil {
		t.Fatal(err)
	}
	session := &outboxHealthSession{
		outbox: outbox, inbound: make(chan controlsession.Inbound), done: make(chan struct{}),
	}
	reporter := newHealthReporter(newFakeHealthSource(), session)
	first := &protocolv1.TunnelSnapshot{Revision: 3}
	for index := range configruntime.MaxServicesPerTunnel {
		first.Services = append(first.Services, healthReporterService(
			fmt.Sprintf("svc_000000000000000000000%05d", index), 3, protocolv1.HealthType_HEALTH_TYPE_DISABLED,
		))
	}
	if err := reporter.publishFull(first); err != nil {
		t.Fatal(err)
	}

	second := &protocolv1.TunnelSnapshot{Revision: 4, Services: append(
		[]*protocolv1.ServiceConfig(nil), first.Services[:len(first.Services)-1]...,
	)}
	replacementID := "svc_11111111111111111111111111"
	second.Services = append(second.Services,
		healthReporterService(replacementID, 4, protocolv1.HealthType_HEALTH_TYPE_DISABLED))
	for _, service := range second.Services[:len(second.Services)-1] {
		service.RequiredRevision = 4
	}
	if err := reporter.publishFull(second); err != nil {
		t.Fatalf("publishFull(replacement) error = %v", err)
	}

	seen := make(map[string]struct{}, configruntime.MaxServicesPerTunnel)
	for {
		envelope, exists := outbox.Dequeue()
		if !exists {
			break
		}
		for _, item := range envelope.GetServiceHealthBatch().GetItems() {
			seen[item.GetServiceId()] = struct{}{}
		}
	}
	removedID := first.Services[len(first.Services)-1].GetServiceId()
	if len(seen) != configruntime.MaxServicesPerTunnel {
		t.Fatalf("dequeued Health service count = %d, want %d", len(seen), configruntime.MaxServicesPerTunnel)
	}
	if _, exists := seen[removedID]; exists {
		t.Fatalf("removed service %q survived full collection replacement", removedID)
	}
	if _, exists := seen[replacementID]; !exists {
		t.Fatalf("replacement service %q missing", replacementID)
	}
}

func TestHealthReporterFullServiceLimitFitsProductionOutbox(t *testing.T) {
	outbox, err := controlsession.NewOutbox(1, controlHighQueue, controlNormalQueue)
	if err != nil {
		t.Fatal(err)
	}
	session := &outboxHealthSession{
		outbox: outbox, inbound: make(chan controlsession.Inbound), done: make(chan struct{}),
	}
	reporter := newHealthReporter(newFakeHealthSource(), session)
	snapshot := &protocolv1.TunnelSnapshot{Revision: 3}
	for range configruntime.MaxServicesPerTunnel {
		serviceID, idErr := identity.NewServiceID()
		if idErr != nil {
			t.Fatal(idErr)
		}
		snapshot.Services = append(snapshot.Services,
			healthReporterService(serviceID, 3, protocolv1.HealthType_HEALTH_TYPE_DISABLED))
	}
	if err := reporter.publishFull(snapshot); err != nil {
		t.Fatalf("publishFull(%d services) error = %v", len(snapshot.Services), err)
	}

	total := 0
	generation := uint64(0)
	for {
		envelope, exists := outbox.Dequeue()
		if !exists {
			break
		}
		batch := envelope.GetServiceHealthBatch()
		generation++
		if batch.GetGeneration() != generation || len(batch.GetItems()) > healthReportBatchSize {
			t.Fatalf("batch generation/items = %d/%d, want %d/<=%d",
				batch.GetGeneration(), len(batch.GetItems()), generation, healthReportBatchSize)
		}
		total += len(batch.GetItems())
	}
	if total != configruntime.MaxServicesPerTunnel {
		t.Fatalf("dequeued Health items = %d, want %d", total, configruntime.MaxServicesPerTunnel)
	}
}

func TestHealthReporterCoalescesIncrementalStateToLatestValue(t *testing.T) {
	source := newFakeHealthSource()
	source.states[firstHealthServiceID] = agenthealth.State{
		Status: protocolv1.HealthStatus_HEALTH_STATUS_UNKNOWN, ServiceRevision: 5,
	}
	session := newHealthReporterSession(4)
	reporter := newHealthReporter(source, session)
	if err := reporter.publishFull(healthReporterSnapshot(5,
		healthReporterService(firstHealthServiceID, 5, protocolv1.HealthType_HEALTH_TYPE_TCP))); err != nil {
		t.Fatal(err)
	}
	_ = receiveEnqueued(t, session.enqueued)

	source.states[firstHealthServiceID] = agenthealth.State{
		Status: protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY, ServiceRevision: 5,
	}
	if err := reporter.collectChanges(); err != nil {
		t.Fatal(err)
	}
	source.states[firstHealthServiceID] = agenthealth.State{
		Status: protocolv1.HealthStatus_HEALTH_STATUS_UNHEALTHY, ServiceRevision: 5,
	}
	if err := reporter.collectChanges(); err != nil {
		t.Fatal(err)
	}
	if err := reporter.flush(); err != nil {
		t.Fatal(err)
	}
	batch := receiveEnqueued(t, session.enqueued).GetServiceHealthBatch()
	if batch.GetGeneration() != 0 || len(batch.GetItems()) != 1 ||
		batch.GetItems()[0].GetStatus() != protocolv1.HealthStatus_HEALTH_STATUS_UNHEALTHY {
		t.Fatalf("incremental Batch = %#v", batch)
	}
}

func TestHealthReporterFlushesIncrementalBatchAtFixedCapacity(t *testing.T) {
	source := newFakeHealthSource()
	session := newHealthReporterSession(4)
	reporter := newHealthReporter(source, session)
	snapshot := &protocolv1.TunnelSnapshot{Revision: 6}
	for index := range healthReportBatchSize {
		serviceID := fmt.Sprintf("svc_capacity_%03d", index)
		snapshot.Services = append(snapshot.Services,
			healthReporterService(serviceID, 6, protocolv1.HealthType_HEALTH_TYPE_TCP))
		source.states[serviceID] = agenthealth.State{
			Status: protocolv1.HealthStatus_HEALTH_STATUS_UNKNOWN, ServiceRevision: 6,
		}
	}
	if err := reporter.publishFull(snapshot); err != nil {
		t.Fatal(err)
	}
	_ = receiveEnqueued(t, session.enqueued)
	for serviceID := range source.states {
		source.states[serviceID] = agenthealth.State{
			Status: protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY, ServiceRevision: 6,
		}
	}
	if err := reporter.collectChanges(); err != nil {
		t.Fatal(err)
	}
	batch := receiveEnqueued(t, session.enqueued).GetServiceHealthBatch()
	if batch.GetGeneration() != 0 || len(batch.GetItems()) != healthReportBatchSize {
		t.Fatalf("capacity-triggered Batch = generation %d, items %d", batch.GetGeneration(), len(batch.GetItems()))
	}
}

func TestHealthReporterDoesNotEnqueueEmptyFullBatch(t *testing.T) {
	reporter := newHealthReporter(newFakeHealthSource(), newHealthReporterSession(1))
	if err := reporter.publishFull(healthReporterSnapshot(1)); err != nil {
		t.Fatal(err)
	}
	if len(reporter.session.(*fakeEstablishedSession).enqueued) != 0 {
		t.Fatal("empty Snapshot enqueued an empty ServiceHealthBatch")
	}
}

func TestApplyInboundQueuesFullHealthOnlyAfterAppliedAck(t *testing.T) {
	_, configSession := newTestConfigSession(t)
	source := newFakeHealthSource()
	session := newHealthReporterSession(4)
	reporter := newHealthReporter(source, session)
	snapshot := testSnapshot(9)
	snapshot.Services = []*protocolv1.ServiceConfig{
		healthReporterService(firstHealthServiceID, 9, protocolv1.HealthType_HEALTH_TYPE_DISABLED),
	}
	if err := applyInboundAndReport(context.Background(), session, configSession, &fakeWorkPool{},
		inbound(&protocolv1.ControlEnvelope_ConfigSnapshot{ConfigSnapshot: snapshot}), reporter); err != nil {
		t.Fatal(err)
	}
	if first := receiveEnqueued(t, session.enqueued); first.GetConfigAck() == nil {
		t.Fatalf("first message = %#v, want ConfigAck", first)
	}
	if second := receiveEnqueued(t, session.enqueued); second.GetServiceHealthBatch() == nil {
		t.Fatalf("second message = %#v, want full Health Batch", second)
	}

	_, rejectedSession := newTestConfigSession(t)
	rejectedSink := newHealthReporterSession(4)
	rejectedReporter := newHealthReporter(source, rejectedSink)
	invalid := testSnapshot(10)
	invalid.TunnelId = "tun_wrong"
	if err := applyInboundAndReport(context.Background(), rejectedSink, rejectedSession, &fakeWorkPool{},
		inbound(&protocolv1.ControlEnvelope_ConfigSnapshot{ConfigSnapshot: invalid}), rejectedReporter); err != nil {
		t.Fatalf("rejected Snapshot ended Session: %v", err)
	}
	if message := receiveEnqueued(t, rejectedSink.enqueued); message.GetConfigAck().GetApplyStatus() != protocolv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_REJECTED {
		t.Fatalf("rejected message = %#v", message)
	}
	select {
	case message := <-rejectedSink.enqueued:
		t.Fatalf("REJECTED Snapshot emitted Health Batch: %#v", message)
	default:
	}

	_, failedSession := newTestConfigSession(t)
	failedSink := newHealthReporterSession(4)
	failedSink.enqueueErr = errors.New("outbox unavailable")
	failedReporter := newHealthReporter(source, failedSink)
	if err := applyInboundAndReport(context.Background(), failedSink, failedSession, &fakeWorkPool{},
		inbound(&protocolv1.ControlEnvelope_ConfigSnapshot{ConfigSnapshot: testSnapshot(11)}), failedReporter); !errors.Is(err, configruntime.ErrAckEnqueue) {
		t.Fatalf("Ack failure error = %v", err)
	}
	if len(failedSink.enqueued) != 1 {
		t.Fatalf("Ack failure enqueued %d messages, want only attempted Ack", len(failedSink.enqueued))
	}
}

func TestApplyInboundAtomicAckFailurePreservesReporterBaselineAndRetries(t *testing.T) {
	_, configSession := newTestConfigSession(t)
	source := newFakeHealthSource()
	session := newHealthReporterSession(4)
	session.enqueueErr = errors.New("outbox unavailable")
	reporter := newHealthReporter(source, session)
	snapshot := testSnapshot(11)
	snapshot.Services = []*protocolv1.ServiceConfig{
		healthReporterService(firstHealthServiceID, 11, protocolv1.HealthType_HEALTH_TYPE_DISABLED),
	}
	message := inbound(&protocolv1.ControlEnvelope_ConfigSnapshot{ConfigSnapshot: snapshot})

	if err := applyInboundAndReport(context.Background(), session, configSession, &fakeWorkPool{},
		message, reporter); !errors.Is(err, configruntime.ErrAckEnqueue) {
		t.Fatalf("first Apply() error = %v, want ErrAckEnqueue", err)
	}
	if reporter.configured || len(reporter.services) != 0 || len(reporter.last) != 0 || len(reporter.pending) != 0 {
		t.Fatalf("Reporter baseline advanced after failed atomic enqueue: configured=%t services=%#v last=%#v pending=%#v",
			reporter.configured, reporter.services, reporter.last, reporter.pending)
	}
	if _, _, observed := configSession.Observed(); observed {
		t.Fatal("Config Session observed baseline advanced after failed atomic enqueue")
	}
	if attempted := receiveEnqueued(t, session.enqueued); attempted.GetConfigAck() == nil {
		t.Fatalf("failed atomic enqueue attempted %#v, want ConfigAck", attempted)
	}

	session.enqueueErr = nil
	if err := applyInboundAndReport(context.Background(), session, configSession, &fakeWorkPool{},
		message, reporter); err != nil {
		t.Fatalf("retry Apply() error = %v", err)
	}
	if ack := receiveEnqueued(t, session.enqueued).GetConfigAck(); ack.GetObservedRevision() != 11 {
		t.Fatalf("retry ConfigAck = %#v, want observed revision 11", ack)
	}
	batch := receiveEnqueued(t, session.enqueued).GetServiceHealthBatch()
	if len(batch.GetItems()) != 1 || batch.GetItems()[0].GetServiceId() != firstHealthServiceID {
		t.Fatalf("retry full Health = %#v, want current Snapshot service", batch)
	}
	if !reporter.configured || reporter.services[firstHealthServiceID].revision != 11 ||
		reporter.last[firstHealthServiceID].revision != 11 || len(reporter.pending) != 0 {
		t.Fatalf("Reporter baseline after successful retry: configured=%t services=%#v last=%#v pending=%#v",
			reporter.configured, reporter.services, reporter.last, reporter.pending)
	}
	if revision, _, observed := configSession.Observed(); !observed || revision != 11 {
		t.Fatalf("Config Session observed baseline after retry = revision %d observed %t, want 11/true", revision, observed)
	}
}

func TestDrainFlushesPendingHealthBeforeDrainRequest(t *testing.T) {
	source := newFakeHealthSource()
	source.states[firstHealthServiceID] = agenthealth.State{
		Status: protocolv1.HealthStatus_HEALTH_STATUS_UNKNOWN, ServiceRevision: 4,
	}
	session := newHealthReporterSession(8)
	reporter := newHealthReporter(source, session)
	if err := reporter.publishFull(healthReporterSnapshot(4,
		healthReporterService(firstHealthServiceID, 4, protocolv1.HealthType_HEALTH_TYPE_TCP))); err != nil {
		t.Fatal(err)
	}
	_ = receiveEnqueued(t, session.enqueued)
	source.states[firstHealthServiceID] = agenthealth.State{
		Status: protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY, ServiceRevision: 4,
	}
	if err := reporter.collectChanges(); err != nil {
		t.Fatal(err)
	}

	processContext, cancelProcess := context.WithCancel(context.Background())
	cancelProcess()
	runtime := &Runtime{
		newDrainID: func() (string, error) { return testDrainID, nil }, drainTimeout: time.Second,
	}
	heartbeatTicker := time.NewTicker(time.Hour)
	reportTicker := time.NewTicker(time.Hour)
	defer heartbeatTicker.Stop()
	defer reportTicker.Stop()
	result := make(chan error, 1)
	go func() {
		result <- runtime.drain(
			processContext, session, nil, &fakeWorkPool{}, heartbeatTicker, reporter, reportTicker,
		)
	}()

	if message := receiveEnqueued(t, session.enqueued); message.GetServiceHealthBatch() == nil {
		t.Fatalf("first drain message = %#v, want pending Health Batch", message)
	}
	request := receiveEnqueued(t, session.enqueued).GetDrainRequest()
	if request.GetDrainId() != testDrainID {
		t.Fatalf("second drain message = %#v, want DrainRequest", request)
	}
	if calls := session.flushCalls.Load(); calls != 1 {
		t.Fatalf("Control Session Flush calls = %d, want 1 before DrainRequest", calls)
	}
	session.inbound <- inbound(&protocolv1.ControlEnvelope_DrainAck{
		DrainAck: &protocolv1.DrainAck{DrainId: testDrainID},
	})
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("drain error = %v, want process cancellation", err)
	}
}

func newHealthReporterSession(capacity int) *fakeEstablishedSession {
	return &fakeEstablishedSession{
		inbound: make(chan controlsession.Inbound, capacity), done: make(chan struct{}),
		enqueued: make(chan *protocolv1.ControlEnvelope, capacity),
	}
}

func healthReporterSnapshot(revision uint64, services ...*protocolv1.ServiceConfig) *protocolv1.TunnelSnapshot {
	return &protocolv1.TunnelSnapshot{TunnelId: testTunnelID, Revision: revision, Services: services}
}

func healthReporterService(serviceID string, revision uint64, healthType protocolv1.HealthType) *protocolv1.ServiceConfig {
	health := &protocolv1.HealthCheckConfig{Type: healthType}
	if healthType == protocolv1.HealthType_HEALTH_TYPE_TCP {
		health.IntervalMs = 1_000
		health.TimeoutMs = 100
		health.FailureThreshold = 2
		health.SuccessThreshold = 2
	}
	return &protocolv1.ServiceConfig{
		ServiceId: serviceID, RequiredRevision: revision, Enabled: true,
		OriginScheme: "http", OriginHost: "origin.test", OriginPort: 8080,
		ConnectTimeoutMs: 500, TlsVerify: true, Health: health,
	}
}

func healthItemsByID(items []*protocolv1.ServiceHealth) map[string]*protocolv1.ServiceHealth {
	result := make(map[string]*protocolv1.ServiceHealth, len(items))
	for _, item := range items {
		result[item.GetServiceId()] = item
	}
	return result
}
