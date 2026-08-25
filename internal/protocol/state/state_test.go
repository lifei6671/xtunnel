package state

import (
	"errors"
	"testing"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"google.golang.org/protobuf/proto"
)

const negotiatedVersion = 1

const (
	testAgentID       = "ag_01J00000000000000000000000"
	testInstanceID    = "ai_01J00000000000000000000000"
	testSessionID     = "sess_01J00000000000000000000000"
	testWorkID        = "work_01J00000000000000000000000"
	otherWorkID       = "work_01J00000000000000000000001"
	testLeaseID       = "lease_01J00000000000000000000000"
	testConnectionID  = "conn_01J00000000000000000000000"
	otherConnectionID = "conn_01J00000000000000000000001"
	testDrainID       = "drain_01J00000000000000000000000"
)

// TestControlAuthBareFrameAndCommitBoundary 锁定 AUTH 只能交换裸认证消息，
// 且双方分别只能在 Server flush 或 Agent 完整解码成功后进入 ESTABLISHED。
func TestControlAuthBareFrameAndCommitBoundary(t *testing.T) {
	server := newControl(t, EndpointServer)
	agent := newControl(t, EndpointAgent)

	if _, err := server.AcceptInbound(&protocolv1.AgentAuthRequest{}); err != nil {
		t.Fatalf("Server AcceptInbound(AgentAuthRequest) error = %v", err)
	}
	if _, err := agent.AcceptOutbound(&protocolv1.AgentAuthRequest{}); err != nil {
		t.Fatalf("Agent AcceptOutbound(AgentAuthRequest) error = %v", err)
	}

	success := authSuccess()
	if _, err := server.AcceptOutbound(success); err != nil {
		t.Fatalf("Server AcceptOutbound(AgentAuthResult) error = %v", err)
	}
	if _, err := agent.AcceptInbound(success); err != nil {
		t.Fatalf("Agent AcceptInbound(AgentAuthResult) error = %v", err)
	}
	if got := server.Phase(); got != ControlAuth {
		t.Fatalf("Server Phase() before flush = %v, want AUTH", got)
	}
	if got := agent.Phase(); got != ControlAuth {
		t.Fatalf("Agent Phase() before decode = %v, want AUTH", got)
	}

	if err := server.CommitAuthSuccessAfterFlush(success); err != nil {
		t.Fatalf("CommitAuthSuccessAfterFlush() error = %v", err)
	}
	if err := agent.CommitAuthSuccessAfterDecode(success); err != nil {
		t.Fatalf("CommitAuthSuccessAfterDecode() error = %v", err)
	}
	if got := server.Phase(); got != ControlEstablished {
		t.Fatalf("Server Phase() = %v, want ESTABLISHED", got)
	}
	if got := agent.Phase(); got != ControlEstablished {
		t.Fatalf("Agent Phase() = %v, want ESTABLISHED", got)
	}

	unauthorized := newControl(t, EndpointServer)
	if _, err := unauthorized.AcceptInbound(controlEnvelope(&protocolv1.ControlEnvelope_Heartbeat{})); !errors.Is(err, ErrIllegalState) {
		t.Fatalf("AUTH AcceptInbound(ControlEnvelope) error = %v, want ErrIllegalState", err)
	}
	if err := unauthorized.CommitAuthSuccessAfterFlush(&protocolv1.AgentAuthResult{}); !errors.Is(err, ErrInvalidAuthResult) {
		t.Fatalf("CommitAuthSuccessAfterFlush(empty) error = %v, want ErrInvalidAuthResult", err)
	}
	if err := unauthorized.CommitAuthSuccessAfterFlush(&protocolv1.AgentAuthResult{
		Result: &protocolv1.AgentAuthResult_Success{Success: &protocolv1.AgentAuthSuccess{}},
	}); !errors.Is(err, ErrInvalidSessionSecret) {
		t.Fatalf("CommitAuthSuccessAfterFlush(short secret) error = %v, want ErrInvalidSessionSecret", err)
	}
}

