package httpingress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lifei6671/xtunnel/internal/logging"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
	"github.com/lifei6671/xtunnel/internal/server/route"
)

func TestHandlerLogsActualConnectionAcrossKeepAliveRequests(t *testing.T) {
	manager, _ := startRouteManager(t, baseHTTPRouteState(1))
	origin := newLoopOriginDialer(t)
	var output bytes.Buffer
	handler := newLoggingTestHandler(t, manager, origin, &output)
	spoofedRequestID := "req_01J00000000000000000000000"

	for _, path := range []string{"/first", "/second"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Host = "public.example.com"
		request.Header.Set("X-Request-ID", spoofedRequestID)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s response status = %d, want 200", path, response.Code)
		}
	}

	records := decodeRequestLogRecords(t, output.String())
	if len(records) != 2 {
		t.Fatalf("request log records = %d, want 2; output=%q", len(records), output.String())
	}
	calls := origin.Calls()
	if len(calls) != 1 {
		t.Fatalf("Tunnel Dial calls = %d, want one KeepAlive connection", len(calls))
	}
	for index, record := range records {
		if record["event"] != logging.EventHTTPIngressRequestCompleted || record["level"] != "info" ||
			record["method"] != http.MethodGet || record["status_code"] != float64(http.StatusOK) {
			t.Fatalf("record %d stable fields = %#v", index, record)
		}
		requestID, _ := record["request_id"].(string)
		if validate.ValidateID(requestID, "req_") != nil || requestID == spoofedRequestID {
			t.Fatalf("record %d request_id = %q, want fresh server ID", index, requestID)
		}
		if record["connection_id"] != calls[0].ConnectionID {
			t.Fatalf("record %d connection_id = %#v, want %q", index, record["connection_id"], calls[0].ConnectionID)
		}
		if _, exists := record["error_code"]; exists {
			t.Fatalf("record %d contains success error_code: %#v", index, record)
		}
	}
	if records[0]["request_id"] == records[1]["request_id"] {
		t.Fatalf("KeepAlive requests reused request_id %q", records[0]["request_id"])
	}
	if calls[0].RequestID != records[0]["request_id"] {
		t.Fatalf("Tunnel Dial request_id = %q, want first request %q", calls[0].RequestID, records[0]["request_id"])
	}
}

func TestHandlerLogsEarlyPublicFailureWithoutFakeConnection(t *testing.T) {
	manager, _ := startRouteManager(t, baseHTTPRouteState(1))
	origin := newLoopOriginDialer(t)
	var output bytes.Buffer
	handler := newLoggingTestHandler(t, manager, origin, &output)

	request := httptest.NewRequest(http.MethodGet, "/missing", nil)
	request.Host = "unknown.example.com"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("response status = %d, want 404", response.Code)
	}
	record := onlyRequestLogRecord(t, output.String())
	if record["level"] != "warn" || record["status_code"] != float64(http.StatusNotFound) ||
		record["error_code"] != "ROUTE_NOT_FOUND" {
		t.Fatalf("early failure record = %#v", record)
	}
	if _, exists := record["connection_id"]; exists {
		t.Fatalf("early failure fabricated connection_id: %#v", record)
	}
	if len(origin.Calls()) != 0 {
		t.Fatalf("early failure Tunnel Dial calls = %d, want 0", len(origin.Calls()))
	}
}

func TestHandlerRequestIDFailureLogsSanitizedInternalError(t *testing.T) {
	manager, _ := startRouteManager(t, baseHTTPRouteState(1))
	origin := newLoopOriginDialer(t)
	var output bytes.Buffer
	handler := newLoggingTestHandler(t, manager, origin, &output)
	secret := "request-id-random-source-secret"
	handler.newRequestID = func() (string, error) { return "", errors.New(secret) }

	request := httptest.NewRequest(http.MethodPost, "/resource", nil)
	request.Host = "public.example.com"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("response status = %d, want 500", response.Code)
	}
	record := onlyRequestLogRecord(t, output.String())
	if record["level"] != "error" || record["method"] != http.MethodPost ||
		record["status_code"] != float64(http.StatusInternalServerError) || record["error_code"] != "INTERNAL_ERROR" {
		t.Fatalf("request ID failure record = %#v", record)
	}
	if _, exists := record["request_id"]; exists {
		t.Fatalf("request ID failure fabricated request_id: %#v", record)
	}
	if _, exists := record["connection_id"]; exists {
		t.Fatalf("request ID failure fabricated connection_id: %#v", record)
	}
	if strings.Contains(output.String(), secret) {
		t.Fatalf("request ID failure leaked underlying error: %q", output.String())
	}
}

