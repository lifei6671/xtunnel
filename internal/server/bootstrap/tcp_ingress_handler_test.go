package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	streamproxy "github.com/lifei6671/xtunnel/internal/proxy"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
	serveropen "github.com/lifei6671/xtunnel/internal/server/open"
	serverroute "github.com/lifei6671/xtunnel/internal/server/route"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
	serverworkpool "github.com/lifei6671/xtunnel/internal/server/workpool"
	internaltracing "github.com/lifei6671/xtunnel/internal/tracing"
	"github.com/lifei6671/xtunnel/internal/tunnel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

const (
	tcpHandlerTunnelID  = "tun_01J00000000000000000000070"
	tcpHandlerServiceID = "svc_01J00000000000000000000070"
)

type tcpDialCall struct {
	request tunnel.DialRequest
	peer    net.Conn
	ctx     context.Context
}

type recordingTCPProxy struct {
	mu         sync.Mutex
	connection net.Conn
	err        error
	observed   bool
	calls      chan tcpDialCall
}

func (dialer *recordingTCPProxy) Serve(
	ctx context.Context,
	request tunnel.DialRequest,
	peer net.Conn,
) error {
	dialer.calls <- tcpDialCall{request: request, peer: peer, ctx: ctx}
	dialer.mu.Lock()
	connection, err := dialer.connection, dialer.err
	dialer.mu.Unlock()
	if err != nil {
		return err
	}
	if connection == nil {
		return errInvalidTCPIngressConnection
	}
	return streamproxy.ProxyBidirectional(ctx, peer, connection)
}

func (dialer *recordingTCPProxy) ServiceConfigObserved(string, string, int64) bool {
	return dialer.observed
}

func TestServeTCPRoutePreservesSSHBytesAndExactRevision(t *testing.T) {
	publicPeer, publicClient := tcpHandlerConnectionPair(t)
	tunnelConnection, tunnelPeer := tcpHandlerConnectionPair(t)
	dialer := &recordingTCPProxy{
		connection: tunnelConnection,
		observed:   true,
		calls:      make(chan tcpDialCall, 1),
	}
	route := serverroute.TCPRoute{
		ID: "tcp-ssh", TunnelID: tcpHandlerTunnelID, ServiceID: tcpHandlerServiceID,
		PublicPort: 22022, RequiredRevision: 17,
	}
	result := make(chan error, 1)
	managerContext, cancelManager := context.WithCancel(context.Background())
	defer cancelManager()
	go func() {
		result <- serveTCPRoute(managerContext, dialer, publicPeer, route)
	}()

	call := receiveTCPDialCall(t, dialer.calls)
	if call.request.TunnelID != route.TunnelID || call.request.ServiceID != route.ServiceID ||
		call.request.RequiredRevision != uint64(route.RequiredRevision) ||
		call.request.Ingress != protocolv1.IngressType_INGRESS_TYPE_TCP ||
		call.request.ClientAddr != "" || call.peer != publicPeer || call.ctx != managerContext {
		t.Fatalf("Serve call = %+v, want exact captured TCP Route, original context and peer", call)
	}

	request := []byte("SSH-2.0-OpenSSH_9.9\r\n\x00\xff\x16\x03\x01raw-tcp")
	response := []byte("SSH-2.0-XTunnel_Test\r\n\x00\x01response")
	setTCPHandlerDeadline(t, publicClient, tunnelPeer)
	if _, err := publicClient.Write(request); err != nil {
		t.Fatalf("public Write() error = %v", err)
	}
	if err := publicClient.CloseWrite(); err != nil {
		t.Fatalf("public CloseWrite() error = %v", err)
	}
	actualRequest, err := io.ReadAll(tunnelPeer)
	if err != nil {
		t.Fatalf("tunnel ReadAll() error = %v", err)
	}
	if !bytes.Equal(actualRequest, request) {
		t.Fatalf("tunnel request = %q, want byte-exact %q", actualRequest, request)
	}
	if bytes.HasPrefix(actualRequest, []byte("PROXY ")) {
		t.Fatalf("tunnel request unexpectedly contains a PROXY header: %q", actualRequest)
	}
	if _, err := tunnelPeer.Write(response); err != nil {
		t.Fatalf("tunnel Write() after public half-close error = %v", err)
	}
	if err := tunnelPeer.CloseWrite(); err != nil {
		t.Fatalf("tunnel CloseWrite() error = %v", err)
	}
	actualResponse, err := io.ReadAll(publicClient)
	if err != nil {
		t.Fatalf("public ReadAll() error = %v", err)
	}
	if !bytes.Equal(actualResponse, response) {
		t.Fatalf("public response = %q, want byte-exact %q", actualResponse, response)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("serveTCPRoute() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serveTCPRoute() did not finish after bidirectional half-close")
	}
}

