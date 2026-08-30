package httpingress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/identity"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/repository"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
	"github.com/lifei6671/xtunnel/internal/server/route"
	"github.com/lifei6671/xtunnel/internal/tunnel"
)

const (
	testTunnelID   = "tun_01J00000000000000000000000"
	testServiceID  = "svc_01J00000000000000000000000"
	testServiceID2 = "svc_01J00000000000000000000001"
)

type memoryRouteSource struct {
	mu    sync.RWMutex
	state repository.RouteDesiredState
}

func (source *memoryRouteSource) LoadRouteDesiredState(ctx context.Context) (repository.RouteDesiredState, error) {
	if err := ctx.Err(); err != nil {
		return repository.RouteDesiredState{}, err
	}
	source.mu.RLock()
	defer source.mu.RUnlock()
	return cloneRouteState(source.state), nil
}

func (source *memoryRouteSource) CurrentRouteGeneration(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	source.mu.RLock()
	defer source.mu.RUnlock()
	return source.state.Generation, nil
}

func (source *memoryRouteSource) replace(state repository.RouteDesiredState) {
	source.mu.Lock()
	source.state = cloneRouteState(state)
	source.mu.Unlock()
}

func cloneRouteState(state repository.RouteDesiredState) repository.RouteDesiredState {
	cloned := state
	cloned.Tunnels = append([]repository.Tunnel(nil), state.Tunnels...)
	cloned.Services = append([]repository.Service(nil), state.Services...)
	cloned.HTTPRoutes = append([]repository.HTTPRoute(nil), state.HTTPRoutes...)
	cloned.TCPRoutes = append([]repository.TCPRoute(nil), state.TCPRoutes...)
	return cloned
}

func startRouteManager(t *testing.T, state repository.RouteDesiredState) (*route.Manager, *memoryRouteSource) {
	t.Helper()
	source := &memoryRouteSource{state: cloneRouteState(state)}
	manager, err := route.NewManager(source)
	if err != nil {
		t.Fatalf("route.NewManager() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := manager.Start(ctx); err != nil {
		cancel()
		t.Fatalf("route Manager.Start() error = %v", err)
	}
	t.Cleanup(func() {
		cancel()
		manager.Wait()
	})
	return manager, source
}

func publishRouteState(t *testing.T, manager *route.Manager, source *memoryRouteSource, state repository.RouteDesiredState) {
	t.Helper()
	source.replace(state)
	manager.MarkDirty(state.Generation)
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if current := manager.Current(); current != nil && current.Generation() == state.Generation {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("route generation did not advance to %d", state.Generation)
		case <-ticker.C:
		}
	}
}

func baseHTTPRouteState(generation uint64) repository.RouteDesiredState {
	return repository.RouteDesiredState{
		Generation: generation,
		Tunnels: []repository.Tunnel{{
			ID: testTunnelID, Name: "office", Version: 1, DesiredRevision: 3,
			CreatedAt: 1, UpdatedAt: 1,
		}},
		Services: []repository.Service{{
			ID: testServiceID, TunnelID: testTunnelID, Name: "web", RequiredRevision: 1,
			OriginScheme: repository.OriginSchemeHTTP, OriginHost: "origin.internal", OriginPort: 8080,
			ConnectTimeoutMS: 5_000, Enabled: true, Version: 1, CreatedAt: 1, UpdatedAt: 1,
		}},
		HTTPRoutes: []repository.HTTPRoute{{
			ID: "http-main", ServiceID: testServiceID, Hostname: "public.example.com", PathPrefix: "/",
			PreserveHost: true, Enabled: true, CreatedAt: 1, UpdatedAt: 1,
		}},
	}
}

type tunnelDialCall struct {
	TunnelID         string
	ServiceID        string
	RequiredRevision uint64
	Ingress          protocolv1.IngressType
	Client           string
	RequestID        string
	ConnectionID     string
}

type dialerFunc func(context.Context, tunnel.DialRequest) (net.Conn, error)

func (dial dialerFunc) Dial(ctx context.Context, request tunnel.DialRequest) (net.Conn, error) {
	return dial(ctx, request)
}

type observedOriginRequest struct {
	ConnectionIndex  int
	Host             string
	RequestURI       string
	Path             string
	RawPath          string
	RawQuery         string
	ForceQuery       bool
	ContentLength    int64
	TransferEncoding []string
	Body             string
	Header           http.Header
}

// loopOriginDialer 在每次 Tunnel Dial 后启动一个真实 HTTP/1.1 peer，并让连接持续服务到
// Handler 关闭 idle transport。连接编号与 Dial 参数共同用于证明 KeepAlive 没有跨池复用。
type loopOriginDialer struct {
	mu       sync.Mutex
	calls    []tunnelDialCall
	peers    []net.Conn
	servers  []net.Conn
	observed chan observedOriginRequest
	wait     sync.WaitGroup
	closing  atomic.Bool
}

func newLoopOriginDialer(t *testing.T) *loopOriginDialer {
	t.Helper()
	dialer := &loopOriginDialer{observed: make(chan observedOriginRequest, 64)}
	t.Cleanup(func() { dialer.close(t) })
	return dialer
}

