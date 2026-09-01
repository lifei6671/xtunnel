package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/state"
	servercontrolauth "github.com/lifei6671/xtunnel/internal/server/controlauth"
	serveropen "github.com/lifei6671/xtunnel/internal/server/open"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
	"github.com/lifei6671/xtunnel/internal/server/sessionruntime"
	serversnapshot "github.com/lifei6671/xtunnel/internal/server/snapshot"
	serverworkauth "github.com/lifei6671/xtunnel/internal/server/workauth"
)

const (
	connectorSelectionBenchmarkHeartbeatInterval = 10 * time.Minute
	connectorSelectionBenchmarkHeartbeatTimeout  = 30 * time.Minute
)

// BenchmarkConnectorSelection 测量 Proxy.selectConnector 的真实产品路径，包括
// Session Manager 的 Pools 索引副本、Registry eligibility/least-active/RR 选择，
// 以及 predicate 内的 WorkPool Snapshot。并发锁争用与 Block Profile 属于 M7-05。
func BenchmarkConnectorSelection(b *testing.B) {
	for _, connectors := range []int{1, 8, 32, 100} {
		b.Run(fmt.Sprintf("connectors_%d", connectors), func(b *testing.B) {
			fixture := newConnectorSelectionBenchmarkFixture(b, connectors)

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				lease, _, pool, membership, err := fixture.proxy.selectConnector(
					context.Background(), testTunnelID, testServiceID, 0,
				)
				if err != nil {
					b.Fatalf("select connector: %v", err)
				}
				if pool == nil || membership != nil {
					lease.Release()
					b.Fatalf("selection returned pool=%p membership=%p, want idle pool without pending membership", pool, membership)
				}
				lease.Release()
			}
			b.StopTimer()
			b.ReportMetric(float64(connectors), "connectors")
		})
	}
}

// BenchmarkConnectorSelectionConcurrent 在真实选择路径上并行施加压力，让 M7-05
// 可以分别归因 Session Manager、TunnelRuntime 与 WorkPool 的锁等待。该基准只测量
// 已就绪 Pool 的成功选择；每次迭代都必须归还 Lease，避免样本之间累积虚假负载。
func BenchmarkConnectorSelectionConcurrent(b *testing.B) {
	for _, connectors := range []int{1, 8, 32, 100} {
		b.Run(fmt.Sprintf("connectors_%d", connectors), func(b *testing.B) {
			fixture := newConnectorSelectionBenchmarkFixture(b, connectors)

			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(parallel *testing.PB) {
				for parallel.Next() {
					if err := runConcurrentConnectorSelection(fixture); err != nil {
						b.Errorf("concurrent select connector: %v", err)
						return
					}
				}
			})
			b.StopTimer()
			b.ReportMetric(float64(connectors), "connectors")
		})
	}
}

func runConcurrentConnectorSelection(fixture *connectorSelectionBenchmarkFixture) error {
	lease, _, pool, membership, err := fixture.proxy.selectConnector(
		context.Background(), testTunnelID, testServiceID, 0,
	)
	if err != nil {
		if lease != nil && !lease.Release() {
			err = errors.Join(err, errors.New("release Connector Lease after selection failure"))
		}
		if membership != nil {
			err = errors.Join(err, membership.Release())
		}
		return err
	}
	if lease == nil || pool == nil || membership != nil {
		var cleanupErr error
		if lease != nil && !lease.Release() {
			cleanupErr = errors.New("release unexpected Connector Lease")
		}
		if membership != nil {
			cleanupErr = errors.Join(cleanupErr, membership.Release())
		}
		return errors.Join(
			fmt.Errorf("selection returned lease=%p pool=%p membership=%p, want idle pool without pending membership", lease, pool, membership),
			cleanupErr,
		)
	}
	if !lease.Release() {
		return errors.New("release selected Connector Lease")
	}
	return nil
}

type connectorSelectionBenchmarkSnapshotProvider struct{}

func (connectorSelectionBenchmarkSnapshotProvider) Current(
	_ context.Context,
	tunnelID string,
) (serversnapshot.Result, error) {
	return serversnapshot.Result{Snapshot: &protocolv1.TunnelSnapshot{
		TunnelId: tunnelID,
		Services: []*protocolv1.ServiceConfig{{
			ServiceId: testServiceID,
			Enabled:   true,
			Health: &protocolv1.HealthCheckConfig{
				Type: protocolv1.HealthType_HEALTH_TYPE_DISABLED,
			},
		}},
	}}, nil
}

