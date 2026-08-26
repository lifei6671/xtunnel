// Package deterministic 提供 Protocol v1 参与 HMAC、大小 Gate 与 Golden Vector 的确定性编码。
package deterministic

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"sort"

	"google.golang.org/protobuf/proto"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
)

const (
	// ProtobufRuntimeVersion 锁定会影响 Protocol v1 Golden Vector 的 Protobuf Runtime 版本。
	ProtobufRuntimeVersion = "google.golang.org/protobuf v1.36.12"

	// WorkMACDomain 将 WorkHello MAC 与其他 HMAC 输入隔离。
	WorkMACDomain = "xtunnel-work-v1"
)

var (
	// ErrNilMessage 表示调用方没有传入可编码的协议消息。
	ErrNilMessage = errors.New("deterministic: nil protobuf message")

	// ErrInvalidSessionSecret 表示 WorkHello HMAC 使用了非 32 字节 Session Secret。
	ErrInvalidSessionSecret = errors.New("deterministic: session secret must be 32 bytes")
)

// Marshal 在拒绝任何递归 Unknown Field 后，返回固定 Protobuf Runtime 的确定性字节。
func Marshal(message proto.Message) ([]byte, error) {
	if message == nil {
		return nil, ErrNilMessage
	}
	if err := validate.RejectUnknownFields(message); err != nil {
		return nil, err
	}
	return proto.MarshalOptions{Deterministic: true}.Marshal(message)
}

// MarshalSnapshot 复制并按 service_id 稳定排序 Tunnel Snapshot 中的 Service，再做确定性编码。
// 它绝不改写调用方的运行中 Snapshot，避免编码或大小检查改变实际配置顺序。
func MarshalSnapshot(snapshot *protocolv1.TunnelSnapshot) ([]byte, error) {
	if snapshot == nil {
		return nil, ErrNilMessage
	}

	copySnapshot := proto.Clone(snapshot).(*protocolv1.TunnelSnapshot)
	sort.SliceStable(copySnapshot.Services, func(left, right int) bool {
		return copySnapshot.Services[left].GetServiceId() < copySnapshot.Services[right].GetServiceId()
	})
	return Marshal(copySnapshot)
}

// WorkHelloBytesWithoutMAC 复制 WorkHello 并清空 mac 后返回确定性 HMAC 输入。
func WorkHelloBytesWithoutMAC(hello *protocolv1.WorkHello) ([]byte, error) {
	if hello == nil {
		return nil, ErrNilMessage
	}
	for _, field := range []struct {
		value  string
		prefix string
	}{
		{value: hello.GetTunnelId(), prefix: "tun_"},
		{value: hello.GetConnectorId(), prefix: "con_"},
		{value: hello.GetSessionId(), prefix: "sess_"},
		{value: hello.GetWorkId(), prefix: "work_"},
		{value: hello.GetBudgetLeaseId(), prefix: "lease_"},
	} {
		if err := validate.ValidateID(field.value, field.prefix); err != nil {
			return nil, err
		}
	}

	copyHello := proto.Clone(hello).(*protocolv1.WorkHello)
	copyHello.Mac = nil
	return Marshal(copyHello)
}

// ComputeWorkHelloMAC 按冻结域分隔规则计算 WorkHello 的 HMAC-SHA256。
func ComputeWorkHelloMAC(sessionSecret []byte, hello *protocolv1.WorkHello) ([]byte, error) {
	if len(sessionSecret) != sha256.Size {
		return nil, ErrInvalidSessionSecret
	}

	payload, err := WorkHelloBytesWithoutMAC(hello)
	if err != nil {
		return nil, err
	}

	mac := hmac.New(sha256.New, sessionSecret)
	_, _ = mac.Write([]byte(WorkMACDomain))
	_, _ = mac.Write(payload)
	return mac.Sum(nil), nil
}
