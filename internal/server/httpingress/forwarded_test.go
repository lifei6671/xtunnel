package httpingress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
	"github.com/lifei6671/xtunnel/internal/tunnel"
)

func TestHandlerRebuildsForwardedHeadersFromUntrustedPeer(t *testing.T) {
	manager, _ := startRouteManager(t, baseHTTPRouteState(1))
	origin := newLoopOriginDialer(t)
	handler := newTestHandlerWithTrustedProxies(t, manager, origin, []string{"10.0.0.0/8"})
	request := httptest.NewRequest(http.MethodGet, "/resource", nil)
	request.Host = "public.example.com"
	request.RemoteAddr = "198.51.100.25:54321"
	request.Header["Forwarded"] = []string{`for="not-an-ip";proto=ftp`}
	request.Header["X-Real-Ip"] = []string{"192.0.2.1"}
	request.Header["X-Forwarded-For"] = []string{"not-an-ip", "also-not-an-ip"}
	request.Header["X-Forwarded-Proto"] = []string{"ftp,http"}
	request.Header["X-Forwarded-Host"] = []string{"bad host"}
	request.Header["X-Forwarded-Secret"] = []string{"must-not-pass"}
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("response = (%d, %q), want 200", response.Code, response.Body.String())
	}
	observed := waitOriginRequest(t, origin)
	assertForwardedHeaders(t, observed.Header, "198.51.100.25", "http", "public.example.com")
	assertSingleHTTPDialClient(t, origin.Calls(), "198.51.100.25")
}

func TestHandlerUsesVerifiedTrustedProxyChain(t *testing.T) {
	manager, _ := startRouteManager(t, baseHTTPRouteState(1))
	origin := newLoopOriginDialer(t)
	handler := newTestHandlerWithTrustedProxies(t, manager, origin, []string{"10.0.0.0/8"})
	request := httptest.NewRequest(http.MethodGet, "/resource", nil)
	request.Host = "public.example.com"
	request.RemoteAddr = "10.2.2.2:443"
	request.Header.Set("X-Forwarded-For", "192.0.2.99, 198.51.100.20, 10.1.1.7")
	request.Header.Set("X-Forwarded-Proto", "HTTPS")
	request.Header.Set("X-Forwarded-Host", "ORIGINAL.Example.COM:443")
	request.Header.Set("Forwarded", "for=192.0.2.5")
	request.Header.Set("X-Real-IP", "192.0.2.6")
	request.Header.Set("X-Forwarded-Unknown", "must-not-pass")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("response = (%d, %q), want 200", response.Code, response.Body.String())
	}
	observed := waitOriginRequest(t, origin)
	assertForwardedHeaders(t, observed.Header, "198.51.100.20", "https", "ORIGINAL.Example.COM:443")
	assertSingleHTTPDialClient(t, origin.Calls(), "198.51.100.20")
}

func TestHandlerAcceptsForwardedChainBoundaryAndIPv6(t *testing.T) {
	manager, _ := startRouteManager(t, baseHTTPRouteState(1))
	origin := newLoopOriginDialer(t)
	handler := newTestHandlerWithTrustedProxies(t, manager, origin, []string{"2001:db8::/32"})
	chain := make([]string, 0, maxForwardedHops)
	for range maxForwardedHops - 2 {
		chain = append(chain, "192.0.2.10")
	}
	chain = append(chain, "2001:db9::25", "2001:db8::10")
	request := httptest.NewRequest(http.MethodGet, "/resource", nil)
	request.Host = "public.example.com"
	request.RemoteAddr = "[2001:db8::20]:8443"
	request.Header.Set("X-Forwarded-For", strings.Join(chain, ", "))
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("response = (%d, %q), want 200", response.Code, response.Body.String())
	}
	observed := waitOriginRequest(t, origin)
	assertForwardedHeaders(t, observed.Header, "2001:db9::25", "http", "public.example.com")
	assertSingleHTTPDialClient(t, origin.Calls(), "2001:db9::25")
}

