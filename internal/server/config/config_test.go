package config

import (
	"runtime"
	"strings"
	"testing"

	baseconfig "github.com/lifei6671/xtunnel/internal/config"
	"github.com/lifei6671/xtunnel/internal/server/pathprofile"
)

func testDataDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return pathprofile.AutomaticDataDir
	}
	return t.TempDir()
}

func TestLoadUsesStableParentDataLeafByDefault(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("production Server defaults use Linux absolute paths")
	}
	result, err := Load(baseconfig.Options{YAML: []byte(`
management:
  public_url: https://admin.example.com
agent_gateway:
  public_hostname: tunnel.example.com
`)})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if result.Server.DataDir != "/var/lib/xtunnel/data" {
		t.Fatalf("Server.DataDir = %q, want /var/lib/xtunnel/data", result.Server.DataDir)
	}
}

func TestLoadDefaultsAndPrecedence(t *testing.T) {
	result, err := Load(baseconfig.Options{
		YAML: []byte(`
management:
  public_url: https://admin.example.com
agent_gateway:
  public_hostname: tunnel.example.com
logging:
  level: warn
`),
		Environment: []string{"XTUNNEL_LOGGING__LEVEL=error"},
		CLI: map[string]string{
			"server.data_dir": testDataDir(t),
			"logging.level":   "debug",
		},
	})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if result.Logging.Level != "debug" {
		t.Fatalf("Logging.Level = %q, want debug", result.Logging.Level)
	}
	if result.Management.Listen != "127.0.0.1:8080" {
		t.Fatalf("Management.Listen = %q, want default", result.Management.Listen)
	}
	if result.Limits.MaxTunnels != 1000 {
		t.Fatalf("Limits.MaxTunnels = %d, want 1000", result.Limits.MaxTunnels)
	}
	if result.Limits.MaxServicesPerTunnel != 1000 {
		t.Fatalf("Limits.MaxServicesPerTunnel = %d, want 1000", result.Limits.MaxServicesPerTunnel)
	}
	if result.Limits.MaxHTTPBodyBytes != 2*1024*1024*1024 {
		t.Fatalf("Limits.MaxHTTPBodyBytes = %d, want 2 GiB", result.Limits.MaxHTTPBodyBytes)
	}
	if result.Transport.TCP.WorkAcquireTimeout.String() != "2s" {
		t.Fatalf("WorkAcquireTimeout = %s, want 2s", result.Transport.TCP.WorkAcquireTimeout)
	}
}

func TestLoadMaxHTTPBodyBytesBounds(t *testing.T) {
	baseYAML := "management:\n  public_url: https://admin.example.com\nagent_gateway:\n  public_hostname: tunnel.example.com\n"

	result, err := Load(baseconfig.Options{
		YAML: []byte(baseYAML + "limits:\n  max_http_body_bytes: 1099511627776\n"),
		CLI:  map[string]string{"server.data_dir": testDataDir(t)},
	})
	if err != nil {
		t.Fatalf("Load() at maximum error = %v", err)
	}
	if result.Limits.MaxHTTPBodyBytes != 1099511627776 {
		t.Fatalf("Limits.MaxHTTPBodyBytes = %d, want 1 TiB", result.Limits.MaxHTTPBodyBytes)
	}

	_, err = Load(baseconfig.Options{
		YAML: []byte(baseYAML + "limits:\n  max_http_body_bytes: 1099511627777\n"),
		CLI:  map[string]string{"server.data_dir": testDataDir(t)},
	})
	if err == nil || !strings.Contains(err.Error(), "max_http_body_bytes") {
		t.Fatalf("Load() above maximum error = %v, want max_http_body_bytes", err)
	}
}

func TestLoadMaxServicesPerTunnelAbsoluteLimit(t *testing.T) {
	baseYAML := "management:\n  public_url: https://admin.example.com\nagent_gateway:\n  public_hostname: tunnel.example.com\n"

	result, err := Load(baseconfig.Options{
		YAML: []byte(baseYAML + "limits:\n  max_services_per_tunnel: 1000\n"),
		CLI:  map[string]string{"server.data_dir": testDataDir(t)},
	})
	if err != nil {
		t.Fatalf("Load() at absolute limit error = %v", err)
	}
	if result.Limits.MaxServicesPerTunnel != 1000 {
		t.Fatalf("Limits.MaxServicesPerTunnel = %d, want 1000", result.Limits.MaxServicesPerTunnel)
	}

	_, err = Load(baseconfig.Options{
		YAML: []byte(baseYAML + "limits:\n  max_services_per_tunnel: 1001\n"),
		CLI:  map[string]string{"server.data_dir": testDataDir(t)},
	})
	if err == nil || !strings.Contains(err.Error(), "max_services_per_tunnel") {
		t.Fatalf("Load() above absolute limit error = %v, want max_services_per_tunnel", err)
	}
}

