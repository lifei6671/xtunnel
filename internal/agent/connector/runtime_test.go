package connector

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/configruntime"
	agenthealth "github.com/lifei6671/xtunnel/internal/agent/health"
	agentorigin "github.com/lifei6671/xtunnel/internal/agent/origin"
	"github.com/lifei6671/xtunnel/internal/agent/reconnect"
	agentsession "github.com/lifei6671/xtunnel/internal/agent/session"
	agentworkpool "github.com/lifei6671/xtunnel/internal/agent/workpool"
	"github.com/lifei6671/xtunnel/internal/controlsession"
	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/safego"
)

const (
	testTunnelID   = "tun_01J00000000000000000000000"
	testDrainID    = "drain_01J00000000000000000000000"
	oldTestDrainID = "drain_01J00000000000000000000001"
)

func TestRunEstablishedSendsOneDrainAndWaitsForMatchingAck(t *testing.T) {
	processContext, cancelProcess := context.WithCancel(context.Background())
	session := newFakeEstablishedSession()
	_, configSession := newTestConfigSession(t)
	pool := &fakeWorkPool{}
	runtime := &Runtime{
		newDrainID:   func() (string, error) { return testDrainID, nil },
		drainTimeout: time.Second,
	}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	result := make(chan error, 1)
	go func() {
		result <- runtime.runEstablished(processContext, session, configSession, pool, ticker)
	}()
	cancelProcess()

	request := receiveEnqueued(t, session.enqueued)
	if request.GetDrainRequest().GetDrainId() != testDrainID ||
		request.GetDrainRequest().GetDrainTimeoutMs() != 1_000 {
		t.Fatalf("DrainRequest = %#v", request.GetDrainRequest())
	}
	// 旧代或错误 ID 的 Ack 不能完成当前握手；匹配 Ack 才是第二阶段提交点。
	session.inbound <- inbound(&protocolv1.ControlEnvelope_DrainAck{
		DrainAck: &protocolv1.DrainAck{DrainId: oldTestDrainID},
	})
	select {
	case err := <-result:
		t.Fatalf("错误 DrainAck 提前结束排空: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	session.inbound <- inbound(&protocolv1.ControlEnvelope_DrainAck{
		DrainAck: &protocolv1.DrainAck{DrainId: testDrainID},
	})
	if err := receiveResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("runEstablished() error = %v, want context.Canceled", err)
	}
	if pool.beginDrainCalls.Load() != 1 || pool.completeDrainCalls.Load() != 1 {
		t.Fatalf("drain calls = begin:%d complete:%d",
			pool.beginDrainCalls.Load(), pool.completeDrainCalls.Load())
	}
	select {
	case extra := <-session.enqueued:
		t.Fatalf("同一取消生成了额外 Control 消息: %#v", extra)
	default:
	}
}

func TestRunEstablishedDrainTimeoutDoesNotAcceptOldAck(t *testing.T) {
	processContext, cancelProcess := context.WithCancel(context.Background())
	session := newFakeEstablishedSession()
	_, configSession := newTestConfigSession(t)
	pool := &fakeWorkPool{}
	runtime := &Runtime{
		newDrainID:   func() (string, error) { return testDrainID, nil },
		drainTimeout: 10 * time.Millisecond,
	}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	result := make(chan error, 1)
	go func() { result <- runtime.runEstablished(processContext, session, configSession, pool, ticker) }()
	cancelProcess()
	_ = receiveEnqueued(t, session.enqueued)
	session.inbound <- inbound(&protocolv1.ControlEnvelope_DrainAck{
		DrainAck: &protocolv1.DrainAck{DrainId: oldTestDrainID},
	})
	if err := receiveResult(t, result); !errors.Is(err, context.DeadlineExceeded) ||
		!errors.Is(err, context.Canceled) {
		t.Fatalf("runEstablished() error = %v, want canceled + drain deadline", err)
	}
	if pool.beginDrainCalls.Load() != 1 || pool.completeDrainCalls.Load() != 0 {
		t.Fatalf("timeout drain calls = begin:%d complete:%d",
			pool.beginDrainCalls.Load(), pool.completeDrainCalls.Load())
	}
}

func TestRunEstablishedTreatsHeartbeatOwnerClosedAsSessionCompletion(t *testing.T) {
	session := newFakeEstablishedSession()
	_, configSession := newTestConfigSession(t)
	session.enqueueErr = controlsession.ErrOwnerClosed
	pool := &fakeWorkPool{}
	runtime := &Runtime{}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	if err := runtime.runEstablished(context.Background(), session, configSession, pool, ticker); err != nil {
		t.Fatalf("runEstablished() error = %v, want ordinary Session completion", err)
	}
}

