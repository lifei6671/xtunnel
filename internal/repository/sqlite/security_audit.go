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

// securityAuditEventRecord 是不可变安全审计事件的 SQLite 形状；可选字段用 NULL
// 区分“未提供”和空文本，Payload 则以独立字节副本保存。
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

// TableName 把 GORM 模型固定到安全审计表。
func (securityAuditEventRecord) TableName() string { return SecurityAuditEventTable }

// SecurityAuditEvents 返回当前写事务内的 append-only 安全审计 Repository。
func (store *transactionStore) SecurityAuditEvents() repository.SecurityAuditEventRepository {
	return securityAuditEventRepository{database: store.database, readOnly: store.readOnly}
}

// securityAuditEventRepository 只允许在 Store 管理的事务边界内追加审计记录。
type securityAuditEventRepository struct {
	database *gorm.DB
	readOnly bool
}

// QuerySecurityAuditEvents 按冻结的 (occurred_at, event_id) DESC 顺序读取一页证据。
// After 使用严格小于比较，确保同一秒内依靠 Event ID 完成稳定、不重叠的翻页。
func (store *Store) QuerySecurityAuditEvents(
	ctx context.Context,
	query repository.SecurityAuditEventQuery,
) (repository.SecurityAuditEventPage, error) {
	if ctx == nil {
		return repository.SecurityAuditEventPage{}, repository.ErrInvalidSecurityAuditEventQuery
	}
	if err := query.Validate(); err != nil {
		return repository.SecurityAuditEventPage{}, err
	}

	database := store.database.WithContext(ctx).Model(&securityAuditEventRecord{})
	if query.AppendSequenceUpper != nil {
		database = database.Where("rowid <= ?", *query.AppendSequenceUpper)
	}
	if query.Action != "" {
		database = database.Where(SecurityAuditEventColumns.Action+" = ?", query.Action)
	}
	if query.Result != "" {
		database = database.Where(SecurityAuditEventColumns.Result+" = ?", query.Result)
	}
	if query.ResourceType != "" {
		database = database.Where(SecurityAuditEventColumns.ResourceType+" = ?", query.ResourceType)
	}
	if query.ResourceID != "" {
		database = database.Where(SecurityAuditEventColumns.ResourceID+" = ?", query.ResourceID)
	}
	if query.OccurredFrom != nil {
		database = database.Where(SecurityAuditEventColumns.OccurredAt+" >= ?", *query.OccurredFrom)
	}
	if query.OccurredTo != nil {
		database = database.Where(SecurityAuditEventColumns.OccurredAt+" < ?", *query.OccurredTo)
	}
	if query.After != nil {
		database = database.Where(
			"("+SecurityAuditEventColumns.OccurredAt+" < ?) OR ("+
				SecurityAuditEventColumns.OccurredAt+" = ? AND "+SecurityAuditEventColumns.EventID+" < ?)",
			query.After.OccurredAt,
			query.After.OccurredAt,
			query.After.EventID,
		)
	}
	if query.Upper != nil {
		database = database.Where(
			"("+SecurityAuditEventColumns.OccurredAt+" < ?) OR ("+
				SecurityAuditEventColumns.OccurredAt+" = ? AND "+SecurityAuditEventColumns.EventID+" <= ?)",
			query.Upper.OccurredAt,
			query.Upper.OccurredAt,
			query.Upper.EventID,
		)
	}

	var records []securityAuditEventRecord
	if err := database.
		Order(SecurityAuditEventColumns.OccurredAt + " DESC").
		Order(SecurityAuditEventColumns.EventID + " DESC").
		Limit(query.Limit + 1).
		Find(&records).Error; err != nil {
		return repository.SecurityAuditEventPage{}, fmt.Errorf("query security audit events: %w", err)
	}

	hasNext := len(records) > query.Limit
	if hasNext {
		records = records[:query.Limit]
	}
	page := repository.SecurityAuditEventPage{
		Events: make([]repository.SecurityAuditEvent, 0, len(records)),
	}
	for _, record := range records {
		event := record.toDomain()
		if err := event.Validate(); err != nil {
			return repository.SecurityAuditEventPage{}, fmt.Errorf("decode security audit event %q: %w", record.EventID, err)
		}
		page.Events = append(page.Events, event)
	}
	if hasNext {
		last := page.Events[len(page.Events)-1]
		page.Next = &repository.SecurityAuditEventCursor{OccurredAt: last.OccurredAt, EventID: last.EventID}
	}
	return page, nil
}

