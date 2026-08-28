package httpingress

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/tunnel"
)

const testWebSocketAccept = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="

type connectionPairFactory func() (net.Conn, net.Conn, error)

type upgradeDialer struct {
	newPair connectionPairFactory
	origins chan net.Conn

	mu    sync.Mutex
	calls []tunnelDialCall
}

func newUpgradeDialer(factory connectionPairFactory) *upgradeDialer {
	return &upgradeDialer{newPair: factory, origins: make(chan net.Conn, 4)}
}

func (dialer *upgradeDialer) Dial(
	ctx context.Context,
	request tunnel.DialRequest,
) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	backend, origin, err := dialer.newPair()
	if err != nil {
		return nil, err
	}
	dialer.mu.Lock()
	dialer.calls = append(dialer.calls, tunnelDialCall{
		TunnelID: request.TunnelID, ServiceID: request.ServiceID,
		RequiredRevision: request.RequiredRevision,
		Ingress:          request.Ingress, Client: request.ClientAddr,
	})
	dialer.mu.Unlock()
	select {
	case dialer.origins <- origin:
		return backend, nil
	case <-ctx.Done():
		return nil, errors.Join(ctx.Err(), backend.Close(), origin.Close())
	}
}

func (dialer *upgradeDialer) callsSnapshot() []tunnelDialCall {
	dialer.mu.Lock()
	defer dialer.mu.Unlock()
	return append([]tunnelDialCall(nil), dialer.calls...)
}

func TestWebSocketUpgradeStreamsFramesAndForwardedMetadata(t *testing.T) {
	server, dialer := startWebSocketServer(t, pipeConnectionPair)
	client, clientReader, origin, originReader, request := openWebSocket(t, server, dialer)
	defer client.Close()
	defer origin.Close()

	if request.Host != "public.example.com" || request.URL.Path != "/socket" ||
		!httpgutsHeaderHasToken(request.Header.Values("Connection"), "upgrade") ||
		!strings.EqualFold(request.Header.Get("Upgrade"), "websocket") {
		t.Fatalf("Origin Upgrade request = host %q path %q connection %v upgrade %q",
			request.Host, request.URL.Path, request.Header.Values("Connection"), request.Header.Get("Upgrade"))
	}
	assertForwardedHeaders(t, request.Header, "198.51.100.40", "https", "public.example.com")
	assertSingleHTTPDialClient(t, dialer.callsSnapshot(), "198.51.100.40")

	clientText := maskedWebSocketFrame(0x1, []byte("client-message"))
	if _, err := client.Write(clientText); err != nil {
		t.Fatalf("write Client WebSocket frame: %v", err)
	}
	assertReadBytes(t, origin, originReader, clientText, "Origin client-message frame")

	originText := unmaskedWebSocketFrame(0x1, []byte("origin-message"))
	if _, err := origin.Write(originText); err != nil {
		t.Fatalf("write Origin WebSocket frame: %v", err)
	}
	assertReadBytes(t, client, clientReader, originText, "Client origin-message frame")

	ping := unmaskedWebSocketFrame(0x9, []byte("p"))
	if _, err := origin.Write(ping); err != nil {
		t.Fatalf("write Origin Ping frame: %v", err)
	}
	assertReadBytes(t, client, clientReader, ping, "Client Ping frame")
	pong := maskedWebSocketFrame(0xa, []byte("p"))
	if _, err := client.Write(pong); err != nil {
		t.Fatalf("write Client Pong frame: %v", err)
	}
	assertReadBytes(t, origin, originReader, pong, "Origin Pong frame")
}

func TestWebSocketUpgradeAlwaysUsesFreshTunnelConnection(t *testing.T) {
	server, dialer := startWebSocketServer(t, pipeConnectionPair)
	firstClient, _, firstOrigin, _, _ := openWebSocket(t, server, dialer)
	defer firstClient.Close()
	defer firstOrigin.Close()
	secondClient, secondReader, secondOrigin, _, _ := openWebSocket(t, server, dialer)
	defer secondClient.Close()
	defer secondOrigin.Close()

	if calls := dialer.callsSnapshot(); len(calls) != 2 {
		t.Fatalf("two concurrent Upgrades used %d Tunnel Dials, want 2 fresh connections", len(calls))
	}
	frame := unmaskedWebSocketFrame(0x1, []byte("second-connection"))
	if _, err := secondOrigin.Write(frame); err != nil {
		t.Fatalf("write through second WebSocket connection: %v", err)
	}
	assertReadBytes(t, secondClient, secondReader, frame, "second WebSocket connection")
}

