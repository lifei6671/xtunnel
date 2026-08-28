package route

import (
	"bufio"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/lifei6671/xtunnel/internal/repository"
)

func TestCanonicalHostname(t *testing.T) {
	valid := []struct {
		name     string
		input    string
		expected string
	}{
		{name: "lowercase", input: "Example.COM", expected: "example.com"},
		{name: "strip trailing dot", input: "Example.COM.", expected: "example.com"},
		{name: "strip port after trailing dot", input: "Example.COM.:443", expected: "example.com"},
		{name: "strip non-default port", input: "api.example.com:8443", expected: "api.example.com"},
		{name: "Unicode IDNA", input: "BÜCHER.Example.", expected: "xn--bcher-kva.example"},
		{name: "canonical IDNA", input: "xn--bcher-kva.example", expected: "xn--bcher-kva.example"},
		{name: "IDNA ideographic trailing dot with port", input: "Example。:443", expected: "example"},
		{name: "IDNA fullwidth trailing dot with port", input: "Example．:443", expected: "example"},
		{name: "IDNA halfwidth trailing dot with port", input: "Example｡:443", expected: "example"},
		{name: "canonical bare IPv6 storage key", input: "2001:db8::1", expected: "2001:db8::1"},
		{name: "bracketed IPv6 authority", input: "[2001:db8::1]:443", expected: "2001:db8::1"},
	}
	for _, test := range valid {
		t.Run(test.name, func(t *testing.T) {
			actual, err := CanonicalHostname(test.input)
			if err != nil {
				t.Fatalf("CanonicalHostname(%q) error = %v", test.input, err)
			}
			if actual != test.expected {
				t.Fatalf("CanonicalHostname(%q) = %q, want %q", test.input, actual, test.expected)
			}
		})
	}

	invalid := []string{
		"",
		" ",
		":443",
		"example.com:",
		"example.com:not-a-port",
		"example.com:70000",
		"http://example.com",
		"example.com/path",
		"bad host.example",
		"bad\x00host.example",
		".example.com",
		"example..com",
		"-bad.example",
		"bad_.example",
		"example。。",
		"[127.0.0.1]",
	}
	for _, input := range invalid {
		t.Run("invalid "+input, func(t *testing.T) {
			actual, err := CanonicalHostname(input)
			if !errors.Is(err, ErrInvalidHost) {
				t.Fatalf("CanonicalHostname(%q) = (%q, %v), want ErrInvalidHost", input, actual, err)
			}
			if actual != "" {
				t.Fatalf("CanonicalHostname(%q) returned partial hostname %q", input, actual)
			}
		})
	}
}

func TestCanonicalPathPrefix(t *testing.T) {
	valid := []struct {
		name     string
		input    string
		expected string
	}{
		{name: "root", input: "/", expected: "/"},
		{name: "non-root", input: "/foo", expected: "/foo"},
		{name: "strip trailing slash", input: "/foo/", expected: "/foo"},
		{name: "strip all trailing slashes", input: "/foo///", expected: "/foo"},
		{name: "preserve repeated internal slash", input: "/foo//bar/", expected: "/foo//bar"},
		{name: "decode allowed percent encoding", input: "/foo/%7Euser/", expected: "/foo/~user"},
	}
	for _, test := range valid {
		t.Run(test.name, func(t *testing.T) {
			actual, err := CanonicalPathPrefix(test.input)
			if err != nil {
				t.Fatalf("CanonicalPathPrefix(%q) error = %v", test.input, err)
			}
			if actual != test.expected {
				t.Fatalf("CanonicalPathPrefix(%q) = %q, want %q", test.input, actual, test.expected)
			}
		})
	}

	invalid := []string{
		"",
		"foo",
		"/%",
		"/%ZZ",
		"/foo%2fbar",
		"/foo%2Fbar",
		"/foo%5cbar",
		"/foo%5Cbar",
		"/foo%252Fbar",
		"/foo%255Cbar",
		"/foo/%25ZZ/%252Fbar",
		"/%2e",
		"/%2E%2E",
		"/%252E%252E",
		"/foo/%2e/bar",
		"/foo/%2E%2e/bar",
		"/.",
		"/..",
		"/foo/./bar",
		"/foo/../bar",
		"/foo\\bar",
		"/foo\x00bar",
		"/foo\nbar",
		"/foo/%FF",
	}
	for _, input := range invalid {
		t.Run("invalid "+input, func(t *testing.T) {
			actual, err := CanonicalPathPrefix(input)
			if !errors.Is(err, ErrInvalidPath) {
				t.Fatalf("CanonicalPathPrefix(%q) = (%q, %v), want ErrInvalidPath", input, actual, err)
			}
			if actual != "" {
				t.Fatalf("CanonicalPathPrefix(%q) returned partial path %q", input, actual)
			}
		})
	}
}

