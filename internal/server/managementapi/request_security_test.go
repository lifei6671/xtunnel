package managementapi

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestManagementLocalHTTPBoundary(t *testing.T) {
	policy, err := newManagementSecurityPolicy("http://127.0.0.1:8080", []string{"attacker.example:8080"}, []string{"0.0.0.0/0"})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, peer, host string
		valid            bool
	}{
		{"local", "127.0.0.1:12345", "127.0.0.1:8080", true},
		{"remote spoof", "192.0.2.1:12345", "127.0.0.1:8080", false},
		{"host spoof", "127.0.0.1:12345", "attacker.example:8080", false},
		{"other port", "127.0.0.1:12345", "127.0.0.1:8081", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/", nil)
			request.RemoteAddr, request.Host = test.peer, test.host
			request.Header.Set("X-Forwarded-For", "127.0.0.1")
			request.Header.Set("X-Forwarded-Proto", "https")
			request.Header.Set("X-Forwarded-Host", "127.0.0.1:8080")
			metadata, err := policy.metadata(request)
			if (err == nil) != test.valid {
				t.Fatalf("metadata error = %v, valid = %t", err, test.valid)
			}
			if test.valid && (metadata.scheme != "http" || !metadata.clientIP.IsLoopback()) {
				t.Fatalf("proxy headers changed local metadata: %+v", metadata)
			}
		})
	}
	for origin, want := range map[string]bool{"http://127.0.0.1:8080": true, "http://127.0.0.1:8081": false, "http://attacker.example:8080": false, "null": false} {
		if got := policy.allowsOrigin(origin); got != want {
			t.Fatalf("allowsOrigin(%q) = %t", origin, got)
		}
	}
}

func TestManagementSecurityPolicyTrustedProxyBoundary(t *testing.T) {
	policy, err := newManagementSecurityPolicy(
		"https://Admin.Example.",
		[]string{"console.example:8443"},
		[]string{"127.0.0.1/32"},
	)
	if err != nil {
		t.Fatalf("newManagementSecurityPolicy() error = %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/", nil)
	request.RemoteAddr = "127.0.0.1:45123"
	request.Header.Set("X-Forwarded-For", "198.51.100.7, 127.0.0.1")
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("X-Forwarded-Host", "admin.example")
	metadata, err := policy.metadata(request)
	if err != nil {
		t.Fatalf("metadata(trusted proxy) error = %v", err)
	}
	if metadata.clientIP != netip.MustParseAddr("198.51.100.7") || metadata.scheme != "https" || metadata.authority != "admin.example:443" {
		t.Fatalf("metadata(trusted proxy) = %#v", metadata)
	}

	request = httptest.NewRequest(http.MethodGet, "https://admin.example/", nil)
	request.RemoteAddr = "203.0.113.9:45123"
	request.Header.Set("X-Forwarded-For", "198.51.100.8")
	metadata, err = policy.metadata(request)
	if err != nil {
		t.Fatalf("metadata(untrusted peer) error = %v", err)
	}
	if metadata.clientIP != netip.MustParseAddr("203.0.113.9") {
		t.Fatalf("untrusted forwarded client IP = %s", metadata.clientIP)
	}
}

func TestManagementSecurityPolicyRejectsAmbiguousMetadata(t *testing.T) {
	policy, err := newManagementSecurityPolicy(
		"https://admin.example",
		nil,
		[]string{"127.0.0.1/32"},
	)
	if err != nil {
		t.Fatalf("newManagementSecurityPolicy() error = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*http.Request)
	}{
		{
			name: "disallowed host",
			mutate: func(request *http.Request) {
				request.Host = "attacker.example"
			},
		},
		{
			name: "remote plaintext even with https authority port",
			mutate: func(request *http.Request) {
				request.RemoteAddr = "203.0.113.9:45123"
				request.Host = "admin.example:443"
				request.Header.Del("X-Forwarded-Proto")
			},
		},
		{
			name: "ambiguous forwarded host",
			mutate: func(request *http.Request) {
				request.Header["X-Forwarded-Host"] = []string{"admin.example", "attacker.example"}
			},
		},
		{
			name: "invalid forwarded proto",
			mutate: func(request *http.Request) {
				request.Header.Set("X-Forwarded-Proto", "ftp")
			},
		},
		{
			name: "oversized forwarded chain",
			mutate: func(request *http.Request) {
				value := "127.0.0.1"
				for range managementMaxForwardedHops {
					value += ", 127.0.0.1"
				}
				request.Header.Set("X-Forwarded-For", value)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://admin.example/", nil)
			request.RemoteAddr = "127.0.0.1:45123"
			request.Header.Set("X-Forwarded-Proto", "https")
			test.mutate(request)
			if _, err := policy.metadata(request); err == nil {
				t.Fatal("metadata() error = nil")
			}
		})
	}
}

func TestManagementSecurityPolicyOriginIsExact(t *testing.T) {
	policy, err := newManagementSecurityPolicy("https://admin.example", nil, nil)
	if err != nil {
		t.Fatalf("newManagementSecurityPolicy() error = %v", err)
	}
	for value, want := range map[string]bool{
		"https://admin.example":      true,
		"https://ADMIN.EXAMPLE:443":  true,
		"http://admin.example":       false,
		"https://admin.example/path": false,
		"https://attacker.example":   false,
	} {
		if got := policy.allowsOrigin(value); got != want {
			t.Errorf("allowsOrigin(%q) = %t, want %t", value, got, want)
		}
	}
}
