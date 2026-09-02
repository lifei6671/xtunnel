//go:build linux

package bootstrap

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/connector"
	"github.com/lifei6671/xtunnel/internal/application"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/repository"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	"github.com/lifei6671/xtunnel/internal/server/gateway"
)

const (
	productGateTunnelID      = "tun_01J00000000000000000000060"
	productGateHTTPServiceID = "svc_01J00000000000000000000060"
	productGateTCPServiceID  = "svc_01J00000000000000000000061"
	productGatePublicHost    = "public.product-gate.test"
	productGateOriginHost    = "origin.product-gate.test"
)

type productGateHTTPRequest struct {
	Host           string
	RequestURI     string
	Body           string
	ForwardedFor   string
	ForwardedProto string
}

type productGateHTTPRouteRecord struct {
	ID           string `gorm:"column:id"`
	ServiceID    string `gorm:"column:service_id"`
	Hostname     string `gorm:"column:hostname"`
	PathPrefix   string `gorm:"column:path_prefix"`
	PreserveHost bool   `gorm:"column:preserve_host"`
	Enabled      bool   `gorm:"column:enabled"`
	CreatedAt    int64  `gorm:"column:created_at"`
	UpdatedAt    int64  `gorm:"column:updated_at"`
}

func (productGateHTTPRouteRecord) TableName() string { return sqlite.HTTPRouteTable }

// TestProductDataPlaneEndToEnd 通过生产 Bootstrap 的完整装配路径验证 M4 公网数据面：
// 两种公网 Listener 共用同一个 Route Snapshot、Tunnel Proxy、Gateway Session、
// Token-only Agent 和 Origin Resolver。测试只用 GORM 预置冷启动 Desired State；
// 启动后所有请求都从真实 Socket 进入，不绕过 Listener、Handler 或限额 owner。
func TestProductDataPlaneEndToEnd(t *testing.T) {
	serverContext, cancelServer := context.WithCancel(context.Background())
	t.Cleanup(cancelServer)

	httpRequests := make(chan productGateHTTPRequest, 8)
	webSocketRequests := make(chan productGateHTTPRequest, 1)
	webSocketResults := make(chan error, 1)
	httpOriginRelease := make(chan struct{})
	var releaseHTTPOriginOnce sync.Once
	releaseHTTPOrigin := func() { releaseHTTPOriginOnce.Do(func() { close(httpOriginRelease) }) }
	httpOrigin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.EqualFold(request.Header.Get("Upgrade"), "websocket") {
			webSocketRequests <- productGateHTTPRequest{
				Host: request.Host, RequestURI: request.RequestURI,
				ForwardedFor:   request.Header.Get("X-Forwarded-For"),
				ForwardedProto: request.Header.Get("X-Forwarded-Proto"),
			}
			webSocketResults <- serveProductGateWebSocket(writer, request)
			return
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read Product Gate HTTP Origin body: %v", err)
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}
		httpRequests <- productGateHTTPRequest{
			Host: request.Host, RequestURI: request.RequestURI, Body: string(body),
			ForwardedFor:   request.Header.Get("X-Forwarded-For"),
			ForwardedProto: request.Header.Get("X-Forwarded-Proto"),
		}
		if request.RequestURI == "/gate//item/?query=1" {
			<-httpOriginRelease
		}
		writer.Header().Set("Content-Type", "text/plain")
		if _, err := io.WriteString(writer, "product-gate-http-ok"); err != nil {
			t.Errorf("write Product Gate HTTP Origin response: %v", err)
		}
	}))
	t.Cleanup(httpOrigin.Close)
	t.Cleanup(releaseHTTPOrigin)

	tcpOrigin := startProductGateTCPOrigin(t)
	publicAddress, publicPort := reserveProductGateTCPPort(t)
	runtimeDir := newRuntimeDirectory(t)
	dataDir := t.TempDir()
	resources, err := openServerStorage(serverContext, dataDir, runtimeDir)
	if err != nil {
		t.Fatalf("open Product Gate Server storage: %v", err)
	}
	resourcesClosed := false
	t.Cleanup(func() {
		if !resourcesClosed {
			if err := resources.Close(); err != nil {
				t.Errorf("close Product Gate Server storage: %v", err)
			}
		}
	})
	seedProductGateDesiredState(
		t, serverContext, resources, httpOrigin.Listener.Addr(), tcpOrigin.listener.Addr(), publicPort,
	)
	if err := resources.database.CreateFirstAdmin(serverContext, "admin", "product data plane gate password"); err != nil {
		t.Fatalf("create Product Gate Admin: %v", err)
	}

	config := gatewayLifecycleTestConfig(dataDir, "127.0.0.1:0")
	config.HTTPIngress.TrustedProxies = []string{"127.0.0.0/8"}
	config.TCPIngress.MinPort = int(publicPort)
	config.TCPIngress.MaxPort = int(publicPort)
	config.Limits.MaxHTTPBodyBytes = 8
	config.Limits.MaxHTTPHeaderBytes = 128
	config.Limits.MaxHTTPRequestsPerSourceIPPerSecond = 1
	config.Limits.MaxWorkConnections = 8
	config.Limits.MaxIdleWorkConnections = 8
	config.Limits.MaxConnectingWorkConnections = 8
	config.Limits.MaxPendingOpens = 4
	config.Limits.MaxOpenRatePerSourceIP = 1
	config.Limits.MaxOpenBurstPerSourceIP = 1
	config.Limits.MaxActiveConnections = 1
	config.Limits.MaxConnectionsPerTunnel = 1
	config.Limits.MaxConnectionsPerService = 1
	config.Limits.MaxConnectionsPerSourceIP = 1
	config.Limits.MaxPendingTLSHandshakes = 16
	config.Limits.MaxPendingAuth = 16

	closer, err := openGatewayAndBootstrapWith(
		serverContext, config, resources,
		slog.New(slog.NewJSONHandler(io.Discard, nil)), runtimeDir,
		func(context.Context, string, string, *sqlite.Store, func() error, func(error)) (io.Closer, error) {
			return nil, errors.New("existing Admin unexpectedly opened Bootstrap Socket")
		},
	)
	if err != nil {
		t.Fatalf("start Product Gate Server runtime: %v", err)
	}
	serverRuntime := closer.(*gatewayBootstrapCloser)
	runtimeClosed := false
	t.Cleanup(func() {
		if !runtimeClosed {
			if err := closer.Close(); err != nil {
				t.Errorf("close Product Gate Server runtime: %v", err)
			}
		}
	})
	if actual := serverRuntime.tcpIngress.Actual(); len(actual) != 1 || actual[0].Address != publicAddress {
		t.Fatalf("Product Gate TCP listeners = %+v, want %s", actual, publicAddress)
	}
	if serverRuntime.httpIngress.Addr() == nil || serverRuntime.gateway.Addr() == nil {
		t.Fatal("Product Gate HTTP or Gateway listener did not start")
	}

	issuedToken := issueProductGateToken(t, serverContext, resources, serverRuntime.gateway.Addr())
	stopAgent := startProductGateAgent(t, issuedToken, serverRuntime, 5)
	t.Cleanup(stopAgent)

	t.Run("public TCP SSH bytes half-close and limits", func(t *testing.T) {
		testProductGateTCP(t, publicAddress, tcpOrigin, serverRuntime)
	})
	t.Run("public HTTP route and limits", func(t *testing.T) {
		testProductGateHTTP(
			t, serverRuntime.httpIngress.Addr().String(), httpRequests, releaseHTTPOrigin, serverRuntime,
		)
	})
	t.Run("public WebSocket through production Agent", func(t *testing.T) {
		testProductGateWebSocket(
			t, serverRuntime.httpIngress.Addr().String(), webSocketRequests, webSocketResults, serverRuntime,
		)
	})
	t.Run("public TCP Pending OPEN capacity", testProductGatePendingOpen)

	// 各子场景已关闭自己创建的公网连接，但 Agent 侧 Handler 的 ACTIVE -> closed
	// 收敛是异步的；HTTP/1.1 Origin 还可能保留空闲 keep-alive。先关闭测试拥有的
	// Origin 连接并观察生产状态归零，再取消 Agent，避免把普通清理误测成 30 秒
	// 硬 Deadline 强关路径。
	httpOrigin.CloseClientConnections()
	waitForProductGateNoActiveWork(t, serverRuntime)
	stopAgent()
	if err := closer.Close(); err != nil {
		t.Fatalf("close Product Gate Server runtime: %v", err)
	}
	runtimeClosed = true
	if err := resources.Close(); err != nil {
		t.Fatalf("close Product Gate Server storage: %v", err)
	}
	resourcesClosed = true
}

