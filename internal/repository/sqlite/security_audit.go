package sqlite

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/lifei6671/xtunnel/internal/repository"
)

const SecurityAuditEventTable = "security_audit_events"

// SecurityAuditEventColumns 集中定义安全审计列名，调用方不得自行拼接更新或删除语句。
var SecurityAuditEventColumns = struct {
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
	BeforeStateDigest string
	AfterStateDigest  string
	OccurredAt        string
}{
	EventID:           "event_id",
	OperationID:       "operation_id",
	Event:             "event",
	Action:            "action",
	ActorType:         "actor_type",
	ActorID:           "actor_id",
	SourceIP:          "source_ip",
	ResourceType:      "resource_type",
	ResourceID:        "resource_id",
	Result:            "result",
	ErrorCode:         "error_code",
	RequestID:         "request_id",
	TraceID:           "trace_id",
	BeforeStateDigest: "before_state_digest",
	AfterStateDigest:  "after_state_digest",
	OccurredAt:        "occurred_at",
}

type securityAuditEventRecord struct {
	EventID           string  `gorm:"column:event_id;primaryKey"`
	OperationID       string  `gorm:"column:operation_id"`
	Event             string  `gorm:"column:event"`
	Action            string  `gorm:"column:action"`
	ActorType         string  `gorm:"column:actor_type"`
	ActorID           *string `gorm:"column:actor_id"`
	SourceIP          *string `gorm:"column:source_ip"`
	ResourceType      string  `gorm:"column:resource_type"`
	ResourceID        string  `gorm:"column:resource_id"`
	Result            string  `gorm:"column:result"`
	ErrorCode         *string `gorm:"column:error_code"`
	RequestID         *string `gorm:"column:request_id"`
	TraceID           *string `gorm:"column:trace_id"`
	BeforeStateDigest []byte  `gorm:"column:before_state_digest"`
	AfterStateDigest  []byte  `gorm:"column:after_state_digest"`
	OccurredAt        int64   `gorm:"column:occurred_at"`
}

func (securityAuditEventRecord) TableName() string { return SecurityAuditEventTable }

// SecurityAuditEvents 返回当前写事务内的 append-only 安全审计 Repository。
func (store *transactionStore) SecurityAuditEvents() repository.SecurityAuditEventRepository {
	return securityAuditEventRepository{database: store.database}
}

type securityAuditEventRepository struct{ database *gorm.DB }

// Append 幂等追加事件。相同 Event ID 与 Operation ID 的完全相同重放视为成功；
// 任一 ID 已绑定到不同内容时快速失败，绝不覆盖已有证据。
func (store securityAuditEventRepository) Append(ctx context.Context, event repository.SecurityAuditEvent) error {
	if err := event.Validate(); err != nil {
		return err
	}
	wanted := securityAuditEventRecordFromDomain(event)
	var existing securityAuditEventRecord
	err := store.database.WithContext(ctx).
		Where(SecurityAuditEventColumns.EventID+" = ? OR "+SecurityAuditEventColumns.OperationID+" = ?", event.EventID, event.OperationID).
		Take(&existing).Error
	switch {
	case err == nil:
		if existing.equal(wanted) {
			return nil
		}
		return repository.ErrSecurityAuditConflict
	case !errors.Is(err, gorm.ErrRecordNotFound):
		return fmt.Errorf("inspect existing security audit event: %w", err)
	}
	if err := store.database.WithContext(ctx).Create(&wanted).Error; err != nil {
		return fmt.Errorf("append security audit event: %w", err)
	}
	return nil
}

func securityAuditEventRecordFromDomain(event repository.SecurityAuditEvent) securityAuditEventRecord {
	return securityAuditEventRecord{
		EventID:           event.EventID,
		OperationID:       event.OperationID,
		Event:             event.Event,
		Action:            event.Action,
		ActorType:         event.ActorType,
		ActorID:           nullableString(event.ActorID),
		SourceIP:          nullableString(event.SourceIP),
		ResourceType:      event.ResourceType,
		ResourceID:        event.ResourceID,
		Result:            event.Result,
		ErrorCode:         nullableString(event.ErrorCode),
		RequestID:         nullableString(event.RequestID),
		TraceID:           nullableString(event.TraceID),
		BeforeStateDigest: nullableBytes(event.BeforeStateDigest),
		AfterStateDigest:  nullableBytes(event.AfterStateDigest),
		OccurredAt:        event.OccurredAt,
	}
}

func (record securityAuditEventRecord) equal(other securityAuditEventRecord) bool {
	return record.EventID == other.EventID && record.OperationID == other.OperationID &&
		record.Event == other.Event && record.Action == other.Action && record.ActorType == other.ActorType &&
		equalNullableString(record.ActorID, other.ActorID) && equalNullableString(record.SourceIP, other.SourceIP) &&
		record.ResourceType == other.ResourceType && record.ResourceID == other.ResourceID &&
		record.Result == other.Result && equalNullableString(record.ErrorCode, other.ErrorCode) &&
		equalNullableString(record.RequestID, other.RequestID) && equalNullableString(record.TraceID, other.TraceID) &&
		bytes.Equal(record.BeforeStateDigest, other.BeforeStateDigest) &&
		bytes.Equal(record.AfterStateDigest, other.AfterStateDigest) && record.OccurredAt == other.OccurredAt
}

func nullableString(value string) *string {
	if value == "" {
		return nil
	}
	copy := value
	return &copy
}

func nullableBytes(value []byte) []byte {
	if len(value) == 0 {
		return nil
	}
	return append([]byte(nil), value...)
}

func equalNullableString(left, right *string) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}