func TestRuntimeShutdownRetiredPoolsCancelsAndWaitsForDone(t *testing.T) {
	runtime := &Runtime{}
	poolDone := make(chan struct{})
	pool := &fakeWorkPool{done: poolDone}
	cancelCalled := make(chan struct{})
	releaseDone := make(chan struct{})
	runtime.retainPool(pool, func() {
		close(cancelCalled)
		go func() {
			<-releaseDone
			close(poolDone)
		}()
	})
	select {
	case <-cancelCalled:
		t.Fatal("retired Pool was canceled before coordinated shutdown")
	case <-time.After(20 * time.Millisecond):
	}

	shutdownDone := make(chan struct{})
	go func() {
		runtime.shutdownRetiredPools()
		close(shutdownDone)
	}()
	select {
	case <-cancelCalled:
	case <-time.After(time.Second):
		t.Fatal("retired Pool was not canceled")
	}
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned before retired Pool Done closed")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseDone)
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after retired Pool Done closed")
	}
}

func TestRuntimeRetiredPoolObserverPanicCancelsAndRecordsFailure(t *testing.T) {
	runtime := &Runtime{}
	canceled := make(chan struct{})
	runtime.retainPool(&fakeWorkPool{donePanic: true}, func() { close(canceled) })

	waitDone := make(chan struct{})
	go func() {
		runtime.retiredWait.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("retired Pool observer panic did not finish owner wait")
	}
	select {
	case <-canceled:
	default:
		t.Fatal("retired Pool observer panic did not cancel Pool")
	}
	if err := runtime.retiredError(); !errors.Is(err, safego.ErrPanic) {
		t.Fatalf("retiredError() = %v, want safego.ErrPanic", err)
	}
	runtime.retiredMu.Lock()
	remaining := len(runtime.retiredPools)
	runtime.retiredMu.Unlock()
	if remaining != 0 {
		t.Fatalf("retired Pool registry size = %d, want 0", remaining)
	}
}

func TestRuntimeDrainRetiredPoolsAllowsNaturalCompletionBeforeDeadline(t *testing.T) {
	runtime := &Runtime{}
	poolDone := make(chan struct{})
	cancelCalled := make(chan struct{})
	runtime.retainPool(&fakeWorkPool{done: poolDone}, func() { close(cancelCalled) })

	drainDone := make(chan struct{})
	go func() {
		runtime.drainRetiredPools(time.Second)
		close(drainDone)
	}()
	select {
	case <-cancelCalled:
		t.Fatal("retired Pool was canceled before drain deadline")
	case <-time.After(20 * time.Millisecond):
	}
	close(poolDone)
	select {
	case <-drainDone:
	case <-time.After(time.Second):
		t.Fatal("retired Pool drain did not finish after natural completion")
	}
	select {
	case <-cancelCalled:
		t.Fatal("naturally completed retired Pool was canceled")
	default:
	}
}

func TestRuntimeDrainRetiredPoolsCancelsAtDeadline(t *testing.T) {
	runtime := &Runtime{}
	poolDone := make(chan struct{})
	cancelCalled := make(chan struct{})
	runtime.retainPool(&fakeWorkPool{done: poolDone}, func() {
		close(cancelCalled)
		close(poolDone)
	})

	drainDone := make(chan struct{})
	go func() {
		runtime.drainRetiredPools(50 * time.Millisecond)
		close(drainDone)
	}()
	select {
	case <-cancelCalled:
		t.Fatal("retired Pool was canceled before drain deadline")
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case <-drainDone:
	case <-time.After(time.Second):
		t.Fatal("retired Pool drain did not force cancellation at deadline")
	}
	select {
	case <-cancelCalled:
	default:
		t.Fatal("retired Pool cancel was not called at deadline")
	}
}

