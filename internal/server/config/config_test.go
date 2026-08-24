package config

import (
	"strings"
	"testing"

	baseconfig "github.com/lifei6671/xtunnel/internal/config"
)

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
			"server.data_dir": t.TempDir(),
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
	if result.Limits.MaxAgents != 1000 {
		t.Fatalf("Limits.MaxAgents = %d, want 1000", result.Limits.MaxAgents)
	}
	if result.Transport.TCP.WorkAcquireTimeout.String() != "2s" {
		t.Fatalf("WorkAcquireTimeout = %s, want 2s", result.Transport.TCP.WorkAcquireTimeout)
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
		CLI:  map[string]string{"server.data_dir": t.TempDir()},
	})
	if err == nil || !strings.Contains(err.Error(), "cert_file") {
		t.Fatalf("Load() error = %v, want missing cert_file", err)
	}

	result, err := Load(baseconfig.Options{
		YAML: []byte(baseYAML + "    cert_file: /run/xtunnel/server.crt\n    key_file: /run/xtunnel/server.key\n"),
		CLI:  map[string]string{"server.data_dir": t.TempDir()},
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
			yaml:  "agent_runtime:\n  heartbeat_interval: 11s\n  heartbeat_timeout: 30s\n",
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
				CLI:  map[string]string{"server.data_dir": t.TempDir()},
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
			"server.data_dir":            t.TempDir(),
			"management.trusted_proxies": `["not-a-cidr"]`,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid CIDR") {
		t.Fatalf("Load() error = %v, want invalid CIDR error", err)
	}
}
