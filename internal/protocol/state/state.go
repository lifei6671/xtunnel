// Package state 固化 Protocol v1 的纯状态、方向和幂等约束。
//
// 本包不读写网络连接，也不负责 Protobuf 解码或 Unknown Field 校验；调用方必须
// 在把已完整解码且已通过 Unknown Field 校验的消息交给本包前完成这些边界检查。
package state

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
)

var (
	// ErrIllegalDirection 表示消息发送者不符合冻结的单向协议定义。
	ErrIllegalDirection = errors.New("protocol state: illegal message direction")
	// ErrIllegalState 表示消息不允许出现在当前连接状态。
	ErrIllegalState = errors.New("protocol state: illegal connection state")
	// ErrUnexpectedMessage 表示当前阶段不接受该结构化消息类型。
	ErrUnexpectedMessage = errors.New("protocol state: unexpected message")
	// ErrProtocolVersion 表示 ControlEnvelope 的版本与认证协商结果不一致。
	ErrProtocolVersion = errors.New("protocol state: protocol version mismatch")
	// ErrInvalidControlEnvelope 表示 Envelope 缺少 oneof payload。
	ErrInvalidControlEnvelope = errors.New("protocol state: invalid control envelope")
	// ErrInvalidAuthResult 表示 AUTH 阶段的结果不是成功或失败之一。
	ErrInvalidAuthResult = errors.New("protocol state: invalid auth result")
	// ErrInvalidSessionSecret 表示 Auth 成功消息没有携带当前 Session 的 32 字节密钥。
	ErrInvalidSessionSecret = errors.New("protocol state: invalid session secret")
	// ErrRepeatedMessage 表示 WorkConn 收到重复的非幂等结构化消息。
	ErrRepeatedMessage = errors.New("protocol state: repeated non-idempotent message")
	// ErrConflictingDrain 表示同一 Drain ID 携带了不同的内容。
	ErrConflictingDrain = errors.New("protocol state: conflicting drain message")
	// ErrRevisionRollback 表示 ConfigAck 的 revision 倒退。
	ErrRevisionRollback = errors.New("protocol state: config ack revision rollback")
	// ErrConflictingConfigAck 表示同一 revision 的 ConfigAck 给出了不同状态。
	ErrConflictingConfigAck = errors.New("protocol state: conflicting config ack")
	// ErrInvalidConfigAck 表示 ConfigAck 使用了未指定的应用状态。
	ErrInvalidConfigAck = errors.New("protocol state: invalid config ack")
	// ErrConnectionIDMismatch 表示 OpenResponse 没有回应正在打开的连接。
	ErrConnectionIDMismatch = errors.New("protocol state: connection id mismatch")
	// ErrWorkIDMismatch 表示 WorkReady 没有回应当前 WorkHello。
	ErrWorkIDMismatch = errors.New("protocol state: work id mismatch")
	// ErrRawBeforeActive 表示 RAW 数据在 WorkConn 进入 ACTIVE 前出现。
	ErrRawBeforeActive = errors.New("protocol state: raw data before active")
	// ErrInvalidIdentifier 表示消息中的 Protocol v1 标识符格式错误。
	ErrInvalidIdentifier = errors.New("protocol state: invalid identifier")
	// ErrInvalidWorkReady 表示 WorkReady 的状态与错误码组合不合法。
	ErrInvalidWorkReady = errors.New("protocol state: invalid work ready")
)

// Endpoint 表示当前状态机所属的一端。
type Endpoint uint8

const (
	// EndpointAgent 表示运行于 Agent 一侧的状态机。
	EndpointAgent Endpoint = iota + 1
	// EndpointServer 表示运行于 Server 一侧的状态机。
	EndpointServer
)

// ControlPhase 是 Control Session 的线协议阶段。
type ControlPhase uint8

const (
	// ControlAuth 只允许裸 AgentAuthRequest 与 AgentAuthResult。
	ControlAuth ControlPhase = iota + 1
	// ControlEstablished 只允许方向矩阵定义的 ControlEnvelope。
	ControlEstablished
	// ControlDraining 禁止下发新的 Snapshot 与 WorkDemand。
	ControlDraining
	// ControlClosed 拒绝后续全部消息。
	ControlClosed
)

// WorkPhase 是 WorkConn 的结构化阶段及 RAW 阶段。
type WorkPhase uint8

