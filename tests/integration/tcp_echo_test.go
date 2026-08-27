package integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/connector"
	"github.com/lifei6671/xtunnel/internal/application"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/repository"
	repositorysqlite "github.com/lifei6671/xtunnel/internal/repository/sqlite"
	"github.com/lifei6671/xtunnel/internal/server/controlauth"
	"github.com/lifei6671/xtunnel/internal/server/gateway"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
	serveropen "github.com/lifei6671/xtunnel/internal/server/open"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
	"github.com/lifei6671/xtunnel/internal/server/sessionruntime"
	serversnapshot "github.com/lifei6671/xtunnel/internal/server/snapshot"
	"github.com/lifei6671/xtunnel/internal/server/workauth"
	"github.com/lifei6671/xtunnel/internal/tunnel"
)

const (
	testTunnelID  = "tun_01J00000000000000000000000"
	testServiceID = "svc_01J00000000000000000000000"
)

// TestTCPEchoEndToEnd 穿过真实 pinned TLS Gateway、Control/Work AUTH、Snapshot
// Origin Resolver、WorkPool、Tunnel Connector 选择、OPEN 和 RAW Half-Close：
// Public TCP -> Server -> Agent -> Echo Origin。
func TestTCPEchoEndToEnd(t *testing.T) {
	baselineGoroutines := runtime.NumGoroutine()
	baselineFDs := openFDCount()
	baselineFDTargets := openFDTargets()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	firstOrigin, firstOriginDone := startEchoOrigin(t, ctx)
	secondOrigin, secondOriginDone := startEchoOrigin(t, ctx)
	store := openStoreWithTunnelAndService(t, ctx, firstOrigin)
	gatewayIdentity, err := gateway.LoadOrCreatePinnedIdentity(t.TempDir(), "gateway.integration.test", true, time.Now())
	if err != nil {
		t.Fatalf("create Gateway identity: %v", err)
	}
	protector, err := application.NewAES256GCMTokenProtector(bytes.Repeat([]byte{0x5a}, 32))
	if err != nil {
		t.Fatalf("create Token protector: %v", err)
	}
	tokenService := application.NewConnectionTokenService(store, protector)

	limitManager, err := serverlimits.New(serverlimits.Options{
		MaxConnectors: 8, MaxConnectorsPerTunnel: 4,
		MaxWorkConnections: 32, MaxIdleWorkConnections: 24, MaxConnectingWorkConnections: 16,
		MaxPendingOpens: 16, MaxActiveConnections: 16, MaxConnectionsPerTunnel: 16,
		MaxConnectionsPerService: 16, MaxConnectionsPerSourceIP: 8,
	})
	if err != nil {
		t.Fatalf("create Limit manager: %v", err)
	}
	registry := serverruntime.NewRegistryWithLimits(limitManager)
	snapshotBuilder, err := serversnapshot.New(serversnapshot.Config{
		ProtocolVersion:      1,
		MaxServices:          serversnapshot.MaxServicesPerTunnel,
		MaxSnapshotBytes:     serversnapshot.MaxTunnelSnapshotSize,
		MaxControlFrameBytes: int(frame.MaxControlFrameSize),
	})
	if err != nil {
		t.Fatalf("create Snapshot builder: %v", err)
	}
	snapshotSource, err := serversnapshot.NewSource(store, snapshotBuilder)
	if err != nil {
		t.Fatalf("create Snapshot source: %v", err)
	}
	sessions, err := sessionruntime.New(registry, sessionruntime.Options{
		HighPriorityCapacity: 32, NormalCapacity: 128, InboundCapacity: 128,
		WriteTimeout: 2 * time.Second, MaxReplayEntries: 256,
		MaxWorkTotal: 32, MaxWorkConnecting: 16,
		LimitManager: limitManager, SnapshotProvider: snapshotSource,
	})
	if err != nil {
		t.Fatalf("create Session manager: %v", err)
	}
	if err := sessions.Start(context.Background()); err != nil {
		t.Fatalf("start Session manager: %v", err)
	}
	t.Cleanup(func() {
		shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelShutdown()
		_ = sessions.Shutdown(shutdownContext)
	})
	controlHandler, err := controlauth.New(tokenService, registry, controlauth.Options{
		ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second,
		HeartbeatInterval: 100 * time.Millisecond, RetryAfter: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("create Control authenticator: %v", err)
	}
	workHandler, err := workauth.NewHandler(sessions, workauth.HandlerOptions{
		ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("create Work authenticator: %v", err)
	}

	gatewayServer, err := gateway.NewServer(gateway.ServerOptions{
		Listen: "127.0.0.1:0", Identity: gatewayIdentity, MaxPendingTLSHandshakes: 32,
		Handle: gatewayHandler(controlHandler, workHandler, sessions),
	})
	if err != nil {
		t.Fatalf("create Gateway server: %v", err)
	}
	if err := gatewayServer.Start(ctx); err != nil {
		t.Fatalf("start Gateway server: %v", err)
	}
	t.Cleanup(func() { _ = gatewayServer.Close() })

	host, portText, err := net.SplitHostPort(gatewayServer.Addr().String())
	if err != nil {
		t.Fatalf("split Gateway address: %v", err)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		t.Fatalf("parse Gateway port: %v", err)
	}
	pin := gatewayIdentity.SPKIHash()
	issued, err := tokenService.Issue(ctx, application.IssueConnectionTokenInput{
		TunnelID: testTunnelID,
		Endpoint: &protocolv1.GatewayEndpoint{Host: host, Port: uint32(port)},
		TLSTrust: &protocolv1.TlsTrustDescriptor{Mode: &protocolv1.TlsTrustDescriptor_PinnedSpkiSha256{
			PinnedSpkiSha256: &protocolv1.PinnedSPKITrust{SpkiSha256: pin[:]},
		}},
	})
	if err != nil {
		t.Fatalf("issue Tunnel Token: %v", err)
	}

	agentConfig, err := connector.HostConfig(issued.Token, "v0.1.0-integration")
	if err != nil {
		t.Fatalf("create Agent host config: %v", err)
	}
	agentRuntime, err := connector.New(agentConfig)
	if err != nil {
		t.Fatalf("create Agent runtime: %v", err)
	}
	agentDone := make(chan error, 1)
	go func() { agentDone <- agentRuntime.Run(ctx) }()

	waitForIdleWork(t, ctx, registry, sessions, agentConfig.Connector.ID())
	serverOpen, err := serveropen.NewHandler(serveropen.Options{
		WriteTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("create Server OPEN handler: %v", err)
	}
	tunnelProxy, err := tunnel.NewProxy(tunnel.Options{
		Registry: registry, Sessions: sessions, OpenHandler: serverOpen, AcquireTimeout: 2 * time.Second,
		LimitManager: limitManager,
	})
	if err != nil {
		t.Fatalf("create Tunnel proxy: %v", err)
	}

	firstPayload := []byte("xtunnel-m3-origin-one\x00with-binary\xff")
	if err := tunnelEchoRoundTrip(ctx, tunnelProxy, firstPayload); err != nil {
		t.Fatalf("first Tunnel Echo: %v", err)
	}
	waitForEchoOrigin(t, firstOriginDone, "first")

	updatedOrigin := integrationOriginInput(t, secondOrigin)
	serviceManagement := application.NewServiceManagementService(store, snapshotBuilder, sessions)
	updated, err := serviceManagement.Update(ctx, application.UpdateServiceInput{
		TunnelID: testTunnelID, ServiceID: testServiceID,
		ExpectedTunnelVersion: 1, ExpectedServiceVersion: 1, Origin: &updatedOrigin,
	})
	if err != nil {
		t.Fatalf("update Origin through Application Service: %v", err)
	}
	if updated.TunnelRevision != 2 || updated.Service.RequiredRevision != 2 || updated.Service.Version != 2 {
		t.Fatalf("updated Origin revision/version = %+v", updated)
	}

	secondPayload := []byte("xtunnel-m3-origin-two\x00without-agent-restart\xfe")
	waitForTunnelEcho(t, ctx, tunnelProxy, secondPayload)
	waitForEchoOrigin(t, secondOriginDone, "second")
	if snapshot := limitManager.Snapshot(); snapshot.PendingOpens != 0 || snapshot.ActiveTotal != 0 {
		t.Fatalf("Limit snapshot after Origin switch = %#v, want no PendingOpen or Active leak", snapshot)
	}

	cancel()
	select {
	case err := <-agentDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Agent runtime shutdown error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Agent runtime did not stop after cancellation")
	}
	if err := gatewayServer.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("close Gateway server: %v", err)
	}
	_ = store.Close()
	waitForResourceBaseline(t, baselineGoroutines, baselineFDs, baselineFDTargets)
}

func tunnelEchoRoundTrip(ctx context.Context, proxy *tunnel.Proxy, payload []byte) error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen public TCP: %w", err)
	}
	defer listener.Close()

	serveDone := make(chan error, 1)
	go func() {
		peer, acceptErr := listener.Accept()
		if acceptErr != nil {
			serveDone <- acceptErr
			return
		}
		serveDone <- proxy.Serve(ctx, testTunnelID, testServiceID, protocolv1.IngressType_INGRESS_TYPE_TCP, peer)
	}()

	clientConnection, err := net.DialTimeout("tcp", listener.Addr().String(), 2*time.Second)
	if err != nil {
		return fmt.Errorf("dial public TCP listener: %w", err)
	}
	client := clientConnection.(*net.TCPConn)
	if _, err := client.Write(payload); err != nil {
		_ = client.Close()
		return fmt.Errorf("write public payload: %w", err)
	}
	if err := client.CloseWrite(); err != nil {
		_ = client.Close()
		return fmt.Errorf("half-close public client: %w", err)
	}
	echoed, readErr := io.ReadAll(client)
	closeErr := client.Close()
	select {
	case serveErr := <-serveDone:
		if serveErr != nil {
			return fmt.Errorf("Tunnel proxy: %w", serveErr)
		}
	case <-time.After(3 * time.Second):
		return errors.New("Tunnel proxy did not finish after TCP half-close")
	}
	if readErr != nil {
		return fmt.Errorf("read echoed payload: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close public client: %w", closeErr)
	}
	if !bytes.Equal(echoed, payload) {
		return fmt.Errorf("echoed payload = %q, want byte-identical %q", echoed, payload)
	}
	return nil
}