func TestApplyInboundRequiresAppliedSnapshotBeforeWorkDemand(t *testing.T) {
	_, configSession := newTestConfigSession(t)
	session := newFakeEstablishedSession()
	pool := &fakeWorkPool{}
	demand := &protocolv1.WorkDemand{DemandGeneration: 7}
	if err := applyInbound(context.Background(), session, configSession, pool,
		inbound(&protocolv1.ControlEnvelope_WorkDemand{WorkDemand: demand})); !errors.Is(err, ErrServiceConfigNotObserved) {
		t.Fatalf("applyInbound(WorkDemand before Snapshot) error = %v", err)
	}
	if pool.demand != nil {
		t.Fatal("WorkDemand was forwarded before this Session observed a Snapshot")
	}

	if err := applyInbound(context.Background(), session, configSession, pool,
		inbound(&protocolv1.ControlEnvelope_ConfigSnapshot{ConfigSnapshot: testSnapshot(7)})); err != nil {
		t.Fatalf("applyInbound(ConfigSnapshot) error = %v", err)
	}
	ack := receiveEnqueued(t, session.enqueued).GetConfigAck()
	if ack.GetApplyStatus() != protocolv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_APPLIED ||
		ack.GetObservedRevision() != 7 || ack.GetErrorCode() != protocolv1.ErrorCode_ERROR_CODE_OK {
		t.Fatalf("ConfigAck = %#v", ack)
	}

	if err := applyInbound(context.Background(), session, configSession, pool,
		inbound(&protocolv1.ControlEnvelope_WorkDemand{WorkDemand: demand})); err != nil {
		t.Fatalf("applyInbound(WorkDemand after Ack) error = %v", err)
	}
	if pool.demand != demand {
		t.Fatal("WorkDemand was not forwarded to the current WorkPool")
	}

	if err := applyInbound(context.Background(), session, configSession, pool,
		inbound(&protocolv1.ControlEnvelope_DrainAck{DrainAck: &protocolv1.DrainAck{}})); err != nil {
		t.Fatalf("applyInbound(DrainAck) error = %v", err)
	}
}

func TestApplyInboundKeepsSessionAfterRejectedSnapshotAndPropagatesAckFailure(t *testing.T) {
	_, configSession := newTestConfigSession(t)
	session := newFakeEstablishedSession()
	invalid := testSnapshot(8)
	invalid.TunnelId = "tun_wrong"
	if err := applyInbound(context.Background(), session, configSession, &fakeWorkPool{},
		inbound(&protocolv1.ControlEnvelope_ConfigSnapshot{ConfigSnapshot: invalid})); err != nil {
		t.Fatalf("rejected ConfigSnapshot ended Session: %v", err)
	}
	rejected := receiveEnqueued(t, session.enqueued).GetConfigAck()
	if rejected.GetApplyStatus() != protocolv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_REJECTED ||
		rejected.GetObservedRevision() != 0 {
		t.Fatalf("rejected ConfigAck = %#v", rejected)
	}
	if _, _, observed := configSession.Observed(); observed {
		t.Fatal("rejected Snapshot advanced observed baseline")
	}

	_, secondSession := newTestConfigSession(t)
	ackErr := errors.New("ack queue closed")
	failingSink := newFakeEstablishedSession()
	failingSink.enqueueErr = ackErr
	if err := applyInbound(context.Background(), failingSink, secondSession, &fakeWorkPool{},
		inbound(&protocolv1.ControlEnvelope_ConfigSnapshot{ConfigSnapshot: testSnapshot(9)})); !errors.Is(err, configruntime.ErrAckEnqueue) || !errors.Is(err, ackErr) {
		t.Fatalf("applyInbound(ConfigSnapshot Ack failure) error = %v", err)
	}
	if _, _, observed := secondSession.Observed(); observed {
		t.Fatal("failed ConfigAck enqueue advanced observed baseline")
	}
}

func TestApplyInboundRejectsDirectionViolationAndPropagatesDemandError(t *testing.T) {
	_, configSession := newTestConfigSession(t)
	session := newFakeEstablishedSession()
	if err := applyInbound(context.Background(), session, configSession, &fakeWorkPool{},
		inbound(&protocolv1.ControlEnvelope_ConfigSnapshot{ConfigSnapshot: testSnapshot(1)})); err != nil {
		t.Fatalf("apply Snapshot before demand error test: %v", err)
	}
	_ = receiveEnqueued(t, session.enqueued)
	demandErr := errors.New("demand failed")
	pool := &fakeWorkPool{applyErr: demandErr}
	if err := applyInbound(context.Background(), session, configSession, pool,
		inbound(&protocolv1.ControlEnvelope_WorkDemand{WorkDemand: &protocolv1.WorkDemand{DemandGeneration: 1}})); !errors.Is(err, demandErr) {
		t.Fatalf("applyInbound(WorkDemand) error = %v, want demand error", err)
	}
	if err := applyInbound(context.Background(), session, configSession, pool,
		inbound(&protocolv1.ControlEnvelope_Heartbeat{Heartbeat: &protocolv1.Heartbeat{}})); !errors.Is(err, ErrUnsupportedControlMessage) {
		t.Fatalf("applyInbound(Heartbeat) error = %v, want direction violation", err)
	}
	if err := applyInbound(context.Background(), session, configSession, pool,
		controlsession.Inbound{}); !errors.Is(err, ErrUnsupportedControlMessage) {
		t.Fatalf("applyInbound(nil) error = %v", err)
	}
}