const (
	// WorkAuthenticating 只允许 WorkHello 与 WorkReady。
	WorkAuthenticating WorkPhase = iota + 1
	// WorkIdle 只等待 Server 下发 OpenRequest。
	WorkIdle
	// WorkOpening 只等待 Agent 返回 OpenResponse。
	WorkOpening
	// WorkActive 只允许双向 RAW 字节，不再解释结构化 Frame。
	WorkActive
	// WorkClosed 拒绝后续全部消息和 RAW 字节。
	WorkClosed
)

// Result 描述一次消息接收或发送在状态机中的幂等结果。
type Result struct {
	// Duplicate 为 true 时表示消息已按协议幂等处理，调用方不应重复执行业务副作用。
	Duplicate bool
}

// Control 是一个 Control Session 的纯内存协议状态。
// 它不是并发安全对象；所属 SessionOwner 必须以单一事件序列调用它。
type Control struct {
	endpoint        Endpoint
	phase           ControlPhase
	protocolVersion uint32
	drains          map[drainKey]drainRecord
	configAck       *configAckRecord
}

type drainKind uint8

const (
	drainRequest drainKind = iota + 1
	drainAck
)

type drainRecord struct {
	kind            drainKind
	drainTimeoutMS  uint32
	remainingActive uint32
}

// drainKey 将 Request 与其对应 Ack 分开记录；它们共享 drain_id 但载荷不同。
type drainKey struct {
	id   string
	kind drainKind
}

type configAckRecord struct {
	revision uint64
	status   protocolv1.ConfigApplyStatus
}

// NewControl 建立处于 AUTH 的 Control Session 状态。
// protocolVersion 必须是 Auth 成功时协商出的非零版本。
func NewControl(endpoint Endpoint, protocolVersion uint32) (*Control, error) {
	if !validEndpoint(endpoint) || protocolVersion == 0 {
		return nil, fmt.Errorf("%w: invalid endpoint or negotiated version", ErrIllegalState)
	}
	return &Control{
		endpoint:        endpoint,
		phase:           ControlAuth,
		protocolVersion: protocolVersion,
		drains:          make(map[drainKey]drainRecord),
	}, nil
}

// Phase 返回当前 Control Session 阶段。
func (control *Control) Phase() ControlPhase {
	return control.phase
}

// AcceptInbound 校验并记录由对端发送的消息。
func (control *Control) AcceptInbound(message proto.Message) (Result, error) {
	return control.accept(otherEndpoint(control.endpoint), message)
}

// AcceptOutbound 校验并记录当前端准备发送的消息。
// 调用方只能在完整 Frame 写入或可确认排队成功后调用，避免状态先于交付提交。
func (control *Control) AcceptOutbound(message proto.Message) (Result, error) {
	return control.accept(control.endpoint, message)
}

// CommitAuthSuccessAfterFlush 是 Server 在完整 success Frame flush 成功后的提交点。
func (control *Control) CommitAuthSuccessAfterFlush(result *protocolv1.AgentAuthResult) error {
	if control.endpoint != EndpointServer {
		return ErrIllegalDirection
	}
	return control.commitAuthSuccess(result)
}

// CommitAuthSuccessAfterDecode 是 Agent 完整解码并验证 success Frame 后的提交点。
func (control *Control) CommitAuthSuccessAfterDecode(result *protocolv1.AgentAuthResult) error {
	if control.endpoint != EndpointAgent {
		return ErrIllegalDirection
	}
	return control.commitAuthSuccess(result)
}

// CommitAuthFailureAfterFlush 将 Server 的认证失败结果提交为 CLOSED。
func (control *Control) CommitAuthFailureAfterFlush(result *protocolv1.AgentAuthResult) error {
	if control.endpoint != EndpointServer {
		return ErrIllegalDirection
	}
	return control.commitAuthFailure(result)
}

// CommitAuthFailureAfterDecode 将 Agent 收到的认证失败结果提交为 CLOSED。
func (control *Control) CommitAuthFailureAfterDecode(result *protocolv1.AgentAuthResult) error {
	if control.endpoint != EndpointAgent {
		return ErrIllegalDirection
	}
	return control.commitAuthFailure(result)
}

// Close 将连接置为 CLOSED；关闭后不会再接受协议消息。
func (control *Control) Close() {
	control.phase = ControlClosed
}

func (control *Control) accept(sender Endpoint, message proto.Message) (Result, error) {
	if control.phase == ControlClosed {
		return Result{}, ErrIllegalState
	}

	switch typed := message.(type) {
	case *protocolv1.AgentAuthRequest:
		if typed == nil {
			return Result{}, ErrUnexpectedMessage
		}
		return control.acceptAuthRequest(sender)
	case *protocolv1.AgentAuthResult:
		if typed == nil || !validAuthResult(typed) {
			return Result{}, ErrInvalidAuthResult
		}
		return control.acceptAuthResult(sender)
	case *protocolv1.ControlEnvelope:
		if typed == nil {
			return Result{}, ErrInvalidControlEnvelope
		}
		return control.acceptEnvelope(sender, typed)
	default:
		return Result{}, ErrUnexpectedMessage
	}
}