type connectorSelectionBenchmarkFixture struct {
	proxy          *Proxy
	sessions       *sessionruntime.Manager
	controlPeers   []net.Conn
	workPeers      []net.Conn
	sessionResults []<-chan error
}

func newConnectorSelectionBenchmarkFixture(
	b *testing.B,
	connectorCount int,
) *connectorSelectionBenchmarkFixture {
	b.Helper()
	registry := serverruntime.NewRegistry()
	// Benchmark fixture 不运行完整 Agent heartbeat writer；正式 2s×5 采样及
	// 校准可能超过生产默认窗口，因此只延长测试 Session 的超时。Shutdown 仍按
	// 下方短 Context 主动收敛，不依赖该长 Timer 回收资源。
	sessions, err := sessionruntime.New(registry, sessionruntime.Options{
		HighPriorityCapacity: 16,
		NormalCapacity:       32,
		InboundCapacity:      16,
		WriteTimeout:         time.Second,
		MaxReplayEntries:     128,
		MaxWorkTotal:         1,
		MaxWorkConnecting:    1,
		HeartbeatTimeout:     connectorSelectionBenchmarkHeartbeatTimeout,
		SnapshotProvider:     connectorSelectionBenchmarkSnapshotProvider{},
		Logger:               slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		b.Fatalf("create Session Manager: %v", err)
	}
	if err := sessions.Start(context.Background()); err != nil {
		b.Fatalf("start Session Manager: %v", err)
	}

	fixture := &connectorSelectionBenchmarkFixture{sessions: sessions}
	b.Cleanup(func() {
		fixture.close(b)
	})
	for index := range connectorCount {
		connectorID := fmt.Sprintf("con_01J%023d", index)
		pending, err := registry.ReserveAuthenticated(testTunnelID, connectorID)
		if err != nil {
			b.Fatalf("reserve Connector %d: %v", index, err)
		}
		session, err := registry.CommitAuthenticated(pending)
		if err != nil {
			b.Fatalf("commit Connector %d: %v", index, err)
		}

		serverControl, agentControl := net.Pipe()
		fixture.controlPeers = append(fixture.controlPeers, agentControl)
		established := connectorSelectionBenchmarkEstablished(b, session)
		result := make(chan error, 1)
		fixture.sessionResults = append(fixture.sessionResults, result)
		go func() {
			result <- sessions.Serve(context.Background(), serverControl, &established)
		}()
		connectorSelectionBenchmarkReadDemand(b, agentControl)
		connectorSelectionBenchmarkWait(b, func() bool {
			_, ready := sessions.Pool(session)
			return ready && registry.EligibleAtRevision(session, testServiceID, 0)
		})

		serverWork, agentWork := net.Pipe()
		fixture.workPeers = append(fixture.workPeers, agentWork)
		workID := fmt.Sprintf("work_01J%023d", index)
		if _, err := sessions.RegisterIdle(
			serverWork,
			connectorSelectionBenchmarkIdle(b, session, workID),
		); err != nil {
			_ = serverWork.Close()
			b.Fatalf("register idle WorkConn %d: %v", index, err)
		}
	}

	openHandler, err := serveropen.NewHandler(serveropen.Options{
		HandshakeTimeout: time.Second,
		WriteTimeout:     time.Second,
		ReadTimeout:      time.Second,
	})
	if err != nil {
		b.Fatalf("create OPEN handler: %v", err)
	}
	fixture.proxy, err = NewProxy(Options{
		Registry: registry, Sessions: sessions, OpenHandler: openHandler,
		AcquireTimeout: time.Second,
		Logger:         slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		b.Fatalf("create Tunnel Proxy: %v", err)
	}
	return fixture
}

func (fixture *connectorSelectionBenchmarkFixture) close(b *testing.B) {
	b.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := fixture.sessions.Shutdown(ctx); err != nil {
		b.Errorf("shutdown Session Manager: %v", err)
	}
	for _, connection := range fixture.controlPeers {
		_ = connection.Close()
	}
	for _, connection := range fixture.workPeers {
		_ = connection.Close()
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for index, result := range fixture.sessionResults {
		select {
		case <-result:
		case <-deadline.C:
			b.Errorf(
				"Control Sessions did not exit before deadline: first pending=%d remaining=%d",
				index, len(fixture.sessionResults)-index,
			)
			return
		}
	}
}

func connectorSelectionBenchmarkEstablished(
	b *testing.B,
	session serverruntime.Session,
) servercontrolauth.Established {
	b.Helper()
	control, err := state.NewControl(state.EndpointServer, 1)
	if err != nil {
		b.Fatalf("create Control state: %v", err)
	}
	result := &protocolv1.ConnectorAuthResult{Result: &protocolv1.ConnectorAuthResult_Success{
		Success: &protocolv1.ConnectorAuthSuccess{SessionSecret: make([]byte, 32)},
	}}
	if _, err := control.AcceptOutbound(result); err != nil {
		b.Fatalf("accept AuthSuccess: %v", err)
	}
	if err := control.CommitAuthSuccessAfterFlush(result); err != nil {
		b.Fatalf("commit AuthSuccess: %v", err)
	}
	var secret [32]byte
	for index := range secret {
		secret[index] = byte(index + 1)
	}
	return servercontrolauth.Established{
		Session: session, SessionSecret: secret, ProtocolVersion: 1,
		HeartbeatInterval: connectorSelectionBenchmarkHeartbeatInterval, Control: control,
	}
}

func connectorSelectionBenchmarkReadDemand(b *testing.B, connection net.Conn) {
	b.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		b.Fatalf("set Control read deadline: %v", err)
	}
	envelope := &protocolv1.ControlEnvelope{}
	if err := frame.ReadControl(connection, envelope); err != nil {
		b.Fatalf("read initial Control message: %v", err)
	}
	if snapshot := envelope.GetConfigSnapshot(); snapshot != nil {
		ack := &protocolv1.ControlEnvelope{
			ProtocolVersion: envelope.GetProtocolVersion(),
			Payload: &protocolv1.ControlEnvelope_ConfigAck{ConfigAck: &protocolv1.ConfigAck{
				ObservedRevision: snapshot.GetRevision(),
				ApplyStatus:      protocolv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_APPLIED,
				ErrorCode:        protocolv1.ErrorCode_ERROR_CODE_OK,
			}},
		}
		if err := frame.WriteControl(connection, ack); err != nil {
			b.Fatalf("write ConfigAck: %v", err)
		}
		envelope.Reset()
		if err := frame.ReadControl(connection, envelope); err != nil {
			b.Fatalf("read WorkDemand: %v", err)
		}
	}
	if envelope.GetWorkDemand() == nil {
		b.Fatalf("initial Control message = %#v, want WorkDemand", envelope)
	}
	if err := connection.SetReadDeadline(time.Time{}); err != nil {
		b.Fatalf("clear Control read deadline: %v", err)
	}
}

func connectorSelectionBenchmarkIdle(
	b *testing.B,
	session serverruntime.Session,
	workID string,
) serverworkauth.Idle {
	b.Helper()
	workState, err := state.NewWork(state.EndpointServer)
	if err != nil {
		b.Fatalf("create Work state: %v", err)
	}
	hello := &protocolv1.WorkHello{
		TunnelId: session.TunnelID, ConnectorId: session.ConnectorID,
		SessionId: session.SessionID, WorkId: workID,
		Nonce: make([]byte, 32), Mac: make([]byte, 32),
		BudgetLeaseId: "lease_01J00000000000000000000000",
	}
	if err := workState.AcceptInbound(hello); err != nil {
		b.Fatalf("accept WorkHello: %v", err)
	}
	ready := &protocolv1.WorkReady{
		WorkId: workID, Status: protocolv1.WorkReadyStatus_WORK_READY_STATUS_READY,
	}
	if err := workState.AcceptOutbound(ready); err != nil {
		b.Fatalf("accept WorkReady: %v", err)
	}
	return serverworkauth.Idle{
		TunnelID: session.TunnelID, ConnectorID: session.ConnectorID,
		SessionID: session.SessionID, WorkID: workID, State: workState,
	}
}

func connectorSelectionBenchmarkWait(b *testing.B, condition func() bool) {
	b.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			b.Fatal("Connector benchmark fixture did not become ready")
		}
		time.Sleep(time.Millisecond)
	}
}
