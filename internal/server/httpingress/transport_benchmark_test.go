package httpingress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/state"
	servercontrolauth "github.com/lifei6671/xtunnel/internal/server/controlauth"
	serveropen "github.com/lifei6671/xtunnel/internal/server/open"
	serverroute "github.com/lifei6671/xtunnel/internal/server/route"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
	"github.com/lifei6671/xtunnel/internal/server/sessionruntime"
	serversnapshot "github.com/lifei6671/xtunnel/internal/server/snapshot"
	serverworkauth "github.com/lifei6671/xtunnel/internal/server/workauth"
	serverworkpool "github.com/lifei6671/xtunnel/internal/server/workpool"
	"github.com/lifei6671/xtunnel/internal/tunnel"
)

const (
	benchmarkHTTPConnectorID = "con_01J00000000000000000000000"
	benchmarkHTTPBody        = "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
		"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
		"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
		"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
		"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
		"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
		"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
		"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
		"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
		"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
		"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
		"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
		"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
		"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
		"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" +
		"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
	benchmarkHTTPWorkConns         = 128
	benchmarkHTTPTimeout           = 5 * time.Second
	benchmarkHTTPHeartbeatInterval = 10 * time.Minute
	benchmarkHTTPHeartbeatTimeout  = 30 * time.Minute
)

// BenchmarkHTTP1WorkConnCapacity 装配生产 transportPool、Tunnel Proxy、Session
// Manager、WorkPool 与 OPEN 握手。每条 HTTP/1.1 Transport 连接都实际取得并激活一条
// WorkConn；同批请求由有界 barrier 保持同时 in-flight，批次之间验证 KeepAlive 复用。
func BenchmarkHTTP1WorkConnCapacity(b *testing.B) {
	for _, concurrency := range []int{1, 16, 64, 100, 128} {
		b.Run(fmt.Sprintf("concurrency_%d", concurrency), func(b *testing.B) {
			fixture := newHTTP1WorkConnBenchmarkFixture(b)

			// 该 Benchmark 使用有限的 128 条真实 WorkConn。把本地 Transport idle
			// 保留值设为相同容量，使计时阶段测量稳态复用，而不会因每轮淘汰连接
			// 不断消耗 fixture；这不修改任何产品默认值。
			transport := fixture.transports.transport(1, serverroute.HTTPRoute{
				TunnelID: testTunnelID, ServiceID: testServiceID, RequiredRevision: 0,
				ProxyOptions: serverroute.HTTPProxyOptions{
					MaxIdleConnections: benchmarkHTTPWorkConns, IdleConnectionTimeoutMS: 90_000,
				},
			})

			if err := runHTTP1WorkConnBatch(transport, fixture, concurrency); err != nil {
				b.Fatalf("warm HTTP/1.1 WorkConn batch: %v", err)
			}
			fixture.waitPoolCounts(b, func(counts serverworkpool.Counts) bool {
				return counts.Active == uint32(concurrency)
			})
			established := fixture.dialer.successful.Load()
			if established != int64(concurrency) {
				b.Fatalf("warm WorkConn dials = %d, want %d", established, concurrency)
			}
			fixture.dialer.successful.Store(0)
			fixture.peakActive.Store(int64(fixture.pool.Snapshot().Active))

			b.ReportAllocs()
			b.SetBytes(int64(concurrency * len(benchmarkHTTPBody)))
			b.ResetTimer()
			for range b.N {
				if err := runHTTP1WorkConnBatch(transport, fixture, concurrency); err != nil {
					b.Fatalf("HTTP/1.1 WorkConn batch: %v", err)
				}
			}
			b.StopTimer()

			requests := int64(b.N * concurrency)
			dials := fixture.dialer.successful.Load()
			peak := fixture.peakActive.Load()
			elapsed := b.Elapsed()
			if err := fixture.close(); err != nil {
				b.Fatalf("close HTTP/1.1 WorkConn fixture: %v", err)
			}
			counts := fixture.pool.Snapshot()

			b.ReportMetric(float64(established), "established_workconns")
			b.ReportMetric(float64(dials)/float64(requests), "workconn_dials/request")
			b.ReportMetric(100*(1-float64(dials)/float64(requests)), "keepalive_reuse_percent")
			b.ReportMetric(float64(peak), "peak_active_workconns")
			b.ReportMetric(float64(counts.Active), "active_workconns_end")
			b.ReportMetric(float64(counts.Total), "total_workconns_end")
			if elapsed > 0 {
				b.ReportMetric(float64(requests)/elapsed.Seconds(), "requests/s")
			}
		})
	}
}

