// Package workauth 实现 Server 侧纯内存 WorkHello 认证与 Budget Lease 原子消费。
//
// 本包不读写网络，不持有 Runtime Registry，也不负责 WorkReady Frame。调用方把每个
// SessionAuthenticator 绑定到一个已认证 Session，并根据 DecisionError 的冻结错误码
// 构造公开响应。
package workauth

import (
	"crypto/hmac"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/protocol/deterministic"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
)

const sessionSecretSize = 32

var (
	// ErrInvalidConfig 表示 Authenticator 构造参数不满足内部安全边界。
	ErrInvalidConfig = errors.New("server work authenticator config is invalid")
)

// MonotonicClock 返回相对同一进程起点的单调时长。
//
// 生产调用方应以 time.Since(startedAt) 实现；本接口不接受 Unix 时间，从类型层避免
// WorkHello 认证依赖 Agent 与 Server 的 wall clock 一致性。
type MonotonicClock func() time.Duration

// Session 描述一个 Authenticator 独占的已认证 Session 身份和 HMAC Secret。
type Session struct {
	// TunnelID 是当前长期 Token 认证出的 tun_<ULID>。
	TunnelID string
	// ConnectorID 是当前 Agent 进程的 con_<ULID>。
	ConnectorID string
	// SessionID 是当前 Control Session 的 sess_<ULID>。
	SessionID string
	// Generation 是同一 Tunnel/Connector 的当前 fencing generation。
	Generation uint64
	// Secret 必须恰为 32 字节；New 会复制，调用方后续修改不影响认证状态。
	Secret []byte
}

// Reason 是 Server 内部稳定的 WorkHello 拒绝原因。
//
// Protocol v1 没有为 Replay 与 Lease Budget 单独冻结公开错误码，因此 Reason 保留
// 精细诊断，DecisionError.Code 只使用已经冻结的 ErrorCode。
type Reason uint8

const (
	// ReasonProtocol 表示消息形状、Unknown Field 或固定 ID 格式不合法。
	ReasonProtocol Reason = iota + 1
	// ReasonSessionInvalid 合并身份不匹配与 HMAC 错误，避免认证 Oracle。
	ReasonSessionInvalid
	// ReasonLeaseInvalid 表示 HMAC 正确但 Lease 不属于当前 Session。
	ReasonLeaseInvalid
	// ReasonLeaseExpired 表示目标 Lease 已到 Server 单调 Deadline。
	ReasonLeaseExpired
	// ReasonBudgetExhausted 表示目标 Lease 已没有可消费 Slot。
	ReasonBudgetExhausted
	// ReasonReplay 表示 WorkID 仍存在于当前 Session Replay Cache。
	ReasonReplay
	// ReasonReplayCapacity 表示当前 Session Replay Cache 达到固定上限。
	ReasonReplayCapacity
	// ReasonClosed 表示 Session Authenticator 已关闭。
	ReasonClosed
)

