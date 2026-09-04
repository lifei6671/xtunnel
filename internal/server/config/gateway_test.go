package config

import "testing"

func TestAgentGatewayPublicEndpoint(t *testing.T) {
	if host, port, err := (AgentGateway{Listen: "127.0.0.1:0", PublicHostname: "localhost"}).PublicEndpoint(); err != nil || host != "localhost" || port != 0 {
		t.Fatalf("dynamic listen endpoint = %q, %d, %v", host, port, err)
	}
	for _, test := range []struct {
		name, value, host string
		port              uint16
	}{
		{"dns", "gateway.example.test", "gateway.example.test", 8443},
		{"ipv4", "127.0.0.1", "127.0.0.1", 8443},
		{"ipv6", "::1", "::1", 8443},
		{"dns port", "gateway.example.test:443", "gateway.example.test", 443},
		{"ipv4 port", "127.0.0.1:9443", "127.0.0.1", 9443},
		{"ipv6 port", "[::1]:9443", "::1", 9443},
	} {
		t.Run(test.name, func(t *testing.T) {
			host, port, err := (AgentGateway{Listen: "0.0.0.0:8443", PublicHostname: test.value}).PublicEndpoint()
			if err != nil || host != test.host || port != test.port {
				t.Fatalf("PublicEndpoint() = %q, %d, %v; want %q, %d", host, port, err, test.host, test.port)
			}
		})
	}
	for _, value := range []string{"", "http://127.0.0.1:8443", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:no", "127.0.0.1:", "[::1]", "[invalid]:443", "host/path", "host name", "user@host", "host?query", "[fe80::1%eth0]:443"} {
		t.Run("invalid "+value, func(t *testing.T) {
			if _, _, err := (AgentGateway{Listen: ":8443", PublicHostname: value}).PublicEndpoint(); err == nil {
				t.Fatal("PublicEndpoint() accepted invalid public hostname")
			}
		})
	}
}
