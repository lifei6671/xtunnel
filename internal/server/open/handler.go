// Package open 实现 Server 侧 IDLE WorkConn 的 OPEN 状态机与 RAW 交接。
package open

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/state"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
	serverworkauth "github.com/lifei6671/xtunnel/internal/server/workauth"
)

var (
	// ErrInvalidInput 表示 WorkConn、IDLE 状态、请求或超时不完整。
	ErrInvalidInput = errors.New("server open input is invalid")
	// ErrProtocol 表示 OpenRequest/OpenResponse 违反冻结 Protocol v1。
	ErrProtocol = errors.New("server open protocol violation")
	// ErrRejected 表示 Agent 完整返回了 OPEN_ERROR。
	ErrRejected = errors.New("server open was rejected by agent")
	// ErrPreRAWTransport 表示 AcceptRaw 前的 WorkConn IO 失败；调用方可按受限契约重试。
	ErrPreRAWTransport = errors.New("server open transport failed before RAW")
	// ErrRawCommitted 表示 AcceptRaw 已提交，后续失败绝不能重放业务连接。
	ErrRawCommitted = errors.New("server open RAW was already committed")
)

// Options 固定完整 OpenRequest/OpenResponse 交换的 IO 上限。
type Options struct {
	WriteTimeout time.Duration
	ReadTimeout  time.Duration
	// MaxFrameBytes 由 Server Schema 收紧 OPEN Frame；零值使用协议绝对上限。
	MaxFrameBytes uint64
}

// Handler 在已经认证的 IDLE WorkConn 上执行一次 OPEN。
type Handler struct {
	options Options
}

// Active 是 OpenResponse OPEN_OK 完整读取并提交后可进入 RAW 的连接。
type Active struct {
	Connection net.Conn
	Identity   serverworkauth.Idle
	Response   *protocolv1.OpenResponse
}

// Rejected 保留 Agent 返回的公开失败码，不暴露内部 Origin 地址或错误文本。
type Rejected struct {
	Code protocolv1.ErrorCode
}

func (rejected *Rejected) Error() string {
	return fmt.Sprintf("%s: code=%s", ErrRejected.Error(), rejected.Code.String())
}

func (rejected *Rejected) Unwrap() error { return ErrRejected }

// NewHandler 创建有界 OPEN Handler。
func NewHandler(options Options) (*Handler, error) {
	if options.MaxFrameBytes == 0 {
		options.MaxFrameBytes = frame.MaxWorkFrameSize
	}
	if options.WriteTimeout <= 0 || options.ReadTimeout <= 0 || options.MaxFrameBytes > frame.MaxWorkFrameSize {
		return nil, ErrInvalidInput
	}
	return &Handler{options: options}, nil
}

