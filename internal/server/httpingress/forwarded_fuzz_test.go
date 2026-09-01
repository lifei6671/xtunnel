package httpingress

import (
	"net/http"
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

const fuzzForwardedHeaderLimit = 1 << 20

func FuzzParseForwardedFor(f *testing.F) {
	chain32 := strings.TrimSuffix(strings.Repeat("192.0.2.1,", maxForwardedHops), ",")
	chain33 := chain32 + ",192.0.2.2"
	for _, seed := range []string{
		"198.51.100.25", "198.51.100.25, 10.1.1.1", "2001:db8::1",
		"::ffff:198.51.100.25", chain32, chain33, ",198.51.100.25",
		"198.51.100.25,", "198.51.100.25:443", "fe80::1%eth0", "garbage",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, value string) {
		if len(value) > fuzzForwardedHeaderLimit {
			return
		}
		hasZone := false
		for _, part := range strings.Split(value, ",") {
			if address, err := netip.ParseAddr(strings.TrimSpace(part)); err == nil && address.Zone() != "" {
				hasZone = true
			}
		}
		chain, err := parseForwardedFor(value)
		if err != nil {
			return
		}
		if hasZone {
			t.Fatalf("parseForwardedFor(%q) accepted an IPv6 zone", value)
		}
		if len(chain) == 0 || len(chain) > maxForwardedHops {
			t.Fatalf("chain length = %d", len(chain))
		}
		canonical := make([]string, len(chain))
		for index, address := range chain {
			if !address.IsValid() || address.Zone() != "" || address.Is4In6() {
				t.Fatalf("chain[%d] = %v is not canonical", index, address)
			}
			canonical[index] = address.String()
		}
		roundTrip, err := parseForwardedFor(strings.Join(canonical, ", "))
		if err != nil || !reflect.DeepEqual(roundTrip, chain) {
			t.Fatalf("canonical round trip = (%v,%v), want (%v,nil)", roundTrip, err, chain)
		}
	})
}

func FuzzNormalizeForwardedHeaders(f *testing.F) {
	seeds := []struct {
		xff, proto, host string
		mode             byte
	}{
		{"198.51.100.25, 10.2.2.2", "https", "origin.example.com:443", 1},
		{"fe80::1%eth0", "https", "origin.example.com", 1},
		{"garbage", "ftp", "bad host", 0},
		{"198.51.100.25", "http", "[2001:db8::1]:8443", 3},
	}
	for _, seed := range seeds {
		f.Add(seed.xff, seed.proto, seed.host, seed.mode)
	}

	trusted, err := newTrustedProxySet([]string{"10.0.0.0/8"})
	if err != nil {
		f.Fatalf("newTrustedProxySet() error = %v", err)
	}
	f.Fuzz(func(t *testing.T, forwardedFor, forwardedProto, forwardedHost string, mode byte) {
		if len(forwardedFor)+len(forwardedProto)+len(forwardedHost) > fuzzForwardedHeaderLimit {
			return
		}
		request := &http.Request{
			Host: "public.example.com", Header: make(http.Header), RemoteAddr: "198.51.100.25:443",
		}
		isTrusted := mode&1 != 0
		if isTrusted {
			request.RemoteAddr = "10.1.1.1:443"
		}
		request.Header["X-Forwarded-For"] = []string{forwardedFor}
		request.Header["X-Forwarded-Proto"] = []string{forwardedProto}
		request.Header["X-Forwarded-Host"] = []string{forwardedHost}
		request.Header["Forwarded"] = []string{"for=192.0.2.1"}
		request.Header["X-Real-IP"] = []string{"192.0.2.2"}
		request.Header["X-Forwarded-Unknown"] = []string{"drop"}
		if mode&2 != 0 {
			request.Header["x-forwarded-for"] = []string{forwardedFor}
		}
		if mode&4 != 0 {
			request.Header["x-forwarded-proto"] = []string{forwardedProto}
		}
		if mode&8 != 0 {
			request.Header["x-forwarded-host"] = []string{forwardedHost}
		}

		metadata, err := trusted.normalizeForwarded(request)
		if !isTrusted {
			if err != nil || metadata.clientIP.String() != "198.51.100.25" || metadata.scheme != "http" || metadata.host != request.Host {
				t.Fatalf("untrusted metadata = (%+v,%v)", metadata, err)
			}
		} else if err != nil {
			if metadata != (forwardedMetadata{}) {
				t.Fatalf("failed normalization returned partial metadata: %+v", metadata)
			}
			return
		} else {
			if !metadata.clientIP.IsValid() || metadata.clientIP.Zone() != "" || metadata.clientIP.Is4In6() {
				t.Fatalf("trusted client IP is not canonical: %v", metadata.clientIP)
			}
			chain, parseErr := parseForwardedFor(forwardedFor)
			if parseErr != nil {
				t.Fatalf("successful normalization has invalid X-Forwarded-For: %v", parseErr)
			}
			wantClient := chain[len(chain)-1]
			trustedPrefix := netip.MustParsePrefix("10.0.0.0/8")
			for index := len(chain) - 1; index >= 0; index-- {
				wantClient = chain[index]
				if !trustedPrefix.Contains(chain[index]) {
					break
				}
			}
			if metadata.clientIP != wantClient {
				t.Fatalf("trusted client IP = %v, want %v from right-to-left proxy stripping", metadata.clientIP, wantClient)
			}
			if metadata.scheme != "http" && metadata.scheme != "https" {
				t.Fatalf("trusted scheme = %q", metadata.scheme)
			}
			if !validForwardedHost(metadata.host) {
				t.Fatalf("trusted host = %q", metadata.host)
			}
		}

		rewriteForwardedHeaders(request.Header, metadata)
		assertSingleFuzzHeader(t, request.Header, "X-Forwarded-For", metadata.clientIP.String())
		assertSingleFuzzHeader(t, request.Header, "X-Forwarded-Proto", metadata.scheme)
		assertSingleFuzzHeader(t, request.Header, "X-Forwarded-Host", metadata.host)
		for key := range request.Header {
			lower := strings.ToLower(key)
			if lower == "forwarded" || lower == "x-real-ip" || lower == "x-forwarded-unknown" {
				t.Fatalf("rewrite retained untrusted header %q", key)
			}
		}
	})
}

func assertSingleFuzzHeader(t *testing.T, header http.Header, name, want string) {
	t.Helper()
	values := header.Values(name)
	if len(values) != 1 || values[0] != want {
		t.Fatalf("%s = %v, want [%q]", name, values, want)
	}
}