func TestWebSocketUpgradeRejectsRequestBodyBeforeTunnelDial(t *testing.T) {
	tests := []struct {
		name         string
		maxBodyBytes int64
		status       int
		code         string
		mutate       func(*http.Request)
	}{
		{
			name:         "known length over HTTP body limit",
			maxBodyBytes: 4,
			status:       http.StatusRequestEntityTooLarge,
			code:         "REQUEST_BODY_TOO_LARGE",
		},
		{
			name:   "known length with expect continue",
			status: http.StatusNotImplemented,
			code:   "UPGRADE_NOT_SUPPORTED",
			mutate: func(request *http.Request) {
				request.Header.Set("Expect", "100-continue")
			},
		},
		{
			name:   "unknown chunked length",
			status: http.StatusNotImplemented,
			code:   "UPGRADE_NOT_SUPPORTED",
			mutate: func(request *http.Request) {
				request.ContentLength = -1
				request.TransferEncoding = []string{"chunked"}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, _ := startRouteManager(t, baseHTTPRouteState(1))
			var dialCount atomic.Int32
			dialer := dialerFunc(func(context.Context, tunnel.DialRequest) (net.Conn, error) {
				dialCount.Add(1)
				return nil, errors.New("Dial must not run for a WebSocket request with a Body")
			})
			handler := newTestHandler(t, manager, dialer)
			if test.maxBodyBytes > 0 {
				handler.maxBodyBytes = test.maxBodyBytes
			}
			request := httptest.NewRequest(http.MethodGet, "/socket", strings.NewReader("slow-body"))
			request.Host = "public.example.com"
			request.Header.Set("Connection", "Upgrade")
			request.Header.Set("Upgrade", "websocket")
			if test.mutate != nil {
				test.mutate(request)
			}
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != test.status || strings.TrimSpace(response.Body.String()) != test.code {
				t.Fatalf("response = (%d, %q), want (%d, %s)",
					response.Code, response.Body.String(), test.status, test.code)
			}
			if got := dialCount.Load(); got != 0 {
				t.Fatalf("Tunnel Dial count = %d, want 0", got)
			}
			if response.Header().Get("Connection") != "close" || !request.Close {
				t.Fatalf("rejected WebSocket Body Connection=%q request.Close=%v, want close/true",
					response.Header().Get("Connection"), request.Close)
			}
			if got := handler.limits.Snapshot(); got.ActiveTotal != 0 {
				t.Fatalf("ACTIVE limits after rejected WebSocket Body = %#v, want empty", got)
			}
		})
	}
}

func TestWebSocketPreservesTCPHalfClose(t *testing.T) {
	server, dialer := startWebSocketServer(t, loopbackTCPConnectionPair)
	client, clientReader, origin, originReader, _ := openWebSocket(t, server, dialer)
	defer client.Close()
	defer origin.Close()

	clientTCP, ok := client.(*net.TCPConn)
	if !ok {
		t.Fatalf("Client connection type = %T, want *net.TCPConn", client)
	}
	originTCP, ok := origin.(*net.TCPConn)
	if !ok {
		t.Fatalf("Origin connection type = %T, want *net.TCPConn", origin)
	}
	if err := clientTCP.CloseWrite(); err != nil {
		t.Fatalf("Client CloseWrite() error = %v", err)
	}
	if err := origin.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set Origin ReadDeadline: %v", err)
	}
	if _, err := originReader.ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatalf("Origin read after Client CloseWrite = %v, want EOF", err)
	}
	if err := origin.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear Origin ReadDeadline: %v", err)
	}

	finalFrame := unmaskedWebSocketFrame(0x8, []byte{0x03, 0xe8})
	if _, err := origin.Write(finalFrame); err != nil {
		t.Fatalf("write Origin Close frame after Client CloseWrite: %v", err)
	}
	if err := originTCP.CloseWrite(); err != nil {
		t.Fatalf("Origin CloseWrite() error = %v", err)
	}
	assertReadBytes(t, client, clientReader, finalFrame, "Client final frame after CloseWrite")
	if err := client.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set Client ReadDeadline: %v", err)
	}
	if _, err := clientReader.ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatalf("Client read after Origin CloseWrite = %v, want EOF", err)
	}
}

