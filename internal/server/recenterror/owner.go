// Package recenterror 提供 Dashboard 最近错误的固定容量进程内投影。
package recenterror

import (
	"errors"
	"sort"
	"sync/atomic"
	"time"
)

var (
	// ErrInvalidCode 表示输入不是 Dashboard 冻结的五类错误之一。
	ErrInvalidCode = errors.New("recent error code is invalid")
	// ErrInvalidOccurredAt 表示错误事件没有有效的发生时间。
	ErrInvalidOccurredAt = errors.New("recent error occurred_at is invalid")
)

// Code 是 Dashboard 使用的有限错误分类。
type Code string

const (
	CodeTunnelOffline    Code = "TUNNEL_OFFLINE"
	CodeConnectorOffline Code = "CONNECTOR_OFFLINE"
	CodeOriginDown       Code = "ORIGIN_DOWN"
	CodeNoCapacity       Code = "NO_CAPACITY"
	CodeProtocolError    Code = "PROTOCOL_ERROR"

	slotCount = 5
)

const (
	messageTunnelOffline    = "Tunnel 已离线"
	messageConnectorOffline = "Connector 已离线"
	messageOriginDown       = "Origin 当前不可用"
	messageNoCapacity       = "当前没有可用容量"
	messageProtocolError    = "检测到协议错误"
)

// Record 是一次类型化错误观测。调用方不能提供 message 或底层 error，固定文案只由
// Owner 根据有限 Code 生成。RequestID 只能传入调用链中已经存在的真实关联 ID。
type Record struct {
	Code       Code
	OccurredAt time.Time
	RequestID  *string
}

// Item 是 Snapshot 返回的稳定 Dashboard 投影。
type Item struct {
	Code       Code
	Message    string
	OccurredAt time.Time
	RequestID  *string
}

type storedRecord struct {
	code       Code
	message    string
	occurredAt time.Time
	requestID  *string
}

// Owner 以每类一个原子槽保存最新不可变记录。它不是事件队列：同类新记录按
// occurred_at 覆盖旧记录，迟到的旧记录不会倒退投影，因此发布不阻塞数据面，也没有
// 队列满或 Drop Metric 语义。Owner 的零值可直接使用。
type Owner struct {
	slots [slotCount]atomic.Pointer[storedRecord]
}

// NewOwner 创建一个空的最近错误投影。
func NewOwner() *Owner {
	return &Owner{}
}

// Publish 把一条有效记录发布到对应类别的固定槽。OccurredAt 会转换为 UTC；同类别
// 迟到的旧记录视为已被更新的投影覆盖，不替换当前最新值。
func (owner *Owner) Publish(record Record) error {
	index, message, ok := codeProjection(record.Code)
	if !ok {
		return ErrInvalidCode
	}
	if record.OccurredAt.IsZero() {
		return ErrInvalidOccurredAt
	}
	projected := &storedRecord{
		code:       record.Code,
		message:    message,
		occurredAt: record.OccurredAt.UTC(),
		requestID:  cloneString(record.RequestID),
	}
	slot := &owner.slots[index]
	for {
		current := slot.Load()
		if current != nil && current.occurredAt.After(projected.occurredAt) {
			return nil
		}
		if slot.CompareAndSwap(current, projected) {
			return nil
		}
	}
}

// Snapshot 返回按发生时间倒序排列的独立副本。相同时间使用 Code 排序，避免并发发布
// 后向 API 暴露不稳定顺序。空 Owner 返回非 nil 空切片。
func (owner *Owner) Snapshot() []Item {
	items := make([]Item, 0, slotCount)
	for index := range owner.slots {
		stored := owner.slots[index].Load()
		if stored == nil {
			continue
		}
		items = append(items, Item{
			Code:       stored.code,
			Message:    stored.message,
			OccurredAt: stored.occurredAt,
			RequestID:  cloneString(stored.requestID),
		})
	}
	sort.Slice(items, func(left, right int) bool {
		if items[left].OccurredAt.Equal(items[right].OccurredAt) {
			return items[left].Code < items[right].Code
		}
		return items[left].OccurredAt.After(items[right].OccurredAt)
	})
	return items
}

func codeProjection(code Code) (int, string, bool) {
	switch code {
	case CodeTunnelOffline:
		return 0, messageTunnelOffline, true
	case CodeConnectorOffline:
		return 1, messageConnectorOffline, true
	case CodeOriginDown:
		return 2, messageOriginDown, true
	case CodeNoCapacity:
		return 3, messageNoCapacity, true
	case CodeProtocolError:
		return 4, messageProtocolError, true
	default:
		return 0, "", false
	}
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
