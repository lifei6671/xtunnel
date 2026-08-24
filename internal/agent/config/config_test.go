package config

import (
	"strings"
	"testing"

	baseconfig "github.com/lifei6671/xtunnel/internal/config"
)

func TestLoadDefaultsAndPrecedence(t *testing.T) {
	result, err := Load(baseconfig.Options{
		YAML: []byte(`
server:
  endpoint: tunnel.example.com:7443
  tls:
    server_pin: sha256:dGVzdC1waW4=
logging:
  level: warn
`),
		Environment: []string{"XTUNNEL_LOGGING__LEVEL=error"},
		CLI: map[string]string{
			"data_dir":        t.TempDir(),
			"auth.token_file": t.TempDir() + "/token",
			"logging.level":   "debug",
		},
	})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if result.Logging.Level != "debug" {
		t.Fatalf("Logging.Level = %q, want debug", result.Logging.Level)
	}
	if result.Transport.TCP.TargetIdle != 8 {
		t.Fatalf("TargetIdle = %d, want 8", result.Transport.TCP.TargetIdle)
	}
	if result.Reconnect.MaxDelay.String() != "30s" {
		t.Fatalf("MaxDelay = %s, want 30s", result.Reconnect.MaxDelay)
	}
}

func TestLoadTLSModeContract(t *testing.T) {
	commonCLI := map[string]string{
		"data_dir":        t.TempDir(),
		"auth.token_file": t.TempDir() + "/token",
	}

	_, err := Load(baseconfig.Options{
		YAML: []byte("server:\n  endpoint: tunnel.example.com:7443\n"),
		CLI:  commonCLI,
	})
	if err == nil || !strings.Contains(err.Error(), "server_pin") {
		t.Fatalf("Load() error = %v, want missing server_pin", err)
	}

	result, err := Load(baseconfig.Options{
		YAML: []byte("server:\n  endpoint: tunnel.example.com:7443\n  tls:\n    mode: public\n"),
		CLI:  commonCLI,
	})
	if err != nil {
		t.Fatalf("Load() public TLS error = %v", err)
	}
	if result.Server.TLS.Mode != "public" {
		t.Fatalf("TLS.Mode = %q, want public", result.Server.TLS.Mode)
	}
}

func TestLoadRejectsCrossFieldViolations(t *testing.T) {
	tests := []struct {
		name  string
		yaml  string
		match string
	}{
		{
			name:  "pool ordering",
			yaml:  "transport:\n  tcp:\n    min_idle: 9\n    target_idle: 8\n",
			match: "min_idle <= target_idle",
		},
		{
			name:  "reconnect ordering",
			yaml:  "reconnect:\n  initial_delay: 31s\n  max_delay: 30s\n",
			match: "initial_delay",
		},
		{
			name:  "health concurrency",
			yaml:  "health:\n  max_concurrent: 3\n  max_concurrent_per_origin: 4\n",
			match: "max_concurrent_per_origin",
		},
		{
			name:  "zero duration",
			yaml:  "control:\n  write_timeout: 0s\n",
			match: "greater than zero",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			yaml := "server:\n  endpoint: tunnel.example.com:7443\n  tls:\n    server_pin: sha256:dGVzdC1waW4=\n" + test.yaml
			_, err := Load(baseconfig.Options{
				YAML: []byte(yaml),
				CLI: map[string]string{
					"data_dir":        t.TempDir(),
					"auth.token_file": t.TempDir() + "/token",
				},
			})
			if err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("Load() error = %v, want substring %q", err, test.match)
			}
		})
	}
}

func TestLoadRejectsNonNumericEndpointPort(t *testing.T) {
	_, err := Load(baseconfig.Options{
		YAML: []byte("server:\n  endpoint: tunnel.example.com:https\n  tls:\n    server_pin: sha256:dGVzdC1waW4=\n"),
		CLI: map[string]string{
			"data_dir":        t.TempDir(),
			"auth.token_file": t.TempDir() + "/token",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "port must be an integer") {
		t.Fatalf("Load() error = %v, want non-numeric port error", err)
	}
}
