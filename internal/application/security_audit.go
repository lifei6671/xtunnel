package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/lifei6671/xtunnel/internal/repository"
)

var ErrSecurityAuditWriterInput = errors.New("security audit writer input is invalid")

// SecurityAuditWriter 持久化权威安全证据，并在提交成功后派生结构化 Security Log。
// SQLite 写入失败时绝不只记录日志后继续。
type SecurityAuditWriter struct {
	store  repository.Store
	logger *slog.Logger
}

// NewSecurityAuditWriter 创建统一的 Application Audit Writer。
func NewSecurityAuditWriter(store repository.Store, logger *slog.Logger) *SecurityAuditWriter {
	return &SecurityAuditWriter{store: store, logger: logger}
}

// Append 幂等追加一条已经冻结字段的安全事件。
func (writer *SecurityAuditWriter) Append(ctx context.Context, event repository.SecurityAuditEvent) error {
	if writer == nil || writer.store == nil || writer.logger == nil || ctx == nil {
		return ErrSecurityAuditWriterInput
	}
	if err := event.Validate(); err != nil {
		return err
	}
	if err := writer.store.WithDurableTx(ctx, func(transaction repository.TxStore) error {
		if err := transaction.SecurityAuditEvents().Append(ctx, event); err != nil {
			return fmt.Errorf("append security audit evidence: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	attributes := []any{
		slog.String("event_id", event.EventID),
		slog.String("operation_id", event.OperationID),
		slog.String("security_event", event.Event),
		slog.String("action", event.Action),
		slog.String("actor_type", event.ActorType),
		slog.String("resource_type", event.ResourceType),
		slog.String("result", event.Result),
	}
	if event.ActorID != "" {
		attributes = append(attributes, slog.String("actor_id", event.ActorID))
	}
	if event.SourceIP != "" {
		attributes = append(attributes, slog.String("source_ip", event.SourceIP))
	}
	if event.ResourceID != "" {
		attributes = append(attributes, slog.String("resource_id", event.ResourceID))
	}
	if event.ErrorCode != "" {
		attributes = append(attributes, slog.String("error_code", event.ErrorCode))
	}
	if event.RequestID != "" {
		attributes = append(attributes, slog.String("request_id", event.RequestID))
	}
	if event.TraceID != "" {
		attributes = append(attributes, slog.String("trace_id", event.TraceID))
	}
	writer.logger.InfoContext(ctx, "security_audit_event", attributes...)
	return nil
}
