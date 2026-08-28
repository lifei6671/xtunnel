package tcpingress

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/repository"
	repositorysqlite "github.com/lifei6671/xtunnel/internal/repository/sqlite"
	"github.com/lifei6671/xtunnel/internal/safego"
	serverroute "github.com/lifei6671/xtunnel/internal/server/route"
	serverstatus "github.com/lifei6671/xtunnel/internal/server/status"
)

const (
	tcpIngressTunnelID  = "tun_01J00000000000000000000040"
	tcpIngressServiceID = "svc_01J00000000000000000000040"
)

func TestManagerFailedPortMoveKeepsOldThenAtomicallyConverges(t *testing.T) {
	routes, source, cancelRoutes := startTCPRouteManager(t, tcpRouteState(1, "tcp-one", tcpIngressServiceID, 10000))
	defer cancelRoutes()
	factory := newFakeListenerFactory()
	manager := newTestManager(t, routes, factory)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	waitTCPCondition(t, func() bool {
		actual := manager.Actual()
		return len(actual) == 1 && actual[0].Route.PublicPort == 10000
	})
	oldListener := factory.listener("127.0.0.1:10000")

	factory.setFailure("127.0.0.1:10002", true)
	publishTCPRouteState(t, routes, source, tcpRouteState(2, "tcp-one", tcpIngressServiceID, 10002))
	manager.MarkDirty(2)
	waitTCPCondition(t, func() bool {
		actual := manager.Actual()
		failures := manager.ApplyFailures()
		return len(actual) == 1 && actual[0].Route.PublicPort == 10000 &&
			actual[0].Route.RequiredRevision == 2 &&
			len(failures) == 1 && failures[0].RouteID == "tcp-one" &&
			failures[0].PublicPort == 10002 && failures[0].Generation == 2 &&
			failures[0].ErrorCode == ListenFailedErrorCode
	})
	if oldListener.closedState() {
		t.Fatal("old Listener closed before replacement became available")
	}

	factory.setFailure("127.0.0.1:10002", false)
	manager.MarkDirty(2)
	waitTCPCondition(t, func() bool {
		actual := manager.Actual()
		return len(actual) == 1 && actual[0].Route.PublicPort == 10002 &&
			len(manager.ApplyFailures()) == 0 && oldListener.closedState()
	})

	state := tcpRouteState(3, "tcp-one", tcpIngressServiceID, 10002)
	state.TCPRoutes = nil
	publishTCPRouteState(t, routes, source, state)
	manager.MarkDirty(3)
	replacement := factory.listener("127.0.0.1:10002")
	waitTCPCondition(t, func() bool {
		return len(manager.Actual()) == 0 && replacement != nil && replacement.closedState()
	})
	if replacement == nil {
		t.Fatal("deleted Route did not stop replacement Listener")
	}
}

func TestManagerSamePortRouteChangeReusesExactIPListener(t *testing.T) {
	state := tcpRouteState(1, "tcp-one", tcpIngressServiceID, 10000)
	secondServiceID := "svc_01J00000000000000000000041"
	state.Services = append(state.Services, validTCPIngressService(secondServiceID, 1))
	routes, source, cancelRoutes := startTCPRouteManager(t, state)
	defer cancelRoutes()
	factory := newFakeListenerFactory()
	manager := newTestManager(t, routes, factory)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	waitTCPCondition(t, func() bool { return len(manager.Actual()) == 1 })
	if calls := factory.callsSnapshot(); len(calls) != 1 || calls[0].network != "tcp4" || calls[0].address != "127.0.0.1:10000" {
		t.Fatalf("Listen calls = %+v, want one tcp4 127.0.0.1:10000", calls)
	}

	updated := tcpRouteState(2, "tcp-two", secondServiceID, 10000)
	updated.Services = append(updated.Services, validTCPIngressService(tcpIngressServiceID, 1))
	publishTCPRouteState(t, routes, source, updated)
	manager.MarkDirty(2)
	waitTCPCondition(t, func() bool {
		actual := manager.Actual()
		return len(actual) == 1 && actual[0].Route.ID == "tcp-two" && actual[0].Route.ServiceID == secondServiceID
	})
	if calls := factory.callsSnapshot(); len(calls) != 1 {
		t.Fatalf("same-port update made %d Listen calls, want 1", len(calls))
	}
}

func TestManagerKeepsPerRouteFailuresAndClearsOnlyRecoveredRoute(t *testing.T) {
	state := tcpRouteState(1, "tcp-one", tcpIngressServiceID, 10000)
	state.TCPRoutes = append(state.TCPRoutes, repository.TCPRoute{
		ID: "tcp-two", ServiceID: tcpIngressServiceID, PublicPort: 10002,
		Enabled: true, CreatedAt: 1, UpdatedAt: 1,
	})
	routes, _, cancelRoutes := startTCPRouteManager(t, state)
	defer cancelRoutes()
	factory := newFakeListenerFactory()
	factory.setFailure("127.0.0.1:10000", true)
	factory.setFailure("127.0.0.1:10002", true)
	manager := newTestManager(t, routes, factory)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	waitTCPCondition(t, func() bool { return len(manager.ApplyFailures()) == 2 })
	failures := manager.ApplyFailures()
	if failures[0].PublicPort != 10000 || failures[1].PublicPort != 10002 ||
		failures[0].ServiceID != tcpIngressServiceID || failures[1].ServiceID != tcpIngressServiceID {
		t.Fatalf("ApplyFailures() = %+v", failures)
	}
	applyFailure := manager.ServiceApplyFailure(tcpIngressServiceID, 1)
	if got := serverstatus.CalculateService(serverstatus.ServiceInput{
		Enabled: true, RequiredRevision: 1, ApplyFailure: applyFailure,
	}); got != serverstatus.ServiceStatusApplyFailed {
		t.Fatalf("CalculateService() = %q, want APPLY_FAILED", got)
	}

	factory.setFailure("127.0.0.1:10000", false)
	manager.MarkDirty(1)
	waitTCPCondition(t, func() bool {
		actual := manager.Actual()
		remaining := manager.ApplyFailures()
		return len(actual) == 1 && actual[0].Route.PublicPort == 10000 &&
			len(remaining) == 1 && remaining[0].PublicPort == 10002
	})
}

