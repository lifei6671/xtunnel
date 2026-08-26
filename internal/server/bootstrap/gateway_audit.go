package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/lifei6671/xtunnel/internal/application"
	"github.com/lifei6671/xtunnel/internal/repository"
	"github.com/lifei6671/xtunnel/internal/server/gateway"
)

// reconcileGatewayRotationAudit 在 Gateway 开始监听前完成身份文件恢复，并将待处理事件
// 幂等写入 SQLite。只有权威事件提交成功后才删除持久化 Journal。
func reconcileGatewayRotationAudit(
	ctx context.Context,
	dataDir string,
	store repository.Store,
	logger *slog.Logger,
) (bool, error) {
	return reconcileGatewayRotationAuditWith(ctx, dataDir, store, logger, gateway.CompleteRotationAudit)
}

func reconcileGatewayRotationAuditWith(
	ctx context.Context,
	dataDir string,
	store repository.Store,
	logger *slog.Logger,
	completeAudit func(string, string, string) error,
) (bool, error) {
	if ctx == nil || store == nil || logger == nil || completeAudit == nil {
		return false, errors.New("gateway rotation audit reconciliation input is invalid")
	}
	if err := gateway.RecoverRotation(dataDir); err != nil {
		return false, fmt.Errorf("recover gateway identity files: %w", err)
	}
	pending, exists, err := gateway.PendingRotationAuditEvent(dataDir)
	if err != nil || !exists {
		return false, err
	}
	current, err := gateway.LoadPinnedIdentity(dataDir)
	if err != nil {
		return false, fmt.Errorf("load gateway identity for audit reconciliation: %w", err)
	}
	if current.SPKIHash() != pending.AfterStateDigest {
		return false, errors.New("gateway identity does not match pending rotation audit after-state")
	}
	event := repository.SecurityAuditEvent{
		EventID: pending.EventID, OperationID: pending.OperationID,
		Event:             repository.SecurityAuditEventOperationResult,
		Action:            repository.SecurityAuditActionGatewayKeyRotate,
		ActorType:         repository.SecurityAuditActorLocalOperator,
		ResourceType:      repository.SecurityAuditResourceGatewayIdentity,
		ResourceID:        pending.ResourceID,
		Result:            repository.SecurityAuditResultSucceeded,
		BeforeStateDigest: append([]byte(nil), pending.BeforeStateDigest[:]...),
		AfterStateDigest:  append([]byte(nil), pending.AfterStateDigest[:]...),
		OccurredAt:        pending.OccurredAt,
	}
	if err := application.NewSecurityAuditWriter(store, logger).Append(ctx, event); err != nil {
		return false, err
	}
	if err := completeAudit(dataDir, pending.EventID, pending.OperationID); err != nil {
		if errors.Is(err, gateway.ErrRotationAuditCleanupUncertain) {
			logger.WarnContext(ctx, "gateway_rotation_audit_cleanup_uncertain",
				"event_id", pending.EventID,
				"operation_id", pending.OperationID,
				"error_code", "AUDIT_JOURNAL_DIRECTORY_SYNC_FAILED",
			)
			return true, nil
		}
		return false, err
	}
	return true, nil
}
