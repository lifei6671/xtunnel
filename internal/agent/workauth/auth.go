// Package workauth 实现 Agent 在 Work ALPN TLS 建立后的 Protocol v1 WorkHello 握手。
//
// 本包只负责生成一次性 Work 身份、发送带 HMAC 的 WorkHello 并接收 WorkReady；
// 它不负责拨号、Budget Lease 调度、WorkPool、OPEN 或 RAW 转发。
package workauth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/controlauth"
	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/protocol/deterministic"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/state"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
)

const (
	nonceSize                = 32
	cancellationPollInterval = 50 * time.Millisecond
)

var (
	// ErrInvalidInput 表示已认证 Session、Budget Lease 或超时参数不符合协议边界。
	ErrInvalidInput = errors.New("agent work auth input is invalid")
	// ErrRandomSource 表示 Work ID 或 nonce 的 CSPRNG 读取失败。
	ErrRandomSource = errors.New("agent work auth random source failed")
	// ErrProtocol 表示 Server 返回了无法安全接受的 WorkReady Frame 或语义。
	ErrProtocol = errors.New("agent work auth protocol violation")
	// ErrWriteTimeout 表示 WorkHello 未在写入上限内完整发送。
	ErrWriteTimeout = errors.New("agent work auth write timeout")
	// ErrReadTimeout 表示 WorkReady 未在读取上限内完整接收。
	ErrReadTimeout = errors.New("agent work auth read timeout")
)

// Config 是一次 WorkConn 认证所需的已认证 Control Session 与 Lease 输入。
type Config struct {
	// Session 提供 Tunnel、Connector、Session 身份及当前 32 字节 Session Secret。
	Session controlauth.Session
	// BudgetLeaseID 是 Server 通过当前 Control Session 发放的 lease_<ULID>。
	BudgetLeaseID string
	// WriteTimeout 与 ReadTimeout 分别限制完整 WorkHello 和 WorkReady Frame。
	WriteTimeout time.Duration
	ReadTimeout  time.Duration
}

// Ready 是 WorkReady READY 成功后交给 WorkPool 的 IDLE WorkConn 状态。
type Ready struct {
	// WorkID 是本次 WorkConn 新生成且已经由 Server 原样确认的身份。
	WorkID string
	// State 已完成 WorkReady 接收提交，返回时固定处于 WorkIdle。
	State *state.Work
}

// Rejected 是 Server 对合法 WorkHello 的显式拒绝结果。
type Rejected struct {
	// WorkID 是被拒绝的本次 WorkConn 身份。
	WorkID string
	// Code 是 Server 返回并经 Protocol v1 状态机验证的非 OK 错误码。
	Code protocolv1.ErrorCode
	// State 已处理 REJECTED，返回时固定处于 WorkClosed。
	State *state.Work
}

// Error 返回不包含 Session Secret、nonce 或 MAC 的稳定拒绝描述。
func (rejected *Rejected) Error() string {
	return fmt.Sprintf("agent work auth rejected: work_id=%s code=%s", rejected.WorkID, rejected.Code.String())
}

// Authenticate 在已经完成 Work ALPN TLS 握手的连接上同步完成 WorkHello/WorkReady。
//
// READY 返回处于 IDLE 的 Work 状态；REJECTED 以 *Rejected 返回且其中状态已关闭。
// 网络与协议错误不会被误包装成 Server 的显式拒绝。
func Authenticate(ctx context.Context, connection net.Conn, config Config) (ready *Ready, resultErr error) {
	return authenticate(ctx, connection, config, generateWorkID, rand.Reader)
}

func authenticate(
	ctx context.Context,
	connection net.Conn,
	config Config,
	newWorkID func() (string, error),
	random io.Reader,
) (ready *Ready, resultErr error) {
	if ctx == nil || connection == nil || newWorkID == nil || random == nil {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if config.WriteTimeout <= 0 || config.ReadTimeout <= 0 {
		return nil, ErrInvalidInput
	}
	if err := validateSessionAndLease(config.Session, config.BudgetLeaseID); err != nil {
		return nil, err
	}
	// Config 按值传入，当前函数可安全擦除自己的 Secret 副本而不改写 Control Session。
	defer clear(config.Session.SessionSecret[:])

	workID, err := newWorkID()
	if err != nil {
		return nil, fmt.Errorf("%w: generate work id: %v", ErrRandomSource, err)
	}
	if err := validate.ValidateID(workID, "work_"); err != nil {
		return nil, fmt.Errorf("%w: generated work id is invalid", ErrRandomSource)
	}
	var nonce [nonceSize]byte
	if _, err := io.ReadFull(random, nonce[:]); err != nil {
		clear(nonce[:])
		return nil, fmt.Errorf("%w: generate nonce: %v", ErrRandomSource, err)
	}
	defer clear(nonce[:])

	hello := &protocolv1.WorkHello{
		TunnelId:      config.Session.TunnelID,
		ConnectorId:   config.Session.ConnectorID,
		SessionId:     config.Session.SessionID,
		WorkId:        workID,
		Nonce:         nonce[:],
		BudgetLeaseId: config.BudgetLeaseID,
	}
	mac, err := deterministic.ComputeWorkHelloMAC(config.Session.SessionSecret[:], hello)
	if err != nil {
		return nil, fmt.Errorf("compute WorkHello MAC: %w", err)
	}
	defer clear(mac)
	hello.Mac = mac

	workState, err := state.NewWork(state.EndpointAgent)
	if err != nil {
		return nil, fmt.Errorf("create agent Work protocol state: %w", err)
	}
	defer func() {
		if resultErr != nil && workState.Phase() != state.WorkClosed {
			workState.Close()
		}
	}()
	if err := workState.AcceptOutbound(hello); err != nil {
		return nil, fmt.Errorf("%w: outbound WorkHello state: %v", ErrProtocol, err)
	}

	// 短 Deadline 轮询让无 Deadline 的 Context 也能取消阻塞 IO；返回前必须清理
	// 临时 Deadline，否则后续 IDLE/OPEN/RAW 会继承已经过期的认证边界。
	defer func() {
		if err := connection.SetDeadline(time.Time{}); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("clear agent work auth deadline: %w", err))
			ready = nil
		}
	}()
	writer := &contextWriter{ctx: ctx, conn: connection, deadline: time.Now().Add(config.WriteTimeout)}
	if err := frame.WriteWork(writer, hello); err != nil {
		return nil, classifyIOError("write WorkHello", err, ErrWriteTimeout)
	}
	// Wire 字节已经完整写出，后续状态机只保存 WorkID；此时即可清理消息内的敏感副本。
	clear(hello.Nonce)
	clear(hello.Mac)

	response := &protocolv1.WorkReady{}
	reader := &contextReader{ctx: ctx, conn: connection, deadline: time.Now().Add(config.ReadTimeout)}
	if err := frame.ReadWork(reader, response); err != nil {
		return nil, classifyReadError(err)
	}
	if err := validate.RejectUnknownFields(response); err != nil {
		return nil, fmt.Errorf("%w: WorkReady unknown fields: %v", ErrProtocol, err)
	}
	if err := workState.AcceptInbound(response); err != nil {
		return nil, fmt.Errorf("%w: inbound WorkReady state: %v", ErrProtocol, err)
	}

	switch response.GetStatus() {
	case protocolv1.WorkReadyStatus_WORK_READY_STATUS_READY:
		return &Ready{WorkID: workID, State: workState}, nil
	case protocolv1.WorkReadyStatus_WORK_READY_STATUS_REJECTED:
		return nil, &Rejected{WorkID: workID, Code: response.GetErrorCode(), State: workState}
	default:
		// 状态机已拒绝所有未声明状态；本分支只用于保持返回语义显式且快速失败。
		workState.Close()
		return nil, fmt.Errorf("%w: unexpected WorkReady status", ErrProtocol)
	}
}