func seedProductGateDesiredState(
	t *testing.T,
	ctx context.Context,
	resources *serverStorage,
	httpOrigin net.Addr,
	tcpOrigin net.Addr,
	publicPort uint16,
) {
	t.Helper()
	httpHost, httpPort := productGateHostPort(t, httpOrigin)
	tcpHost, tcpPort := productGateHostPort(t, tcpOrigin)
	if err := resources.database.WithTx(ctx, func(transaction repository.TxStore) error {
		if err := transaction.Tunnels().Create(ctx, repository.Tunnel{
			ID: productGateTunnelID, Name: "m4-product-gate", Version: 1, DesiredRevision: 1,
			CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			return err
		}
		for _, service := range []repository.Service{
			{
				ID: productGateHTTPServiceID, TunnelID: productGateTunnelID, Name: "product-http",
				RequiredRevision: 1, OriginScheme: repository.OriginSchemeHTTP,
				OriginHost: httpHost, OriginPort: httpPort, TLSVerify: true,
				OriginHTTPHost: productGateOriginHost, ConnectTimeoutMS: 2_000,
				Enabled: true, Version: 1, CreatedAt: 1, UpdatedAt: 1,
			},
			{
				ID: productGateTCPServiceID, TunnelID: productGateTunnelID, Name: "product-tcp",
				RequiredRevision: 1, OriginScheme: repository.OriginSchemeTCP,
				OriginHost: tcpHost, OriginPort: tcpPort, TLSVerify: true, ConnectTimeoutMS: 2_000,
				Enabled: true, Version: 1, CreatedAt: 1, UpdatedAt: 1,
			},
		} {
			if err := transaction.Services().Create(ctx, service); err != nil {
				return err
			}
		}
		if err := transaction.Routes().CreateTCP(ctx, repository.TCPRoute{
			ID: "product-tcp", ServiceID: productGateTCPServiceID, PublicPort: publicPort,
			Enabled: true, CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("seed Product Gate Tunnel, Services and TCP Route: %v", err)
	}

	// HTTP Route Repository 当前只有运行时读接口；这里仅在冷启动前写入类型化
	// Desired State fixture，不形成第二个生产写入口。全部 Route 写完后才推进 generation。
	database := openGatewayAuditDatabase(t, resources.dataDir)
	if err := database.Create(&productGateHTTPRouteRecord{
		ID: "product-http", ServiceID: productGateHTTPServiceID,
		Hostname: productGatePublicHost, PathPrefix: "/gate",
		PreserveHost: false, Enabled: true, CreatedAt: 1, UpdatedAt: 1,
	}).Error; err != nil {
		closeGatewayAuditDatabase(t, database)
		t.Fatalf("seed Product Gate HTTP Route through GORM: %v", err)
	}
	closeGatewayAuditDatabase(t, database)
	if err := resources.database.WithTx(ctx, func(transaction repository.TxStore) error {
		_, err := transaction.Routes().AdvanceGeneration(ctx, 0)
		return err
	}); err != nil {
		t.Fatalf("publish Product Gate Route generation: %v", err)
	}
}

func issueProductGateToken(
	t *testing.T,
	ctx context.Context,
	resources *serverStorage,
	gatewayAddress net.Addr,
) string {
	t.Helper()
	identity, err := gateway.LoadOrCreatePinnedIdentity(
		resources.dataDir, "gateway.example.test", false, time.Now(),
	)
	if err != nil {
		t.Fatalf("load Product Gate pinned identity: %v", err)
	}
	protector, err := application.NewAES256GCMTokenProtector(resources.tokenMasterKey[:])
	if err != nil {
		t.Fatalf("construct Product Gate Token protector: %v", err)
	}
	host, port := productGateHostPort(t, gatewayAddress)
	pin := identity.SPKIHash()
	issued, err := application.NewConnectionTokenService(resources.database, protector).Issue(
		ctx,
		application.IssueConnectionTokenInput{
			TunnelID: productGateTunnelID,
			Endpoint: &protocolv1.GatewayEndpoint{Host: host, Port: port},
			TLSTrust: &protocolv1.TlsTrustDescriptor{
				Mode: &protocolv1.TlsTrustDescriptor_PinnedSpkiSha256{
					PinnedSpkiSha256: &protocolv1.PinnedSPKITrust{SpkiSha256: pin[:]},
				},
			},
		},
	)
	if err != nil {
		t.Fatalf("issue Product Gate Connection Token: %v", err)
	}
	return issued.Token
}

func startProductGateAgent(
	t *testing.T,
	token string,
	runtime *gatewayBootstrapCloser,
	wantIdle uint32,
) func() {
	t.Helper()
	agentConfig, err := connector.HostConfig(token, "v0.1.0-m4-product-gate")
	if err != nil {
		t.Fatalf("build Product Gate Agent config: %v", err)
	}
	agentConfig.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	agentRuntime, err := connector.New(agentConfig)
	if err != nil {
		t.Fatalf("construct Product Gate Agent runtime: %v", err)
	}
	agentContext, cancelAgent := context.WithCancel(context.Background())
	agentDone := make(chan error, 1)
	go func() { agentDone <- agentRuntime.Run(agentContext) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancelAgent()
			select {
			case runErr := <-agentDone:
				if runErr != nil && !errors.Is(runErr, context.Canceled) {
					t.Errorf("stop Product Gate Agent runtime: %v", runErr)
				}
			case <-time.After(5 * time.Second):
				t.Errorf("Product Gate Agent runtime did not stop after cancellation")
			}
		})
	}

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
				return stop
			}
		}
		select {
		case runErr := <-agentDone:
			cancelAgent()
			t.Fatalf("Product Gate Agent exited before ready: %v", runErr)
		case <-deadline.C:
			cancelAgent()
			select {
			case <-agentDone:
			case <-time.After(5 * time.Second):
				t.Error("Product Gate Agent did not stop after readiness timeout")
			}
			t.Fatal("Product Gate Agent did not publish a ready two-Service Snapshot and IDLE Work")
		case <-ticker.C:
		}
	}
}