func TestWebSocketClientAndOriginDisconnectReleasePeer(t *testing.T) {
	t.Run("client disconnect closes Origin", func(t *testing.T) {
		server, dialer := startWebSocketServer(t, pipeConnectionPair)
		client, _, origin, originReader, _ := openWebSocket(t, server, dialer)
		defer origin.Close()

		if err := client.Close(); err != nil {
			t.Fatalf("close WebSocket client: %v", err)
		}
		assertConnectionClosed(t, origin, originReader, "Origin")
	})

	t.Run("Origin disconnect closes client", func(t *testing.T) {
		server, dialer := startWebSocketServer(t, pipeConnectionPair)
		client, clientReader, origin, _, _ := openWebSocket(t, server, dialer)
		defer client.Close()

		if err := origin.Close(); err != nil {
			t.Fatalf("close WebSocket Origin: %v", err)
		}
		assertConnectionClosed(t, client, clientReader, "Client")
	})
}

func TestWebSocketRemainsUsableWithoutShortTotalTimeout(t *testing.T) {
	server, dialer := startWebSocketServer(t, pipeConnectionPair)
	client, clientReader, origin, _, _ := openWebSocket(t, server, dialer)
	defer client.Close()
	defer origin.Close()

	time.Sleep(250 * time.Millisecond)
	frame := unmaskedWebSocketFrame(0x1, []byte("still-open"))
	if _, err := origin.Write(frame); err != nil {
		t.Fatalf("write after idle period: %v", err)
	}
	assertReadBytes(t, client, clientReader, frame, "Client frame after idle period")
}

func TestWebSocketIdleTimeoutSlidesOnEitherDirectionProgress(t *testing.T) {
	for _, originToClient := range []bool{true, false} {
		name := "Client to Origin"
		if originToClient {
			name = "Origin to Client"
		}
		t.Run(name, func(t *testing.T) {
			testWebSocketOneWayProgressRenewsBothSides(t, originToClient)
		})
	}
}

func testWebSocketOneWayProgressRenewsBothSides(t *testing.T, originToClient bool) {
	t.Helper()
	const idleTimeout = 500 * time.Millisecond
	server, dialer := startWebSocketServerWithIdleTimeout(t, pipeConnectionPair, idleTimeout)
	client, clientReader, origin, originReader, _ := openWebSocket(t, server, dialer)
	defer client.Close()
	defer origin.Close()

	// 只让一个方向持续进展，且总时长超过 idle window；另一侧也必须被共同续期。
	for sequence := byte(0); sequence < 12; sequence++ {
		time.Sleep(50 * time.Millisecond)
		if originToClient {
			frame := unmaskedWebSocketFrame(0x9, []byte{sequence})
			if _, err := origin.Write(frame); err != nil {
				t.Fatalf("write Origin progress %d: %v", sequence, err)
			}
			assertReadBytes(t, client, clientReader, frame, "Client one-way sliding-idle frame")
			continue
		}
		frame := maskedWebSocketFrame(0xa, []byte{sequence})
		if _, err := client.Write(frame); err != nil {
			t.Fatalf("write Client progress %d: %v", sequence, err)
		}
		assertReadBytes(t, origin, originReader, frame, "Origin one-way sliding-idle frame")
	}

	// 跨窗后验证一直静默的反方向仍可立即使用。
	if originToClient {
		frame := maskedWebSocketFrame(0x1, []byte("reverse"))
		if _, err := client.Write(frame); err != nil {
			t.Fatalf("write reverse Client frame: %v", err)
		}
		assertReadBytes(t, origin, originReader, frame, "Origin reverse frame")
	} else {
		frame := unmaskedWebSocketFrame(0x1, []byte("reverse"))
		if _, err := origin.Write(frame); err != nil {
			t.Fatalf("write reverse Origin frame: %v", err)
		}
		assertReadBytes(t, client, clientReader, frame, "Client reverse frame")
	}

	// 双方完全静默后，两个阻塞方向共享的 Deadline 必须主动解除 IO。
	time.Sleep(idleTimeout + 100*time.Millisecond)
	assertConnectionClosed(t, client, clientReader, "Client")
	assertConnectionClosed(t, origin, originReader, "Origin")
}