// TestControlDirectionAndDrainingMatrix 用表驱动方式覆盖冻结的方向与状态矩阵。
func TestControlDirectionAndDrainingMatrix(t *testing.T) {
	cases := []struct {
		name     string
		endpoint Endpoint
		inbound  bool
		message  proto.Message
		wantErr  error
	}{
		{name: "Agent 接收 Heartbeat", endpoint: EndpointAgent, inbound: true, message: controlEnvelope(&protocolv1.ControlEnvelope_Heartbeat{}), wantErr: ErrIllegalDirection},
		{name: "Server 接收 Heartbeat", endpoint: EndpointServer, inbound: true, message: controlEnvelope(&protocolv1.ControlEnvelope_Heartbeat{})},
		{name: "Agent 接收 Snapshot", endpoint: EndpointAgent, inbound: true, message: controlEnvelope(&protocolv1.ControlEnvelope_ConfigSnapshot{})},
		{name: "Server 接收 Snapshot", endpoint: EndpointServer, inbound: true, message: controlEnvelope(&protocolv1.ControlEnvelope_ConfigSnapshot{}), wantErr: ErrIllegalDirection},
		{name: "Agent 接收 WorkDemand", endpoint: EndpointAgent, inbound: true, message: controlEnvelope(&protocolv1.ControlEnvelope_WorkDemand{})},
		{name: "Server 接收 WorkDemand", endpoint: EndpointServer, inbound: true, message: controlEnvelope(&protocolv1.ControlEnvelope_WorkDemand{}), wantErr: ErrIllegalDirection},
		{name: "双方接收 Error", endpoint: EndpointAgent, inbound: true, message: controlEnvelope(&protocolv1.ControlEnvelope_Error{})},
		{name: "Server 接收 Error", endpoint: EndpointServer, inbound: true, message: controlEnvelope(&protocolv1.ControlEnvelope_Error{})},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			control := establishedControl(t, testCase.endpoint)
			var err error
			if testCase.inbound {
				_, err = control.AcceptInbound(testCase.message)
			} else {
				_, err = control.AcceptOutbound(testCase.message)
			}
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("accept() error = %v, want %v", err, testCase.wantErr)
			}
		})
	}

	server := establishedControl(t, EndpointServer)
	if _, err := server.AcceptInbound(controlEnvelope(&protocolv1.ControlEnvelope_DrainRequest{
		DrainRequest: &protocolv1.DrainRequest{DrainId: testDrainID, DrainTimeoutMs: 30_000},
	})); err != nil {
		t.Fatalf("Server AcceptInbound(DrainRequest) error = %v", err)
	}
	if got := server.Phase(); got != ControlDraining {
		t.Fatalf("Server Phase() after DrainRequest = %v, want DRAINING", got)
	}
	if _, err := server.AcceptOutbound(controlEnvelope(&protocolv1.ControlEnvelope_ConfigSnapshot{})); !errors.Is(err, ErrIllegalState) {
		t.Fatalf("DRAINING AcceptOutbound(ConfigSnapshot) error = %v, want ErrIllegalState", err)
	}
	if _, err := server.AcceptOutbound(controlEnvelope(&protocolv1.ControlEnvelope_WorkDemand{})); !errors.Is(err, ErrIllegalState) {
		t.Fatalf("DRAINING AcceptOutbound(WorkDemand) error = %v, want ErrIllegalState", err)
	}
	if _, err := server.AcceptOutbound(controlEnvelope(&protocolv1.ControlEnvelope_DrainAck{
		DrainAck: &protocolv1.DrainAck{DrainId: testDrainID},
	})); err != nil {
		t.Fatalf("DRAINING AcceptOutbound(DrainAck) error = %v", err)
	}
}