func testProductGateHTTP(
	t *testing.T,
	address string,
	observed <-chan productGateHTTPRequest,
	releaseOrigin func(),
	runtime *gatewayBootstrapCloser,
) {
	t.Helper()
	t.Cleanup(releaseOrigin)
	waitForProductGateIdleWork(t, runtime, 1)
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	// 全仓 Race 会放大调度成本；这里验证路由、限额和数据面语义，不把
	// Runner 上的 10 秒墙钟作为产品性能 SLO。
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	do := func(method, path, source, body string) *http.Response {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), method, "http://"+address+path, strings.NewReader(body))
		if err != nil {
			t.Fatalf("construct Product Gate HTTP request: %v", err)
		}
		request.Host = productGatePublicHost
		request.Header.Set("X-Forwarded-For", source)
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("execute Product Gate HTTP request: %v", err)
		}
		return response
	}

	firstRequest, err := http.NewRequestWithContext(
		t.Context(), http.MethodPost, "http://"+address+"/gate//item/?query=1", strings.NewReader("okay"),
	)
	if err != nil {
		t.Fatalf("construct first Product Gate HTTP request: %v", err)
	}
	firstRequest.Host = productGatePublicHost
	firstRequest.Header.Set("X-Forwarded-For", "198.51.100.10")
	type httpResult struct {
		response *http.Response
		err      error
	}
	firstResult := make(chan httpResult, 1)
	go func() {
		response, requestErr := client.Do(firstRequest)
		firstResult <- httpResult{response: response, err: requestErr}
	}()

	var originRequest productGateHTTPRequest
	var first httpResult
	select {
	case originRequest = <-observed:
	case first = <-firstResult:
		if first.response != nil {
			_ = first.response.Body.Close()
			t.Fatalf("first Product Gate HTTP request returned status %d before reaching Origin: %v", first.response.StatusCode, first.err)
		}
		t.Fatalf("first Product Gate HTTP request failed before reaching Origin: %v", first.err)
	case <-time.After(30 * time.Second):
		t.Fatal("Product Gate HTTP Origin did not receive routed request")
	}

	rateLimited := do(http.MethodGet, "/gate/rate", "198.51.100.10", "")
	releaseOrigin()
	select {
	case first = <-firstResult:
	case <-time.After(30 * time.Second):
		t.Fatal("first Product Gate HTTP request did not finish after Origin release")
	}
	if first.err != nil {
		t.Fatalf("execute first Product Gate HTTP request: %v", first.err)
	}
	body, err := io.ReadAll(first.response.Body)
	closeErr := first.response.Body.Close()
	if err != nil || closeErr != nil || first.response.StatusCode != http.StatusOK || string(body) != "product-gate-http-ok" {
		t.Fatalf("Product Gate HTTP response = status %d body %q, read %v close %v",
			first.response.StatusCode, body, err, closeErr)
	}
	if originRequest.Host != productGateOriginHost || originRequest.RequestURI != "/gate//item/?query=1" ||
		originRequest.Body != "okay" || originRequest.ForwardedFor != "198.51.100.10" ||
		originRequest.ForwardedProto != "http" {
		t.Fatalf("Product Gate Origin request = %+v", originRequest)
	}
	assertProductGateHTTPError(t, rateLimited, http.StatusTooManyRequests, "RATE_LIMITED")

	oversize := do(http.MethodPost, "/gate/body", "198.51.100.11", "123456789")
	assertProductGateHTTPError(t, oversize, http.StatusRequestEntityTooLarge, "REQUEST_BODY_TOO_LARGE")
	assertNoProductGateOriginRequest(t, observed, "oversize body")

	invalid := rawProductGateHTTPResponse(t, address,
		"GET /gate/%2Fhidden HTTP/1.1\r\nHost: "+productGatePublicHost+
			"\r\nX-Forwarded-For: 198.51.100.12\r\nConnection: close\r\n\r\n")
	assertProductGateHTTPError(t, invalid, http.StatusBadRequest, "INVALID_PATH")
	assertNoProductGateOriginRequest(t, observed, "invalid encoded separator")

	headerTooLarge := rawProductGateHTTPResponse(t, address,
		"GET /gate/header HTTP/1.1\r\nHost: "+productGatePublicHost+
			"\r\nX-Oversize: "+strings.Repeat("x", 8192)+"\r\n\r\n")
	if err := headerTooLarge.Body.Close(); err != nil {
		t.Fatalf("close Product Gate 431 response: %v", err)
	}
	if headerTooLarge.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("Product Gate oversized Header status = %d, want 431", headerTooLarge.StatusCode)
	}
}

