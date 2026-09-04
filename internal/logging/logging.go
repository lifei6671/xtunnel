// Package logging 提供 Server 和 Agent 共用的结构化日志基座。
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
)

const (
	// TimestampKey、LevelKey、ComponentKey 和 EventKey 是每条日志固定包含的字段。
	TimestampKey = "timestamp"
	LevelKey     = "level"
	ComponentKey = "component"
	EventKey     = "event"
	// ErrorCodeKey 是有限枚举的机器错误分类，不承载底层错误文本。
	ErrorCodeKey = "error_code"

	// RequestIDKey 是一次管理面请求的关联字段。
	RequestIDKey = "request_id"
	// TraceIDKey 是来自 OpenTelemetry Trace Context 的关联字段。
	TraceIDKey = "trace_id"
	// TunnelIDKey、ConnectorIDKey、SessionIDKey、ServiceIDKey、ConnectionIDKey
	// 和 GenerationKey 只记录业务流程中已经存在的真实标识。
	TunnelIDKey     = "tunnel_id"
	ConnectorIDKey  = "connector_id"
	SessionIDKey    = "session_id"
	ServiceIDKey    = "service_id"
	ConnectionIDKey = "connection_id"
	GenerationKey   = "generation"

	// LevelDebug 仅用于开发诊断；LevelInfo 记录正常生命周期；LevelWarn 记录
	// 可恢复异常；LevelError 记录导致当前操作或进程无法继续的异常。
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"

	// 以下事件名是已经由技术方案冻结、跨 Server/Agent 使用的稳定日志契约。
	EventProcessStarted              = "process_started"
	EventProcessStopped              = "process_stopped"
	EventConnectorConnected          = "connector_connected"
	EventConnectorSessionReplaced    = "connector_session_replaced"
	EventConnectorDraining           = "connector_draining"
	EventConnectorDisconnected       = "connector_disconnected"
	EventSecurityAudit               = "security_audit_event"
	EventTCPIngressConnectionFailed  = "tcp_ingress_connection_failed"
	EventManagementRequestCompleted  = "management_request_completed"
	EventHTTPIngressRequestCompleted = "http_ingress_request_completed"
	EventTunnelConnectionOpened      = "tunnel_connection_opened"
	EventTunnelConnectionFailed      = "tunnel_connection_failed"
	EventTunnelConnectionClosed      = "tunnel_connection_closed"
	EventAgentOriginConnectionFailed = "agent_origin_connection_failed"
	EventAgentConnectionFailed       = "agent_connection_failed"
	EventAgentConnectionOpened       = "agent_connection_opened"
	EventAgentConnectionClosed       = "agent_connection_closed"
	EventAgentServerConnected        = "agent_server_connected"
	EventAgentServerConnectionFailed = "agent_server_connection_failed"
	EventWindowsServiceStarting      = "windows_service_starting"
	EventWindowsServiceRunning       = "windows_service_running"
	EventWindowsServiceStopRequested = "windows_service_stop_requested"
	EventWindowsServiceStopped       = "windows_service_stopped"
	EventWindowsServiceFailed        = "windows_service_failed"

	redactedValue = "[REDACTED]"
)

// Correlation 是日志关联字段的值型快照。零值字段会被省略；调用方不得为
// 缺失上下文生成替代 ID，TraceID 只能来自真实 OpenTelemetry Trace Context。
type Correlation struct {
	RequestID    string
	TraceID      string
	TunnelID     string
	ConnectorID  string
	SessionID    string
	ServiceID    string
	ConnectionID string
	Generation   uint64
}

// Options 定义创建 JSON Logger 所需的稳定配置。
type Options struct {
	Level     string
	Format    string
	Component string
}

// New 创建一个带固定 component 的 JSON Logger。
//
// Handler 将 slog 内建字段规范化为技术方案冻结的 timestamp、level 和
// event。调用方不得把 Secret 拼入 event，也不得直接记录完整配置、请求或
// Header；明确的敏感属性名会在写出前统一脱敏。
func New(writer io.Writer, options Options) (*slog.Logger, error) {
	if writer == nil {
		return nil, fmt.Errorf("logging writer is required")
	}
	if options.Format != "json" {
		return nil, fmt.Errorf("unsupported logging format %q", options.Format)
	}
	if options.Component == "" || options.Component != strings.TrimSpace(options.Component) {
		return nil, fmt.Errorf("logging component must be non-empty and trimmed")
	}

	level, err := parseLevel(options.Level)
	if err != nil {
		return nil, err
	}

	handler := slog.NewJSONHandler(writer, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: replaceAttr,
	})
	return slog.New(handler).With(slog.String(ComponentKey, options.Component)), nil
}

// WithCorrelation 返回携带真实请求和链路标识的 Logger。
// 空标识不会写入日志，也不会在日志层生成无法关联的替代 ID。
func WithCorrelation(logger *slog.Logger, requestID, traceID string) *slog.Logger {
	return WithCorrelationFields(logger, Correlation{RequestID: requestID, TraceID: traceID})
}

// WithCorrelationFields 返回携带真实关联字段的 Logger。字符串空值和零
// generation 会被省略，避免日志层伪造不存在的业务上下文。
func WithCorrelationFields(logger *slog.Logger, correlation Correlation) *slog.Logger {
	attributes := make([]any, 0, 8)
	for _, field := range []struct {
		key   string
		value string
	}{
		{RequestIDKey, correlation.RequestID},
		{TraceIDKey, correlation.TraceID},
		{TunnelIDKey, correlation.TunnelID},
		{ConnectorIDKey, correlation.ConnectorID},
		{SessionIDKey, correlation.SessionID},
		{ServiceIDKey, correlation.ServiceID},
		{ConnectionIDKey, correlation.ConnectionID},
	} {
		if field.value != "" {
			attributes = append(attributes, slog.String(field.key, field.value))
		}
	}
	if correlation.Generation != 0 {
		attributes = append(attributes, slog.Uint64(GenerationKey, correlation.Generation))
	}
	return logger.With(attributes...)
}

func parseLevel(value string) (slog.Level, error) {
	switch value {
	case LevelDebug:
		return slog.LevelDebug, nil
	case LevelInfo:
		return slog.LevelInfo, nil
	case LevelWarn:
		return slog.LevelWarn, nil
	case LevelError:
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unsupported logging level %q", value)
	}
}

func replaceAttr(_ []string, attribute slog.Attr) slog.Attr {
	switch attribute.Key {
	case slog.TimeKey:
		attribute.Key = TimestampKey
		attribute.Value = slog.StringValue(attribute.Value.Time().UTC().Format(time.RFC3339Nano))
	case slog.LevelKey:
		attribute.Key = LevelKey
		attribute.Value = slog.StringValue(strings.ToLower(attribute.Value.String()))
	case slog.MessageKey:
		attribute.Key = EventKey
	default:
		if isSecretKey(attribute.Key) {
			attribute.Value = slog.StringValue(redactedValue)
		}
	}
	return attribute
}

func isSecretKey(key string) bool {
	switch strings.ToLower(key) {
	case "agent_token",
		"connection_token",
		"tunnel_token",
		"token",
		"admin_password",
		"password",
		"session_cookie",
		"cookie",
		"set_cookie",
		"tls_private_key",
		"private_key",
		"authorization",
		"authorization_header",
		"config_signing_private_key",
		"session_secret",
		"csrf_token":
		return true
	default:
		return false
	}
}
