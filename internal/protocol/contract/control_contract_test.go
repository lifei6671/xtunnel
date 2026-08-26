package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestControlProtoFreezesTokenAndControlContract(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "..", "..", "api", "proto", "control.proto"))
	if err != nil {
		t.Fatalf("读取 control.proto 失败: %v", err)
	}

	text := string(content)
	for _, declaration := range []string{
		`import "common.proto";`,
		"message ConnectionToken {",
		"uint32 format_version = 1;",
		"GatewayEndpoint endpoint = 2;",
		"TlsTrustDescriptor tls_trust = 3;",
		"string tunnel_id = 4;",
		"string token_id = 5;",
		"uint64 token_version = 6;",
		"bytes authentication_secret = 7;",
		"bytes integrity_tag = 8;",
		"message ConnectorAuthRequest {",
		"string connector_id = 2;",
		"repeated string capabilities = 9;",
		"message ConnectorAuthResult {",
		"ConnectorAuthSuccess success = 1;",
		"ConnectorAuthFailure failure = 2;",
		"message ControlEnvelope {",
		"Heartbeat heartbeat = 10;",
		"TunnelSnapshot config_snapshot = 11;",
		"ConfigAck config_ack = 12;",
		"WorkDemand work_demand = 13;",
		"ServiceHealthBatch service_health_batch = 14;",
		"message TunnelSnapshot {",
		"repeated ServiceConfig services = 3;",
		"message ServiceConfig {",
		"string service_id = 1;",
		"message ServiceHealth {",
		"uint64 service_revision = 6;",
		"message ServiceHealthBatch {",
		"DrainRequest drain_request = 15;",
		"Error error = 16;",
		"DrainAck drain_ack = 17;",
	} {
		if !strings.Contains(text, declaration) {
			t.Fatalf("control.proto 缺少冻结声明 %q", declaration)
		}
	}
}