func (control *Control) acceptAuthRequest(sender Endpoint) (Result, error) {
	if sender != EndpointAgent {
		return Result{}, ErrIllegalDirection
	}
	if control.phase != ControlAuth {
		return Result{}, ErrIllegalState
	}
	return Result{}, nil
}

func (control *Control) acceptAuthResult(sender Endpoint) (Result, error) {
	if sender != EndpointServer {
		return Result{}, ErrIllegalDirection
	}
	if control.phase != ControlAuth {
		return Result{}, ErrIllegalState
	}
	return Result{}, nil
}

func (control *Control) acceptEnvelope(sender Endpoint, envelope *protocolv1.ControlEnvelope) (Result, error) {
	if control.phase == ControlAuth {
		return Result{}, ErrIllegalState
	}
	if envelope.GetProtocolVersion() != control.protocolVersion {
		return Result{}, ErrProtocolVersion
	}

	switch payload := envelope.GetPayload().(type) {
	case *protocolv1.ControlEnvelope_Heartbeat:
		return control.acceptEstablishedMessage(sender, EndpointAgent, control.phase != ControlClosed)
	case *protocolv1.ControlEnvelope_ConfigSnapshot:
		return control.acceptEstablishedMessage(sender, EndpointServer, control.phase == ControlEstablished)
	case *protocolv1.ControlEnvelope_ConfigAck:
		_, err := control.acceptEstablishedMessage(sender, EndpointAgent, control.phase != ControlClosed)
		if err != nil || payload.ConfigAck == nil {
			if err != nil {
				return Result{}, err
			}
			return Result{}, ErrInvalidConfigAck
		}
		return control.acceptConfigAck(payload.ConfigAck)
	case *protocolv1.ControlEnvelope_WorkDemand:
		return control.acceptEstablishedMessage(sender, EndpointServer, control.phase == ControlEstablished)
	case *protocolv1.ControlEnvelope_TunnelHealthBatch:
		return control.acceptEstablishedMessage(sender, EndpointAgent, control.phase != ControlClosed)
	case *protocolv1.ControlEnvelope_DrainRequest:
		result, err := control.acceptEstablishedMessage(sender, EndpointAgent, control.phase == ControlEstablished || control.phase == ControlDraining)
		if err != nil || payload.DrainRequest == nil {
			if err != nil {
				return Result{}, err
			}
			return Result{}, ErrConflictingDrain
		}
		_ = result
		return control.acceptDrainRequest(payload.DrainRequest)
	case *protocolv1.ControlEnvelope_Error:
		return control.acceptEstablishedMessage(sender, sender, control.phase != ControlClosed)
	case *protocolv1.ControlEnvelope_DrainAck:
		result, err := control.acceptEstablishedMessage(sender, EndpointServer, control.phase == ControlEstablished || control.phase == ControlDraining)
		if err != nil || payload.DrainAck == nil {
			if err != nil {
				return Result{}, err
			}
			return Result{}, ErrConflictingDrain
		}
		_ = result
		return control.acceptDrainAck(payload.DrainAck)
	default:
		return Result{}, ErrInvalidControlEnvelope
	}
}

func (control *Control) acceptEstablishedMessage(sender, expected Endpoint, allowed bool) (Result, error) {
	if sender != expected {
		return Result{}, ErrIllegalDirection
	}
	if !allowed {
		return Result{}, ErrIllegalState
	}
	return Result{}, nil
}

func (control *Control) acceptDrainRequest(request *protocolv1.DrainRequest) (Result, error) {
	if result, found, err := control.acceptDrain(request.GetDrainId(), drainRecord{
		kind:           drainRequest,
		drainTimeoutMS: request.GetDrainTimeoutMs(),
	}); found || err != nil {
		return result, err
	}
	// Server 收到首次 Request 后立即进入 DRAINING 并停止新的 Acquire。
	if control.endpoint == EndpointServer {
		control.phase = ControlDraining
	}
	return Result{}, nil
}

