package application

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
)

func TestSystemReadServiceProjectsInfoAndSafeConfig(t *testing.T) {
	startedAt := time.Date(2026, 8, 29, 12, 0, 0, 0, time.FixedZone("test", 8*60*60))
	config := validSystemReadConfig()
	service, err := NewSystemReadService("v0.1.0", startedAt, config)
	if err != nil {
		t.Fatalf("NewSystemReadService() error = %v", err)
	}
	service.now = func() time.Time { return startedAt.Add(125*time.Second + 900*time.Millisecond) }

	info, err := service.Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info.Version != "v0.1.0" || info.GoVersion != runtime.Version() ||
		info.ProtocolVersion != "v1" || info.OS != runtime.GOOS || info.Arch != runtime.GOARCH ||
		!info.StartedAt.Equal(startedAt.UTC()) || info.UptimeSeconds != 125 {
		t.Fatalf("Info() = %#v", info)
	}

	projected, err := service.Config(context.Background())
	if err != nil {
		t.Fatalf("Config() error = %v", err)
	}
	if projected.Management.PublicURL != config.Management.PublicURL ||
		projected.AgentGateway.PublicHostname != config.AgentGateway.PublicHostname ||
		projected.AgentGateway.TLSMode != config.AgentGateway.TLS.Mode ||
		projected.TCPIngress.MinPort != config.TCPIngress.MinPort ||
		projected.TCPIngress.MaxPort != config.TCPIngress.MaxPort ||
		projected.Logging.Level != config.Logging.Level || !projected.ChangesRequireRestart {
		t.Fatalf("Config() = %#v", projected)
	}
	wantLimits := SystemPublicLimits{
		MaxTunnels: config.Limits.MaxTunnels, MaxConnectors: config.Limits.MaxConnectors,
		MaxConnectorsPerTunnel:              config.Limits.MaxConnectorsPerTunnel,
		MaxServicesPerTunnel:                config.Limits.MaxServicesPerTunnel,
		MaxActiveConnections:                config.Limits.MaxActiveConnections,
		MaxConnectionsPerTunnel:             config.Limits.MaxConnectionsPerTunnel,
		MaxConnectionsPerService:            config.Limits.MaxConnectionsPerService,
		MaxConnectionsPerSourceIP:           config.Limits.MaxConnectionsPerSourceIP,
		MaxOpenRatePerSourceIP:              config.Limits.MaxOpenRatePerSourceIP,
		MaxOpenBurstPerSourceIP:             config.Limits.MaxOpenBurstPerSourceIP,
		MaxHTTPRequestsPerSourceIPPerSecond: config.Limits.MaxHTTPRequestsPerSourceIPPerSecond,
		MaxHTTPHeaderBytes:                  config.Limits.MaxHTTPHeaderBytes,
		MaxHTTPBodyBytes:                    config.Limits.MaxHTTPBodyBytes,
	}
	if projected.Limits != wantLimits {
		t.Fatalf("Config().Limits = %#v, want %#v", projected.Limits, wantLimits)
	}
	wantFields := []string{
		"AgentGateway.PublicHostname", "AgentGateway.TLSMode", "ChangesRequireRestart",
		"Limits.MaxActiveConnections", "Limits.MaxConnectionsPerService",
		"Limits.MaxConnectionsPerSourceIP", "Limits.MaxConnectionsPerTunnel",
		"Limits.MaxConnectors", "Limits.MaxConnectorsPerTunnel", "Limits.MaxHTTPBodyBytes",
		"Limits.MaxHTTPHeaderBytes", "Limits.MaxHTTPRequestsPerSourceIPPerSecond",
		"Limits.MaxOpenBurstPerSourceIP", "Limits.MaxOpenRatePerSourceIP",
		"Limits.MaxServicesPerTunnel", "Limits.MaxTunnels", "Logging.Level",
		"Management.PublicURL", "TCPIngress.MaxPort", "TCPIngress.MinPort",
	}
	gotFields := exportedLeafFields(reflect.TypeOf(projected), "")
	slices.Sort(gotFields)
	slices.Sort(wantFields)
	if !slices.Equal(gotFields, wantFields) {
		t.Fatalf("SystemConfig exported fields = %v, want %v", gotFields, wantFields)
	}
	formatted := fmt.Sprintf("%#v", projected)
	for _, secret := range []string{
		config.Server.DataDir,
		config.Management.Listen,
		config.Management.AllowedHosts[0],
		config.Management.TrustedProxies[0],
		config.AgentGateway.Listen,
		config.AgentGateway.TLS.CertFile,
		config.AgentGateway.TLS.KeyFile,
	} {
		if strings.Contains(formatted, secret) {
			t.Fatalf("SystemConfig leaked source-only value %q", secret)
		}
	}
}

