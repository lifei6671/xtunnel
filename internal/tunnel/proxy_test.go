package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/logging"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/state"
	servercontrolauth "github.com/lifei6671/xtunnel/internal/server/controlauth"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
	serveropen "github.com/lifei6671/xtunnel/internal/server/open"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
	"github.com/lifei6671/xtunnel/internal/server/sessionruntime"
	serversnapshot "github.com/lifei6671/xtunnel/internal/server/snapshot"
	serverworkauth "github.com/lifei6671/xtunnel/internal/server/workauth"
	serverworkpool "github.com/lifei6671/xtunnel/internal/server/workpool"
)

const (
	testTunnelID     = "tun_01J00000000000000000000000"
	testConnectorID  = "con_01J00000000000000000000000"
	testConnectorTwo = "con_01J00000000000000000000001"
	testServiceID    = "svc_01J00000000000000000000000"
	testServiceTwo   = "svc_01J00000000000000000000001"
	testWorkID       = "work_01J00000000000000000000000"
	testTimeout      = 3 * time.Second
)

func testTCPDialRequest() DialRequest {
	return DialRequest{
		TunnelID: testTunnelID, ServiceID: testServiceID, RequiredRevision: 0,
		Ingress: protocolv1.IngressType_INGRESS_TYPE_TCP,
	}
}

type tunnelSnapshotProvider struct{}

