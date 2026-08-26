package connector

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/open"
	agentworkpool "github.com/lifei6671/xtunnel/internal/agent/workpool"
	"github.com/lifei6671/xtunnel/internal/controlsession"
	"github.com/lifei6671/xtunnel/internal/identity"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
)

const (
	testDrainID    = "drain_01J00000000000000000000000"
	oldTestDrainID = "drain_01J00000000000000000000001"
)

func TestRunEstablishedSendsOneDrainAndWaitsForMatchingAck(t *testing.T) {
	processContext, cancelProcess := context.WithCancel(context.Background())
	session := newFakeEstablishedSession()
	pool := &fakeWorkPool{}
	runtime := &Runtime{
		newDrainID:   func() (string, error) { return testDrainID, nil },
		drainTimeout: time.Second,
	}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	result := make(chan error, 1)
	go func() {
		result <- runtime.runEstablished(processContext, session, pool, ticker)
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
	pool := &fakeWorkPool{}
	runtime := &Runtime{
		newDrainID:   func() (string, error) { return testDrainID, nil },
		drainTimeout: 10 * time.Millisecond,
	}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	result := make(chan error, 1)
	go func() { result <- runtime.runEstablished(processContext, session, pool, ticker) }()
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
	session.enqueueErr = controlsession.ErrOwnerClosed
	pool := &fakeWorkPool{}
	runtime := &Runtime{}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	if err := runtime.runEstablished(context.Background(), session, pool, ticker); err != nil {
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

func TestApplyInboundRoutesWorkDemandAndAcceptsDeferredM3Messages(t *testing.T) {
	pool := &fakeWorkPool{}
	demand := &protocolv1.WorkDemand{DemandGeneration: 7}
	if err := applyInbound(pool, inbound(&protocolv1.ControlEnvelope_WorkDemand{WorkDemand: demand})); err != nil {
		t.Fatalf("applyInbound(WorkDemand) error = %v", err)
	}
	if pool.demand != demand {
		t.Fatal("WorkDemand was not forwarded to the current WorkPool")
	}

	if err := applyInbound(pool, inbound(&protocolv1.ControlEnvelope_ConfigSnapshot{
		ConfigSnapshot: &protocolv1.TunnelSnapshot{},
	})); err != nil {
		t.Fatalf("applyInbound(ConfigSnapshot) error = %v", err)
	}
	if err := applyInbound(pool, inbound(&protocolv1.ControlEnvelope_DrainAck{
		DrainAck: &protocolv1.DrainAck{},
	})); err != nil {
		t.Fatalf("applyInbound(DrainAck) error = %v", err)
	}
}

func TestApplyInboundRejectsDirectionViolationAndPropagatesDemandError(t *testing.T) {
	demandErr := errors.New("demand failed")
	pool := &fakeWorkPool{applyErr: demandErr}
	if err := applyInbound(pool, inbound(&protocolv1.ControlEnvelope_WorkDemand{
		WorkDemand: &protocolv1.WorkDemand{DemandGeneration: 1},
	})); !errors.Is(err, demandErr) {
		t.Fatalf("applyInbound(WorkDemand) error = %v, want demand error", err)
	}
	if err := applyInbound(pool, inbound(&protocolv1.ControlEnvelope_Heartbeat{
		Heartbeat: &protocolv1.Heartbeat{},
	})); !errors.Is(err, ErrUnsupportedControlMessage) {
		t.Fatalf("applyInbound(Heartbeat) error = %v, want direction violation", err)
	}
	if err := applyInbound(pool, controlsession.Inbound{}); !errors.Is(err, ErrUnsupportedControlMessage) {
		t.Fatalf("applyInbound(nil) error = %v", err)
	}
}

func TestHeartbeatEnvelopeUsesProtocolVersionAndNonNegativeMilliseconds(t *testing.T) {
	envelope := heartbeatEnvelope(time.UnixMilli(1234))
	if envelope.GetProtocolVersion() != 1 || envelope.GetHeartbeat().GetTimestampMs() != 1234 {
		t.Fatalf("heartbeatEnvelope() = %#v", envelope)
	}
	if got := heartbeatEnvelope(time.UnixMilli(-1)).GetHeartbeat().GetTimestampMs(); got != 0 {
		t.Fatalf("negative heartbeat timestamp = %d, want 0", got)
	}
}

func TestHostConfigCreatesEphemeralConnectorAndUnobservedOriginFailsClosed(t *testing.T) {
	config, err := HostConfig("xta_test", "v0.1.0-test", openOriginDialer())
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
	connection, code, err := UnobservedOriginDialer(context.Background(), "svc_ignored")
	if connection != nil || code != protocolv1.ErrorCode_ERROR_CODE_SERVICE_CONFIG_NOT_OBSERVED ||
		!errors.Is(err, ErrServiceConfigNotObserved) {
		t.Fatalf("UnobservedOriginDialer() = (%v, %s, %v)", connection, code, err)
	}
}

type fakeWorkPool struct {
	demand             *protocolv1.WorkDemand
	applyErr           error
	beginDrainCalls    atomic.Int32
	completeDrainCalls atomic.Int32
	done               chan struct{}
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

func openOriginDialer() open.OriginDialer {
	return open.OriginDialerFunc(UnobservedOriginDialer)
}