func TestHeartbeatEnvelopeUsesProtocolVersionAndNonNegativeMilliseconds(t *testing.T) {
	envelope := heartbeatEnvelope(time.UnixMilli(1234), 42)
	if envelope.GetProtocolVersion() != 1 || envelope.GetHeartbeat().GetTimestampMs() != 1234 ||
		envelope.GetHeartbeat().GetObservedRevision() != 42 {
		t.Fatalf("heartbeatEnvelope() = %#v", envelope)
	}
	if got := heartbeatEnvelope(time.UnixMilli(-1), 0).GetHeartbeat().GetTimestampMs(); got != 0 {
		t.Fatalf("negative heartbeat timestamp = %d, want 0", got)
	}
}

func TestConfigManagerReusesCurrentAcrossSessionsButResetsObservedBaseline(t *testing.T) {
	manager, first := newTestConfigSession(t)
	firstOwner := newFakeEstablishedSession()
	if err := applyInbound(context.Background(), firstOwner, first, &fakeWorkPool{},
		inbound(&protocolv1.ControlEnvelope_ConfigSnapshot{ConfigSnapshot: testSnapshot(11)})); err != nil {
		t.Fatalf("first Session Apply error = %v", err)
	}
	_ = receiveEnqueued(t, firstOwner.enqueued)
	if current, ok := manager.Current(); !ok || current.Revision != 11 || !current.Acked {
		t.Fatalf("process current after first Session = %#v, %v", current, ok)
	}

	second, err := manager.NewSession(testTunnelID)
	if err != nil {
		t.Fatalf("NewSession(second) error = %v", err)
	}
	if revision := observedRevision(second); revision != 0 {
		t.Fatalf("new Session observed revision = %d, want 0", revision)
	}
	pool := &fakeWorkPool{}
	if err := applyInbound(context.Background(), newFakeEstablishedSession(), second, pool,
		inbound(&protocolv1.ControlEnvelope_WorkDemand{WorkDemand: &protocolv1.WorkDemand{DemandGeneration: 1}})); !errors.Is(err, ErrServiceConfigNotObserved) {
		t.Fatalf("second Session WorkDemand before Ack error = %v", err)
	}
	if pool.demand != nil {
		t.Fatal("second Session reused process current as its observed baseline")
	}
}

func TestRunEstablishedHeartbeatUsesCurrentSessionObservedRevision(t *testing.T) {
	_, configSession := newTestConfigSession(t)
	session := newFakeEstablishedSession()
	if err := applyInbound(context.Background(), session, configSession, &fakeWorkPool{},
		inbound(&protocolv1.ControlEnvelope_ConfigSnapshot{ConfigSnapshot: testSnapshot(12)})); err != nil {
		t.Fatalf("Apply error = %v", err)
	}
	_ = receiveEnqueued(t, session.enqueued)

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	result := make(chan error, 1)
	go func() {
		result <- (&Runtime{}).runEstablished(context.Background(), session, configSession, &fakeWorkPool{}, ticker)
	}()
	heartbeat := receiveEnqueued(t, session.enqueued).GetHeartbeat()
	if heartbeat.GetObservedRevision() != 12 {
		t.Fatalf("normal heartbeat observed revision = %d, want 12", heartbeat.GetObservedRevision())
	}
	close(session.done)
	if err := receiveResult(t, result); err != nil {
		t.Fatalf("runEstablished() error = %v", err)
	}
}

func TestDrainingHeartbeatUsesCurrentSessionObservedRevision(t *testing.T) {
	_, configSession := newTestConfigSession(t)
	session := newFakeEstablishedSession()
	if err := applyInbound(context.Background(), session, configSession, &fakeWorkPool{},
		inbound(&protocolv1.ControlEnvelope_ConfigSnapshot{ConfigSnapshot: testSnapshot(13)})); err != nil {
		t.Fatalf("Apply error = %v", err)
	}
	_ = receiveEnqueued(t, session.enqueued)

	processContext, cancelProcess := context.WithCancel(context.Background())
	runtime := &Runtime{newDrainID: func() (string, error) { return testDrainID, nil }, drainTimeout: time.Second}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	result := make(chan error, 1)
	go func() {
		result <- runtime.runEstablished(processContext, session, configSession, &fakeWorkPool{}, ticker)
	}()
	cancelProcess()
	_ = receiveEnqueued(t, session.enqueued)
	ticker.Reset(time.Millisecond)
	heartbeat := receiveEnqueued(t, session.enqueued).GetHeartbeat()
	if heartbeat.GetObservedRevision() != 13 {
		t.Fatalf("draining heartbeat observed revision = %d, want 13", heartbeat.GetObservedRevision())
	}
	session.inbound <- inbound(&protocolv1.ControlEnvelope_DrainAck{
		DrainAck: &protocolv1.DrainAck{DrainId: testDrainID},
	})
	if err := receiveResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("runEstablished() error = %v, want context.Canceled", err)
	}
}

