package workpool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/controlauth"
	agentopen "github.com/lifei6671/xtunnel/internal/agent/open"
	"github.com/lifei6671/xtunnel/internal/agent/workauth"
	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/protocol/deterministic"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/state"
	connectiontoken "github.com/lifei6671/xtunnel/internal/protocol/token"
	"github.com/lifei6671/xtunnel/internal/safego"
	servergateway "github.com/lifei6671/xtunnel/internal/server/gateway"
)

var _ Handler = (*agentopen.Handler)(nil)
var _ phaseObservingHandler = (*agentopen.Handler)(nil)

const (
	testTunnelID     = "tun_01ARZ3NDEKTSV4RRFFQ69G5FAV"
	testTokenID      = "tok_01ARZ3NDEKTSV4RRFFQ69G5FAW"
	testConnectorID  = "con_01ARZ3NDEKTSV4RRFFQ69G5FAX"
	testSessionID    = "sess_01ARZ3NDEKTSV4RRFFQ69G5FAY"
	testSessionIDTwo = "sess_01ARZ3NDEKTSV4RRFFQ69G5FB2"
	testLeaseID      = "lease_01ARZ3NDEKTSV4RRFFQ69G5FAZ"
	testConnectionID = "conn_01ARZ3NDEKTSV4RRFFQ69G5FB0"
	testServiceID    = "svc_01ARZ3NDEKTSV4RRFFQ69G5FB1"
)

func TestPoolPerformsRealWorkAuthenticationBeforeHandler(t *testing.T) {
	sessionDone := make(chan struct{})
	config := testConfig(t, sessionDone)
	type handoff struct {
		connection net.Conn
		ready      *workauth.Ready
	}
	received := make(chan handoff, 1)
	config.Handler = HandlerFunc(func(ctx context.Context, connection net.Conn, ready *workauth.Ready) error {
		received <- handoff{connection: connection, ready: ready}
		<-ctx.Done()
		return ctx.Err()
	})
	serverResult := make(chan error, 1)
	dial := func(_ context.Context, token, alpn string) (net.Conn, error) {
		if token != config.ConnectionToken {
			return nil, errors.New("Work Dial 未逐字节复用 Connection Token")
		}
		if alpn != servergateway.WorkALPN {
			return nil, fmt.Errorf("Work Dial ALPN = %q", alpn)
		}
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			serverResult <- serveWorkAuthentication(server, config.Session, testLeaseID)
		}()
		return client, nil
	}
	pool, err := newPool(config, dependencies{
		dial:         dial,
		authenticate: workauth.Authenticate,
		now:          time.Now,
	})
	if err != nil {
		t.Fatalf("newPool() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := pool.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	result, err := pool.ApplyDemand(demand(testLeaseID, 1, 1, 1, 1_000))
	if err != nil {
		t.Fatalf("ApplyDemand() error = %v", err)
	}
	if !result.Accepted || result.Started != 1 {
		t.Fatalf("ApplyDemand() result = %+v", result)
	}

	select {
	case handed := <-received:
		if handed.connection == nil || handed.ready.WorkID == "" || handed.ready.State == nil || handed.ready.State.Phase() != state.WorkIdle {
			t.Fatalf("Handler 收到的 handoff = %+v", handed)
		}
	case <-time.After(time.Second):
		t.Fatal("等待已认证 IDLE WorkConn 超时")
	}
	waitFor(t, func() bool {
		stats := pool.Stats()
		return stats.Connecting == 0 && stats.Idle == 1 && stats.Total == 1
	})

	cancel()
	if err := pool.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() error = %v, want context.Canceled", err)
	}
	if err := <-serverResult; err != nil {
		t.Fatalf("Work auth server error = %v", err)
	}
	stats := pool.Stats()
	if stats.Connecting != 0 || stats.Idle != 0 || stats.Total != 0 {
		t.Fatalf("关闭后 Stats = %+v", stats)
	}
}

