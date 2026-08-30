package httpingress

import (
	"context"
	"log/slog"
	"net/http"
	"sync"

	"github.com/lifei6671/xtunnel/internal/logging"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
	internaltracing "github.com/lifei6671/xtunnel/internal/tracing"
)

type requestLogContextKey struct{}

// requestLogObservation 只保存当前公网 HTTP 请求已经真实观察到的关联信息。
// Transport 的 GotConn 可能来自连接池内部 goroutine，因此读写必须受锁保护。
type requestLogObservation struct {
	requestID string

	mu           sync.Mutex
	connectionID string
	errorCode    string
}

type requestLogSnapshot struct {
	connectionID string
	errorCode    string
	status       int
}

func newRequestLogObservation(requestID string) *requestLogObservation {
	return &requestLogObservation{requestID: requestID}
}

func requestLogObservationFrom(ctx context.Context) *requestLogObservation {
	if ctx == nil {
		return nil
	}
	observation, _ := ctx.Value(requestLogContextKey{}).(*requestLogObservation)
	return observation
}

func (observation *requestLogObservation) observeConnection(connectionID string) {
	if observation == nil || validate.ValidateID(connectionID, "conn_") != nil {
		return
	}
	observation.mu.Lock()
	observation.connectionID = connectionID
	observation.mu.Unlock()
}

func (observation *requestLogObservation) observeErrorCode(code string) {
	if observation == nil || code == "" {
		return
	}
	observation.mu.Lock()
	observation.errorCode = code
	observation.mu.Unlock()
}

func (observation *requestLogObservation) snapshot(status int) requestLogSnapshot {
	if observation == nil {
		return requestLogSnapshot{status: status}
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	return requestLogSnapshot{
		connectionID: observation.connectionID,
		errorCode:    observation.errorCode,
		status:       status,
	}
}

type requestLogResponseWriter struct {
	http.ResponseWriter
	observation *requestLogObservation

	mu     sync.Mutex
	status int
}

func newRequestLogResponseWriter(writer http.ResponseWriter, observation *requestLogObservation) *requestLogResponseWriter {
	return &requestLogResponseWriter{ResponseWriter: writer, observation: observation}
}

func (writer *requestLogResponseWriter) Unwrap() http.ResponseWriter {
	return writer.ResponseWriter
}

func (writer *requestLogResponseWriter) WriteHeader(status int) {
	writer.mu.Lock()
	if writer.status != 0 {
		writer.mu.Unlock()
		return
	}
	writer.status = status
	writer.mu.Unlock()
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *requestLogResponseWriter) Write(buffer []byte) (int, error) {
	writer.mu.Lock()
	if writer.status == 0 {
		writer.status = http.StatusOK
	}
	writer.mu.Unlock()
	return writer.ResponseWriter.Write(buffer)
}

func (writer *requestLogResponseWriter) statusCode() int {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.status == 0 {
		return http.StatusOK
	}
	return writer.status
}

func setRequestLogErrorCode(writer http.ResponseWriter, code string) {
	for writer != nil {
		if observed, ok := writer.(*requestLogResponseWriter); ok {
			observed.observation.observeErrorCode(code)
			return
		}
		unwrapper, ok := writer.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}
		writer = unwrapper.Unwrap()
	}
}

func (handler *Handler) logRequestCompleted(
	ctx context.Context,
	method string,
	requestID string,
	snapshot requestLogSnapshot,
) {
	if handler == nil || handler.logger == nil {
		return
	}
	attributes := []slog.Attr{
		slog.String("method", method),
		slog.Int("status_code", snapshot.status),
	}
	logger := logging.WithCorrelationFields(handler.logger, logging.Correlation{
		RequestID: requestID, TraceID: internaltracing.TraceID(ctx), ConnectionID: snapshot.connectionID,
	})
	if snapshot.errorCode != "" {
		attributes = append(attributes, slog.String(logging.ErrorCodeKey, snapshot.errorCode))
	}
	logger.LogAttrs(ctx, requestLogLevel(ctx, snapshot), logging.EventHTTPIngressRequestCompleted, attributes...)
}

func requestLogLevel(ctx context.Context, snapshot requestLogSnapshot) slog.Level {
	if snapshot.errorCode == "PROTOCOL_ERROR" || snapshot.errorCode == "INTERNAL_ERROR" ||
		(snapshot.errorCode == "" && snapshot.status >= http.StatusInternalServerError) {
		return slog.LevelError
	}
	if ctx != nil && ctx.Err() != nil {
		return slog.LevelInfo
	}
	if snapshot.errorCode != "" || snapshot.status >= http.StatusBadRequest {
		return slog.LevelWarn
	}
	return slog.LevelInfo
}