func TestRuntimeRunKeepsConfigAliveThroughWorkPoolDrainThenClosesManager(t *testing.T) {
	processContext, cancelProcess := context.WithCancel(context.Background())
	defer cancelProcess()
	poolDone := make(chan struct{})
	retired := make(chan struct{})
	var closePool sync.Once
	resource := &orderedConfigResource{poolDone: poolDone, retired: retired}
	var manager *configruntime.Manager

	runtime := &Runtime{drainTimeout: time.Second, retiredPools: make(map[uint64]retiredPool)}
	runtime.newConfigManager = func(parent context.Context) (*configruntime.Manager, error) {
		var err error
		manager, err = configruntime.New(parent, testConfigRuntimeConfig(orderedConfigBuilder{resource: resource}))
		return manager, err
	}
	runtime.runControlSessions = func(ctx context.Context, _ reconnect.SessionHandler[*agentsession.Session]) error {
		configSession, err := manager.NewSession(testTunnelID)
		if err != nil {
			return err
		}
		if err := configSession.Apply(ctx, testSnapshot(14), newFakeEstablishedSession()); err != nil {
			return err
		}
		runtime.retainPool(&fakeWorkPool{done: poolDone}, func() { closePool.Do(func() { close(poolDone) }) })
		cancelProcess()
		<-ctx.Done()
		select {
		case <-retired:
			return errors.New("config runtime retired before Control loop returned")
		default:
		}
		go func() {
			time.Sleep(10 * time.Millisecond)
			closePool.Do(func() { close(poolDone) })
		}()
		return ctx.Err()
	}

	if err := runtime.Run(processContext); !errors.Is(err, context.Canceled) {
		t.Fatalf("Runtime.Run() error = %v, want context.Canceled", err)
	}
	select {
	case <-retired:
	case <-time.After(time.Second):
		t.Fatal("config Manager was not closed after WorkPool drain")
	}
	if resource.retiredBeforePool.Load() {
		t.Fatal("config Manager retired resources before WorkPool drain completed")
	}
}

func TestRuntimeRunStartsHealthBeforeControlAndShutsItDownAfterConfig(t *testing.T) {
	recorder := &lifecycleRecorder{}
	health := newFakeHealthRuntime(recorder)
	resource := &recordingConfigResource{recorder: recorder}
	var manager *configruntime.Manager
	runtime := &Runtime{
		drainTimeout: time.Second,
		retiredPools: make(map[uint64]retiredPool),
		health:       health,
	}
	runtime.newConfigManager = func(parent context.Context) (*configruntime.Manager, error) {
		var err error
		manager, err = configruntime.New(parent, testConfigRuntimeConfig(recordingConfigBuilder{resource: resource}))
		return manager, err
	}
	runtime.runControlSessions = func(ctx context.Context, _ reconnect.SessionHandler[*agentsession.Session]) error {
		recorder.add("control.run")
		configSession, err := manager.NewSession(testTunnelID)
		if err != nil {
			return err
		}
		if err := configSession.Apply(ctx, testSnapshot(1), newFakeEstablishedSession()); err != nil {
			return err
		}
		return nil
	}

	if err := runtime.Run(context.Background()); err != nil {
		t.Fatalf("Runtime.Run() error = %v", err)
	}
	want := []string{"health.start", "control.run", "config.retire", "health.shutdown"}
	if got := recorder.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("shutdown order = %v, want %v", got, want)
	}
}

