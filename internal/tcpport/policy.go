// Package tcpport 定义公网 TCP Route 的逻辑端口池规则。
package tcpport

import (
	"errors"
	"fmt"
)

var (
	// ErrInvalidPolicy 表示端口池边界不合法。
	ErrInvalidPolicy = errors.New("TCP port policy is invalid")
	// ErrPortUnavailable 表示请求端口不在池内、被保留或已被 Desired Route 占用。
	ErrPortUnavailable = errors.New("TCP public port is unavailable")
	// ErrPoolExhausted 表示逻辑端口池没有可分配端口。
	ErrPoolExhausted = errors.New("TCP public port pool is exhausted")
)

// Policy 是 Server 配置解析后冻结的逻辑端口池。它只裁决 Desired State，绝不通过
// 试绑 Socket 探测 OS 状态；外部进程冲突由 Listener Reconcile 记录 APPLY_FAILED。
type Policy struct {
	min      uint16
	max      uint16
	reserved map[uint16]struct{}
}

// New 创建一个包含两端边界的逻辑端口池。
func New(minPort, maxPort int, reserved []uint16) (Policy, error) {
	if minPort < 1 || minPort > 65535 || maxPort < 1 || maxPort > 65535 || minPort > maxPort {
		return Policy{}, ErrInvalidPolicy
	}
	policy := Policy{
		min:      uint16(minPort),
		max:      uint16(maxPort),
		reserved: make(map[uint16]struct{}, len(reserved)),
	}
	for _, port := range reserved {
		if port != 0 {
			policy.reserved[port] = struct{}{}
		}
	}
	return policy, nil
}

// ValidateExplicit 拒绝池外、保留或已被任意 Desired Route 占用的显式端口。
// 禁用 Route 也必须出现在 used 中，因为其持久化行继续保留逻辑占用。
func (policy Policy) ValidateExplicit(port uint16, used map[uint16]struct{}) error {
	if port < policy.min || port > policy.max {
		return fmt.Errorf("%w: port %d is outside %d..%d", ErrPortUnavailable, port, policy.min, policy.max)
	}
	if _, exists := policy.reserved[port]; exists {
		return fmt.Errorf("%w: port %d is reserved", ErrPortUnavailable, port)
	}
	if _, exists := used[port]; exists {
		return fmt.Errorf("%w: port %d is already assigned", ErrPortUnavailable, port)
	}
	return nil
}

// Allocate 返回池内第一个逻辑空闲端口。选择顺序只是确定性实现细节；持久化后只有
// 具体端口是权威，不新增 allocator 游标或第二套状态。
func (policy Policy) Allocate(used map[uint16]struct{}) (uint16, error) {
	for candidate := uint32(policy.min); candidate <= uint32(policy.max); candidate++ {
		port := uint16(candidate)
		if _, reserved := policy.reserved[port]; reserved {
			continue
		}
		if _, assigned := used[port]; assigned {
			continue
		}
		return port, nil
	}
	return 0, ErrPoolExhausted
}
