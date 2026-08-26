package repository

import (
	"context"
	"errors"
	"strings"

	"github.com/lifei6671/xtunnel/internal/protocol/validate"
)

const (
	SecurityAuditEventOperationResult    = "SECURITY_OPERATION_RESULT"
	SecurityAuditActionGatewayKeyRotate  = "GATEWAY_KEY_ROTATE"
	SecurityAuditActorLocalOperator      = "LOCAL_OPERATOR"
	SecurityAuditResourceGatewayIdentity = "GATEWAY_IDENTITY"
	SecurityAuditResultSucceeded         = "SUCCEEDED"
	SecurityAuditResultFailed            = "FAILED"

	maxAuditResourceIDBytes  = 256
	maxAuditErrorCodeBytes   = 64
	maxAuditCorrelationBytes = 128
	auditDigestBytes         = 32
)

var (
	// ErrInvalidSecurityAuditEvent 表示事件字段不符合冻结的有界契约。
	ErrInvalidSecurityAuditEvent = errors.New("security audit event is invalid")
	// ErrSecurityAuditConflict 表示相同事件或操作 ID 已绑定到不同内容。
	ErrSecurityAuditConflict = errors.New("security audit event conflicts with existing evidence")
)

// SecurityAuditEvent 是 M1 写入的最小持久化安全证据。
// 可选字符串使用空值表示 SQL NULL；Digest 只能为空或精确 32 字节。
// V0.1 不接受通用 Metadata，新增字段必须先冻结允许列表和边界。
type SecurityAuditEvent struct {
	EventID           string
	OperationID       string
	Event             string
	Action            string
	ActorType         string
	ActorID           string
	SourceIP          string
	ResourceType      string
	ResourceID        string
	Result            string
	ErrorCode         string
	RequestID         string
	TraceID           string
	BeforeStateDigest []byte
	AfterStateDigest  []byte
	OccurredAt        int64
}

// Validate 检查安全审计事件的枚举、标识、Nullable 和长度不变量。
func (event SecurityAuditEvent) Validate() error {
	if !validate.ValidID(event.EventID, "evt_") || !validate.ValidID(event.OperationID, "op_") ||
		event.Event != SecurityAuditEventOperationResult ||
		event.Action != SecurityAuditActionGatewayKeyRotate ||
		event.ActorType != SecurityAuditActorLocalOperator ||
		event.ResourceType != SecurityAuditResourceGatewayIdentity ||
		event.OccurredAt <= 0 ||
		event.ActorID != "" || event.SourceIP != "" ||
		!validRequiredAuditText(event.ResourceID, maxAuditResourceIDBytes) ||
		!validOptionalAuditText(event.RequestID, maxAuditCorrelationBytes) ||
		!validOptionalAuditText(event.TraceID, maxAuditCorrelationBytes) ||
		!validOptionalAuditDigest(event.BeforeStateDigest) ||
		!validOptionalAuditDigest(event.AfterStateDigest) {
		return ErrInvalidSecurityAuditEvent
	}
	switch event.Result {
	case SecurityAuditResultSucceeded:
		if event.ErrorCode != "" {
			return ErrInvalidSecurityAuditEvent
		}
	case SecurityAuditResultFailed:
		if !validRequiredAuditText(event.ErrorCode, maxAuditErrorCodeBytes) {
			return ErrInvalidSecurityAuditEvent
		}
	default:
		return ErrInvalidSecurityAuditEvent
	}
	return nil
}

func validOptionalAuditText(value string, maximum int) bool {
	return value == "" || validRequiredAuditText(value, maximum)
}

func validRequiredAuditText(value string, maximum int) bool {
	return len(value) >= 1 && len(value) <= maximum && strings.TrimSpace(value) == value
}

func validOptionalAuditDigest(value []byte) bool {
	return len(value) == 0 || len(value) == auditDigestBytes
}

// SecurityAuditEventRepository 只允许幂等追加安全事件，不暴露修改或删除入口。
type SecurityAuditEventRepository interface {
	Append(context.Context, SecurityAuditEvent) error
}