func TestRuntimeRunSharesOneDeadlineAcrossConfigAndHealthShutdown(t *testing.T) {
	const (
		shutdownBudget = 120 * time.Millisecond
		configDelay    = 60 * time.Millisecond
	)
	configRetireStarted := make(chan time.Time, 1)
	healthDeadline := make(chan time.Time, 1)
	recorder := &lifecycleRecorder{}
	health := newFakeHealthRuntime(recorder)
	health.shutdown = func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			return errors.New("health shutdown context has no deadline")
		}
		healthDeadline <- deadline
		return nil
	}
	resource := &recordingConfigResource{
		recorder: recorder,
		retire: func(context.Context) error {
			configRetireStarted <- time.Now()
			time.Sleep(configDelay)
			return nil
		},
	}
	var manager *configruntime.Manager
	runtime := &Runtime{
		drainTimeout: shutdownBudget,
		retiredPools: make(map[uint64]retiredPool),
		health:       health,
	}
	runtime.newConfigManager = func(parent context.Context) (*configruntime.Manager, error) {
		var err error
		manager, err = configruntime.New(parent, testConfigRuntimeConfig(recordingConfigBuilder{resource: resource}))
		return manager, err
	}
	runtime.runControlSessions = func(ctx context.Context, _ reconnect.SessionHandler[*agentsession.Session]) error {
		configSession, err := manager.NewSession(testTunnelID)
		if err != nil {
			return err
		}
		return configSession.Apply(ctx, testSnapshot(1), newFakeEstablishedSession())
	}

	if err := runtime.Run(context.Background()); err != nil {
		t.Fatalf("Runtime.Run() error = %v", err)
	}
	retireStarted := <-configRetireStarted
	deadline := <-healthDeadline
	if latest := retireStarted.Add(shutdownBudget + 20*time.Millisecond); deadline.After(latest) {
		t.Fatalf("Health deadline = %v, want shared deadline no later than %v", deadline, latest)
	}
}

func TestRuntimeRunPropagatesUnexpectedHealthFailure(t *testing.T) {
	recorder := &lifecycleRecorder{}
	health := newFakeHealthRuntime(recorder)
	healthFailure := errors.New("health owner failed")
	retireNoise := configruntime.ErrClosed
	var manager *configruntime.Manager
	runtime := &Runtime{
		drainTimeout: time.Second,
		retiredPools: make(map[uint64]retiredPool),
		health:       health,
	}
	runtime.newConfigManager = func(parent context.Context) (*configruntime.Manager, error) {
		var err error
		manager, err = configruntime.New(parent, testConfigRuntimeConfig(recordingConfigBuilder{
			resource: &recordingConfigResource{recorder: recorder, retireErr: retireNoise},
		}))
		return manager, err
	}
	runtime.runControlSessions = func(ctx context.Context, _ reconnect.SessionHandler[*agentsession.Session]) error {
		configSession, err := manager.NewSession(testTunnelID)
		if err != nil {
			return err
		}
		if err := configSession.Apply(ctx, testSnapshot(1), newFakeEstablishedSession()); err != nil {
			return err
		}
		health.fail(healthFailure)
		<-ctx.Done()
		return ctx.Err()
	}

	err := runtime.Run(context.Background())
	if !errors.Is(err, healthFailure) || !errors.Is(err, context.Canceled) || !errors.Is(err, retireNoise) {
		t.Fatalf("Runtime.Run() error = %v, want health failure, canceled Control loop, and cleanup noise", err)
	}
	if got := recorder.snapshot(); !reflect.DeepEqual(got, []string{"health.start", "config.retire", "health.shutdown"}) {
		t.Fatalf("health lifecycle = %v", got)
	}
}

func TestHostConfigCreatesEphemeralConnectorAndUnobservedOriginFailsClosed(t *testing.T) {
	config, err := HostConfig("xta_test", "v0.1.0-test")
	if err != nil {
		t.Fatalf("HostConfig() error = %v", err)
	}
	if err := identity.ValidateConnectorID(config.Connector.ID()); err != nil {
		t.Fatalf("HostConfig() connector id error = %v", err)
	}
	if config.ConnectionToken != "xta_test" || config.Version != "v0.1.0-test" ||
		config.Hostname == "" || config.OS == "" || config.Arch == "" {
		t.Fatalf("HostConfig() = %#v", config)
	}
	runtime, err := New(config)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if runtime.health == nil {
		t.Fatal("New() did not install the production Health runtime")
	}
	connection, code, err := runtime.origin.DialOrigin(context.Background(), "svc_01J00000000000000000000000")
	if connection != nil || code != protocolv1.ErrorCode_ERROR_CODE_SERVICE_CONFIG_NOT_OBSERVED ||
		!errors.Is(err, agentorigin.ErrConfigNotObserved) {
		t.Fatalf("DialOrigin() = (%v, %s, %v)", connection, code, err)
	}
}

type orderedConfigBuilder struct {
	resource *orderedConfigResource
}