// TestControlDrainAndConfigAckIdempotency 保证重复消息没有第二次业务副作用，
// 同一键的冲突内容和倒退 revision 必须被拒绝。
func TestControlDrainAndConfigAckIdempotency(t *testing.T) {
	server := establishedControl(t, EndpointServer)
	request := controlEnvelope(&protocolv1.ControlEnvelope_DrainRequest{
		DrainRequest: &protocolv1.DrainRequest{DrainId: testDrainID, DrainTimeoutMs: 30_000},
	})
	if result, err := server.AcceptInbound(request); err != nil || result.Duplicate {
		t.Fatalf("first DrainRequest = (%+v, %v), want non-duplicate success", result, err)
	}
	if result, err := server.AcceptInbound(request); err != nil || !result.Duplicate {
		t.Fatalf("duplicate DrainRequest = (%+v, %v), want duplicate success", result, err)
	}
	conflictRequest := controlEnvelope(&protocolv1.ControlEnvelope_DrainRequest{
		DrainRequest: &protocolv1.DrainRequest{DrainId: testDrainID, DrainTimeoutMs: 15_000},
	})
	if _, err := server.AcceptInbound(conflictRequest); !errors.Is(err, ErrConflictingDrain) {
		t.Fatalf("conflicting DrainRequest error = %v, want ErrConflictingDrain", err)
	}

	acks := establishedControl(t, EndpointServer)
	ack := configAck(7, protocolv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_APPLIED)
	if result, err := acks.AcceptInbound(ack); err != nil || result.Duplicate {
		t.Fatalf("first ConfigAck = (%+v, %v), want non-duplicate success", result, err)
	}
	if result, err := acks.AcceptInbound(ack); err != nil || !result.Duplicate {
		t.Fatalf("duplicate ConfigAck = (%+v, %v), want duplicate success", result, err)
	}
	if _, err := acks.AcceptInbound(configAck(7, protocolv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_REJECTED)); !errors.Is(err, ErrConflictingConfigAck) {
		t.Fatalf("conflicting ConfigAck error = %v, want ErrConflictingConfigAck", err)
	}
	if _, err := acks.AcceptInbound(configAck(6, protocolv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_APPLIED)); !errors.Is(err, ErrRevisionRollback) {
		t.Fatalf("rollback ConfigAck error = %v, want ErrRevisionRollback", err)
	}
	if _, err := acks.AcceptInbound(configAck(8, protocolv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_APPLIED)); err != nil {
		t.Fatalf("higher ConfigAck error = %v", err)
	}
}

func TestControlRejectsMalformedDrainIDBeforeStateLookup(t *testing.T) {
	control := establishedControl(t, EndpointServer)
	_, err := control.AcceptInbound(controlEnvelope(&protocolv1.ControlEnvelope_DrainRequest{
		DrainRequest: &protocolv1.DrainRequest{DrainId: "drain-invalid", DrainTimeoutMs: 30_000},
	}))
	if !errors.Is(err, ErrInvalidIdentifier) {
		t.Fatalf("AcceptInbound(DrainRequest) error = %v, want ErrInvalidIdentifier", err)
	}
	if got := control.Phase(); got != ControlEstablished {
		t.Fatalf("Control Phase() = %v, want ESTABLISHED", got)
	}
}

