package tunnel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
	internaltracing "github.com/lifei6671/xtunnel/internal/tracing"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

const testDialClientAddr = "192.0.2.10:54321"

func testHTTPDialRequest(revision uint64, clientAddr string) DialRequest {
	return DialRequest{
		TunnelID: testTunnelID, ServiceID: testServiceID,
		RequiredRevision: revision,
		Ingress:          protocolv1.IngressType_INGRESS_TYPE_HTTP,
		ClientAddr:       clientAddr,
	}
}

func TestProxyDialAcceptsNormalizedHTTPClientIPWithLimits(t *testing.T) {
	fixture := newFailoverFixture(t, testConnectorID)
	defer cleanupDialFixture(t, fixture)
	agent := fixture.registerWork(t, fixture.sessionsByConnector[testConnectorID], nil)
	requests := make(chan *protocolv1.OpenRequest, 1)
	agentResult := make(chan error, 1)
	go func() { agentResult <- runDialEchoConnector(agent, requests) }()

	const normalizedClientIP = "198.51.100.20"
	connection, err := fixture.proxy.Dial(
		context.Background(), testHTTPDialRequest(0, normalizedClientIP),
	)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	if snapshot := fixture.limits.Snapshot(); snapshot.PendingOpens != 0 || snapshot.ActiveTotal != 0 {
		t.Fatalf(
			"HTTP WorkConn limits after Dial = pending:%d active:%d, want request-owned ACTIVE outside Tunnel Proxy",
			snapshot.PendingOpens, snapshot.ActiveTotal,
		)
	}
	request := <-requests
	if request.GetClientAddr() != normalizedClientIP {
		t.Fatalf("OpenRequest client_addr = %q, want %q", request.GetClientAddr(), normalizedClientIP)
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	waitDialAgent(t, agentResult)
	waitForSnapshot(t, fixture.limits, func(snapshot serverlimits.Snapshot) bool {
		return snapshot.ActiveTotal == 0 && snapshot.PendingOpens == 0
	})
}

func TestProxyDialConsumesHTTPSourceOpenTokenBeforeDownstreamSelection(t *testing.T) {
	limits, err := serverlimits.New(serverlimits.Options{
		MaxConnectors: 8, MaxConnectorsPerTunnel: 8,
		MaxWorkConnections: 64, MaxIdleWorkConnections: 64, MaxConnectingWorkConnections: 64,
		MaxPendingOpens: 16, MaxActiveConnections: 64,
		MaxConnectionsPerTunnel: 64, MaxConnectionsPerService: 64, MaxConnectionsPerSourceIP: 64,
		MaxOpenRatePerSourceIP: 1, MaxOpenBurstPerSourceIP: 1,
		MaxHTTPRequestsPerSourceIPPerSecond: 100,
	})
	if err != nil {
		t.Fatalf("limits.New() error = %v", err)
	}
	fixture := newFailoverFixtureWithLimits(t, limits, testConnectorID)
	defer cleanupDialFixture(t, fixture)
	request := testHTTPDialRequest(999, "198.51.100.40")

	connection, err := fixture.proxy.Dial(context.Background(), request)
	if connection != nil {
		_ = connection.Close()
		t.Fatal("first Dial() unexpectedly returned a connection")
	}
	if !errors.Is(err, serverruntime.ErrNoAvailableConnector) {
		t.Fatalf("first Dial() error = %v, want downstream selection failure", err)
	}
	connection, err = fixture.proxy.Dial(context.Background(), request)
	if connection != nil {
		_ = connection.Close()
		t.Fatal("second Dial() unexpectedly returned a connection")
	}
	if !errors.Is(err, serverlimits.ErrOpenRateExceeded) {
		t.Fatalf("second Dial() error = %v, want ErrOpenRateExceeded", err)
	}
	if snapshot := limits.Snapshot(); snapshot.PendingOpens != 0 || snapshot.ActiveTotal != 0 {
		t.Fatalf("rejected HTTP Dial limits = pending:%d active:%d", snapshot.PendingOpens, snapshot.ActiveTotal)
	}
}

func TestProxyDialPinsConnectorServiceRevision(t *testing.T) {
	fixture := newFailoverFixture(t, testConnectorID)
	defer cleanupDialFixture(t, fixture)
	session := fixture.sessionsByConnector[testConnectorID]
	if !fixture.registry.PublishEligibility(session, serverruntime.SessionEligibility{
		ConfigReady: true, HasObserved: true, ObservedRevision: 8,
		Services: map[string]serverruntime.ServiceEligibility{testServiceID: {
			RequiredRevision: 8, Enabled: true, HealthDisabled: true,
		}},
	}) {
		t.Fatal("PublishEligibility() rejected current Session")
	}
	agent := fixture.registerWork(t, session, nil)

	connection, err := fixture.proxy.Dial(
		context.Background(), testHTTPDialRequest(7, testDialClientAddr),
	)
	if connection != nil {
		_ = connection.Close()
		t.Fatal("Dial(revision 7) returned a connection backed by revision 8")
	}
	if !errors.Is(err, serverruntime.ErrNoAvailableConnector) {
		t.Fatalf("Dial(revision 7) error = %v, want ErrNoAvailableConnector", err)
	}
	pool := fixture.sessions.Pools()[session]
	if counts := pool.Snapshot(); counts.Idle != 1 {
		t.Fatalf("pool after rejected stale Dial = %+v, want the IDLE Work untouched", counts)
	}

	agentResult := make(chan error, 1)
	go func() { agentResult <- runDialEchoConnector(agent, nil) }()
	connection, err = fixture.proxy.Dial(
		context.Background(), testHTTPDialRequest(8, testDialClientAddr),
	)
	if err != nil {
		t.Fatalf("Dial(revision 8) error = %v", err)
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	waitDialAgent(t, agentResult)
}

func TestProxyDialPinsZeroConnectorServiceRevision(t *testing.T) {
	fixture := newFailoverFixture(t, testConnectorID)
	defer cleanupDialFixture(t, fixture)
	session := fixture.sessionsByConnector[testConnectorID]
	if !fixture.registry.PublishEligibility(session, serverruntime.SessionEligibility{
		ConfigReady: true, HasObserved: true, ObservedRevision: 1,
		Services: map[string]serverruntime.ServiceEligibility{testServiceID: {
			RequiredRevision: 1, Enabled: true, HealthDisabled: true,
		}},
	}) {
		t.Fatal("PublishEligibility() rejected current Session")
	}
	agent := fixture.registerWork(t, session, nil)

	connection, err := fixture.proxy.Dial(
		context.Background(), testHTTPDialRequest(0, testDialClientAddr),
	)
	if connection != nil {
		_ = connection.Close()
		t.Fatal("Dial(revision 0) returned a connection backed by revision 1")
	}
	if !errors.Is(err, serverruntime.ErrNoAvailableConnector) {
		t.Fatalf("Dial(revision 0) error = %v, want ErrNoAvailableConnector", err)
	}
	pool := fixture.sessions.Pools()[session]
	if counts := pool.Snapshot(); counts.Idle != 1 {
		t.Fatalf("pool after rejected revision 0 Dial = %+v, want the IDLE Work untouched", counts)
	}

	agentResult := make(chan error, 1)
	go func() { agentResult <- runDialEchoConnector(agent, nil) }()
	connection, err = fixture.proxy.Dial(
		context.Background(), testHTTPDialRequest(1, testDialClientAddr),
	)
	if err != nil {
		t.Fatalf("Dial(revision 1) error = %v", err)
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	waitDialAgent(t, agentResult)
}

func TestProxyDialSucceedsAndRequestCancellationDoesNotPoisonConnection(t *testing.T) {
	fixture := newFailoverFixture(t, testConnectorID)
	defer cleanupDialFixture(t, fixture)
	metrics := &recordingTunnelMetrics{}
	fixture.proxy.options.Metrics = metrics
	agent := fixture.registerWork(t, fixture.sessionsByConnector[testConnectorID], nil)
	requests := make(chan *protocolv1.OpenRequest, 1)
	agentResult := make(chan error, 1)
	go func() { agentResult <- runDialEchoConnector(agent, requests) }()

	requestContext, cancelRequest := context.WithCancel(context.Background())
	connection, err := fixture.proxy.Dial(
		requestContext, testHTTPDialRequest(0, testDialClientAddr),
	)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	cancelRequest()

	payload := []byte("second-request-can-reuse-the-same-tunnel-connection")
	if _, err := connection.Write(payload); err != nil {
		t.Fatalf("Write() after request cancellation error = %v", err)
	}
	echoed := make([]byte, len(payload))
	if _, err := io.ReadFull(connection, echoed); err != nil {
		t.Fatalf("ReadFull() after request cancellation error = %v", err)
	}
	if !bytes.Equal(echoed, payload) {
		t.Fatalf("echoed bytes = %q, want %q", echoed, payload)
	}
	request := <-requests
	if request.GetClientAddr() != testDialClientAddr ||
		request.GetIngressType() != protocolv1.IngressType_INGRESS_TYPE_HTTP {
		t.Fatalf("OpenRequest = %#v, want HTTP ingress and original client address", request)
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	waitDialAgent(t, agentResult)
	waitForSnapshot(t, fixture.limits, func(snapshot serverlimits.Snapshot) bool {
		return snapshot.ActiveTotal == 0 && snapshot.WorkTotal == 0
	})
	metricSnapshot := assertSingleOpenMetric(t, metrics, protocolv1.ErrorCode_ERROR_CODE_OK)
	if metricSnapshot.ingressBytes != uint64(len(payload)) || metricSnapshot.egressBytes != uint64(len(payload)) {
		t.Fatalf("HTTP Tunnel traffic metrics = ingress:%d egress:%d, want %d each",
			metricSnapshot.ingressBytes, metricSnapshot.egressBytes, len(payload))
	}
}

func TestProxyDialCreatesServerSpanTopologyAndInjectsWireContext(t *testing.T) {
	fixture := newFailoverFixture(t, testConnectorID)
	defer cleanupDialFixture(t, fixture)
	traceRuntime, recorder := newTunnelTraceRuntime(t)
	fixture.proxy.options.Tracing = traceRuntime
	agent := fixture.registerWork(t, fixture.sessionsByConnector[testConnectorID], nil)
	requests := make(chan *protocolv1.OpenRequest, 1)
	agentResult := make(chan error, 1)
	go func() { agentResult <- runDialEchoConnector(agent, requests) }()

	ingressContext, ingressSpan := traceRuntime.Tracer("xtunnel/server/httpingress").Start(
		context.Background(), "ingress.Accept",
	)
	connection, err := fixture.proxy.Dial(
		ingressContext, testHTTPDialRequest(0, testDialClientAddr),
	)
	if err != nil {
		ingressSpan.End()
		t.Fatalf("Dial() error = %v", err)
	}
	request := <-requests
	if err := connection.Close(); err != nil {
		ingressSpan.End()
		t.Fatalf("Close() error = %v", err)
	}
	waitDialAgent(t, agentResult)
	ingressSpan.End()

	spans := recorder.Ended()
	if len(spans) != 3 {
		t.Fatalf("ended spans = %d, want ingress, tunnel and acquire", len(spans))
	}
	byName := make(map[string]sdktrace.ReadOnlySpan, len(spans))
	for _, span := range spans {
		byName[span.Name()] = span
	}
	ingress := byName["ingress.Accept"]
	tunnelSpan := byName["tunnel.DialContext"]
	acquire := byName["transport.Acquire"]
	if ingress == nil || tunnelSpan == nil || acquire == nil {
		t.Fatalf("span names = %#v, want exact M6-03 topology", byName)
	}
	if tunnelSpan.Parent().SpanID() != ingress.SpanContext().SpanID() ||
		acquire.Parent().SpanID() != tunnelSpan.SpanContext().SpanID() {
		t.Fatalf("span topology ingress=%s tunnel parent=%s acquire parent=%s",
			ingress.SpanContext().SpanID(), tunnelSpan.Parent().SpanID(), acquire.Parent().SpanID())
	}
	carrier := propagation.MapCarrier{
		"traceparent": request.GetTraceparent(),
		"tracestate":  request.GetTracestate(),
	}
	remote := traceRuntime.Extract(context.Background(), carrier)
	remoteContext := trace.SpanContextFromContext(remote)
	if !remoteContext.IsValid() || !remoteContext.IsRemote() {
		t.Fatalf("OpenRequest Trace Context = %v, want valid remote parent", remoteContext)
	}
	if got, want := request.GetTraceId(), acquire.SpanContext().TraceID().String(); got != want {
		t.Fatalf("OpenRequest trace_id = %q, want %q", got, want)
	}
	if remoteContext.TraceID() != acquire.SpanContext().TraceID() ||
		remoteContext.SpanID() != acquire.SpanContext().SpanID() {
		t.Fatalf("OpenRequest traceparent = %q, want acquire span %s/%s",
			request.GetTraceparent(), acquire.SpanContext().TraceID(), acquire.SpanContext().SpanID())
	}
}

func newTunnelTraceRuntime(t *testing.T) (*internaltracing.Runtime, *tracetest.SpanRecorder) {
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

func TestProxyDialCancellationDuringOpenClosesWorkAndReleasesLimits(t *testing.T) {
	fixture := newFailoverFixture(t, testConnectorID)
	defer cleanupDialFixture(t, fixture)
	metrics := &recordingTunnelMetrics{}
	fixture.proxy.options.Metrics = metrics
	agent := fixture.registerWork(t, fixture.sessionsByConnector[testConnectorID], nil)
	requestRead := make(chan struct{})
	agentResult := make(chan error, 1)
	go func() {
		request := &protocolv1.OpenRequest{}
		if err := frame.ReadWork(agent, request); err != nil {
			agentResult <- err
			return
		}
		close(requestRead)
		buffer := make([]byte, 1)
		_, err := agent.Read(buffer)
		agentResult <- err
	}()

	dialContext, cancelDial := context.WithCancel(context.Background())
	dialResult := make(chan error, 1)
	go func() {
		connection, err := fixture.proxy.Dial(
			dialContext, testHTTPDialRequest(0, testDialClientAddr),
		)
		if connection != nil {
			_ = connection.Close()
		}
		dialResult <- err
	}()
	select {
	case <-requestRead:
	case <-time.After(testTimeout):
		t.Fatal("Dial() did not enter OPEN")
	}
	cancelDial()
	select {
	case err := <-dialResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Dial() error = %v, want context.Canceled", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Dial() did not unblock after cancellation")
	}
	waitDialAgent(t, agentResult)
	waitForSnapshot(t, fixture.limits, func(snapshot serverlimits.Snapshot) bool {
		return snapshot.PendingOpens == 0 && snapshot.ActiveTotal == 0 && snapshot.WorkTotal == 0
	})
	assertSingleOpenMetric(t, metrics, protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR)
}

func TestProxyDialCloseFinishesResourcesExactlyOnce(t *testing.T) {
	fixture := newFailoverFixture(t, testConnectorID)
	defer cleanupDialFixture(t, fixture)
	closeCount := &atomic.Int32{}
	agent := fixture.registerWork(t, fixture.sessionsByConnector[testConnectorID], func(connection net.Conn) net.Conn {
		return &countCloseConnection{Conn: connection, count: closeCount}
	})
	agentResult := make(chan error, 1)
	go func() { agentResult <- runDialEchoConnector(agent, nil) }()

	connection, err := fixture.proxy.Dial(
		context.Background(), testHTTPDialRequest(0, testDialClientAddr),
	)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	var wait sync.WaitGroup
	errorsCh := make(chan error, 32)
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsCh <- connection.Close()
		}()
	}
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Errorf("concurrent Close() error = %v", err)
		}
	}
	waitDialAgent(t, agentResult)
	if got := closeCount.Load(); got != 1 {
		t.Fatalf("WorkConn Close count = %d, want 1", got)
	}
	waitForSnapshot(t, fixture.limits, func(snapshot serverlimits.Snapshot) bool {
		return snapshot.ActiveTotal == 0 && snapshot.WorkTotal == 0
	})
}