func TestSystemReadServiceAggregatesRealHealthChecks(t *testing.T) {
	readyMessage := "SQLite 可读写"
	degradedMessage := "Gateway Listener 未就绪"
	ready := SystemHealthCheck(func(context.Context) SystemHealthCheckResult {
		return SystemHealthCheckResult{Name: "sqlite", Status: SystemHealthReady, Message: &readyMessage}
	})
	degraded := SystemHealthCheck(func(context.Context) SystemHealthCheckResult {
		return SystemHealthCheckResult{Name: "agent_gateway", Status: SystemHealthDegraded, Message: &degradedMessage}
	})
	checkedAt := time.Date(2026, 8, 29, 5, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		checks     []SystemHealthCheck
		wantStatus SystemHealthStatus
		wantNames  []string
	}{
		{name: "all ready", checks: []SystemHealthCheck{ready}, wantStatus: SystemHealthReady, wantNames: []string{"sqlite"}},
		{name: "one degraded", checks: []SystemHealthCheck{ready, degraded}, wantStatus: SystemHealthDegraded, wantNames: []string{"sqlite", "agent_gateway"}},
		{name: "no checks never fabricates ready", wantStatus: SystemHealthDegraded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, err := NewSystemReadService("v0.1.0", checkedAt.Add(-time.Hour), validSystemReadConfig(), test.checks...)
			if err != nil {
				t.Fatalf("NewSystemReadService() error = %v", err)
			}
			service.now = func() time.Time { return checkedAt }
			result, err := service.Health(context.Background())
			if err != nil {
				t.Fatalf("Health() error = %v", err)
			}
			if result.Status != test.wantStatus || !result.CheckedAt.Equal(checkedAt) || len(result.Checks) != len(test.wantNames) {
				t.Fatalf("Health() = %#v", result)
			}
			for index, name := range test.wantNames {
				if result.Checks[index].Name != name {
					t.Fatalf("Health().Checks[%d].Name = %q, want %q", index, result.Checks[index].Name, name)
				}
			}
		})
	}
}

func TestSystemReadServiceRejectsInvalidHealthResultsAndContexts(t *testing.T) {
	tests := []struct {
		name   string
		checks []SystemHealthCheck
	}{
		{name: "empty name", checks: []SystemHealthCheck{SystemHealthCheck(func(context.Context) SystemHealthCheckResult {
			return SystemHealthCheckResult{Status: SystemHealthReady}
		})}},
		{name: "unknown status", checks: []SystemHealthCheck{SystemHealthCheck(func(context.Context) SystemHealthCheckResult {
			return SystemHealthCheckResult{Name: "sqlite", Status: "UNKNOWN"}
		})}},
		{name: "duplicate names", checks: []SystemHealthCheck{
			SystemHealthCheck(func(context.Context) SystemHealthCheckResult {
				return SystemHealthCheckResult{Name: "sqlite", Status: SystemHealthReady}
			}),
			SystemHealthCheck(func(context.Context) SystemHealthCheckResult {
				return SystemHealthCheckResult{Name: "sqlite", Status: SystemHealthDegraded}
			}),
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, err := NewSystemReadService("v0.1.0", time.Now().Add(-time.Hour), validSystemReadConfig(), test.checks...)
			if err != nil {
				t.Fatalf("NewSystemReadService() error = %v", err)
			}
			if _, err := service.Health(context.Background()); !errors.Is(err, ErrSystemReadInput) {
				t.Fatalf("Health() error = %v, want ErrSystemReadInput", err)
			}
		})
	}

	service, err := NewSystemReadService("v0.1.0", time.Now().Add(-time.Hour), validSystemReadConfig())
	if err != nil {
		t.Fatalf("NewSystemReadService() error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.Health(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Health(canceled) error = %v", err)
	}
	if _, err := service.Info(nil); !errors.Is(err, ErrSystemReadInput) {
		t.Fatalf("Info(nil) error = %v", err)
	}
	if _, err := service.Config(nil); !errors.Is(err, ErrSystemReadInput) {
		t.Fatalf("Config(nil) error = %v", err)
	}
}