// TestWorkStateMachineAndRawHandoff 锁定 WorkConn 的唯一消息顺序和 RAW 交接点。
func TestWorkStateMachineAndRawHandoff(t *testing.T) {
	server := newWork(t, EndpointServer)
	agent := newWork(t, EndpointAgent)

	hello := workHello()
	mustAcceptWork(t, server.AcceptInbound(hello))
	mustAcceptWork(t, agent.AcceptOutbound(hello))
	ready := &protocolv1.WorkReady{WorkId: testWorkID, Status: protocolv1.WorkReadyStatus_WORK_READY_STATUS_READY}
	mustAcceptWork(t, server.AcceptOutbound(ready))
	mustAcceptWork(t, agent.AcceptInbound(ready))
	if got := server.Phase(); got != WorkIdle {
		t.Fatalf("Server Phase() = %v, want IDLE", got)
	}

	request := &protocolv1.OpenRequest{ConnectionId: testConnectionID, TunnelId: "tun_test"}
	mustAcceptWork(t, server.AcceptOutbound(request))
	mustAcceptWork(t, agent.AcceptInbound(request))
	response := &protocolv1.OpenResponse{ConnectionId: testConnectionID, Status: protocolv1.OpenStatus_OPEN_STATUS_OK}
	mustAcceptWork(t, agent.AcceptOutbound(response))
	mustAcceptWork(t, server.AcceptInbound(response))
	mustAcceptWork(t, agent.AcceptRaw())
	mustAcceptWork(t, server.AcceptRaw())
}

// TestWorkProtocolViolationsCloseConnection 锁定违规 WorkConn 必须立即关闭。
func TestWorkProtocolViolationsCloseConnection(t *testing.T) {
	cases := []struct {
		name    string
		accept  func(*testing.T, *Work) error
		wantErr error
	}{
		{
			name: "错误方向 WorkHello",
			accept: func(_ *testing.T, work *Work) error {
				return work.AcceptOutbound(workHello())
			},
			wantErr: ErrIllegalDirection,
		},
		{
			name: "非法 WorkHello ID",
			accept: func(_ *testing.T, work *Work) error {
				hello := workHello()
				hello.WorkId = "work-invalid"
				return work.AcceptInbound(hello)
			},
			wantErr: ErrInvalidIdentifier,
		},
		{
			name: "重复 WorkHello",
			accept: func(t *testing.T, work *Work) error {
				mustAcceptWork(t, work.AcceptInbound(workHello()))
				return work.AcceptInbound(workHello())
			},
			wantErr: ErrRepeatedMessage,
		},
		{
			name: "ACTIVE 前 RAW",
			accept: func(_ *testing.T, work *Work) error {
				return work.AcceptRaw()
			},
			wantErr: ErrRawBeforeActive,
		},
		{
			name: "WorkReady ID 不匹配",
			accept: func(t *testing.T, work *Work) error {
				mustAcceptWork(t, work.AcceptInbound(workHello()))
				return work.AcceptOutbound(&protocolv1.WorkReady{WorkId: otherWorkID, Status: protocolv1.WorkReadyStatus_WORK_READY_STATUS_READY})
			},
			wantErr: ErrWorkIDMismatch,
		},
		{
			name: "OpenResponse ID 不匹配",
			accept: func(t *testing.T, work *Work) error {
				mustAcceptWork(t, work.AcceptInbound(workHello()))
				mustAcceptWork(t, work.AcceptOutbound(&protocolv1.WorkReady{WorkId: testWorkID, Status: protocolv1.WorkReadyStatus_WORK_READY_STATUS_READY}))
				mustAcceptWork(t, work.AcceptOutbound(&protocolv1.OpenRequest{ConnectionId: testConnectionID, TunnelId: "tun_test"}))
				return work.AcceptInbound(&protocolv1.OpenResponse{ConnectionId: otherConnectionID, Status: protocolv1.OpenStatus_OPEN_STATUS_OK})
			},
			wantErr: ErrConnectionIDMismatch,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			work := newWork(t, EndpointServer)
			if err := testCase.accept(t, work); !errors.Is(err, testCase.wantErr) {
				t.Fatalf("违规 WorkConn error = %v, want %v", err, testCase.wantErr)
			}
			if got := work.Phase(); got != WorkClosed {
				t.Fatalf("违规 WorkConn Phase() = %v, want CLOSED", got)
			}
		})
	}
}