func TestNewHandlerRequiresLogger(t *testing.T) {
	manager, _ := startRouteManager(t, baseHTTPRouteState(1))
	origin := newLoopOriginDialer(t)
	limits := newTestLimitManager(t, serverlimits.Options{
		MaxConnectors: 1, MaxConnectorsPerTunnel: 1,
		MaxWorkConnections: 1, MaxIdleWorkConnections: 1,
		MaxConnectingWorkConnections: 1, MaxPendingOpens: 1,
		MaxActiveConnections: 1, MaxConnectionsPerTunnel: 1,
		MaxConnectionsPerService: 1, MaxConnectionsPerSourceIP: 1,
		MaxOpenRatePerSourceIP: 1, MaxOpenBurstPerSourceIP: 1,
		MaxHTTPRequestsPerSourceIPPerSecond: 1,
	})
	if _, err := NewHandler(HandlerOptions{
		Routes: manager, Dialer: origin, Limits: limits, MaxBodyBytes: 1,
	}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("NewHandler(nil Logger) error = %v, want ErrInvalidOptions", err)
	}
}

func TestRequestCancellationDoesNotEscalateRecoverableFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if level := requestLogLevel(ctx, requestLogSnapshot{
		status: http.StatusServiceUnavailable, errorCode: "SERVICE_UNAVAILABLE",
	}); level != slog.LevelInfo {
		t.Fatalf("canceled request level = %s, want info", level)
	}
}

func TestRequestCancellationDoesNotHideProtocolOrInternalFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, code := range []string{"PROTOCOL_ERROR", "INTERNAL_ERROR"} {
		if level := requestLogLevel(ctx, requestLogSnapshot{
			status: http.StatusServiceUnavailable, errorCode: code,
		}); level != slog.LevelError {
			t.Fatalf("canceled request with %s level = %s, want error", code, level)
		}
	}
}

func TestWebSocketActivityConnectionForwardsConnectionID(t *testing.T) {
	server, peer := net.Pipe()
	t.Cleanup(func() {
		_ = server.Close()
		_ = peer.Close()
	})
	connectionID := "conn_01J00000000000000000000000"
	wrapped := &webSocketActivityConn{
		Conn: &identifiedTestConnection{Conn: server, id: connectionID},
	}
	if actual := wrapped.ConnectionID(); actual != connectionID {
		t.Fatalf("WebSocket ConnectionID() = %q, want %q", actual, connectionID)
	}
}

func newLoggingTestHandler(
	t *testing.T,
	manager *route.Manager,
	dialer TunnelDialer,
	output io.Writer,
) *Handler {
	t.Helper()
	logger, err := logging.New(output, logging.Options{Level: "debug", Format: "json", Component: "server"})
	if err != nil {
		t.Fatalf("logging.New() error = %v", err)
	}
	limits := newTestLimitManager(t, serverlimits.Options{
		MaxConnectors: 1_024, MaxConnectorsPerTunnel: 1_024,
		MaxWorkConnections: 1_024, MaxIdleWorkConnections: 1_024,
		MaxConnectingWorkConnections: 1_024, MaxPendingOpens: 1_024,
		MaxActiveConnections: 1_024, MaxConnectionsPerTunnel: 1_024,
		MaxConnectionsPerService: 1_024, MaxConnectionsPerSourceIP: 1_024,
		MaxOpenRatePerSourceIP: 100_000, MaxOpenBurstPerSourceIP: 100_000,
		MaxHTTPRequestsPerSourceIPPerSecond: 100_000,
	})
	return newTestHandlerWithLimitsAndLogger(
		t, manager, dialer, []string{"127.0.0.1/32", "::1/128"}, limits, 2<<30, logger,
	)
}

func decodeRequestLogRecords(t *testing.T, output string) []map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	records := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode request log %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func onlyRequestLogRecord(t *testing.T, output string) map[string]any {
	t.Helper()
	records := decodeRequestLogRecords(t, output)
	if len(records) != 1 {
		t.Fatalf("request log records = %d, want 1; output=%q", len(records), output)
	}
	return records[0]
}