func TestProxyDialRegistryRevokeClosesConnectionAndWork(t *testing.T) {
	fixture := newFailoverFixture(t, testConnectorID)
	defer cleanupDialFixture(t, fixture)
	closeCount := &atomic.Int32{}
	agent := fixture.registerWork(t, fixture.sessionsByConnector[testConnectorID], func(connection net.Conn) net.Conn {
		return &countCloseConnection{Conn: connection, count: closeCount}
	})
	agentResult := make(chan error, 1)
	go func() { agentResult <- runDialEchoConnector(agent, nil) }()

	connection, err := fixture.proxy.Dial(
		context.Background(), testHTTPDialRequest(0, testDialClientAddr),
	)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	if err := fixture.registry.RevokeTunnel(testTunnelID); err != nil {
		t.Fatalf("RevokeTunnel() error = %v", err)
	}
	waitDialAgent(t, agentResult)
	if _, err := connection.Write([]byte("closed")); err == nil {
		t.Fatal("Write() after RevokeTunnel() succeeded")
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("Close() after RevokeTunnel() error = %v", err)
	}
	if got := closeCount.Load(); got != 1 {
		t.Fatalf("WorkConn Close count = %d, want 1", got)
	}
	waitForSnapshot(t, fixture.limits, func(snapshot serverlimits.Snapshot) bool {
		return snapshot.ActiveTotal == 0 && snapshot.WorkTotal == 0
	})
}