// TestWorkReadyStatusErrorCodeCombinations 锁定 WorkReady 状态与错误码组合。
func TestWorkReadyStatusErrorCodeCombinations(t *testing.T) {
	cases := []struct {
		name      string
		ready     *protocolv1.WorkReady
		wantPhase WorkPhase
		wantError bool
	}{
		{
			name:      "READY 仅接受 OK",
			ready:     &protocolv1.WorkReady{WorkId: testWorkID, Status: protocolv1.WorkReadyStatus_WORK_READY_STATUS_READY, ErrorCode: protocolv1.ErrorCode_ERROR_CODE_OK},
			wantPhase: WorkIdle,
		},
		{
			name:      "READY 携带失败码关闭连接",
			ready:     &protocolv1.WorkReady{WorkId: testWorkID, Status: protocolv1.WorkReadyStatus_WORK_READY_STATUS_READY, ErrorCode: protocolv1.ErrorCode_ERROR_CODE_AGENT_BUSY},
			wantPhase: WorkClosed,
			wantError: true,
		},
		{
			name:      "REJECTED 必须携带失败码",
			ready:     &protocolv1.WorkReady{WorkId: testWorkID, Status: protocolv1.WorkReadyStatus_WORK_READY_STATUS_REJECTED, ErrorCode: protocolv1.ErrorCode_ERROR_CODE_OK},
			wantPhase: WorkClosed,
			wantError: true,
		},
		{
			name:      "REJECTED 拒绝未声明错误码",
			ready:     &protocolv1.WorkReady{WorkId: testWorkID, Status: protocolv1.WorkReadyStatus_WORK_READY_STATUS_REJECTED, ErrorCode: protocolv1.ErrorCode(9_999)},
			wantPhase: WorkClosed,
			wantError: true,
		},
		{
			name:      "REJECTED 携带失败码后关闭连接",
			ready:     &protocolv1.WorkReady{WorkId: testWorkID, Status: protocolv1.WorkReadyStatus_WORK_READY_STATUS_REJECTED, ErrorCode: protocolv1.ErrorCode_ERROR_CODE_AGENT_BUSY},
			wantPhase: WorkClosed,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			work := newWork(t, EndpointServer)
			mustAcceptWork(t, work.AcceptInbound(workHello()))
			err := work.AcceptOutbound(testCase.ready)
			if (err != nil) != testCase.wantError {
				t.Fatalf("AcceptOutbound(WorkReady) error = %v, want error=%t", err, testCase.wantError)
			}
			if got := work.Phase(); got != testCase.wantPhase {
				t.Fatalf("WorkReady Phase() = %v, want %v", got, testCase.wantPhase)
			}
		})
	}
}

// TestWorkRejectPaths 验证认证或打开失败只能关闭当前 WorkConn，不能回到 IDLE。
func TestWorkRejectPaths(t *testing.T) {
	work := newWork(t, EndpointServer)
	mustAcceptWork(t, work.AcceptInbound(workHello()))
	mustAcceptWork(t, work.AcceptOutbound(&protocolv1.WorkReady{
		WorkId:    testWorkID,
		Status:    protocolv1.WorkReadyStatus_WORK_READY_STATUS_REJECTED,
		ErrorCode: protocolv1.ErrorCode_ERROR_CODE_AGENT_BUSY,
	}))
	if got := work.Phase(); got != WorkClosed {
		t.Fatalf("rejected WorkReady Phase() = %v, want CLOSED", got)
	}

	opening := newWork(t, EndpointServer)
	mustAcceptWork(t, opening.AcceptInbound(workHello()))
	mustAcceptWork(t, opening.AcceptOutbound(&protocolv1.WorkReady{WorkId: testWorkID, Status: protocolv1.WorkReadyStatus_WORK_READY_STATUS_READY}))
	mustAcceptWork(t, opening.AcceptOutbound(&protocolv1.OpenRequest{ConnectionId: testConnectionID, TunnelId: "tun_test"}))
	mustAcceptWork(t, opening.AcceptInbound(&protocolv1.OpenResponse{
		ConnectionId: testConnectionID,
		Status:       protocolv1.OpenStatus_OPEN_STATUS_ERROR,
		ErrorCode:    protocolv1.ErrorCode_ERROR_CODE_ORIGIN_REFUSED,
	}))
	if got := opening.Phase(); got != WorkClosed {
		t.Fatalf("error OpenResponse Phase() = %v, want CLOSED", got)
	}
}

