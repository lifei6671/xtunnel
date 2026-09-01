//go:build linux

package bootstrap

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/connector"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
)

const (
	m7ShutdownGracePeriod = 2 * time.Second
	m7ShutdownHardPeriod  = 250 * time.Millisecond
)

// TestM7GracefulShutdownChaos 只从真实公网 Listener 进入生产 Bootstrap、Gateway、
// Token-only Agent 和 Origin。各场景用 Channel/Socket 作为确定性阶段栅栏，验证关闭
// 期间已准入流量的自然排空、Deadline 强关和 Agent 两阶段 Drain；不使用随机 sleep
// 决定业务断言。
func TestM7GracefulShutdownChaos(t *testing.T) {
	t.Run("tcp half-close survives Server graceful drain", testM7TCPHalfCloseNaturalDrain)
	t.Run("HTTP streaming survives Server graceful drain", testM7HTTPStreamingNaturalDrain)
	t.Run("WebSocket remains usable during Server graceful drain", testM7WebSocketNaturalDrain)
	t.Run("hard deadline force closes all active transports", testM7HardDeadlineForceClose)
	t.Run("Agent initiated drain preserves active TCP", testM7AgentInitiatedDrain)
	t.Run("Agent hard deadline force closes active TCP", testM7AgentHardDeadlineForceClose)
}

type m7ShutdownFixture struct {
	cancelServer context.CancelFunc
	resources    *serverStorage
	runtime      *gatewayBootstrapCloser
	httpOrigin   *httptest.Server
	tcpOrigin    *productGateTCPOrigin
	publicTCP    string
	publicHTTP   string
	agent        *m7ShutdownAgent

	serverCloseOnce sync.Once
	serverCloseErr  error
	storageOnce     sync.Once
	storageErr      error
	originCloseOnce sync.Once
}

func newM7ShutdownFixture(t *testing.T, originHandler http.Handler) *m7ShutdownFixture {
	t.Helper()
	serverContext, cancelServer := context.WithCancel(context.Background())
	if originHandler == nil {
		originHandler = http.NotFoundHandler()
	}
	httpOrigin := httptest.NewServer(originHandler)
	t.Cleanup(httpOrigin.Close)
	tcpOrigin := startProductGateTCPOrigin(t)
	publicAddress, publicPort := reserveProductGateTCPPort(t)
	runtimeDir := newRuntimeDirectory(t)
	dataDir := t.TempDir()
	resources, err := openServerStorage(serverContext, dataDir, runtimeDir)
	if err != nil {
		cancelServer()
		t.Fatalf("open M7-03 Server storage: %v", err)
	}
	seedProductGateDesiredState(
		t, serverContext, resources, httpOrigin.Listener.Addr(), tcpOrigin.listener.Addr(), publicPort,
	)
	if err := resources.database.CreateFirstAdmin(serverContext, "admin", "m7 graceful shutdown chaos password"); err != nil {
		_ = resources.Close()
		cancelServer()
		t.Fatalf("create M7-03 Admin: %v", err)
	}

	config := gatewayLifecycleTestConfig(dataDir, "127.0.0.1:0")
	config.HTTPIngress.TrustedProxies = []string{"127.0.0.0/8"}
	config.TCPIngress.MinPort = int(publicPort)
	config.TCPIngress.MaxPort = int(publicPort)
	config.Limits.MaxHTTPBodyBytes = 1 << 20
	config.Limits.MaxHTTPHeaderBytes = 16 << 10
	config.Limits.MaxHTTPRequestsPerSourceIPPerSecond = 100
	config.Limits.MaxWorkConnections = 16
	config.Limits.MaxIdleWorkConnections = 16
	config.Limits.MaxConnectingWorkConnections = 16
	config.Limits.MaxPendingOpens = 16
	config.Limits.MaxOpenRatePerSourceIP = 100
	config.Limits.MaxOpenBurstPerSourceIP = 100
	config.Limits.MaxActiveConnections = 16
	config.Limits.MaxConnectionsPerTunnel = 16
	config.Limits.MaxConnectionsPerService = 16
	config.Limits.MaxConnectionsPerSourceIP = 16
	config.Limits.MaxPendingTLSHandshakes = 16
	config.Limits.MaxPendingAuth = 16

	closer, err := openGatewayAndBootstrapWith(
		serverContext, config, resources,
		slog.New(slog.NewJSONHandler(io.Discard, nil)), runtimeDir,
		func(context.Context, string, string, *sqlite.Store, func() error, func(error)) (io.Closer, error) {
			return nil, errors.New("M7-03 unexpectedly opened Bootstrap Socket")
		},
	)
	if err != nil {
		_ = resources.Close()
		cancelServer()
		t.Fatalf("start M7-03 Server runtime: %v", err)
	}
	runtime := closer.(*gatewayBootstrapCloser)
	issuedToken := issueProductGateToken(t, serverContext, resources, runtime.gateway.Addr())
	fixture := &m7ShutdownFixture{
		cancelServer: cancelServer,
		resources:    resources,
		runtime:      runtime,
		httpOrigin:   httpOrigin,
		tcpOrigin:    tcpOrigin,
		publicTCP:    publicAddress,
		publicHTTP:   runtime.httpIngress.Addr().String(),
	}
	fixture.agent = startM7ShutdownAgent(t, issuedToken, runtime, 8)
	t.Cleanup(func() { fixture.cleanup(t) })
	return fixture
}