// String 返回固定且不含任何认证输入的原因文本。
func (reason Reason) String() string {
	switch reason {
	case ReasonProtocol:
		return "protocol"
	case ReasonSessionInvalid:
		return "session_invalid"
	case ReasonLeaseInvalid:
		return "lease_invalid"
	case ReasonLeaseExpired:
		return "lease_expired"
	case ReasonBudgetExhausted:
		return "budget_exhausted"
	case ReasonReplay:
		return "replay"
	case ReasonReplayCapacity:
		return "replay_capacity"
	case ReasonClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// DecisionError 是可稳定映射到 WorkReady 的拒绝结果。
type DecisionError struct {
	// Reason 保留 Server 内部的精确拒绝原因。
	Reason Reason
	// Code 是允许写入公开 WorkReady 的冻结 Protocol v1 错误码。
	Code protocolv1.ErrorCode
}

// Error 只输出内部原因和冻结错误码，绝不包含 ID、Secret、MAC 或 nonce。
func (decision *DecisionError) Error() string {
	return fmt.Sprintf("server work auth rejected: reason=%s code=%s", decision.Reason.String(), decision.Code.String())
}

type leaseState struct {
	deadline  time.Duration
	remaining uint32
	replays   map[string]struct{}
}

// SessionAuthenticator 线性化一个已认证 Session 的 Lease、Slot 与 Replay 状态。
//
// 所有 Map 访问、过期清理、Slot 消费与 Replay 登记都只发生在 mu 临界区；HMAC 在
// Map 外计算，但 Secret 会先在同一锁下复制，并在最终原子裁决时再次检查 closed。
type SessionAuthenticator struct {
	mu sync.Mutex

	tunnelID    string
	connectorID string
	sessionID   string
	generation  uint64
	secret      [sessionSecretSize]byte
	maxReplay   int
	clock       MonotonicClock
	closed      bool

	leases      map[string]*leaseState
	replayIndex map[string]struct{}
}

// New 构造只属于一个已认证完整 Session 的 Authenticator。
func New(session Session, maxReplay int, clock MonotonicClock) (*SessionAuthenticator, error) {
	if err := identity.ValidateTunnelID(session.TunnelID); err != nil {
		return nil, fmt.Errorf("%w: tunnel id", ErrInvalidConfig)
	}
	if err := identity.ValidateConnectorID(session.ConnectorID); err != nil {
		return nil, fmt.Errorf("%w: connector id", ErrInvalidConfig)
	}
	if err := identity.ValidateSessionID(session.SessionID); err != nil {
		return nil, fmt.Errorf("%w: session id", ErrInvalidConfig)
	}
	if session.Generation == 0 || len(session.Secret) != sessionSecretSize || maxReplay <= 0 || clock == nil {
		return nil, ErrInvalidConfig
	}

	authenticator := &SessionAuthenticator{
		tunnelID: session.TunnelID, connectorID: session.ConnectorID,
		sessionID: session.SessionID, generation: session.Generation,
		maxReplay: maxReplay, clock: clock,
		leases: make(map[string]*leaseState), replayIndex: make(map[string]struct{}),
	}
	copy(authenticator.secret[:], session.Secret)
	return authenticator, nil
}

// GrantLease 为当前 Session 建立一个使用 Server 单调时间裁决的 Budget Lease。
//
// leaseID、slots 和 TTL 是 Server Control Owner 的内部输入；非法值或重复的未过期
// Lease 作为协议生命周期错误拒绝，绝不静默覆盖剩余 Slot 或 Replay Bucket。
func (authenticator *SessionAuthenticator) GrantLease(leaseID string, slots uint32, ttl time.Duration) error {
	if authenticator == nil {
		return decisionError(ReasonClosed)
	}
	if err := validate.ValidateID(leaseID, "lease_"); err != nil || slots == 0 || ttl <= 0 {
		return decisionError(ReasonProtocol)
	}

	authenticator.mu.Lock()
	defer authenticator.mu.Unlock()
	if authenticator.closed {
		return decisionError(ReasonClosed)
	}
	now := authenticator.clock()
	deadline := now + ttl
	if now < 0 || deadline <= now {
		return decisionError(ReasonProtocol)
	}
	authenticator.cleanupExpiredLocked(now)
	if _, exists := authenticator.leases[leaseID]; exists {
		return decisionError(ReasonProtocol)
	}
	authenticator.leases[leaseID] = &leaseState{
		deadline: deadline, remaining: slots, replays: make(map[string]struct{}),
	}
	return nil
}

// RevokeLease 立即撤销 Demand 已替换或取消的 Budget Lease。缺失 Lease 视为已经
// 撤销或过期；成功返回后，尚未在线性化点消费的 WorkHello 会得到 LeaseInvalid。
func (authenticator *SessionAuthenticator) RevokeLease(leaseID string) error {
	if authenticator == nil {
		return decisionError(ReasonClosed)
	}
	if err := validate.ValidateID(leaseID, "lease_"); err != nil {
		return decisionError(ReasonProtocol)
	}
	authenticator.mu.Lock()
	defer authenticator.mu.Unlock()
	if authenticator.closed {
		return decisionError(ReasonClosed)
	}
	if lease := authenticator.leases[leaseID]; lease != nil {
		authenticator.removeLeaseLocked(leaseID, lease)
	}
	return nil
}

// ValidateAndConsume 校验 WorkHello，并原子消费一个 Lease Slot、登记 Replay。
//
// Unknown Field、ID、身份和固定长度检查在任何 Map/HMAC 前完成。HMAC 通过后，目标
// Lease、Deadline、Slot、Replay 与容量的最终裁决位于同一个 mu 临界区；任一失败
// 都不会消费 Slot 或写入 Replay。
func (authenticator *SessionAuthenticator) ValidateAndConsume(hello *protocolv1.WorkHello) error {
	if authenticator == nil {
		return decisionError(ReasonClosed)
	}
	if err := validateHelloShape(hello); err != nil {
		return err
	}
	if hello.GetTunnelId() != authenticator.tunnelID || hello.GetConnectorId() != authenticator.connectorID ||
		hello.GetSessionId() != authenticator.sessionID {
		return decisionError(ReasonSessionInvalid)
	}

	var secret [sessionSecretSize]byte
	authenticator.mu.Lock()
	if authenticator.closed {
		authenticator.mu.Unlock()
		return decisionError(ReasonClosed)
	}
	copy(secret[:], authenticator.secret[:])
	authenticator.mu.Unlock()
	defer clear(secret[:])
	wantedMAC, err := deterministic.ComputeWorkHelloMAC(secret[:], hello)
	if err != nil {
		return decisionError(ReasonProtocol)
	}
	defer clear(wantedMAC)
	if !hmac.Equal(hello.GetMac(), wantedMAC) {
		// 未知 Session 与错误 HMAC 使用同一公开 Code，避免认证 Oracle。
		return decisionError(ReasonSessionInvalid)
	}

	authenticator.mu.Lock()
	defer authenticator.mu.Unlock()
	if authenticator.closed {
		return decisionError(ReasonClosed)
	}
	now := authenticator.clock()
	target, exists := authenticator.leases[hello.GetBudgetLeaseId()]
	if exists && now >= target.deadline {
		authenticator.removeLeaseLocked(hello.GetBudgetLeaseId(), target)
		authenticator.cleanupExpiredLocked(now)
		return decisionError(ReasonLeaseExpired)
	}
	authenticator.cleanupExpiredLocked(now)
	if !exists {
		return decisionError(ReasonLeaseInvalid)
	}
	if target.remaining == 0 {
		return decisionError(ReasonBudgetExhausted)
	}
	if _, replayed := authenticator.replayIndex[hello.GetWorkId()]; replayed {
		return decisionError(ReasonReplay)
	}
	if len(authenticator.replayIndex) >= authenticator.maxReplay {
		return decisionError(ReasonReplayCapacity)
	}

	// 这是唯一成功线性化点：Slot 消费与 Replay 登记在锁内不可分割。
	target.remaining--
	target.replays[hello.GetWorkId()] = struct{}{}
	authenticator.replayIndex[hello.GetWorkId()] = struct{}{}
	return nil
}

// Close 幂等关闭 Session，清除 Secret、Lease 与 Replay 状态。
func (authenticator *SessionAuthenticator) Close() {
	if authenticator == nil {
		return
	}
	authenticator.mu.Lock()
	defer authenticator.mu.Unlock()
	if authenticator.closed {
		return
	}
	authenticator.closed = true
	clear(authenticator.secret[:])
	for leaseID, lease := range authenticator.leases {
		authenticator.removeLeaseLocked(leaseID, lease)
	}
	clear(authenticator.replayIndex)
	authenticator.leases = nil
	authenticator.replayIndex = nil
}

func validateHelloShape(hello *protocolv1.WorkHello) error {
	if hello == nil {
		return decisionError(ReasonProtocol)
	}
	if err := validate.RejectUnknownFields(hello); err != nil {
		return decisionError(ReasonProtocol)
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
			return decisionError(ReasonProtocol)
		}
	}
	if len(hello.GetNonce()) != sessionSecretSize || len(hello.GetMac()) != sessionSecretSize {
		return decisionError(ReasonProtocol)
	}
	return nil
}

func (authenticator *SessionAuthenticator) cleanupExpiredLocked(now time.Duration) {
	for leaseID, lease := range authenticator.leases {
		if now >= lease.deadline {
			authenticator.removeLeaseLocked(leaseID, lease)
		}
	}
}

func (authenticator *SessionAuthenticator) removeLeaseLocked(leaseID string, lease *leaseState) {
	for workID := range lease.replays {
		delete(authenticator.replayIndex, workID)
	}
	clear(lease.replays)
	delete(authenticator.leases, leaseID)
}

func decisionError(reason Reason) *DecisionError {
	var code protocolv1.ErrorCode
	switch reason {
	case ReasonProtocol:
		code = protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR
	case ReasonLeaseExpired, ReasonBudgetExhausted:
		code = protocolv1.ErrorCode_ERROR_CODE_WORK_POOL_EXHAUSTED
	case ReasonReplayCapacity:
		code = protocolv1.ErrorCode_ERROR_CODE_SESSION_RESOURCE_EXHAUSTED
	case ReasonSessionInvalid, ReasonLeaseInvalid, ReasonReplay, ReasonClosed:
		code = protocolv1.ErrorCode_ERROR_CODE_SESSION_INVALID
	default:
		code = protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR
	}
	return &DecisionError{Reason: reason, Code: code}
}