func waitForTunnelEcho(t *testing.T, ctx context.Context, proxy *tunnel.Proxy, payload []byte) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := tunnelEchoRoundTrip(ctx, proxy, payload); err == nil {
			return
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context ended while waiting for updated Origin: %v", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatalf("updated Origin did not become active without Agent restart: %v", lastErr)
}

func waitForEchoOrigin(t *testing.T, done <-chan error, name string) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, context.Canceled) {
			t.Fatalf("%s Echo origin error = %v", name, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("%s Echo origin did not stop after half-close", name)
	}
}

func integrationOriginInput(t *testing.T, address net.Addr) application.ServiceOriginInput {
	t.Helper()
	host, portText, err := net.SplitHostPort(address.String())
	if err != nil {
		t.Fatalf("split integration Origin address: %v", err)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		t.Fatalf("parse integration Origin port: %v", err)
	}
	verify := true
	timeout := uint32(2_000)
	return application.ServiceOriginInput{
		Scheme: repository.OriginSchemeTCP, Host: host, Port: uint32(port),
		TLSVerify: &verify, ConnectTimeoutMS: &timeout,
	}
}

func waitForResourceBaseline(t *testing.T, baselineGoroutines, baselineFDs int, baselineFDTargets map[string]string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		goroutines := runtime.NumGoroutine()
		fds := openFDCount()
		// Go 测试框架、SQLite 和 Race Runtime 可能保留少量共享后台任务，因此
		// goroutine 只允许固定 4 条抖动；Linux FD 则必须回到测试前基线。
		if goroutines <= baselineGoroutines+4 && (baselineFDs < 0 || fds <= baselineFDs) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("resources did not return to baseline: goroutines=%d baseline=%d fds=%d baseline_fds=%d baseline_targets=%v current_targets=%v",
		runtime.NumGoroutine(), baselineGoroutines, openFDCount(), baselineFDs, baselineFDTargets, openFDTargets())
}