func validateSessionAndLease(session controlauth.Session, budgetLeaseID string) error {
	if err := identity.ValidateTunnelID(session.TunnelID); err != nil {
		return fmt.Errorf("%w: tunnel id: %v", ErrInvalidInput, err)
	}
	if err := identity.ValidateConnectorID(session.ConnectorID); err != nil {
		return fmt.Errorf("%w: connector id: %v", ErrInvalidInput, err)
	}
	if err := identity.ValidateSessionID(session.SessionID); err != nil {
		return fmt.Errorf("%w: session id: %v", ErrInvalidInput, err)
	}
	if err := validate.ValidateID(budgetLeaseID, "lease_"); err != nil {
		return fmt.Errorf("%w: budget lease id: %v", ErrInvalidInput, err)
	}
	return nil
}

// generateWorkID 复用 identity 的唯一 ULID CSPRNG 与时间编码实现。
func generateWorkID() (string, error) {
	return identity.NewWorkID()
}

func classifyIOError(operation string, err, timeoutSentinel error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", operation, err)
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return fmt.Errorf("%s: %w: %v", operation, timeoutSentinel, err)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func classifyReadError(err error) error {
	classified := classifyIOError("read WorkReady", err, ErrReadTimeout)
	if errors.Is(classified, ErrReadTimeout) || errors.Is(classified, context.Canceled) ||
		errors.Is(classified, context.DeadlineExceeded) {
		return classified
	}
	if errors.Is(err, frame.ErrInvalidLength) || errors.Is(err, frame.ErrTruncatedFrame) ||
		errors.Is(err, frame.ErrFrameTooLarge) || errors.Is(err, frame.ErrMalformedMessage) {
		return fmt.Errorf("%w: WorkReady frame: %v", ErrProtocol, err)
	}
	return classified
}

type contextReader struct {
	ctx      context.Context
	conn     net.Conn
	deadline time.Time
}

func (reader *contextReader) Read(buffer []byte) (int, error) {
	for {
		if err := reader.ctx.Err(); err != nil {
			return 0, err
		}
		if err := reader.conn.SetReadDeadline(nextDeadline(reader.ctx, reader.deadline)); err != nil {
			return 0, err
		}
		count, err := reader.conn.Read(buffer)
		if count > 0 {
			return count, nil
		}
		if contextErr := reader.ctx.Err(); contextErr != nil {
			return 0, contextErr
		}
		if !isPollingTimeout(err, reader.ctx, reader.deadline) {
			return 0, err
		}
	}
}

type contextWriter struct {
	ctx      context.Context
	conn     net.Conn
	deadline time.Time
}

func (writer *contextWriter) Write(buffer []byte) (int, error) {
	for {
		if err := writer.ctx.Err(); err != nil {
			return 0, err
		}
		if err := writer.conn.SetWriteDeadline(nextDeadline(writer.ctx, writer.deadline)); err != nil {
			return 0, err
		}
		count, err := writer.conn.Write(buffer)
		if count > 0 {
			return count, nil
		}
		if contextErr := writer.ctx.Err(); contextErr != nil {
			return 0, contextErr
		}
		if !isPollingTimeout(err, writer.ctx, writer.deadline) {
			return 0, err
		}
	}
}

func nextDeadline(ctx context.Context, operationDeadline time.Time) time.Time {
	deadline := time.Now().Add(cancellationPollInterval)
	if operationDeadline.Before(deadline) {
		deadline = operationDeadline
	}
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	return deadline
}

func isPollingTimeout(err error, ctx context.Context, operationDeadline time.Time) bool {
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout() &&
		ctx.Err() == nil && time.Now().Before(operationDeadline)
}