func serveProductGateWebSocket(writer http.ResponseWriter, request *http.Request) error {
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		return errors.New("Product Gate HTTP Origin does not support Hijack")
	}
	connection, buffered, err := hijacker.Hijack()
	if err != nil {
		return fmt.Errorf("hijack Product Gate HTTP Origin: %w", err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return fmt.Errorf("set Product Gate WebSocket Origin deadline: %w", err)
	}
	key := request.Header.Get("Sec-WebSocket-Key")
	acceptDigest := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")) //nolint:gosec // RFC 6455 固定握手算法，不用于安全哈希。
	if _, err := fmt.Fprintf(buffered,
		"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
		base64.StdEncoding.EncodeToString(acceptDigest[:]),
	); err != nil {
		return fmt.Errorf("write Product Gate WebSocket handshake: %w", err)
	}
	if err := buffered.Flush(); err != nil {
		return fmt.Errorf("flush Product Gate WebSocket handshake: %w", err)
	}
	frame := make([]byte, 11)
	if _, err := io.ReadFull(buffered, frame); err != nil {
		return fmt.Errorf("read Product Gate masked WebSocket frame: %w", err)
	}
	if frame[0] != 0x81 || frame[1] != 0x85 {
		return fmt.Errorf("Product Gate WebSocket frame header = %x %x, want 81 85", frame[0], frame[1])
	}
	payload := make([]byte, 5)
	for index := range payload {
		payload[index] = frame[6+index] ^ frame[2+index%4]
	}
	if string(payload) != "hello" {
		return fmt.Errorf("Product Gate WebSocket payload = %q, want hello", payload)
	}
	if _, err := connection.Write(append([]byte{0x81, 0x05}, payload...)); err != nil {
		return fmt.Errorf("write Product Gate WebSocket echo: %w", err)
	}
	return nil
}

