package httpingress

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/repository"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
	serveropen "github.com/lifei6671/xtunnel/internal/server/open"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
	serverworkpool "github.com/lifei6671/xtunnel/internal/server/workpool"
	"github.com/lifei6671/xtunnel/internal/tunnel"
)

func TestHandlerAppliesFrozenOriginHostPriority(t *testing.T) {
	tests := []struct {
		name           string
		originHTTPHost string
		preserveHost   bool
		originHost     string
		originPort     uint32
		wantHost       string
	}{
		{
			name: "explicit origin HTTP Host wins", originHTTPHost: "virtual.origin.internal:9443",
			preserveHost: true, originHost: "ignored.internal", originPort: 8080,
			wantHost: "virtual.origin.internal:9443",
		},
		{
			name: "preserve public Host when no explicit override", preserveHost: true,
			originHost: "ignored.internal", originPort: 8080,
			wantHost: "PUBLIC.Example.COM.:443",
		},
		{
			name: "fallback formats IPv6 origin authority", preserveHost: false,
			originHost: "2001:db8::20", originPort: 8080,
			wantHost: "[2001:db8::20]:8080",
		},
		{
			name: "fallback omits the default HTTP origin port", preserveHost: false,
			originHost: "origin.example", originPort: 80,
			wantHost: "origin.example",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := baseHTTPRouteState(1)
			state.Services[0].OriginHTTPHost = test.originHTTPHost
			state.Services[0].OriginHost = test.originHost
			state.Services[0].OriginPort = test.originPort
			state.HTTPRoutes[0].PreserveHost = test.preserveHost
			manager, _ := startRouteManager(t, state)
			origin := newLoopOriginDialer(t)
			handler := newTestHandler(t, manager, origin)

			request := httptest.NewRequest(http.MethodGet, "/resource", nil)
			request.Host = "PUBLIC.Example.COM.:443"
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != http.StatusOK || response.Body.String() != "connection-1" {
				t.Fatalf("response = (%d, %q), want (200, connection-1)", response.Code, response.Body.String())
			}
			observed := waitOriginRequest(t, origin)
			if observed.Host != test.wantHost {
				t.Fatalf("origin Host = %q, want %q", observed.Host, test.wantHost)
			}
			assertSingleHTTPDial(t, origin.Calls(), testServiceID)
		})
	}
}

func TestHandlerPreservesValidatedPathAndQueryRepresentation(t *testing.T) {
	state := baseHTTPRouteState(1)
	state.HTTPRoutes[0].PathPrefix = "/docs"
	manager, _ := startRouteManager(t, state)
	origin := newLoopOriginDialer(t)
	handler := newTestHandler(t, manager, origin)

	tests := []struct {
		name           string
		target         string
		wantRequestURI string
		wantPath       string
		wantRawPath    string
		wantRawQuery   string
		wantForceQuery bool
	}{
		{
			name: "RawPath repeated slash and RawQuery", target: "/docs/%7Ealice//note?x=1&x=2",
			wantRequestURI: "/docs/%7Ealice//note?x=1&x=2", wantPath: "/docs/~alice//note",
			wantRawPath: "/docs/%7Ealice//note", wantRawQuery: "x=1&x=2",
		},
		{
			name: "ForceQuery trailing question mark", target: "/docs?",
			wantRequestURI: "/docs?", wantPath: "/docs", wantForceQuery: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.target, nil)
			request.Host = "public.example.com"
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("response status = %d, want 200; body=%q", response.Code, response.Body.String())
			}
			observed := waitOriginRequest(t, origin)
			if observed.RequestURI != test.wantRequestURI || observed.Path != test.wantPath ||
				observed.RawPath != test.wantRawPath || observed.RawQuery != test.wantRawQuery ||
				observed.ForceQuery != test.wantForceQuery {
				t.Fatalf("origin URL = uri %q path %q raw_path %q raw_query %q force_query=%v, want uri %q path %q raw_path %q raw_query %q force_query=%v",
					observed.RequestURI, observed.Path, observed.RawPath, observed.RawQuery, observed.ForceQuery,
					test.wantRequestURI, test.wantPath, test.wantRawPath, test.wantRawQuery, test.wantForceQuery)
			}
		})
	}
}