func TestSnapshotMatchHTTPSelectsExactHostAndLongestSegmentPrefix(t *testing.T) {
	snapshot := matcherSnapshot(t)
	tests := []struct {
		name         string
		host         string
		path         string
		rawPath      string
		rawQuery     string
		requestURI   string
		wantRouteID  string
		wantHostname string
		wantPath     string
		wantRawPath  string
	}{
		{
			name: "case trailing dot and port", host: "APP.Example.COM.:443", path: "/foo/bar/item",
			requestURI: "/foo/bar/item", wantRouteID: "http-foo-bar", wantHostname: "app.example.com",
			wantPath: "/foo/bar/item",
		},
		{
			name: "exact second host", host: "other.example.com", path: "/foo/item",
			requestURI: "/foo/item", wantRouteID: "http-other", wantHostname: "other.example.com",
			wantPath: "/foo/item",
		},
		{
			name: "Unicode host matches IDNA route", host: "BÜCHER.example", path: "/",
			requestURI: "/", wantRouteID: "http-idna", wantHostname: "xn--bcher-kva.example", wantPath: "/",
		},
		{
			name: "exact prefix", host: "app.example.com", path: "/foo",
			requestURI: "/foo", wantRouteID: "http-foo", wantHostname: "app.example.com", wantPath: "/foo",
		},
		{
			name: "prefix trailing slash", host: "app.example.com", path: "/foo/",
			requestURI: "/foo/", wantRouteID: "http-foo", wantHostname: "app.example.com", wantPath: "/foo/",
		},
		{
			name: "repeated slash is not collapsed", host: "app.example.com", path: "/foo//bar",
			requestURI: "/foo//bar", wantRouteID: "http-foo", wantHostname: "app.example.com", wantPath: "/foo//bar",
		},
		{
			name: "segment boundary rejects longer token", host: "app.example.com", path: "/foobar",
			requestURI: "/foobar", wantRouteID: "http-root", wantHostname: "app.example.com", wantPath: "/foobar",
		},
		{
			name: "root fallback", host: "app.example.com", path: "/unmatched",
			requestURI: "/unmatched", wantRouteID: "http-root", wantHostname: "app.example.com", wantPath: "/unmatched",
		},
		{
			name: "valid encoded unreserved character", host: "app.example.com", path: "/foo/~user",
			rawPath: "/foo/%7Euser", rawQuery: "view=1", requestURI: "/foo/%7Euser?view=1", wantRouteID: "http-foo",
			wantHostname: "app.example.com", wantPath: "/foo/~user", wantRawPath: "/foo/%7Euser",
		},
		{
			name: "empty RawPath with canonical escape", host: "app.example.com", path: "/foo/space here",
			requestURI: "/foo/space%20here", wantRouteID: "http-foo", wantHostname: "app.example.com",
			wantPath: "/foo/space here",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := &http.Request{
				Host:       test.host,
				URL:        &url.URL{Path: test.path, RawPath: test.rawPath, RawQuery: test.rawQuery},
				RequestURI: test.requestURI,
			}
			match, found, err := snapshot.MatchHTTP(request)
			if err != nil {
				t.Fatalf("MatchHTTP() error = %v", err)
			}
			if !found {
				t.Fatal("MatchHTTP() found = false, want true")
			}
			if match.Route.ID != test.wantRouteID || match.Hostname != test.wantHostname ||
				match.Path != test.wantPath || match.RawPath != test.wantRawPath {
				t.Fatalf("MatchHTTP() = route %q host %q path %q raw_path %q, want route %q host %q path %q raw_path %q",
					match.Route.ID, match.Hostname, match.Path, match.RawPath,
					test.wantRouteID, test.wantHostname, test.wantPath, test.wantRawPath)
			}
			if match.Route.Hostname != test.wantHostname {
				t.Fatalf("MatchHTTP() Route.Hostname = %q, want %q", match.Route.Hostname, test.wantHostname)
			}
		})
	}
}

