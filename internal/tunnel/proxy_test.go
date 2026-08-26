package tunnel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/state"
	servercontrolauth "github.com/lifei6671/xtunnel/internal/server/controlauth"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
	serveropen "github.com/lifei6671/xtunnel/internal/server/open"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
	"github.com/lifei6671/xtunnel/internal/server/sessionruntime"
	serverworkauth "github.com/lifei6671/xtunnel/internal/server/workauth"
	serverworkpool "github.com/lifei6671/xtunnel/internal/server/workpool"
)

const (
	testTunnelID     = "tun_01J00000000000000000000000"
	testConnectorID  = "con_01J00000000000000000000000"
	testConnectorTwo = "con_01J00000000000000000000001"
	testServiceID    = "svc_01J00000000000000000000000"
	testWorkID       = "work_01J00000000000000000000000"
	testTimeout      = 3 * time.Second
)

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
		MaxWorkTotal: 64, MaxWorkConnecting: 16,
	})
	if err != nil {
		t.Fatalf("sessionruntime.New() error = %v", err)
	}
	controlServer, controlAgent := net.Pipe()
	defer controlAgent.Close()
	established := establishedControl(t, session)
	controlResult := make(chan error, 1)
	go func() { controlResult <- sessions.Serve(context.Background(), controlServer, &established) }()
	initialDemand := &protocolv1.ControlEnvelope{}
	if err := frame.ReadControl(controlAgent, initialDemand); err != nil {
		t.Fatalf("read initial WorkDemand: %v", err)
	}
	if initialDemand.GetWorkDemand() == nil {
		t.Fatal("initial Control message is not WorkDemand")
	}

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

	openHandler, err := serveropen.NewHandler(serveropen.Options{WriteTimeout: time.Second, ReadTimeout: time.Second})
	if err != nil {
		t.Fatalf("open.NewHandler() error = %v", err)
	}
	tunnelProxy, err := NewProxy(Options{
		Registry: registry, Sessions: sessions, OpenHandler: openHandler, AcquireTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewProxy() error = %v", err)
	}
	serverPeer, publicClient := tcpPair(t)
	defer publicClient.Close()
	proxyResult := make(chan error, 1)
	go func() {
		proxyResult <- tunnelProxy.Serve(context.Background(), testTunnelID, testServiceID,
			protocolv1.IngressType_INGRESS_TYPE_TCP, serverPeer)
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

	_ = controlAgent.Close()
	select {
	case <-controlResult:
	case <-time.After(testTimeout):
		t.Fatal("Control Session did not finish")
	}
}

func TestProxyAggregatesConcurrentPendingOpensAndRefillsBeyondInitialDemand(t *testing.T) {
	registry := serverruntime.NewRegistry()
	limits := newLimitManager(t, 32)
	sessions, err := sessionruntime.New(registry, sessionruntime.Options{
		HighPriorityCapacity: 16, NormalCapacity: 32, InboundCapacity: 16,
		WriteTimeout: time.Second, MaxReplayEntries: 128,
		MaxWorkTotal: 64, MaxWorkConnecting: 16, LimitManager: limits,
	})
	if err != nil {
		t.Fatalf("sessionruntime.New() error = %v", err)
	}
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

	openHandler, err := serveropen.NewHandler(serveropen.Options{WriteTimeout: time.Second, ReadTimeout: time.Second})
	if err != nil {
		t.Fatalf("open.NewHandler() error = %v", err)
	}
	tunnelProxy, err := NewProxy(Options{
		Registry: registry, Sessions: sessions, OpenHandler: openHandler,
		AcquireTimeout: time.Second, LimitManager: limits,
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
			result <- tunnelProxy.Serve(context.Background(), testTunnelID, testServiceID,
				protocolv1.IngressType_INGRESS_TYPE_TCP, serverPeer)
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
			})
			if err != nil {
				t.Fatalf("sessionruntime.New() error = %v", err)
			}
			controlServer, controlAgent := net.Pipe()
			established := establishedControl(t, session)
			controlResult := make(chan error, 1)
			go func() { controlResult <- sessions.Serve(context.Background(), controlServer, &established) }()
			readDemand(t, controlAgent)
			openHandler, err := serveropen.NewHandler(serveropen.Options{WriteTimeout: time.Second, ReadTimeout: time.Second})
			if err != nil {
				t.Fatalf("open.NewHandler() error = %v", err)
			}
			tunnelProxy, err := NewProxy(Options{
				Registry: registry, Sessions: sessions, OpenHandler: openHandler,
				AcquireTimeout: 80 * time.Millisecond, LimitManager: limits,
			})
			if err != nil {
				t.Fatalf("NewProxy() error = %v", err)
			}
			serverPeer, publicClient := tcpPair(t)
			defer publicClient.Close()
			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan error, 1)
			go func() {
				result <- tunnelProxy.Serve(ctx, testTunnelID, testServiceID,
					protocolv1.IngressType_INGRESS_TYPE_TCP, serverPeer)
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
	})
	if err != nil {
		t.Fatalf("sessionruntime.New() error = %v", err)
	}
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
	openHandler, err := serveropen.NewHandler(serveropen.Options{WriteTimeout: time.Second, ReadTimeout: time.Second})
	if err != nil {
		t.Fatalf("open.NewHandler() error = %v", err)
	}
	tunnelProxy, err := NewProxy(Options{
		Registry: registry, Sessions: sessions, OpenHandler: openHandler,
		AcquireTimeout: time.Second, LimitManager: limits,
	})
	if err != nil {
		t.Fatalf("NewProxy() error = %v", err)
	}
	serverPeer, publicClient := tcpPair(t)
	defer publicClient.Close()
	proxyResult := make(chan error, 1)
	go func() {
		proxyResult <- tunnelProxy.Serve(context.Background(), testTunnelID, testServiceID,
			protocolv1.IngressType_INGRESS_TYPE_TCP, serverPeer)
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
	})
	if err != nil {
		t.Fatalf("sessionruntime.New() error = %v", err)
	}
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

	openHandler, err := serveropen.NewHandler(serveropen.Options{WriteTimeout: time.Second, ReadTimeout: time.Second})
	if err != nil {
		t.Fatalf("open.NewHandler() error = %v", err)
	}
	tunnelProxy, err := NewProxy(Options{
		Registry: registry, Sessions: sessions, OpenHandler: openHandler,
		AcquireTimeout: time.Second, LimitManager: limits,
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
	tunnelProxy.pendingGroups[testTunnelID] = &pendingGroup{session: oldSession, pool: oldPool}
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
		lease, session, pool, work, acquireErr := tunnelProxy.acquireWork(context.Background(), testTunnelID)
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

func readDemand(t *testing.T, connection net.Conn) *protocolv1.WorkDemand {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatalf("set WorkDemand deadline: %v", err)
	}
	envelope := &protocolv1.ControlEnvelope{}
	if err := frame.ReadControl(connection, envelope); err != nil {
		t.Fatalf("read WorkDemand: %v", err)
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
