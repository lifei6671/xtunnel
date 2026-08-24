package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestNewWritesStableJSONFields(t *testing.T) {
	var output bytes.Buffer
	logger, err := New(&output, Options{Level: "info", Format: "json", Component: "server"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	WithCorrelation(logger, "req_123", "trace_456").Info("process_started",
		slog.String("error_code", "TEST_ERROR"),
	)

	record := decodeRecord(t, output.String())
	if record["level"] != "info" || record["component"] != "server" || record["event"] != "process_started" {
		t.Fatalf("stable fields = %#v", record)
	}
	if record[RequestIDKey] != "req_123" || record[TraceIDKey] != "trace_456" || record["error_code"] != "TEST_ERROR" {
		t.Fatalf("correlation fields = %#v", record)
	}
	if _, exists := record[slog.TimeKey]; exists {
		t.Fatalf("record contains legacy %q field: %#v", slog.TimeKey, record)
	}
	if _, exists := record[slog.MessageKey]; exists {
		t.Fatalf("record contains legacy %q field: %#v", slog.MessageKey, record)
	}

	timestamp, ok := record["timestamp"].(string)
	if !ok {
		t.Fatalf("timestamp = %#v, want string", record["timestamp"])
	}
	parsed, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		t.Fatalf("timestamp %q is not RFC3339Nano: %v", timestamp, err)
	}
	if parsed.Location() != time.UTC {
		t.Fatalf("timestamp location = %v, want UTC", parsed.Location())
	}
}

func TestNewAppliesConfiguredLevel(t *testing.T) {
	tests := []struct {
		level      string
		wantEvents []string
	}{
		{level: "debug", wantEvents: []string{"debug", "info", "warn", "error"}},
		{level: "info", wantEvents: []string{"info", "warn", "error"}},
		{level: "warn", wantEvents: []string{"warn", "error"}},
		{level: "error", wantEvents: []string{"error"}},
	}

	for _, test := range tests {
		t.Run(test.level, func(t *testing.T) {
			var output bytes.Buffer
			logger, err := New(&output, Options{Level: test.level, Format: "json", Component: "test"})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			logger.Debug("debug")
			logger.Info("info")
			logger.Warn("warn")
			logger.Error("error")

			lines := strings.Split(strings.TrimSpace(output.String()), "\n")
			if len(lines) != len(test.wantEvents) {
				t.Fatalf("log lines = %d, want %d; output = %q", len(lines), len(test.wantEvents), output.String())
			}
			for index, line := range lines {
				record := decodeRecord(t, line)
				if record["event"] != test.wantEvents[index] || record["level"] != test.wantEvents[index] {
					t.Fatalf("record %d = %#v", index, record)
				}
			}
		})
	}
}

func TestNewRedactsSecretAttributes(t *testing.T) {
	var output bytes.Buffer
	logger, err := New(&output, Options{Level: "info", Format: "json", Component: "server"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	secret := "unique-secret-sentinel"
	logger.Info("credentials_rejected",
		slog.String("agent_token", secret),
		slog.String("admin_password", secret),
		slog.String("session_cookie", secret),
		slog.String("tls_private_key", secret),
		slog.String("Authorization", secret),
		slog.String("authorization_header", secret),
		slog.String("config_signing_private_key", secret),
		slog.Group("session", slog.String("session_secret", secret)),
		slog.String("token_file", "/run/secrets/agent-token"),
	)

	if strings.Contains(output.String(), secret) {
		t.Fatalf("log output contains raw secret: %s", output.String())
	}
	record := decodeRecord(t, output.String())
	if record["agent_token"] != redactedValue || record["Authorization"] != redactedValue {
		t.Fatalf("top-level secrets were not redacted: %#v", record)
	}
	session, ok := record["session"].(map[string]any)
	if !ok || session["session_secret"] != redactedValue {
		t.Fatalf("nested secret was not redacted: %#v", record["session"])
	}
	if record["token_file"] != "/run/secrets/agent-token" {
		t.Fatalf("non-secret path changed: %#v", record["token_file"])
	}
}

func TestWithCorrelationOmitsMissingIDs(t *testing.T) {
	var output bytes.Buffer
	logger, err := New(&output, Options{Level: "info", Format: "json", Component: "agent"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	WithCorrelation(logger, "", "").Info("process_started")
	record := decodeRecord(t, output.String())
	if _, exists := record[RequestIDKey]; exists {
		t.Fatalf("record contains empty %q: %#v", RequestIDKey, record)
	}
	if _, exists := record[TraceIDKey]; exists {
		t.Fatalf("record contains empty %q: %#v", TraceIDKey, record)
	}
}

func TestNewRejectsUnsupportedOptions(t *testing.T) {
	tests := []Options{
		{Level: "notice", Format: "json", Component: "server"},
		{Level: "info", Format: "text", Component: "server"},
	}
	for _, options := range tests {
		if _, err := New(&bytes.Buffer{}, options); err == nil {
			t.Fatalf("New(%#v) error = nil", options)
		}
	}
}

func decodeRecord(t *testing.T, line string) map[string]any {
	t.Helper()
	var record map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &record); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", line, err)
	}
	return record
}