func TestLoadTLSModeContract(t *testing.T) {
	baseYAML := `
management:
  public_url: https://admin.example.com
agent_gateway:
  public_hostname: tunnel.example.com
  tls:
    mode: public
`

	_, err := Load(baseconfig.Options{
		YAML: []byte(baseYAML),
		CLI:  map[string]string{"server.data_dir": testDataDir(t)},
	})
	if err == nil || !strings.Contains(err.Error(), "cert_file") {
		t.Fatalf("Load() error = %v, want missing cert_file", err)
	}

	result, err := Load(baseconfig.Options{
		YAML: []byte(baseYAML + "    cert_file: /run/xtunnel/server.crt\n    key_file: /run/xtunnel/server.key\n"),
		CLI:  map[string]string{"server.data_dir": testDataDir(t)},
	})
	if err != nil {
		t.Fatalf("Load() public TLS error = %v", err)
	}
	if result.AgentGateway.TLS.Mode != "public" {
		t.Fatalf("TLS.Mode = %q, want public", result.AgentGateway.TLS.Mode)
	}
}

func TestLoadRejectsCrossFieldViolations(t *testing.T) {
	tests := []struct {
		name  string
		yaml  string
		match string
	}{
		{
			name:  "port range",
			yaml:  "tcp_ingress:\n  min_port: 20000\n  max_port: 10000\n",
			match: "min_port",
		},
		{
			name:  "heartbeat ratio",
			yaml:  "connector_runtime:\n  heartbeat_interval: 11s\n  heartbeat_timeout: 30s\n",
			match: "one third",
		},
		{
			name:  "work connection budget",
			yaml:  "limits:\n  max_work_connections: 10\n  max_idle_work_connections: 11\n",
			match: "max_idle_work_connections",
		},
		{
			name:  "zero duration",
			yaml:  "control:\n  write_timeout: 0s\n",
			match: "greater than zero",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			yaml := "management:\n  public_url: https://admin.example.com\nagent_gateway:\n  public_hostname: tunnel.example.com\n" + test.yaml
			_, err := Load(baseconfig.Options{
				YAML: []byte(yaml),
				CLI:  map[string]string{"server.data_dir": testDataDir(t)},
			})
			if err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("Load() error = %v, want substring %q", err, test.match)
			}
		})
	}
}

func TestLoadRejectsInvalidTrustedProxy(t *testing.T) {
	_, err := Load(baseconfig.Options{
		YAML: []byte("management:\n  public_url: https://admin.example.com\nagent_gateway:\n  public_hostname: tunnel.example.com\n"),
		CLI: map[string]string{
			"server.data_dir":            testDataDir(t),
			"management.trusted_proxies": `["not-a-cidr"]`,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid CIDR") {
		t.Fatalf("Load() error = %v, want invalid CIDR error", err)
	}
}

func TestLoadAcceptsDualStackAndIPv6ListenAddresses(t *testing.T) {
	result, err := Load(baseconfig.Options{
		YAML: []byte("management:\n  public_url: https://admin.example.com\nagent_gateway:\n  public_hostname: tunnel.example.com\n"),
		CLI: map[string]string{
			"server.data_dir":      testDataDir(t),
			"management.listen":    ":8080",
			"http_ingress.listen":  "[::]:8081",
			"agent_gateway.listen": ":7443",
			"tcp_ingress.bind":     "::",
			"metrics.listen":       "[::]:9090",
		},
	})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if result.AgentGateway.Listen != ":7443" || result.TCPIngress.Bind != "::" {
		t.Fatalf("AgentGateway.Listen = %q, TCPIngress.Bind = %q", result.AgentGateway.Listen, result.TCPIngress.Bind)
	}
}

func TestLoadRejectsUnbracketedIPv6ListenAddress(t *testing.T) {
	_, err := Load(baseconfig.Options{
		YAML: []byte("management:\n  public_url: https://admin.example.com\nagent_gateway:\n  public_hostname: tunnel.example.com\n"),
		CLI: map[string]string{
			"server.data_dir":      testDataDir(t),
			"agent_gateway.listen": ":::7443",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "too many colons") {
		t.Fatalf("Load() error = %v, want bracketed IPv6 error", err)
	}
}