func TestHandlerChunkedPolicy(t *testing.T) {
	t.Run("default policy streams unknown length as chunked", func(t *testing.T) {
		manager, _ := startRouteManager(t, baseHTTPRouteState(1))
		origin := newLoopOriginDialer(t)
		handler := newTestHandler(t, manager, origin)
		request := httptest.NewRequest(http.MethodPost, "/upload", io.NopCloser(strings.NewReader("chunked-body")))
		request.Host = "public.example.com"
		request.ContentLength = -1
		request.TransferEncoding = []string{"chunked"}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)

		if response.Code != http.StatusOK {
			t.Fatalf("response status = %d, want 200; body=%q", response.Code, response.Body.String())
		}
		observed := waitOriginRequest(t, origin)
		if observed.Body != "chunked-body" || observed.ContentLength != -1 ||
			len(observed.TransferEncoding) != 1 || observed.TransferEncoding[0] != "chunked" {
			t.Fatalf("origin request = body %q content_length=%d transfer_encoding=%v, want chunked unknown-length body",
				observed.Body, observed.ContentLength, observed.TransferEncoding)
		}
	})

	t.Run("disable chunked rejects unknown length before Dial", func(t *testing.T) {
		state := baseHTTPRouteState(1)
		state.Services[0].ProxyOptions = explicitHTTPProxyOptions(true)
		manager, _ := startRouteManager(t, state)
		var dialCount atomic.Int32
		dialer := dialerFunc(func(context.Context, tunnel.DialRequest) (net.Conn, error) {
			dialCount.Add(1)
			return nil, errors.New("Dial must not run for rejected chunked request")
		})
		handler := newTestHandler(t, manager, dialer)
		request := httptest.NewRequest(http.MethodPost, "/upload", io.NopCloser(strings.NewReader("unknown")))
		request.Host = "public.example.com"
		request.ContentLength = -1
		request.TransferEncoding = []string{"chunked"}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)

		if response.Code != http.StatusBadRequest {
			t.Fatalf("response status = %d, want 400; body=%q", response.Code, response.Body.String())
		}
		if got := dialCount.Load(); got != 0 {
			t.Fatalf("Dial count = %d, want 0", got)
		}
	})

	t.Run("disable chunked rejects non-empty Body with zero Content-Length", func(t *testing.T) {
		state := baseHTTPRouteState(1)
		state.Services[0].ProxyOptions = explicitHTTPProxyOptions(true)
		manager, _ := startRouteManager(t, state)
		var dialCount atomic.Int32
		dialer := dialerFunc(func(context.Context, tunnel.DialRequest) (net.Conn, error) {
			dialCount.Add(1)
			return nil, errors.New("Dial must not run for implicit chunked request")
		})
		handler := newTestHandler(t, manager, dialer)
		request := httptest.NewRequest(http.MethodPost, "/upload", nil)
		request.Host = "public.example.com"
		request.Body = io.NopCloser(strings.NewReader("implicit-chunked"))
		request.ContentLength = 0
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)

		if response.Code != http.StatusBadRequest || strings.TrimSpace(response.Body.String()) != "CONTENT_LENGTH_REQUIRED" {
			t.Fatalf("response = (%d, %q), want (400, CONTENT_LENGTH_REQUIRED)", response.Code, response.Body.String())
		}
		if got := dialCount.Load(); got != 0 {
			t.Fatalf("Dial count = %d, want 0", got)
		}
	})

	t.Run("disable chunked forwards known Content-Length", func(t *testing.T) {
		state := baseHTTPRouteState(1)
		state.Services[0].ProxyOptions = explicitHTTPProxyOptions(true)
		manager, _ := startRouteManager(t, state)
		origin := newLoopOriginDialer(t)
		handler := newTestHandler(t, manager, origin)
		body := []byte("known-length")
		request := httptest.NewRequest(http.MethodPost, "/upload", bytes.NewReader(body))
		request.Host = "public.example.com"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)

		if response.Code != http.StatusOK {
			t.Fatalf("response status = %d, want 200; body=%q", response.Code, response.Body.String())
		}
		observed := waitOriginRequest(t, origin)
		if observed.Body != string(body) || observed.ContentLength != int64(len(body)) || len(observed.TransferEncoding) != 0 {
			t.Fatalf("origin request = body %q content_length=%d transfer_encoding=%v, want fixed-length body",
				observed.Body, observed.ContentLength, observed.TransferEncoding)
		}
	})
}