// Handle 写出唯一 OpenRequest 并读取唯一 OpenResponse。
//
// 失败路径关闭 WorkConn；成功路径保留连接并返回 Active，由调用方立即注册
// ActiveWork 并进入 RAW。Frame Reader 精确停在响应边界，不会吞掉同一底层 Read 中
// 已经跟随 OPEN_OK 到达的首批业务字节。
func (handler *Handler) Handle(
	ctx context.Context,
	connection net.Conn,
	idle serverworkauth.Idle,
	request *protocolv1.OpenRequest,
) (active *Active, resultErr error) {
	if handler == nil || ctx == nil || connection == nil || idle.State == nil || idle.State.Phase() != state.WorkIdle ||
		request == nil || request.GetProtocolVersion() != 1 ||
		request.GetIngressType() == protocolv1.IngressType_INGRESS_TYPE_UNSPECIFIED {
		if connection != nil {
			_ = connection.Close()
		}
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		_ = connection.Close()
		idle.State.Close()
		return nil, err
	}
	if err := validate.RejectUnknownFields(request); err != nil {
		_ = connection.Close()
		idle.State.Close()
		return nil, fmt.Errorf("%w: OpenRequest unknown fields", ErrProtocol)
	}
	if err := validate.ValidateID(request.GetConnectionId(), "conn_"); err != nil {
		_ = connection.Close()
		idle.State.Close()
		return nil, fmt.Errorf("%w: invalid connection id", ErrProtocol)
	}
	if err := validate.ValidateID(request.GetServiceId(), "svc_"); err != nil {
		_ = connection.Close()
		idle.State.Close()
		return nil, fmt.Errorf("%w: invalid service id", ErrProtocol)
	}
	defer func() {
		if resultErr == nil {
			return
		}
		idle.State.Close()
		if err := connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			resultErr = errors.Join(resultErr, fmt.Errorf("close Server WorkConn after OPEN failure: %w", err))
		}
	}()
	stopContextIO := context.AfterFunc(ctx, func() { _ = connection.SetDeadline(time.Now()) })
	defer stopContextIO()

	if err := connection.SetWriteDeadline(operationDeadline(ctx, handler.options.WriteTimeout)); err != nil {
		return nil, fmt.Errorf("%w: set OpenRequest write deadline: %w", ErrPreRAWTransport, err)
	}
	if err := frame.WriteWorkLimit(connection, request, handler.options.MaxFrameBytes); err != nil {
		return nil, classifyFrameFailure("write OpenRequest", err)
	}
	// 完整 Request Frame flush 后才提交 IDLE→OPENING；若写到一半，连接直接关闭，
	// 不会被错误归还到 Idle Pool。
	if err := idle.State.AcceptOutbound(request); err != nil {
		return nil, fmt.Errorf("%w: commit OpenRequest: %v", ErrProtocol, err)
	}

	if err := connection.SetReadDeadline(operationDeadline(ctx, handler.options.ReadTimeout)); err != nil {
		return nil, fmt.Errorf("%w: set OpenResponse read deadline: %w", ErrPreRAWTransport, err)
	}
	response := &protocolv1.OpenResponse{}
	if err := frame.ReadWorkLimit(connection, response, handler.options.MaxFrameBytes); err != nil {
		return nil, classifyFrameFailure("read OpenResponse", err)
	}
	if err := validate.RejectUnknownFields(response); err != nil {
		return nil, fmt.Errorf("%w: OpenResponse unknown fields", ErrProtocol)
	}
	if response.GetStatus() == protocolv1.OpenStatus_OPEN_STATUS_OK {
		if response.GetErrorCode() != protocolv1.ErrorCode_ERROR_CODE_OK {
			return nil, fmt.Errorf("%w: OPEN_OK carried non-OK code", ErrProtocol)
		}
	} else if response.GetStatus() == protocolv1.OpenStatus_OPEN_STATUS_ERROR {
		if response.GetErrorCode() == protocolv1.ErrorCode_ERROR_CODE_OK {
			return nil, fmt.Errorf("%w: OPEN_ERROR carried OK code", ErrProtocol)
		}
	}
	if err := idle.State.AcceptInbound(response); err != nil {
		return nil, fmt.Errorf("%w: accept OpenResponse: %v", ErrProtocol, err)
	}
	if response.GetStatus() == protocolv1.OpenStatus_OPEN_STATUS_ERROR {
		return nil, &Rejected{Code: response.GetErrorCode()}
	}
	if err := idle.State.AcceptRaw(); err != nil {
		return nil, fmt.Errorf("%w: RAW handoff: %v", ErrProtocol, err)
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("%w: clear Server WorkConn OPEN deadline: %w", ErrRawCommitted, err)
	}
	return &Active{Connection: connection, Identity: idle, Response: response}, nil
}

func classifyFrameFailure(operation string, err error) error {
	classification := ErrPreRAWTransport
	if errors.Is(err, frame.ErrInvalidLength) || errors.Is(err, frame.ErrFrameTooLarge) ||
		errors.Is(err, frame.ErrMalformedMessage) || errors.Is(err, frame.ErrNilMessage) {
		classification = ErrProtocol
	}
	return fmt.Errorf("%w: %s: %w", classification, operation, err)
}

func operationDeadline(ctx context.Context, timeout time.Duration) time.Time {
	deadline := time.Now().Add(timeout)
	if contextDeadline, exists := ctx.Deadline(); exists && contextDeadline.Before(deadline) {
		return contextDeadline
	}
	return deadline
}