func (fixture *m7ShutdownFixture) closeServer() error {
	fixture.serverCloseOnce.Do(func() { fixture.serverCloseErr = fixture.runtime.Close() })
	return fixture.serverCloseErr
}

func (fixture *m7ShutdownFixture) closeStorage() error {
	fixture.storageOnce.Do(func() { fixture.storageErr = fixture.resources.Close() })
	return fixture.storageErr
}

func (fixture *m7ShutdownFixture) cleanup(t *testing.T) {
	t.Helper()
	fixture.agent.stop(t)
	if err := fixture.closeServer(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("close M7-03 Server runtime: %v", err)
	}
	fixture.cancelServer()
	if err := fixture.closeStorage(); err != nil {
		t.Errorf("close M7-03 Server storage: %v", err)
	}
	fixture.originCloseOnce.Do(func() {
		fixture.tcpOrigin.close(t)
		fixture.httpOrigin.Close()
	})
}

func testM7TCPHalfCloseNaturalDrain(t *testing.T) {
	baseline := m7MustReadShutdownResources(t)
	fixture := newM7ShutdownFixture(t, nil)
	fixture.runtime.drainTimeout = m7ShutdownGracePeriod
	waitForProductGateIdleWork(t, fixture.runtime, 1)

	public := dialProductGateTCP(t, fixture.publicTCP, "127.0.0.1")
	origin := fixture.tcpOrigin.next(t, "M7-03 TCP Half-Close")
	var originCloseOnce sync.Once
	closeOrigin := func() {
		originCloseOnce.Do(func() {
			if err := origin.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				t.Errorf("close M7-03 TCP Origin: %v", err)
			}
		})
	}
	t.Cleanup(closeOrigin)
	originReceived := make(chan []byte, 1)
	releaseOrigin := make(chan struct{})
	var releaseOriginOnce sync.Once
	release := func() { releaseOriginOnce.Do(func() { close(releaseOrigin) }) }
	t.Cleanup(release)
	originDone := make(chan error, 1)
	go func() {
		request, readErr := io.ReadAll(origin)
		originReceived <- request
		if readErr == nil {
			<-releaseOrigin
			_, writeErr := origin.Write([]byte("origin-tail"))
			readErr = errors.Join(writeErr, origin.(*net.TCPConn).CloseWrite())
		}
		// CloseWrite 已为反向流发布 EOF；延后整个 Socket 的 Close，避免尾部
		// 字节尚未被 Public 确认时产生 RST，干扰自然排空断言。
		originDone <- readErr
	}()

	if _, err := public.Write([]byte("public-head")); err != nil {
		t.Fatalf("write M7-03 TCP request: %v", err)
	}
	shutdownDone := m7StartServerClose(fixture)
	// 先观察生产 StopAccepting 发布的空 Actual，证明 admission fence 已建立；
	// 此后 Listener 关闭探针即使在内核队列中完成握手，也不会进入 Handler/OPEN。
	m7WaitForTCPAdmissionFence(t, fixture)
	m7WaitForListenerClosed(t, fixture.publicTCP)
	fixture.tcpOrigin.assertNoNext(t, "Server shutdown listener probe")
	m7AssertShutdownPending(t, shutdownDone)

	if err := public.CloseWrite(); err != nil {
		t.Fatalf("half-close M7-03 public TCP: %v", err)
	}
	select {
	case received := <-originReceived:
		if !bytes.Equal(received, []byte("public-head")) {
			t.Fatalf("M7-03 TCP Origin received %q", received)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("M7-03 TCP Origin did not observe public Half-Close")
	}

	release()
	tail, err := io.ReadAll(public)
	if err != nil || !bytes.Equal(tail, []byte("origin-tail")) {
		t.Fatalf("M7-03 TCP reverse tail = %q, error %v", tail, err)
	}
	if err := public.Close(); err != nil {
		t.Fatalf("close M7-03 public TCP: %v", err)
	}
	m7RequireResult(t, originDone, "TCP Origin", false)
	m7RequireResult(t, shutdownDone, "Server graceful shutdown", false)
	closeOrigin()

	fixture.cleanup(t)
	m7AssertShutdownQuiescent(t, fixture, baseline)
	t.Log("M7-03 TCP: sent=11 received=11 reverse_sent=11 reverse_received=11 lost=0 duplicate=0")
}

