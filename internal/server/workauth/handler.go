package workauth

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
)

const cancellationPollInterval = 50 * time.Millisecond

var (
	// ErrInvalidHandlerOptions 表示 Handler 缺少 Resolver 或 IO 超时边界。
	ErrInvalidHandlerOptions = errors.New("server work authentication handler options are invalid")
	// ErrReadTimeout 表示 WorkHello 没有在读取上限内形成一个完整 Frame。
	ErrReadTimeout = errors.New("server work authentication read timeout")
	// ErrWriteTimeout 表示 WorkReady 没有在写入上限内完整交付给 Agent。
	ErrWriteTimeout = errors.New("server work authentication write timeout")
)

// Resolver 根据已经通过协议形状校验的 Session ID 查找当前 Session Authenticator。
//
// Gateway 在读取 WorkHello 前并不知道连接属于哪个 Session，因此 Handler 必须自己
// 完成唯一一次 Frame 读取，再调用 Resolver。实现方只能返回当前可接受 WorkConn 的
// Authenticator；未命中必须返回 (nil, false)，不得创建占位 Session。
type Resolver interface {
	ResolveSessionAuthenticator(sessionID string) (*SessionAuthenticator, bool)
}

// ResolverFunc 允许 Runtime/Gateway 用一个纯内存查找闭包满足 Resolver。
type ResolverFunc func(sessionID string) (*SessionAuthenticator, bool)

// ResolveSessionAuthenticator 调用底层闭包。
func (resolve ResolverFunc) ResolveSessionAuthenticator(sessionID string) (*SessionAuthenticator, bool) {
	return resolve(sessionID)
}

// HandlerOptions 固定单条 WorkConn 认证阶段的有界 IO。
type HandlerOptions struct {
	// ReadTimeout 限制完整 WorkHello Frame 的读取时间。
	ReadTimeout time.Duration
	// WriteTimeout 限制完整 WorkReady Frame 的写出时间。
	WriteTimeout time.Duration
	// MaxFrameBytes 由 Server Schema 收紧 Work AUTH Frame；零值使用协议绝对上限。
	MaxFrameBytes uint64
}

// Idle 是 READY Frame 完整写出后交给 WorkPool 的 Server 侧 WorkConn 身份与状态。
//
// 这里只返回非敏感协议 ID。调用方必须按 SessionID 做 generation fencing，确认目标
// Session 仍为 Current 后再把连接发布进 WorkPool。
type Idle struct {
	TunnelID    string
	ConnectorID string
	SessionID   string
	WorkID      string
	State       *state.Work
}

// HandleError 描述认证连接为何关闭。Error 文本只包含冻结错误码与响应提交状态；
// cause 只用于 errors.Is/As，不会把 WorkHello、MAC、nonce 或任何 Secret 拼入日志。
type HandleError struct {
	code         protocolv1.ErrorCode
	responseSent bool
	cause        error
}

// Error 返回可安全记录的稳定错误描述。
func (handleErr *HandleError) Error() string {
	if handleErr == nil {
		return "server work authentication failed"
	}
	return fmt.Sprintf("server work authentication failed: code=%s response_sent=%t", handleErr.code.String(), handleErr.responseSent)
}

// Unwrap 暴露分类后的底层原因，供生命周期按 Context、Frame 或认证决策分类。
func (handleErr *HandleError) Unwrap() error {
	if handleErr == nil {
		return nil
	}
	return handleErr.cause
}

// Code 返回已经发送或计划发送的公开错误码；无法安全解码时为 OK。
func (handleErr *HandleError) Code() protocolv1.ErrorCode {
	if handleErr == nil {
		return protocolv1.ErrorCode_ERROR_CODE_OK
	}
	return handleErr.code
}

// ResponseSent 返回对端是否已经收到一个完整 WorkReady Frame。
func (handleErr *HandleError) ResponseSent() bool {
	return handleErr != nil && handleErr.responseSent
}