func TestHandlerNormalizesIPv4MappedProxyChain(t *testing.T) {
	for _, trustedProxies := range [][]string{{"10.0.0.0/8"}, {"::ffff:10.0.0.0/104"}} {
		t.Run(trustedProxies[0], func(t *testing.T) {
			manager, _ := startRouteManager(t, baseHTTPRouteState(1))
			origin := newLoopOriginDialer(t)
			handler := newTestHandlerWithTrustedProxies(t, manager, origin, trustedProxies)
			request := httptest.NewRequest(http.MethodGet, "/resource", nil)
			request.Host = "public.example.com"
			request.RemoteAddr = "[::ffff:10.1.1.1]:443"
			request.Header.Set("X-Forwarded-For", "::ffff:198.51.100.25, ::ffff:10.2.2.2")
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("response = (%d, %q), want 200", response.Code, response.Body.String())
			}
			observed := waitOriginRequest(t, origin)
			assertForwardedHeaders(t, observed.Header, "198.51.100.25", "http", "public.example.com")
			assertSingleHTTPDialClient(t, origin.Calls(), "198.51.100.25")
		})
	}
}

func TestHandlerAcceptsBracketedIPv6ForwardedHost(t *testing.T) {
	manager, _ := startRouteManager(t, baseHTTPRouteState(1))
	origin := newLoopOriginDialer(t)
	handler := newTestHandlerWithTrustedProxies(t, manager, origin, []string{"10.0.0.0/8"})
	request := httptest.NewRequest(http.MethodGet, "/resource", nil)
	request.Host = "public.example.com"
	request.RemoteAddr = "10.1.1.1:443"
	request.Header.Set("X-Forwarded-Host", "[2001:db8::1]:8443")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("response = (%d, %q), want 200", response.Code, response.Body.String())
	}
	observed := waitOriginRequest(t, origin)
	assertForwardedHeaders(t, observed.Header, "10.1.1.1", "http", "[2001:db8::1]:8443")
}

func TestHTTPServerRejectsDuplicateForwardedHeaderLines(t *testing.T) {
	manager, _ := startRouteManager(t, baseHTTPRouteState(1))
	var dialCount atomic.Int32
	dialer := dialerFunc(func(context.Context, tunnel.DialRequest) (net.Conn, error) {
		dialCount.Add(1)
		return nil, errors.New("Dial must not run for duplicate Forwarded Header lines")
	})
	handler := newTestHandlerWithTrustedProxies(t, manager, dialer, []string{"127.0.0.0/8", "::1/128"})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	connection, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial HTTP server: %v", err)
	}
	defer connection.Close()
	if _, err := fmt.Fprint(connection, "GET /resource HTTP/1.1\r\nHost: public.example.com\r\nX-Forwarded-For: 198.51.100.1\r\nX-Forwarded-For: 198.51.100.2\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatalf("write raw HTTP request: %v", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		t.Fatalf("read HTTP response: %v", err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close HTTP response body: %v", errors.Join(readErr, closeErr))
	}
	if response.StatusCode != http.StatusBadRequest || strings.TrimSpace(string(body)) != "INVALID_FORWARDED_HEADER" {
		t.Fatalf("response = (%d, %q), want (400, INVALID_FORWARDED_HEADER)", response.StatusCode, body)
	}
	if got := dialCount.Load(); got != 0 {
		t.Fatalf("Dial count = %d, want 0", got)
	}
}