func TestHandlerRejectsUnsupportedUpgradeBeforeTunnelDial(t *testing.T) {
	manager, _ := startRouteManager(t, baseHTTPRouteState(1))
	var dialCount atomic.Int32
	dialer := dialerFunc(func(context.Context, tunnel.DialRequest) (net.Conn, error) {
		dialCount.Add(1)
		return nil, errors.New("Dial must not run for unsupported Upgrade")
	})
	handler := newTestHandler(t, manager, dialer)
	request := httptest.NewRequest(http.MethodGet, "/websocket", nil)
	request.Host = "public.example.com"
	request.Header.Set("Connection", "keep-alive, UpGrAdE")
	request.Header.Set("Upgrade", "h2c")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNotImplemented || strings.TrimSpace(response.Body.String()) != "UPGRADE_NOT_SUPPORTED" {
		t.Fatalf("response = (%d, %q), want (501, UPGRADE_NOT_SUPPORTED)", response.Code, response.Body.String())
	}
	if got := dialCount.Load(); got != 0 {
		t.Fatalf("Dial count = %d, want 0", got)
	}
}

func TestHandlerRejectsMalformedWebSocketHandshakeBeforeTunnelDial(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*http.Request)
	}{
		{name: "HTTP 2", mutate: func(request *http.Request) {
			request.Proto = "HTTP/2.0"
			request.ProtoMajor = 2
			request.ProtoMinor = 0
		}},
		{name: "non GET", mutate: func(request *http.Request) { request.Method = http.MethodPost }},
		{name: "missing Connection token", mutate: func(request *http.Request) {
			request.Header.Set("Connection", "keep-alive")
		}},
		{name: "ambiguous Upgrade", mutate: func(request *http.Request) {
			request.Header.Set("Upgrade", "websocket, h2c")
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, _ := startRouteManager(t, baseHTTPRouteState(1))
			var dialCount atomic.Int32
			dialer := dialerFunc(func(context.Context, tunnel.DialRequest) (net.Conn, error) {
				dialCount.Add(1)
				return nil, errors.New("Dial must not run for malformed WebSocket handshake")
			})
			handler := newTestHandler(t, manager, dialer)
			request := httptest.NewRequest(http.MethodGet, "/websocket", nil)
			request.Host = "public.example.com"
			request.Header.Set("Connection", "keep-alive, Upgrade")
			request.Header.Set("Upgrade", "websocket")
			request.Header.Set("Sec-WebSocket-Version", "13")
			request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
			test.mutate(request)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != http.StatusNotImplemented || strings.TrimSpace(response.Body.String()) != "UPGRADE_NOT_SUPPORTED" {
				t.Fatalf("response = (%d, %q), want (501, UPGRADE_NOT_SUPPORTED)", response.Code, response.Body.String())
			}
			if got := dialCount.Load(); got != 0 {
				t.Fatalf("Dial count = %d, want 0", got)
			}
		})
	}
}

