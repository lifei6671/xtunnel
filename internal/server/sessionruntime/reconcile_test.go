package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/controlsession"
	"github.com/lifei6671/xtunnel/internal/protocol/deterministic"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
	serversnapshot "github.com/lifei6671/xtunnel/internal/server/snapshot"
)

func TestReconcileDiscardsBuildWhenTunnelGenerationAdvances(t *testing.T) {
	provider := &blockingGenerationProvider{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	registry := serverruntime.NewRegistry()
	manager := newReconcileTestManager(t, registry, provider)
	session := commitSession(t, registry, testTunnelID, testConnectorID)
	server, client := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	established := establishedSession(t, session, 0x31)
	serveResult := make(chan error, 1)
	go func() { serveResult <- manager.Serve(context.Background(), server, &established) }()

	select {
	case <-provider.firstStarted:
	case <-time.After(testWait):
		t.Fatal("first Snapshot build did not start")
	}
	if err := manager.MarkDirty(testTunnelID); err != nil {
		t.Fatalf("MarkDirty() error = %v", err)
	}
	close(provider.releaseFirst)

	snapshot := readConfigSnapshot(t, client)
	if snapshot.GetRevision() != 2 {
		t.Fatalf("first published revision = %d, want latest revision 2", snapshot.GetRevision())
	}
	provider.mu.Lock()
	calls := provider.calls
	provider.mu.Unlock()
	if calls != 2 {
		t.Fatalf("Snapshot builds = %d, want stale build plus immediate rebuild", calls)
	}
	_ = client.Close()
	select {
	case <-serveResult:
	case <-time.After(testWait):
		t.Fatal("Serve() did not stop after stale generation test")
	}
}

func TestReconcileKeepsHighestPendingAndSendsItAfterAck(t *testing.T) {
	provider := &revisionSnapshotProvider{revision: 1}
	registry := serverruntime.NewRegistry()
	manager := newReconcileTestManager(t, registry, provider)
	session := commitSession(t, registry, testTunnelID, testConnectorID)
	server, client := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	established := establishedSession(t, session, 0x32)
	serveResult := make(chan error, 1)
	go func() { serveResult <- manager.Serve(context.Background(), server, &established) }()

	initial := readConfigSnapshot(t, client)
	writeConfigAck(t, client, appliedConfigAck(initial.GetRevision()))
	_ = readWorkDemand(t, client)

	provider.setRevision(2)
	if err := manager.MarkDirty(testTunnelID); err != nil {
		t.Fatalf("mark revision 2 dirty: %v", err)
	}
	revisionTwo := readConfigSnapshot(t, client)
	if revisionTwo.GetRevision() != 2 {
		t.Fatalf("outstanding revision = %d, want 2", revisionTwo.GetRevision())
	}

	provider.setRevision(3)
	if err := manager.MarkDirty(testTunnelID); err != nil {
		t.Fatalf("mark revision 3 dirty: %v", err)
	}
	provider.waitCalls(t, 3)
	provider.setRevision(4)
	if err := manager.MarkDirty(testTunnelID); err != nil {
		t.Fatalf("mark revision 4 dirty: %v", err)
	}
	provider.waitCalls(t, 4)

	writeConfigAck(t, client, appliedConfigAck(2))
	latest := readConfigSnapshot(t, client)
	if latest.GetRevision() != 4 {
		t.Fatalf("Snapshot after Ack = %d, want highest pending revision 4", latest.GetRevision())
	}
	writeConfigAck(t, client, appliedConfigAck(4))

	_ = client.Close()
	select {
	case <-serveResult:
	case <-time.After(testWait):
		t.Fatal("Serve() did not stop after pending Snapshot test")
	}
}

func TestShutdownCancelsBlockedSnapshotBuildAndWaitsForLoop(t *testing.T) {
	provider := &cancelBlockingProvider{started: make(chan struct{}), canceled: make(chan struct{})}
	registry := serverruntime.NewRegistry()
	observed := make(chan protocolv1.ErrorCode, 1)
	options := reconcileTestOptions(provider)
	options.ReconcileObserver = func(_ time.Duration, code protocolv1.ErrorCode) { observed <- code }
	manager, err := New(registry, options)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := manager.MarkDirty(testTunnelID); err != nil {
		t.Fatalf("MarkDirty() error = %v", err)
	}
	select {
	case <-provider.started:
	case <-time.After(testWait):
		t.Fatal("blocked Snapshot build did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	if err := manager.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	select {
	case <-provider.canceled:
	default:
		t.Fatal("Shutdown() returned before Snapshot Source observed cancellation")
	}
	select {
	case <-manager.snapshotDone:
	default:
		t.Fatal("Shutdown() returned before Snapshot Reconcile Loop exited")
	}
	if err := manager.MarkDirty(testTunnelID); err != ErrReconcilerNotRunning {
		t.Fatalf("MarkDirty(after Shutdown) error = %v, want ErrReconcilerNotRunning", err)
	}
	select {
	case code := <-observed:
		t.Fatalf("Shutdown cancellation emitted Reconcile metrics with code %s", code)
	default:
	}
}

func TestReconcilePublishesFailureStateAndClearsItAfterRecovery(t *testing.T) {
	sourceErr := errors.New("injected snapshot source failure")
	provider := &revisionSnapshotProvider{revision: 1, err: sourceErr}
	manager := newReconcileTestManager(t, serverruntime.NewRegistry(), provider)

	if err := manager.MarkDirty(testTunnelID); err != nil {
		t.Fatalf("MarkDirty(failure) error = %v", err)
	}
	provider.waitCalls(t, 1)
	waitFor(t, func() bool {
		err, exists := manager.SnapshotError(testTunnelID)
		return exists && errors.Is(err, sourceErr)
	})

	provider.setError(nil)
	if err := manager.MarkDirty(testTunnelID); err != nil {
		t.Fatalf("MarkDirty(recovery) error = %v", err)
	}
	provider.waitCalls(t, 2)
	waitFor(t, func() bool {
		_, exists := manager.SnapshotError(testTunnelID)
		return !exists
	})
}

func TestReconcileMetricsObserveBuildResultAndPublishedSnapshot(t *testing.T) {
	provider := &revisionSnapshotProvider{
		revision: 1,
		delay:    5 * time.Millisecond,
		services: []*protocolv1.ServiceConfig{
			{ServiceId: "svc_01J00000000000000000000000"},
			{ServiceId: "svc_01J00000000000000000000001"},
		},
	}
	type observation struct {
		duration time.Duration
		code     protocolv1.ErrorCode
	}
	observed := make(chan observation, 2)
	options := reconcileTestOptions(provider)
	options.ReconcileObserver = func(duration time.Duration, code protocolv1.ErrorCode) {
		observed <- observation{duration: duration, code: code}
	}
	manager, err := New(serverruntime.NewRegistry(), options)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	startTestManager(t, manager)

	if err := manager.MarkDirty(testTunnelID); err != nil {
		t.Fatalf("MarkDirty(success) error = %v", err)
	}
	success := waitReconcileObservation(t, observed)
	if success.code != protocolv1.ErrorCode_ERROR_CODE_OK || success.duration < provider.delay {
		t.Fatalf("success observation = %+v, want OK and duration >= %v", success, provider.delay)
	}
	expectedSnapshot := &protocolv1.TunnelSnapshot{
		TunnelId: testTunnelID, Revision: 1, Services: provider.services,
	}
	encoded, err := deterministic.MarshalSnapshot(expectedSnapshot)
	if err != nil {
		t.Fatalf("MarshalSnapshot() error = %v", err)
	}
	waitFor(t, func() bool {
		metrics := manager.MetricsSnapshot()
		return metrics.XTunnelRouteSnapshotBytes == uint64(len(encoded)) &&
			metrics.XTunnelRouteSnapshotRoutes == 2
	})

	provider.setError(errors.New("injected metrics failure"))
	if err := manager.MarkDirty(testTunnelID); err != nil {
		t.Fatalf("MarkDirty(failure) error = %v", err)
	}
	failure := waitReconcileObservation(t, observed)
	if failure.code != protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR || failure.duration < provider.delay {
		t.Fatalf("failure observation = %+v, want INTERNAL_ERROR and duration >= %v", failure, provider.delay)
	}
	metrics := manager.MetricsSnapshot()
	if metrics.XTunnelRouteSnapshotBytes != uint64(len(encoded)) || metrics.XTunnelRouteSnapshotRoutes != 2 {
		t.Fatalf("failed reconcile changed published snapshot metrics: %+v", metrics)
	}
}

func TestReconcileMetricsCountMergedDirtyNotificationsExactlyOnce(t *testing.T) {
	provider := &blockingGenerationProvider{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	manager, err := New(serverruntime.NewRegistry(), reconcileTestOptions(provider))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	startTestManager(t, manager)
	if err := manager.MarkDirty(testTunnelID); err != nil {
		t.Fatalf("MarkDirty(initial) error = %v", err)
	}
	select {
	case <-provider.firstStarted:
	case <-time.After(testWait):
		t.Fatal("initial Snapshot build did not start")
	}
	if err := manager.MarkDirty(testTunnelID); err != nil {
		t.Fatal(err)
	}
	if err := manager.MarkDirty(testTunnelID); err != nil {
		t.Fatal(err)
	}
	if err := manager.MarkDirty("tun_01J00000000000000000000001"); err != nil {
		t.Fatal(err)
	}
	// build 已取走 initial dirty 与对应 wake；第一条新 dirty 负责重新唤醒，
	// 后两条分别因同 Tunnel 替换、共享 wake 已占用而合并。
	if got := manager.MetricsSnapshot().XTunnelReconcileCoalescedTotal; got != 2 {
		t.Fatalf("XTunnelReconcileCoalescedTotal = %d, want 2", got)
	}
	close(provider.releaseFirst)
}

func TestReconcileMetricsAggregateMultipleTunnelsAndReplaceOneContribution(t *testing.T) {
	const secondTunnelID = "tun_01J00000000000000000000001"
	provider := &multiTunnelSnapshotProvider{snapshots: map[string]snapshotSpec{
		testTunnelID:   {revision: 1, routes: 1},
		secondTunnelID: {revision: 1, routes: 2},
	}}
	observed := make(chan protocolv1.ErrorCode, 3)
	options := reconcileTestOptions(provider)
	options.ReconcileObserver = func(_ time.Duration, code protocolv1.ErrorCode) { observed <- code }
	manager, err := New(serverruntime.NewRegistry(), options)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	startTestManager(t, manager)

	for _, tunnelID := range []string{testTunnelID, secondTunnelID} {
		if err := manager.MarkDirty(tunnelID); err != nil {
			t.Fatalf("MarkDirty(%s) error = %v", tunnelID, err)
		}
		if code := waitReconcileObservation(t, observed); code != protocolv1.ErrorCode_ERROR_CODE_OK {
			t.Fatalf("reconcile code = %s, want OK", code)
		}
	}
	firstBytes, firstRoutes := provider.metrics(t, testTunnelID)
	secondBytes, secondRoutes := provider.metrics(t, secondTunnelID)
	waitFor(t, func() bool {
		metrics := manager.MetricsSnapshot()
		return metrics.XTunnelRouteSnapshotBytes == firstBytes+secondBytes &&
			metrics.XTunnelRouteSnapshotRoutes == firstRoutes+secondRoutes
	})

	provider.set(testTunnelID, snapshotSpec{revision: 2, routes: 3})
	if err := manager.MarkDirty(testTunnelID); err != nil {
		t.Fatalf("MarkDirty(updated first tunnel) error = %v", err)
	}
	if code := waitReconcileObservation(t, observed); code != protocolv1.ErrorCode_ERROR_CODE_OK {
		t.Fatalf("updated reconcile code = %s, want OK", code)
	}
	updatedBytes, updatedRoutes := provider.metrics(t, testTunnelID)
	waitFor(t, func() bool {
		metrics := manager.MetricsSnapshot()
		return metrics.XTunnelRouteSnapshotBytes == updatedBytes+secondBytes &&
			metrics.XTunnelRouteSnapshotRoutes == updatedRoutes+secondRoutes
	})
}

func waitReconcileObservation[T any](t *testing.T, observed <-chan T) T {
	t.Helper()
	select {
	case item := <-observed:
		return item
	case <-time.After(testWait):
		t.Fatal("timed out waiting for reconcile observation")
		var zero T
		return zero
	}
}

func TestRejectedSnapshotKeepsObservedAndWaitsForHigherRevision(t *testing.T) {
	provider := &revisionSnapshotProvider{revision: 1}
	registry := serverruntime.NewRegistry()
	manager := newReconcileTestManager(t, registry, provider)
	session := commitSession(t, registry, testTunnelID, testConnectorID)
	server, client := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	established := establishedSession(t, session, 0x33)
	serveResult := make(chan error, 1)
	go func() { serveResult <- manager.Serve(context.Background(), server, &established) }()

	initial := readConfigSnapshot(t, client)
	writeConfigAck(t, client, appliedConfigAck(initial.GetRevision()))
	_ = readWorkDemand(t, client)
	provider.setRevision(2)
	if err := manager.MarkDirty(testTunnelID); err != nil {
		t.Fatalf("mark revision 2 dirty: %v", err)
	}
	if snapshot := readConfigSnapshot(t, client); snapshot.GetRevision() != 2 {
		t.Fatalf("outstanding revision = %d, want 2", snapshot.GetRevision())
	}
	writeConfigAck(t, client, &protocolv1.ConfigAck{
		ObservedRevision: 1,
		ApplyStatus:      protocolv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_REJECTED,
		ErrorCode:        protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR,
	})
	waitFor(t, func() bool {
		_, exists := manager.Resolve(session.SessionID)
		return exists
	})

	if err := manager.MarkDirty(testTunnelID); err != nil {
		t.Fatalf("remark rejected revision dirty: %v", err)
	}
	provider.waitCalls(t, 3)
	if err := client.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("set rejected Snapshot read deadline: %v", err)
	}
	envelope := &protocolv1.ControlEnvelope{}
	if err := frame.ReadControl(client, envelope); err == nil || !isTimeout(err) {
		t.Fatalf("rejected revision was resent: envelope=%#v error=%v", envelope, err)
	}
	if err := client.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear rejected Snapshot read deadline: %v", err)
	}

	provider.setRevision(3)
	if err := manager.MarkDirty(testTunnelID); err != nil {
		t.Fatalf("mark revision 3 dirty: %v", err)
	}
	if snapshot := readConfigSnapshot(t, client); snapshot.GetRevision() != 3 {
		t.Fatalf("Snapshot after REJECTED = %d, want higher revision 3", snapshot.GetRevision())
	}
	_ = client.Close()
	select {
	case <-serveResult:
	case <-time.After(testWait):
		t.Fatal("Serve() did not stop after REJECTED Snapshot test")
	}
}

func TestDuplicateConfigAckDoesNotConsumeNewOutstandingSnapshot(t *testing.T) {
	managed := &managedSession{
		hasObserved:      true,
		observedRevision: 1,
		outstanding:      &snapshotCandidate{revision: 2},
	}
	duplicate := controlsession.Inbound{
		Duplicate: true,
		Envelope: &protocolv1.ControlEnvelope{
			Payload: &protocolv1.ControlEnvelope_ConfigAck{ConfigAck: appliedConfigAck(1)},
		},
	}

	if err := (&Manager{}).handleConfigAck(managed, duplicate); err != nil {
		t.Fatalf("handleConfigAck(duplicate) error = %v", err)
	}
	managed.configMu.Lock()
	defer managed.configMu.Unlock()
	if managed.outstanding == nil || managed.outstanding.revision != 2 || managed.observedRevision != 1 {
		t.Fatalf("duplicate Ack changed config state: observed=%d outstanding=%#v", managed.observedRevision, managed.outstanding)
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

type blockingGenerationProvider struct {
	mu           sync.Mutex
	calls        int
	firstStarted chan struct{}
	releaseFirst chan struct{}
}

func (provider *blockingGenerationProvider) Current(ctx context.Context, tunnelID string) (serversnapshot.Result, error) {
	provider.mu.Lock()
	provider.calls++
	call := provider.calls
	provider.mu.Unlock()
	if call == 1 {
		close(provider.firstStarted)
		select {
		case <-provider.releaseFirst:
		case <-ctx.Done():
			return serversnapshot.Result{}, ctx.Err()
		}
	}
	return serversnapshot.Result{Snapshot: &protocolv1.TunnelSnapshot{TunnelId: tunnelID, Revision: uint64(call)}}, nil
}

type revisionSnapshotProvider struct {
	mu       sync.Mutex
	revision uint64
	calls    int
	err      error
	delay    time.Duration
	services []*protocolv1.ServiceConfig
}

func (provider *revisionSnapshotProvider) Current(_ context.Context, tunnelID string) (serversnapshot.Result, error) {
	provider.mu.Lock()
	provider.calls++
	revision := provider.revision
	err := provider.err
	delay := provider.delay
	services := append([]*protocolv1.ServiceConfig(nil), provider.services...)
	provider.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if err != nil {
		return serversnapshot.Result{}, err
	}
	return serversnapshot.Result{Snapshot: &protocolv1.TunnelSnapshot{
		TunnelId: tunnelID, Revision: revision, Services: services,
	}}, nil
}

func (provider *revisionSnapshotProvider) setRevision(revision uint64) {
	provider.mu.Lock()
	provider.revision = revision
	provider.mu.Unlock()
}

func (provider *revisionSnapshotProvider) setError(err error) {
	provider.mu.Lock()
	provider.err = err
	provider.mu.Unlock()
}

func (provider *revisionSnapshotProvider) waitCalls(t *testing.T, want int) {
	t.Helper()
	waitFor(t, func() bool {
		provider.mu.Lock()
		defer provider.mu.Unlock()
		return provider.calls >= want
	})
}

type cancelBlockingProvider struct {
	started  chan struct{}
	canceled chan struct{}
}

type snapshotSpec struct {
	revision uint64
	routes   int
}

type multiTunnelSnapshotProvider struct {
	mu        sync.Mutex
	snapshots map[string]snapshotSpec
}

func (provider *multiTunnelSnapshotProvider) Current(_ context.Context, tunnelID string) (serversnapshot.Result, error) {
	return serversnapshot.Result{Snapshot: provider.snapshot(tunnelID)}, nil
}

func (provider *multiTunnelSnapshotProvider) set(tunnelID string, spec snapshotSpec) {
	provider.mu.Lock()
	provider.snapshots[tunnelID] = spec
	provider.mu.Unlock()
}

func (provider *multiTunnelSnapshotProvider) snapshot(tunnelID string) *protocolv1.TunnelSnapshot {
	provider.mu.Lock()
	spec := provider.snapshots[tunnelID]
	provider.mu.Unlock()
	services := make([]*protocolv1.ServiceConfig, spec.routes)
	for index := range services {
		services[index] = &protocolv1.ServiceConfig{
			ServiceId: fmt.Sprintf("svc_01J00000000000000000000%03d", index),
		}
	}
	return &protocolv1.TunnelSnapshot{TunnelId: tunnelID, Revision: spec.revision, Services: services}
}

func (provider *multiTunnelSnapshotProvider) metrics(t *testing.T, tunnelID string) (uint64, uint64) {
	t.Helper()
	snapshot := provider.snapshot(tunnelID)
	encoded, err := deterministic.MarshalSnapshot(snapshot)
	if err != nil {
		t.Fatalf("MarshalSnapshot(%s) error = %v", tunnelID, err)
	}
	return uint64(len(encoded)), uint64(len(snapshot.GetServices()))
}

func (provider *cancelBlockingProvider) Current(ctx context.Context, _ string) (serversnapshot.Result, error) {
	close(provider.started)
	<-ctx.Done()
	close(provider.canceled)
	return serversnapshot.Result{}, ctx.Err()
}

func newReconcileTestManager(t *testing.T, registry *serverruntime.Registry, provider SnapshotProvider) *Manager {
	t.Helper()
	manager, err := New(registry, reconcileTestOptions(provider))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	startTestManager(t, manager)
	return manager
}

func reconcileTestOptions(provider SnapshotProvider) Options {
	return Options{
		HighPriorityCapacity: 8, NormalCapacity: 8, InboundCapacity: 8,
		WriteTimeout: testWriteTimeout, MaxReplayEntries: testReplayEntries,
		MaxWorkTotal: 64, MaxWorkConnecting: 16, HeartbeatTimeout: 5 * time.Second,
		SnapshotProvider: provider,
	}
}