// workHello 返回状态机测试所需的最小合法 WorkHello。
func workHello() *protocolv1.WorkHello {
	return &protocolv1.WorkHello{
		AgentId:       testAgentID,
		InstanceId:    testInstanceID,
		SessionId:     testSessionID,
		WorkId:        testWorkID,
		BudgetLeaseId: testLeaseID,
	}
}

// newControl 创建测试所需的初始 Control 状态。
func newControl(t *testing.T, endpoint Endpoint) *Control {
	t.Helper()
	control, err := NewControl(endpoint, negotiatedVersion)
	if err != nil {
		t.Fatalf("NewControl() error = %v", err)
	}
	return control
}

// establishedControl 创建已经按各端提交规则进入 ESTABLISHED 的状态。
func establishedControl(t *testing.T, endpoint Endpoint) *Control {
	t.Helper()
	control := newControl(t, endpoint)
	if endpoint == EndpointServer {
		mustControl(t, control.CommitAuthSuccessAfterFlush(authSuccess()))
	} else {
		mustControl(t, control.CommitAuthSuccessAfterDecode(authSuccess()))
	}
	return control
}

// newWork 创建测试所需的初始 WorkConn 状态。
func newWork(t *testing.T, endpoint Endpoint) *Work {
	t.Helper()
	work, err := NewWork(endpoint)
	if err != nil {
		t.Fatalf("NewWork() error = %v", err)
	}
	return work
}

// controlEnvelope 统一构造协商版本正确的 ControlEnvelope。
func controlEnvelope(payload any) *protocolv1.ControlEnvelope {
	envelope := &protocolv1.ControlEnvelope{ProtocolVersion: negotiatedVersion}
	switch value := payload.(type) {
	case *protocolv1.ControlEnvelope_Heartbeat:
		envelope.Payload = value
	case *protocolv1.ControlEnvelope_ConfigSnapshot:
		envelope.Payload = value
	case *protocolv1.ControlEnvelope_ConfigAck:
		envelope.Payload = value
	case *protocolv1.ControlEnvelope_WorkDemand:
		envelope.Payload = value
	case *protocolv1.ControlEnvelope_DrainRequest:
		envelope.Payload = value
	case *protocolv1.ControlEnvelope_Error:
		envelope.Payload = value
	case *protocolv1.ControlEnvelope_DrainAck:
		envelope.Payload = value
	default:
		panic("测试传入了未覆盖的 ControlEnvelope payload")
	}
	return envelope
}

// authSuccess 返回符合 AUTH 成功提交类型的最小消息。
func authSuccess() *protocolv1.AgentAuthResult {
	return &protocolv1.AgentAuthResult{Result: &protocolv1.AgentAuthResult_Success{Success: &protocolv1.AgentAuthSuccess{
		SessionSecret: make([]byte, 32),
	}}}
}

// configAck 返回来自 Agent 的 ConfigAck Envelope。
func configAck(revision uint64, status protocolv1.ConfigApplyStatus) *protocolv1.ControlEnvelope {
	return controlEnvelope(&protocolv1.ControlEnvelope_ConfigAck{
		ConfigAck: &protocolv1.ConfigAck{ObservedRevision: revision, ApplyStatus: status},
	})
}

// mustControl 将 Control 操作失败立即报告为测试失败。
func mustControl(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// mustAcceptWork 将 WorkConn 操作失败立即报告为测试失败。
func mustAcceptWork(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