func TestHandlerRejectsInvalidTrustedForwardedMetadata(t *testing.T) {
	tooManyHops := strings.TrimSuffix(strings.Repeat("192.0.2.1,", maxForwardedHops+1), ",")
	tests := []struct {
		name       string
		remoteAddr string
		headers    http.Header
	}{
		{name: "invalid peer", remoteAddr: "not-an-address"},
		{name: "duplicate field lines", headers: http.Header{
			"X-Forwarded-For": []string{"198.51.100.1"},
			"x-forwarded-for": []string{"198.51.100.2"},
		}},
		{name: "empty forwarded for", headers: http.Header{"X-Forwarded-For": []string{"   "}}},
		{name: "invalid forwarded IP", headers: http.Header{"X-Forwarded-For": []string{"198.51.100.1:443"}}},
		{name: "too many hops", headers: http.Header{"X-Forwarded-For": []string{tooManyHops}}},
		{name: "ambiguous proto", headers: http.Header{"X-Forwarded-Proto": []string{"https,http"}}},
		{name: "unsupported proto", headers: http.Header{"X-Forwarded-Proto": []string{"ftp"}}},
		{name: "duplicate proto", headers: http.Header{"X-Forwarded-Proto": []string{"https", "http"}}},
		{name: "empty host", headers: http.Header{"X-Forwarded-Host": []string{" "}}},
		{name: "invalid host", headers: http.Header{"X-Forwarded-Host": []string{"bad host"}}},
		{name: "ambiguous host", headers: http.Header{"X-Forwarded-Host": []string{"one.example,two.example"}}},
		{name: "bare IPv6 host", headers: http.Header{"X-Forwarded-Host": []string{"2001:db8::1"}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, _ := startRouteManager(t, baseHTTPRouteState(1))
			var dialCount atomic.Int32
			dialer := dialerFunc(func(context.Context, tunnel.DialRequest) (net.Conn, error) {
				dialCount.Add(1)
				return nil, errors.New("Dial must not run for an invalid Forwarded Header")
			})
			handler := newTestHandlerWithTrustedProxies(t, manager, dialer, []string{"10.0.0.0/8"})
			request := httptest.NewRequest(http.MethodGet, "/resource", nil)
			request.Host = "public.example.com"
			request.RemoteAddr = test.remoteAddr
			if request.RemoteAddr == "" {
				request.RemoteAddr = "10.1.1.1:443"
			}
			request.Header = test.headers.Clone()
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != http.StatusBadRequest ||
				strings.TrimSpace(response.Body.String()) != "INVALID_FORWARDED_HEADER" {
				t.Fatalf("response = (%d, %q), want (400, INVALID_FORWARDED_HEADER)", response.Code, response.Body.String())
			}
			if got := dialCount.Load(); got != 0 {
				t.Fatalf("Dial count = %d, want 0", got)
			}
		})
	}
}

func TestNewHandlerRejectsInvalidTrustedProxyCIDR(t *testing.T) {
	manager, _ := startRouteManager(t, baseHTTPRouteState(1))
	dialer := dialerFunc(func(context.Context, tunnel.DialRequest) (net.Conn, error) {
		return nil, errors.New("not used")
	})
	if _, err := NewHandler(HandlerOptions{
		Routes: manager, Dialer: dialer, TrustedProxies: []string{"not-a-cidr"},
		Limits: newTestLimitManager(t, serverlimits.Options{
			MaxConnectors: 1, MaxConnectorsPerTunnel: 1,
			MaxWorkConnections: 1, MaxIdleWorkConnections: 1,
			MaxConnectingWorkConnections: 1, MaxPendingOpens: 1,
			MaxActiveConnections: 1, MaxConnectionsPerTunnel: 1,
			MaxConnectionsPerService: 1, MaxConnectionsPerSourceIP: 1,
			MaxOpenRatePerSourceIP: 1, MaxOpenBurstPerSourceIP: 1,
			MaxHTTPRequestsPerSourceIPPerSecond: 1,
		}), MaxBodyBytes: 1,
	}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("NewHandler() error = %v, want ErrInvalidOptions", err)
	}
}

func assertForwardedHeaders(t *testing.T, header http.Header, clientIP, scheme, host string) {
	t.Helper()
	if got := header.Values("X-Forwarded-For"); len(got) != 1 || got[0] != clientIP {
		t.Fatalf("X-Forwarded-For = %v, want [%q]", got, clientIP)
	}
	if got := header.Values("X-Forwarded-Proto"); len(got) != 1 || got[0] != scheme {
		t.Fatalf("X-Forwarded-Proto = %v, want [%q]", got, scheme)
	}
	if got := header.Values("X-Forwarded-Host"); len(got) != 1 || got[0] != host {
		t.Fatalf("X-Forwarded-Host = %v, want [%q]", got, host)
	}
	for key := range header {
		if strings.EqualFold(key, "Forwarded") || strings.EqualFold(key, "X-Real-IP") ||
			(strings.HasPrefix(strings.ToLower(key), "x-forwarded-") &&
				!strings.EqualFold(key, "X-Forwarded-For") &&
				!strings.EqualFold(key, "X-Forwarded-Proto") &&
				!strings.EqualFold(key, "X-Forwarded-Host")) {
			t.Fatalf("untrusted Forwarded Header %q reached Origin", key)
		}
	}
}

func assertSingleHTTPDialClient(t *testing.T, calls []tunnelDialCall, clientIP string) {
	t.Helper()
	if len(calls) != 1 {
		t.Fatalf("Dial calls = %d, want 1: %+v", len(calls), calls)
	}
	if calls[0].Client != clientIP {
		t.Fatalf("Dial client = %q, want %q", calls[0].Client, clientIP)
	}
}
