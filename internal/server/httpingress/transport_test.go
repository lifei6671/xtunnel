package httpingress

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/repository"
	serverroute "github.com/lifei6671/xtunnel/internal/server/route"
)

func TestHandlerReusesKeepAliveOnlyWithinTunnelServiceRevisionPool(t *testing.T) {
	t.Run("same Tunnel Service and Revision reuses one WorkConn", func(t *testing.T) {
		manager, _ := startRouteManager(t, baseHTTPRouteState(1))
		origin := newLoopOriginDialer(t)
		handler := newTestHandler(t, manager, origin)

		firstBody := serveAndReadBody(t, handler, "public.example.com", "/first")
		firstObserved := waitOriginRequest(t, origin)
		secondBody := serveAndReadBody(t, handler, "public.example.com", "/second")
		secondObserved := waitOriginRequest(t, origin)

		if firstBody != "connection-1" || secondBody != "connection-1" ||
			firstObserved.ConnectionIndex != 1 || secondObserved.ConnectionIndex != 1 {
			t.Fatalf("responses/connection indices = (%q,%d) (%q,%d), want both on connection 1",
				firstBody, firstObserved.ConnectionIndex, secondBody, secondObserved.ConnectionIndex)
		}
		assertSingleHTTPDial(t, origin.Calls(), testServiceID)
	})

	t.Run("different Service never reuses idle WorkConn", func(t *testing.T) {
		state := baseHTTPRouteState(1)
		state.Services = append(state.Services, repository.Service{
			ID: testServiceID2, TunnelID: testTunnelID, Name: "web-two", RequiredRevision: 1,
			OriginScheme: repository.OriginSchemeHTTP, OriginHost: "origin-two.internal", OriginPort: 8081,
			ConnectTimeoutMS: 5_000, Enabled: true, Version: 1, CreatedAt: 1, UpdatedAt: 1,
		})
		state.HTTPRoutes[0].Hostname = "one.example.com"
		state.HTTPRoutes = append(state.HTTPRoutes, repository.HTTPRoute{
			ID: "http-two", ServiceID: testServiceID2, Hostname: "two.example.com", PathPrefix: "/",
			PreserveHost: true, Enabled: true, CreatedAt: 1, UpdatedAt: 1,
		})
		manager, _ := startRouteManager(t, state)
		origin := newLoopOriginDialer(t)
		handler := newTestHandler(t, manager, origin)

		firstBody := serveAndReadBody(t, handler, "one.example.com", "/first")
		firstObserved := waitOriginRequest(t, origin)
		secondBody := serveAndReadBody(t, handler, "two.example.com", "/second")
		secondObserved := waitOriginRequest(t, origin)

		if firstBody != "connection-1" || secondBody != "connection-2" ||
			firstObserved.ConnectionIndex != 1 || secondObserved.ConnectionIndex != 2 {
			t.Fatalf("responses/connection indices = (%q,%d) (%q,%d), want service-isolated connections 1 and 2",
				firstBody, firstObserved.ConnectionIndex, secondBody, secondObserved.ConnectionIndex)
		}
		calls := origin.Calls()
		if len(calls) != 2 || calls[0].ServiceID != testServiceID || calls[1].ServiceID != testServiceID2 {
			t.Fatalf("Dial calls = %+v, want services [%q %q]", calls, testServiceID, testServiceID2)
		}
	})

	t.Run("new RequiredRevision never reuses old idle WorkConn", func(t *testing.T) {
		state := baseHTTPRouteState(1)
		manager, source := startRouteManager(t, state)
		origin := newLoopOriginDialer(t)
		handler := newTestHandler(t, manager, origin)

		firstBody := serveAndReadBody(t, handler, "public.example.com", "/revision-1")
		firstObserved := waitOriginRequest(t, origin)
		state.Generation = 2
		state.Services[0].RequiredRevision = 2
		publishRouteState(t, manager, source, state)
		secondBody := serveAndReadBody(t, handler, "public.example.com", "/revision-2")
		secondObserved := waitOriginRequest(t, origin)

		if firstBody != "connection-1" || secondBody != "connection-2" ||
			firstObserved.ConnectionIndex != 1 || secondObserved.ConnectionIndex != 2 {
			t.Fatalf("responses/connection indices = (%q,%d) (%q,%d), want revision-isolated connections 1 and 2",
				firstBody, firstObserved.ConnectionIndex, secondBody, secondObserved.ConnectionIndex)
		}
		calls := origin.Calls()
		if len(calls) != 2 || calls[0].ServiceID != testServiceID || calls[0].RequiredRevision != 1 ||
			calls[1].ServiceID != testServiceID || calls[1].RequiredRevision != 2 {
			t.Fatalf("Dial calls = %+v, want the same Service at revisions 1 then 2", calls)
		}
	})
}