func testM7HTTPStreamingNaturalDrain(t *testing.T) {
	baseline := m7MustReadShutdownResources(t)
	firstFlushed := make(chan struct{})
	releaseOrigin := make(chan struct{})
	var signalOnce sync.Once
	fixture := newM7ShutdownFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(writer, "first-")
		writer.(http.Flusher).Flush()
		signalOnce.Do(func() { close(firstFlushed) })
		select {
		case <-releaseOrigin:
			_, _ = io.WriteString(writer, "second")
		case <-request.Context().Done():
		}
	}))
	fixture.runtime.drainTimeout = m7ShutdownGracePeriod
	waitForProductGateIdleWork(t, fixture.runtime, 1)

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		"http://"+fixture.publicHTTP+"/gate/stream", nil)
	if err != nil {
		t.Fatalf("construct M7-03 streaming request: %v", err)
	}
	request.Host = productGatePublicHost
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("start M7-03 streaming request: %v", err)
	}
	select {
	case <-firstFlushed:
	case <-time.After(3 * time.Second):
		t.Fatal("M7-03 HTTP Origin did not flush first chunk")
	}

	shutdownDone := m7StartServerClose(fixture)
	m7WaitForListenerClosed(t, fixture.publicHTTP)
	m7AssertShutdownPending(t, shutdownDone)
	close(releaseOrigin)
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(body, []byte("first-second")) {
		t.Fatalf("M7-03 streaming response = %q, read %v close %v", body, readErr, closeErr)
	}
	m7RequireResult(t, shutdownDone, "Server graceful shutdown", false)

	fixture.cleanup(t)
	m7AssertShutdownQuiescent(t, fixture, baseline)
	t.Log("M7-03 HTTP streaming: sent=12 received=12 lost=0 duplicate=0")
}

func testM7WebSocketNaturalDrain(t *testing.T) {
	baseline := m7MustReadShutdownResources(t)
	webSocketReady := make(chan struct{})
	originDone := make(chan error, 1)
	fixture := newM7ShutdownFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		originDone <- m7ServeShutdownWebSocket(writer, request, webSocketReady)
	}))
	fixture.runtime.drainTimeout = m7ShutdownGracePeriod
	waitForProductGateIdleWork(t, fixture.runtime, 1)

	connection, reader := m7DialShutdownWebSocket(t, fixture.publicHTTP)
	defer connection.Close()
	select {
	case <-webSocketReady:
	case <-time.After(3 * time.Second):
		t.Fatal("M7-03 WebSocket Origin did not complete handshake")
	}

	shutdownDone := m7StartServerClose(fixture)
	m7WaitForListenerClosed(t, fixture.publicHTTP)
	m7AssertShutdownPending(t, shutdownDone)
	m7WriteMaskedFrame(t, connection, []byte("hello"))
	echoed := make([]byte, 7)
	if _, err := io.ReadFull(reader, echoed); err != nil {
		t.Fatalf("read M7-03 WebSocket echo during drain: %v", err)
	}
	if !bytes.Equal(echoed, []byte{0x81, 0x05, 'h', 'e', 'l', 'l', 'o'}) {
		t.Fatalf("M7-03 WebSocket echo = %x", echoed)
	}
	m7RequireResult(t, originDone, "WebSocket Origin", false)
	if err := connection.Close(); err != nil {
		t.Fatalf("close M7-03 WebSocket client: %v", err)
	}
	m7RequireResult(t, shutdownDone, "Server graceful shutdown", false)

	fixture.cleanup(t)
	m7AssertShutdownQuiescent(t, fixture, baseline)
	t.Log("M7-03 WebSocket: sent=5 received=5 lost=0 duplicate=0")
}