func testProductGateWebSocket(
	t *testing.T,
	address string,
	observed <-chan productGateHTTPRequest,
	results <-chan error,
	runtime *gatewayBootstrapCloser,
) {
	t.Helper()
	waitForProductGateIdleWork(t, runtime, 1)
	connection, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatalf("dial Product Gate WebSocket listener: %v", err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set Product Gate WebSocket client deadline: %v", err)
	}
	const webSocketKey = "dGhlIHNhbXBsZSBub25jZQ=="
	if _, err := io.WriteString(connection,
		"GET /gate/ws?via=production HTTP/1.1\r\nHost: "+productGatePublicHost+"\r\n"+
			"Connection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\n"+
			"Sec-WebSocket-Key: "+webSocketKey+"\r\nX-Forwarded-For: 198.51.100.13\r\n\r\n"); err != nil {
		t.Fatalf("write Product Gate WebSocket handshake: %v", err)
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read Product Gate WebSocket handshake: %v", err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols ||
		response.Header.Get("Sec-WebSocket-Accept") != productGateWebSocketAccept(webSocketKey) {
		t.Fatalf("Product Gate WebSocket handshake = %d accept %q",
			response.StatusCode, response.Header.Get("Sec-WebSocket-Accept"))
	}
	mask := [4]byte{0x11, 0x22, 0x33, 0x44}
	payload := []byte("hello")
	frame := []byte{0x81, 0x85, mask[0], mask[1], mask[2], mask[3]}
	for index, value := range payload {
		frame = append(frame, value^mask[index%len(mask)])
	}
	if _, err := connection.Write(frame); err != nil {
		t.Fatalf("write Product Gate masked WebSocket frame: %v", err)
	}
	echoed := make([]byte, 7)
	if _, err := io.ReadFull(reader, echoed); err != nil {
		t.Fatalf("read Product Gate WebSocket echo: %v", err)
	}
	if !bytes.Equal(echoed, append([]byte{0x81, 0x05}, payload...)) {
		t.Fatalf("Product Gate WebSocket echo = %x", echoed)
	}
	select {
	case request := <-observed:
		if request.Host != productGateOriginHost || request.RequestURI != "/gate/ws?via=production" ||
			request.ForwardedFor != "198.51.100.13" || request.ForwardedProto != "http" {
			t.Fatalf("Product Gate WebSocket Origin request = %+v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("Product Gate WebSocket Origin did not observe Upgrade")
	}
	select {
	case originErr := <-results:
		if originErr != nil {
			t.Fatalf("Product Gate WebSocket Origin: %v", originErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Product Gate WebSocket Origin did not finish frame echo")
	}
}

func productGateWebSocketAccept(key string) string {
	digest := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")) //nolint:gosec // RFC 6455 固定握手算法。
	return base64.StdEncoding.EncodeToString(digest[:])
}

func testProductGateTCP(
	t *testing.T,
	publicAddress string,
	origin *productGateTCPOrigin,
	runtime *gatewayBootstrapCloser,
) {
	t.Helper()
	waitForProductGateIdleWork(t, runtime, 2)

	// 两条同源连接先完成公网 TCP 握手，再读取任何结果。Burst=1 保证一条
	// 进入 OPEN，另一条在 Accept 后由来源 Rate 收敛；集合断言不依赖调度顺序。
	contest := []*net.TCPConn{
		dialProductGateTCP(t, publicAddress, "127.0.0.9"),
		dialProductGateTCP(t, publicAddress, "127.0.0.9"),
	}
	contestEchoDone := startProductGateOriginEcho(origin.next(t, "SSH Accept contest"))
	ssh := assertProductGateConnectionContest(
		t, contest, []byte("SSH-2.0-XTunnel-M4-Gate\r\n"), "SSH/Accept Rate",
	)
	origin.assertNoNext(t, "source Accept OPEN rate")

	// 不同来源绕过来源 Rate，但在 Agent 已拨通 Origin、返回 OPEN_OK 后由
	// Global ACTIVE=1 拒绝；公网负载不得在 ACTIVE 提交前进入 Origin。
	activeRejected := dialProductGateTCP(t, publicAddress, "127.0.0.1")
	assertProductGateTCPRejected(t, activeRejected, "global ACTIVE capacity")
	assertProductGateOriginClosedWithoutPayload(t, origin.next(t, "global ACTIVE capacity"), "global ACTIVE capacity")
	finishProductGateTCP(t, ssh, contestEchoDone, "SSH")

	raw := dialProductGateTCP(t, publicAddress, "127.0.0.2")
	defer raw.Close()
	rawEchoDone := startProductGateOriginEcho(origin.next(t, "Raw TCP"))
	rawPayload := []byte{0x00, 0xff, 0x42, 0x00, 0x7f}
	assertProductGateRoundTrip(t, raw, rawPayload, "Raw TCP")
	finishProductGateTCP(t, raw, rawEchoDone, "Raw TCP")
}

// testProductGatePendingOpen 使用独立的完整 Server/Agent Runtime，把 Work 总量
// 固定为 1。首条 ACTIVE 占住唯一 Work 后，第二条公网连接稳定停留在 Pending
// OPEN，第三条才能无墙钟竞态地验证 MaxPendingOpens=1。
func testProductGatePendingOpen(t *testing.T) {
	serverContext, cancelServer := context.WithCancel(context.Background())
	t.Cleanup(cancelServer)
	httpOrigin := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(httpOrigin.Close)
	tcpOrigin := startProductGateTCPOrigin(t)
	publicAddress, publicPort := reserveProductGateTCPPort(t)
	runtimeDir := newRuntimeDirectory(t)
	dataDir := t.TempDir()
	resources, err := openServerStorage(serverContext, dataDir, runtimeDir)
	if err != nil {
		t.Fatalf("open Pending OPEN Product Gate storage: %v", err)
	}
	t.Cleanup(func() {
		if err := resources.Close(); err != nil {
			t.Errorf("close Pending OPEN Product Gate storage: %v", err)
		}
	})
	seedProductGateDesiredState(
		t, serverContext, resources, httpOrigin.Listener.Addr(), tcpOrigin.listener.Addr(), publicPort,
	)
	if err := resources.database.CreateFirstAdmin(serverContext, "admin", "pending open product gate password"); err != nil {
		t.Fatalf("create Pending OPEN Product Gate Admin: %v", err)
	}

	config := gatewayLifecycleTestConfig(dataDir, "127.0.0.1:0")
	config.TCPIngress.MinPort = int(publicPort)
	config.TCPIngress.MaxPort = int(publicPort)
	config.Limits.MaxWorkConnections = 1
	config.Limits.MaxIdleWorkConnections = 1
	config.Limits.MaxConnectingWorkConnections = 1
	config.Limits.MaxPendingOpens = 1
	config.Limits.MaxOpenRatePerSourceIP = 4
	config.Limits.MaxOpenBurstPerSourceIP = 4
	config.Limits.MaxActiveConnections = 4
	config.Limits.MaxConnectionsPerTunnel = 4
	config.Limits.MaxConnectionsPerService = 4
	config.Limits.MaxConnectionsPerSourceIP = 4
	config.Limits.MaxPendingTLSHandshakes = 16
	config.Limits.MaxPendingAuth = 16

	closer, err := openGatewayAndBootstrapWith(
		serverContext, config, resources,
		slog.New(slog.NewJSONHandler(io.Discard, nil)), runtimeDir,
		func(context.Context, string, string, *sqlite.Store, func() error, func(error)) (io.Closer, error) {
			return nil, errors.New("existing Admin unexpectedly opened Pending OPEN Bootstrap Socket")
		},
	)
	if err != nil {
		t.Fatalf("start Pending OPEN Product Gate runtime: %v", err)
	}
	runtime := closer.(*gatewayBootstrapCloser)
	t.Cleanup(func() {
		if err := closer.Close(); err != nil {
			t.Errorf("close Pending OPEN Product Gate runtime: %v", err)
		}
	})
	issuedToken := issueProductGateToken(t, serverContext, resources, runtime.gateway.Addr())
	stopAgent := startProductGateAgent(t, issuedToken, runtime, 1)
	t.Cleanup(stopAgent)

	active := dialProductGateTCP(t, publicAddress, "127.0.0.21")
	defer active.Close()
	activeEchoDone := startProductGateOriginEcho(tcpOrigin.next(t, "Pending OPEN holder"))
	assertProductGateRoundTrip(t, active, []byte("hold-the-only-work"), "Pending OPEN holder")

	pending := dialProductGateTCP(t, publicAddress, "127.0.0.22")
	defer pending.Close()
	pendingPayload := []byte("pending-open-payload")
	if _, err := pending.Write(pendingPayload); err != nil {
		t.Fatalf("write Product Gate Pending OPEN payload: %v", err)
	}
	assertProductGateTCPPending(t, pending)
	tcpOrigin.assertNoNext(t, "first Pending OPEN")

	rejected := dialProductGateTCP(t, publicAddress, "127.0.0.23")
	assertProductGateTCPRejected(t, rejected, "Pending OPEN capacity")
	tcpOrigin.assertNoNext(t, "Pending OPEN capacity")

	finishProductGateTCP(t, active, activeEchoDone, "Pending OPEN holder")
	if err := pending.Close(); err != nil {
		t.Fatalf("close Product Gate Pending OPEN client: %v", err)
	}
}

func waitForProductGateIdleWork(t *testing.T, runtime *gatewayBootstrapCloser, want uint32) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		for _, snapshot := range runtime.sessions.RuntimeStatusSnapshots() {
			if snapshot.TunnelID == productGateTunnelID && snapshot.CurrentControlSession &&
				snapshot.WorkPool.Idle >= want {
				return
			}
		}
		select {
		case <-deadline.C:
			t.Fatalf("Product Gate Agent did not replenish %d IDLE Work connections", want)
		case <-ticker.C:
		}
	}
}

func waitForProductGateNoActiveWork(t *testing.T, runtime *gatewayBootstrapCloser) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		for _, snapshot := range runtime.sessions.RuntimeStatusSnapshots() {
			if snapshot.TunnelID == productGateTunnelID && snapshot.CurrentControlSession &&
				snapshot.WorkPool.Active == 0 {
				return
			}
		}
		select {
		case <-deadline.C:
			t.Fatal("Product Gate Agent ACTIVE Work did not settle before graceful shutdown")
		case <-ticker.C:
		}
	}
}

func waitForProductGateNoPendingOpen(t *testing.T, runtime *gatewayBootstrapCloser) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if runtime.limits.Snapshot().PendingOpens == 0 {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("Product Gate Pending OPEN did not settle before ACTIVE release: %+v", runtime.limits.Snapshot())
		case <-ticker.C:
		}
	}
}