func openFDTargets() map[string]string {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return nil
	}
	targets := make(map[string]string, len(entries))
	for _, entry := range entries {
		target, err := os.Readlink("/proc/self/fd/" + entry.Name())
		if err == nil {
			targets[entry.Name()] = target
		}
	}
	return targets
}

func openFDCount() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		// 非 Linux 平台没有统一的进程 FD 目录；返回 -1 表示该维度不可观测，
		// Linux CI/生产 Gate 会执行严格计数。
		return -1
	}
	return len(entries)
}

func gatewayHandler(
	controlHandler *controlauth.Handler,
	workHandler *workauth.Handler,
	sessions *sessionruntime.Manager,
) func(context.Context, *tls.Conn, gateway.Protocol) {
	return func(ctx context.Context, connection *tls.Conn, protocol gateway.Protocol) {
		switch protocol {
		case gateway.ControlProtocol:
			established, err := controlHandler.Handle(ctx, connection)
			if err == nil {
				_ = sessions.Serve(ctx, connection, &established)
			}
		case gateway.WorkProtocol:
			idle, err := workHandler.Handle(ctx, connection)
			if err != nil {
				return
			}
			work, err := sessions.RegisterIdle(connection, idle)
			if err != nil {
				idle.State.Close()
				return
			}
			<-work.Done()
			idle.State.Close()
		}
	}
}