func (control *Control) acceptDrainAck(ack *protocolv1.DrainAck) (Result, error) {
	result, found, err := control.acceptDrain(ack.GetDrainId(), drainRecord{
		kind:            drainAck,
		remainingActive: ack.GetRemainingActive(),
	})
	if found || err != nil {
		return result, err
	}
	// Agent 收到首次 Ack 后停止接受新的 OPEN，因此切入 DRAINING。
	if control.endpoint == EndpointAgent {
		control.phase = ControlDraining
	}
	return Result{}, nil
}

func (control *Control) acceptDrain(id string, next drainRecord) (Result, bool, error) {
	if err := validate.ValidateID(id, "drain_"); err != nil {
		return Result{}, false, fmt.Errorf("%w: %w", ErrInvalidIdentifier, err)
	}
	key := drainKey{id: id, kind: next.kind}
	if previous, found := control.drains[key]; found {
		if previous != next {
			return Result{}, true, ErrConflictingDrain
		}
		return Result{Duplicate: true}, true, nil
	}
	control.drains[key] = next
	return Result{}, false, nil
}

func (control *Control) acceptConfigAck(ack *protocolv1.ConfigAck) (Result, error) {
	if ack.GetApplyStatus() == protocolv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_UNSPECIFIED {
		return Result{}, ErrInvalidConfigAck
	}
	next := configAckRecord{revision: ack.GetObservedRevision(), status: ack.GetApplyStatus()}
	if control.configAck == nil {
		control.configAck = &next
		return Result{}, nil
	}
	if next.revision < control.configAck.revision {
		return Result{}, ErrRevisionRollback
	}
	if next.revision == control.configAck.revision {
		if next.status != control.configAck.status {
			return Result{}, ErrConflictingConfigAck
		}
		return Result{Duplicate: true}, nil
	}
	control.configAck = &next
	return Result{}, nil
}

func (control *Control) commitAuthSuccess(result *protocolv1.AgentAuthResult) error {
	if control.phase != ControlAuth {
		return ErrIllegalState
	}
	if result == nil || result.GetSuccess() == nil || result.GetFailure() != nil {
		return ErrInvalidAuthResult
	}
	if len(result.GetSuccess().GetSessionSecret()) != 32 {
		return ErrInvalidSessionSecret
	}
	control.phase = ControlEstablished
	return nil
}

func (control *Control) commitAuthFailure(result *protocolv1.AgentAuthResult) error {
	if control.phase != ControlAuth {
		return ErrIllegalState
	}
	if result == nil || result.GetFailure() == nil || result.GetSuccess() != nil {
		return ErrInvalidAuthResult
	}
	control.phase = ControlClosed
	return nil
}

func validAuthResult(result *protocolv1.AgentAuthResult) bool {
	return (result.GetSuccess() == nil) != (result.GetFailure() == nil)
}

// Work 是一个 WorkConn 的纯内存协议状态。
// 它不是并发安全对象；WorkConn 所属读写协调器必须串行调用它。
type Work struct {
	endpoint     Endpoint
	phase        WorkPhase
	helloSeen    bool
	workID       string
	connectionID string
}

// NewWork 建立处于 AUTHENTICATING 的 WorkConn 状态。
func NewWork(endpoint Endpoint) (*Work, error) {
	if !validEndpoint(endpoint) {
		return nil, fmt.Errorf("%w: invalid endpoint", ErrIllegalState)
	}
	return &Work{endpoint: endpoint, phase: WorkAuthenticating}, nil
}

// Phase 返回当前 WorkConn 阶段。
func (work *Work) Phase() WorkPhase {
	return work.phase
}

// AcceptInbound 校验并记录由 WorkConn 对端发送的裸结构化消息。
func (work *Work) AcceptInbound(message proto.Message) error {
	return work.accept(otherEndpoint(work.endpoint), message)
}

// AcceptOutbound 校验并记录当前端准备发送的裸结构化消息。
func (work *Work) AcceptOutbound(message proto.Message) error {
	return work.accept(work.endpoint, message)
}

// AcceptRaw 只在双方已经进入 ACTIVE 后允许转交原始字节。
func (work *Work) AcceptRaw() error {
	if work.phase != WorkActive {
		work.phase = WorkClosed
		return ErrRawBeforeActive
	}
	return nil
}

// Close 将 WorkConn 置为 CLOSED；ACTIVE 不会回退为 IDLE。
func (work *Work) Close() {
	work.phase = WorkClosed
}

