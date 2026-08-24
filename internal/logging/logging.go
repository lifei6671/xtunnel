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
	// RequestIDKey 是一次管理面请求的关联字段。
	RequestIDKey = "request_id"
	// TraceIDKey 是来自 OpenTelemetry Trace Context 的关联字段。
	TraceIDKey = "trace_id"

	redactedValue = "[REDACTED]"
)

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
	if options.Format != "json" {
		return nil, fmt.Errorf("unsupported logging format %q", options.Format)
	}

	level, err := parseLevel(options.Level)
	if err != nil {
		return nil, err
	}

	handler := slog.NewJSONHandler(writer, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: replaceAttr,
	})
	return slog.New(handler).With(slog.String("component", options.Component)), nil
}

// WithCorrelation 返回携带真实请求和链路标识的 Logger。
// 空标识不会写入日志，也不会在日志层生成无法关联的替代 ID。
func WithCorrelation(logger *slog.Logger, requestID, traceID string) *slog.Logger {
	attributes := make([]any, 0, 2)
	if requestID != "" {
		attributes = append(attributes, slog.String(RequestIDKey, requestID))
	}
	if traceID != "" {
		attributes = append(attributes, slog.String(TraceIDKey, traceID))
	}
	return logger.With(attributes...)
}

func parseLevel(value string) (slog.Level, error) {
	switch value {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unsupported logging level %q", value)
	}
}

func replaceAttr(_ []string, attribute slog.Attr) slog.Attr {
	switch attribute.Key {
	case slog.TimeKey:
		attribute.Key = "timestamp"
		attribute.Value = slog.StringValue(attribute.Value.Time().UTC().Format(time.RFC3339Nano))
	case slog.LevelKey:
		attribute.Value = slog.StringValue(strings.ToLower(attribute.Value.String()))
	case slog.MessageKey:
		attribute.Key = "event"
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
		"admin_password",
		"session_cookie",
		"tls_private_key",
		"authorization",
		"authorization_header",
		"config_signing_private_key",
		"session_secret":
		return true
	default:
		return false
	}
}