func TestTransportPoolGenerationFenceRejectsLateOldWriteback(t *testing.T) {
	state := baseHTTPRouteState(1)
	manager, source := startRouteManager(t, state)
	oldSnapshot := manager.Current()
	oldRoute := matchedHTTPRoute(t, oldSnapshot)
	origin := newLoopOriginDialer(t)
	dialer := &closeTrackingDialer{base: origin}
	pool := newTransportPool(dialer)
	t.Cleanup(pool.closeIdleConnections)

	// 旧请求先取得 generation=1 的不可变 Snapshot，但暂停在 Transport 池边界前。
	// 这正是旧请求可能晚于新代请求写池的交错窗口。
	oldReady := make(chan struct{})
	resumeOld := make(chan struct{})
	oldResult := make(chan transportRoundTripResult, 1)
	go func() {
		close(oldReady)
		<-resumeOld
		transport := pool.transport(oldSnapshot.Generation(), oldRoute)
		body, err := executeTransportRoundTrip(transport, "/old-late")
		oldResult <- transportRoundTripResult{transport: transport, body: body, err: err}
	}()
	<-oldReady

	state.Generation = 2
	state.Services[0].RequiredRevision = 2
	publishRouteState(t, manager, source, state)
	newSnapshot := manager.Current()
	newRoute := matchedHTTPRoute(t, newSnapshot)
	newTransport := pool.transport(newSnapshot.Generation(), newRoute)
	newConcrete, ok := newTransport.(*http.Transport)
	if !ok {
		t.Fatalf("generation 2 transport type = %T, want cached *http.Transport", newTransport)
	}
	newBody, err := executeTransportRoundTrip(newTransport, "/new-first")
	if err != nil || newBody != "connection-1" {
		t.Fatalf("first generation 2 RoundTrip = (%q, %v), want (connection-1, nil)", newBody, err)
	}
	if observed := waitOriginRequest(t, origin); observed.ConnectionIndex != 1 {
		t.Fatalf("first generation 2 connection = %d, want 1", observed.ConnectionIndex)
	}
	assertTrackedConnectionOpen(t, dialer, 0)

	close(resumeOld)
	var late transportRoundTripResult
	select {
	case late = <-oldResult:
	case <-time.After(2 * time.Second):
		t.Fatal("late generation 1 request did not finish")
	}
	if late.err != nil || late.body != "connection-2" {
		t.Fatalf("late generation 1 RoundTrip = (%q, %v), want (connection-2, nil)", late.body, late.err)
	}
	if _, ok := late.transport.(*uncachedTransport); !ok {
		t.Fatalf("late generation 1 transport type = %T, want uncachedTransport", late.transport)
	}
	if observed := waitOriginRequest(t, origin); observed.ConnectionIndex != 2 {
		t.Fatalf("late generation 1 connection = %d, want 2", observed.ConnectionIndex)
	}
	assertTrackedConnectionClosed(t, dialer, 1)
	assertTrackedConnectionOpen(t, dialer, 0)

	// 旧请求完成后，新请求必须继续复用 generation=2 的同一条 idle WorkConn；若旧
	// generation 曾回退或关闭当前池，这里会产生第三次 Dial。
	newAgain := pool.transport(newSnapshot.Generation(), newRoute)
	if newAgain != newTransport {
		t.Fatalf("generation 2 transport changed after late old request: first=%p again=%p", newConcrete, newAgain)
	}
	newBody, err = executeTransportRoundTrip(newAgain, "/new-second")
	if err != nil || newBody != "connection-1" {
		t.Fatalf("second generation 2 RoundTrip = (%q, %v), want (connection-1, nil)", newBody, err)
	}
	if observed := waitOriginRequest(t, origin); observed.ConnectionIndex != 1 {
		t.Fatalf("second generation 2 connection = %d, want reused connection 1", observed.ConnectionIndex)
	}
	assertTrackedConnectionOpen(t, dialer, 0)

	// 第二个低代请求必须再创建一次性 Transport；这证明旧代既没有写入当前池，
	// 也没有在池外留下可复用 idle 连接。
	staleAgain := pool.transport(oldSnapshot.Generation(), oldRoute)
	staleBody, err := executeTransportRoundTrip(staleAgain, "/old-again")
	if err != nil || staleBody != "connection-3" {
		t.Fatalf("second stale RoundTrip = (%q, %v), want (connection-3, nil)", staleBody, err)
	}
	if observed := waitOriginRequest(t, origin); observed.ConnectionIndex != 3 {
		t.Fatalf("second stale connection = %d, want fresh connection 3", observed.ConnectionIndex)
	}
	assertTrackedConnectionClosed(t, dialer, 2)

	newKey := transportKey{
		tunnelID: newRoute.TunnelID, serviceID: newRoute.ServiceID,
		requiredRevision: newRoute.RequiredRevision,
	}
	oldKey := transportKey{
		tunnelID: oldRoute.TunnelID, serviceID: oldRoute.ServiceID,
		requiredRevision: oldRoute.RequiredRevision,
	}
	pool.mu.Lock()
	generation := pool.generation
	initialized := pool.initialized
	cached := pool.transports[newKey]
	_, oldCached := pool.transports[oldKey]
	cacheSize := len(pool.transports)
	pool.mu.Unlock()
	if !initialized || generation != 2 || cacheSize != 1 || cached != newConcrete || oldCached {
		t.Fatalf("pool state = initialized=%v generation=%d size=%d new_cached=%v old_cached=%v, want true,2,1,true,false",
			initialized, generation, cacheSize, cached == newConcrete, oldCached)
	}
	if calls := origin.Calls(); len(calls) != 3 {
		t.Fatalf("Dial calls = %d, want 3 (new cached once plus two uncached stale requests)", len(calls))
	}
}