func TestHTTP1WorkConnBenchmarkBatchReleasesBarrierOnError(t *testing.T) {
	fixture := &http1WorkConnBenchmarkFixture{}
	injected := errors.New("injected RoundTrip failure")
	transport := &http1WorkConnBenchmarkFailingTransport{fixture: fixture, err: injected}

	started := time.Now()
	err := runHTTP1WorkConnBatch(transport, fixture, 16)
	if !errors.Is(err, injected) {
		t.Fatalf("runHTTP1WorkConnBatch() error = %v, want injected failure", err)
	}
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("error convergence took %v, want less than 2s", elapsed)
	}
	batch := fixture.batch.Load()
	if batch == nil {
		t.Fatal("runHTTP1WorkConnBatch() did not publish a barrier")
	}
	select {
	case <-batch.release:
	default:
		t.Fatal("error path did not release the barrier")
	}
}

type http1WorkConnBenchmarkFailingTransport struct {
	fixture *http1WorkConnBenchmarkFixture
	err     error
	calls   atomic.Int64
}

func (transport *http1WorkConnBenchmarkFailingTransport) RoundTrip(
	request *http.Request,
) (*http.Response, error) {
	if transport.calls.Add(1) == 1 {
		return nil, transport.err
	}
	batch := transport.fixture.batch.Load()
	if batch == nil {
		return nil, errors.New("benchmark barrier is unavailable")
	}
	select {
	case <-batch.release:
		return nil, transport.err
	case <-request.Context().Done():
		return nil, request.Context().Err()
	}
}

type http1WorkConnBenchmarkSnapshotProvider struct{}