func TestWebSocketIdleDeadlineCannotMoveBackward(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	controlled := &blockingDeadlineConn{
		Conn: left, firstStarted: make(chan struct{}), releaseFirst: make(chan struct{}),
	}
	owner := newWebSocketIdleOwner(time.Minute)
	owner.mu.Lock()
	owner.client = controlled
	owner.mu.Unlock()

	firstResult := make(chan error, 1)
	go func() { firstResult <- owner.touch() }()
	<-controlled.firstStarted
	time.Sleep(20 * time.Millisecond)
	if err := owner.touch(); err != nil {
		t.Fatalf("newer touch() error = %v", err)
	}
	close(controlled.releaseFirst)
	if err := <-firstResult; err != nil {
		t.Fatalf("older touch() error = %v", err)
	}

	controlled.mu.Lock()
	deadlines := append([]time.Time(nil), controlled.deadlines...)
	controlled.mu.Unlock()
	if len(deadlines) < 2 || !deadlines[len(deadlines)-1].After(deadlines[0]) {
		t.Fatalf("applied deadlines = %v, want final deadline later than blocked first deadline", deadlines)
	}
}

func TestWebSocketLongConnectionSoak(t *testing.T) {
	if os.Getenv("XTUNNEL_RUN_WEBSOCKET_SOAK") != "1" {
		t.Skip("set XTUNNEL_RUN_WEBSOCKET_SOAK=1 to run the >=1h WebSocket soak")
	}
	server, dialer := startWebSocketServer(t, pipeConnectionPair)
	client, clientReader, origin, _, _ := openWebSocket(t, server, dialer)
	defer client.Close()
	defer origin.Close()

	deadline := time.NewTimer(time.Hour)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for sequence := byte(0); ; sequence++ {
		select {
		case <-ticker.C:
			frame := unmaskedWebSocketFrame(0x9, []byte{sequence})
			if _, err := origin.Write(frame); err != nil {
				t.Fatalf("write soak Ping %d: %v", sequence, err)
			}
			assertReadBytes(t, client, clientReader, frame, "Client soak Ping")
		case <-deadline.C:
			return
		}
	}
}

func TestWebSocketHandshakeFailureDoesNotRetryTunnelDial(t *testing.T) {
	server, dialer := startWebSocketServer(t, pipeConnectionPair)
	client, err := net.Dial("tcp", server.Addr().String())
	if err != nil {
		t.Fatalf("Dial HTTP ingress: %v", err)
	}
	defer client.Close()
	writeWebSocketHandshake(t, client)

	var origin net.Conn
	select {
	case origin = <-dialer.origins:
	case <-time.After(2 * time.Second):
		t.Fatal("Tunnel Dial did not expose Origin connection")
	}
	originReader := bufio.NewReader(origin)
	request, err := http.ReadRequest(originReader)
	if err != nil {
		origin.Close()
		t.Fatalf("Origin read WebSocket handshake: %v", err)
	}
	if err := request.Body.Close(); err != nil {
		origin.Close()
		t.Fatalf("close Origin handshake body: %v", err)
	}
	if err := origin.Close(); err != nil {
		t.Fatalf("close Origin before handshake response: %v", err)
	}

	response, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("Client read handshake failure response: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("handshake failure status = %d, want 503", response.StatusCode)
	}
	if calls := dialer.callsSnapshot(); len(calls) != 1 {
		t.Fatalf("Tunnel Dial count = %d, want exactly 1 after handshake bytes were sent", len(calls))
	}
}

func TestWebSocketShutdownWaitsForNaturalClose(t *testing.T) {
	server, dialer := startWebSocketServer(t, pipeConnectionPair)
	client, _, origin, _, _ := openWebSocket(t, server, dialer)
	defer origin.Close()

	shutdownResult := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		shutdownResult <- server.Shutdown(ctx)
	}()
	select {
	case err := <-shutdownResult:
		t.Fatalf("Shutdown returned while WebSocket remained active: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close WebSocket client: %v", err)
	}
	select {
	case err := <-shutdownResult:
		if err != nil {
			t.Fatalf("Shutdown() after natural WebSocket close error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not finish after WebSocket client closed")
	}
}