func (tunnelSnapshotProvider) Current(_ context.Context, tunnelID string) (serversnapshot.Result, error) {
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

type healthTunnelSnapshotProvider struct{}

func (healthTunnelSnapshotProvider) Current(_ context.Context, tunnelID string) (serversnapshot.Result, error) {
	return serversnapshot.Result{Snapshot: &protocolv1.TunnelSnapshot{
		TunnelId: tunnelID,
		Services: []*protocolv1.ServiceConfig{{
			ServiceId: testServiceID,
			Enabled:   true,
			Health: &protocolv1.HealthCheckConfig{
				Type: protocolv1.HealthType_HEALTH_TYPE_TCP, IntervalMs: 1_000,
				TimeoutMs: 100, FailureThreshold: 2, SuccessThreshold: 2,
			},
		}},
	}}, nil
}

func TestProxyServesTCPEchoThroughSelectedConnector(t *testing.T) {
	registry := serverruntime.NewRegistry()
	pending, err := registry.ReserveAuthenticated(testTunnelID, testConnectorID)
	if err != nil {
		t.Fatalf("ReserveAuthenticated() error = %v", err)
	}
	session, err := registry.CommitAuthenticated(pending)
	if err != nil {
		t.Fatalf("CommitAuthenticated() error = %v", err)
	}
	sessions, err := sessionruntime.New(registry, sessionruntime.Options{
		HighPriorityCapacity: 8, NormalCapacity: 16, InboundCapacity: 8,
		WriteTimeout: time.Second, MaxReplayEntries: 64,
		MaxWorkTotal: 64, MaxWorkConnecting: 16, SnapshotProvider: tunnelSnapshotProvider{},
	})
	if err != nil {
		t.Fatalf("sessionruntime.New() error = %v", err)
	}
	startSessionManager(t, sessions)
	controlServer, controlAgent := net.Pipe()
	defer controlAgent.Close()
	established := establishedControl(t, session)
	controlResult := make(chan error, 1)
	go func() { controlResult <- sessions.Serve(context.Background(), controlServer, &established) }()
	readDemand(t, controlAgent)

	serverWork, agentWork := tcpPair(t)
	idle := authenticatedIdle(t, session)
	registered, err := sessions.RegisterIdle(serverWork, idle)
	if err != nil {
		t.Fatalf("RegisterIdle() error = %v", err)
	}
	if registered.State().String() != "IDLE" {
		t.Fatalf("registered Work state = %s", registered.State())
	}
	agentResult := make(chan error, 1)
	go func() { agentResult <- runEchoConnector(agentWork) }()

	openHandler, err := serveropen.NewHandler(serveropen.Options{HandshakeTimeout: time.Second, WriteTimeout: time.Second, ReadTimeout: time.Second})
	if err != nil {
		t.Fatalf("open.NewHandler() error = %v", err)
	}
	var logOutput bytes.Buffer
	tunnelProxy, err := NewProxy(Options{
		Registry: registry, Sessions: sessions, OpenHandler: openHandler, AcquireTimeout: time.Second,
		Logger: testTunnelLoggerTo(&logOutput),
	})
	if err != nil {
		t.Fatalf("NewProxy() error = %v", err)
	}
	serverPeer, publicClient := tcpPair(t)
	defer publicClient.Close()
	proxyResult := make(chan error, 1)
	request := testTCPDialRequest()
	request.RequestID = "req_01J00000000000000000000000"
	request.TraceID = "trace-01J00000000000000000000000"
	go func() {
		proxyResult <- tunnelProxy.Serve(context.Background(), request, serverPeer)
	}()

	payload := bytes.Repeat([]byte("xtunnel-echo-"), 1024)
	if _, err := publicClient.Write(payload); err != nil {
		t.Fatalf("write public payload: %v", err)
	}
	if err := publicClient.CloseWrite(); err != nil {
		t.Fatalf("public CloseWrite: %v", err)
	}
	echoed, err := io.ReadAll(publicClient)
	if err != nil {
		t.Fatalf("read echoed payload: %v", err)
	}
	if !bytes.Equal(echoed, payload) {
		t.Fatalf("echoed bytes changed: got=%d want=%d", len(echoed), len(payload))
	}
	select {
	case err := <-proxyResult:
		if err != nil {
			t.Fatalf("Proxy.Serve() error = %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Proxy.Serve() did not finish after half-close echo")
	}
	select {
	case err := <-agentResult:
		if err != nil {
			t.Fatalf("echo Connector error = %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("echo Connector did not finish")
	}
	assertTunnelLifecycleLogs(t, logOutput.String(), request, session)

	_ = controlAgent.Close()
	select {
	case <-controlResult:
	case <-time.After(testTimeout):
		t.Fatal("Control Session did not finish")
	}
}

func assertTunnelLifecycleLogs(t *testing.T, output string, request DialRequest, session serverruntime.Session) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 2 {
		t.Fatalf("Tunnel lifecycle log count = %d, want 2; output=%q", len(lines), output)
	}
	wantEvents := []string{logging.EventTunnelConnectionOpened, logging.EventTunnelConnectionClosed}
	for index, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode Tunnel log %q: %v", line, err)
		}
		if record[logging.EventKey] != wantEvents[index] ||
			record[logging.RequestIDKey] != request.RequestID ||
			record[logging.TraceIDKey] != request.TraceID ||
			record[logging.TunnelIDKey] != request.TunnelID ||
			record[logging.ServiceIDKey] != request.ServiceID ||
			record[logging.ConnectorIDKey] != session.ConnectorID ||
			record[logging.SessionIDKey] != session.SessionID ||
			record[logging.GenerationKey] != float64(session.Generation) {
			t.Fatalf("Tunnel lifecycle record %d = %#v", index, record)
		}
		connectionID, _ := record[logging.ConnectionIDKey].(string)
		if !strings.HasPrefix(connectionID, "conn_") {
			t.Fatalf("Tunnel lifecycle connection_id = %q", connectionID)
		}
		if _, exists := record[logging.ErrorCodeKey]; exists {
			t.Fatalf("successful Tunnel lifecycle contains error_code: %#v", record)
		}
	}
}

func TestProxyServeDoesNotConsumeSecondTCPSourceOpenToken(t *testing.T) {
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
	defer fixture.close(t)
	agent := fixture.registerWork(t, fixture.sessionsByConnector[testConnectorID], nil)
	agentTCP, ok := agent.(*net.TCPConn)
	if !ok {
		t.Fatalf("registered agent WorkConn = %T, want *net.TCPConn", agent)
	}
	agentResult := make(chan error, 1)
	go func() { agentResult <- runEchoConnector(agentTCP) }()

	serverPeer, publicClient := tcpPair(t)
	defer publicClient.Close()
	sourceIP, err := sourceAddress(serverPeer.RemoteAddr())
	if err != nil {
		t.Fatalf("sourceAddress() error = %v", err)
	}
	// 模拟真实 TCP Listener 已完成的一次 Accept 计数。Proxy.Serve 必须直接进入
	// Pending/ACTIVE 生命周期；若重复消费，本次唯一 burst 会在这里把业务连接拒绝。
	if err := limits.AllowOpen(sourceIP); err != nil {
		t.Fatalf("listener AllowOpen() error = %v", err)
	}
	proxyResult := make(chan error, 1)
	go func() { proxyResult <- fixture.proxy.Serve(context.Background(), testTCPDialRequest(), serverPeer) }()
	payload := []byte("single-tcp-open-token")
	if _, err := publicClient.Write(payload); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := publicClient.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite() error = %v", err)
	}
	if echoed, err := io.ReadAll(publicClient); err != nil || !bytes.Equal(echoed, payload) {
		t.Fatalf("echo = %q, %v, want %q", echoed, err, payload)
	}
	select {
	case err := <-proxyResult:
		if err != nil {
			t.Fatalf("Proxy.Serve() error = %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Proxy.Serve() did not finish")
	}
	waitDialAgent(t, agentResult)
	waitForSnapshot(t, limits, func(snapshot serverlimits.Snapshot) bool {
		return snapshot.PendingOpens == 0 && snapshot.ActiveTotal == 0
	})
}

func TestProxyServeRevokeClosesPublicPeerAndReturnsLifecycleCancellation(t *testing.T) {
	fixture := newFailoverFixture(t, testConnectorID)
	defer fixture.close(t)

	closeCount := &atomic.Int32{}
	agent := fixture.registerWork(t, fixture.sessionsByConnector[testConnectorID], func(connection net.Conn) net.Conn {
		tcpConnection, ok := connection.(*net.TCPConn)
		if !ok {
			t.Fatalf("registered WorkConn type = %T, want *net.TCPConn", connection)
		}
		return &eofCloseTCPConnection{TCPConn: tcpConnection, closeCount: closeCount}
	})
	openCompleted := make(chan struct{})
	agentResult := make(chan error, 1)
	go func() {
		defer agent.Close()
		request := &protocolv1.OpenRequest{}
		if err := frame.ReadWork(agent, request); err != nil {
			agentResult <- err
			return
		}
		if err := frame.WriteWork(agent, &protocolv1.OpenResponse{
			ConnectionId: request.GetConnectionId(), Status: protocolv1.OpenStatus_OPEN_STATUS_OK,
			ErrorCode: protocolv1.ErrorCode_ERROR_CODE_OK,
		}); err != nil {
			agentResult <- err
			return
		}
		close(openCompleted)
		buffer := make([]byte, 1)
		_, err := agent.Read(buffer)
		agentResult <- err
	}()

	serverPeer, publicClient := tcpPair(t)
	defer publicClient.Close()
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- fixture.proxy.Serve(context.Background(), testTCPDialRequest(), serverPeer)
	}()
	select {
	case <-openCompleted:
	case <-time.After(testTimeout):
		t.Fatal("Proxy.Serve() did not complete OPEN")
	}
	waitForSnapshot(t, fixture.limits, func(snapshot serverlimits.Snapshot) bool {
		return snapshot.ActiveTotal == 1 && snapshot.PendingOpens == 0
	})

	revokeErr := fixture.registry.RevokeTunnel(testTunnelID)
	if !errors.Is(revokeErr, io.EOF) {
		t.Fatalf("RevokeTunnel() error = %v, want wrapped io.EOF from WorkConn close", revokeErr)
	}
	select {
	case err := <-serveResult:
		if !errors.Is(err, context.Canceled) || !errors.Is(err, io.EOF) {
			t.Fatalf("Proxy.Serve() error = %v, want context.Canceled and io.EOF", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Proxy.Serve() did not return after Tunnel Revoke")
	}
	select {
	case <-agentResult:
	case <-time.After(testTimeout):
		t.Fatal("Agent WorkConn did not unblock after Tunnel Revoke")
	}
	if err := publicClient.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set public peer read deadline: %v", err)
	}
	if _, err := publicClient.Read(make([]byte, 1)); err == nil {
		t.Fatal("public peer remained readable after Tunnel Revoke")
	}
	if got := closeCount.Load(); got != 1 {
		t.Fatalf("WorkConn Close count = %d, want 1", got)
	}
	waitForSnapshot(t, fixture.limits, func(snapshot serverlimits.Snapshot) bool {
		return snapshot.PendingOpens == 0 && snapshot.ActiveTotal == 0 && snapshot.WorkTotal == 0
	})
}

func TestProxyServePreOpenTimeoutDoesNotBoundRawLifecycle(t *testing.T) {
	fixture := newFailoverFixture(t, testConnectorID)
	defer fixture.close(t)
	fixture.proxy.publicTCPPreOpenTimeout = 100 * time.Millisecond

	requests := make(chan *protocolv1.OpenRequest, 1)
	payloads := make(chan []byte, 1)
	agent := fixture.registerWork(t, fixture.sessionsByConnector[testConnectorID], nil)
	agentResult := make(chan error, 1)
	go func() { agentResult <- runRecordingEchoConnector(agent, requests, payloads) }()

	serverPeer, publicClient := tcpPair(t)
	defer publicClient.Close()
	serveResult := make(chan error, 1)
	request := testTCPDialRequest()
	request.ClientAddr = "forged.example:65535"
	go func() { serveResult <- fixture.proxy.Serve(context.Background(), request, serverPeer) }()

	openRequest := <-requests
	if openRequest.GetClientAddr() != serverPeer.RemoteAddr().String() {
		t.Fatalf("OpenRequest client_addr = %q, want accepted peer %q", openRequest.GetClientAddr(), serverPeer.RemoteAddr())
	}
	waitForSnapshot(t, fixture.limits, func(snapshot serverlimits.Snapshot) bool {
		return snapshot.ActiveTotal == 1 && snapshot.PendingOpens == 0
	})
	time.Sleep(150 * time.Millisecond)

	payload := []byte("RAW-survives-Pre-OPEN-deadline")
	if _, err := publicClient.Write(payload); err != nil {
		t.Fatalf("public Write() after Pre-OPEN timeout window: %v", err)
	}
	if err := publicClient.CloseWrite(); err != nil {
		t.Fatalf("public CloseWrite(): %v", err)
	}
	echoed, err := io.ReadAll(publicClient)
	if err != nil || !bytes.Equal(echoed, payload) {
		t.Fatalf("RAW echo after Pre-OPEN window = %q, %v, want %q", echoed, err, payload)
	}
	if got := <-payloads; !bytes.Equal(got, payload) {
		t.Fatalf("Agent RAW payload = %q, want %q", got, payload)
	}
	select {
	case err := <-serveResult:
		if err != nil {
			t.Fatalf("Proxy.Serve() error = %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Proxy.Serve() did not finish after RAW echo")
	}
	select {
	case err := <-agentResult:
		if err != nil {
			t.Fatalf("Agent error = %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Agent did not finish RAW echo")
	}
}

func TestProxyServeBoundsPreOpenWithoutIdleWork(t *testing.T) {
	fixture := newFailoverFixture(t, testConnectorID)
	defer fixture.close(t)
	fixture.proxy.publicTCPPreOpenTimeout = 30 * time.Millisecond

	serverPeer, publicClient := tcpPair(t)
	defer publicClient.Close()
	started := time.Now()
	err := fixture.proxy.Serve(context.Background(), testTCPDialRequest(), serverPeer)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Proxy.Serve() error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond || elapsed > time.Second {
		t.Fatalf("Proxy.Serve() Pre-OPEN elapsed = %v, want bounded near 30ms", elapsed)
	}
	if err := publicClient.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set public peer read deadline: %v", err)
	}
	if _, err := publicClient.Read(make([]byte, 1)); err == nil {
		t.Fatal("public peer remained open after Pre-OPEN timeout")
	}
	waitForSnapshot(t, fixture.limits, func(snapshot serverlimits.Snapshot) bool {
		return snapshot.PendingOpens == 0 && snapshot.ActiveTotal == 0 && snapshot.WorkTotal == 0
	})
}

func TestProxyServeTreatsRevisionZeroAsExact(t *testing.T) {
	fixture := newFailoverFixture(t, testConnectorID)
	defer fixture.close(t)
	session := fixture.sessionsByConnector[testConnectorID]
	if !fixture.registry.PublishEligibility(session, serverruntime.SessionEligibility{
		ConfigReady: true, HasObserved: true, ObservedRevision: 1,
		Services: map[string]serverruntime.ServiceEligibility{testServiceID: {
			RequiredRevision: 1, Enabled: true, HealthDisabled: true,
		}},
	}) {
		t.Fatal("PublishEligibility() rejected current Session")
	}
	fixture.registerWork(t, session, nil)

	serverPeer, publicClient := tcpPair(t)
	defer publicClient.Close()
	err := fixture.proxy.Serve(context.Background(), testTCPDialRequest(), serverPeer)
	if !errors.Is(err, serverruntime.ErrNoAvailableConnector) {
		t.Fatalf("Proxy.Serve(revision 0) error = %v, want ErrNoAvailableConnector", err)
	}
	pool := fixture.sessions.Pools()[session]
	if counts := pool.Snapshot(); counts.Idle != 1 {
		t.Fatalf("pool after rejected revision 0 Serve = %+v, want IDLE Work untouched", counts)
	}
}

func TestProxyAggregatesConcurrentPendingOpensAndRefillsBeyondInitialDemand(t *testing.T) {
	registry := serverruntime.NewRegistry()
	limits := newLimitManager(t, 32)
	sessions, err := sessionruntime.New(registry, sessionruntime.Options{
		HighPriorityCapacity: 16, NormalCapacity: 32, InboundCapacity: 16,
		WriteTimeout: time.Second, MaxReplayEntries: 128,
		MaxWorkTotal: 64, MaxWorkConnecting: 16, LimitManager: limits,
		SnapshotProvider: tunnelSnapshotProvider{},
	})
	if err != nil {
		t.Fatalf("sessionruntime.New() error = %v", err)
	}
	startSessionManager(t, sessions)
	connectorIDs := []string{testConnectorID, testConnectorTwo}
	controlPeers := make([]net.Conn, 0, len(connectorIDs))
	controlResults := make([]<-chan error, 0, len(connectorIDs))
	for _, connectorID := range connectorIDs {
		pending, err := registry.ReserveAuthenticated(testTunnelID, connectorID)
		if err != nil {
			t.Fatalf("ReserveAuthenticated(%s) error = %v", connectorID, err)
		}
		session, err := registry.CommitAuthenticated(pending)
		if err != nil {
			t.Fatalf("CommitAuthenticated(%s) error = %v", connectorID, err)
		}
		controlServer, controlAgent := net.Pipe()
		controlPeers = append(controlPeers, controlAgent)
		established := establishedControl(t, session)
		result := make(chan error, 1)
		controlResults = append(controlResults, result)
		go func() { result <- sessions.Serve(context.Background(), controlServer, &established) }()
		readDemand(t, controlAgent)
	}

	openHandler, err := serveropen.NewHandler(serveropen.Options{HandshakeTimeout: time.Second, WriteTimeout: time.Second, ReadTimeout: time.Second})
	if err != nil {
		t.Fatalf("open.NewHandler() error = %v", err)
	}
	tunnelProxy, err := NewProxy(Options{
		Registry: registry, Sessions: sessions, OpenHandler: openHandler,
		AcquireTimeout: time.Second, LimitManager: limits, Logger: testTunnelLogger(),
	})
	if err != nil {
		t.Fatalf("NewProxy() error = %v", err)
	}

	const pendingCount = 9
	publicClients := make([]*net.TCPConn, 0, pendingCount)
	proxyResults := make([]<-chan error, 0, pendingCount)
	for range pendingCount {
		serverPeer, publicClient := tcpPair(t)
		publicClients = append(publicClients, publicClient)
		result := make(chan error, 1)
		proxyResults = append(proxyResults, result)
		go func() {
			result <- tunnelProxy.Serve(context.Background(), testTCPDialRequest(), serverPeer)
		}()
	}
	waitForSnapshot(t, limits, func(snapshot serverlimits.Snapshot) bool { return snapshot.PendingOpens == pendingCount })
	var selectedSession serverruntime.Session
	waitFor(t, func() bool {
		tunnelProxy.pendingMu.Lock()
		defer tunnelProxy.pendingMu.Unlock()
		group := tunnelProxy.pendingGroups[testTunnelID]
		if group == nil || group.waiters != pendingCount {
			return false
		}
		selectedSession = group.session
		return true
	})

	agentResults := make([]<-chan error, 0, pendingCount)
	for index := 0; index < pendingCount-1; index++ {
		agentResults = append(agentResults, registerEchoWork(t, sessions, selectedSession))
	}
	waitForSnapshot(t, limits, func(snapshot serverlimits.Snapshot) bool { return snapshot.PendingOpens == 1 })
	refill := readDemand(t, controlForSession(t, selectedSession, connectorIDs, controlPeers))
	if refill.GetDemandGeneration() <= 1 || refill.GetMaxNewConnections() == 0 {
		t.Fatalf("refill WorkDemand = %#v, want a newer non-zero Demand", refill)
	}
	agentResults = append(agentResults, registerEchoWork(t, sessions, selectedSession))
	waitForSnapshot(t, limits, func(snapshot serverlimits.Snapshot) bool { return snapshot.PendingOpens == 0 })

	for index, client := range publicClients {
		payload := []byte{byte(index), 0x78, 0x74}
		if _, err := client.Write(payload); err != nil {
			t.Fatalf("write public payload %d: %v", index, err)
		}
		if err := client.CloseWrite(); err != nil {
			t.Fatalf("public CloseWrite %d: %v", index, err)
		}
		echoed, err := io.ReadAll(client)
		if err != nil || !bytes.Equal(echoed, payload) {
			t.Fatalf("echo %d = %x, %v, want %x", index, echoed, err, payload)
		}
		_ = client.Close()
	}
	for index, result := range proxyResults {
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("Proxy.Serve(%d) error = %v", index, err)
			}
		case <-time.After(testTimeout):
			t.Fatalf("Proxy.Serve(%d) did not finish", index)
		}
	}
	for index, result := range agentResults {
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("echo Connector(%d) error = %v", index, err)
			}
		case <-time.After(testTimeout):
			t.Fatalf("echo Connector(%d) did not finish", index)
		}
	}
	waitForSnapshot(t, limits, func(snapshot serverlimits.Snapshot) bool {
		return snapshot.PendingOpens == 0 && snapshot.ActiveTotal == 0 && snapshot.WorkTotal == 0
	})

	for _, peer := range controlPeers {
		_ = peer.Close()
	}
	for index, result := range controlResults {
		select {
		case <-result:
		case <-time.After(testTimeout):
			t.Fatalf("Control Session(%d) did not finish", index)
		}
	}
}