func TestHandlerCancellationPropagatesToDialAndRoundTrip(t *testing.T) {
	t.Run("cancel unblocks Dial", func(t *testing.T) {
		manager, _ := startRouteManager(t, baseHTTPRouteState(1))
		dialStarted := make(chan struct{})
		dialResult := make(chan error, 1)
		dialer := dialerFunc(func(ctx context.Context, _ tunnel.DialRequest) (net.Conn, error) {
			close(dialStarted)
			<-ctx.Done()
			dialResult <- ctx.Err()
			return nil, ctx.Err()
		})
		handler := newTestHandler(t, manager, dialer)
		ctx, cancel := context.WithCancel(context.Background())
		request := httptest.NewRequest(http.MethodGet, "/wait", nil).WithContext(ctx)
		request.Host = "public.example.com"
		response := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			handler.ServeHTTP(response, request)
			close(done)
		}()

		select {
		case <-dialStarted:
		case <-time.After(2 * time.Second):
			t.Fatal("Dial did not start")
		}
		cancel()
		select {
		case err := <-dialResult:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Dial context error = %v, want context.Canceled", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Dial context was not canceled")
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("ServeHTTP did not return after Dial cancellation")
		}
	})

	t.Run("cancel closes active RoundTrip connection", func(t *testing.T) {
		manager, _ := startRouteManager(t, baseHTTPRouteState(1))
		requestArrived := make(chan struct{})
		peerDone := make(chan error, 1)
		var peer net.Conn
		dialer := dialerFunc(func(ctx context.Context, _ tunnel.DialRequest) (net.Conn, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			server, origin := net.Pipe()
			peer = origin
			go func() {
				defer origin.Close()
				request, err := http.ReadRequest(bufio.NewReader(origin))
				if err != nil {
					peerDone <- err
					return
				}
				if err := request.Body.Close(); err != nil {
					peerDone <- err
					return
				}
				close(requestArrived)
				if err := origin.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
					peerDone <- err
					return
				}
				var buffer [1]byte
				_, err = origin.Read(buffer[:])
				peerDone <- err
			}()
			return server, nil
		})
		handler := newTestHandler(t, manager, dialer)
		t.Cleanup(func() {
			if peer != nil {
				_ = peer.Close()
			}
		})
		ctx, cancel := context.WithCancel(context.Background())
		request := httptest.NewRequest(http.MethodGet, "/wait", nil).WithContext(ctx)
		request.Host = "public.example.com"
		response := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			handler.ServeHTTP(response, request)
			close(done)
		}()

		select {
		case <-requestArrived:
		case <-time.After(2 * time.Second):
			t.Fatal("origin did not receive RoundTrip request")
		}
		cancel()
		select {
		case err := <-peerDone:
			if err == nil {
				t.Fatal("origin connection remained readable after cancellation")
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				t.Fatalf("RoundTrip cancellation did not close connection: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("origin connection was not released after cancellation")
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("ServeHTTP did not return after RoundTrip cancellation")
		}
	})
}

func TestHandlerDoesNotExposeDialErrors(t *testing.T) {
	state := baseHTTPRouteState(1)
	state.Services[0].OriginHost = "private-origin.internal"
	manager, _ := startRouteManager(t, state)
	dialer := dialerFunc(func(context.Context, tunnel.DialRequest) (net.Conn, error) {
		return nil, errors.New("dial private-origin.internal:8080 with token super-secret failed")
	})
	handler := newTestHandler(t, manager, dialer)
	request := httptest.NewRequest(http.MethodGet, "/fail", nil)
	request.Host = "public.example.com"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("response status = %d, want 503", response.Code)
	}
	body := response.Body.String()
	for _, forbidden := range []string{"private-origin.internal", "8080", "super-secret", testTunnelID, testServiceID} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("error response %q leaked %q", body, forbidden)
		}
	}
}