type orderedConfigCandidate struct {
	resource *orderedConfigResource
}

type orderedConfigResource struct {
	poolDone          <-chan struct{}
	retired           chan struct{}
	retiredBeforePool atomic.Bool
}

func (builder orderedConfigBuilder) Build(
	context.Context,
	*protocolv1.TunnelSnapshot,
	configruntime.Gate,
) (configruntime.Candidate, error) {
	return orderedConfigCandidate{resource: builder.resource}, nil
}

func (candidate orderedConfigCandidate) Start(context.Context) error { return nil }
func (candidate orderedConfigCandidate) Abort(context.Context) error { return nil }
func (candidate orderedConfigCandidate) Runtime() configruntime.Resources {
	return candidate.resource
}

func (resource *orderedConfigResource) Retire(context.Context) error {
	select {
	case <-resource.poolDone:
	default:
		resource.retiredBeforePool.Store(true)
	}
	close(resource.retired)
	return nil
}

func newTestConfigSession(t *testing.T) (*configruntime.Manager, *configruntime.Session) {
	t.Helper()
	manager, err := configruntime.New(context.Background(), testConfigRuntimeConfig(agentorigin.New()))
	if err != nil {
		t.Fatalf("configruntime.New() error = %v", err)
	}
	t.Cleanup(func() {
		closeContext, cancelClose := context.WithTimeout(context.Background(), time.Second)
		defer cancelClose()
		if err := manager.Close(closeContext); err != nil {
			t.Errorf("config Manager Close error = %v", err)
		}
	})
	session, err := manager.NewSession(testTunnelID)
	if err != nil {
		t.Fatalf("Manager.NewSession() error = %v", err)
	}
	return manager, session
}

func testConfigRuntimeConfig(builder configruntime.Builder) configruntime.Config {
	return configruntime.Config{
		ProtocolVersion:      1,
		MaxServices:          configruntime.MaxServicesPerTunnel,
		MaxSnapshotBytes:     configruntime.MaxSnapshotSize,
		MaxControlFrameBytes: int(frame.MaxControlFrameSize),
		RetireTimeout:        time.Second,
		Builder:              builder,
	}
}

func testSnapshot(revision uint64) *protocolv1.TunnelSnapshot {
	return &protocolv1.TunnelSnapshot{TunnelId: testTunnelID, Revision: revision}
}

type fakeWorkPool struct {
	demand             *protocolv1.WorkDemand
	applyErr           error
	beginDrainCalls    atomic.Int32
	completeDrainCalls atomic.Int32
	done               chan struct{}
	donePanic          bool
}

type fakeHealthRuntime struct {
	recorder *lifecycleRecorder
	done     chan struct{}
	changed  chan struct{}
	doneOnce sync.Once
	errMu    sync.Mutex
	err      error
	states   map[string]agenthealth.State
	shutdown func(context.Context) error
}

func newFakeHealthRuntime(recorder *lifecycleRecorder) *fakeHealthRuntime {
	return &fakeHealthRuntime{
		recorder: recorder, done: make(chan struct{}), changed: make(chan struct{}, 1),
		states: make(map[string]agenthealth.State),
	}
}

func (runtime *fakeHealthRuntime) Start(context.Context) error {
	runtime.recorder.add("health.start")
	return nil
}

func (runtime *fakeHealthRuntime) Done() <-chan struct{} { return runtime.done }

func (runtime *fakeHealthRuntime) Snapshot() map[string]agenthealth.State {
	result := make(map[string]agenthealth.State, len(runtime.states))
	for serviceID, state := range runtime.states {
		result[serviceID] = state
	}
	return result
}

func (runtime *fakeHealthRuntime) Changed() <-chan struct{} { return runtime.changed }

func (runtime *fakeHealthRuntime) Err() error {
	runtime.errMu.Lock()
	defer runtime.errMu.Unlock()
	return runtime.err
}

func (runtime *fakeHealthRuntime) Shutdown(ctx context.Context) error {
	runtime.recorder.add("health.shutdown")
	runtime.doneOnce.Do(func() { close(runtime.done) })
	if runtime.shutdown != nil {
		return errors.Join(runtime.shutdown(ctx), runtime.Err())
	}
	return runtime.Err()
}

func (runtime *fakeHealthRuntime) fail(err error) {
	runtime.errMu.Lock()
	runtime.err = err
	runtime.errMu.Unlock()
	runtime.doneOnce.Do(func() { close(runtime.done) })
}

type recordingConfigBuilder struct {
	resource configruntime.Resources
}