func testM7HardDeadlineForceClose(t *testing.T) {
	baseline := m7MustReadShutdownResources(t)
	httpReady := make(chan struct{})
	httpCanceled := make(chan error, 1)
	webSocketReady := make(chan struct{})
	webSocketDone := make(chan error, 1)
	fixture := newM7ShutdownFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.EqualFold(request.Header.Get("Upgrade"), "websocket") {
			webSocketDone <- m7ServeShutdownWebSocket(writer, request, webSocketReady)
			return
		}
		_, _ = io.WriteString(writer, "partial")
		writer.(http.Flusher).Flush()
		close(httpReady)
		<-request.Context().Done()
		httpCanceled <- request.Context().Err()
	}))
	fixture.runtime.drainTimeout = m7ShutdownHardPeriod
	waitForProductGateIdleWork(t, fixture.runtime, 3)

	publicTCP := dialProductGateTCP(t, fixture.publicTCP, "127.0.0.1")
	originTCP := fixture.tcpOrigin.next(t, "M7-03 hard deadline")
	if _, err := publicTCP.Write([]byte("x")); err != nil {
		t.Fatalf("prime M7-03 hard-deadline TCP: %v", err)
	}
	one := make([]byte, 1)
	if _, err := io.ReadFull(originTCP, one); err != nil {
		t.Fatalf("read M7-03 hard-deadline TCP prime: %v", err)
	}
	originTCPDone := make(chan error, 1)
	go func() {
		_, readErr := originTCP.Read(one)
		originTCPDone <- errors.Join(readErr, originTCP.Close())
	}()

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		"http://"+fixture.publicHTTP+"/gate/slow", nil)
	if err != nil {
		t.Fatalf("construct M7-03 hard-deadline HTTP request: %v", err)
	}
	request.Host = productGatePublicHost
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("start M7-03 hard-deadline HTTP request: %v", err)
	}
	select {
	case <-httpReady:
	case <-time.After(3 * time.Second):
		t.Fatal("M7-03 hard-deadline HTTP Origin did not start")
	}

	webSocket, webSocketReader := m7DialShutdownWebSocket(t, fixture.publicHTTP)
	select {
	case <-webSocketReady:
	case <-time.After(3 * time.Second):
		t.Fatal("M7-03 hard-deadline WebSocket Origin did not start")
	}

	started := time.Now()
	shutdownDone := m7StartServerClose(fixture)
	shutdownErr := m7ReceiveResult(t, shutdownDone, "Server hard-deadline shutdown")
	elapsed := time.Since(started)
	if !errors.Is(shutdownErr, context.DeadlineExceeded) {
		t.Fatalf("M7-03 hard-deadline shutdown error = %v, want DeadlineExceeded", shutdownErr)
	}
	if elapsed < m7ShutdownHardPeriod-50*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("M7-03 hard-deadline duration = %s", elapsed)
	}

	m7AssertConnectionClosed(t, publicTCP, "public TCP")
	m7RequireTransportUnblocked(t, originTCPDone, "TCP Origin force close")
	m7AssertHTTPBodyClosed(t, response.Body)
	m7RequireResult(t, httpCanceled, "HTTP Origin cancellation", true)
	if err := webSocket.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set M7-03 force-closed WebSocket deadline: %v", err)
	}
	m7AssertReaderClosed(t, webSocketReader, "public WebSocket")
	m7RequireResult(t, webSocketDone, "WebSocket Origin force close", true)
	_ = webSocket.Close()

	fixture.cleanup(t)
	m7AssertShutdownQuiescent(t, fixture, baseline)
	t.Logf("M7-03 hard deadline: deadline=%s force_close=%s active_transports=3", m7ShutdownHardPeriod, elapsed)
}