func openStoreWithTunnelAndService(t *testing.T, ctx context.Context, origin net.Addr) *repositorysqlite.Store {
	t.Helper()
	host, portText, err := net.SplitHostPort(origin.String())
	if err != nil {
		t.Fatalf("split Echo origin address: %v", err)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		t.Fatalf("parse Echo origin port: %v", err)
	}
	store, err := repositorysqlite.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open SQLite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.WithTx(ctx, func(transaction repository.TxStore) error {
		if err := transaction.Tunnels().Create(ctx, repository.Tunnel{
			ID: testTunnelID, Name: "m3-integration", Version: 1, DesiredRevision: 1,
			CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			return err
		}
		return transaction.Services().Create(ctx, repository.Service{
			ID: testServiceID, TunnelID: testTunnelID, Name: "echo",
			RequiredRevision: 1, OriginScheme: repository.OriginSchemeTCP,
			OriginHost: host, OriginPort: uint32(port), TLSVerify: true,
			ConnectTimeoutMS: 2_000, Enabled: true, Version: 1, CreatedAt: 1, UpdatedAt: 1,
		})
	}); err != nil {
		t.Fatalf("create integration Tunnel: %v", err)
	}
	return store
}

func startEchoOrigin(t *testing.T, ctx context.Context) (net.Addr, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen Echo origin: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		var resultErr error
		// done 同时是资源释放完成信号。必须先关闭连接和 Listener，再通知测试主流程，
		// 否则 FD 基线断言可能与 defer 并发，既产生假失败，也掩盖真实的 Listener 泄漏。
		defer func() {
			if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
				resultErr = errors.Join(resultErr, closeErr)
			}
			done <- resultErr
		}()

		stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
		defer stop()
		connection, err := listener.Accept()
		if err != nil {
			resultErr = err
			return
		}
		defer func() {
			if closeErr := connection.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
				resultErr = errors.Join(resultErr, closeErr)
			}
		}()
		// TCPConn 同时实现 ReaderFrom/WriterTo，直接 io.Copy 会在 Linux 首次调用时
		// 启用 splice，并把一对 pipe 缓存在标准库的进程级 sync.Pool 中。Echo 夹具
		// 不需要验证零拷贝；隐藏这两个可选接口后走普通缓冲复制，资源基线便只统计
		// 当前 Tunnel 生命周期真正拥有且必须关闭的 FD。
		reader := struct{ io.Reader }{Reader: connection}
		writer := struct{ io.Writer }{Writer: connection}
		_, resultErr = io.Copy(writer, reader)
	}()
	return listener.Addr(), done
}

func waitForIdleWork(
	t *testing.T,
	ctx context.Context,
	registry *serverruntime.Registry,
	sessions *sessionruntime.Manager,
	connectorID string,
) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		session, exists := registry.Current(testTunnelID, connectorID)
		if exists {
			if pool, exists := sessions.Pool(session); exists && pool.Snapshot().Idle > 0 {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context ended while waiting for IDLE Work: %v", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatal("Agent did not establish an IDLE WorkConn before timeout")
}