func (builder recordingConfigBuilder) Build(context.Context, *protocolv1.TunnelSnapshot, configruntime.Gate) (configruntime.Candidate, error) {
	return &recordingConfigCandidate{resource: builder.resource}, nil
}

type recordingConfigCandidate struct {
	resource configruntime.Resources
}

func (*recordingConfigCandidate) Start(context.Context) error { return nil }
func (*recordingConfigCandidate) Abort(context.Context) error { return nil }
func (candidate *recordingConfigCandidate) Runtime() configruntime.Resources {
	return candidate.resource
}

type recordingConfigResource struct {
	recorder  *lifecycleRecorder
	retire    func(context.Context) error
	retireErr error
}

func (resource *recordingConfigResource) Retire(ctx context.Context) error {
	resource.recorder.add("config.retire")
	if resource.retire != nil {
		return errors.Join(resource.retire(ctx), resource.retireErr)
	}
	return resource.retireErr
}

func closedSignal() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}

func (pool *fakeWorkPool) Start(context.Context) error { return nil }

func (pool *fakeWorkPool) ApplyDemand(demand *protocolv1.WorkDemand) (agentworkpool.DemandResult, error) {
	pool.demand = demand
	return agentworkpool.DemandResult{}, pool.applyErr
}

func (pool *fakeWorkPool) BeginDrain() error {
	pool.beginDrainCalls.Add(1)
	return nil
}

func (pool *fakeWorkPool) CompleteDrain(context.Context) error {
	pool.completeDrainCalls.Add(1)
	return nil
}

func (pool *fakeWorkPool) Wait() error { return nil }

func (pool *fakeWorkPool) Done() <-chan struct{} {
	if pool.donePanic {
		panic("retired Pool Done panic must not escape its goroutine")
	}
	if pool.done != nil {
		return pool.done
	}
	done := make(chan struct{})
	close(done)
	return done
}

type fakeEstablishedSession struct {
	inbound    chan controlsession.Inbound
	done       chan struct{}
	enqueued   chan *protocolv1.ControlEnvelope
	enqueueErr error
	flushErr   error
	flushCalls atomic.Int32
}

func newFakeEstablishedSession() *fakeEstablishedSession {
	return &fakeEstablishedSession{
		inbound: make(chan controlsession.Inbound, 4), done: make(chan struct{}),
		enqueued: make(chan *protocolv1.ControlEnvelope, 4),
	}
}

func (session *fakeEstablishedSession) Enqueue(envelope *protocolv1.ControlEnvelope) error {
	session.enqueued <- envelope
	return session.enqueueErr
}

func (session *fakeEstablishedSession) ReplaceHealth(items []*protocolv1.ServiceHealth) error {
	if len(items) > 0 {
		session.enqueued <- healthBatchEnvelope(0, items)
	}
	return session.enqueueErr
}

func (session *fakeEstablishedSession) EnqueueConfigAckAndReplaceHealth(
	ack *protocolv1.ControlEnvelope,
	items []*protocolv1.ServiceHealth,
) error {
	session.enqueued <- ack
	if session.enqueueErr == nil && len(items) > 0 {
		session.enqueued <- healthBatchEnvelope(0, items)
	}
	return session.enqueueErr
}

func (session *fakeEstablishedSession) Flush(context.Context) error {
	session.flushCalls.Add(1)
	return session.flushErr
}

func (session *fakeEstablishedSession) Inbound() <-chan controlsession.Inbound {
	return session.inbound
}
func (session *fakeEstablishedSession) Done() <-chan struct{} { return session.done }

func receiveEnqueued(t *testing.T, messages <-chan *protocolv1.ControlEnvelope) *protocolv1.ControlEnvelope {
	t.Helper()
	select {
	case message := <-messages:
		return message
	case <-time.After(time.Second):
		t.Fatal("等待 Agent Control 消息超时")
		return nil
	}
}

func receiveResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatal("等待 Agent 排空结果超时")
		return nil
	}
}

func inbound(payload any) controlsession.Inbound {
	envelope := &protocolv1.ControlEnvelope{ProtocolVersion: 1}
	switch value := payload.(type) {
	case *protocolv1.ControlEnvelope_WorkDemand:
		envelope.Payload = value
	case *protocolv1.ControlEnvelope_ConfigSnapshot:
		envelope.Payload = value
	case *protocolv1.ControlEnvelope_DrainAck:
		envelope.Payload = value
	case *protocolv1.ControlEnvelope_Heartbeat:
		envelope.Payload = value
	default:
		panic("unsupported test payload")
	}
	return controlsession.Inbound{Envelope: envelope}
}