func testM7AgentInitiatedDrain(t *testing.T) {
	baseline := m7MustReadShutdownResources(t)
	fixture := newM7ShutdownFixture(t, nil)
	fixture.runtime.drainTimeout = m7ShutdownGracePeriod
	waitForProductGateIdleWork(t, fixture.runtime, 1)

	public := dialProductGateTCP(t, fixture.publicTCP, "127.0.0.1")
	origin := fixture.tcpOrigin.next(t, "M7-03 Agent drain")
	originDone := startProductGateOriginEcho(origin)
	assertProductGateRoundTrip(t, public, []byte("before-drain"), "Agent drain before request")

	fixture.agent.beginDrain()
	m7WaitForAgentDraining(t, fixture)
	m7AssertShutdownSignalPending(t, fixture.agent.done)
	assertProductGateRoundTrip(t, public, []byte("during-drain"), "Agent drain active stream")
	m7AssertNewOpenRejected(t, fixture)
	finishProductGateTCP(t, public, originDone, "Agent drain")
	if err := public.Close(); err != nil {
		t.Fatalf("close M7-03 Agent drain public TCP: %v", err)
	}
	agentErr := fixture.agent.wait(5 * time.Second)
	if !errors.Is(agentErr, context.Canceled) || errors.Is(agentErr, context.DeadlineExceeded) {
		t.Fatalf("M7-03 Agent graceful-drain error = %v, want canceled without DeadlineExceeded", agentErr)
	}

	if err := fixture.closeServer(); err != nil {
		t.Fatalf("close M7-03 Server after Agent drain: %v", err)
	}
	fixture.cleanup(t)
	m7AssertShutdownQuiescent(t, fixture, baseline)
	t.Log("M7-03 Agent drain: active bytes preserved, new OPEN rejected, owners_done=true")
}

func testM7AgentHardDeadlineForceClose(t *testing.T) {
	baseline := m7MustReadShutdownResources(t)
	fixture := newM7ShutdownFixture(t, nil)
	fixture.runtime.drainTimeout = m7ShutdownGracePeriod
	waitForProductGateIdleWork(t, fixture.runtime, 1)

	public := dialProductGateTCP(t, fixture.publicTCP, "127.0.0.1")
	if err := public.SetDeadline(time.Time{}); err != nil {
		t.Fatalf("clear M7-03 Agent hard-deadline public TCP deadline: %v", err)
	}
	origin := fixture.tcpOrigin.next(t, "M7-03 Agent hard deadline")
	originDone := startProductGateOriginEcho(origin)
	assertProductGateRoundTrip(t, public, []byte("before-drain"), "Agent hard deadline before request")
	trafficDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		buffer := make([]byte, 1)
		for range ticker.C {
			if _, err := public.Write([]byte("x")); err != nil {
				trafficDone <- err
				return
			}
			if _, err := io.ReadFull(public, buffer); err != nil {
				trafficDone <- err
				return
			}
		}
	}()

	started := time.Now()
	fixture.agent.beginDrain()
	m7WaitForAgentDraining(t, fixture)
	m7AssertShutdownSignalPending(t, fixture.agent.done)
	agentErr := fixture.agent.wait(35 * time.Second)
	elapsed := time.Since(started)
	if !errors.Is(agentErr, context.Canceled) || !errors.Is(agentErr, context.DeadlineExceeded) {
		t.Fatalf("M7-03 Agent hard-deadline error = %v, want canceled + DeadlineExceeded", agentErr)
	}
	if elapsed < 29*time.Second || elapsed > 35*time.Second {
		t.Fatalf("M7-03 Agent hard-deadline duration = %s", elapsed)
	}
	m7RequireResult(t, trafficDone, "Agent hard-deadline active traffic force close", true)
	m7AssertConnectionClosed(t, public, "Agent hard-deadline public TCP")
	m7RequireTransportUnblocked(t, originDone, "Agent hard-deadline Origin force close")
	_ = public.Close()

	if err := fixture.closeServer(); err != nil {
		t.Fatalf("close M7-03 Server after Agent hard deadline: %v", err)
	}
	fixture.cleanup(t)
	m7AssertShutdownQuiescent(t, fixture, baseline)
	t.Logf("M7-03 Agent hard deadline: deadline=30s force_close=%s", elapsed)
}