func startProductGateOriginEcho(originPeer net.Conn) <-chan error {
	echoDone := make(chan error, 1)
	go func() {
		reader := struct{ io.Reader }{Reader: originPeer}
		writer := struct{ io.Writer }{Writer: originPeer}
		_, copyErr := io.Copy(writer, reader)
		echoDone <- errors.Join(copyErr, originPeer.Close())
	}()
	return echoDone
}

func assertProductGateConnectionContest(
	t *testing.T,
	connections []*net.TCPConn,
	payload []byte,
	operation string,
) *net.TCPConn {
	t.Helper()
	var successful *net.TCPConn
	rejected := 0
	for index, connection := range connections {
		if _, err := connection.Write(payload); err != nil {
			rejected++
			_ = connection.Close()
			continue
		}
		echoed := make([]byte, len(payload))
		read, err := io.ReadFull(connection, echoed)
		switch {
		case err == nil && bytes.Equal(echoed, payload):
			if successful != nil {
				t.Fatalf("Product Gate %s contest produced multiple usable connections", operation)
			}
			successful = connection
		case read == 0 && err != nil:
			rejected++
			_ = connection.Close()
		default:
			t.Fatalf("Product Gate %s candidate %d returned %d bytes %q with error %v",
				operation, index, read, echoed[:read], err)
		}
	}
	if successful == nil || rejected != len(connections)-1 {
		t.Fatalf("Product Gate %s contest = success %t rejected %d, want one success and %d rejected",
			operation, successful != nil, rejected, len(connections)-1)
	}
	return successful
}