func TestTCPIngressHandlerLogsOnlyStableFailureCode(t *testing.T) {
	publicPeer, _ := tcpHandlerConnectionPair(t)
	secret := "origin-secret.internal:5432 token-secret"
	dialer := &recordingTCPProxy{
		err: fmt.Errorf("%s: %w", secret, &serveropen.Rejected{
			Code: protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TIMEOUT,
		}),
		observed: true,
		calls:    make(chan tcpDialCall, 1),
	}
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	traceRuntime, recorder := newTCPIngressTraceRuntime(t)
	handler, err := newTCPIngressHandler(dialer, logger, traceRuntime)
	if err != nil {
		t.Fatalf("newTCPIngressHandler() error = %v", err)
	}
	route := serverroute.TCPRoute{
		ID: "tcp-failure", TunnelID: tcpHandlerTunnelID, ServiceID: tcpHandlerServiceID,
		PublicPort: 22023, RequiredRevision: 18,
	}
	handler(context.Background(), publicPeer, route)
	call := receiveTCPDialCall(t, dialer.calls)
	logLine := output.String()
	spans := recorder.Ended()
	if len(spans) != 1 || spans[0].Name() != "ingress.Accept" || spans[0].Parent().IsValid() {
		t.Fatalf("TCP ingress spans = %#v, want one local root ingress.Accept", spans)
	}
	traceID := spans[0].SpanContext().TraceID()
	if got := trace.SpanContextFromContext(call.ctx).TraceID(); got != traceID {
		t.Fatalf("Tunnel context TraceID = %s, want %s", got, traceID)
	}
	for _, want := range []string{
		`"msg":"tcp_ingress_connection_failed"`,
		`"error_code":"ORIGIN_TIMEOUT"`,
		`"tunnel_id":"` + tcpHandlerTunnelID + `"`,
		`"service_id":"` + tcpHandlerServiceID + `"`,
		`"public_port":22023`,
		`"trace_id":"` + traceID.String() + `"`,
	} {
		if !strings.Contains(logLine, want) {
			t.Errorf("log = %q, want %q", logLine, want)
		}
	}
	if strings.Contains(logLine, secret) || strings.Contains(logLine, "origin-secret.internal") {
		t.Fatalf("log leaked Origin or underlying error text: %q", logLine)
	}
}

func newTCPIngressTraceRuntime(t *testing.T) (*internaltracing.Runtime, *tracetest.SpanRecorder) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	runtime, err := internaltracing.New(t.Context(), internaltracing.Config{
		ServiceName: "xtunnel-server", ServiceVersion: "test",
		TracerProvider: provider, ProviderShutdown: provider.Shutdown,
	})
	if err != nil {
		t.Fatalf("tracing.New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Shutdown(context.Background()); err != nil {
			t.Errorf("tracing Runtime Shutdown() error = %v", err)
		}
	})
	return runtime, recorder
}

func TestTCPIngressErrorCode(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		observed bool
		want     string
	}{
		{name: "Agent origin refused", err: &serveropen.Rejected{Code: protocolv1.ErrorCode_ERROR_CODE_ORIGIN_REFUSED}, observed: true, want: "ORIGIN_REFUSED"},
		{name: "Agent service disabled", err: &serveropen.Rejected{Code: protocolv1.ErrorCode_ERROR_CODE_SERVICE_DISABLED}, observed: true, want: "SERVICE_DISABLED"},
		{name: "Local pending capacity", err: serverlimits.ErrPendingOpenCapacity, observed: true, want: "WORK_POOL_EXHAUSTED"},
		{name: "Local acquire timeout", err: serverworkpool.ErrAcquireTimeout, observed: true, want: "WORK_POOL_EXHAUSTED"},
		{name: "Revision not observed", err: serverruntime.ErrNoAvailableConnector, want: "SERVICE_CONFIG_NOT_OBSERVED"},
		{name: "Observed tunnel offline", err: serverruntime.ErrNoAvailableConnector, observed: true, want: "TUNNEL_OFFLINE"},
		{name: "OPEN protocol violation", err: serveropen.ErrProtocol, want: "PROTOCOL_ERROR"},
		{name: "Unknown internal", err: errors.New("private origin failure"), observed: true, want: "INTERNAL_ERROR"},
		{name: "Unknown Agent code", err: &serveropen.Rejected{}, observed: true, want: "INTERNAL_ERROR"},
	}
	route := serverroute.TCPRoute{
		TunnelID: tcpHandlerTunnelID, ServiceID: tcpHandlerServiceID, RequiredRevision: 19,
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dialer := &recordingTCPProxy{observed: test.observed}
			if actual := tcpIngressErrorCode(dialer, route, test.err); actual != test.want {
				t.Fatalf("tcpIngressErrorCode() = %q, want %q", actual, test.want)
			}
		})
	}
}

