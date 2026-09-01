package route

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

const fuzzRequestLimit = 8 << 10

func FuzzSnapshotMatchHTTPFromWire(f *testing.F) {
	seeds := [][2]string{
		{"/", "app.example.com"}, {"/foo", "APP.EXAMPLE.COM:80"},
		{"/foobar", "app.example.com"}, {"/foo/bar", "app.example.com"},
		{"/foo//bar?x=1", "app.example.com."}, {"/foo/%7Euser", "app.example.com"},
		{"/foo/%2Fbar", "app.example.com"}, {"/foo/%252E%252E/bar", "app.example.com"},
		{"/", "xn--bcher-kva.example"}, {"/", "[2001:db8::1]:443"},
		{"%", "app.example.com"}, {"*", "app.example.com"},
	}
	for _, seed := range seeds {
		f.Add(seed[0], seed[1])
	}

	snapshot := fuzzMatcherSnapshot()
	f.Fuzz(func(t *testing.T, requestTarget, host string) {
		if len(requestTarget)+len(host) > fuzzRequestLimit {
			return
		}
		request, err := readFuzzRequest(requestTarget, host)
		if err != nil {
			// net/http 在 Handler 前拒绝的 Wire 输入不属于 Matcher 结果。
			return
		}
		before := requestShape(request)
		first, firstFound, firstErr := snapshot.MatchHTTP(request)
		second, secondFound, secondErr := snapshot.MatchHTTP(request)
		if !sameMatchResult(first, firstFound, firstErr, second, secondFound, secondErr) {
			t.Fatalf("MatchHTTP is not deterministic: first=(%+v,%t,%v) second=(%+v,%t,%v)", first, firstFound, firstErr, second, secondFound, secondErr)
		}
		if got := requestShape(request); !reflect.DeepEqual(got, before) {
			t.Fatalf("MatchHTTP mutated request: before=%+v after=%+v", before, got)
		}
		assertMatchResult(t, request, first, firstFound, firstErr)
	})
}

func FuzzSnapshotMatchHTTPRejectsDangerousPath(f *testing.F) {
	for category := range byte(4) {
		for depth := byte(1); depth <= 9; depth++ {
			f.Add("safe", "tail", category, depth)
		}
	}

	snapshot := fuzzMatcherSnapshot()
	f.Fuzz(func(t *testing.T, prefix, suffix string, category, depth byte) {
		if len(prefix)+len(suffix) > fuzzRequestLimit/2 {
			return
		}
		prefix = strings.Map(safePathRune, prefix)
		suffix = strings.Map(safePathRune, suffix)
		core := []string{"%2F", "%5C", "%2E", "%2E%2E"}[int(category)%4]
		for layer := byte(1); layer < depth%9+1; layer++ {
			core = strings.ReplaceAll(core, "%", "%25")
		}
		target := "/" + prefix + "/" + core + "/" + suffix
		request, err := readFuzzRequest(target, "app.example.com")
		if err != nil {
			return
		}
		match, found, err := snapshot.MatchHTTP(request)
		if !errors.Is(err, ErrInvalidPath) || found || match != (HTTPMatch{}) {
			t.Fatalf("dangerous target %q result = (%+v,%t,%v), want zero,false,ErrInvalidPath", target, match, found, err)
		}
	})
}

func fuzzMatcherSnapshot() *Snapshot {
	return &Snapshot{http: map[string]HostRoutes{
		"app.example.com": {routes: []HTTPRoute{
			{ID: "root", Hostname: "app.example.com", PathPrefix: "/"},
			{ID: "foo", Hostname: "app.example.com", PathPrefix: "/foo"},
		}},
		"xn--bcher-kva.example": {routes: []HTTPRoute{{ID: "idna", Hostname: "xn--bcher-kva.example", PathPrefix: "/"}}},
		"2001:db8::1":           {routes: []HTTPRoute{{ID: "ipv6", Hostname: "2001:db8::1", PathPrefix: "/"}}},
	}}
}

func readFuzzRequest(target, host string) (*http.Request, error) {
	wire := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", target, host)
	return http.ReadRequest(bufio.NewReader(bytes.NewBufferString(wire)))
}

type fuzzRequestShape struct {
	host, requestURI, path, rawPath, rawQuery string
	forceQuery                                bool
}

func requestShape(request *http.Request) fuzzRequestShape {
	return fuzzRequestShape{
		host: request.Host, requestURI: request.RequestURI, path: request.URL.Path,
		rawPath: request.URL.RawPath, rawQuery: request.URL.RawQuery, forceQuery: request.URL.ForceQuery,
	}
}

func sameMatchResult(left HTTPMatch, leftFound bool, leftErr error, right HTTPMatch, rightFound bool, rightErr error) bool {
	return left == right && leftFound == rightFound && errors.Is(leftErr, rightErr) && errors.Is(rightErr, leftErr)
}

func assertMatchResult(t *testing.T, request *http.Request, match HTTPMatch, found bool, err error) {
	t.Helper()
	if err != nil {
		if !errors.Is(err, ErrInvalidPath) && !errors.Is(err, ErrInvalidHost) {
			t.Fatalf("MatchHTTP error = %v", err)
		}
		if found || match != (HTTPMatch{}) {
			t.Fatalf("error result = (%+v,%t,%v), want zero,false,error", match, found, err)
		}
		return
	}
	if !found {
		if match != (HTTPMatch{}) {
			t.Fatalf("not-found match = %+v, want zero", match)
		}
		canonicalHost, canonicalErr := canonicalRequestHostname(request.Host)
		if canonicalErr == nil && (canonicalHost == "app.example.com" || canonicalHost == "xn--bcher-kva.example" || canonicalHost == "2001:db8::1") {
			t.Fatalf("known host %q returned not found", canonicalHost)
		}
		return
	}
	if match.Path != request.URL.Path || match.RawPath != request.URL.RawPath {
		t.Fatalf("matched path = (%q,%q), request = (%q,%q)", match.Path, match.RawPath, request.URL.Path, request.URL.RawPath)
	}
	canonicalHost, canonicalErr := canonicalRequestHostname(request.Host)
	if canonicalErr != nil || match.Hostname != canonicalHost {
		t.Fatalf("matched host = %q, want canonical request host %q (error=%v)", match.Hostname, canonicalHost, canonicalErr)
	}
	wantRoute := ""
	switch canonicalHost {
	case "app.example.com":
		wantRoute = "root"
		if request.URL.Path == "/foo" || strings.HasPrefix(request.URL.Path, "/foo/") {
			wantRoute = "foo"
		}
	case "xn--bcher-kva.example":
		wantRoute = "idna"
	case "2001:db8::1":
		wantRoute = "ipv6"
	}
	if wantRoute == "" || match.Route.ID != wantRoute || match.Route.Hostname != match.Hostname {
		t.Fatalf("matched route = (%q,%q,%q), want id=%q with matching host", match.Route.ID, match.Route.Hostname, match.Hostname, wantRoute)
	}
}

func safePathRune(character rune) rune {
	if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' {
		return character
	}
	return '-'
}