func assertProductGateRoundTrip(t *testing.T, connection net.Conn, payload []byte, operation string) {
	t.Helper()
	if _, err := connection.Write(payload); err != nil {
		t.Fatalf("write Product Gate %s payload: %v", operation, err)
	}
	echoed := make([]byte, len(payload))
	if _, err := io.ReadFull(connection, echoed); err != nil {
		t.Fatalf("read Product Gate %s payload: %v", operation, err)
	}
	if !bytes.Equal(echoed, payload) {
		t.Fatalf("Product Gate %s echo = %q, want %q", operation, echoed, payload)
	}
}

func finishProductGateTCP(t *testing.T, connection *net.TCPConn, echoDone <-chan error, operation string) {
	t.Helper()
	if err := connection.CloseWrite(); err != nil {
		t.Fatalf("half-close Product Gate %s client: %v", operation, err)
	}
	trailing, err := io.ReadAll(connection)
	if err != nil {
		t.Fatalf("finish Product Gate %s stream: %v", operation, err)
	}
	if len(trailing) != 0 {
		t.Fatalf("Product Gate %s returned unexpected trailing bytes %q", operation, trailing)
	}
	select {
	case echoErr := <-echoDone:
		if echoErr != nil && !errors.Is(echoErr, net.ErrClosed) {
			t.Fatalf("Product Gate %s Origin echo: %v", operation, echoErr)
		}
	case <-time.After(time.Second):
		t.Fatalf("Product Gate %s Origin did not finish after public Half-Close", operation)
	}
}

func assertProductGateTCPPending(t *testing.T, connection *net.TCPConn) {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Fatalf("set Product Gate Pending OPEN deadline: %v", err)
	}
	buffer := make([]byte, 1)
	read, err := connection.Read(buffer)
	if read != 0 {
		t.Fatalf("Product Gate Pending OPEN returned %d bytes before Work was available", read)
	}
	if networkErr, ok := err.(net.Error); !ok || !networkErr.Timeout() {
		t.Fatalf("Product Gate first Pending OPEN did not remain pending: %v", err)
	}
	if err := connection.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("restore Product Gate Pending OPEN deadline: %v", err)
	}
}

func assertProductGateHTTPError(t *testing.T, response *http.Response, status int, code string) {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if err != nil || closeErr != nil || response.StatusCode != status || strings.TrimSpace(string(body)) != code {
		t.Fatalf("Product Gate HTTP error = status %d body %q, want %d %s; read %v close %v",
			response.StatusCode, body, status, code, err, closeErr)
	}
}

func assertNoProductGateOriginRequest(t *testing.T, observed <-chan productGateHTTPRequest, operation string) {
	t.Helper()
	select {
	case request := <-observed:
		t.Fatalf("%s reached Product Gate HTTP Origin: %+v", operation, request)
	case <-time.After(100 * time.Millisecond):
	}
}

func rawProductGateHTTPResponse(t *testing.T, address, request string) *http.Response {
	t.Helper()
	connection, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatalf("dial Product Gate HTTP listener: %v", err)
	}
	if err := connection.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		_ = connection.Close()
		t.Fatalf("set Product Gate raw HTTP deadline: %v", err)
	}
	if _, err := io.WriteString(connection, request); err != nil {
		_ = connection.Close()
		t.Fatalf("write Product Gate raw HTTP request: %v", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodGet})
	if err != nil {
		_ = connection.Close()
		t.Fatalf("read Product Gate raw HTTP response: %v", err)
	}
	response.Body = &productGateResponseBody{ReadCloser: response.Body, connection: connection}
	return response
}

type productGateResponseBody struct {
	io.ReadCloser
	connection net.Conn
}

func (body *productGateResponseBody) Close() error {
	return errors.Join(body.ReadCloser.Close(), body.connection.Close())
}

type productGateTCPOrigin struct {
	listener net.Listener
	peers    chan net.Conn
	done     chan error
	cancel   context.CancelFunc
	once     sync.Once
}