func TestSnapshotMatchHTTPRejectsInvalidHostOrPath(t *testing.T) {
	snapshot := matcherSnapshot(t)
	tests := []struct {
		name    string
		request *http.Request
		wantErr error
	}{
		{name: "nil request", request: nil, wantErr: ErrInvalidPath},
		{name: "nil URL", request: &http.Request{Host: "app.example.com"}, wantErr: ErrInvalidPath},
		{
			name: "empty Host", request: matcherRequest("", "/foo", "", "/foo"), wantErr: ErrInvalidHost,
		},
		{
			name: "Host contains whitespace", request: matcherRequest("bad host", "/foo", "", "/foo"), wantErr: ErrInvalidHost,
		},
		{
			name: "Host contains control character", request: matcherRequest("bad\x00host", "/foo", "", "/foo"), wantErr: ErrInvalidHost,
		},
		{
			name: "invalid percent encoding", request: matcherRequest("app.example.com", "/foo", "/foo%ZZ", "/foo%ZZ"), wantErr: ErrInvalidPath,
		},
		{
			name: "RawPath disagrees with Path", request: matcherRequest("app.example.com", "/foo", "/bar", "/bar"), wantErr: ErrInvalidPath,
		},
		{
			name: "RawPath disagrees with RequestURI", request: matcherRequest("app.example.com", "/foo/~user", "/foo/%7Euser", "/bar"), wantErr: ErrInvalidPath,
		},
		{
			name: "RawQuery disagrees with RequestURI",
			request: &http.Request{
				Host:       "app.example.com",
				URL:        &url.URL{Path: "/foo", RawQuery: "left=1"},
				RequestURI: "/foo?right=1",
			},
			wantErr: ErrInvalidPath,
		},
		{
			name: "ForceQuery disagrees with RequestURI",
			request: &http.Request{
				Host:       "app.example.com",
				URL:        &url.URL{Path: "/foo"},
				RequestURI: "/foo?",
			},
			wantErr: ErrInvalidPath,
		},
		{
			name: "literal fragment delimiter in RawQuery",
			request: &http.Request{
				Host:       "app.example.com",
				URL:        &url.URL{Path: "/foo", RawQuery: "value=#fragment"},
				RequestURI: "/foo?value=#fragment",
			},
			wantErr: ErrInvalidPath,
		},
		{
			name: "encoded slash", request: matcherRequest("app.example.com", "/foo/bar", "/foo%2Fbar", "/foo%2Fbar"), wantErr: ErrInvalidPath,
		},
		{
			name: "encoded backslash", request: matcherRequest("app.example.com", "/foo\\bar", "/foo%5Cbar", "/foo%5Cbar"), wantErr: ErrInvalidPath,
		},
		{
			name: "encoded dot segment", request: matcherRequest("app.example.com", "/foo/../bar", "/foo/%2e%2e/bar", "/foo/%2e%2e/bar"), wantErr: ErrInvalidPath,
		},
		{
			name: "double encoded slash", request: matcherRequest("app.example.com", "/foo/%2Fbar", "/foo/%252Fbar", "/foo/%252Fbar"), wantErr: ErrInvalidPath,
		},
		{
			name: "double encoded backslash", request: matcherRequest("app.example.com", "/foo/%5Cbar", "/foo/%255Cbar", "/foo/%255Cbar"), wantErr: ErrInvalidPath,
		},
		{
			name: "double encoded dot segment", request: matcherRequest("app.example.com", "/foo/%2E%2E/bar", "/foo/%252E%252E/bar", "/foo/%252E%252E/bar"), wantErr: ErrInvalidPath,
		},
		{
			name:    "invalid literal percent cannot hide double encoded slash",
			request: matcherRequest("app.example.com", "/foo/%ZZ/%2Fbar", "/foo/%25ZZ/%252Fbar", "/foo/%25ZZ/%252Fbar"),
			wantErr: ErrInvalidPath,
		},
		{
			name: "plain current dot segment", request: matcherRequest("app.example.com", "/foo/./bar", "", "/foo/./bar"), wantErr: ErrInvalidPath,
		},
		{
			name: "plain parent dot segment", request: matcherRequest("app.example.com", "/foo/../bar", "", "/foo/../bar"), wantErr: ErrInvalidPath,
		},
		{
			name: "plain backslash", request: matcherRequest("app.example.com", "/foo\\bar", "", "/foo\\bar"), wantErr: ErrInvalidPath,
		},
		{
			name: "plain control character", request: matcherRequest("app.example.com", "/foo\x00bar", "", "/foo\x00bar"), wantErr: ErrInvalidPath,
		},
		{
			name: "percent encoded invalid UTF-8", request: matcherRequest("app.example.com", "/foo/\xff", "/foo/%FF", "/foo/%FF"), wantErr: ErrInvalidPath,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			match, found, err := snapshot.MatchHTTP(test.request)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("MatchHTTP() error = %v, want %v", err, test.wantErr)
			}
			if found {
				t.Fatal("MatchHTTP() found = true for invalid request")
			}
			if match != (HTTPMatch{}) {
				t.Fatalf("MatchHTTP() returned partial match %+v for invalid request", match)
			}
		})
	}
}