func TestWebSocketShutdownDeadlineForceClosesHijackedConnection(t *testing.T) {
	server, dialer := startWebSocketServer(t, pipeConnectionPair)
	client, clientReader, origin, originReader, _ := openWebSocket(t, server, dialer)
	defer client.Close()
	defer origin.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := server.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown() error = %v, want context.DeadlineExceeded", err)
	}
	assertConnectionClosed(t, client, clientReader, "Client")
	assertConnectionClosed(t, origin, originReader, "Origin")
}

func TestWebSocketCloseForceClosesActiveHijackedConnection(t *testing.T) {
	server, dialer := startWebSocketServer(t, pipeConnectionPair)
	client, clientReader, origin, originReader, _ := openWebSocket(t, server, dialer)
	defer client.Close()
	defer origin.Close()

	closeResult := make(chan error, 1)
	go func() { closeResult <- server.Close() }()
	select {
	case err := <-closeResult:
		if err != nil {
			t.Fatalf("Server.Close() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Server.Close() did not force-close active WebSocket")
	}
	assertConnectionClosed(t, client, clientReader, "Client")
	assertConnectionClosed(t, origin, originReader, "Origin")
}

func startWebSocketServer(t *testing.T, factory connectionPairFactory) (*Server, *upgradeDialer) {
	return startWebSocketServerWithIdleTimeout(t, factory, webSocketIdleTimeout)
}

func startWebSocketServerWithIdleTimeout(
	t *testing.T,
	factory connectionPairFactory,
	idleTimeout time.Duration,
) (*Server, *upgradeDialer) {
	t.Helper()
	manager, _ := startRouteManager(t, baseHTTPRouteState(1))
	dialer := newUpgradeDialer(factory)
	handler := newTestHandlerWithTrustedProxies(t, manager, dialer, []string{"127.0.0.0/8", "::1/128"})
	handler.webSocketIdleTimeout = idleTimeout
	server, err := NewServer(ServerOptions{
		Listen: "127.0.0.1:0", Handler: handler, MaxHeaderBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Server.Start() error = %v", err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("Server.Close() error = %v", err)
		}
	})
	return server, dialer
}