func startProductGateTCPOrigin(t *testing.T) *productGateTCPOrigin {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen Product Gate TCP Origin: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	origin := &productGateTCPOrigin{
		listener: listener, peers: make(chan net.Conn, 4), done: make(chan error, 1), cancel: cancel,
	}
	go func() {
		var resultErr error
		defer func() { origin.done <- resultErr }()
		for {
			peer, acceptErr := listener.Accept()
			if acceptErr != nil {
				if ctx.Err() == nil && !errors.Is(acceptErr, net.ErrClosed) {
					resultErr = acceptErr
				}
				return
			}
			select {
			case origin.peers <- peer:
			case <-ctx.Done():
				resultErr = errors.Join(resultErr, peer.Close())
				return
			}
		}
	}()
	t.Cleanup(func() {
		origin.close(t)
	})
	return origin
}

func (origin *productGateTCPOrigin) next(t *testing.T, operation string) net.Conn {
	t.Helper()
	select {
	case peer := <-origin.peers:
		return peer
	case err := <-origin.done:
		t.Fatalf("Product Gate TCP Origin stopped before %s accept: %v", operation, err)
	case <-time.After(3 * time.Second):
		t.Fatalf("Product Gate TCP Origin did not receive %s Agent connection", operation)
	}
	return nil
}

func (origin *productGateTCPOrigin) assertNoNext(t *testing.T, operation string) {
	t.Helper()
	select {
	case peer := <-origin.peers:
		_ = peer.Close()
		t.Fatalf("%s reached Product Gate TCP Origin", operation)
	case err := <-origin.done:
		t.Fatalf("Product Gate TCP Origin stopped during %s: %v", operation, err)
	case <-time.After(100 * time.Millisecond):
	}
}

func (origin *productGateTCPOrigin) close(t *testing.T) {
	t.Helper()
	origin.once.Do(func() {
		origin.cancel()
		closeErr := origin.listener.Close()
		select {
		case runErr := <-origin.done:
			if err := errors.Join(closeErr, runErr); err != nil && !errors.Is(err, net.ErrClosed) {
				t.Errorf("close Product Gate TCP Origin: %v", err)
			}
		case <-time.After(time.Second):
			t.Errorf("Product Gate TCP Origin did not stop after Listener close")
		}
		for {
			select {
			case peer := <-origin.peers:
				closeErr = errors.Join(closeErr, peer.Close())
			default:
				if closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
					t.Errorf("close queued Product Gate TCP Origin peers: %v", closeErr)
				}
				return
			}
		}
	})
}

func reserveProductGateTCPPort(t *testing.T) (string, uint16) {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("reserve Product Gate public TCP port: %v", err)
	}
	address := listener.Addr().String()
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	if err := listener.Close(); err != nil {
		t.Fatalf("release Product Gate public TCP port: %v", err)
	}
	return address, port
}

func productGateHostPort(t *testing.T, address net.Addr) (string, uint32) {
	t.Helper()
	host, portText, err := net.SplitHostPort(address.String())
	if err != nil {
		t.Fatalf("split Product Gate address %q: %v", address, err)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		t.Fatalf("parse Product Gate port %q: %v", portText, err)
	}
	return host, uint32(port)
}

func dialProductGateTCP(t *testing.T, address, source string) *net.TCPConn {
	t.Helper()
	dialer := net.Dialer{
		Timeout:   time.Second,
		LocalAddr: &net.TCPAddr{IP: net.ParseIP(source)},
	}
	connection, err := dialer.DialContext(t.Context(), "tcp4", address)
	if err != nil {
		t.Fatalf("dial Product Gate TCP listener from %s: %v", source, err)
	}
	tcpConnection := connection.(*net.TCPConn)
	if err := tcpConnection.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		_ = tcpConnection.Close()
		t.Fatalf("set Product Gate TCP deadline from %s: %v", source, err)
	}
	return tcpConnection
}

func assertProductGateTCPRejected(t *testing.T, connection *net.TCPConn, operation string) {
	t.Helper()
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		t.Fatalf("set %s rejection deadline: %v", operation, err)
	}
	_, writeErr := connection.Write([]byte("must-not-reach-origin"))
	if writeErr != nil {
		return
	}
	buffer := make([]byte, 1)
	read, readErr := connection.Read(buffer)
	if read != 0 || readErr == nil {
		t.Fatalf("%s connection returned %d in-band bytes before close: %v", operation, read, readErr)
	}
	if networkErr, ok := readErr.(net.Error); ok && networkErr.Timeout() {
		t.Fatalf("%s connection was not closed before the rejection deadline", operation)
	}
}

func assertProductGateOriginClosedWithoutPayload(t *testing.T, connection net.Conn, operation string) {
	t.Helper()
	defer connection.Close()
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set Product Gate Origin %s deadline: %v", operation, err)
	}
	buffer := make([]byte, 1)
	read, err := connection.Read(buffer)
	if read != 0 {
		t.Fatalf("%s forwarded %d bytes to Product Gate Origin before ACTIVE commit", operation, read)
	}
	if err == nil {
		t.Fatalf("%s left Product Gate Origin connection open", operation)
	}
	if networkErr, ok := err.(net.Error); ok && networkErr.Timeout() {
		t.Fatalf("%s did not close Product Gate Origin before deadline", operation)
	}
}