type m7ShutdownAgent struct {
	cancel     context.CancelFunc
	cancelOnce sync.Once
	done       chan struct{}
	mu         sync.Mutex
	runErr     error
}

func startM7ShutdownAgent(
	t *testing.T,
	token string,
	runtime *gatewayBootstrapCloser,
	wantIdle uint32,
) *m7ShutdownAgent {
	t.Helper()
	agentConfig, err := connector.HostConfig(token, "v0.1.0-m7-03-chaos")
	if err != nil {
		t.Fatalf("build M7-03 Agent config: %v", err)
	}
	agentConfig.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	agentRuntime, err := connector.New(agentConfig)
	if err != nil {
		t.Fatalf("construct M7-03 Agent runtime: %v", err)
	}
	agentContext, cancelAgent := context.WithCancel(context.Background())
	agent := &m7ShutdownAgent{cancel: cancelAgent, done: make(chan struct{})}
	go func() {
		runErr := agentRuntime.Run(agentContext)
		agent.mu.Lock()
		agent.runErr = runErr
		agent.mu.Unlock()
		close(agent.done)
	}()

	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		for _, snapshot := range runtime.sessions.RuntimeStatusSnapshots() {
			httpService, hasHTTP := snapshot.Config.Services[productGateHTTPServiceID]
			tcpService, hasTCP := snapshot.Config.Services[productGateTCPServiceID]
			if snapshot.TunnelID == productGateTunnelID && snapshot.CurrentControlSession &&
				snapshot.Config.ConfigReady && snapshot.Config.HasObserved &&
				snapshot.Config.ObservedRevision == 1 && hasHTTP && hasTCP &&
				httpService.Enabled && httpService.RequiredRevision == 1 &&
				tcpService.Enabled && tcpService.RequiredRevision == 1 && snapshot.WorkPool.Idle >= wantIdle {
				return agent
			}
		}
		select {
		case <-agent.done:
			t.Fatalf("M7-03 Agent exited before ready: %v", agent.err())
		case <-deadline.C:
			agent.stop(t)
			t.Fatal("M7-03 Agent did not publish a ready two-Service Snapshot and IDLE Work")
		case <-ticker.C:
		}
	}
}

func (agent *m7ShutdownAgent) beginDrain() {
	agent.cancelOnce.Do(agent.cancel)
}

func (agent *m7ShutdownAgent) wait(timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-agent.done:
		return agent.err()
	case <-timer.C:
		return fmt.Errorf("M7-03 Agent did not stop within %s", timeout)
	}
}

func (agent *m7ShutdownAgent) err() error {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return agent.runErr
}

func (agent *m7ShutdownAgent) stop(t *testing.T) {
	t.Helper()
	agent.beginDrain()
	err := agent.wait(35 * time.Second)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("stop M7-03 Agent runtime: %v", err)
	}
}

func m7StartServerClose(fixture *m7ShutdownFixture) <-chan error {
	done := make(chan error, 1)
	go func() { done <- fixture.closeServer() }()
	return done
}

func m7WaitForTCPAdmissionFence(t *testing.T, fixture *m7ShutdownFixture) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if len(fixture.runtime.tcpIngress.Actual()) == 0 {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("M7-03 TCP admission fence was not published for %s", fixture.publicTCP)
		case <-ticker.C:
		}
	}
}

func m7WaitForListenerClosed(t *testing.T, address string) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		connection, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if err != nil {
			return
		}
		_ = connection.Close()
		select {
		case <-deadline.C:
			t.Fatalf("M7-03 listener %s still accepted new connections", address)
		case <-ticker.C:
		}
	}
}

func m7AssertShutdownPending(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("M7-03 drain returned before active traffic completed: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
}

func m7AssertShutdownSignalPending(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
		t.Fatal("M7-03 drain returned before active traffic reached its deadline")
	case <-time.After(75 * time.Millisecond):
	}
}