func TestPoolWorkerPanicShutsDownAndReturnsFailure(t *testing.T) {
	sessionDone := make(chan struct{})
	budget := NewBudget()
	pool, err := newPoolWithBudget(testConfig(t, sessionDone), dependencies{
		dial: func(context.Context, string, string) (net.Conn, error) {
			panic("WorkConn Dial panic must not escape its goroutine")
		},
		authenticate: func(context.Context, net.Conn, workauth.Config) (*workauth.Ready, error) {
			return nil, errors.New("authenticate must not run after Dial panic")
		},
		now: time.Now,
	}, budget)
	if err != nil {
		t.Fatalf("newPoolWithBudget() error = %v", err)
	}
	if err := pool.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := pool.ApplyDemand(demand(testLeaseID, 1, 1, 1, 1_000)); err != nil {
		t.Fatalf("ApplyDemand() error = %v", err)
	}

	waitResult := make(chan error, 1)
	go func() { waitResult <- pool.Wait() }()
	select {
	case err := <-waitResult:
		if !errors.Is(err, safego.ErrPanic) {
			t.Fatalf("Wait() error = %v, want safego.ErrPanic", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait() blocked after WorkConn worker panic")
	}
	select {
	case <-pool.Done():
	case <-time.After(time.Second):
		t.Fatal("Done() did not close after WorkConn worker panic")
	}
	stats := pool.Stats()
	if stats.Total != 0 || stats.Connecting != 0 || stats.Idle != 0 || stats.Opening != 0 || stats.Active != 0 {
		t.Fatalf("Stats after WorkConn worker panic = %+v", stats)
	}
	if used := budget.usedCount(); used != 0 {
		t.Fatalf("shared Budget used = %d, want 0", used)
	}
}

func TestPoolLifetimePanicShutsDownAndReturnsFailure(t *testing.T) {
	pool, err := newPool(testConfig(t, make(chan struct{})), dependencies{
		dial: func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("unused") },
		authenticate: func(context.Context, net.Conn, workauth.Config) (*workauth.Ready, error) {
			return nil, errors.New("unused")
		},
		now: time.Now,
	})
	if err != nil {
		t.Fatalf("newPool() error = %v", err)
	}
	// 注入一个构造后不可能出现的内部状态，验证生命周期 goroutine 的 panic 仍走 Pool shutdown。
	pool.budget = nil
	if err := pool.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := pool.Wait(); !errors.Is(err, safego.ErrPanic) {
		t.Fatalf("Wait() error = %v, want safego.ErrPanic", err)
	}
	select {
	case <-pool.Done():
	case <-time.After(time.Second):
		t.Fatal("Done() did not close after lifetime panic")
	}
}

func TestPoolConcurrentDemandKeepsHighestGenerationAndConnectingBound(t *testing.T) {
	sessionDone := make(chan struct{})
	config := testConfig(t, sessionDone)
	config.Handler = HandlerFunc(func(context.Context, net.Conn, *workauth.Ready) error { return nil })
	var active atomic.Int32
	var maximum atomic.Int32
	dial := func(ctx context.Context, _ string, _ string) (net.Conn, error) {
		current := active.Add(1)
		updateMaximum(&maximum, current)
		defer active.Add(-1)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	pool, err := newPool(config, dependencies{
		dial:         dial,
		authenticate: workauth.Authenticate,
		now:          time.Now,
	})
	if err != nil {
		t.Fatalf("newPool() error = %v", err)
	}
	if err := pool.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	var applies sync.WaitGroup
	for generation := uint64(1); generation <= 64; generation++ {
		generation := generation
		applies.Add(1)
		go func() {
			defer applies.Done()
			_, applyErr := pool.ApplyDemand(demand(testLeaseID, generation, 1_000, 1_000, 5_000))
			if applyErr != nil {
				t.Errorf("ApplyDemand(generation=%d) error = %v", generation, applyErr)
			}
		}()
	}
	applies.Wait()
	waitFor(t, func() bool {
		stats := pool.Stats()
		return stats.DemandGeneration == 64 && stats.Connecting == MaxConnecting && active.Load() == MaxConnecting
	})
	if got := maximum.Load(); got > MaxConnecting {
		t.Fatalf("实际并发 Dial 峰值 = %d, hard limit = %d", got, MaxConnecting)
	}
	stats := pool.Stats()
	if stats.Total > MaxTotal || stats.Connecting > MaxConnecting {
		t.Fatalf("硬上限被突破: %+v", stats)
	}

	close(sessionDone)
	if err := pool.Wait(); err != nil {
		t.Fatalf("SessionDone Wait() error = %v", err)
	}
	if active.Load() != 0 {
		t.Fatalf("Pool 关闭后仍有 %d 个 Dial goroutine", active.Load())
	}
}

func TestPoolClampsTotalAndClosesEveryOwnedConnectionExactlyOnce(t *testing.T) {
	sessionDone := make(chan struct{})
	config := testConfig(t, sessionDone)
	var handled atomic.Int32
	var returned atomic.Int32
	config.Handler = HandlerFunc(func(ctx context.Context, _ net.Conn, _ *workauth.Ready) error {
		handled.Add(1)
		<-ctx.Done()
		returned.Add(1)
		return ctx.Err()
	})
	var connectionsMu sync.Mutex
	connections := make([]*countingConn, 0, MaxTotal)
	dial := func(context.Context, string, string) (net.Conn, error) {
		connection := &countingConn{}
		connectionsMu.Lock()
		connections = append(connections, connection)
		connectionsMu.Unlock()
		return connection, nil
	}
	pool, err := newPool(config, dependencies{
		dial: dial,
		authenticate: func(_ context.Context, _ net.Conn, authConfig workauth.Config) (*workauth.Ready, error) {
			return authenticatedReady(authConfig)
		},
		now: time.Now,
	})
	if err != nil {
		t.Fatalf("newPool() error = %v", err)
	}
	if err := pool.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	result, err := pool.ApplyDemand(demand(testLeaseID, 1, ^uint32(0), ^uint32(0), 5_000))
	if err != nil {
		t.Fatalf("ApplyDemand() error = %v", err)
	}
	if result.Started != MaxConnecting {
		t.Fatalf("首批 Started = %d, want %d", result.Started, MaxConnecting)
	}
	waitFor(t, func() bool {
		stats := pool.Stats()
		return stats.Total == MaxTotal && stats.Idle == MaxTotal && stats.Connecting == 0 && handled.Load() == MaxTotal
	})
	stats := pool.Stats()
	if stats.Total != MaxTotal || stats.Total > MaxTotal {
		t.Fatalf("max_total 钳制失败: %+v", stats)
	}

	close(sessionDone)
	if err := pool.Wait(); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if returned.Load() != MaxTotal {
		t.Fatalf("Handler 退出数 = %d, want %d", returned.Load(), MaxTotal)
	}
	connectionsMu.Lock()
	defer connectionsMu.Unlock()
	if len(connections) != MaxTotal {
		t.Fatalf("建立连接数 = %d, want %d", len(connections), MaxTotal)
	}
	for index, connection := range connections {
		if got := connection.closeCalls.Load(); got != 1 {
			t.Fatalf("connection[%d] Close 次数 = %d, want 1", index, got)
		}
	}
	if stats := pool.Stats(); stats.Total != 0 || stats.Connecting != 0 || stats.Idle != 0 || stats.Failures != 0 || stats.LastFailure != nil {
		t.Fatalf("关闭后的 Pool Stats = %+v", stats)
	}
}

func TestPoolAuthenticationFailureConsumesLeaseWithoutCounterUnderflow(t *testing.T) {
	sessionDone := make(chan struct{})
	config := testConfig(t, sessionDone)
	config.Handler = HandlerFunc(func(context.Context, net.Conn, *workauth.Ready) error { return nil })
	wantErr := errors.New("test WorkHello rejection")
	var dialCount atomic.Int32
	var connectionsMu sync.Mutex
	connections := make([]*countingConn, 0, 3)
	pool, err := newPool(config, dependencies{
		dial: func(context.Context, string, string) (net.Conn, error) {
			dialCount.Add(1)
			connection := &countingConn{}
			connectionsMu.Lock()
			connections = append(connections, connection)
			connectionsMu.Unlock()
			return connection, nil
		},
		authenticate: func(context.Context, net.Conn, workauth.Config) (*workauth.Ready, error) {
			return nil, wantErr
		},
		now: time.Now,
	})
	if err != nil {
		t.Fatalf("newPool() error = %v", err)
	}
	if err := pool.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := pool.ApplyDemand(demand(testLeaseID, 1, 10, 3, 1_000)); err != nil {
		t.Fatalf("ApplyDemand() error = %v", err)
	}
	waitFor(t, func() bool {
		stats := pool.Stats()
		return stats.Total == 0 && stats.Connecting == 0 && stats.Idle == 0 && stats.Failures == 3 && dialCount.Load() == 3
	})
	if stats := pool.Stats(); !errors.Is(stats.LastFailure, wantErr) {
		t.Fatalf("LastFailure = %v, want auth failure", stats.LastFailure)
	}
	connectionsMu.Lock()
	for index, connection := range connections {
		if got := connection.closeCalls.Load(); got != 1 {
			t.Fatalf("failed connection[%d] Close 次数 = %d", index, got)
		}
	}
	connectionsMu.Unlock()

	close(sessionDone)
	if err := pool.Wait(); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
}

func TestPoolLeaseDeadlineStopsFurtherRefill(t *testing.T) {
	sessionDone := make(chan struct{})
	config := testConfig(t, sessionDone)
	config.Handler = HandlerFunc(func(context.Context, net.Conn, *workauth.Ready) error { return nil })
	var dialCount atomic.Int32
	var active atomic.Int32
	var maximum atomic.Int32
	pool, err := newPool(config, dependencies{
		dial: func(ctx context.Context, _ string, _ string) (net.Conn, error) {
			dialCount.Add(1)
			current := active.Add(1)
			updateMaximum(&maximum, current)
			defer active.Add(-1)
			<-ctx.Done()
			return nil, ctx.Err()
		},
		authenticate: workauth.Authenticate,
		now:          time.Now,
	})
	if err != nil {
		t.Fatalf("newPool() error = %v", err)
	}
	if err := pool.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := pool.ApplyDemand(demand(testLeaseID, 1, 100, 100, 20)); err != nil {
		t.Fatalf("ApplyDemand() error = %v", err)
	}
	waitFor(t, func() bool {
		stats := pool.Stats()
		return stats.Total == 0 && stats.Failures == MaxConnecting && active.Load() == 0
	})
	if got := dialCount.Load(); got != MaxConnecting {
		t.Fatalf("Lease 过期后的总 Dial 数 = %d, want %d", got, MaxConnecting)
	}
	if got := maximum.Load(); got > MaxConnecting {
		t.Fatalf("Lease 测试并发峰值 = %d, limit %d", got, MaxConnecting)
	}

	close(sessionDone)
	if err := pool.Wait(); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
}

func TestPoolLowerGenerationCancelsOlderConnecting(t *testing.T) {
	sessionDone := make(chan struct{})
	config := testConfig(t, sessionDone)
	started := make(chan struct{}, 2)
	pool, err := newPool(config, dependencies{
		dial: func(ctx context.Context, _ string, _ string) (net.Conn, error) {
			started <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		},
		authenticate: workauth.Authenticate,
		now:          time.Now,
	})
	if err != nil {
		t.Fatalf("newPool() error = %v", err)
	}
	if err := pool.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := pool.ApplyDemand(demand(testLeaseID, 1, 2, 2, 5_000)); err != nil {
		t.Fatalf("ApplyDemand(grant) error = %v", err)
	}
	<-started
	<-started
	waitFor(t, func() bool { return pool.Stats().Connecting == 2 })

	result, err := pool.ApplyDemand(demand("", 2, 0, 0, 0))
	if err != nil {
		t.Fatalf("ApplyDemand(cancel) error = %v", err)
	}
	if !result.Accepted || result.CanceledConnecting != 2 || result.Started != 0 {
		t.Fatalf("ApplyDemand(cancel) result = %+v", result)
	}
	waitFor(t, func() bool {
		stats := pool.Stats()
		return stats.Connecting == 0 && stats.Total == 0
	})
	if stats := pool.Stats(); stats.Failures != 0 || stats.LastFailure != nil {
		t.Fatalf("canceled CONNECTING was recorded as failure: %+v", stats)
	}

	close(sessionDone)
	if err := pool.Wait(); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
}

func TestPoolObservesOpenPhasesRefillsActiveAndTrimsOnlyIdle(t *testing.T) {
	sessionDone := make(chan struct{})
	config := testConfig(t, sessionDone)
	openingReached := make(chan struct{}, 1)
	allowOrigin := make(chan struct{})
	activeReached := make(chan struct{}, 1)
	releaseActive := make(chan struct{})
	originPeers := make(chan net.Conn, 1)

	openHandler, err := agentopen.NewHandler(agentopen.Options{
		ReadTimeout:  time.Second,
		WriteTimeout: time.Second,
		Logger:       slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Dialer: agentopen.OriginDialerFunc(func(ctx context.Context, serviceID string) (net.Conn, protocolv1.ErrorCode, error) {
			if serviceID != testServiceID {
				return nil, protocolv1.ErrorCode_ERROR_CODE_SERVICE_NOT_FOUND, errors.New("unexpected service id")
			}
			openingReached <- struct{}{}
			select {
			case <-allowOrigin:
			case <-ctx.Done():
				return nil, protocolv1.ErrorCode_ERROR_CODE_ORIGIN_UNREACHABLE, ctx.Err()
			}
			origin, peer := net.Pipe()
			originPeers <- peer
			return origin, protocolv1.ErrorCode_ERROR_CODE_OK, nil
		}),
		Proxy: func(ctx context.Context, _ net.Conn, _ net.Conn) error {
			activeReached <- struct{}{}
			select {
			case <-releaseActive:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	if err != nil {
		t.Fatalf("open.NewHandler() error = %v", err)
	}
	config.Handler = openHandler

	serverPeers := make(chan net.Conn, 2)
	pool, err := newPool(config, dependencies{
		dial: func(context.Context, string, string) (net.Conn, error) {
			agent, server := net.Pipe()
			serverPeers <- server
			return agent, nil
		},
		authenticate: func(_ context.Context, _ net.Conn, authConfig workauth.Config) (*workauth.Ready, error) {
			return authenticatedReady(authConfig)
		},
		now: time.Now,
	})
	if err != nil {
		t.Fatalf("newPool() error = %v", err)
	}
	if err := pool.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := pool.ApplyDemand(demand(testLeaseID, 1, 2, 2, 5_000)); err != nil {
		t.Fatalf("ApplyDemand(grant) error = %v", err)
	}

	firstPeer := <-serverPeers
	defer firstPeer.Close()
	secondPeer := <-serverPeers
	defer secondPeer.Close()
	if err := frame.WriteWork(firstPeer, &protocolv1.OpenRequest{
		ProtocolVersion: 1,
		ConnectionId:    testConnectionID,
		ServiceId:       testServiceID,
		IngressType:     protocolv1.IngressType_INGRESS_TYPE_TCP,
	}); err != nil {
		t.Fatalf("write first OpenRequest: %v", err)
	}
	select {
	case <-openingReached:
	case <-time.After(time.Second):
		t.Fatal("production OPEN Handler did not enter OPENING")
	}
	waitFor(t, func() bool {
		stats := pool.Stats()
		return stats.Connecting == 0 && stats.Idle == 1 && stats.Opening == 1 && stats.Active == 0 && stats.Total == 2
	})

	// 目标从 2 降到 1 时，只能裁掉另一条真实 IDLE；正在连接 Origin 的 OPENING
	// 必须继续存活，不能被普通 Demand 调池操作中断。
	result, err := pool.ApplyDemand(demand("", 2, 1, 0, 0))
	if err != nil {
		t.Fatalf("ApplyDemand(lower while OPENING) error = %v", err)
	}
	if !result.Accepted || result.ClosedIdle != 1 {
		t.Fatalf("ApplyDemand(lower while OPENING) result = %+v", result)
	}
	if _, err := secondPeer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("trimmed IDLE peer read error = %v, want EOF/closed", err)
	}
	waitFor(t, func() bool {
		stats := pool.Stats()
		return stats.Idle == 0 && stats.Opening == 1 && stats.Active == 0 && stats.Total == 1
	})

	close(allowOrigin)
	response := &protocolv1.OpenResponse{}
	if err := frame.ReadWork(firstPeer, response); err != nil {
		t.Fatalf("read first OpenResponse: %v", err)
	}
	if response.GetStatus() != protocolv1.OpenStatus_OPEN_STATUS_OK {
		t.Fatalf("OpenResponse status = %v, want OK", response.GetStatus())
	}
	select {
	case <-activeReached:
	case <-time.After(time.Second):
		t.Fatal("production OPEN Handler did not enter ACTIVE")
	}
	originPeer := <-originPeers
	defer originPeer.Close()

	// ACTIVE 不计入 desired_non_active；新 Grant 应按目标补出一条新的 IDLE。
	if _, err := pool.ApplyDemand(demand(testLeaseID, 3, 1, 1, 5_000)); err != nil {
		t.Fatalf("ApplyDemand(refill ACTIVE) error = %v", err)
	}
	thirdPeer := <-serverPeers
	defer thirdPeer.Close()
	waitFor(t, func() bool {
		stats := pool.Stats()
		return stats.Connecting == 0 && stats.Idle == 1 && stats.Opening == 0 && stats.Active == 1 && stats.Total == 2
	})

	result, err = pool.ApplyDemand(demand("", 4, 0, 0, 0))
	if err != nil {
		t.Fatalf("ApplyDemand(cancel) error = %v", err)
	}
	if !result.Accepted || result.ClosedIdle != 1 || result.CanceledConnecting != 0 {
		t.Fatalf("ApplyDemand(cancel) result = %+v", result)
	}
	if _, err := thirdPeer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("trimmed IDLE peer read error = %v, want EOF/closed", err)
	}
	waitFor(t, func() bool {
		stats := pool.Stats()
		return stats.Connecting == 0 && stats.Idle == 0 && stats.Opening == 0 && stats.Active == 1 && stats.Total == 1
	})

	// 普通 Demand 降低不能中断 ACTIVE；只有业务 Handler 正常返回后才回收它。
	close(releaseActive)
	waitFor(t, func() bool { return pool.Stats().Total == 0 })
	close(sessionDone)
	if err := pool.Wait(); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
}

func TestPoolSessionDoneDetachesActiveAndWaitDoesNotBlockReconnect(t *testing.T) {
	sessionDone := make(chan struct{})
	releaseActive := make(chan struct{})
	activeReached := make(chan struct{})
	handler := &blockingObservedHandler{activeReached: activeReached, release: releaseActive}
	config := testConfig(t, sessionDone)
	config.Handler = handler
	connection := &countingConn{}
	pool, err := newPool(config, dependencies{
		dial: func(context.Context, string, string) (net.Conn, error) { return connection, nil },
		authenticate: func(_ context.Context, _ net.Conn, authConfig workauth.Config) (*workauth.Ready, error) {
			return authenticatedReady(authConfig)
		},
		now: time.Now,
	})
	if err != nil {
		t.Fatalf("newPool() error = %v", err)
	}
	if err := pool.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := pool.ApplyDemand(demand(testLeaseID, 1, 1, 1, 5_000)); err != nil {
		t.Fatalf("ApplyDemand() error = %v", err)
	}
	select {
	case <-activeReached:
	case <-time.After(time.Second):
		t.Fatal("WorkConn did not enter ACTIVE")
	}

	close(sessionDone)
	waitResult := make(chan error, 1)
	go func() { waitResult <- pool.Wait() }()
	select {
	case err := <-waitResult:
		if err != nil {
			t.Fatalf("Wait() after SessionDone error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait() blocked reconnect on old ACTIVE WorkConn")
	}
	if calls := connection.closeCalls.Load(); calls != 0 {
		t.Fatalf("old ACTIVE close calls after SessionDone = %d, want 0", calls)
	}
	if stats := pool.Stats(); stats.Total != 0 || stats.Active != 0 {
		t.Fatalf("detached old ACTIVE remained in Pool Stats: %+v", stats)
	}

	close(releaseActive)
	select {
	case <-pool.Done():
	case <-time.After(time.Second):
		t.Fatal("Pool Done did not close after detached ACTIVE finished")
	}
	if calls := connection.closeCalls.Load(); calls != 1 {
		t.Fatalf("old ACTIVE close calls after natural finish = %d, want 1", calls)
	}
}

func TestSharedBudgetCountsDetachedActiveAcrossSessionGenerations(t *testing.T) {
	budget := NewBudget()
	firstSessionDone := make(chan struct{})
	releaseActive := make(chan struct{})
	activeReached := make(chan struct{})
	firstConfig := testConfig(t, firstSessionDone)
	firstConfig.Handler = &blockingObservedHandler{activeReached: activeReached, release: releaseActive}
	firstPool, err := newPoolWithBudget(firstConfig, dependencies{
		dial: func(context.Context, string, string) (net.Conn, error) { return &countingConn{}, nil },
		authenticate: func(_ context.Context, _ net.Conn, authConfig workauth.Config) (*workauth.Ready, error) {
			return authenticatedReady(authConfig)
		},
		now: time.Now,
	}, budget)
	if err != nil {
		t.Fatalf("newPoolWithBudget(first) error = %v", err)
	}
	if err := firstPool.Start(context.Background()); err != nil {
		t.Fatalf("Start(first) error = %v", err)
	}
	if _, err := firstPool.ApplyDemand(demand(testLeaseID, 1, 1, 1, 5_000)); err != nil {
		t.Fatalf("ApplyDemand(first) error = %v", err)
	}
	select {
	case <-activeReached:
	case <-time.After(time.Second):
		t.Fatal("first generation WorkConn did not enter ACTIVE")
	}
	close(firstSessionDone)
	if err := firstPool.Wait(); err != nil {
		t.Fatalf("Wait(first) error = %v", err)
	}

	secondSessionDone := make(chan struct{})
	secondConfig := testConfig(t, secondSessionDone)
	secondConfig.Session.SessionID = testSessionIDTwo
	secondConfig.Handler = HandlerFunc(func(ctx context.Context, _ net.Conn, _ *workauth.Ready) error {
		<-ctx.Done()
		return ctx.Err()
	})
	secondPool, err := newPoolWithBudget(secondConfig, dependencies{
		dial: func(context.Context, string, string) (net.Conn, error) { return &countingConn{}, nil },
		authenticate: func(_ context.Context, _ net.Conn, authConfig workauth.Config) (*workauth.Ready, error) {
			return authenticatedReady(authConfig)
		},
		now: time.Now,
	}, budget)
	if err != nil {
		t.Fatalf("newPoolWithBudget(second) error = %v", err)
	}
	if err := secondPool.Start(context.Background()); err != nil {
		t.Fatalf("Start(second) error = %v", err)
	}
	if _, err := secondPool.ApplyDemand(demand(testLeaseID, 1, MaxTotal, MaxTotal, 5_000)); err != nil {
		t.Fatalf("ApplyDemand(second) error = %v", err)
	}
	waitFor(t, func() bool {
		stats := secondPool.Stats()
		return stats.Total == MaxTotal-1 && stats.Idle == MaxTotal-1 && stats.Connecting == 0
	})
	if used := budget.usedCount(); used != MaxTotal {
		t.Fatalf("shared Budget used = %d, want %d including detached ACTIVE", used, MaxTotal)
	}

	close(releaseActive)
	select {
	case <-firstPool.Done():
	case <-time.After(time.Second):
		t.Fatal("first generation detached ACTIVE did not finish")
	}
	waitFor(t, func() bool {
		stats := secondPool.Stats()
		return stats.Total == MaxTotal && stats.Idle == MaxTotal && stats.Connecting == 0
	})

	close(secondSessionDone)
	if err := secondPool.Wait(); err != nil {
		t.Fatalf("Wait(second) error = %v", err)
	}
	if used := budget.usedCount(); used != 0 {
		t.Fatalf("shared Budget used after shutdown = %d, want 0", used)
	}
}

func TestPoolAuthenticationCompletionCannotCommitCanceledWorkToIdle(t *testing.T) {
	for _, test := range []struct {
		name   string
		ttlMS  uint32
		cancel func(*testing.T, *Pool, <-chan struct{})
	}{
		{
			name:  "drain",
			ttlMS: 5_000,
			cancel: func(t *testing.T, pool *Pool, _ <-chan struct{}) {
				t.Helper()
				if err := pool.BeginDrain(); err != nil {
					t.Fatalf("BeginDrain() error = %v", err)
				}
			},
		},
		{
			name:  "new demand generation",
			ttlMS: 5_000,
			cancel: func(t *testing.T, pool *Pool, _ <-chan struct{}) {
				t.Helper()
				if _, err := pool.ApplyDemand(demand("", 2, 0, 0, 0)); err != nil {
					t.Fatalf("ApplyDemand(new generation) error = %v", err)
				}
			},
		},
		{
			name:  "attempt context deadline",
			ttlMS: 10,
			cancel: func(t *testing.T, _ *Pool, attemptDone <-chan struct{}) {
				t.Helper()
				select {
				case <-attemptDone:
				case <-time.After(time.Second):
					t.Fatal("attempt context deadline did not expire")
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			sessionDone := make(chan struct{})
			authStarted := make(chan struct{})
			attemptDone := make(chan (<-chan struct{}), 1)
			releaseAuth := make(chan struct{})
			var handlerCalls atomic.Int32
			config := testConfig(t, sessionDone)
			config.Handler = HandlerFunc(func(context.Context, net.Conn, *workauth.Ready) error {
				handlerCalls.Add(1)
				return nil
			})
			connection := &countingConn{}
			pool, err := newPool(config, dependencies{
				dial: func(context.Context, string, string) (net.Conn, error) { return connection, nil },
				authenticate: func(ctx context.Context, _ net.Conn, authConfig workauth.Config) (*workauth.Ready, error) {
					attemptDone <- ctx.Done()
					close(authStarted)
					<-releaseAuth
					return authenticatedReady(authConfig)
				},
				now: time.Now,
			})
			if err != nil {
				t.Fatalf("newPool() error = %v", err)
			}
			if err := pool.Start(context.Background()); err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			if _, err := pool.ApplyDemand(demand(testLeaseID, 1, 1, 1, test.ttlMS)); err != nil {
				t.Fatalf("ApplyDemand() error = %v", err)
			}
			select {
			case <-authStarted:
			case <-time.After(time.Second):
				t.Fatal("authentication did not start")
			}
			test.cancel(t, pool, <-attemptDone)
			close(releaseAuth)
			waitFor(t, func() bool {
				stats := pool.Stats()
				return stats.Connecting == 0 && stats.Idle == 0
			})
			if calls := handlerCalls.Load(); calls != 0 {
				t.Fatalf("Handler calls after canceled authentication = %d, want 0", calls)
			}
			if calls := connection.closeCalls.Load(); calls != 1 {
				t.Fatalf("WorkConn close calls = %d, want 1", calls)
			}
			close(sessionDone)
			if err := pool.Wait(); err != nil {
				t.Fatalf("Wait() error = %v", err)
			}
		})
	}
}

func TestPoolDrainStopsRefillThenPreservesActiveUntilNaturalFinish(t *testing.T) {
	sessionDone := make(chan struct{})
	pool, err := newPool(testConfig(t, sessionDone), dependencies{
		dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("Drain 后不得补建 WorkConn")
		},
		authenticate: workauth.Authenticate,
		now:          time.Now,
	})
	if err != nil {
		t.Fatalf("newPool() error = %v", err)
	}
	parent, cancelParent := context.WithCancel(context.Background())
	if err := pool.Start(parent); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	connecting, connectingConn := installTestEntry(pool, 1, workConnecting)
	_, idleConn := installTestEntry(pool, 2, workIdle)
	_, openingConn := installTestEntry(pool, 3, workOpening)
	active, activeConn := installTestEntry(pool, 4, workActive)
	pool.mu.Lock()
	pool.demand = demandState{generation: 7, leaseID: testLeaseID, desired: 4, remaining: 4, deadline: time.Now().Add(time.Minute)}
	pool.mu.Unlock()

	if err := pool.BeginDrain(); err != nil {
		t.Fatalf("BeginDrain() error = %v", err)
	}
	if connectingConn.closeCalls.Load() != 1 {
		t.Fatalf("CONNECTING close calls after BeginDrain = %d, want 1", connectingConn.closeCalls.Load())
	}
	if result, err := pool.ApplyDemand(demand(testLeaseID, 8, 10, 10, 5_000)); err != nil || result != (DemandResult{}) {
		t.Fatalf("draining ApplyDemand() = (%+v, %v), want ignored", result, err)
	}

	drainContext, cancelDrain := context.WithTimeout(context.Background(), time.Second)
	defer cancelDrain()
	drainResult := make(chan error, 1)
	go func() { drainResult <- pool.CompleteDrain(drainContext) }()
	waitFor(t, func() bool {
		stats := pool.Stats()
		return stats.Connecting == 0 && stats.Idle == 0 && stats.Opening == 0 && stats.Active == 1 && stats.Total == 1
	})
	select {
	case err := <-drainResult:
		t.Fatalf("ACTIVE 自然结束前 CompleteDrain() 提前返回: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	// 模拟生产 Handler 完成 RAW：它先关闭 transport，再把 ACTIVE 计数归零。
	if err := closeTransport(active); err != nil {
		t.Fatalf("close ACTIVE transport: %v", err)
	}
	pool.finishHandled(active, nil)
	if err := <-drainResult; err != nil {
		t.Fatalf("CompleteDrain() error = %v", err)
	}
	for name, connection := range map[string]*countingConn{
		"connecting": connectingConn, "idle": idleConn, "opening": openingConn, "active": activeConn,
	} {
		if calls := connection.closeCalls.Load(); calls != 1 {
			t.Fatalf("%s close calls = %d, want exactly 1", name, calls)
		}
	}
	// CONNECTING 已在 BeginDrain 被取消，CompleteDrain 再次看到它也只能走 closeOnce。
	pool.finishConnecting(connecting, context.Canceled)
	cancelParent()
	if err := pool.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() error = %v, want context.Canceled", err)
	}
}

func TestPoolDrainDeadlineForceClosesActive(t *testing.T) {
	sessionDone := make(chan struct{})
	pool, err := newPool(testConfig(t, sessionDone), dependencies{
		dial:         func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("unexpected dial") },
		authenticate: workauth.Authenticate,
		now:          time.Now,
	})
	if err != nil {
		t.Fatalf("newPool() error = %v", err)
	}
	if err := pool.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	_, activeConn := installTestEntry(pool, 1, workActive)
	if err := pool.BeginDrain(); err != nil {
		t.Fatalf("BeginDrain() error = %v", err)
	}
	drainContext, cancelDrain := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancelDrain()
	if err := pool.CompleteDrain(drainContext); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CompleteDrain() error = %v, want deadline", err)
	}
	if calls := activeConn.closeCalls.Load(); calls != 1 {
		t.Fatalf("forced ACTIVE close calls = %d, want exactly 1", calls)
	}
	if err := pool.Wait(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait() error = %v, want deadline", err)
	}
	if stats := pool.Stats(); stats.Total != 0 || stats.Active != 0 {
		t.Fatalf("deadline 后 Stats = %+v", stats)
	}
}

func TestPoolIgnoresOldDemandAndRejectsMalformedDemand(t *testing.T) {
	sessionDone := make(chan struct{})
	config := testConfig(t, sessionDone)
	config.Handler = HandlerFunc(func(context.Context, net.Conn, *workauth.Ready) error { return nil })
	pool, err := newPool(config, dependencies{
		dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("zero-slot Demand must not dial")
		},
		authenticate: workauth.Authenticate,
		now:          time.Now,
	})
	if err != nil {
		t.Fatalf("newPool() error = %v", err)
	}
	if _, err := pool.ApplyDemand(demand(testLeaseID, 1, 1, 1, 1_000)); !errors.Is(err, ErrPoolNotRunning) {
		t.Fatalf("Start 前 ApplyDemand() error = %v", err)
	}
	if err := pool.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	invalid := []struct {
		name    string
		message *protocolv1.WorkDemand
	}{
		{name: "nil", message: nil},
		{name: "invalid lease id", message: demand("bad-lease", 1, 1, 1, 1_000)},
		{name: "zero generation", message: demand(testLeaseID, 0, 1, 1, 1_000)},
		{name: "lease without maximum", message: demand(testLeaseID, 1, 1, 0, 1_000)},
		{name: "lease without ttl", message: demand(testLeaseID, 1, 1, 1, 0)},
		{name: "lease with zero desired", message: demand(testLeaseID, 1, 0, 1, 1_000)},
		{name: "empty lease with maximum", message: demand("", 1, 1, 1, 0)},
		{name: "empty lease with ttl", message: demand("", 1, 1, 0, 1_000)},
	}
	unknown := demand("", 1, 0, 0, 0)
	unknown.ProtoReflect().SetUnknown([]byte{0x30, 0x01})
	invalid = append(invalid, struct {
		name    string
		message *protocolv1.WorkDemand
	}{name: "unknown fields", message: unknown})
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if _, err := pool.ApplyDemand(test.message); !errors.Is(err, ErrInvalidDemand) {
				t.Fatalf("ApplyDemand() error = %v, want ErrInvalidDemand", err)
			}
		})
	}

	result, err := pool.ApplyDemand(demand("", 2, 0, 0, 0))
	if err != nil || !result.Accepted {
		t.Fatalf("generation 2 result = %+v, error=%v", result, err)
	}
	for _, generation := range []uint64{1, 2} {
		result, err = pool.ApplyDemand(demand(testLeaseID, generation, 100, 100, 1_000))
		if err != nil || result.Accepted || result.Started != 0 {
			t.Fatalf("stale generation %d result = %+v, error=%v", generation, result, err)
		}
	}
	if stats := pool.Stats(); stats.DemandGeneration != 2 || stats.Total != 0 {
		t.Fatalf("stale Demand 改写 Pool: %+v", stats)
	}

	close(sessionDone)
	if err := pool.Wait(); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	sessionDone := make(chan struct{})
	valid := testConfig(t, sessionDone)
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "missing token", mutate: func(config *Config) { config.ConnectionToken = "" }},
		{name: "oversize token", mutate: func(config *Config) {
			config.ConnectionToken = string(bytes.Repeat([]byte{'x'}, maxConnectionTokenBytes+1))
		}},
		{name: "invalid tunnel", mutate: func(config *Config) { config.Session.TunnelID = "tun_bad" }},
		{name: "invalid connector", mutate: func(config *Config) { config.Session.ConnectorID = "con_bad" }},
		{name: "invalid session", mutate: func(config *Config) { config.Session.SessionID = "sess_bad" }},
		{name: "missing session done", mutate: func(config *Config) { config.SessionDone = nil }},
		{name: "missing handler", mutate: func(config *Config) { config.Handler = nil }},
		{name: "typed nil handler", mutate: func(config *Config) { config.Handler = HandlerFunc(nil) }},
		{name: "missing write timeout", mutate: func(config *Config) { config.AuthWriteTimeout = 0 }},
		{name: "missing read timeout", mutate: func(config *Config) { config.AuthReadTimeout = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			if _, err := New(config); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("New() error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func serveWorkAuthentication(connection net.Conn, session controlauth.Session, leaseID string) error {
	hello := &protocolv1.WorkHello{}
	if err := frame.ReadWork(connection, hello); err != nil {
		return err
	}
	if hello.GetTunnelId() != session.TunnelID || hello.GetConnectorId() != session.ConnectorID ||
		hello.GetSessionId() != session.SessionID || hello.GetBudgetLeaseId() != leaseID {
		return fmt.Errorf("WorkHello identity mismatch: %+v", hello)
	}
	wantMAC, err := deterministic.ComputeWorkHelloMAC(session.SessionSecret[:], hello)
	if err != nil {
		return err
	}
	if !bytes.Equal(hello.GetMac(), wantMAC) {
		return errors.New("WorkHello HMAC mismatch")
	}
	if err := frame.WriteWork(connection, &protocolv1.WorkReady{
		WorkId:    hello.GetWorkId(),
		Status:    protocolv1.WorkReadyStatus_WORK_READY_STATUS_READY,
		ErrorCode: protocolv1.ErrorCode_ERROR_CODE_OK,
	}); err != nil {
		return err
	}
	var one [1]byte
	_, err = connection.Read(one[:])
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func testConfig(t *testing.T, sessionDone <-chan struct{}) Config {
	t.Helper()
	return Config{
		ConnectionToken: testToken(t),
		Session: controlauth.Session{
			TunnelID:    testTunnelID,
			ConnectorID: testConnectorID,
			SessionID:   testSessionID,
			SessionSecret: [32]byte{
				0x41, 0x41, 0x41, 0x41, 0x41, 0x41, 0x41, 0x41,
				0x41, 0x41, 0x41, 0x41, 0x41, 0x41, 0x41, 0x41,
				0x41, 0x41, 0x41, 0x41, 0x41, 0x41, 0x41, 0x41,
				0x41, 0x41, 0x41, 0x41, 0x41, 0x41, 0x41, 0x41,
			},
			ProtocolVersion: 1,
		},
		SessionDone:      sessionDone,
		Handler:          HandlerFunc(func(context.Context, net.Conn, *workauth.Ready) error { return nil }),
		AuthWriteTimeout: time.Second,
		AuthReadTimeout:  time.Second,
	}
}

func testToken(t *testing.T) string {
	t.Helper()
	text, err := connectiontoken.Encode(&protocolv1.ConnectionToken{
		FormatVersion: connectiontoken.FormatVersionV1,
		Endpoint:      &protocolv1.GatewayEndpoint{Host: "gateway.example.test", Port: 7844},
		TlsTrust: &protocolv1.TlsTrustDescriptor{Mode: &protocolv1.TlsTrustDescriptor_PublicCa{
			PublicCa: &protocolv1.PublicCATrust{},
		}},
		TunnelId:             testTunnelID,
		TokenId:              testTokenID,
		TokenVersion:         1,
		AuthenticationSecret: bytes.Repeat([]byte{0x31}, 32),
	})
	if err != nil {
		t.Fatalf("token.Encode() error = %v", err)
	}
	return text
}

func demand(leaseID string, generation uint64, desired, maximum, ttlMS uint32) *protocolv1.WorkDemand {
	return &protocolv1.WorkDemand{
		BudgetLeaseId:     leaseID,
		DesiredNonActive:  desired,
		MaxNewConnections: maximum,
		LeaseTtlMs:        ttlMS,
		DemandGeneration:  generation,
	}
}

func authenticatedReady(config workauth.Config) (*workauth.Ready, error) {
	workID, err := identity.NewWorkID()
	if err != nil {
		return nil, err
	}
	workState, err := state.NewWork(state.EndpointAgent)
	if err != nil {
		return nil, err
	}
	if err := workState.AcceptOutbound(&protocolv1.WorkHello{
		TunnelId:      config.Session.TunnelID,
		ConnectorId:   config.Session.ConnectorID,
		SessionId:     config.Session.SessionID,
		WorkId:        workID,
		BudgetLeaseId: config.BudgetLeaseID,
	}); err != nil {
		return nil, err
	}
	if err := workState.AcceptInbound(&protocolv1.WorkReady{
		WorkId:    workID,
		Status:    protocolv1.WorkReadyStatus_WORK_READY_STATUS_READY,
		ErrorCode: protocolv1.ErrorCode_ERROR_CODE_OK,
	}); err != nil {
		return nil, err
	}
	return &workauth.Ready{WorkID: workID, State: workState}, nil
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("等待并发状态收敛超时")
		}
		time.Sleep(time.Millisecond)
	}
}

func updateMaximum(maximum *atomic.Int32, candidate int32) {
	for {
		current := maximum.Load()
		if candidate <= current || maximum.CompareAndSwap(current, candidate) {
			return
		}
	}
}

type countingConn struct {
	closeCalls atomic.Int32
}

type blockingObservedHandler struct {
	activeReached chan<- struct{}
	release       <-chan struct{}
}

func (handler *blockingObservedHandler) Handle(ctx context.Context, connection net.Conn, ready *workauth.Ready) error {
	return handler.HandleObserved(ctx, connection, ready, func(_ state.WorkPhase, commit func() error) error {
		return commit()
	})
}

func (handler *blockingObservedHandler) HandleObserved(
	ctx context.Context,
	_ net.Conn,
	_ *workauth.Ready,
	transition func(state.WorkPhase, func() error) error,
) error {
	if err := transition(state.WorkOpening, func() error { return nil }); err != nil {
		return err
	}
	if err := transition(state.WorkActive, func() error { return nil }); err != nil {
		return err
	}
	close(handler.activeReached)
	select {
	case <-handler.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func installTestEntry(pool *Pool, id uint64, phase workPhase) (*workEntry, *countingConn) {
	connection := &countingConn{}
	_, cancel := context.WithCancel(context.Background())
	handlerContext, handlerCancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	close(done)
	entry := &workEntry{
		id:             id,
		generation:     1,
		phase:          phase,
		cancel:         cancel,
		handlerContext: handlerContext,
		handlerCancel:  handlerCancel,
		observable:     true,
		connection:     connection,
		done:           done,
	}
	pool.mu.Lock()
	pool.entries[id] = entry
	switch phase {
	case workConnecting:
		pool.connecting++
	case workIdle:
		pool.idle++
	case workOpening:
		pool.opening++
	case workActive:
		pool.active++
	}
	pool.mu.Unlock()
	return entry, connection
}

func (*countingConn) Read([]byte) (int, error)         { return 0, errors.New("unexpected Read") }
func (*countingConn) Write(value []byte) (int, error)  { return len(value), nil }
func (connection *countingConn) Close() error          { connection.closeCalls.Add(1); return nil }
func (*countingConn) LocalAddr() net.Addr              { return testAddr("local") }
func (*countingConn) RemoteAddr() net.Addr             { return testAddr("remote") }
func (*countingConn) SetDeadline(time.Time) error      { return nil }
func (*countingConn) SetReadDeadline(time.Time) error  { return nil }
func (*countingConn) SetWriteDeadline(time.Time) error { return nil }

type testAddr string

func (address testAddr) Network() string { return "test" }
func (address testAddr) String() string  { return string(address) }