// SecurityAuditEventExportBoundary 先冻结 append-only rowid 栅栏，再在同一筛选下
// 读取排序首项。tuple Upper 满足外部导出契约，rowid 栅栏额外排除导出开始后
// 回填旧 occurred_at 的并发追加；两者都只读，不改变 append-only 表。
func (store *Store) SecurityAuditEventExportBoundary(
	ctx context.Context,
	query repository.SecurityAuditEventQuery,
) (repository.SecurityAuditEventExportBoundary, bool, error) {
	if ctx == nil || query.After != nil || query.Upper != nil || query.AppendSequenceUpper != nil {
		return repository.SecurityAuditEventExportBoundary{}, false, repository.ErrInvalidSecurityAuditEventQuery
	}
	if err := query.Validate(); err != nil {
		return repository.SecurityAuditEventExportBoundary{}, false, err
	}
	var maximum int64
	if err := store.database.WithContext(ctx).Model(&securityAuditEventRecord{}).
		Select("COALESCE(MAX(rowid), 0)").Scan(&maximum).Error; err != nil {
		return repository.SecurityAuditEventExportBoundary{}, false, fmt.Errorf("freeze security audit append boundary: %w", err)
	}
	if maximum == 0 {
		return repository.SecurityAuditEventExportBoundary{}, false, nil
	}
	query.Limit = 1
	query.AppendSequenceUpper = &maximum
	page, err := store.QuerySecurityAuditEvents(ctx, query)
	if err != nil {
		return repository.SecurityAuditEventExportBoundary{}, false, err
	}
	if len(page.Events) == 0 {
		return repository.SecurityAuditEventExportBoundary{}, false, nil
	}
	first := page.Events[0]
	return repository.SecurityAuditEventExportBoundary{
		Upper:             repository.SecurityAuditEventCursor{OccurredAt: first.OccurredAt, EventID: first.EventID},
		MaxAppendSequence: maximum,
	}, true, nil
}

// Append 幂等追加事件。相同 Event ID 与 Operation ID 的完全相同重放视为成功；
// 任一 ID 已绑定到不同内容时快速失败，绝不覆盖已有证据。
func (store securityAuditEventRepository) Append(ctx context.Context, event repository.SecurityAuditEvent) error {
	if store.readOnly {
		return errRepositoryWriteOutsideTransaction
	}
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

// securityAuditEventRecordFromDomain 复制审计事件，避免调用方在提交后修改 Payload。
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

// toDomain 返回与 GORM 扫描缓存解耦的事件，避免上层修改 Digest 时污染后续读取。
func (record securityAuditEventRecord) toDomain() repository.SecurityAuditEvent {
	return repository.SecurityAuditEvent{
		EventID:           record.EventID,
		OperationID:       record.OperationID,
		Event:             record.Event,
		Action:            record.Action,
		ActorType:         record.ActorType,
		ActorID:           valueOrEmpty(record.ActorID),
		SourceIP:          valueOrEmpty(record.SourceIP),
		ResourceType:      record.ResourceType,
		ResourceID:        record.ResourceID,
		Result:            record.Result,
		ErrorCode:         valueOrEmpty(record.ErrorCode),
		RequestID:         valueOrEmpty(record.RequestID),
		TraceID:           valueOrEmpty(record.TraceID),
		BeforeStateDigest: nullableBytes(record.BeforeStateDigest),
		AfterStateDigest:  nullableBytes(record.AfterStateDigest),
		OccurredAt:        record.OccurredAt,
	}
}

// equal 用于幂等追加：同一 Event ID 只有逐字段完全一致才算安全重放，任何差异
// 都必须报告冲突，不能覆盖或吞掉已经落库的审计事实。
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

// nullableString 把缺失的可选审计文本编码为 SQL NULL。
func nullableString(value string) *string {
	if value == "" {
		return nil
	}
	copy := value
	return &copy
}

// nullableBytes 返回独立副本，空 Payload 保持 SQL NULL。
func nullableBytes(value []byte) []byte {
	if len(value) == 0 {
		return nil
	}
	return append([]byte(nil), value...)
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// equalNullableString 保留 NULL 与空字符串的语义差异。
func equalNullableString(left, right *string) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}