func TestHandlerMapsTypedTunnelFailuresToStableHTTPContract(t *testing.T) {
	manager, _ := startRouteManager(t, baseHTTPRouteState(1))
	for _, test := range []struct {
		name     string
		err      error
		observed bool
		wantHTTP int
		wantCode string
	}{
		{name: "Agent tunnel offline", err: &serveropen.Rejected{Code: protocolv1.ErrorCode_ERROR_CODE_TUNNEL_OFFLINE}, wantHTTP: 503, wantCode: "TUNNEL_OFFLINE"},
		{name: "Agent origin refused", err: &serveropen.Rejected{Code: protocolv1.ErrorCode_ERROR_CODE_ORIGIN_REFUSED}, wantHTTP: 502, wantCode: "ORIGIN_REFUSED"},
		{name: "Agent origin timeout", err: &serveropen.Rejected{Code: protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TIMEOUT}, wantHTTP: 504, wantCode: "ORIGIN_TIMEOUT"},
		{name: "Agent work pool exhausted", err: &serveropen.Rejected{Code: protocolv1.ErrorCode_ERROR_CODE_WORK_POOL_EXHAUSTED}, wantHTTP: 503, wantCode: "WORK_POOL_EXHAUSTED"},
		{name: "Agent config not observed", err: &serveropen.Rejected{Code: protocolv1.ErrorCode_ERROR_CODE_SERVICE_CONFIG_NOT_OBSERVED}, wantHTTP: 503, wantCode: "SERVICE_CONFIG_NOT_OBSERVED"},
		{name: "Agent service disabled", err: &serveropen.Rejected{Code: protocolv1.ErrorCode_ERROR_CODE_SERVICE_DISABLED}, wantHTTP: 503, wantCode: "SERVICE_DISABLED"},
		{name: "Server pending capacity", err: serverlimits.ErrPendingOpenCapacity, observed: true, wantHTTP: 503, wantCode: "WORK_POOL_EXHAUSTED"},
		{name: "Server acquire timeout", err: serverworkpool.ErrAcquireTimeout, observed: true, wantHTTP: 503, wantCode: "WORK_POOL_EXHAUSTED"},
		{name: "new revision not observed", err: serverruntime.ErrNoAvailableConnector, wantHTTP: 503, wantCode: "SERVICE_CONFIG_NOT_OBSERVED"},
		{name: "observed tunnel offline", err: serverruntime.ErrNoAvailableConnector, observed: true, wantHTTP: 503, wantCode: "TUNNEL_OFFLINE"},
		{name: "unknown internal failure", err: errors.New("private origin and token"), observed: true, wantHTTP: 503, wantCode: "SERVICE_UNAVAILABLE"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dialer := &observingErrorDialer{observed: test.observed, err: test.err}
			handler := newTestHandler(t, manager, dialer)
			request := httptest.NewRequest(http.MethodGet, "/fail", nil)
			request.Host = "public.example.com"
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantHTTP || strings.TrimSpace(response.Body.String()) != test.wantCode {
				t.Fatalf("response = (%d, %q), want (%d, %q)", response.Code, response.Body.String(), test.wantHTTP, test.wantCode)
			}
		})
	}
}

type observingErrorDialer struct {
	observed bool
	err      error
}

func (dialer *observingErrorDialer) Dial(
	context.Context,
	tunnel.DialRequest,
) (net.Conn, error) {
	return nil, dialer.err
}

func (dialer *observingErrorDialer) ServiceConfigObserved(string, string, int64) bool {
	return dialer.observed
}

func explicitHTTPProxyOptions(disableChunked bool) repository.ServiceProxyOptions {
	return repository.ServiceProxyOptions{
		DisableChunkedEncoding:      disableChunked,
		HTTPIdleConnectionTimeoutMS: 90_000,
		HTTPMaxIdleConnections:      100,
		TCPKeepAliveIntervalMS:      30_000,
	}
}

func assertSingleHTTPDial(t *testing.T, calls []tunnelDialCall, serviceID string) {
	t.Helper()
	if len(calls) != 1 {
		t.Fatalf("Dial calls = %d, want 1: %+v", len(calls), calls)
	}
	call := calls[0]
	if call.TunnelID != testTunnelID || call.ServiceID != serviceID || call.RequiredRevision != 1 ||
		call.Ingress != protocolv1.IngressType_INGRESS_TYPE_HTTP || call.Client == "" {
		t.Fatalf("Dial call = %+v, want tunnel=%q service=%q revision=1 HTTP ingress and client address", call, testTunnelID, serviceID)
	}
}
