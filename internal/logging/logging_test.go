package logging

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestNewWritesStableJSONFields(t *testing.T) {
	var output bytes.Buffer
	logger, err := New(&output, Options{Level: LevelInfo, Format: "json", Component: "server"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	WithCorrelation(logger, "req_123", "trace_456").Info(EventProcessStarted,
		slog.String(ErrorCodeKey, "TEST_ERROR"),
	)

	record := decodeRecord(t, output.String())
	if record[LevelKey] != LevelInfo || record[ComponentKey] != "server" || record[EventKey] != EventProcessStarted {
		t.Fatalf("stable fields = %#v", record)
	}
	if record[RequestIDKey] != "req_123" || record[TraceIDKey] != "trace_456" || record[ErrorCodeKey] != "TEST_ERROR" {
		t.Fatalf("correlation fields = %#v", record)
	}
	if _, exists := record[slog.TimeKey]; exists {
		t.Fatalf("record contains legacy %q field: %#v", slog.TimeKey, record)
	}
	if _, exists := record[slog.MessageKey]; exists {
		t.Fatalf("record contains legacy %q field: %#v", slog.MessageKey, record)
	}

	timestamp, ok := record[TimestampKey].(string)
	if !ok {
		t.Fatalf("timestamp = %#v, want string", record[TimestampKey])
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
		{level: LevelDebug, wantEvents: []string{LevelDebug, LevelInfo, LevelWarn, LevelError}},
		{level: LevelInfo, wantEvents: []string{LevelInfo, LevelWarn, LevelError}},
		{level: LevelWarn, wantEvents: []string{LevelWarn, LevelError}},
		{level: LevelError, wantEvents: []string{LevelError}},
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
				if record[EventKey] != test.wantEvents[index] || record[LevelKey] != test.wantEvents[index] {
					t.Fatalf("record %d = %#v", index, record)
				}
			}
		})
	}
}

func TestNewRedactsSecretAttributes(t *testing.T) {
	var output bytes.Buffer
	logger, err := New(&output, Options{Level: LevelInfo, Format: "json", Component: "server"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	secret := "unique-secret-sentinel"
	logger.Info("credentials_rejected",
		slog.String("agent_token", secret),
		slog.String("connection_token", secret),
		slog.String("tunnel_token", secret),
		slog.String("token", secret),
		slog.String("admin_password", secret),
		slog.String("password", secret),
		slog.String("session_cookie", secret),
		slog.String("cookie", secret),
		slog.String("set_cookie", secret),
		slog.String("tls_private_key", secret),
		slog.String("private_key", secret),
		slog.String("Authorization", secret),
		slog.String("authorization_header", secret),
		slog.String("config_signing_private_key", secret),
		slog.Group("session",
			slog.String("session_secret", secret),
			slog.String("CSRF_TOKEN", secret),
		),
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
	logger, err := New(&output, Options{Level: LevelInfo, Format: "json", Component: "agent"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	WithCorrelationFields(logger, Correlation{}).Info(EventProcessStarted)
	record := decodeRecord(t, output.String())
	if _, exists := record[RequestIDKey]; exists {
		t.Fatalf("record contains empty %q: %#v", RequestIDKey, record)
	}
	if _, exists := record[TraceIDKey]; exists {
		t.Fatalf("record contains empty %q: %#v", TraceIDKey, record)
	}
	for _, key := range []string{TunnelIDKey, ConnectorIDKey, SessionIDKey, ServiceIDKey, ConnectionIDKey, GenerationKey} {
		if _, exists := record[key]; exists {
			t.Fatalf("record contains empty %q: %#v", key, record)
		}
	}
}

func TestWithCorrelationFieldsWritesRealBusinessIDs(t *testing.T) {
	var output bytes.Buffer
	logger, err := New(&output, Options{Level: LevelInfo, Format: "json", Component: "server"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	correlation := Correlation{
		RequestID: "req_123", TraceID: "0123456789abcdef", TunnelID: "tun_123",
		ConnectorID: "con_123", SessionID: "ses_123", ServiceID: "svc_123",
		ConnectionID: "conn_123", Generation: 7,
	}
	WithCorrelationFields(logger, correlation).Info(EventConnectorConnected)
	record := decodeRecord(t, output.String())

	want := map[string]any{
		RequestIDKey: correlation.RequestID, TraceIDKey: correlation.TraceID,
		TunnelIDKey: correlation.TunnelID, ConnectorIDKey: correlation.ConnectorID,
		SessionIDKey: correlation.SessionID, ServiceIDKey: correlation.ServiceID,
		ConnectionIDKey: correlation.ConnectionID, GenerationKey: float64(correlation.Generation),
	}
	for key, value := range want {
		if record[key] != value {
			t.Fatalf("record[%q] = %#v, want %#v; record = %#v", key, record[key], value, record)
		}
	}
}

func TestNewRejectsUnsupportedOptions(t *testing.T) {
	tests := []struct {
		name    string
		writer  io.Writer
		options Options
	}{
		{name: "missing writer", options: Options{Level: LevelInfo, Format: "json", Component: "server"}},
		{name: "unknown level", writer: &bytes.Buffer{}, options: Options{Level: "notice", Format: "json", Component: "server"}},
		{name: "non JSON format", writer: &bytes.Buffer{}, options: Options{Level: LevelInfo, Format: "text", Component: "server"}},
		{name: "empty component", writer: &bytes.Buffer{}, options: Options{Level: LevelInfo, Format: "json"}},
		{name: "blank component", writer: &bytes.Buffer{}, options: Options{Level: LevelInfo, Format: "json", Component: " \t"}},
		{name: "padded component", writer: &bytes.Buffer{}, options: Options{Level: LevelInfo, Format: "json", Component: " server "}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := New(test.writer, test.options); err == nil {
				t.Fatalf("New(%#v) error = nil", test.options)
			}
		})
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