func TestProxyDialRegistryDrainClosesConnectionAndWork(t *testing.T) {
	fixture := newFailoverFixture(t, testConnectorID)
	defer cleanupDialFixture(t, fixture)
	closeCount := &atomic.Int32{}
	agent := fixture.registerWork(t, fixture.sessionsByConnector[testConnectorID], func(connection net.Conn) net.Conn {
		return &countCloseConnection{Conn: connection, count: closeCount}
	})
	agentResult := make(chan error, 1)
	go func() { agentResult <- runDialEchoConnector(agent, nil) }()

	connection, err := fixture.proxy.Dial(
		context.Background(), testHTTPDialRequest(0, testDialClientAddr),
	)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	drainContext, cancelDrain := context.WithCancel(context.Background())
	cancelDrain()
	if err := fixture.registry.DrainActive(drainContext); err != nil {
		t.Fatalf("DrainActive() error = %v", err)
	}
	waitDialAgent(t, agentResult)
	if _, err := connection.Write([]byte("closed")); err == nil {
		t.Fatal("Write() after DrainActive() succeeded")
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("Close() after DrainActive() error = %v", err)
	}
	if got := closeCount.Load(); got != 1 {
		t.Fatalf("WorkConn Close count = %d, want 1", got)
	}
	waitForSnapshot(t, fixture.limits, func(snapshot serverlimits.Snapshot) bool {
		return snapshot.ActiveTotal == 0 && snapshot.WorkTotal == 0
	})
}