func TestSnapshotMatchHTTPRejectsAmbiguousURLMetadata(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*url.URL)
	}{
		{name: "scheme", mutate: func(value *url.URL) { value.Scheme = "http" }},
		{name: "host", mutate: func(value *url.URL) { value.Host = "other.example.com" }},
		{name: "user info", mutate: func(value *url.URL) { value.User = url.User("user") }},
		{name: "opaque", mutate: func(value *url.URL) { value.Opaque = "opaque" }},
		{name: "fragment", mutate: func(value *url.URL) { value.Fragment = "fragment" }},
		{name: "raw fragment", mutate: func(value *url.URL) { value.RawFragment = "raw-fragment" }},
	}
	snapshot := matcherSnapshot(t)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := matcherRequest("app.example.com", "/foo", "", "/foo")
			test.mutate(request.URL)
			match, found, err := snapshot.MatchHTTP(request)
			if !errors.Is(err, ErrInvalidPath) || found || match != (HTTPMatch{}) {
				t.Fatalf("MatchHTTP() = (%+v, %v, %v), want zero, false, ErrInvalidPath", match, found, err)
			}
		})
	}
}

func TestSnapshotMatchHTTPUsesRealNetHTTPRequestShape(t *testing.T) {
	snapshot := matcherSnapshot(t)
	tests := []struct {
		name        string
		requestLine string
		wantRouteID string
		wantPath    string
		wantRawPath string
		wantErr     error
	}{
		{
			name: "RawPath is populated for non-canonical escape", requestLine: "GET /foo/%7Euser?view=1 HTTP/1.1",
			wantRouteID: "http-foo", wantPath: "/foo/~user", wantRawPath: "/foo/%7Euser",
		},
		{
			name: "RawPath stays empty for canonical escape", requestLine: "GET /foo/space%20here HTTP/1.1",
			wantRouteID: "http-foo", wantPath: "/foo/space here",
		},
		{
			name: "real parser encoded slash", requestLine: "GET /foo%2Fbar HTTP/1.1", wantErr: ErrInvalidPath,
		},
		{
			name: "real parser encoded backslash with empty RawPath", requestLine: "GET /foo%5Cbar HTTP/1.1", wantErr: ErrInvalidPath,
		},
		{
			name: "real parser literal fragment delimiter in query", requestLine: "GET /foo?value=#fragment HTTP/1.1", wantErr: ErrInvalidPath,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wire := test.requestLine + "\r\nHost: APP.Example.COM.:443\r\n\r\n"
			request, err := http.ReadRequest(bufio.NewReader(strings.NewReader(wire)))
			if err != nil {
				t.Fatalf("ReadRequest() error = %v", err)
			}
			t.Cleanup(func() { _ = request.Body.Close() })

			match, found, err := snapshot.MatchHTTP(request)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) || found || match != (HTTPMatch{}) {
					t.Fatalf("MatchHTTP() = (%+v, %v, %v), want zero, false, %v", match, found, err, test.wantErr)
				}
				return
			}
			if err != nil || !found {
				t.Fatalf("MatchHTTP() = (%+v, %v, %v), want successful match", match, found, err)
			}
			if match.Route.ID != test.wantRouteID || match.Path != test.wantPath || match.RawPath != test.wantRawPath {
				t.Fatalf("MatchHTTP() = route %q path %q raw_path %q, want route %q path %q raw_path %q",
					match.Route.ID, match.Path, match.RawPath, test.wantRouteID, test.wantPath, test.wantRawPath)
			}
		})
	}
}