// Handler 处理一条已经完成 xtunnel-work/1 ALPN TLS 握手的连接。
type Handler struct {
	resolver Resolver
	options  HandlerOptions
}

// NewHandler 创建生产 Work AUTH Handler。
func NewHandler(resolver Resolver, options HandlerOptions) (*Handler, error) {
	if options.MaxFrameBytes == 0 {
		options.MaxFrameBytes = frame.MaxWorkFrameSize
	}
	if !validResolver(resolver) || options.ReadTimeout <= 0 || options.WriteTimeout <= 0 ||
		options.MaxFrameBytes > frame.MaxWorkFrameSize {
		return nil, ErrInvalidHandlerOptions
	}
	return &Handler{resolver: resolver, options: options}, nil
}

// Handle 同步认证单条 Work 连接。
//
// 失败路径始终关闭 connection；成功路径只有在 READY Frame 完整写出、状态提交为
// WorkIdle 且临时 Deadline 清理成功后才返回，连接保持打开供 WorkPool 接管。
func (handler *Handler) Handle(ctx context.Context, connection net.Conn) (idle Idle, resultErr error) {
	if handler == nil || ctx == nil || connection == nil || !validResolver(handler.resolver) {
		if connection != nil {
			_ = connection.Close()
		}
		return Idle{}, ErrInvalidHandlerOptions
	}

	workState, err := state.NewWork(state.EndpointServer)
	if err != nil {
		return Idle{}, handler.closeWithError(connection, &HandleError{cause: err})
	}
	// 所有错误都在一个出口关闭状态和连接，避免 malformed、timeout 或失败响应写到
	// 一半时遗漏资源清理；成功返回不会触碰已交接的连接。
	defer func() {
		if resultErr != nil {
			workState.Close()
			resultErr = handler.closeWithError(connection, resultErr)
		}
	}()
	if err := ctx.Err(); err != nil {
		return Idle{}, &HandleError{cause: err}
	}

	hello := &protocolv1.WorkHello{}
	// Protobuf 在 malformed 返回前可能已经填入部分 bytes 字段，故无论读取结果如何
	// 都清除本地 nonce/MAC 副本；Authenticator 也不会保留它们。
	defer func() {
		clear(hello.Nonce)
		clear(hello.Mac)
	}()
	reader := &contextReader{ctx: ctx, conn: connection, deadline: time.Now().Add(handler.options.ReadTimeout)}
	if err := frame.ReadWorkLimit(reader, hello, handler.options.MaxFrameBytes); err != nil {
		// 边界不完整、超长或 Protobuf malformed 时不能确信消息语义，必须直接关闭，
		// 不能在同一字节流上追加一个看似可信的 WorkReady。
		return Idle{}, &HandleError{cause: classifyIOError("read WorkHello", err, ErrReadTimeout)}
	}
	if err := validate.RejectUnknownFields(hello); err != nil {
		// 冻结协议禁止 Unknown Field；该类非法消息直接关闭，不发送结构化响应。
		return Idle{}, &HandleError{code: protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR, cause: err}
	}
	if err := validateHelloShape(hello); err != nil {
		// Resolver 只能看见完整合法的 ID、nonce 与 MAC 形状，避免用非法输入触碰
		// Runtime Map，也避免在尚未接受 WorkID 时构造无法关联的 WorkReady。
		return Idle{}, &HandleError{code: protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR, cause: err}
	}
	if err := workState.AcceptInbound(hello); err != nil {
		return Idle{}, &HandleError{code: protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR, cause: err}
	}

	authenticator, found := handler.resolver.ResolveSessionAuthenticator(hello.GetSessionId())
	if !found || authenticator == nil {
		// 未知 Session 与已知 Session 的错误 HMAC 使用完全相同的公开响应，避免
		// WorkReady 成为 Session 是否存在的认证 Oracle。
		return Idle{}, handler.reject(ctx, connection, workState, hello.GetWorkId(), decisionError(ReasonSessionInvalid))
	}
	if err := authenticator.ValidateAndConsume(hello); err != nil {
		var decision *DecisionError
		if !errors.As(err, &decision) {
			decision = decisionError(ReasonSessionInvalid)
		}
		return Idle{}, handler.reject(ctx, connection, workState, hello.GetWorkId(), decision)
	}

	ready := &protocolv1.WorkReady{
		WorkId: hello.GetWorkId(), Status: protocolv1.WorkReadyStatus_WORK_READY_STATUS_READY,
		ErrorCode: protocolv1.ErrorCode_ERROR_CODE_OK,
	}
	writer := &contextWriter{ctx: ctx, conn: connection, deadline: time.Now().Add(handler.options.WriteTimeout)}
	if err := frame.WriteWorkLimit(writer, ready, handler.options.MaxFrameBytes); err != nil {
		return Idle{}, &HandleError{cause: classifyIOError("write READY WorkReady", err, ErrWriteTimeout)}
	}
	// 完整 READY Frame flush 是 Server 侧 WorkIdle 的唯一提交点。即使此前 Lease Slot
	// 已经消费，半写响应也绝不能把连接发布给 WorkPool；Replay 仍保留以禁止复用。
	if err := workState.AcceptOutbound(ready); err != nil {
		return Idle{}, &HandleError{responseSent: true, cause: err}
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		return Idle{}, &HandleError{responseSent: true, cause: fmt.Errorf("clear server work auth deadline: %w", err)}
	}
	return Idle{
		TunnelID: hello.GetTunnelId(), ConnectorID: hello.GetConnectorId(),
		SessionID: hello.GetSessionId(), WorkID: hello.GetWorkId(), State: workState,
	}, nil
}