func (dialer *loopOriginDialer) Dial(
	ctx context.Context,
	request tunnel.DialRequest,
) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	server, peer := net.Pipe()
	connectionID, err := identity.NewConnectionID()
	if err != nil {
		_ = server.Close()
		_ = peer.Close()
		return nil, err
	}
	dialer.mu.Lock()
	index := len(dialer.calls) + 1
	dialer.calls = append(dialer.calls, tunnelDialCall{
		TunnelID: request.TunnelID, ServiceID: request.ServiceID,
		RequiredRevision: request.RequiredRevision,
		Ingress:          request.Ingress, Client: request.ClientAddr,
		RequestID: request.RequestID, ConnectionID: connectionID,
	})
	dialer.servers = append(dialer.servers, server)
	dialer.peers = append(dialer.peers, peer)
	dialer.mu.Unlock()

	dialer.wait.Add(1)
	go func() {
		defer dialer.wait.Done()
		defer peer.Close()
		reader := bufio.NewReader(peer)
		for {
			request, err := http.ReadRequest(reader)
			if err != nil {
				return
			}
			body, err := io.ReadAll(request.Body)
			closeErr := request.Body.Close()
			if err != nil || closeErr != nil {
				return
			}
			dialer.observed <- observedOriginRequest{
				ConnectionIndex: index,
				Host:            request.Host, RequestURI: request.RequestURI,
				Path: request.URL.Path, RawPath: request.URL.RawPath,
				RawQuery: request.URL.RawQuery, ForceQuery: request.URL.ForceQuery,
				ContentLength:    request.ContentLength,
				TransferEncoding: append([]string(nil), request.TransferEncoding...),
				Body:             string(body),
				Header:           request.Header.Clone(),
			}
			responseBody := fmt.Sprintf("connection-%d", index)
			response := &http.Response{
				StatusCode: http.StatusOK, ProtoMajor: 1, ProtoMinor: 1,
				Header: make(http.Header), Body: io.NopCloser(strings.NewReader(responseBody)),
				ContentLength: int64(len(responseBody)),
			}
			if err := response.Write(peer); err != nil {
				return
			}
		}
	}()
	return &identifiedTestConnection{Conn: server, id: connectionID}, nil
}

type identifiedTestConnection struct {
	net.Conn
	id string
}

func (connection *identifiedTestConnection) ConnectionID() string { return connection.id }

func (dialer *loopOriginDialer) Calls() []tunnelDialCall {
	dialer.mu.Lock()
	defer dialer.mu.Unlock()
	return append([]tunnelDialCall(nil), dialer.calls...)
}

func (dialer *loopOriginDialer) close(t *testing.T) {
	t.Helper()
	if !dialer.closing.CompareAndSwap(false, true) {
		return
	}
	dialer.mu.Lock()
	connections := append(append([]net.Conn(nil), dialer.servers...), dialer.peers...)
	dialer.mu.Unlock()
	for _, connection := range connections {
		if err := connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("close test tunnel connection: %v", err)
		}
	}
	done := make(chan struct{})
	go func() {
		dialer.wait.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("test origin goroutines did not exit")
	}
}

func waitOriginRequest(t *testing.T, dialer *loopOriginDialer) observedOriginRequest {
	t.Helper()
	select {
	case request := <-dialer.observed:
		return request
	case <-time.After(2 * time.Second):
		t.Fatal("origin did not receive request")
		return observedOriginRequest{}
	}
}

func newTestHandler(t *testing.T, manager *route.Manager, dialer any) *Handler {
	return newTestHandlerWithTrustedProxies(t, manager, dialer, []string{"127.0.0.1/32", "::1/128"})
}

func newTestHandlerWithTrustedProxies(
	t *testing.T,
	manager *route.Manager,
	dialer any,
	trustedProxies []string,
) *Handler {
	t.Helper()
	tunnelDialer, ok := dialer.(interface {
		Dial(context.Context, tunnel.DialRequest) (net.Conn, error)
	})
	if !ok {
		t.Fatal("test dialer does not implement the expected Tunnel Dial contract")
	}
	limitManager := newTestLimitManager(t, serverlimits.Options{
		MaxConnectors: 1_024, MaxConnectorsPerTunnel: 1_024,
		MaxWorkConnections: 1_024, MaxIdleWorkConnections: 1_024,
		MaxConnectingWorkConnections: 1_024, MaxPendingOpens: 1_024,
		MaxActiveConnections: 1_024, MaxConnectionsPerTunnel: 1_024,
		MaxConnectionsPerService: 1_024, MaxConnectionsPerSourceIP: 1_024,
		MaxOpenRatePerSourceIP: 100_000, MaxOpenBurstPerSourceIP: 100_000,
		MaxHTTPRequestsPerSourceIPPerSecond: 100_000,
	})
	return newTestHandlerWithLimits(t, manager, tunnelDialer, trustedProxies, limitManager, 2<<30)
}

func newTestHandlerWithLimits(
	t *testing.T,
	manager *route.Manager,
	dialer TunnelDialer,
	trustedProxies []string,
	limitManager *serverlimits.Manager,
	maxBodyBytes int64,
) *Handler {
	return newTestHandlerWithLimitsAndLogger(
		t, manager, dialer, trustedProxies, limitManager, maxBodyBytes,
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
	)
}

func newTestHandlerWithLimitsAndLogger(
	t *testing.T,
	manager *route.Manager,
	dialer TunnelDialer,
	trustedProxies []string,
	limitManager *serverlimits.Manager,
	maxBodyBytes int64,
	logger *slog.Logger,
) *Handler {
	t.Helper()
	handler, err := NewHandler(HandlerOptions{
		Routes: manager, Dialer: dialer, TrustedProxies: trustedProxies,
		Limits: limitManager, MaxBodyBytes: maxBodyBytes, Logger: logger,
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	t.Cleanup(func() {
		closer, ok := any(handler).(interface{ CloseIdleConnections() })
		if !ok {
			t.Error("Handler does not expose CloseIdleConnections")
			return
		}
		closer.CloseIdleConnections()
	})
	return handler
}

func newTestLimitManager(t *testing.T, options serverlimits.Options) *serverlimits.Manager {
	t.Helper()
	manager, err := serverlimits.New(options)
	if err != nil {
		t.Fatalf("limits.New() error = %v", err)
	}
	return manager
}