func (http1WorkConnBenchmarkSnapshotProvider) Current(
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

type http1WorkConnBenchmarkFixture struct {
	transports *transportPool
	dialer     *http1WorkConnBenchmarkDialer
	sessions   *sessionruntime.Manager
	pool       *serverworkpool.Pool

	batch      atomic.Pointer[http1WorkConnBenchmarkBatch]
	peakActive atomic.Int64
	control    net.Conn
	controlEnd <-chan error
	workPeers  []net.Conn
	peerWait   sync.WaitGroup
	closeOnce  sync.Once
	closeErr   error
}

type http1WorkConnBenchmarkDialer struct {
	proxy      *tunnel.Proxy
	successful atomic.Int64
}

func (dialer *http1WorkConnBenchmarkDialer) Dial(
	ctx context.Context,
	request tunnel.DialRequest,
) (net.Conn, error) {
	connection, err := dialer.proxy.Dial(ctx, request)
	if err != nil {
		return nil, err
	}
	dialer.successful.Add(1)
	return connection, nil
}

func newHTTP1WorkConnBenchmarkFixture(b *testing.B) *http1WorkConnBenchmarkFixture {
	b.Helper()
	registry := serverruntime.NewRegistry()
	// Benchmark fixture 不运行完整 Agent heartbeat writer；长采样及校准可能超过
	// 生产默认窗口，因此只延长测试 Session 的超时。fixture.close 仍按短 Context
	// 主动关闭 Transport、WorkPool 与 Control Owner。
	sessions, err := sessionruntime.New(registry, sessionruntime.Options{
		HighPriorityCapacity: 16,
		NormalCapacity:       32,
		InboundCapacity:      16,
		WriteTimeout:         time.Second,
		MaxReplayEntries:     128,
		MaxWorkTotal:         benchmarkHTTPWorkConns,
		MaxWorkConnecting:    benchmarkHTTPWorkConns,
		HeartbeatTimeout:     benchmarkHTTPHeartbeatTimeout,
		SnapshotProvider:     http1WorkConnBenchmarkSnapshotProvider{},
		Logger:               slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		b.Fatalf("create Session Manager: %v", err)
	}
	if err := sessions.Start(context.Background()); err != nil {
		b.Fatalf("start Session Manager: %v", err)
	}
	fixture := &http1WorkConnBenchmarkFixture{sessions: sessions}
	b.Cleanup(func() {
		if err := fixture.close(); err != nil {
			b.Errorf("cleanup HTTP/1.1 WorkConn fixture: %v", err)
		}
	})

	pending, err := registry.ReserveAuthenticated(testTunnelID, benchmarkHTTPConnectorID)
	if err != nil {
		b.Fatalf("reserve benchmark Connector: %v", err)
	}
	session, err := registry.CommitAuthenticated(pending)
	if err != nil {
		b.Fatalf("commit benchmark Connector: %v", err)
	}
	serverControl, agentControl := net.Pipe()
	fixture.control = agentControl
	established := http1WorkConnBenchmarkEstablished(b, session)
	controlEnd := make(chan error, 1)
	fixture.controlEnd = controlEnd
	go func() {
		controlEnd <- sessions.Serve(context.Background(), serverControl, &established)
	}()
	http1WorkConnBenchmarkReadDemand(b, agentControl)
	http1WorkConnBenchmarkWait(b, func() bool {
		pool, ready := sessions.Pool(session)
		if ready && registry.EligibleAtRevision(session, testServiceID, 0) {
			fixture.pool = pool
			return true
		}
		return false
	})

	for index := range benchmarkHTTPWorkConns {
		serverWork, agentWork := net.Pipe()
		fixture.workPeers = append(fixture.workPeers, agentWork)
		workID := fmt.Sprintf("work_01J%023d", index)
		if _, err := sessions.RegisterIdle(
			serverWork,
			http1WorkConnBenchmarkIdle(b, session, workID),
		); err != nil {
			_ = serverWork.Close()
			b.Fatalf("register benchmark WorkConn %d: %v", index, err)
		}
		fixture.peerWait.Add(1)
		go func() {
			defer fixture.peerWait.Done()
			serveHTTP1BenchmarkWorkConn(agentWork, fixture)
		}()
	}

	openHandler, err := serveropen.NewHandler(serveropen.Options{
		HandshakeTimeout: time.Second,
		WriteTimeout:     time.Second,
		ReadTimeout:      time.Second,
	})
	if err != nil {
		b.Fatalf("create OPEN handler: %v", err)
	}
	tunnelProxy, err := tunnel.NewProxy(tunnel.Options{
		Registry: registry, Sessions: sessions, OpenHandler: openHandler,
		AcquireTimeout: benchmarkHTTPTimeout,
		Logger:         slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		b.Fatalf("create Tunnel Proxy: %v", err)
	}
	fixture.dialer = &http1WorkConnBenchmarkDialer{proxy: tunnelProxy}
	fixture.transports = newTransportPool(fixture.dialer)
	return fixture
}

func (fixture *http1WorkConnBenchmarkFixture) observeActive() {
	active := int64(fixture.pool.Snapshot().Active)
	for peak := fixture.peakActive.Load(); active > peak && !fixture.peakActive.CompareAndSwap(peak, active); peak = fixture.peakActive.Load() {
	}
}

func (fixture *http1WorkConnBenchmarkFixture) waitPoolCounts(
	b *testing.B,
	condition func(serverworkpool.Counts) bool,
) {
	b.Helper()
	deadline := time.Now().Add(benchmarkHTTPTimeout)
	for !condition(fixture.pool.Snapshot()) {
		if time.Now().After(deadline) {
			b.Fatalf("WorkPool counts = %#v before deadline", fixture.pool.Snapshot())
		}
		time.Sleep(time.Millisecond)
	}
}

func (fixture *http1WorkConnBenchmarkFixture) close() error {
	fixture.closeOnce.Do(func() {
		if batch := fixture.batch.Load(); batch != nil {
			batch.releaseAll()
		}
		if fixture.transports != nil {
			fixture.transports.closeIdleConnections()
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if fixture.sessions != nil {
			fixture.closeErr = errors.Join(fixture.closeErr, fixture.sessions.Shutdown(ctx))
		}
		if fixture.control != nil {
			fixture.closeErr = errors.Join(fixture.closeErr, fixture.control.Close())
		}
		for _, connection := range fixture.workPeers {
			fixture.closeErr = errors.Join(fixture.closeErr, connection.Close())
		}

		peersDone := make(chan struct{})
		go func() {
			fixture.peerWait.Wait()
			close(peersDone)
		}()
		select {
		case <-peersDone:
		case <-ctx.Done():
			fixture.closeErr = errors.Join(fixture.closeErr, fmt.Errorf("wait benchmark WorkConn peers: %w", ctx.Err()))
		}
		if fixture.controlEnd != nil {
			select {
			case <-fixture.controlEnd:
			case <-ctx.Done():
				fixture.closeErr = errors.Join(fixture.closeErr, fmt.Errorf("wait benchmark Control Session: %w", ctx.Err()))
			}
		}
		if fixture.pool != nil {
			counts := fixture.pool.Snapshot()
			if counts.Active != 0 || counts.Total != 0 {
				fixture.closeErr = errors.Join(
					fixture.closeErr,
					fmt.Errorf("WorkPool did not converge: counts=%#v", counts),
				)
			}
		}
	})
	return fixture.closeErr
}

type http1WorkConnBenchmarkBatch struct {
	expected    int64
	arrived     atomic.Int64
	ready       chan struct{}
	release     chan struct{}
	readyOnce   sync.Once
	releaseOnce sync.Once
}

func (batch *http1WorkConnBenchmarkBatch) arrive() {
	if batch.arrived.Add(1) == batch.expected {
		batch.readyOnce.Do(func() {
			close(batch.ready)
		})
		batch.releaseAll()
	}
}

func (batch *http1WorkConnBenchmarkBatch) releaseAll() {
	batch.releaseOnce.Do(func() {
		close(batch.release)
	})
}

func runHTTP1WorkConnBatch(
	transport http.RoundTripper,
	fixture *http1WorkConnBenchmarkFixture,
	concurrency int,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), benchmarkHTTPTimeout)
	defer cancel()
	batch := &http1WorkConnBenchmarkBatch{
		expected: int64(concurrency), ready: make(chan struct{}), release: make(chan struct{}),
	}
	fixture.batch.Store(batch)
	defer batch.releaseAll()

	errs := make([]error, concurrency)
	firstErr := make(chan error, 1)
	var requests sync.WaitGroup
	requests.Add(concurrency)
	for index := range concurrency {
		go func() {
			defer requests.Done()
			err := http1WorkConnBenchmarkRoundTrip(ctx, transport)
			errs[index] = err
			if err != nil {
				select {
				case firstErr <- err:
				default:
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		requests.Wait()
		close(done)
	}()

	var triggerErr error
	select {
	case <-batch.ready:
	case triggerErr = <-firstErr:
		batch.releaseAll()
	case <-ctx.Done():
		triggerErr = fmt.Errorf("wait HTTP/1.1 WorkConn barrier: %w", ctx.Err())
		batch.releaseAll()
	}
	select {
	case <-done:
	case <-ctx.Done():
		batch.releaseAll()
		cleanupTimer := time.NewTimer(benchmarkHTTPTimeout)
		defer cleanupTimer.Stop()
		select {
		case <-done:
		case <-cleanupTimer.C:
			return errors.Join(triggerErr, errors.New("HTTP/1.1 WorkConn requests did not exit after cancellation"))
		}
	}
	if triggerErr != nil {
		return triggerErr
	}
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	if arrived := batch.arrived.Load(); arrived != int64(concurrency) {
		return fmt.Errorf("HTTP/1.1 WorkConn batch arrivals = %d, want %d", arrived, concurrency)
	}
	return nil
}

func http1WorkConnBenchmarkRoundTrip(ctx context.Context, transport http.RoundTripper) error {
	ctx = context.WithValue(ctx, clientAddressKey{}, "192.0.2.1")
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet, "http://"+transportAuthority+"/benchmark", nil,
	)
	if err != nil {
		return fmt.Errorf("build benchmark request: %w", err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		return fmt.Errorf("round trip benchmark request: %w", err)
	}
	readBytes, readErr := io.Copy(io.Discard, response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	if readBytes != int64(len(benchmarkHTTPBody)) {
		return fmt.Errorf("benchmark response bytes = %d, want %d", readBytes, len(benchmarkHTTPBody))
	}
	return nil
}

func serveHTTP1BenchmarkWorkConn(connection net.Conn, fixture *http1WorkConnBenchmarkFixture) {
	defer connection.Close()
	openRequest := &protocolv1.OpenRequest{}
	if err := frame.ReadWork(connection, openRequest); err != nil {
		return
	}
	openResponse := &protocolv1.OpenResponse{
		ConnectionId: openRequest.GetConnectionId(),
		Status:       protocolv1.OpenStatus_OPEN_STATUS_OK,
		ErrorCode:    protocolv1.ErrorCode_ERROR_CODE_OK,
	}
	if err := frame.WriteWork(connection, openResponse); err != nil {
		return
	}

	reader := bufio.NewReader(connection)
	writer := bufio.NewWriter(connection)
	for {
		request, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		if err := request.Body.Close(); err != nil {
			return
		}
		batch := fixture.batch.Load()
		if batch == nil {
			return
		}
		fixture.observeActive()
		batch.arrive()
		<-batch.release
		if _, err := io.WriteString(writer, "HTTP/1.1 200 OK\r\nContent-Length: 1024\r\n\r\n"); err != nil {
			return
		}
		if _, err := writer.WriteString(benchmarkHTTPBody); err != nil {
			return
		}
		if err := writer.Flush(); err != nil {
			return
		}
	}
}

func http1WorkConnBenchmarkEstablished(
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
		HeartbeatInterval: benchmarkHTTPHeartbeatInterval, Control: control,
	}
}

func http1WorkConnBenchmarkReadDemand(b *testing.B, connection net.Conn) {
	b.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(benchmarkHTTPTimeout)); err != nil {
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

func http1WorkConnBenchmarkIdle(
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

func http1WorkConnBenchmarkWait(b *testing.B, condition func() bool) {
	b.Helper()
	deadline := time.Now().Add(benchmarkHTTPTimeout)
	for !condition() {
		if time.Now().After(deadline) {
			b.Fatal("HTTP/1.1 WorkConn benchmark fixture did not become ready")
		}
		time.Sleep(time.Millisecond)
	}
}