func TestSnapshotMatchHTTPValidatesRealHostAuthority(t *testing.T) {
	snapshot := matcherSnapshot(t)
	tests := []struct {
		name        string
		host        string
		wantRouteID string
		wantErr     error
	}{
		{name: "IPv4 with port", host: "192.0.2.1:8080", wantRouteID: "http-ipv4"},
		{name: "bracketed IPv6 with port", host: "[2001:db8::1]:8443", wantRouteID: "http-ipv6"},
		{name: "bare IPv6 is ambiguous authority", host: "2001:db8::1", wantErr: ErrInvalidHost},
		{name: "brackets cannot wrap IPv4", host: "[192.0.2.1]:8080", wantErr: ErrInvalidHost},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wire := "GET / HTTP/1.1\r\nHost: " + test.host + "\r\n\r\n"
			request, err := http.ReadRequest(bufio.NewReader(strings.NewReader(wire)))
			if err != nil {
				t.Fatalf("ReadRequest() error = %v", err)
			}
			t.Cleanup(func() { _ = request.Body.Close() })

			match, found, err := snapshot.MatchHTTP(request)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) || found || match != (HTTPMatch{}) {
					t.Fatalf("MatchHTTP() = (%+v, %v, %v), want zero, false, %v", match, found, err, test.wantErr)
				}
				return
			}
			if err != nil || !found || match.Route.ID != test.wantRouteID {
				t.Fatalf("MatchHTTP() = (%+v, %v, %v), want route %q", match, found, err, test.wantRouteID)
			}
		})
	}
}