func TestNewSystemReadServiceValidatesProjectionBoundary(t *testing.T) {
	startedAt := time.Date(2026, 8, 29, 4, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		version string
		started time.Time
		mutate  func(*serverconfig.Config)
		checks  []SystemHealthCheck
	}{
		{name: "empty version", started: startedAt},
		{name: "zero started at", version: "v0.1.0"},
		{name: "invalid public URL", version: "v0.1.0", started: startedAt, mutate: func(config *serverconfig.Config) { config.Management.PublicURL = "http://admin.example" }},
		{name: "public URL contains userinfo", version: "v0.1.0", started: startedAt, mutate: func(config *serverconfig.Config) { config.Management.PublicURL = "https://secret@admin.example" }},
		{name: "empty public hostname", version: "v0.1.0", started: startedAt, mutate: func(config *serverconfig.Config) { config.AgentGateway.PublicHostname = "" }},
		{name: "invalid TLS mode", version: "v0.1.0", started: startedAt, mutate: func(config *serverconfig.Config) { config.AgentGateway.TLS.Mode = "off" }},
		{name: "invalid port range", version: "v0.1.0", started: startedAt, mutate: func(config *serverconfig.Config) { config.TCPIngress.MinPort = 6000; config.TCPIngress.MaxPort = 5000 }},
		{name: "invalid logging level", version: "v0.1.0", started: startedAt, mutate: func(config *serverconfig.Config) { config.Logging.Level = "trace" }},
		{name: "invalid public limit", version: "v0.1.0", started: startedAt, mutate: func(config *serverconfig.Config) { config.Limits.MaxTunnels = 0 }},
		{name: "nil check", version: "v0.1.0", started: startedAt, checks: []SystemHealthCheck{nil}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validSystemReadConfig()
			if test.mutate != nil {
				test.mutate(&config)
			}
			if _, err := NewSystemReadService(test.version, test.started, config, test.checks...); !errors.Is(err, ErrSystemReadInput) {
				t.Fatalf("NewSystemReadService() error = %v, want ErrSystemReadInput", err)
			}
		})
	}

	service, err := NewSystemReadService("v0.1.0", startedAt, validSystemReadConfig())
	if err != nil {
		t.Fatalf("NewSystemReadService() error = %v", err)
	}
	service.now = func() time.Time { return startedAt.Add(-time.Second) }
	if _, err := service.Info(context.Background()); !errors.Is(err, ErrSystemReadInput) {
		t.Fatalf("Info() with clock before start error = %v, want ErrSystemReadInput", err)
	}
}

func validSystemReadConfig() serverconfig.Config {
	return serverconfig.Config{
		Server: serverconfig.Server{DataDir: "C:/private/xtunnel-data"},
		Management: serverconfig.Management{
			Listen: "127.0.0.1:8080", PublicURL: "https://admin.example",
			AllowedHosts: []string{"private-admin.internal"}, TrustedProxies: []string{"10.0.0.0/8"},
		},
		AgentGateway: serverconfig.AgentGateway{
			Listen: "0.0.0.0:9443", PublicHostname: "gateway.example",
			TLS: serverconfig.AgentGatewayTLS{
				Mode: "pinned", CertFile: "C:/private/gateway.crt", KeyFile: "C:/private/gateway.key",
			},
		},
		TCPIngress: serverconfig.TCPIngress{Bind: "0.0.0.0", MinPort: 20000, MaxPort: 20100},
		Logging:    serverconfig.Logging{Level: "info", Format: "json"},
		Limits: serverconfig.Limits{
			MaxTunnels: 10, MaxConnectors: 20, MaxConnectorsPerTunnel: 5, MaxServicesPerTunnel: 30,
			MaxActiveConnections: 100, MaxConnectionsPerTunnel: 50, MaxConnectionsPerService: 25,
			MaxConnectionsPerSourceIP: 10, MaxOpenRatePerSourceIP: 9, MaxOpenBurstPerSourceIP: 8,
			MaxHTTPRequestsPerSourceIPPerSecond: 7, MaxHTTPHeaderBytes: 65536, MaxHTTPBodyBytes: 1048576,
		},
	}
}

func exportedLeafFields(value reflect.Type, prefix string) []string {
	result := make([]string, 0)
	for index := range value.NumField() {
		field := value.Field(index)
		name := field.Name
		if prefix != "" {
			name = prefix + "." + name
		}
		if field.Type.Kind() == reflect.Struct {
			result = append(result, exportedLeafFields(field.Type, name)...)
			continue
		}
		result = append(result, name)
	}
	return result
}