func openWebSocket(
	t *testing.T,
	server *Server,
	dialer *upgradeDialer,
) (net.Conn, *bufio.Reader, net.Conn, *bufio.Reader, *http.Request) {
	t.Helper()
	client, err := net.Dial("tcp", server.Addr().String())
	if err != nil {
		t.Fatalf("Dial HTTP ingress: %v", err)
	}
	writeWebSocketHandshake(t, client)

	var origin net.Conn
	select {
	case origin = <-dialer.origins:
	case <-time.After(2 * time.Second):
		client.Close()
		t.Fatal("Tunnel Dial did not expose Origin connection")
	}
	originReader := bufio.NewReader(origin)
	originRequest, err := http.ReadRequest(originReader)
	if err != nil {
		client.Close()
		origin.Close()
		t.Fatalf("Origin read WebSocket handshake: %v", err)
	}
	if err := originRequest.Body.Close(); err != nil {
		client.Close()
		origin.Close()
		t.Fatalf("close Origin handshake body: %v", err)
	}
	responseText := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: websocket\r\n" +
		"Sec-WebSocket-Accept: " + testWebSocketAccept + "\r\n\r\n"
	if _, err := io.WriteString(origin, responseText); err != nil {
		client.Close()
		origin.Close()
		t.Fatalf("Origin write WebSocket handshake: %v", err)
	}
	clientReader := bufio.NewReader(client)
	response, err := http.ReadResponse(clientReader, &http.Request{Method: http.MethodGet})
	if err != nil {
		client.Close()
		origin.Close()
		t.Fatalf("Client read WebSocket handshake: %v", err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols ||
		!httpgutsHeaderHasToken(response.Header.Values("Connection"), "upgrade") ||
		!strings.EqualFold(response.Header.Get("Upgrade"), "websocket") ||
		response.Header.Get("Sec-WebSocket-Accept") != testWebSocketAccept {
		client.Close()
		origin.Close()
		t.Fatalf("WebSocket response = status %d upgrade %q accept %q",
			response.StatusCode, response.Header.Get("Upgrade"), response.Header.Get("Sec-WebSocket-Accept"))
	}
	return client, clientReader, origin, originReader, originRequest
}

func writeWebSocketHandshake(t *testing.T, connection net.Conn) {
	t.Helper()
	requestText := "GET /socket HTTP/1.1\r\n" +
		"Host: public.example.com\r\n" +
		"Connection: keep-alive, Upgrade\r\n" +
		"Upgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"X-Forwarded-For: 198.51.100.40\r\n" +
		"X-Forwarded-Proto: https\r\n" +
		"X-Forwarded-Host: public.example.com\r\n" +
		"X-Forwarded-Unknown: must-not-pass\r\n\r\n"
	if _, err := io.WriteString(connection, requestText); err != nil {
		t.Fatalf("write WebSocket handshake: %v", err)
	}
}

func pipeConnectionPair() (net.Conn, net.Conn, error) {
	backend, origin := net.Pipe()
	return backend, origin, nil
}

func loopbackTCPConnectionPair() (net.Conn, net.Conn, error) {
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, nil, err
	}
	type acceptResult struct {
		connection *net.TCPConn
		err        error
	}
	accepted := make(chan acceptResult, 1)
	go func() {
		connection, acceptErr := listener.AcceptTCP()
		accepted <- acceptResult{connection: connection, err: acceptErr}
	}()
	backend, dialErr := net.DialTCP("tcp", nil, listener.Addr().(*net.TCPAddr))
	if dialErr != nil {
		closeErr := listener.Close()
		result := <-accepted
		if result.connection != nil {
			_ = result.connection.Close()
		}
		return nil, nil, errors.Join(dialErr, result.err, closeErr)
	}
	result := <-accepted
	closeErr := listener.Close()
	if dialErr != nil || result.err != nil || closeErr != nil {
		if backend != nil {
			_ = backend.Close()
		}
		if result.connection != nil {
			_ = result.connection.Close()
		}
		return nil, nil, errors.Join(dialErr, result.err, closeErr)
	}
	return backend, result.connection, nil
}

func maskedWebSocketFrame(opcode byte, payload []byte) []byte {
	mask := [4]byte{0x12, 0x34, 0x56, 0x78}
	frame := []byte{0x80 | opcode, 0x80 | byte(len(payload)), mask[0], mask[1], mask[2], mask[3]}
	for index, value := range payload {
		frame = append(frame, value^mask[index%len(mask)])
	}
	return frame
}

func unmaskedWebSocketFrame(opcode byte, payload []byte) []byte {
	return append([]byte{0x80 | opcode, byte(len(payload))}, payload...)
}

func assertReadBytes(t *testing.T, connection net.Conn, reader io.Reader, expected []byte, label string) {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set %s ReadDeadline: %v", label, err)
	}
	actual := make([]byte, len(expected))
	_, readErr := io.ReadFull(reader, actual)
	clearErr := connection.SetReadDeadline(time.Time{})
	if readErr != nil || clearErr != nil {
		t.Fatalf("read %s: %v", label, errors.Join(readErr, clearErr))
	}
	if string(actual) != string(expected) {
		t.Fatalf("%s = %x, want %x", label, actual, expected)
	}
}

func assertConnectionClosed(t *testing.T, connection net.Conn, reader io.Reader, label string) {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		// net.Pipe 在对端已主动关闭后会直接拒绝设置 Deadline，这本身就是连接
		// 已收敛的证据；若仍存活，下面的 Read 必须立刻返回非超时错误。
		if errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
			return
		}
		t.Fatalf("set %s ReadDeadline: %v", label, err)
	}
	var buffer [1]byte
	if _, err := reader.Read(buffer[:]); err == nil {
		t.Fatalf("%s connection remained open after Shutdown deadline", label)
	} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		t.Fatalf("%s connection was not actively closed: %v", label, err)
	}
}

func httpgutsHeaderHasToken(values []string, token string) bool {
	for _, value := range values {
		for _, candidate := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(candidate), token) {
				return true
			}
		}
	}
	return false
}

type blockingDeadlineConn struct {
	net.Conn
	firstStarted chan struct{}
	releaseFirst chan struct{}
	first        sync.Once

	mu        sync.Mutex
	deadlines []time.Time
}

func (connection *blockingDeadlineConn) SetDeadline(deadline time.Time) error {
	connection.first.Do(func() {
		close(connection.firstStarted)
		<-connection.releaseFirst
	})
	connection.mu.Lock()
	connection.deadlines = append(connection.deadlines, deadline)
	connection.mu.Unlock()
	return nil
}