func TestBuildSnapshotRejectsNonCanonicalHTTPRouteKeys(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*repository.HTTPRoute)
	}{
		{name: "uppercase hostname", mutate: func(route *repository.HTTPRoute) { route.Hostname = "APP.example.com" }},
		{name: "hostname with port", mutate: func(route *repository.HTTPRoute) { route.Hostname = "app.example.com:443" }},
		{name: "Unicode hostname", mutate: func(route *repository.HTTPRoute) { route.Hostname = "bücher.example" }},
		{name: "path prefix with trailing slash", mutate: func(route *repository.HTTPRoute) { route.PathPrefix = "/foo/" }},
		{name: "path prefix with dot segment", mutate: func(route *repository.HTTPRoute) { route.PathPrefix = "/foo/../bar" }},
		{name: "path prefix with encoded slash", mutate: func(route *repository.HTTPRoute) { route.PathPrefix = "/foo/%2Fbar" }},
		{name: "path prefix with encoded dot segment", mutate: func(route *repository.HTTPRoute) { route.PathPrefix = "/%2E%2E" }},
		{name: "path prefix with double encoded slash", mutate: func(route *repository.HTTPRoute) { route.PathPrefix = "/foo/%252Fbar" }},
		{name: "literal percent cannot hide encoded slash", mutate: func(route *repository.HTTPRoute) { route.PathPrefix = "/foo/%ZZ/%2Fbar" }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := validDesiredState(12)
			test.mutate(&state.HTTPRoutes[0])
			_, err := buildSnapshot(state)
			if !errors.Is(err, ErrInvalidDesiredState) {
				t.Fatalf("buildSnapshot() error = %v, want ErrInvalidDesiredState", err)
			}
		})
	}
}

func TestSnapshotMatchHTTPReturnsNotFoundWithoutPartialMatch(t *testing.T) {
	tests := []struct {
		name     string
		snapshot *Snapshot
		request  *http.Request
	}{
		{
			name:     "unknown exact host",
			snapshot: matcherSnapshot(t),
			request:  matcherRequest("sub.app.example.com", "/foo", "", "/foo"),
		},
		{
			name:     "known host without matching prefix or root",
			snapshot: matcherSnapshotWithoutRoot(t),
			request:  matcherRequest("api.example.com", "/other", "", "/other"),
		},
		{
			name:     "nil snapshot with valid request",
			snapshot: nil,
			request:  matcherRequest("app.example.com", "/foo", "", "/foo"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			match, found, err := test.snapshot.MatchHTTP(test.request)
			if err != nil {
				t.Fatalf("MatchHTTP() error = %v", err)
			}
			if found {
				t.Fatal("MatchHTTP() found = true, want false")
			}
			if match != (HTTPMatch{}) {
				t.Fatalf("MatchHTTP() = %+v, want zero value", match)
			}
		})
	}
}

func matcherSnapshot(t *testing.T) *Snapshot {
	t.Helper()
	state := validDesiredState(10)
	state.HTTPRoutes = []repository.HTTPRoute{
		matcherRoute("http-root", "app.example.com", "/"),
		matcherRoute("http-foo", "app.example.com", "/foo"),
		matcherRoute("http-foo-bar", "app.example.com", "/foo/bar"),
		matcherRoute("http-other", "other.example.com", "/foo"),
		matcherRoute("http-idna", "xn--bcher-kva.example", "/"),
		matcherRoute("http-ipv4", "192.0.2.1", "/"),
		matcherRoute("http-ipv6", "2001:db8::1", "/"),
	}
	snapshot, err := buildSnapshot(state)
	if err != nil {
		t.Fatalf("buildSnapshot() error = %v", err)
	}
	return snapshot
}

func matcherSnapshotWithoutRoot(t *testing.T) *Snapshot {
	t.Helper()
	state := validDesiredState(11)
	state.HTTPRoutes = []repository.HTTPRoute{matcherRoute("http-api", "api.example.com", "/api")}
	snapshot, err := buildSnapshot(state)
	if err != nil {
		t.Fatalf("buildSnapshot() error = %v", err)
	}
	return snapshot
}

func matcherRoute(id, hostname, pathPrefix string) repository.HTTPRoute {
	return repository.HTTPRoute{
		ID: id, ServiceID: testServiceID, Hostname: hostname, PathPrefix: pathPrefix,
		PreserveHost: true, Enabled: true, CreatedAt: 1, UpdatedAt: 1,
	}
}

func matcherRequest(hostname, path, rawPath, requestURI string) *http.Request {
	return &http.Request{
		Host:       hostname,
		URL:        &url.URL{Path: path, RawPath: rawPath},
		RequestURI: requestURI,
	}
}