type countCloseConnection struct {
	net.Conn
	count *atomic.Int32
}

func (connection *countCloseConnection) Close() error {
	connection.count.Add(1)
	return connection.Conn.Close()
}

func runDialEchoConnector(
	connection net.Conn,
	requests chan<- *protocolv1.OpenRequest,
) error {
	defer connection.Close()
	request := &protocolv1.OpenRequest{}
	if err := frame.ReadWork(connection, request); err != nil {
		return err
	}
	if requests != nil {
		requests <- request
	}
	response := &protocolv1.OpenResponse{
		ConnectionId: request.GetConnectionId(), Status: protocolv1.OpenStatus_OPEN_STATUS_OK,
		ErrorCode: protocolv1.ErrorCode_ERROR_CODE_OK,
	}
	if err := frame.WriteWork(connection, response); err != nil {
		return err
	}
	buffer := make([]byte, 1024)
	for {
		read, err := connection.Read(buffer)
		if read > 0 {
			if _, writeErr := connection.Write(buffer[:read]); writeErr != nil {
				return writeErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
	}
}

func waitDialAgent(t *testing.T, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			t.Fatalf("dial Connector error = %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("dial Connector did not finish")
	}
}

func cleanupDialFixture(t *testing.T, fixture *failoverFixture) {
	t.Helper()
	_ = fixture.registry.RevokeTunnel(testTunnelID)
	fixture.close(t)
}
