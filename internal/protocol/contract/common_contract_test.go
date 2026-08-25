package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommonProtoFreezesSharedEnums(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "..", "..", "api", "proto", "common.proto"))
	if err != nil {
		t.Fatalf("read common.proto: %v", err)
	}

	text := string(content)
	for _, declaration := range []string{
		`syntax = "proto3";`,
		"package xtunnel.protocol.v1;",
		`option go_package = "github.com/lifei6671/xtunnel/internal/protocol/gen;protocolv1";`,
		"agent_id=ag_<ULID>",
		"instance_id=ai_<ULID>",
		"session_id=sess_<ULID>",
		"work_id=work_<ULID>",
		"connection_id=conn_<ULID>",
		"budget_lease_id=lease_<ULID>",
		"drain_id=drain_<ULID>.",
	} {
		if !strings.Contains(text, declaration) {
			t.Fatalf("common.proto missing %q", declaration)
		}
	}

	assertEnum(t, text, "ErrorCode", []string{
		"ERROR_CODE_OK = 0;",
		"ERROR_CODE_TUNNEL_NOT_FOUND = 0x1001;",
		"ERROR_CODE_TUNNEL_DISABLED = 0x1002;",
		"ERROR_CODE_AGENT_OFFLINE = 0x1003;",
		"ERROR_CODE_NO_HEALTHY_INSTANCE = 0x1004;",
		"ERROR_CODE_CONFIG_NOT_OBSERVED = 0x1005;",
		"ERROR_CODE_ORIGIN_REFUSED = 0x2001;",
		"ERROR_CODE_ORIGIN_TIMEOUT = 0x2002;",
		"ERROR_CODE_ORIGIN_UNREACHABLE = 0x2003;",
		"ERROR_CODE_ORIGIN_RESET = 0x2004;",
		"ERROR_CODE_ORIGIN_TLS_ERROR = 0x2005;",
		"ERROR_CODE_WORK_POOL_EXHAUSTED = 0x3001;",
		"ERROR_CODE_AGENT_BUSY = 0x3002;",
		"ERROR_CODE_OPEN_DRAINING = 0x3003;",
		"ERROR_CODE_HEALTH_BUDGET_EXCEEDED = 0x3004;",
		"ERROR_CODE_TOKEN_INVALID = 0x4001;",
		"ERROR_CODE_TOKEN_REVOKED = 0x4002;",
		"ERROR_CODE_AGENT_REVOKED = 0x4003;",
		"ERROR_CODE_SESSION_INVALID = 0x4004;",
		"ERROR_CODE_SESSION_RESOURCE_EXHAUSTED = 0x4005;",
		"ERROR_CODE_PROTOCOL_ERROR = 0x5001;",
		"ERROR_CODE_VERSION_UNSUPPORTED = 0x5002;",
		"ERROR_CODE_INTERNAL_ERROR = 0x6001;",
	})
	assertEnum(t, text, "WorkReadyStatus", []string{
		"WORK_READY_STATUS_UNSPECIFIED = 0;",
		"WORK_READY_STATUS_READY = 1;",
		"WORK_READY_STATUS_REJECTED = 2;",
	})
	assertEnum(t, text, "OpenStatus", []string{
		"OPEN_STATUS_UNSPECIFIED = 0;",
		"OPEN_STATUS_OK = 1;",
		"OPEN_STATUS_ERROR = 2;",
	})
	assertEnum(t, text, "IngressType", []string{
		"INGRESS_TYPE_UNSPECIFIED = 0;",
		"INGRESS_TYPE_HTTP = 1;",
		"INGRESS_TYPE_TCP = 2;",
	})
	assertEnum(t, text, "HealthType", []string{
		"HEALTH_TYPE_UNSPECIFIED = 0;",
		"HEALTH_TYPE_DISABLED = 1;",
		"HEALTH_TYPE_TCP = 2;",
		"HEALTH_TYPE_HTTP = 3;",
	})
	assertEnum(t, text, "HealthStatus", []string{
		"HEALTH_STATUS_UNKNOWN = 0;",
		"HEALTH_STATUS_HEALTHY = 1;",
		"HEALTH_STATUS_UNHEALTHY = 2;",
	})
	assertEnum(t, text, "ConfigApplyStatus", []string{
		"CONFIG_APPLY_STATUS_UNSPECIFIED = 0;",
		"CONFIG_APPLY_STATUS_APPLIED = 1;",
		"CONFIG_APPLY_STATUS_REJECTED = 2;",
	})
}

func assertEnum(t *testing.T, content, name string, want []string) {
	t.Helper()
	start := strings.Index(content, "enum "+name+" {")
	if start < 0 {
		t.Fatalf("common.proto missing enum %s", name)
	}
	end := strings.Index(content[start:], "\n}")
	if end < 0 {
		t.Fatalf("common.proto enum %s is not closed", name)
	}

	var got []string
	for _, line := range strings.Split(content[start:start+end], "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, " = ") && strings.HasSuffix(line, ";") {
			got = append(got, line)
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("enum %s values = %q, want %q", name, got, want)
	}
}