func TestProxySelectsOnlyConnectorHealthyForService(t *testing.T) {
	registry := serverruntime.NewRegistry()
	sessions, err := sessionruntime.New(registry, sessionruntime.Options{
		HighPriorityCapacity: 16, NormalCapacity: 32, InboundCapacity: 16,
		WriteTimeout: time.Second, MaxReplayEntries: 128,
		MaxWorkTotal: 64, MaxWorkConnecting: 16, SnapshotProvider: healthTunnelSnapshotProvider{},
	})
	if err != nil {
		t.Fatal(err)
	}
	startSessionManager(t, sessions)

	connectorIDs := []string{testConnectorID, testConnectorTwo}
	peers := make([]net.Conn, 0, len(connectorIDs))
	results := make([]<-chan error, 0, len(connectorIDs))
	var healthySession serverruntime.Session
	for _, connectorID := range connectorIDs {
		pending, reserveErr := registry.ReserveAuthenticated(testTunnelID, connectorID)
		if reserveErr != nil {
			t.Fatal(reserveErr)
		}
		current, commitErr := registry.CommitAuthenticated(pending)
		if commitErr != nil {
			t.Fatal(commitErr)
		}
		serverConn, agentConn := net.Pipe()
		peers = append(peers, agentConn)
		established := establishedControl(t, current)
		result := make(chan error, 1)
		results = append(results, result)
		go func() { result <- sessions.Serve(context.Background(), serverConn, &established) }()
		readDemand(t, agentConn)
		if connectorID == testConnectorTwo {
			healthySession = current
			if writeErr := frame.WriteControl(agentConn, &protocolv1.ControlEnvelope{
				ProtocolVersion: 1,
				Payload: &protocolv1.ControlEnvelope_ServiceHealthBatch{ServiceHealthBatch: &protocolv1.ServiceHealthBatch{
					Generation: 1,
					Items: []*protocolv1.ServiceHealth{{
						ServiceId: testServiceID, Status: protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY,
					}},
				}},
			}); writeErr != nil {
				t.Fatal(writeErr)
			}
		}
	}
	waitFor(t, func() bool { return sessions.Eligible(healthySession, testServiceID) })

	openHandler, err := serveropen.NewHandler(serveropen.Options{HandshakeTimeout: time.Second, WriteTimeout: time.Second, ReadTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	tunnelProxy, err := NewProxy(Options{
		Registry: registry, Sessions: sessions, OpenHandler: openHandler, AcquireTimeout: time.Second,
		Logger: testTunnelLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, selected, _, membership, err := tunnelProxy.selectConnector(
		context.Background(), testTunnelID, testServiceID, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if selected != healthySession {
		t.Fatalf("selected Session = %#v, want only HEALTHY Session %#v", selected, healthySession)
	}
	if membership == nil {
		t.Fatal("selection without IDLE WorkConn did not create pending membership")
	}
	if err := membership.Release(); err != nil {
		t.Fatal(err)
	}
	lease.Release()

	for _, peer := range peers {
		_ = peer.Close()
	}
	for _, result := range results {
		select {
		case <-result:
		case <-time.After(testTimeout):
			t.Fatal("Control Session did not finish")
		}
	}
}

func TestAcquireWorkReselectsWhenPendingSessionBecomesUnhealthy(t *testing.T) {
	registry := serverruntime.NewRegistry()
	limits := newLimitManager(t, 4)
	sessions, err := sessionruntime.New(registry, sessionruntime.Options{
		HighPriorityCapacity: 16, NormalCapacity: 32, InboundCapacity: 16,
		WriteTimeout: time.Second, MaxReplayEntries: 128,
		MaxWorkTotal: 64, MaxWorkConnecting: 16, LimitManager: limits,
		SnapshotProvider: healthTunnelSnapshotProvider{},
	})
	if err != nil {
		t.Fatal(err)
	}
	startSessionManager(t, sessions)

	sessionByConnector := make(map[string]serverruntime.Session, 2)
	controlBySession := make(map[serverruntime.Session]net.Conn, 2)
	results := make([]<-chan error, 0, 2)
	for _, connectorID := range []string{testConnectorID, testConnectorTwo} {
		pending, reserveErr := registry.ReserveAuthenticated(testTunnelID, connectorID)
		if reserveErr != nil {
			t.Fatal(reserveErr)
		}
		session, commitErr := registry.CommitAuthenticated(pending)
		if commitErr != nil {
			t.Fatal(commitErr)
		}
		serverConn, agentConn := net.Pipe()
		result := make(chan error, 1)
		established := establishedControl(t, session)
		go func() { result <- sessions.Serve(context.Background(), serverConn, &established) }()
		readDemand(t, agentConn)
		writeServiceHealth(t, agentConn, 1, protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY)
		sessionByConnector[connectorID] = session
		controlBySession[session] = agentConn
		results = append(results, result)
	}
	for _, session := range sessionByConnector {
		waitFor(t, func() bool { return registry.Eligible(session, testServiceID) })
	}

	openHandler, err := serveropen.NewHandler(serveropen.Options{HandshakeTimeout: time.Second, WriteTimeout: time.Second, ReadTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	tunnelProxy, err := NewProxy(Options{
		Registry: registry, Sessions: sessions, OpenHandler: openHandler,
		AcquireTimeout: 2 * time.Second, LimitManager: limits, Logger: testTunnelLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	type acquireResult struct {
		lease   *serverruntime.ConnectorLease
		session serverruntime.Session
		work    *serverworkpool.Work
		err     error
	}
	result := make(chan acquireResult, 1)
	go func() {
		lease, session, _, work, acquireErr := tunnelProxy.acquireWork(
			context.Background(), testTunnelID, testServiceID, 0,
		)
		result <- acquireResult{lease: lease, session: session, work: work, err: acquireErr}
	}()

	var selected serverruntime.Session
	waitFor(t, func() bool {
		tunnelProxy.pendingMu.Lock()
		defer tunnelProxy.pendingMu.Unlock()
		group := tunnelProxy.pendingGroups[testTunnelID]
		if group == nil || group.waiters != 1 {
			return false
		}
		selected = group.session
		return true
	})
	fallback := sessionByConnector[testConnectorID]
	if fallback == selected {
		fallback = sessionByConnector[testConnectorTwo]
	}
	serverWork, agentWork := tcpPair(t)
	defer agentWork.Close()
	workID, err := identity.NewWorkID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.RegisterIdle(serverWork, authenticatedIdleWithWorkID(t, fallback, workID)); err != nil {
		t.Fatal(err)
	}
	writeServiceHealth(t, controlBySession[selected], 2, protocolv1.HealthStatus_HEALTH_STATUS_UNHEALTHY)

	select {
	case got := <-result:
		if got.err != nil || got.session != fallback || got.work == nil || got.lease == nil {
			t.Fatalf("acquireWork() session=%#v work=%p lease=%p error=%v, want healthy fallback %#v",
				got.session, got.work, got.lease, got.err, fallback)
		}
		if err := got.work.Close(); err != nil {
			t.Fatal(err)
		}
		if !got.lease.Release() {
			t.Fatal("ConnectorLease.Release() = false")
		}
	case <-time.After(time.Second):
		t.Fatal("acquireWork() did not wake and reselect after Health became UNHEALTHY")
	}
	waitForSnapshot(t, limits, func(snapshot serverlimits.Snapshot) bool { return snapshot.PendingOpens == 0 })

	for _, control := range controlBySession {
		_ = control.Close()
	}
	for _, sessionResult := range results {
		select {
		case <-sessionResult:
		case <-time.After(testTimeout):
			t.Fatal("Control Session did not finish")
		}
	}
}

func TestAcquirePendingWorkReselectsAfterHealthTTLExpires(t *testing.T) {
	registry := serverruntime.NewRegistry()
	sessions, err := sessionruntime.New(registry, sessionruntime.Options{
		HighPriorityCapacity: 16, NormalCapacity: 32, InboundCapacity: 16,
		WriteTimeout: time.Second, MaxReplayEntries: 128,
		MaxWorkTotal: 64, MaxWorkConnecting: 16, SnapshotProvider: tunnelSnapshotProvider{},
	})
	if err != nil {
		t.Fatal(err)
	}
	startSessionManager(t, sessions)

	sessionByConnector := make(map[string]serverruntime.Session, 2)
	controlBySession := make(map[serverruntime.Session]net.Conn, 2)
	results := make([]<-chan error, 0, 2)
	for _, connectorID := range []string{testConnectorID, testConnectorTwo} {
		pending, reserveErr := registry.ReserveAuthenticated(testTunnelID, connectorID)
		if reserveErr != nil {
			t.Fatal(reserveErr)
		}
		session, commitErr := registry.CommitAuthenticated(pending)
		if commitErr != nil {
			t.Fatal(commitErr)
		}
		serverConn, agentConn := net.Pipe()
		result := make(chan error, 1)
		established := establishedControl(t, session)
		go func() { result <- sessions.Serve(context.Background(), serverConn, &established) }()
		readDemand(t, agentConn)
		sessionByConnector[connectorID] = session
		controlBySession[session] = agentConn
		results = append(results, result)
	}

	expiresAt := time.Now().Add(150 * time.Millisecond)
	first := sessionByConnector[testConnectorID]
	fallback := sessionByConnector[testConnectorTwo]
	if !registry.PublishEligibility(first, serverruntime.SessionEligibility{
		ConfigReady: true, HasObserved: true,
		Services: map[string]serverruntime.ServiceEligibility{testServiceID: {
			Enabled: true, HealthHealthy: true, HealthyUntil: expiresAt,
		}},
	}) {
		t.Fatal("PublishEligibility(first) rejected current Session")
	}
	if !registry.PublishEligibility(fallback, serverruntime.SessionEligibility{
		ConfigReady: true, HasObserved: true,
		Services: map[string]serverruntime.ServiceEligibility{testServiceID: {
			Enabled: true, HealthDisabled: true,
		}},
	}) {
		t.Fatal("PublishEligibility(fallback) rejected current Session")
	}

	openHandler, err := serveropen.NewHandler(serveropen.Options{HandshakeTimeout: time.Second, WriteTimeout: time.Second, ReadTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	tunnelProxy, err := NewProxy(Options{
		Registry: registry, Sessions: sessions, OpenHandler: openHandler, AcquireTimeout: time.Second,
		Logger: testTunnelLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	type acquireResult struct {
		lease   *serverruntime.ConnectorLease
		session serverruntime.Session
		work    *serverworkpool.Work
		err     error
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	acquired := make(chan acquireResult, 1)
	go func() {
		lease, session, _, work, acquireErr := tunnelProxy.acquireWork(
			ctx, testTunnelID, testServiceID, 0,
		)
		acquired <- acquireResult{lease: lease, session: session, work: work, err: acquireErr}
	}()

	waitFor(t, func() bool {
		tunnelProxy.pendingMu.Lock()
		defer tunnelProxy.pendingMu.Unlock()
		group := tunnelProxy.pendingGroups[testTunnelID]
		return group != nil && group.session == first && group.waiters == 1
	})
	serverWork, agentWork := tcpPair(t)
	defer agentWork.Close()
	workID, err := identity.NewWorkID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.RegisterIdle(serverWork, authenticatedIdleWithWorkID(t, fallback, workID)); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-acquired:
		if got.err != nil || got.session != fallback || got.work == nil || got.lease == nil {
			t.Fatalf("acquireWork() session=%#v work=%p lease=%p error=%v, want TTL fallback %#v",
				got.session, got.work, got.lease, got.err, fallback)
		}
		if err := got.work.Close(); err != nil {
			t.Fatal(err)
		}
		if !got.lease.Release() {
			t.Fatal("ConnectorLease.Release() = false")
		}
	case <-time.After(time.Second):
		t.Fatal("acquireWork() did not reselect after Health TTL expired")
	}

	for _, control := range controlBySession {
		_ = control.Close()
	}
	for _, sessionResult := range results {
		select {
		case <-sessionResult:
		case <-time.After(testTimeout):
			t.Fatal("Control Session did not finish")
		}
	}
}

func TestPendingGroupsAggregateCountsAcrossServicesPerSession(t *testing.T) {
	registry := serverruntime.NewRegistry()
	sessions, err := sessionruntime.New(registry, sessionruntime.Options{
		HighPriorityCapacity: 16, NormalCapacity: 32, InboundCapacity: 16,
		WriteTimeout: time.Second, MaxReplayEntries: 128,
		MaxWorkTotal: 64, MaxWorkConnecting: 16, SnapshotProvider: tunnelSnapshotProvider{},
	})
	if err != nil {
		t.Fatal(err)
	}
	startSessionManager(t, sessions)
	pending, err := registry.ReserveAuthenticated(testTunnelID, testConnectorID)
	if err != nil {
		t.Fatal(err)
	}
	session, err := registry.CommitAuthenticated(pending)
	if err != nil {
		t.Fatal(err)
	}
	serverConn, agentConn := net.Pipe()
	result := make(chan error, 1)
	established := establishedControl(t, session)
	go func() { result <- sessions.Serve(context.Background(), serverConn, &established) }()
	readDemand(t, agentConn)
	if !registry.PublishEligibility(session, serverruntime.SessionEligibility{
		ConfigReady: true, HasObserved: true,
		Services: map[string]serverruntime.ServiceEligibility{
			testServiceID:  {Enabled: true, HealthDisabled: true},
			testServiceTwo: {Enabled: true, HealthDisabled: true},
		},
	}) {
		t.Fatal("PublishEligibility() rejected current Session")
	}
	openHandler, err := serveropen.NewHandler(serveropen.Options{HandshakeTimeout: time.Second, WriteTimeout: time.Second, ReadTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	tunnelProxy, err := NewProxy(Options{
		Registry: registry, Sessions: sessions, OpenHandler: openHandler, AcquireTimeout: time.Second,
		Logger: testTunnelLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}

	first, err := tunnelProxy.joinPendingGroup(
		context.Background(), testTunnelID, testServiceID, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := tunnelProxy.joinPendingGroup(
		context.Background(), testTunnelID, testServiceTwo, 0,
	)
	if err != nil {
		_ = first.Release()
		t.Fatal(err)
	}
	assertPending := func(want uint32, wantGroup bool) {
		t.Helper()
		tunnelProxy.pendingMu.Lock()
		defer tunnelProxy.pendingMu.Unlock()
		if got := tunnelProxy.pendingBySession[session]; got != want {
			t.Fatalf("pendingBySession = %d, want %d", got, want)
		}
		group, exists := tunnelProxy.pendingGroups[testTunnelID]
		var waiters uint32
		if group != nil {
			waiters = group.waiters
		}
		if exists != wantGroup || (exists && waiters != want) {
			t.Fatalf("pending group = exists:%t waiters:%d, want exists:%t waiters:%d",
				exists, waiters, wantGroup, want)
		}
	}
	assertPending(2, true)
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	assertPending(1, true)
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
	assertPending(0, false)

	_ = agentConn.Close()
	select {
	case <-result:
	case <-time.After(testTimeout):
		t.Fatal("Control Session did not finish")
	}
}

func TestPendingGroupSerializesServicesWithDisjointEligibleConnectors(t *testing.T) {
	registry := serverruntime.NewRegistry()
	sessions, err := sessionruntime.New(registry, sessionruntime.Options{
		HighPriorityCapacity: 16, NormalCapacity: 32, InboundCapacity: 16,
		WriteTimeout: time.Second, MaxReplayEntries: 128,
		MaxWorkTotal: 64, MaxWorkConnecting: 16, SnapshotProvider: tunnelSnapshotProvider{},
	})
	if err != nil {
		t.Fatal(err)
	}
	startSessionManager(t, sessions)

	sessionByService := make(map[string]serverruntime.Session, 2)
	peers := make([]net.Conn, 0, 2)
	results := make([]<-chan error, 0, 2)
	for index, connectorID := range []string{testConnectorID, testConnectorTwo} {
		pending, reserveErr := registry.ReserveAuthenticated(testTunnelID, connectorID)
		if reserveErr != nil {
			t.Fatal(reserveErr)
		}
		session, commitErr := registry.CommitAuthenticated(pending)
		if commitErr != nil {
			t.Fatal(commitErr)
		}
		serverConn, agentConn := net.Pipe()
		peers = append(peers, agentConn)
		result := make(chan error, 1)
		results = append(results, result)
		established := establishedControl(t, session)
		go func() { result <- sessions.Serve(context.Background(), serverConn, &established) }()
		readDemand(t, agentConn)

		serviceID := []string{testServiceID, testServiceTwo}[index]
		if !registry.PublishEligibility(session, serverruntime.SessionEligibility{
			ConfigReady: true, HasObserved: true,
			Services: map[string]serverruntime.ServiceEligibility{
				serviceID: {Enabled: true, HealthDisabled: true},
			},
		}) {
			t.Fatalf("PublishEligibility(%s) rejected current Session", serviceID)
		}
		sessionByService[serviceID] = session
	}

	openHandler, err := serveropen.NewHandler(serveropen.Options{HandshakeTimeout: time.Second, WriteTimeout: time.Second, ReadTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	tunnelProxy, err := NewProxy(Options{
		Registry: registry, Sessions: sessions, OpenHandler: openHandler, AcquireTimeout: time.Second,
		Logger: testTunnelLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := tunnelProxy.joinPendingGroup(
		context.Background(), testTunnelID, testServiceID, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.group.session != sessionByService[testServiceID] {
		t.Fatalf("first pending Session = %#v, want Service one eligible Session", first.group.session)
	}

	waitContext, cancelWait := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancelWait()
	if second, err := tunnelProxy.joinPendingGroup(
		waitContext, testTunnelID, testServiceTwo, 0,
	); second != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Service join while Tunnel group active = (%#v, %v), want deadline", second, err)
	}
	tunnelProxy.pendingMu.Lock()
	groups := len(tunnelProxy.pendingGroups)
	tunnelProxy.pendingMu.Unlock()
	if groups != 1 {
		t.Fatalf("Pending groups while Services disagree = %d, want one per Tunnel", groups)
	}

	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := tunnelProxy.joinPendingGroup(
		context.Background(), testTunnelID, testServiceTwo, 0,
	)
	if err != nil {
		t.Fatalf("second Service join after prior Tunnel group drained: %v", err)
	}
	if second.group.session != sessionByService[testServiceTwo] {
		t.Fatalf("second pending Session = %#v, want Service two eligible Session", second.group.session)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}

	for _, peer := range peers {
		_ = peer.Close()
	}
	for _, result := range results {
		select {
		case <-result:
		case <-time.After(testTimeout):
			t.Fatal("Control Session did not finish")
		}
	}
}

func TestProxyPendingOpenTimeoutAndCancelReleaseQuota(t *testing.T) {
	tests := []struct {
		name      string
		cancel    bool
		wantError error
	}{
		{name: "timeout", wantError: serverworkpool.ErrAcquireTimeout},
		{name: "cancel", cancel: true, wantError: context.Canceled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := serverruntime.NewRegistry()
			pending, err := registry.ReserveAuthenticated(testTunnelID, testConnectorID)
			if err != nil {
				t.Fatalf("ReserveAuthenticated() error = %v", err)
			}
			session, err := registry.CommitAuthenticated(pending)
			if err != nil {
				t.Fatalf("CommitAuthenticated() error = %v", err)
			}
			limits := newLimitManager(t, 1)
			sessions, err := sessionruntime.New(registry, sessionruntime.Options{
				HighPriorityCapacity: 8, NormalCapacity: 16, InboundCapacity: 8,
				WriteTimeout: time.Second, MaxReplayEntries: 64,
				MaxWorkTotal: 64, MaxWorkConnecting: 16, LimitManager: limits,
				SnapshotProvider: tunnelSnapshotProvider{},
			})
			if err != nil {
				t.Fatalf("sessionruntime.New() error = %v", err)
			}
			startSessionManager(t, sessions)
			controlServer, controlAgent := net.Pipe()
			established := establishedControl(t, session)
			controlResult := make(chan error, 1)
			go func() { controlResult <- sessions.Serve(context.Background(), controlServer, &established) }()
			readDemand(t, controlAgent)
			openHandler, err := serveropen.NewHandler(serveropen.Options{HandshakeTimeout: time.Second, WriteTimeout: time.Second, ReadTimeout: time.Second})
			if err != nil {
				t.Fatalf("open.NewHandler() error = %v", err)
			}
			tunnelProxy, err := NewProxy(Options{
				Registry: registry, Sessions: sessions, OpenHandler: openHandler,
				AcquireTimeout: 80 * time.Millisecond, LimitManager: limits, Logger: testTunnelLogger(),
			})
			if err != nil {
				t.Fatalf("NewProxy() error = %v", err)
			}
			serverPeer, publicClient := tcpPair(t)
			defer publicClient.Close()
			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan error, 1)
			go func() {
				result <- tunnelProxy.Serve(ctx, testTCPDialRequest(), serverPeer)
			}()
			waitForSnapshot(t, limits, func(snapshot serverlimits.Snapshot) bool { return snapshot.PendingOpens == 1 })
			if test.cancel {
				cancel()
			} else {
				defer cancel()
			}
			select {
			case err := <-result:
				if !errors.Is(err, test.wantError) {
					t.Fatalf("Proxy.Serve() error = %v, want %v", err, test.wantError)
				}
			case <-time.After(testTimeout):
				t.Fatal("Proxy.Serve() did not stop")
			}
			waitForSnapshot(t, limits, func(snapshot serverlimits.Snapshot) bool { return snapshot.PendingOpens == 0 })
			tunnelProxy.pendingMu.Lock()
			groups := len(tunnelProxy.pendingGroups)
			tunnelProxy.pendingMu.Unlock()
			if groups != 0 {
				t.Fatalf("Pending groups = %d after %s, want zero", groups, test.name)
			}
			_ = controlAgent.Close()
			select {
			case <-controlResult:
			case <-time.After(testTimeout):
				t.Fatal("Control Session did not finish")
			}
		})
	}
}

func TestProxyPendingGroupReselectsWhenSelectedSessionDrains(t *testing.T) {
	registry := serverruntime.NewRegistry()
	limits := newLimitManager(t, 4)
	sessions, err := sessionruntime.New(registry, sessionruntime.Options{
		HighPriorityCapacity: 8, NormalCapacity: 16, InboundCapacity: 8,
		WriteTimeout: time.Second, MaxReplayEntries: 64,
		MaxWorkTotal: 64, MaxWorkConnecting: 16, LimitManager: limits,
		SnapshotProvider: tunnelSnapshotProvider{},
	})
	if err != nil {
		t.Fatalf("sessionruntime.New() error = %v", err)
	}
	startSessionManager(t, sessions)
	connectorIDs := []string{testConnectorID, testConnectorTwo}
	controlPeers := make([]net.Conn, 0, 2)
	controlResults := make([]<-chan error, 0, 2)
	sessionByConnector := make(map[string]serverruntime.Session, 2)
	for _, connectorID := range connectorIDs {
		pending, err := registry.ReserveAuthenticated(testTunnelID, connectorID)
		if err != nil {
			t.Fatalf("ReserveAuthenticated(%s) error = %v", connectorID, err)
		}
		session, err := registry.CommitAuthenticated(pending)
		if err != nil {
			t.Fatalf("CommitAuthenticated(%s) error = %v", connectorID, err)
		}
		sessionByConnector[connectorID] = session
		controlServer, controlAgent := net.Pipe()
		controlPeers = append(controlPeers, controlAgent)
		established := establishedControl(t, session)
		result := make(chan error, 1)
		controlResults = append(controlResults, result)
		go func() { result <- sessions.Serve(context.Background(), controlServer, &established) }()
		readDemand(t, controlAgent)
	}
	openHandler, err := serveropen.NewHandler(serveropen.Options{HandshakeTimeout: time.Second, WriteTimeout: time.Second, ReadTimeout: time.Second})
	if err != nil {
		t.Fatalf("open.NewHandler() error = %v", err)
	}
	tunnelProxy, err := NewProxy(Options{
		Registry: registry, Sessions: sessions, OpenHandler: openHandler,
		AcquireTimeout: time.Second, LimitManager: limits, Logger: testTunnelLogger(),
	})
	if err != nil {
		t.Fatalf("NewProxy() error = %v", err)
	}
	serverPeer, publicClient := tcpPair(t)
	defer publicClient.Close()
	proxyResult := make(chan error, 1)
	go func() {
		proxyResult <- tunnelProxy.Serve(context.Background(), testTCPDialRequest(), serverPeer)
	}()
	var first serverruntime.Session
	waitFor(t, func() bool {
		tunnelProxy.pendingMu.Lock()
		defer tunnelProxy.pendingMu.Unlock()
		group := tunnelProxy.pendingGroups[testTunnelID]
		if group == nil || group.waiters != 1 {
			return false
		}
		// PendingOpen 配额早于 Pending Group 创建，不能把配额快照当作 Group
		// 已就绪的同步信号。Session 必须和就绪判断在同一锁区间内取得，避免
		// Race 调度在两次读之间删除或替换 Group。
		first = group.session
		return true
	})
	if snapshot := limits.Snapshot(); snapshot.PendingOpens != 1 {
		t.Fatalf("Pending opens = %d，want 1", snapshot.PendingOpens)
	}
	firstPool, exists := sessions.Pool(first)
	if !exists {
		t.Fatal("selected Pending group Pool disappeared")
	}
	if err := firstPool.BeginDrain(); err != nil {
		t.Fatalf("BeginDrain() error = %v", err)
	}
	var second serverruntime.Session
	waitFor(t, func() bool {
		tunnelProxy.pendingMu.Lock()
		defer tunnelProxy.pendingMu.Unlock()
		group := tunnelProxy.pendingGroups[testTunnelID]
		if group == nil || group.session == first || group.waiters != 1 {
			return false
		}
		second = group.session
		return true
	})
	if second != sessionByConnector[testConnectorTwo] && second != sessionByConnector[testConnectorID] {
		t.Fatalf("reselected Session = %#v, want the other current Connector", second)
	}
	agentResult := registerEchoWork(t, sessions, second)
	payload := []byte("reselected")
	if _, err := publicClient.Write(payload); err != nil {
		t.Fatalf("write public payload: %v", err)
	}
	if err := publicClient.CloseWrite(); err != nil {
		t.Fatalf("public CloseWrite: %v", err)
	}
	echoed, err := io.ReadAll(publicClient)
	if err != nil || !bytes.Equal(echoed, payload) {
		t.Fatalf("echo = %q, %v, want %q", echoed, err, payload)
	}
	select {
	case err := <-proxyResult:
		if err != nil {
			t.Fatalf("Proxy.Serve() error = %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Proxy.Serve() did not finish after reselection")
	}
	select {
	case err := <-agentResult:
		if err != nil {
			t.Fatalf("echo Connector error = %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("echo Connector did not finish")
	}
	waitForSnapshot(t, limits, func(snapshot serverlimits.Snapshot) bool {
		return snapshot.PendingOpens == 0 && snapshot.ActiveTotal == 0
	})
	for _, peer := range controlPeers {
		_ = peer.Close()
	}
	for _, result := range controlResults {
		select {
		case <-result:
		case <-time.After(testTimeout):
			t.Fatal("Control Session did not finish")
		}
	}
}

func TestAcquireWorkRetriesStalePendingSessionAfterGenerationReplacement(t *testing.T) {
	registry := serverruntime.NewRegistry()
	limits := newLimitManager(t, 4)
	sessions, err := sessionruntime.New(registry, sessionruntime.Options{
		HighPriorityCapacity: 8, NormalCapacity: 16, InboundCapacity: 8,
		WriteTimeout: time.Second, MaxReplayEntries: 64,
		MaxWorkTotal: 64, MaxWorkConnecting: 16, LimitManager: limits,
		SnapshotProvider: tunnelSnapshotProvider{},
	})
	if err != nil {
		t.Fatalf("sessionruntime.New() error = %v", err)
	}
	startSessionManager(t, sessions)
	startSession := func(connectorID string) (serverruntime.Session, net.Conn, <-chan error) {
		pending, reserveErr := registry.ReserveAuthenticated(testTunnelID, connectorID)
		if reserveErr != nil {
			t.Fatalf("ReserveAuthenticated(%s) error = %v", connectorID, reserveErr)
		}
		session, commitErr := registry.CommitAuthenticated(pending)
		if commitErr != nil {
			t.Fatalf("CommitAuthenticated(%s) error = %v", connectorID, commitErr)
		}
		controlServer, controlAgent := net.Pipe()
		established := establishedControl(t, session)
		result := make(chan error, 1)
		go func() { result <- sessions.Serve(context.Background(), controlServer, &established) }()
		readDemand(t, controlAgent)
		return session, controlAgent, result
	}
	oldSession, oldControl, oldResult := startSession(testConnectorID)
	fallbackSession, fallbackControl, fallbackResult := startSession(testConnectorTwo)

	openHandler, err := serveropen.NewHandler(serveropen.Options{HandshakeTimeout: time.Second, WriteTimeout: time.Second, ReadTimeout: time.Second})
	if err != nil {
		t.Fatalf("open.NewHandler() error = %v", err)
	}
	tunnelProxy, err := NewProxy(Options{
		Registry: registry, Sessions: sessions, OpenHandler: openHandler,
		AcquireTimeout: time.Second, LimitManager: limits, Logger: testTunnelLogger(),
	})
	if err != nil {
		t.Fatalf("NewProxy() error = %v", err)
	}
	oldPool, exists := sessions.Pool(oldSession)
	if !exists {
		t.Fatal("old Session Pool is missing")
	}
	// 固定在“旧 pending group 已选定、尚未按该 Session 取得 ConnectorLease”的
	// 线性化窗口；下一次 join 会成为这个 group 的唯一 membership。
	tunnelProxy.pendingGroups[testTunnelID] = &pendingGroup{
		session: oldSession, pool: oldPool, done: make(chan struct{}),
	}
	replacement, err := registry.ReserveAuthenticated(testTunnelID, testConnectorID)
	if err != nil {
		t.Fatalf("ReserveAuthenticated(replacement) error = %v", err)
	}
	if _, err := registry.CommitAuthenticated(replacement); err != nil {
		t.Fatalf("CommitAuthenticated(replacement) error = %v", err)
	}

	type acquireResult struct {
		lease   *serverruntime.ConnectorLease
		session serverruntime.Session
		pool    *serverworkpool.Pool
		work    *serverworkpool.Work
		err     error
	}
	result := make(chan acquireResult, 1)
	go func() {
		lease, session, pool, work, acquireErr := tunnelProxy.acquireWork(
			context.Background(), testTunnelID, testServiceID, 0,
		)
		result <- acquireResult{lease: lease, session: session, pool: pool, work: work, err: acquireErr}
	}()

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(testTimeout)
	defer deadline.Stop()
	for {
		tunnelProxy.pendingMu.Lock()
		group := tunnelProxy.pendingGroups[testTunnelID]
		reselected := group != nil && group.session == fallbackSession
		tunnelProxy.pendingMu.Unlock()
		if reselected {
			break
		}
		select {
		case got := <-result:
			t.Fatalf("acquireWork() returned before reselecting replacement generation: %v", got.err)
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("acquireWork() did not reselect the remaining current Session")
		}
	}

	serverWork, agentWork := tcpPair(t)
	defer agentWork.Close()
	workID, err := identity.NewWorkID()
	if err != nil {
		t.Fatalf("NewWorkID() error = %v", err)
	}
	if _, err := sessions.RegisterIdle(
		serverWork,
		authenticatedIdleWithWorkID(t, fallbackSession, workID),
	); err != nil {
		t.Fatalf("RegisterIdle(fallback) error = %v", err)
	}
	select {
	case got := <-result:
		if got.err != nil || got.session != fallbackSession || got.pool == nil || got.work == nil || got.lease == nil {
			t.Fatalf("acquireWork() = %#v, want fallback Session", got)
		}
		if err := got.work.Close(); err != nil {
			t.Fatalf("close acquired Work: %v", err)
		}
		if !got.lease.Release() {
			t.Fatal("release acquired Connector lease = false")
		}
	case <-time.After(testTimeout):
		t.Fatal("acquireWork() did not finish after fallback Work became IDLE")
	}

	_ = oldControl.Close()
	_ = fallbackControl.Close()
	for name, sessionResult := range map[string]<-chan error{"old": oldResult, "fallback": fallbackResult} {
		select {
		case <-sessionResult:
		case <-time.After(testTimeout):
			t.Fatalf("%s Control Session did not finish", name)
		}
	}
}

func startSessionManager(t *testing.T, sessions *sessionruntime.Manager) {
	t.Helper()
	if err := sessions.Start(context.Background()); err != nil {
		t.Fatalf("sessionruntime.Manager.Start() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = sessions.Shutdown(ctx)
	})
}

func runEchoConnector(connection *net.TCPConn) error {
	defer connection.Close()
	request := &protocolv1.OpenRequest{}
	if err := frame.ReadWork(connection, request); err != nil {
		return err
	}
	response := &protocolv1.OpenResponse{
		ConnectionId: request.GetConnectionId(), Status: protocolv1.OpenStatus_OPEN_STATUS_OK,
		ErrorCode: protocolv1.ErrorCode_ERROR_CODE_OK,
	}
	if err := frame.WriteWork(connection, response); err != nil {
		return err
	}
	payload, err := io.ReadAll(connection)
	if err != nil {
		return err
	}
	if _, err := connection.Write(payload); err != nil {
		return err
	}
	return connection.CloseWrite()
}

func establishedControl(t *testing.T, session serverruntime.Session) servercontrolauth.Established {
	t.Helper()
	control, err := state.NewControl(state.EndpointServer, 1)
	if err != nil {
		t.Fatalf("NewControl() error = %v", err)
	}
	result := &protocolv1.ConnectorAuthResult{Result: &protocolv1.ConnectorAuthResult_Success{
		Success: &protocolv1.ConnectorAuthSuccess{SessionSecret: make([]byte, 32)},
	}}
	if _, err := control.AcceptOutbound(result); err != nil {
		t.Fatalf("AcceptOutbound(AuthSuccess) error = %v", err)
	}
	if err := control.CommitAuthSuccessAfterFlush(result); err != nil {
		t.Fatalf("CommitAuthSuccessAfterFlush() error = %v", err)
	}
	var secret [32]byte
	for index := range secret {
		secret[index] = byte(index + 1)
	}
	return servercontrolauth.Established{
		Session: session, SessionSecret: secret, ProtocolVersion: 1,
		HeartbeatInterval: 10 * time.Second, Control: control,
	}
}

func authenticatedIdle(t *testing.T, session serverruntime.Session) serverworkauth.Idle {
	return authenticatedIdleWithWorkID(t, session, testWorkID)
}

func authenticatedIdleWithWorkID(t *testing.T, session serverruntime.Session, workID string) serverworkauth.Idle {
	t.Helper()
	workState, err := state.NewWork(state.EndpointServer)
	if err != nil {
		t.Fatalf("NewWork() error = %v", err)
	}
	hello := &protocolv1.WorkHello{
		TunnelId: session.TunnelID, ConnectorId: session.ConnectorID, SessionId: session.SessionID,
		WorkId: workID, Nonce: make([]byte, 32), Mac: make([]byte, 32),
		BudgetLeaseId: "lease_01J00000000000000000000000",
	}
	if err := workState.AcceptInbound(hello); err != nil {
		t.Fatalf("AcceptInbound(WorkHello) error = %v", err)
	}
	ready := &protocolv1.WorkReady{WorkId: workID, Status: protocolv1.WorkReadyStatus_WORK_READY_STATUS_READY}
	if err := workState.AcceptOutbound(ready); err != nil {
		t.Fatalf("AcceptOutbound(WorkReady) error = %v", err)
	}
	return serverworkauth.Idle{
		TunnelID: session.TunnelID, ConnectorID: session.ConnectorID,
		SessionID: session.SessionID, WorkID: workID, State: workState,
	}
}

func registerEchoWork(
	t *testing.T,
	sessions *sessionruntime.Manager,
	session serverruntime.Session,
) <-chan error {
	t.Helper()
	serverWork, agentWork := tcpPair(t)
	workID, err := identity.NewWorkID()
	if err != nil {
		t.Fatalf("NewWorkID() error = %v", err)
	}
	if _, err := sessions.RegisterIdle(serverWork, authenticatedIdleWithWorkID(t, session, workID)); err != nil {
		_ = agentWork.Close()
		t.Fatalf("RegisterIdle(%s) error = %v", workID, err)
	}
	result := make(chan error, 1)
	go func() { result <- runEchoConnector(agentWork) }()
	return result
}

func writeServiceHealth(
	t *testing.T,
	connection net.Conn,
	generation uint64,
	status protocolv1.HealthStatus,
) {
	t.Helper()
	if err := frame.WriteControl(connection, &protocolv1.ControlEnvelope{
		ProtocolVersion: 1,
		Payload: &protocolv1.ControlEnvelope_ServiceHealthBatch{ServiceHealthBatch: &protocolv1.ServiceHealthBatch{
			Generation: generation,
			Items: []*protocolv1.ServiceHealth{{
				ServiceId: testServiceID, Status: status,
			}},
		}},
	}); err != nil {
		t.Fatalf("write ServiceHealthBatch generation %d: %v", generation, err)
	}
}

func readDemand(t *testing.T, connection net.Conn) *protocolv1.WorkDemand {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatalf("set WorkDemand deadline: %v", err)
	}
	envelope := &protocolv1.ControlEnvelope{}
	if err := frame.ReadControl(connection, envelope); err != nil {
		t.Fatalf("read initial Control message: %v", err)
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
			t.Fatalf("write ConfigAck: %v", err)
		}
		envelope.Reset()
		if err := frame.ReadControl(connection, envelope); err != nil {
			t.Fatalf("read WorkDemand after ConfigAck: %v", err)
		}
	}
	if err := connection.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear WorkDemand deadline: %v", err)
	}
	if envelope.GetWorkDemand() == nil {
		t.Fatalf("Control message = %#v, want WorkDemand", envelope)
	}
	return envelope.GetWorkDemand()
}

func controlForSession(
	t *testing.T,
	session serverruntime.Session,
	connectorIDs []string,
	connections []net.Conn,
) net.Conn {
	t.Helper()
	for index, connectorID := range connectorIDs {
		if connectorID == session.ConnectorID {
			return connections[index]
		}
	}
	t.Fatalf("no Control peer for Connector %s", session.ConnectorID)
	return nil
}

func newLimitManager(t *testing.T, maxPending uint64) *serverlimits.Manager {
	t.Helper()
	manager, err := serverlimits.New(serverlimits.Options{
		MaxConnectors: 8, MaxConnectorsPerTunnel: 8,
		MaxWorkConnections: 64, MaxIdleWorkConnections: 64, MaxConnectingWorkConnections: 64,
		MaxPendingOpens: maxPending, MaxActiveConnections: 64,
		MaxConnectionsPerTunnel: 64, MaxConnectionsPerService: 64, MaxConnectionsPerSourceIP: 64,
		MaxOpenRatePerSourceIP: 1_000, MaxOpenBurstPerSourceIP: 1_000,
		MaxHTTPRequestsPerSourceIPPerSecond: 1_000,
	})
	if err != nil {
		t.Fatalf("limits.New() error = %v", err)
	}
	return manager
}

func waitForSnapshot(
	t *testing.T,
	manager *serverlimits.Manager,
	condition func(serverlimits.Snapshot) bool,
) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for {
		if condition(manager.Snapshot()) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Limit snapshot = %#v before deadline", manager.Snapshot())
		}
		time.Sleep(time.Millisecond)
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition was not satisfied before deadline")
		}
		time.Sleep(time.Millisecond)
	}
}

func tcpPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("ListenTCP: %v", err)
	}
	defer listener.Close()
	type dialResult struct {
		connection *net.TCPConn
		err        error
	}
	dialed := make(chan dialResult, 1)
	go func() {
		connection, dialErr := net.DialTCP("tcp", nil, listener.Addr().(*net.TCPAddr))
		dialed <- dialResult{connection: connection, err: dialErr}
	}()
	accepted, err := listener.AcceptTCP()
	if err != nil {
		t.Fatalf("AcceptTCP: %v", err)
	}
	peer := <-dialed
	if peer.err != nil {
		accepted.Close()
		t.Fatalf("DialTCP: %v", peer.err)
	}
	return accepted, peer.connection
}

// eofCloseTCPConnection 保留 TCP Half-Close 方法集，同时让 Revoke 回归可确定地
// 观察 WorkConn Close 错误。Close 必须先真正关闭 FD，再返回 EOF。
type eofCloseTCPConnection struct {
	*net.TCPConn
	closeCount *atomic.Int32
}

func (connection *eofCloseTCPConnection) Close() error {
	connection.closeCount.Add(1)
	return errors.Join(connection.TCPConn.Close(), io.EOF)
}

func (connection *eofCloseTCPConnection) CloseWrite() error {
	return connection.TCPConn.CloseWrite()
}

func (connection *eofCloseTCPConnection) CloseRead() error {
	return connection.TCPConn.CloseRead()
}