func (work *Work) accept(sender Endpoint, message proto.Message) error {
	if work.phase == WorkClosed {
		return ErrIllegalState
	}

	var err error
	switch typed := message.(type) {
	case *protocolv1.WorkHello:
		if typed == nil {
			err = ErrUnexpectedMessage
			break
		}
		err = work.acceptHello(sender, typed)
	case *protocolv1.WorkReady:
		if typed == nil {
			err = ErrUnexpectedMessage
			break
		}
		err = work.acceptReady(sender, typed)
	case *protocolv1.OpenRequest:
		if typed == nil {
			err = ErrUnexpectedMessage
			break
		}
		err = work.acceptOpenRequest(sender, typed)
	case *protocolv1.OpenResponse:
		if typed == nil {
			err = ErrUnexpectedMessage
			break
		}
		err = work.acceptOpenResponse(sender, typed)
	default:
		err = ErrUnexpectedMessage
	}
	if err != nil {
		work.phase = WorkClosed
	}
	return err
}

func (work *Work) acceptHello(sender Endpoint, hello *protocolv1.WorkHello) error {
	if sender != EndpointAgent {
		return ErrIllegalDirection
	}
	if work.phase != WorkAuthenticating {
		return ErrIllegalState
	}
	if work.helloSeen {
		return ErrRepeatedMessage
	}
	for _, field := range []struct {
		value  string
		prefix string
	}{
		{value: hello.GetAgentId(), prefix: "ag_"},
		{value: hello.GetInstanceId(), prefix: "ai_"},
		{value: hello.GetSessionId(), prefix: "sess_"},
		{value: hello.GetWorkId(), prefix: "work_"},
		{value: hello.GetBudgetLeaseId(), prefix: "lease_"},
	} {
		if err := validate.ValidateID(field.value, field.prefix); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidIdentifier, err)
		}
	}
	work.helloSeen = true
	work.workID = hello.GetWorkId()
	return nil
}

func (work *Work) acceptReady(sender Endpoint, ready *protocolv1.WorkReady) error {
	if sender != EndpointServer {
		return ErrIllegalDirection
	}
	if work.phase != WorkAuthenticating || !work.helloSeen {
		return ErrIllegalState
	}
	if err := validate.ValidateID(ready.GetWorkId(), "work_"); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidIdentifier, err)
	}
	if ready.GetWorkId() != work.workID {
		return ErrWorkIDMismatch
	}
	switch ready.GetStatus() {
	case protocolv1.WorkReadyStatus_WORK_READY_STATUS_READY:
		if ready.GetErrorCode() != protocolv1.ErrorCode_ERROR_CODE_OK {
			return ErrInvalidWorkReady
		}
		work.phase = WorkIdle
		return nil
	case protocolv1.WorkReadyStatus_WORK_READY_STATUS_REJECTED:
		if ready.GetErrorCode() == protocolv1.ErrorCode_ERROR_CODE_OK || !validErrorCode(ready.GetErrorCode()) {
			return ErrInvalidWorkReady
		}
		work.phase = WorkClosed
		return nil
	default:
		return ErrUnexpectedMessage
	}
}

func (work *Work) acceptOpenRequest(sender Endpoint, request *protocolv1.OpenRequest) error {
	if sender != EndpointServer {
		return ErrIllegalDirection
	}
	if work.phase != WorkIdle {
		return ErrIllegalState
	}
	if err := validate.ValidateID(request.GetConnectionId(), "conn_"); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidIdentifier, err)
	}
	work.connectionID = request.GetConnectionId()
	work.phase = WorkOpening
	return nil
}

func (work *Work) acceptOpenResponse(sender Endpoint, response *protocolv1.OpenResponse) error {
	if sender != EndpointAgent {
		return ErrIllegalDirection
	}
	if work.phase != WorkOpening {
		return ErrIllegalState
	}
	if err := validate.ValidateID(response.GetConnectionId(), "conn_"); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidIdentifier, err)
	}
	if response.GetConnectionId() != work.connectionID {
		return ErrConnectionIDMismatch
	}
	switch response.GetStatus() {
	case protocolv1.OpenStatus_OPEN_STATUS_OK:
		work.phase = WorkActive
		return nil
	case protocolv1.OpenStatus_OPEN_STATUS_ERROR:
		work.phase = WorkClosed
		return nil
	default:
		return ErrUnexpectedMessage
	}
}

func validEndpoint(endpoint Endpoint) bool {
	return endpoint == EndpointAgent || endpoint == EndpointServer
}

func otherEndpoint(endpoint Endpoint) Endpoint {
	if endpoint == EndpointAgent {
		return EndpointServer
	}
	return EndpointAgent
}

func validErrorCode(code protocolv1.ErrorCode) bool {
	_, ok := protocolv1.ErrorCode_name[int32(code)]
	return ok
}
