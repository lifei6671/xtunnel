// Package controlsession 提供 Control Session 的内存运行组件。
package controlsession

import (
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

var (
	// ErrInvalidOutboxProtocol 表示 Outbox 没有可写入 Envelope 的协商协议版本。
	ErrInvalidOutboxProtocol = errors.New("control outbox protocol version is invalid")
	// ErrInvalidOutboxCapacity 表示高优先级或普通队列容量不是正数。
	ErrInvalidOutboxCapacity = errors.New("control outbox capacity is invalid")
	// ErrOutboxFull 表示消息经过允许的合并后仍无法放入有界队列。
	ErrOutboxFull = errors.New("control outbox is full")
	// ErrInvalidOutboxMessage 表示消息缺少合并所需的协议版本、Payload 或业务 Key。
	ErrInvalidOutboxMessage = errors.New("control outbox message is invalid")
	// ErrOutboxMessageTooLarge 表示单个不可再拆的待发消息已超过当前 Control Frame 上限。
	ErrOutboxMessageTooLarge = errors.New("control outbox message exceeds frame limit")
	// ErrUnsupportedOutboxMessage 表示消息包含 Protocol v1 未声明的字段或 Payload。
	ErrUnsupportedOutboxMessage = errors.New("control outbox message is unsupported")
)

type normalKind uint8

const (
	normalTunnelSnapshot normalKind = iota + 1
	normalWorkDemand
	normalServiceHealth
)

type normalEntry struct {
	kind     normalKind
	key      string
	envelope *protocolv1.ControlEnvelope
	health   *protocolv1.ServiceHealth
}

// Outbox 是每条 Control Session 独占的并发安全、有界且非阻塞的发送队列。
//
// WorkDemand 的冻结消息没有 connector_id；一个 Outbox 只属于一个已经认证的
// Connector Session，因此该 Key 由实例边界隐含，同一实例内只保留最高 generation。
// normalCapacity 按可合并 Key 计数：每个 Tunnel Snapshot、当前 Session 的 WorkDemand
// 以及每个 service_id 的待发 Health 各占一个槽，从而让 Health accumulator 同样有界。
// Outbox 不启动 goroutine，也不接触 net.Conn；未来唯一 writeLoop 主动调用 Dequeue。
type Outbox struct {
	mu sync.Mutex

	protocolVersion  uint32
	highCapacity     int
	normalCapacity   int
	maxFrameBytes    uint64
	high             []*protocolv1.ControlEnvelope
	normal           []normalEntry
	healthGeneration uint64
}

// NewOutbox 创建固定 Protocol 版本和容量的空 Outbox。
func NewOutbox(protocolVersion uint32, highCapacity, normalCapacity int) (*Outbox, error) {
	return newOutbox(protocolVersion, highCapacity, normalCapacity, frame.MaxControlFrameSize)
}

func newOutbox(protocolVersion uint32, highCapacity, normalCapacity int, maxFrameBytes uint64) (*Outbox, error) {
	if protocolVersion == 0 {
		return nil, ErrInvalidOutboxProtocol
	}
	if highCapacity <= 0 || normalCapacity <= 0 || maxFrameBytes == 0 || maxFrameBytes > frame.MaxControlFrameSize {
		return nil, ErrInvalidOutboxCapacity
	}
	return &Outbox{
		protocolVersion: protocolVersion,
		highCapacity:    highCapacity,
		normalCapacity:  normalCapacity,
		maxFrameBytes:   maxFrameBytes,
		high:            make([]*protocolv1.ControlEnvelope, 0, highCapacity),
		normal:          make([]normalEntry, 0, normalCapacity),
	}, nil
}

// Enqueue 尝试立即投递一条 ControlEnvelope；它从不等待容量释放。
//
// 输入在持锁前完成深拷贝，调用方在本方法返回后修改原消息不会改变队列内容。
// ServiceHealthBatch 仅作为待合并的 Health 容器输入，generation 必须为零；真正的
// Batch generation 只在 Dequeue 冻结该批次时分配。
func (outbox *Outbox) Enqueue(envelope *protocolv1.ControlEnvelope) error {
	if err := validateEnvelopeShape(envelope, outbox.protocolVersion); err != nil {
		return err
	}
	owned := proto.Clone(envelope).(*protocolv1.ControlEnvelope)

	outbox.mu.Lock()
	defer outbox.mu.Unlock()

	switch payload := owned.GetPayload().(type) {
	case *protocolv1.ControlEnvelope_Heartbeat:
		return outbox.enqueueHeartbeat(owned)
	case *protocolv1.ControlEnvelope_Error,
		*protocolv1.ControlEnvelope_DrainRequest,
		*protocolv1.ControlEnvelope_DrainAck,
		*protocolv1.ControlEnvelope_ConfigAck:
		return outbox.enqueueHigh(owned)
	case *protocolv1.ControlEnvelope_ConfigSnapshot:
		return outbox.enqueueSnapshot(owned, payload.ConfigSnapshot)
	case *protocolv1.ControlEnvelope_WorkDemand:
		return outbox.enqueueWorkDemand(owned, payload.WorkDemand)
	case *protocolv1.ControlEnvelope_ServiceHealthBatch:
		return outbox.enqueueHealth(payload.ServiceHealthBatch)
	default:
		return ErrUnsupportedOutboxMessage
	}
}

// Dequeue 立即取出下一条消息，高优先级始终先于普通消息。
// 返回的消息已经脱离 Outbox 所有内部引用；之后的合并不会改写已出队 Frame。
func (outbox *Outbox) Dequeue() (*protocolv1.ControlEnvelope, bool) {
	outbox.mu.Lock()
	defer outbox.mu.Unlock()

	if len(outbox.high) > 0 {
		envelope := outbox.high[0]
		clear(outbox.high[:1])
		outbox.high = outbox.high[1:]
		return envelope, true
	}
	if len(outbox.normal) == 0 {
		return nil, false
	}
	if outbox.normal[0].kind != normalServiceHealth {
		envelope := outbox.normal[0].envelope
		outbox.normal[0] = normalEntry{}
		outbox.normal = outbox.normal[1:]
		return envelope, true
	}

	return outbox.dequeueHealthBatch(), true
}

func (outbox *Outbox) enqueueHigh(envelope *protocolv1.ControlEnvelope) error {
	if len(outbox.high) >= outbox.highCapacity {
		return ErrOutboxFull
	}
	outbox.high = append(outbox.high, envelope)
	return nil
}

func (outbox *Outbox) enqueueHeartbeat(envelope *protocolv1.ControlEnvelope) error {
	for index, queued := range outbox.high {
		if _, ok := queued.GetPayload().(*protocolv1.ControlEnvelope_Heartbeat); ok {
			outbox.high[index] = envelope
			return nil
		}
	}
	return outbox.enqueueHigh(envelope)
}

func (outbox *Outbox) enqueueSnapshot(envelope *protocolv1.ControlEnvelope, snapshot *protocolv1.TunnelSnapshot) error {
	for index := range outbox.normal {
		entry := &outbox.normal[index]
		if entry.kind != normalTunnelSnapshot || entry.key != snapshot.GetTunnelId() {
			continue
		}
		queued := entry.envelope.GetConfigSnapshot()
		if snapshot.GetRevision() > queued.GetRevision() {
			entry.envelope = envelope
		}
		return nil
	}
	if len(outbox.normal) >= outbox.normalCapacity {
		return ErrOutboxFull
	}
	outbox.normal = append(outbox.normal, normalEntry{
		kind: normalTunnelSnapshot, key: snapshot.GetTunnelId(), envelope: envelope,
	})
	return nil
}

func (outbox *Outbox) enqueueWorkDemand(envelope *protocolv1.ControlEnvelope, demand *protocolv1.WorkDemand) error {
	for index := range outbox.normal {
		entry := &outbox.normal[index]
		if entry.kind != normalWorkDemand {
			continue
		}
		queued := entry.envelope.GetWorkDemand()
		if demand.GetDemandGeneration() > queued.GetDemandGeneration() {
			entry.envelope = envelope
		}
		return nil
	}
	if len(outbox.normal) >= outbox.normalCapacity {
		return ErrOutboxFull
	}
	outbox.normal = append(outbox.normal, normalEntry{kind: normalWorkDemand, envelope: envelope})
	return nil
}

func (outbox *Outbox) enqueueHealth(batch *protocolv1.ServiceHealthBatch) error {
	// 同一输入 Batch 内重复 service_id 时，以最后出现的观测为最新值；firstSeen
	// 只用于让新增 Key 的队列次序保持确定。
	latest := make(map[string]*protocolv1.ServiceHealth, len(batch.GetItems()))
	firstSeen := make([]string, 0, len(batch.GetItems()))
	for _, item := range batch.GetItems() {
		// 使用最大 uint64 generation 做单项上限检查，保证条目进入队列后不会因
		// generation 的 Varint 长度增长而变成无法发送的永久队头。
		batchPayloadSize := protowire.SizeTag(1) + protowire.SizeVarint(math.MaxUint64) + healthItemSize(item)
		if uint64(healthEnvelopeSize(outbox.protocolVersion, math.MaxUint64, batchPayloadSize)) > outbox.maxFrameBytes {
			return ErrOutboxMessageTooLarge
		}
		serviceID := item.GetServiceId()
		if _, exists := latest[serviceID]; !exists {
			firstSeen = append(firstSeen, serviceID)
		}
		latest[serviceID] = item
	}

	existing := make(map[string]int, len(latest))
	for index := range outbox.normal {
		entry := &outbox.normal[index]
		if entry.kind == normalServiceHealth {
			existing[entry.key] = index
		}
	}
	newItems := 0
	for _, serviceID := range firstSeen {
		if _, exists := existing[serviceID]; !exists {
			newItems++
		}
	}
	if len(outbox.normal)+newItems > outbox.normalCapacity {
		return ErrOutboxFull
	}

	for _, serviceID := range firstSeen {
		if index, exists := existing[serviceID]; exists {
			outbox.normal[index].health = latest[serviceID]
			continue
		}
		outbox.normal = append(outbox.normal, normalEntry{
			kind: normalServiceHealth, key: serviceID, health: latest[serviceID],
		})
	}
	return nil
}

func (outbox *Outbox) dequeueHealthBatch() *protocolv1.ControlEnvelope {
	nextGeneration := outbox.healthGeneration + 1
	items := make([]*protocolv1.ServiceHealth, 0, len(outbox.normal))
	remaining := make([]normalEntry, 0, len(outbox.normal))
	batchPayloadSize := protowire.SizeTag(1) + protowire.SizeVarint(nextGeneration)
	batchFull := false
	for index := range outbox.normal {
		entry := outbox.normal[index]
		if entry.kind != normalServiceHealth || batchFull {
			remaining = append(remaining, entry)
			continue
		}
		candidatePayloadSize := batchPayloadSize + healthItemSize(entry.health)
		if uint64(healthEnvelopeSize(outbox.protocolVersion, nextGeneration, candidatePayloadSize)) > outbox.maxFrameBytes {
			batchFull = true
			remaining = append(remaining, entry)
			continue
		}
		items = append(items, entry.health)
		batchPayloadSize = candidatePayloadSize
		outbox.normal[index] = normalEntry{}
	}
	outbox.normal = remaining
	outbox.healthGeneration = nextGeneration
	return &protocolv1.ControlEnvelope{
		ProtocolVersion: outbox.protocolVersion,
		Payload: &protocolv1.ControlEnvelope_ServiceHealthBatch{
			ServiceHealthBatch: &protocolv1.ServiceHealthBatch{
				Generation: outbox.healthGeneration,
				Items:      items,
			},
		},
	}
}

func healthItemSize(item *protocolv1.ServiceHealth) int {
	return protowire.SizeTag(2) + protowire.SizeBytes(proto.Size(item))
}

func healthEnvelopeSize(protocolVersion uint32, generation uint64, batchPayloadSize int) int {
	return protowire.SizeTag(1) + protowire.SizeVarint(uint64(protocolVersion)) +
		protowire.SizeTag(14) + protowire.SizeBytes(batchPayloadSize)
}

func validateEnvelopeShape(envelope *protocolv1.ControlEnvelope, protocolVersion uint32) error {
	if envelope == nil || envelope.GetProtocolVersion() != protocolVersion {
		return ErrInvalidOutboxMessage
	}
	if err := validate.RejectUnknownFields(envelope); err != nil {
		return fmt.Errorf("%w: %w", ErrUnsupportedOutboxMessage, err)
	}

	switch payload := envelope.GetPayload().(type) {
	case *protocolv1.ControlEnvelope_Heartbeat:
		if payload == nil || payload.Heartbeat == nil {
			return ErrInvalidOutboxMessage
		}
	case *protocolv1.ControlEnvelope_Error:
		if payload == nil || payload.Error == nil {
			return ErrInvalidOutboxMessage
		}
	case *protocolv1.ControlEnvelope_DrainRequest:
		if payload == nil || payload.DrainRequest == nil {
			return ErrInvalidOutboxMessage
		}
	case *protocolv1.ControlEnvelope_DrainAck:
		if payload == nil || payload.DrainAck == nil {
			return ErrInvalidOutboxMessage
		}
	case *protocolv1.ControlEnvelope_ConfigAck:
		if payload == nil || payload.ConfigAck == nil {
			return ErrInvalidOutboxMessage
		}
	case *protocolv1.ControlEnvelope_ConfigSnapshot:
		if payload == nil || payload.ConfigSnapshot == nil ||
			!validate.ValidID(payload.ConfigSnapshot.GetTunnelId(), "tun_") {
			return ErrInvalidOutboxMessage
		}
	case *protocolv1.ControlEnvelope_WorkDemand:
		if payload == nil || payload.WorkDemand == nil {
			return ErrInvalidOutboxMessage
		}
	case *protocolv1.ControlEnvelope_ServiceHealthBatch:
		if payload == nil || payload.ServiceHealthBatch == nil || payload.ServiceHealthBatch.GetGeneration() != 0 ||
			len(payload.ServiceHealthBatch.GetItems()) == 0 {
			return ErrInvalidOutboxMessage
		}
		for _, item := range payload.ServiceHealthBatch.GetItems() {
			if item == nil || !validate.ValidID(item.GetServiceId(), "svc_") {
				return ErrInvalidOutboxMessage
			}
		}
	default:
		return ErrUnsupportedOutboxMessage
	}
	return nil
}
