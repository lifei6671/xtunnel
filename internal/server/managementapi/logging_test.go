package managementapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lifei6671/xtunnel/internal/logging"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
)

func TestManagementRequestCompletedLogsStatusLevelAndRequestID(t *testing.T) {
	var output bytes.Buffer
	logger, err := logging.New(&output, logging.Options{Level: logging.LevelInfo, Format: "json", Component: "server"})
	if err != nil {
		t.Fatalf("logging.New() error = %v", err)
	}
	handler, err := NewHandler(HandlerOptions{
		Management: serverconfig.Management{PublicURL: "https://admin.example"},
		Store:      &blockingAdminAuthStore{},
		Logger:     logger,
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	tests := []struct {
		name       string
		method     string
		target     string
		body       string
		headers    map[string]string
		wantStatus int
		wantLevel  string
		wantPath   string
	}{
		{
			name: "success", method: http.MethodGet,
			target: "https://admin.example/?token=query-secret", wantStatus: http.StatusOK,
			wantLevel: logging.LevelInfo, wantPath: "/",
		},
		{
			name: "client error", method: http.MethodPost,
			target: "https://admin.example/api/v1/auth/login?token=query-secret",
			body:   `{"username":"admin","password":"body-secret","unknown":true}`,
			headers: map[string]string{
				"Content-Type": "application/json", "Origin": "https://admin.example",
				"Authorization": "Bearer header-secret", "Cookie": "cookie-secret=value",
			},
			wantStatus: http.StatusBadRequest, wantLevel: logging.LevelWarn,
			wantPath: "/api/v1/auth/login",
		},
		{
			name: "server error", method: http.MethodGet,
			target:     "https://admin.example/api/v1/not-implemented?token=query-secret",
			wantStatus: http.StatusInternalServerError, wantLevel: logging.LevelError,
			wantPath: "/api/v1/not-implemented",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.target, strings.NewReader(test.body))
			for name, value := range test.headers {
				request.Header.Set(name, value)
			}
			recorder := httptest.NewRecorder()
			before := len(managementLogRecords(t, output.String()))
			handler.ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus {
				t.Fatalf("response status = %d, want %d; body = %s", recorder.Code, test.wantStatus, recorder.Body.String())
			}

			records := managementLogRecords(t, output.String())
			if len(records) != before+1 {
				t.Fatalf("new log records = %d, want 1; records = %#v", len(records)-before, records[before:])
			}
			record := records[before]
			if record[logging.EventKey] != logging.EventManagementRequestCompleted || record[logging.LevelKey] != test.wantLevel ||
				record["method"] != test.method || record["path"] != test.wantPath ||
				record["status_code"] != float64(test.wantStatus) {
				t.Fatalf("completion record = %#v", record)
			}
			requestID, ok := record[logging.RequestIDKey].(string)
			if !ok || requestID == "" {
				t.Fatalf("completion request_id = %#v", record[logging.RequestIDKey])
			}
			if duration, ok := record["duration_ms"].(float64); !ok || duration < 0 {
				t.Fatalf("completion duration_ms = %#v", record["duration_ms"])
			}
			if test.wantStatus >= http.StatusBadRequest {
				var response ErrorResponse
				if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
					t.Fatalf("json.Unmarshal(error response) error = %v", err)
				}
				if response.Error.RequestId != requestID {
					t.Fatalf("response request_id = %q, completion request_id = %q", response.Error.RequestId, requestID)
				}
			}
		})
	}

	for _, secret := range []string{"query-secret", "body-secret", "header-secret", "cookie-secret"} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("management logs contain secret %q: %s", secret, output.String())
		}
	}
}

func TestManagementResponseWriterPreservesDefaultAndExplicitStatus(t *testing.T) {
	t.Run("default 200", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		writer := &managementResponseWriter{ResponseWriter: recorder}
		if _, err := writer.Write([]byte("ok")); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		if writer.finalStatusCode() != http.StatusOK || recorder.Code != http.StatusOK || recorder.Body.String() != "ok" {
			t.Fatalf("writer status/body = (%d, %d, %q)", writer.finalStatusCode(), recorder.Code, recorder.Body.String())
		}
		if writer.Unwrap() != recorder {
			t.Fatal("Unwrap() did not return the original ResponseWriter")
		}
	})

	t.Run("explicit status", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		writer := &managementResponseWriter{ResponseWriter: recorder}
		writer.WriteHeader(http.StatusNoContent)
		if writer.finalStatusCode() != http.StatusNoContent || recorder.Code != http.StatusNoContent {
			t.Fatalf("writer status = (%d, %d), want 204", writer.finalStatusCode(), recorder.Code)
		}
	})
}

func TestManagementResponseEncodeFailureUsesRequestID(t *testing.T) {
	var output bytes.Buffer
	logger, err := logging.New(&output, logging.Options{Level: logging.LevelInfo, Format: "json", Component: "server"})
	if err != nil {
		t.Fatalf("logging.New() error = %v", err)
	}
	handler := &ManagementHandler{logger: logger}
	request := httptest.NewRequest(http.MethodGet, "https://admin.example/api/v1/test", nil)
	const requestID = "req_01K00000000000000000000000"
	requestContext := &managementRequestContext{requestID: requestID, request: request}
	request = request.WithContext(context.WithValue(request.Context(), requestContextKey{}, requestContext))
	requestContext.request = request

	handler.writeJSON(&failingManagementResponseWriter{header: make(http.Header)}, request, http.StatusInternalServerError, map[string]string{"status": "failed"})
	records := managementLogRecords(t, output.String())
	if len(records) != 1 || records[0][logging.EventKey] != "management_response_encode_failed" ||
		records[0][logging.RequestIDKey] != requestID || records[0][logging.LevelKey] != logging.LevelError ||
		records[0][logging.ErrorCodeKey] != "INTERNAL_ERROR" {
		t.Fatalf("encode failure records = %#v", records)
	}
	if _, exists := records[0]["error"]; exists {
		t.Fatalf("encode failure exposed raw error text: %#v", records[0])
	}
}

type failingManagementResponseWriter struct {
	header http.Header
}

func (writer *failingManagementResponseWriter) Header() http.Header { return writer.header }

func (*failingManagementResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("injected response write failure")
}

func (*failingManagementResponseWriter) WriteHeader(int) {}

func managementLogRecords(t *testing.T, output string) []map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	records := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("json.Unmarshal(log %q) error = %v", line, err)
		}
		records = append(records, record)
	}
	return records
}