func TestServeTCPRouteCancellationUnblocksBothConnections(t *testing.T) {
	publicPeer, publicClient := tcpHandlerConnectionPair(t)
	tunnelConnection, tunnelPeer := tcpHandlerConnectionPair(t)
	dialer := &recordingTCPProxy{
		connection: tunnelConnection,
		observed:   true,
		calls:      make(chan tcpDialCall, 1),
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- serveTCPRoute(ctx, dialer, publicPeer, serverroute.TCPRoute{
			TunnelID: tcpHandlerTunnelID, ServiceID: tcpHandlerServiceID,
			PublicPort: 22024, RequiredRevision: 0,
		})
	}()
	call := receiveTCPDialCall(t, dialer.calls)
	if call.request.RequiredRevision != 0 {
		t.Fatalf("Serve revision = %d, want legal revision 0", call.request.RequiredRevision)
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("serveTCPRoute() error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serveTCPRoute() did not unblock after cancellation")
	}
	assertTCPHandlerConnectionClosed(t, publicClient)
	assertTCPHandlerConnectionClosed(t, tunnelPeer)
}

func TestServeTCPRouteRejectsInvalidBoundary(t *testing.T) {
	peer, _ := tcpHandlerConnectionPair(t)
	dialer := &recordingTCPProxy{calls: make(chan tcpDialCall, 1)}
	tests := []struct {
		name   string
		ctx    context.Context
		dialer tcpTunnelProxy
		peer   net.Conn
		route  serverroute.TCPRoute
	}{
		{name: "nil context", dialer: dialer, peer: peer},
		{name: "nil dialer", ctx: context.Background(), peer: peer},
		{name: "nil peer", ctx: context.Background(), dialer: dialer},
		{name: "negative revision", ctx: context.Background(), dialer: dialer, peer: peer, route: serverroute.TCPRoute{RequiredRevision: -1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := serveTCPRoute(test.ctx, test.dialer, test.peer, test.route); !errors.Is(err, errInvalidTCPIngressConnection) {
				t.Fatalf("serveTCPRoute() error = %v, want errInvalidTCPIngressConnection", err)
			}
		})
	}

	nilBackend := &recordingTCPProxy{observed: true, calls: make(chan tcpDialCall, 1)}
	err := serveTCPRoute(context.Background(), nilBackend, peer, serverroute.TCPRoute{RequiredRevision: 0})
	if !errors.Is(err, errInvalidTCPIngressConnection) {
		t.Fatalf("serveTCPRoute(nil backend) error = %v, want errInvalidTCPIngressConnection", err)
	}
	receiveTCPDialCall(t, nilBackend.calls)

	if _, err := newTCPIngressHandler(nil, slog.Default(), nil); !errors.Is(err, errInvalidTCPIngressConnection) {
		t.Fatalf("newTCPIngressHandler(nil dialer) error = %v, want errInvalidTCPIngressConnection", err)
	}
	if _, err := newTCPIngressHandler(dialer, nil, nil); !errors.Is(err, errInvalidTCPIngressConnection) {
		t.Fatalf("newTCPIngressHandler(nil logger) error = %v, want errInvalidTCPIngressConnection", err)
	}
}

func tcpHandlerConnectionPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenTCP() error = %v", err)
	}
	accepted := make(chan *net.TCPConn, 1)
	acceptErrors := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.AcceptTCP()
		if acceptErr != nil {
			acceptErrors <- acceptErr
			return
		}
		accepted <- connection
	}()
	client, err := net.DialTCP("tcp4", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		_ = listener.Close()
		t.Fatalf("DialTCP() error = %v", err)
	}
	var server *net.TCPConn
	select {
	case server = <-accepted:
	case acceptErr := <-acceptErrors:
		_ = client.Close()
		_ = listener.Close()
		t.Fatalf("AcceptTCP() error = %v", acceptErr)
	case <-time.After(2 * time.Second):
		_ = client.Close()
		_ = listener.Close()
		t.Fatal("AcceptTCP() timed out")
	}
	if err := listener.Close(); err != nil {
		_ = server.Close()
		_ = client.Close()
		t.Fatalf("listener Close() error = %v", err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("server Close() error = %v", err)
		}
		if err := client.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("client Close() error = %v", err)
		}
	})
	return server, client
}

func receiveTCPDialCall(t *testing.T, calls <-chan tcpDialCall) tcpDialCall {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(2 * time.Second):
		t.Fatal("Dial was not called")
		return tcpDialCall{}
	}
}

func setTCPHandlerDeadline(t *testing.T, connections ...net.Conn) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for _, connection := range connections {
		if err := connection.SetDeadline(deadline); err != nil {
			t.Fatalf("SetDeadline() error = %v", err)
		}
	}
}

func assertTCPHandlerConnectionClosed(t *testing.T, connection net.Conn) {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("SetReadDeadline() error = %v", err)
	}
	buffer := make([]byte, 1)
	_, err := connection.Read(buffer)
	if err == nil {
		t.Fatal("Read() unexpectedly succeeded after proxy cancellation")
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		t.Fatalf("Read() timed out instead of observing a closed connection: %v", err)
	}
}