func m7ReceiveResult(t *testing.T, done <-chan error, operation string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("M7-03 %s did not finish", operation)
	}
	return nil
}

func m7RequireResult(t *testing.T, done <-chan error, operation string, wantError bool) {
	t.Helper()
	err := m7ReceiveResult(t, done, operation)
	if wantError && err == nil {
		t.Fatalf("M7-03 %s returned nil, want transport close", operation)
	}
	if wantError && m7IsTimeoutError(err) {
		t.Fatalf("M7-03 %s timed out instead of observing transport close: %v", operation, err)
	}
	if !wantError && err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("M7-03 %s: %v", operation, err)
	}
}

func m7RequireTransportUnblocked(t *testing.T, done <-chan error, operation string) {
	t.Helper()
	err := m7ReceiveResult(t, done, operation)
	// TCP 对端被主动关闭时，Linux 可能把结果呈现为 EOF（io.Copy 返回 nil），
	// 也可能呈现为 RST。两者都证明阻塞 IO 已解除；只有超时才违反 Shutdown 契约。
	if m7IsTimeoutError(err) {
		t.Fatalf("M7-03 %s timed out instead of observing transport close: %v", operation, err)
	}
}

func m7WaitForAgentDraining(t *testing.T, fixture *m7ShutdownFixture) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		for _, snapshot := range fixture.runtime.sessions.RuntimeStatusSnapshots() {
			if snapshot.TunnelID == productGateTunnelID && snapshot.WorkPool.Draining {
				return
			}
		}
		select {
		case <-deadline.C:
			t.Fatal("M7-03 Agent did not publish draining WorkPool")
		case <-ticker.C:
		}
	}
}

func m7AssertNewOpenRejected(t *testing.T, fixture *m7ShutdownFixture) {
	t.Helper()
	connection := dialProductGateTCP(t, fixture.publicTCP, "127.0.0.2")
	defer connection.Close()
	if _, err := connection.Write([]byte("late-open")); err != nil {
		return
	}
	buffer := make([]byte, 1)
	read, err := connection.Read(buffer)
	if read != 0 || err == nil {
		t.Fatalf("M7-03 Agent drain late OPEN read=%d error=%v", read, err)
	}
	fixture.tcpOrigin.assertNoNext(t, "Agent drain late OPEN")
}

func m7ServeShutdownWebSocket(writer http.ResponseWriter, request *http.Request, ready chan<- struct{}) error {
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		return errors.New("M7-03 HTTP Origin does not support Hijack")
	}
	connection, buffered, err := hijacker.Hijack()
	if err != nil {
		return fmt.Errorf("hijack M7-03 WebSocket Origin: %w", err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	accept := productGateWebSocketAccept(request.Header.Get("Sec-WebSocket-Key"))
	if _, err := fmt.Fprintf(buffered,
		"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
		accept,
	); err != nil {
		return err
	}
	if err := buffered.Flush(); err != nil {
		return err
	}
	close(ready)
	frame := make([]byte, 11)
	if _, err := io.ReadFull(buffered, frame); err != nil {
		return err
	}
	payload := make([]byte, 5)
	for index := range payload {
		payload[index] = frame[6+index] ^ frame[2+index%4]
	}
	_, err = connection.Write(append([]byte{0x81, byte(len(payload))}, payload...))
	return err
}

func m7DialShutdownWebSocket(t *testing.T, address string) (net.Conn, *bufio.Reader) {
	t.Helper()
	connection, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatalf("dial M7-03 WebSocket listener: %v", err)
	}
	if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		_ = connection.Close()
		t.Fatalf("set M7-03 WebSocket deadline: %v", err)
	}
	const key = "dGhlIHNhbXBsZSBub25jZQ=="
	if _, err := io.WriteString(connection,
		"GET /gate/ws HTTP/1.1\r\nHost: "+productGatePublicHost+"\r\n"+
			"Connection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\n"+
			"Sec-WebSocket-Key: "+key+"\r\n\r\n"); err != nil {
		_ = connection.Close()
		t.Fatalf("write M7-03 WebSocket handshake: %v", err)
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		_ = connection.Close()
		t.Fatalf("read M7-03 WebSocket handshake: %v", err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols ||
		response.Header.Get("Sec-WebSocket-Accept") != productGateWebSocketAccept(key) {
		_ = connection.Close()
		t.Fatalf("M7-03 WebSocket handshake = %d accept %q",
			response.StatusCode, response.Header.Get("Sec-WebSocket-Accept"))
	}
	return connection, reader
}

func m7WriteMaskedFrame(t *testing.T, connection net.Conn, payload []byte) {
	t.Helper()
	mask := [4]byte{0x11, 0x22, 0x33, 0x44}
	frame := []byte{0x81, 0x80 | byte(len(payload)), mask[0], mask[1], mask[2], mask[3]}
	for index, value := range payload {
		frame = append(frame, value^mask[index%len(mask)])
	}
	if _, err := connection.Write(frame); err != nil {
		t.Fatalf("write M7-03 WebSocket frame: %v", err)
	}
}

func m7AssertConnectionClosed(t *testing.T, connection net.Conn, operation string) {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set M7-03 %s read deadline: %v", operation, err)
	}
	buffer := make([]byte, 1)
	if read, err := connection.Read(buffer); read != 0 || err == nil || m7IsTimeoutError(err) {
		t.Fatalf("M7-03 %s remained open: read=%d error=%v", operation, read, err)
	}
	_ = connection.Close()
}

