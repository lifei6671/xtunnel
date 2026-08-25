package contract

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestWorkProtoFreezesWireContract(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "..", "..", "api", "proto", "work.proto"))
	if err != nil {
		t.Fatalf("read work.proto: %v", err)
	}

	text := string(content)
	for _, declaration := range []string{
		`syntax = "proto3";`,
		"package xtunnel.protocol.v1;",
		`option go_package = "github.com/lifei6671/xtunnel/internal/protocol/gen;protocolv1";`,
		`import "common.proto";`,
	} {
		if !strings.Contains(text, declaration) {
			t.Fatalf("work.proto missing %q", declaration)
		}
	}

	assertWorkMessages(t, text, []string{"WorkHello", "WorkReady", "OpenRequest", "OpenResponse"})
	assertWorkMessageFields(t, text, "WorkHello", []string{
		"string agent_id = 1;",
		"string instance_id = 2;",
		"string session_id = 3;",
		"string work_id = 4;",
		"reserved 5, 6;",
		"bytes nonce = 7;",
		"bytes mac = 8;",
		"string budget_lease_id = 9;",
	})
	assertWorkMessageFields(t, text, "WorkReady", []string{
		"string work_id = 1;",
		"WorkReadyStatus status = 2;",
		"ErrorCode error_code = 3;",
	})
	assertWorkMessageFields(t, text, "OpenRequest", []string{
		"uint32 protocol_version = 1;",
		"string connection_id = 2;",
		"string tunnel_id = 3;",
		"string trace_id = 4;",
		"string client_addr = 5;",
		"uint64 timestamp_ms = 6;",
		"IngressType ingress_type = 7;",
		"string traceparent = 8;",
		"string tracestate = 9;",
	})
	assertWorkMessageFields(t, text, "OpenResponse", []string{
		"string connection_id = 1;",
		"OpenStatus status = 2;",
		"ErrorCode error_code = 3;",
		"uint32 origin_connect_latency_ms = 4;",
	})
}

func assertWorkMessages(t *testing.T, content string, want []string) {
	t.Helper()

	matches := regexp.MustCompile(`(?m)^message\s+([A-Za-z][A-Za-z0-9_]*)\s*\{`).FindAllStringSubmatch(content, -1)
	got := make([]string, 0, len(matches))
	for _, match := range matches {
		got = append(got, match[1])
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("work.proto messages = %q, want %q", got, want)
	}
}

func assertWorkMessageFields(t *testing.T, content, name string, want []string) {
	t.Helper()

	start := strings.Index(content, "message "+name+" {")
	if start < 0 {
		t.Fatalf("work.proto missing message %s", name)
	}
	end := strings.Index(content[start:], "\n}")
	if end < 0 {
		t.Fatalf("work.proto message %s is not closed", name)
	}

	var got []string
	for _, line := range strings.Split(content[start:start+end], "\n") {
		line = strings.TrimSpace(line)
		if strings.HasSuffix(line, ";") {
			got = append(got, line)
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("message %s fields = %q, want %q", name, got, want)
	}
}
