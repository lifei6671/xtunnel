package httpingress

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
	"github.com/lifei6671/xtunnel/internal/tunnel"
)

func TestHandlerEnforcesSourceRequestRateAfterForwardedNormalization(t *testing.T) {
	routes, _ := startRouteManager(t, baseHTTPRouteState(1))
	origin := newLoopOriginDialer(t)
	options := testHTTPLimitOptions()
	options.MaxHTTPRequestsPerSourceIPPerSecond = 1
	limits := newTestLimitManager(t, options)
	handler := newTestHandlerWithLimits(
		t, routes, origin, []string{"127.0.0.1/32"}, limits, 1024,
	)

	first := httptest.NewRequest(http.MethodGet, "/first", nil)
	first.Host = "public.example.com"
	first.RemoteAddr = "127.0.0.1:50001"
	first.Header.Set("X-Forwarded-For", "198.51.100.20")
	firstResponse := httptest.NewRecorder()
	handler.ServeHTTP(firstResponse, first)
	if firstResponse.Code != http.StatusOK {
		t.Fatalf("first response = (%d, %q), want 200", firstResponse.Code, firstResponse.Body.String())
	}
	waitOriginRequest(t, origin)

	second := httptest.NewRequest(http.MethodGet, "/second", nil)
	second.Host = "public.example.com"
	second.RemoteAddr = "127.0.0.1:50002"
	second.Header.Set("X-Forwarded-For", "198.51.100.20")
	secondResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondResponse, second)

	if secondResponse.Code != http.StatusTooManyRequests ||
		strings.TrimSpace(secondResponse.Body.String()) != "RATE_LIMITED" {
		t.Fatalf("second response = (%d, %q), want (429, RATE_LIMITED)",
			secondResponse.Code, secondResponse.Body.String())
	}
	if retryAfter := secondResponse.Header().Get("Retry-After"); retryAfter != "1" {
		t.Fatalf("Retry-After = %q, want 1", retryAfter)
	}
	if calls := origin.Calls(); len(calls) != 1 || calls[0].Client != "198.51.100.20" {
		t.Fatalf("Dial calls = %+v, want one call for normalized source", calls)
	}
}

func TestHandlerRejectsKnownOversizeBodyBeforeTunnelDial(t *testing.T) {
	routes, _ := startRouteManager(t, baseHTTPRouteState(1))
	var dialCount atomic.Int32
	dialer := dialerFunc(func(context.Context, tunnel.DialRequest) (net.Conn, error) {
		dialCount.Add(1)
		return nil, errors.New("Dial must not run for a known oversize request")
	})
	limits := newTestLimitManager(t, testHTTPLimitOptions())
	handler := newTestHandlerWithLimits(t, routes, dialer, nil, limits, 4)
	request := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("12345"))
	request.Host = "public.example.com"
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge ||
		strings.TrimSpace(response.Body.String()) != "REQUEST_BODY_TOO_LARGE" {
		t.Fatalf("response = (%d, %q), want (413, REQUEST_BODY_TOO_LARGE)",
			response.Code, response.Body.String())
	}
	if response.Header().Get("Connection") != "close" || !request.Close {
		t.Fatalf("oversize response Connection=%q request.Close=%v, want close/true",
			response.Header().Get("Connection"), request.Close)
	}
	if got := dialCount.Load(); got != 0 {
		t.Fatalf("Dial count = %d, want 0", got)
	}
	if got := limits.Snapshot(); got.ActiveTotal != 0 {
		t.Fatalf("active limits after rejection = %#v, want empty", got)
	}
}

func TestHandlerRejectsStreamingOversizeBody(t *testing.T) {
	routes, _ := startRouteManager(t, baseHTTPRouteState(1))
	origin := newLoopOriginDialer(t)
	limits := newTestLimitManager(t, testHTTPLimitOptions())
	handler := newTestHandlerWithLimits(t, routes, origin, nil, limits, 4)
	request := httptest.NewRequest(http.MethodPost, "/upload", io.NopCloser(strings.NewReader("12345")))
	request.Host = "public.example.com"
	request.ContentLength = -1
	request.TransferEncoding = []string{"chunked"}
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge ||
		strings.TrimSpace(response.Body.String()) != "REQUEST_BODY_TOO_LARGE" {
		t.Fatalf("response = (%d, %q), want (413, REQUEST_BODY_TOO_LARGE)",
			response.Code, response.Body.String())
	}
	if response.Header().Get("Connection") != "close" {
		t.Fatalf("oversize response Connection=%q, want close", response.Header().Get("Connection"))
	}
	if got := limits.Snapshot(); got.ActiveTotal != 0 {
		t.Fatalf("active limits after streaming rejection = %#v, want empty", got)
	}
}