func serveAndReadBody(t *testing.T, handler http.Handler, host, path string) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Host = host
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	result := response.Result()
	defer func() {
		if err := result.Body.Close(); err != nil {
			t.Errorf("close handler response body: %v", err)
		}
	}()
	body, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatalf("read handler response body: %v", err)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("response status = %d, want 200; body=%q", result.StatusCode, body)
	}
	return string(body)
}

func matchedHTTPRoute(t *testing.T, snapshot *serverroute.Snapshot) serverroute.HTTPRoute {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Host = "public.example.com"
	match, found, err := snapshot.MatchHTTP(request)
	if err != nil || !found {
		t.Fatalf("Snapshot.MatchHTTP() = (%+v, %v, %v), want a route", match, found, err)
	}
	return match.Route
}

type transportRoundTripResult struct {
	transport http.RoundTripper
	body      string
	err       error
}

func executeTransportRoundTrip(transport http.RoundTripper, path string) (string, error) {
	request, err := http.NewRequest(http.MethodGet, "http://"+transportAuthority+path, nil)
	if err != nil {
		return "", fmt.Errorf("create transport request: %w", err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		return "", fmt.Errorf("round trip: %w", err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil {
		return "", fmt.Errorf("read response body: %w", readErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close response body: %w", closeErr)
	}
	return string(body), nil
}

type closeTrackingDialer struct {
	base *loopOriginDialer

	mu          sync.Mutex
	connections []*closeTrackingConn
}

func (dialer *closeTrackingDialer) Dial(
	ctx context.Context,
	tunnelID string,
	serviceID string,
	requiredRevision uint64,
	ingress protocolv1.IngressType,
	clientAddr string,
) (net.Conn, error) {
	connection, err := dialer.base.Dial(
		ctx, tunnelID, serviceID, requiredRevision, ingress, clientAddr,
	)
	if err != nil {
		return nil, err
	}
	tracked := &closeTrackingConn{Conn: connection, closed: make(chan struct{})}
	dialer.mu.Lock()
	dialer.connections = append(dialer.connections, tracked)
	dialer.mu.Unlock()
	return tracked, nil
}

func (dialer *closeTrackingDialer) connection(index int) *closeTrackingConn {
	dialer.mu.Lock()
	defer dialer.mu.Unlock()
	if index < 0 || index >= len(dialer.connections) {
		return nil
	}
	return dialer.connections[index]
}

type closeTrackingConn struct {
	net.Conn
	closed chan struct{}
	once   sync.Once
}

func (connection *closeTrackingConn) Close() error {
	err := connection.Conn.Close()
	connection.once.Do(func() { close(connection.closed) })
	return err
}

func assertTrackedConnectionClosed(t *testing.T, dialer *closeTrackingDialer, index int) {
	t.Helper()
	connection := dialer.connection(index)
	if connection == nil {
		t.Fatalf("tracked connection %d does not exist", index+1)
	}
	select {
	case <-connection.closed:
	case <-time.After(2 * time.Second):
		t.Fatalf("tracked connection %d remained open", index+1)
	}
}

func assertTrackedConnectionOpen(t *testing.T, dialer *closeTrackingDialer, index int) {
	t.Helper()
	connection := dialer.connection(index)
	if connection == nil {
		t.Fatalf("tracked connection %d does not exist", index+1)
	}
	select {
	case <-connection.closed:
		t.Fatalf("tracked connection %d was unexpectedly closed", index+1)
	default:
	}
}
