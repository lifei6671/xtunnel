package repository

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/lifei6671/xtunnel/internal/protocol/validate"
)

const (
	SecurityAuditEventOperationResult    = "SECURITY_OPERATION_RESULT"
	SecurityAuditActionGatewayKeyRotate  = "GATEWAY_KEY_ROTATE"
	SecurityAuditActionTokenReveal       = "CONNECTION_TOKEN_REVEAL"
	SecurityAuditActionTokenRotate       = "CONNECTION_TOKEN_ROTATE"
	SecurityAuditActionTokenRevoke       = "CONNECTION_TOKEN_REVOKE"
	SecurityAuditActionTunnelRevoke      = "TUNNEL_REVOKE"
	SecurityAuditActorLocalOperator      = "LOCAL_OPERATOR"
	SecurityAuditActorAdmin              = "ADMIN"
	SecurityAuditResourceGatewayIdentity = "GATEWAY_IDENTITY"
	SecurityAuditResourceTunnelToken     = "TUNNEL_TOKEN"
	SecurityAuditResourceTunnel          = "TUNNEL"
	SecurityAuditResultSucceeded         = "SUCCEEDED"
	SecurityAuditResultFailed            = "FAILED"

	maxAuditResourceIDBytes  = 256
	maxAuditErrorCodeBytes   = 64
	maxAuditCorrelationBytes = 128
	auditDigestBytes         = 32
	// MaxSecurityAuditEventQueryLimit 是 Repository 单次只读查询的硬上限。
	// Handler 仍负责应用 API 默认值并在进入 Application 前拒绝超限输入。
	MaxSecurityAuditEventQueryLimit = 200
)

var (
	// ErrInvalidSecurityAuditEvent 表示事件字段不符合冻结的有界契约。
	ErrInvalidSecurityAuditEvent = errors.New("security audit event is invalid")
	// ErrSecurityAuditConflict 表示相同事件或操作 ID 已绑定到不同内容。
	ErrSecurityAuditConflict = errors.New("security audit event conflicts with existing evidence")
	// ErrInvalidSecurityAuditEventQuery 表示查询筛选、时间边界或 keyset 不合法。
	ErrInvalidSecurityAuditEventQuery = errors.New("security audit event query is invalid")
)

// SecurityAuditEvent 是 M1/M2 写入的最小持久化安全证据。
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
		event.OccurredAt <= 0 ||
		!validRequiredAuditText(event.ResourceID, maxAuditResourceIDBytes) ||
		!validOptionalAuditText(event.SourceIP, maxAuditCorrelationBytes) ||
		!validOptionalAuditText(event.RequestID, maxAuditCorrelationBytes) ||
		!validOptionalAuditText(event.TraceID, maxAuditCorrelationBytes) ||
		!validOptionalAuditDigest(event.BeforeStateDigest) ||
		!validOptionalAuditDigest(event.AfterStateDigest) {
		return ErrInvalidSecurityAuditEvent
	}
	if !validAuditSubject(event) {
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

func validAuditSubject(event SecurityAuditEvent) bool {
	switch event.Action {
	case SecurityAuditActionGatewayKeyRotate:
		return event.ActorType == SecurityAuditActorLocalOperator && event.ActorID == "" && event.SourceIP == "" &&
			event.ResourceType == SecurityAuditResourceGatewayIdentity
	case SecurityAuditActionTokenReveal, SecurityAuditActionTokenRotate, SecurityAuditActionTokenRevoke:
		return event.ActorType == SecurityAuditActorAdmin && validate.ValidID(event.ActorID, "adm_") &&
			event.ResourceType == SecurityAuditResourceTunnelToken && validate.ValidID(event.ResourceID, "tun_")
	case SecurityAuditActionTunnelRevoke:
		return event.ActorType == SecurityAuditActorAdmin && validate.ValidID(event.ActorID, "adm_") &&
			event.ResourceType == SecurityAuditResourceTunnel && validate.ValidID(event.ResourceID, "tun_")
	default:
		return false
	}
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

// SecurityAuditEventCursor 是已经由上层 opaque cursor codec 验证过的稳定分页键。
// Repository 只按该键执行 keyset 查询，不负责编码 cursor 或重新绑定筛选条件。
type SecurityAuditEventCursor struct {
	OccurredAt int64
	EventID    string
}

// SecurityAuditEventQuery 描述安全审计事件的只读筛选。
// OccurredFrom 包含边界，OccurredTo 不包含边界；After 按
// (occurred_at, event_id) DESC 继续读取下一页。
type SecurityAuditEventQuery struct {
	Action       string
	Result       string
	ResourceType string
	ResourceID   string
	OccurredFrom *int64
	OccurredTo   *int64
	After        *SecurityAuditEventCursor
	Limit        int
}

// Validate 检查 Repository 查询边界；Limit 不在此处应用默认值。
func (query SecurityAuditEventQuery) Validate() error {
	if query.Limit < 1 || query.Limit > MaxSecurityAuditEventQueryLimit ||
		!validOptionalAuditAction(query.Action) ||
		!validOptionalAuditResult(query.Result) ||
		!validOptionalAuditResourceType(query.ResourceType) ||
		!validOptionalAuditQueryResourceID(query.ResourceID) {
		return ErrInvalidSecurityAuditEventQuery
	}
	if query.OccurredFrom != nil && query.OccurredTo != nil && *query.OccurredFrom >= *query.OccurredTo {
		return ErrInvalidSecurityAuditEventQuery
	}
	if query.After != nil &&
		(query.After.OccurredAt <= 0 || !validate.ValidID(query.After.EventID, "evt_")) {
		return ErrInvalidSecurityAuditEventQuery
	}
	return nil
}

func validOptionalAuditQueryResourceID(value string) bool {
	return value == "" || utf8.ValidString(value) && utf8.RuneCountInString(value) <= maxAuditResourceIDBytes
}

func validOptionalAuditAction(value string) bool {
	switch value {
	case "", SecurityAuditActionGatewayKeyRotate, SecurityAuditActionTokenReveal,
		SecurityAuditActionTokenRotate, SecurityAuditActionTokenRevoke, SecurityAuditActionTunnelRevoke:
		return true
	default:
		return false
	}
}

func validOptionalAuditResult(value string) bool {
	return value == "" || value == SecurityAuditResultSucceeded || value == SecurityAuditResultFailed
}

func validOptionalAuditResourceType(value string) bool {
	switch value {
	case "", SecurityAuditResourceGatewayIdentity, SecurityAuditResourceTunnelToken, SecurityAuditResourceTunnel:
		return true
	default:
		return false
	}
}

// SecurityAuditEventPage 返回一页权威审计事件。仅当还有下一页时 Next 非空。
type SecurityAuditEventPage struct {
	Events []SecurityAuditEvent
	Next   *SecurityAuditEventCursor
}

// SecurityAuditEventQueryStore 只暴露读取能力，不允许修改或删除审计证据。
type SecurityAuditEventQueryStore interface {
	QuerySecurityAuditEvents(context.Context, SecurityAuditEventQuery) (SecurityAuditEventPage, error)
}

// SecurityAuditEventRepository 只允许幂等追加安全事件，不暴露修改或删除入口。
type SecurityAuditEventRepository interface {
	Append(context.Context, SecurityAuditEvent) error
}