func TestHandlerActiveLeaseCoversRequestAndReleasesBeforeCrossSourceKeepAliveReuse(t *testing.T) {
	routes, _ := startRouteManager(t, baseHTTPRouteState(1))
	firstArrived := make(chan struct{})
	releaseFirst := make(chan struct{})
	peerDone := make(chan error, 1)
	var dialCount atomic.Int32
	var peer net.Conn
	dialer := dialerFunc(func(ctx context.Context, _ tunnel.DialRequest) (net.Conn, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		server, origin := net.Pipe()
		peer = origin
		dialCount.Add(1)
		go func() {
			defer origin.Close()
			reader := bufio.NewReader(origin)
			for index := 0; index < 2; index++ {
				request, err := http.ReadRequest(reader)
				if err != nil {
					peerDone <- err
					return
				}
				if _, err := io.Copy(io.Discard, request.Body); err != nil {
					peerDone <- err
					return
				}
				if err := request.Body.Close(); err != nil {
					peerDone <- err
					return
				}
				if index == 0 {
					close(firstArrived)
					<-releaseFirst
				}
				body := "ok"
				response := &http.Response{
					StatusCode: http.StatusOK, ProtoMajor: 1, ProtoMinor: 1,
					Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)),
					ContentLength: int64(len(body)),
				}
				if err := response.Write(origin); err != nil {
					peerDone <- err
					return
				}
			}
			peerDone <- nil
		}()
		return server, nil
	})
	t.Cleanup(func() {
		if peer != nil {
			_ = peer.Close()
		}
	})

	options := testHTTPLimitOptions()
	options.MaxActiveConnections = 2
	options.MaxConnectionsPerTunnel = 2
	options.MaxConnectionsPerService = 2
	options.MaxConnectionsPerSourceIP = 1
	limits := newTestLimitManager(t, options)
	handler := newTestHandlerWithLimits(t, routes, dialer, nil, limits, 1024)

	firstRequest := httptest.NewRequest(http.MethodGet, "/first", nil)
	firstRequest.Host = "public.example.com"
	firstRequest.RemoteAddr = "192.0.2.10:50001"
	firstResponse := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(firstResponse, firstRequest)
		close(firstDone)
	}()
	select {
	case <-firstArrived:
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not reach origin")
	}
	if got := limits.Snapshot(); got.ActiveTotal != 1 || got.ActiveBySource[firstRequestIP] != 1 {
		t.Fatalf("active limits during first request = %#v, want one lease for first source", got)
	}

	blockedRequest := httptest.NewRequest(http.MethodGet, "/blocked", nil)
	blockedRequest.Host = "public.example.com"
	blockedRequest.RemoteAddr = "192.0.2.10:50002"
	blockedResponse := httptest.NewRecorder()
	handler.ServeHTTP(blockedResponse, blockedRequest)
	if blockedResponse.Code != http.StatusServiceUnavailable ||
		strings.TrimSpace(blockedResponse.Body.String()) != "WORK_POOL_EXHAUSTED" {
		t.Fatalf("blocked response = (%d, %q), want (503, WORK_POOL_EXHAUSTED)",
			blockedResponse.Code, blockedResponse.Body.String())
	}
	if got := dialCount.Load(); got != 1 {
		t.Fatalf("Dial count while same source is blocked = %d, want 1", got)
	}

	close(releaseFirst)
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not finish")
	}
	if firstResponse.Code != http.StatusOK {
		t.Fatalf("first response = (%d, %q), want 200", firstResponse.Code, firstResponse.Body.String())
	}

	secondSourceRequest := httptest.NewRequest(http.MethodGet, "/second-source", nil)
	secondSourceRequest.Host = "public.example.com"
	secondSourceRequest.RemoteAddr = "198.51.100.30:50003"
	secondSourceResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondSourceResponse, secondSourceRequest)
	if secondSourceResponse.Code != http.StatusOK {
		t.Fatalf("second source response = (%d, %q), want 200",
			secondSourceResponse.Code, secondSourceResponse.Body.String())
	}
	if got := dialCount.Load(); got != 1 {
		t.Fatalf("Dial count after cross-source KeepAlive reuse = %d, want 1", got)
	}
	if got := limits.Snapshot(); got.ActiveTotal != 0 || len(got.ActiveBySource) != 0 {
		t.Fatalf("active limits after requests = %#v, want empty", got)
	}
	select {
	case err := <-peerDone:
		if err != nil {
			t.Fatalf("origin peer error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("origin peer did not finish")
	}
}

func TestHandlerMapsOpenRateLimitToPublicContract(t *testing.T) {
	routes, _ := startRouteManager(t, baseHTTPRouteState(1))
	dialer := dialerFunc(func(context.Context, tunnel.DialRequest) (net.Conn, error) {
		return nil, serverlimits.ErrOpenRateExceeded
	})
	limits := newTestLimitManager(t, testHTTPLimitOptions())
	handler := newTestHandlerWithLimits(t, routes, dialer, nil, limits, 1024)
	request := httptest.NewRequest(http.MethodGet, "/rate", nil)
	request.Host = "public.example.com"
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusTooManyRequests ||
		strings.TrimSpace(response.Body.String()) != "RATE_LIMITED" ||
		response.Header().Get("Retry-After") != "1" {
		t.Fatalf("response = (%d, %q, Retry-After=%q), want (429, RATE_LIMITED, 1)",
			response.Code, response.Body.String(), response.Header().Get("Retry-After"))
	}
}

var firstRequestIP = netip.MustParseAddr("192.0.2.10")

func testHTTPLimitOptions() serverlimits.Options {
	return serverlimits.Options{
		MaxConnectors: 16, MaxConnectorsPerTunnel: 16,
		MaxWorkConnections: 16, MaxIdleWorkConnections: 16,
		MaxConnectingWorkConnections: 16, MaxPendingOpens: 16,
		MaxActiveConnections: 16, MaxConnectionsPerTunnel: 16,
		MaxConnectionsPerService: 16, MaxConnectionsPerSourceIP: 16,
		MaxOpenRatePerSourceIP: 1_000, MaxOpenBurstPerSourceIP: 1_000,
		MaxHTTPRequestsPerSourceIPPerSecond: 1_000,
	}
}