func m7AssertReaderClosed(t *testing.T, reader *bufio.Reader, operation string) {
	t.Helper()
	if read, err := reader.ReadByte(); err == nil || m7IsTimeoutError(err) {
		t.Fatalf("M7-03 %s remained open: byte=%x error=%v", operation, read, err)
	}
}

func m7IsTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	var networkError net.Error
	return errors.Is(err, os.ErrDeadlineExceeded) ||
		(errors.As(err, &networkError) && networkError.Timeout())
}

func m7AssertHTTPBodyClosed(t *testing.T, body io.ReadCloser) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, readErr := io.ReadAll(body)
		done <- errors.Join(readErr, body.Close())
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		_ = body.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("M7-03 public HTTP body reader did not unblock after explicit Close")
		}
		t.Fatal("M7-03 public HTTP response body remained blocked after force close")
	}
}

func m7MustReadShutdownResources(t *testing.T) m7ResourceSample {
	t.Helper()
	sample, err := m7ReadResources()
	if err != nil {
		t.Fatalf("read M7-03 resource baseline: %v", err)
	}
	return sample
}

func m7AssertShutdownQuiescent(t *testing.T, fixture *m7ShutdownFixture, baseline m7ResourceSample) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	var final m7ResourceSample
	for {
		current, err := m7ReadResources()
		if err != nil {
			t.Fatalf("read M7-03 final resources: %v", err)
		}
		final = current
		snapshots := fixture.runtime.sessions.RuntimeStatusSnapshots()
		limits := fixture.runtime.limits.Snapshot()
		limitsZero := limits.Connectors == 0 && len(limits.ConnectorsByTunnel) == 0 &&
			limits.WorkTotal == 0 && limits.WorkConnecting == 0 && limits.WorkIdle == 0 &&
			limits.PendingOpens == 0 && limits.ActiveTotal == 0 && len(limits.ActiveByTunnel) == 0 &&
			len(limits.ActiveByService) == 0 && len(limits.ActiveBySource) == 0
		// Session 与 Quota 是本场景拥有的资源，必须精确归零。进程级 FD/goroutine
		// 基线还包含 Go test 运行时和先前用例的异步清理；终态低于基线不是泄漏，
		// 但任何高于基线的结果都必须继续等待并最终失败。
		if len(snapshots) == 0 && limitsZero && current.FDs <= baseline.FDs &&
			current.Goroutines <= baseline.Goroutines {
			t.Logf("M7-03 resources: baseline_fd=%d final_fd=%d baseline_goroutines=%d final_goroutines=%d",
				baseline.FDs, current.FDs, baseline.Goroutines, current.Goroutines)
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("M7-03 resources did not return to zero: baseline=%+v final=%+v sessions=%+v limits=%+v",
				baseline, final, snapshots, limits)
		case <-ticker.C:
		}
	}
}