func TestManagerReconcilesOnlyAfterRouteSnapshotPublicationNotification(t *testing.T) {
	routes, source, cancelRoutes := startTCPRouteManager(
		t, tcpRouteState(1, "tcp-one", tcpIngressServiceID, 10000),
	)
	defer cancelRoutes()
	factory := newFakeListenerFactory()
	manager := newTestManager(t, routes, factory)
	if err := routes.ObservePublished(manager.MarkDirty); err != nil {
		t.Fatalf("ObservePublished() error = %v", err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	// Config Write 只唤醒 Route owner。TCP owner 的唯一即时通知来自 Snapshot
	// 成功发布后的 observer，因此不可能先消费旧代次并吞掉本轮唤醒。
	publishTCPRouteState(t, routes, source, tcpRouteState(2, "tcp-one", tcpIngressServiceID, 10002))
	waitTCPCondition(t, func() bool {
		actual := manager.Actual()
		return len(actual) == 1 && actual[0].Route.PublicPort == 10002
	})
}

func TestManagerRejectsOldListenerAfterNewRouteSnapshotPublication(t *testing.T) {
	routes, source, cancelRoutes := startTCPRouteManager(
		t, tcpRouteState(1, "tcp-one", tcpIngressServiceID, 10000),
	)
	defer cancelRoutes()
	factory := newFakeListenerFactory()
	handled := make(chan serverroute.TCPRoute, 1)
	manager, err := NewManager(Options{
		Bind: netip.MustParseAddr("127.0.0.1"), MinPort: 10000, MaxPort: 10010,
		Routes: routes,
		Handler: func(_ context.Context, _ net.Conn, route serverroute.TCPRoute) {
			handled <- route
		},
		listen: factory.listen, retryInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	// 只发布 Route Snapshot，不通知 TCP owner，固定复现 Snapshot 已到 G+1、
	// Listener 仍停留在 G 的窗口。旧 Listener 必须在准入处立即拒绝连接。
	publishTCPRouteState(t, routes, source, tcpRouteState(2, "tcp-one", tcpIngressServiceID, 10000))
	actual := manager.Actual()
	if len(actual) != 1 || actual[0].Route.RequiredRevision != 1 {
		t.Fatalf("Actual() before TCP reconcile = %+v, want generation 1 listener", actual)
	}
	serverConnection, peer := net.Pipe()
	factory.listener("127.0.0.1:10000").acceptConnection(serverConnection)
	assertPeerClosed(t, peer)
	select {
	case route := <-handled:
		t.Fatalf("old listener admitted route after generation 2 publication: %+v", route)
	default:
	}
}

func TestManagerGenerationFenceClosesLateListenerAndDoesNotResurrectDeletedRoute(t *testing.T) {
	routes, source, cancelRoutes := startTCPRouteManager(
		t, tcpRouteState(1, "tcp-one", tcpIngressServiceID, 10000),
	)
	defer cancelRoutes()
	factory := newFakeListenerFactory()
	manager := newTestManager(t, routes, factory)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	factory.block("127.0.0.1:10002")
	publishTCPRouteState(t, routes, source, tcpRouteState(2, "tcp-one", tcpIngressServiceID, 10002))
	manager.MarkDirty(2)
	waitTCPCondition(t, func() bool {
		calls := factory.callsSnapshot()
		return len(calls) >= 2 && calls[len(calls)-1].address == "127.0.0.1:10002"
	})
	deleted := tcpRouteState(3, "tcp-one", tcpIngressServiceID, 10002)
	deleted.TCPRoutes = nil
	publishTCPRouteState(t, routes, source, deleted)
	manager.MarkDirty(3)
	factory.release("127.0.0.1:10002")

	waitTCPCondition(t, func() bool { return len(manager.Actual()) == 0 })
	late := factory.listener("127.0.0.1:10002")
	if late == nil || !late.closedState() {
		t.Fatal("late stale Listener was published or lost instead of being closed")
	}
}

func TestManagerCancellationClosesEarlierUnpublishedCandidates(t *testing.T) {
	state := tcpRouteState(1, "tcp-one", tcpIngressServiceID, 10000)
	state.TCPRoutes = append(state.TCPRoutes, repository.TCPRoute{
		ID: "tcp-two", ServiceID: tcpIngressServiceID, PublicPort: 10002,
		Enabled: true, CreatedAt: 1, UpdatedAt: 1,
	})
	routes, _, cancelRoutes := startTCPRouteManager(t, state)
	defer cancelRoutes()
	factory := newFakeListenerFactory()
	factory.block("127.0.0.1:10002")
	manager := newTestManager(t, routes, factory)
	startResult := make(chan error, 1)
	go func() { startResult <- manager.Start(context.Background()) }()
	waitTCPCondition(t, func() bool {
		calls := factory.callsSnapshot()
		return len(calls) == 2 && calls[1].address == "127.0.0.1:10002"
	})
	first := factory.listener("127.0.0.1:10000")
	if first == nil {
		t.Fatal("first candidate Listener was not created")
	}
	if err := manager.StopAccepting(); err != nil {
		t.Fatalf("StopAccepting() error = %v", err)
	}
	if err := <-startResult; err != nil {
		t.Fatalf("Start() after cancellation error = %v", err)
	}
	if !first.closedState() || len(manager.Actual()) != 0 {
		t.Fatalf("canceled candidates = closed:%t actual:%+v", first.closedState(), manager.Actual())
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestManagerSamePortUpdatePreservesAcceptedRouteValue(t *testing.T) {
	state := tcpRouteState(1, "tcp-one", tcpIngressServiceID, 10000)
	secondServiceID := "svc_01J00000000000000000000041"
	state.Services = append(state.Services, validTCPIngressService(secondServiceID, 1))
	routes, source, cancelRoutes := startTCPRouteManager(t, state)
	defer cancelRoutes()
	factory := newFakeListenerFactory()
	received := make(chan serverroute.TCPRoute, 2)
	manager, err := NewManager(Options{
		Bind: netip.MustParseAddr("127.0.0.1"), MinPort: 10000, MaxPort: 10010,
		Routes: routes, Handler: func(ctx context.Context, _ net.Conn, route serverroute.TCPRoute) {
			received <- route
			<-ctx.Done()
		},
		listen: factory.listen, retryInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	listener := factory.listener("127.0.0.1:10000")
	firstServer, firstPeer := net.Pipe()
	defer firstPeer.Close()
	listener.acceptConnection(firstServer)
	if route := <-received; route.ID != "tcp-one" {
		t.Fatalf("first accepted Route = %q, want tcp-one", route.ID)
	}

	updated := tcpRouteState(2, "tcp-two", secondServiceID, 10000)
	updated.Services = append(updated.Services, validTCPIngressService(tcpIngressServiceID, 1))
	publishTCPRouteState(t, routes, source, updated)
	manager.MarkDirty(2)
	waitTCPCondition(t, func() bool {
		actual := manager.Actual()
		return len(actual) == 1 && actual[0].Route.ID == "tcp-two"
	})
	secondServer, secondPeer := net.Pipe()
	defer secondPeer.Close()
	listener.acceptConnection(secondServer)
	if route := <-received; route.ID != "tcp-two" {
		t.Fatalf("second accepted Route = %q, want tcp-two", route.ID)
	}
}

func TestManagerConsumesSourceOpenBeforeConnectionAdmission(t *testing.T) {
	routes, _, cancelRoutes := startTCPRouteManager(
		t, tcpRouteState(1, "tcp-one", tcpIngressServiceID, 10000),
	)
	defer cancelRoutes()
	factory := newFakeListenerFactory()
	limiter := &recordingSourceLimiter{rejected: netip.MustParseAddr("203.0.113.5")}
	handled := make(chan netip.Addr, 1)
	manager, err := NewManager(Options{
		Bind: netip.MustParseAddr("127.0.0.1"), MinPort: 10000, MaxPort: 10010,
		Routes: routes, SourceLimiter: limiter,
		Handler: func(ctx context.Context, connection net.Conn, _ serverroute.TCPRoute) {
			handled <- netip.MustParseAddrPort(connection.RemoteAddr().String()).Addr()
			<-ctx.Done()
		},
		listen: factory.listen, retryInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	listener := factory.listener("127.0.0.1:10000")

	invalidServer, invalidPeer := net.Pipe()
	listener.acceptConnection(&remoteAddrConn{Conn: invalidServer, remote: fakeAddr("invalid-peer")})
	assertPeerClosed(t, invalidPeer)
	if calls := limiter.callsSnapshot(); len(calls) != 0 {
		t.Fatalf("limiter calls after invalid peer = %v, want none", calls)
	}

	rejectedServer, rejectedPeer := net.Pipe()
	listener.acceptConnection(&remoteAddrConn{Conn: rejectedServer, remote: fakeAddr("203.0.113.5:41000")})
	assertPeerClosed(t, rejectedPeer)
	if calls := limiter.callsSnapshot(); len(calls) != 1 || calls[0] != netip.MustParseAddr("203.0.113.5") {
		t.Fatalf("limiter calls after rejected peer = %v", calls)
	}
	select {
	case source := <-handled:
		t.Fatalf("rejected peer reached Handler with source %s", source)
	default:
	}

	allowedServer, allowedPeer := net.Pipe()
	defer allowedPeer.Close()
	listener.acceptConnection(&remoteAddrConn{Conn: allowedServer, remote: fakeAddr("203.0.113.6:41001")})
	select {
	case source := <-handled:
		if source != netip.MustParseAddr("203.0.113.6") {
			t.Fatalf("handled source = %s", source)
		}
	case <-time.After(time.Second):
		t.Fatal("allowed peer did not reach Handler")
	}
	if calls := limiter.callsSnapshot(); len(calls) != 2 || calls[1] != netip.MustParseAddr("203.0.113.6") {
		t.Fatalf("limiter calls after allowed peer = %v", calls)
	}
}

func TestManagerRetainsRejectedConnectionCloseFailureWithoutFatalReport(t *testing.T) {
	tests := []struct {
		name            string
		sourceLimiter   SourceLimiter
		remote          net.Addr
		rejectAdmission bool
	}{
		{
			name:          "invalid source address",
			sourceLimiter: &recordingSourceLimiter{},
			remote:        fakeAddr("invalid-peer"),
		},
		{
			name:            "admission fence",
			rejectAdmission: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			routes, _, cancelRoutes := startTCPRouteManager(
				t, tcpRouteState(1, "tcp-one", tcpIngressServiceID, 10000),
			)
			defer cancelRoutes()
			factory := newFakeListenerFactory()
			runtimeErrors := make(chan error, 1)
			var runtimeCalls atomic.Int32
			manager, err := NewManager(Options{
				Bind: netip.MustParseAddr("127.0.0.1"), MinPort: 10000, MaxPort: 10010,
				Routes: routes, SourceLimiter: test.sourceLimiter,
				ReportRuntimeError: func(err error) {
					runtimeCalls.Add(1)
					runtimeErrors <- err
				},
				listen: factory.listen, retryInterval: time.Hour,
			})
			if err != nil {
				t.Fatalf("NewManager() error = %v", err)
			}
			if err := manager.Start(context.Background()); err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			t.Cleanup(func() { _ = manager.Close() })
			listener := factory.listener("127.0.0.1:10000")
			if test.rejectAdmission {
				manager.mu.Lock()
				managed := manager.actual[10000]
				if managed != nil {
					manager.admissionMu.Lock()
					managed.accepting = false
					manager.admissionMu.Unlock()
				}
				manager.mu.Unlock()
				if managed == nil {
					t.Fatal("managed Listener was not published")
				}
			}

			closeFailure := errors.New("injected connection close failure")
			serverConnection, peer := net.Pipe()
			defer serverConnection.Close()
			defer peer.Close()
			failingConnection := &closeErrorConn{Conn: serverConnection, err: closeFailure}
			var acceptedConnection net.Conn = failingConnection
			if test.remote != nil {
				acceptedConnection = &remoteAddrConn{Conn: failingConnection, remote: test.remote}
			}
			listener.acceptConnection(acceptedConnection)

			waitTCPCondition(t, func() bool {
				manager.errorMu.Lock()
				defer manager.errorMu.Unlock()
				return errors.Is(manager.connectionErr, closeFailure)
			})
			if calls := failingConnection.closeCallCount(); calls != 1 {
				t.Fatalf("connection Close calls = %d, want 1", calls)
			}
			if calls := runtimeCalls.Load(); calls != 0 {
				t.Fatalf("ReportRuntimeError calls = %d, want 0", calls)
			}
			select {
			case runtimeErr := <-runtimeErrors:
				t.Fatalf("ordinary rejected-connection Close error reached fatal runtime owner: %v", runtimeErr)
			default:
			}
			if err := manager.Close(); !errors.Is(err, closeFailure) {
				t.Fatalf("Close() error = %v, want recorded connection Close failure", err)
			}
		})
	}
}

func TestManagerRetainsHandledConnectionCloseFailureWithoutFatalReport(t *testing.T) {
	routes, _, cancelRoutes := startTCPRouteManager(
		t, tcpRouteState(1, "tcp-one", tcpIngressServiceID, 10000),
	)
	defer cancelRoutes()
	factory := newFakeListenerFactory()
	runtimeErrors := make(chan error, 1)
	var runtimeCalls atomic.Int32
	manager, err := NewManager(Options{
		Bind: netip.MustParseAddr("127.0.0.1"), MinPort: 10000, MaxPort: 10010,
		Routes: routes,
		ReportRuntimeError: func(err error) {
			runtimeCalls.Add(1)
			runtimeErrors <- err
		},
		listen: factory.listen, retryInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	closeFailure := errors.New("injected handled connection close failure")
	serverConnection, peer := net.Pipe()
	defer peer.Close()
	accepted := &closeAndErrorConn{Conn: serverConnection, err: closeFailure}
	factory.listener("127.0.0.1:10000").acceptConnection(accepted)
	waitTCPCondition(t, func() bool {
		manager.errorMu.Lock()
		defer manager.errorMu.Unlock()
		return errors.Is(manager.connectionErr, closeFailure)
	})
	if calls := accepted.closeCallCount(); calls != 1 {
		t.Fatalf("connection Close calls = %d, want 1", calls)
	}
	if calls := runtimeCalls.Load(); calls != 0 {
		t.Fatalf("ReportRuntimeError calls = %d, want 0", calls)
	}
	select {
	case runtimeErr := <-runtimeErrors:
		t.Fatalf("ordinary handled-connection Close error reached fatal runtime owner: %v", runtimeErr)
	default:
	}
	if err := manager.Close(); !errors.Is(err, closeFailure) {
		t.Fatalf("Close() error = %v, want recorded connection Close failure", err)
	}
}

func TestManagerHandlerPanicClosesConnectionBeforeReleasingOwnership(t *testing.T) {
	routes, _, cancelRoutes := startTCPRouteManager(
		t, tcpRouteState(1, "tcp-one", tcpIngressServiceID, 10000),
	)
	defer cancelRoutes()
	factory := newFakeListenerFactory()
	runtimeErrors := make(chan error, 1)
	manager, err := NewManager(Options{
		Bind: netip.MustParseAddr("127.0.0.1"), MinPort: 10000, MaxPort: 10010,
		Routes: routes,
		Handler: func(context.Context, net.Conn, serverroute.TCPRoute) {
			panic("injected handler panic")
		},
		ReportRuntimeError: func(err error) { runtimeErrors <- err },
		listen:             factory.listen,
		retryInterval:      time.Hour,
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	serverConnection, peer := net.Pipe()
	defer peer.Close()
	listener := factory.listener("127.0.0.1:10000")
	listener.acceptConnection(serverConnection)

	select {
	case runtimeErr := <-runtimeErrors:
		if !errors.Is(runtimeErr, safego.ErrPanic) {
			t.Fatalf("runtime error = %v, want recovered Handler panic", runtimeErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Handler panic was not reported")
	}
	// safego 只在 Handler 栈上的 Close/release defer 完成后报告 panic，
	// 因此此处要么已无法设置 Deadline，要么读取立即观察到关闭。
	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("SetReadDeadline() after Handler panic error = %v", err)
		}
	} else if _, err := peer.Read(make([]byte, 1)); err == nil {
		t.Fatal("peer remained open after Handler panic")
	}
	manager.admissionMu.Lock()
	remaining := len(manager.connections)
	manager.admissionMu.Unlock()
	if remaining != 0 {
		t.Fatalf("owned connections after Handler panic = %d, want 0", remaining)
	}
	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
	defer cancelShutdown()
	if err := manager.Shutdown(shutdownContext); err != nil {
		t.Fatalf("Shutdown() after Handler panic error = %v", err)
	}
	if err := manager.Close(); !errors.Is(err, safego.ErrPanic) {
		t.Fatalf("Close() error = %v, want recovered Handler panic", err)
	}
}

func TestManagerHandlerPanicJoinsConnectionCloseFailure(t *testing.T) {
	routes, _, cancelRoutes := startTCPRouteManager(
		t, tcpRouteState(1, "tcp-one", tcpIngressServiceID, 10000),
	)
	defer cancelRoutes()
	factory := newFakeListenerFactory()
	runtimeErrors := make(chan error, 2)
	var runtimeCalls atomic.Int32
	manager, err := NewManager(Options{
		Bind: netip.MustParseAddr("127.0.0.1"), MinPort: 10000, MaxPort: 10010,
		Routes: routes,
		Handler: func(context.Context, net.Conn, serverroute.TCPRoute) {
			panic("injected handler panic")
		},
		ReportRuntimeError: func(err error) {
			runtimeCalls.Add(1)
			runtimeErrors <- err
		},
		listen: factory.listen, retryInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	closeFailure := errors.New("injected handler connection close failure")
	serverConnection, peer := net.Pipe()
	defer peer.Close()
	accepted := &closeAndErrorConn{Conn: serverConnection, err: closeFailure}
	factory.listener("127.0.0.1:10000").acceptConnection(accepted)

	select {
	case runtimeErr := <-runtimeErrors:
		if !errors.Is(runtimeErr, safego.ErrPanic) || !errors.Is(runtimeErr, closeFailure) {
			t.Fatalf("runtime error = %v, want joined Handler panic and Close error", runtimeErr)
		}
	case <-time.After(time.Second):
		t.Fatal("joined Handler panic and Close error were not reported")
	}
	if calls := accepted.closeCallCount(); calls != 1 {
		t.Fatalf("connection Close calls = %d, want 1", calls)
	}
	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("SetReadDeadline() after Handler panic error = %v", err)
		}
	} else if _, err := peer.Read(make([]byte, 1)); err == nil {
		t.Fatal("peer remained open after Handler panic and Close error")
	}
	manager.admissionMu.Lock()
	remaining := len(manager.connections)
	manager.admissionMu.Unlock()
	if remaining != 0 {
		t.Fatalf("owned connections after Handler panic = %d, want 0", remaining)
	}
	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
	defer cancelShutdown()
	if err := manager.Shutdown(shutdownContext); err != nil {
		t.Fatalf("Shutdown() after Handler panic error = %v", err)
	}
	if calls := runtimeCalls.Load(); calls != 1 {
		t.Fatalf("ReportRuntimeError calls = %d, want 1", calls)
	}
	select {
	case runtimeErr := <-runtimeErrors:
		t.Fatalf("unexpected second runtime error: %v", runtimeErr)
	default:
	}
	if err := manager.Close(); !errors.Is(err, safego.ErrPanic) || !errors.Is(err, closeFailure) {
		t.Fatalf("Close() error = %v, want joined Handler panic and Close error", err)
	}
}

func TestManagerShutdownDeadlineCancelsHandlerContext(t *testing.T) {
	routes, _, cancelRoutes := startTCPRouteManager(
		t, tcpRouteState(1, "tcp-one", tcpIngressServiceID, 10000),
	)
	defer cancelRoutes()
	factory := newFakeListenerFactory()
	started := make(chan struct{})
	manager, err := NewManager(Options{
		Bind: netip.MustParseAddr("127.0.0.1"), MinPort: 10000, MaxPort: 10010,
		Routes: routes, Handler: func(ctx context.Context, _ net.Conn, _ serverroute.TCPRoute) {
			close(started)
			<-ctx.Done()
		},
		listen: factory.listen, retryInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	serverConnection, peer := net.Pipe()
	defer peer.Close()
	factory.listener("127.0.0.1:10000").acceptConnection(serverConnection)
	<-started
	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelShutdown()
	if err := manager.Shutdown(shutdownContext); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown() error = %v, want context deadline exceeded", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestManagerRetainsCloseFailureForFinalRetry(t *testing.T) {
	routes, source, cancelRoutes := startTCPRouteManager(
		t, tcpRouteState(1, "tcp-one", tcpIngressServiceID, 10000),
	)
	defer cancelRoutes()
	factory := newFakeListenerFactory()
	runtimeErrors := make(chan error, 1)
	manager, err := NewManager(Options{
		Bind: netip.MustParseAddr("127.0.0.1"), MinPort: 10000, MaxPort: 10010,
		Routes: routes, ReportRuntimeError: func(err error) { runtimeErrors <- err },
		listen: factory.listen, retryInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	listener := factory.listener("127.0.0.1:10000")
	listener.failClose(2)
	deleted := tcpRouteState(2, "tcp-one", tcpIngressServiceID, 10000)
	deleted.TCPRoutes = nil
	publishTCPRouteState(t, routes, source, deleted)
	manager.MarkDirty(2)
	select {
	case err := <-runtimeErrors:
		closeResult := make(chan error, 1)
		go func() { closeResult <- manager.Close() }()
		var closeErr error
		select {
		case closeErr = <-closeResult:
		case <-time.After(time.Second):
			t.Fatal("Close() blocked after repeated Listener Close failures")
		}
		if err == nil || closeErr == nil {
			t.Fatalf("runtime/Close errors = %v/%v, want both non-nil", err, closeErr)
		}
	case <-time.After(time.Second):
		t.Fatal("close failure was not reported to runtime owner")
	}
	if !listener.closedState() {
		t.Fatal("final Close did not retry residual Listener")
	}
}

func TestManagerUnexpectedAcceptErrorBecomesPerRouteFailureAndRetries(t *testing.T) {
	routes, _, cancelRoutes := startTCPRouteManager(
		t, tcpRouteState(1, "tcp-one", tcpIngressServiceID, 10000),
	)
	defer cancelRoutes()
	factory := newFakeListenerFactory()
	runtimeErrors := make(chan error, 1)
	manager, err := NewManager(Options{
		Bind: netip.MustParseAddr("127.0.0.1"), MinPort: 10000, MaxPort: 10010,
		Routes: routes, ReportRuntimeError: func(err error) { runtimeErrors <- err },
		listen: factory.listen, retryInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	failed := factory.listener("127.0.0.1:10000")
	factory.setFailure("127.0.0.1:10000", true)
	failed.failAccept(errors.New("injected accept failure"))
	waitTCPCondition(t, func() bool {
		failures := manager.ApplyFailures()
		return len(manager.Actual()) == 0 && len(failures) == 1 &&
			failures[0].RouteID == "tcp-one" && failures[0].ErrorCode == ListenFailedErrorCode &&
			failed.closedState()
	})
	select {
	case err := <-runtimeErrors:
		t.Fatalf("per-route Accept failure reached fatal runtime owner: %v", err)
	default:
	}
	factory.setFailure("127.0.0.1:10000", false)
	manager.MarkDirty(1)
	waitTCPCondition(t, func() bool {
		return len(manager.Actual()) == 1 && len(manager.ApplyFailures()) == 0
	})
}

func TestManagerLoopbackAcceptSmoke(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen loopback error = %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listenerOwned := false
	t.Cleanup(func() {
		if !listenerOwned {
			_ = listener.Close()
		}
	})
	routes, _, cancelRoutes := startTCPRouteManager(
		t, tcpRouteState(1, "tcp-one", tcpIngressServiceID, uint16(port)),
	)
	defer cancelRoutes()
	manager, err := NewManager(Options{
		Bind: netip.MustParseAddr("127.0.0.1"), MinPort: port, MaxPort: port,
		Routes: routes, Handler: func(_ context.Context, connection net.Conn, _ serverroute.TCPRoute) {
			_, _ = io.Copy(connection, connection)
		},
		listen: func(_ context.Context, network, address string) (net.Listener, error) {
			if network != "tcp4" || address != listener.Addr().String() {
				return nil, errors.New("unexpected loopback listen request")
			}
			listenerOwned = true
			return listener, nil
		},
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer manager.Close()
	connection, err := net.DialTimeout("tcp4", manager.Actual()[0].Address, time.Second)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte("ping")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	buffer := make([]byte, 4)
	if _, err := io.ReadFull(connection, buffer); err != nil || string(buffer) != "ping" {
		t.Fatalf("ReadFull() = %q, %v", buffer, err)
	}
}

func TestManagerRestoresTCPListenersFromReopenedSQLiteDesiredState(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := repositorysqlite.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	if err := store.WithTx(ctx, func(transaction repository.TxStore) error {
		if err := transaction.Tunnels().Create(ctx, repository.Tunnel{
			ID: tcpIngressTunnelID, Name: "tcp", Version: 1, DesiredRevision: 1,
			CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			return err
		}
		if err := transaction.Services().Create(ctx, validTCPIngressService(tcpIngressServiceID, 1)); err != nil {
			return err
		}
		if err := transaction.Routes().CreateTCP(ctx, repository.TCPRoute{
			ID: "tcp-one", ServiceID: tcpIngressServiceID, PublicPort: 10000,
			Enabled: true, CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			return err
		}
		_, err := transaction.Routes().AdvanceGeneration(ctx, 0)
		return err
	}); err != nil {
		_ = store.Close()
		t.Fatalf("seed SQLite TCP Desired State error = %v", err)
	}

	startAndStop := func(label string, source *repositorysqlite.Store) {
		routes, err := serverroute.NewManager(source)
		if err != nil {
			t.Fatalf("%s route.NewManager() error = %v", label, err)
		}
		routeContext, cancelRoutes := context.WithCancel(context.Background())
		if err := routes.Start(routeContext); err != nil {
			cancelRoutes()
			t.Fatalf("%s Route Start() error = %v", label, err)
		}
		factory := newFakeListenerFactory()
		manager := newTestManager(t, routes, factory)
		if err := manager.Start(context.Background()); err != nil {
			cancelRoutes()
			routes.Wait()
			t.Fatalf("%s TCP Start() error = %v", label, err)
		}
		actual := manager.Actual()
		if len(actual) != 1 || actual[0].Route.ID != "tcp-one" || actual[0].Route.PublicPort != 10000 {
			t.Fatalf("%s restored Actual = %+v", label, actual)
		}
		if err := manager.Close(); err != nil {
			t.Fatalf("%s TCP Close() error = %v", label, err)
		}
		cancelRoutes()
		routes.Wait()
	}

	startAndStop("first start", store)
	if err := store.Close(); err != nil {
		t.Fatalf("close first SQLite store error = %v", err)
	}
	reopened, err := repositorysqlite.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("reopen SQLite error = %v", err)
	}
	defer reopened.Close()
	startAndStop("restart", reopened)
}

func newTestManager(t *testing.T, routes *serverroute.Manager, factory *fakeListenerFactory) *Manager {
	t.Helper()
	manager, err := NewManager(Options{
		Bind: netip.MustParseAddr("127.0.0.1"), MinPort: 10000, MaxPort: 10010,
		Reserved: []uint16{10001}, Routes: routes,
		listen: factory.listen, retryInterval: time.Hour,
		now: func() time.Time { return time.Unix(200, 0) },
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	return manager
}

type mutableTCPRouteSource struct {
	mu    sync.Mutex
	state repository.RouteDesiredState
}

func (source *mutableTCPRouteSource) LoadRouteDesiredState(context.Context) (repository.RouteDesiredState, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	state := source.state
	state.Tunnels = append([]repository.Tunnel(nil), state.Tunnels...)
	state.Services = append([]repository.Service(nil), state.Services...)
	state.TCPRoutes = append([]repository.TCPRoute(nil), state.TCPRoutes...)
	return state, nil
}

func (source *mutableTCPRouteSource) CurrentRouteGeneration(context.Context) (uint64, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.state.Generation, nil
}

func (source *mutableTCPRouteSource) replace(state repository.RouteDesiredState) {
	source.mu.Lock()
	source.state = state
	source.mu.Unlock()
}

func startTCPRouteManager(
	t *testing.T,
	state repository.RouteDesiredState,
) (*serverroute.Manager, *mutableTCPRouteSource, context.CancelFunc) {
	t.Helper()
	source := &mutableTCPRouteSource{state: state}
	routes, err := serverroute.NewManager(source)
	if err != nil {
		t.Fatalf("route.NewManager() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := routes.Start(ctx); err != nil {
		cancel()
		t.Fatalf("route Manager Start() error = %v", err)
	}
	return routes, source, func() {
		cancel()
		routes.Wait()
	}
}

func publishTCPRouteState(
	t *testing.T,
	routes *serverroute.Manager,
	source *mutableTCPRouteSource,
	state repository.RouteDesiredState,
) {
	t.Helper()
	source.replace(state)
	routes.MarkDirty(state.Generation)
	waitTCPCondition(t, func() bool {
		current := routes.Current()
		return current != nil && current.Generation() == state.Generation
	})
}

func tcpRouteState(generation uint64, routeID, serviceID string, port uint16) repository.RouteDesiredState {
	return repository.RouteDesiredState{
		Generation: generation,
		Tunnels: []repository.Tunnel{{
			ID: tcpIngressTunnelID, Name: "tcp", Version: 1, DesiredRevision: int64(generation),
			CreatedAt: 1, UpdatedAt: 1,
		}},
		Services: []repository.Service{validTCPIngressService(serviceID, int64(generation))},
		TCPRoutes: []repository.TCPRoute{{
			ID: routeID, ServiceID: serviceID, PublicPort: port, Enabled: true, CreatedAt: 1, UpdatedAt: 1,
		}},
	}
}

func validTCPIngressService(id string, revision int64) repository.Service {
	return repository.Service{
		ID: id, TunnelID: tcpIngressTunnelID, Name: id, RequiredRevision: revision,
		OriginScheme: repository.OriginSchemeTCP, OriginHost: "127.0.0.1", OriginPort: 22,
		ConnectTimeoutMS: 5_000, Enabled: true, Version: 1, CreatedAt: 1, UpdatedAt: 1,
	}
}

func waitTCPCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("condition did not become true before deadline")
		case <-ticker.C:
		}
	}
}

type fakeListenCall struct {
	network string
	address string
}

type fakeListenerFactory struct {
	mu        sync.Mutex
	failures  map[string]bool
	blocks    map[string]chan struct{}
	listeners map[string]*fakeListener
	calls     []fakeListenCall
}

func newFakeListenerFactory() *fakeListenerFactory {
	return &fakeListenerFactory{
		failures: make(map[string]bool), blocks: make(map[string]chan struct{}),
		listeners: make(map[string]*fakeListener),
	}
}

func (factory *fakeListenerFactory) listen(ctx context.Context, network, address string) (net.Listener, error) {
	factory.mu.Lock()
	factory.calls = append(factory.calls, fakeListenCall{network: network, address: address})
	failed := factory.failures[address]
	block := factory.blocks[address]
	factory.mu.Unlock()
	if block != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-block:
		}
	}
	if failed {
		return nil, errors.New("injected listen failure")
	}
	listener := newFakeListener(address)
	factory.mu.Lock()
	factory.listeners[address] = listener
	factory.mu.Unlock()
	return listener, nil
}

func (factory *fakeListenerFactory) setFailure(address string, failed bool) {
	factory.mu.Lock()
	factory.failures[address] = failed
	factory.mu.Unlock()
}

func (factory *fakeListenerFactory) block(address string) {
	factory.mu.Lock()
	factory.blocks[address] = make(chan struct{})
	factory.mu.Unlock()
}

func (factory *fakeListenerFactory) release(address string) {
	factory.mu.Lock()
	block := factory.blocks[address]
	delete(factory.blocks, address)
	factory.mu.Unlock()
	if block != nil {
		close(block)
	}
}

func (factory *fakeListenerFactory) listener(address string) *fakeListener {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	return factory.listeners[address]
}

func (factory *fakeListenerFactory) callsSnapshot() []fakeListenCall {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	return append([]fakeListenCall(nil), factory.calls...)
}

type fakeListener struct {
	address      fakeAddr
	closed       chan struct{}
	connections  chan net.Conn
	acceptErrors chan error
	deadline     chan struct{}
	once         sync.Once
	deadlineOnce sync.Once
	closeMu      sync.Mutex
	closeErrors  int
}

func newFakeListener(address string) *fakeListener {
	return &fakeListener{
		address: fakeAddr(address), closed: make(chan struct{}), connections: make(chan net.Conn, 8),
		acceptErrors: make(chan error, 1), deadline: make(chan struct{}),
	}
}

func (listener *fakeListener) Accept() (net.Conn, error) {
	select {
	case <-listener.closed:
		return nil, net.ErrClosed
	case connection := <-listener.connections:
		return connection, nil
	case err := <-listener.acceptErrors:
		return nil, err
	case <-listener.deadline:
		return nil, os.ErrDeadlineExceeded
	}
}

func (listener *fakeListener) Close() error {
	listener.closeMu.Lock()
	if listener.closeErrors > 0 {
		listener.closeErrors--
		listener.closeMu.Unlock()
		return errors.New("injected close failure")
	}
	listener.closeMu.Unlock()
	listener.once.Do(func() { close(listener.closed) })
	return nil
}

func (listener *fakeListener) Addr() net.Addr { return listener.address }

func (listener *fakeListener) SetDeadline(time.Time) error {
	listener.deadlineOnce.Do(func() { close(listener.deadline) })
	return nil
}

func (listener *fakeListener) closedState() bool {
	select {
	case <-listener.closed:
		return true
	default:
		return false
	}
}

func (listener *fakeListener) acceptConnection(connection net.Conn) {
	listener.connections <- connection
}

func (listener *fakeListener) failClose(count int) {
	listener.closeMu.Lock()
	listener.closeErrors = count
	listener.closeMu.Unlock()
}

func (listener *fakeListener) failAccept(err error) {
	listener.acceptErrors <- err
}

type fakeAddr string

func (address fakeAddr) Network() string { return "tcp" }
func (address fakeAddr) String() string  { return string(address) }

type remoteAddrConn struct {
	net.Conn
	remote net.Addr
}

func (connection *remoteAddrConn) RemoteAddr() net.Addr { return connection.remote }

type closeErrorConn struct {
	net.Conn
	mu    sync.Mutex
	err   error
	calls int
}

func (connection *closeErrorConn) Close() error {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	connection.calls++
	return connection.err
}

func (connection *closeErrorConn) closeCallCount() int {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return connection.calls
}

type closeAndErrorConn struct {
	net.Conn
	mu    sync.Mutex
	err   error
	calls int
}

func (connection *closeAndErrorConn) Close() error {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	connection.calls++
	return errors.Join(connection.Conn.Close(), connection.err)
}

func (connection *closeAndErrorConn) closeCallCount() int {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return connection.calls
}

type recordingSourceLimiter struct {
	mu       sync.Mutex
	rejected netip.Addr
	calls    []netip.Addr
}

func (limiter *recordingSourceLimiter) AllowOpen(sourceIP netip.Addr) error {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	limiter.calls = append(limiter.calls, sourceIP)
	if sourceIP == limiter.rejected {
		return errors.New("injected source OPEN rejection")
	}
	return nil
}

func (limiter *recordingSourceLimiter) callsSnapshot() []netip.Addr {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	return append([]netip.Addr(nil), limiter.calls...)
}

func assertPeerClosed(t *testing.T, connection net.Conn) {
	t.Helper()
	defer connection.Close()
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("SetReadDeadline() error = %v", err)
	}
	buffer := make([]byte, 1)
	if _, err := connection.Read(buffer); err == nil {
		t.Fatal("peer remained open")
	}
}