func (handler *Handler) reject(
	ctx context.Context,
	connection net.Conn,
	workState *state.Work,
	workID string,
	decision *DecisionError,
) error {
	ready := &protocolv1.WorkReady{
		WorkId: workID, Status: protocolv1.WorkReadyStatus_WORK_READY_STATUS_REJECTED,
		ErrorCode: decision.Code,
	}
	writer := &contextWriter{ctx: ctx, conn: connection, deadline: time.Now().Add(handler.options.WriteTimeout)}
	if err := frame.WriteWorkLimit(writer, ready, handler.options.MaxFrameBytes); err != nil {
		// 即使响应未能完整写出，也保留不含敏感输入的内部 Reason，便于 Owner
		// 通过 errors.As 诊断；HandleError.Error 仍不会展开底层错误文本。
		return &HandleError{code: decision.Code, cause: errors.Join(decision, classifyIOError("write REJECTED WorkReady", err, ErrWriteTimeout))}
	}
	// REJECTED 也只在完整 Frame flush 后提交 CLOSED；调用方随后统一关闭 TLS 连接。
	if err := workState.AcceptOutbound(ready); err != nil {
		return &HandleError{code: decision.Code, responseSent: true, cause: err}
	}
	return &HandleError{code: decision.Code, responseSent: true, cause: decision}
}

func (handler *Handler) closeWithError(connection net.Conn, cause error) error {
	result := cause
	if err := connection.Close(); err != nil {
		result = errors.Join(result, fmt.Errorf("close server work authentication connection: %w", err))
	}
	return result
}

func validResolver(resolver Resolver) bool {
	if resolver == nil {
		return false
	}
	// ResolverFunc 是公开的便捷适配器；nil 函数装入接口后接口本身并不为 nil，
	// 必须显式拒绝，避免首个合法 WorkHello 触发运行时 panic。
	if function, ok := resolver.(ResolverFunc); ok {
		return function != nil
	}
	return true
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

// contextReader/contextWriter 通过短 Deadline 轮询把无 Deadline 的 Context 取消
// 转换为同步 net.Conn IO 退出；不启动额外 goroutine，也不会在返回后遗留后台任务。
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
			return count, err
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
			return count, err
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
