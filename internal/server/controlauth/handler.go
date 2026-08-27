package controlauth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lifei6671/xtunnel/internal/application"
	"github.com/lifei6671/xtunnel/internal/healthbudget"
	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/state"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
)

const (
	protocolVersionV1 = uint32(1)
	sessionSecretSize = 32

	// maxConnectionTokenBytes 是冻结的原始 Credential 输入边界。
	// 必须在调用 Verify 以及未来计算认证限流 fingerprint 之前执行，避免攻击者
	// 使用超长随机 Token 放大解析与哈希成本或绕过统一的 Bootstrap 契约。
	maxConnectionTokenBytes = 8192
	maxHostnameBytes        = 255
	maxVersionBytes         = 128
	maxPlatformFieldBytes   = 64
	maxCapabilities         = 64
	maxCapabilityBytes      = 128

	cancellationPollInterval = 50 * time.Millisecond
)

var (
	// ErrInvalidOptions 表示 Handler 缺少依赖或使用了无法编码到 Protocol v1 的超时值。
	ErrInvalidOptions = errors.New("control authentication options are invalid")

	// ErrAuthenticationCapacity 表示 Server 暂时没有容量接受新 Control Session。
	// M1-12 的 Limit Manager 可在进入 Verify 前返回/包装本错误；它是认证失败映射中
	// 唯一明确的容量信号，客户端可按 retry_after_ms 退避重连。
	ErrAuthenticationCapacity = errors.New("control authentication capacity exhausted")
)

// TokenVerifier 是 Control AUTH 唯一需要的长期 Credential 校验能力。
// application.ConnectionTokenService 直接满足本接口。
type TokenVerifier interface {
	Verify(context.Context, string) (application.VerifiedConnectionToken, error)
}

var _ TokenVerifier = (*application.ConnectionTokenService)(nil)

// Options 固定一次 AUTH 的有界 IO 和成功响应参数。
type Options struct {
	// ReadTimeout 限制完整 ConnectorAuthRequest Frame 的读取时间。
	ReadTimeout time.Duration
	// WriteTimeout 限制完整 ConnectorAuthResult Frame 的写出时间。
	WriteTimeout time.Duration
	// HeartbeatInterval 是成功响应下发给 Connector 的 Server 权威心跳周期。
	HeartbeatInterval time.Duration
	// RetryAfter 是容量或 Server 内部瞬态错误建议的退避时间；永久错误固定返回零。
	RetryAfter time.Duration
	// MaxFrameBytes 由 Server Schema 收紧 AUTH Frame；零值使用 Protocol v1 绝对上限。
	MaxFrameBytes uint64
}

// Established 是成功认证后交给 Control Session Owner 的内存状态。
// SessionSecret 只用于当前 Session 的 WorkConn HMAC，禁止记录或持久化。
type Established struct {
	// Session 是已经在成功 Frame 写出后发布的 Current Session。
	Session serverruntime.Session
	// ConnectorMetadata 是 Auth Request 已验证的非敏感进程元数据。
	ConnectorMetadata serverruntime.ConnectorMetadata
	// SessionSecret 是仅驻留双方内存的 32 字节 WorkConn HMAC 密钥。
	SessionSecret [sessionSecretSize]byte
	// ProtocolVersion 是本次认证协商出的 Protocol v1 版本。
	ProtocolVersion uint32
	// DesiredRevision 是认证事务内读取的 Tunnel 当前期望配置版本。
	DesiredRevision uint64
	// HeartbeatInterval 是成功响应已经下发的 Server 权威心跳周期。
	HeartbeatInterval time.Duration
	// Control 已在成功 Frame 写出后进入 ESTABLISHED，可移交给单 Owner。
	Control *state.Control
}

// HandleError 描述认证连接为何关闭。Error 文本只包含稳定错误码，故上层可以安全
// 记录它；底层 cause 仅供 errors.Is/As 检查，不会把 Token 或 Secret 拼进日志文本。
type HandleError struct {
	code        protocolv1.ErrorCode
	failureSent bool
	cause       error
}

// Error 返回不含 Credential 内容的稳定错误描述。
func (err *HandleError) Error() string {
	if err == nil {
		return "control authentication failed"
	}
	return fmt.Sprintf("control authentication failed: code=%s failure_sent=%t", err.code.String(), err.failureSent)
}

// Unwrap 允许测试和上层生命周期按错误类别处理，但不会改变 Error 的脱敏文本。
func (err *HandleError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.cause
}

// Code 返回已发送或计划发送的 Protocol v1 错误码；无法安全解码 Frame 时为 OK。
func (err *HandleError) Code() protocolv1.ErrorCode {
	if err == nil {
		return protocolv1.ErrorCode_ERROR_CODE_OK
	}
	return err.code
}

// FailureSent 返回对端是否已经收到一个完整 ConnectorAuthFailure Frame。
func (err *HandleError) FailureSent() bool {
	return err != nil && err.failureSent
}

// Handler 处理一条已经协商为 xtunnel-control/1 的连接。
type Handler struct {
	verifier   TokenVerifier
	registry   *serverruntime.Registry
	options    Options
	random     io.Reader
	now        func() time.Time
	retryAfter uint32
	heartbeat  uint32
}

// New 创建生产 Control AUTH Handler。
func New(verifier TokenVerifier, registry *serverruntime.Registry, options Options) (*Handler, error) {
	return newHandler(verifier, registry, options, rand.Reader, time.Now)
}

func newHandler(verifier TokenVerifier, registry *serverruntime.Registry, options Options, random io.Reader, now func() time.Time) (*Handler, error) {
	if options.MaxFrameBytes == 0 {
		options.MaxFrameBytes = frame.MaxAuthFrameSize
	}
	heartbeat, heartbeatOK := durationMilliseconds(options.HeartbeatInterval, false)
	retryAfter, retryOK := durationMilliseconds(options.RetryAfter, true)
	if verifier == nil || registry == nil || random == nil || now == nil || options.ReadTimeout <= 0 ||
		options.WriteTimeout <= 0 || options.MaxFrameBytes > frame.MaxAuthFrameSize || !heartbeatOK || !retryOK {
		return nil, ErrInvalidOptions
	}
	return &Handler{
		verifier: verifier, registry: registry, options: options,
		random: random, now: now, retryAfter: retryAfter, heartbeat: heartbeat,
	}, nil
}

// Handle 认证单条 Control 连接。
//
// 失败路径会关闭 connection；成功路径保留连接并返回所属 Session。调用方必须在
// Session 生命周期结束时关闭连接并用 Registry.ClearIfCurrent 做 generation fencing。
func (handler *Handler) Handle(ctx context.Context, connection net.Conn) (Established, error) {
	if handler == nil || connection == nil {
		return Established{}, ErrInvalidOptions
	}
	if err := ctx.Err(); err != nil {
		return Established{}, handler.closeWithError(connection, &HandleError{cause: err})
	}
	request := &protocolv1.ConnectorAuthRequest{}
	reader := &contextReader{
		ctx: ctx, conn: connection, deadline: handler.deadline(ctx, handler.options.ReadTimeout),
	}
	if err := frame.ReadAuthLimit(reader, request, handler.options.MaxFrameBytes); err != nil {
		// 长度越界、截断或 Protobuf 无法解码时，Server 无法确信消息边界，必须直接
		// 关闭，不能继续在同一字节流上发送看似可信的失败响应。
		return Established{}, handler.closeWithError(connection, &HandleError{cause: err})
	}

	control, err := state.NewControl(state.EndpointServer, protocolVersionV1)
	if err != nil {
		return Established{}, handler.closeWithError(connection, &HandleError{cause: err})
	}
	if err := validate.RejectUnknownFields(request); err != nil {
		return Established{}, handler.fail(ctx, connection, control, protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR, 0, err)
	}
	if _, err := control.AcceptInbound(request); err != nil {
		return Established{}, handler.fail(ctx, connection, control, protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR, 0, err)
	}
	if err := validateRequest(request); err != nil {
		return Established{}, handler.fail(ctx, connection, control, protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR, 0, err)
	}

	// Token 是 immutable string，无法原地擦除；校验后立即移除 Request 对它的引用，
	// 并确保后续错误、结果和 Runtime 对象都不保存该长期 Credential。
	encodedToken := request.GetConnectionToken()
	request.ConnectionToken = ""
	verified, verifyErr := handler.verifier.Verify(ctx, encodedToken)
	encodedToken = ""
	if verifyErr != nil {
		code, retryAfter := handler.classifyVerificationError(verifyErr)
		return Established{}, handler.fail(ctx, connection, control, code, retryAfter, verifyErr)
	}
	if verified.DesiredRevision < 0 {
		return Established{}, handler.fail(ctx, connection, control, protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR, handler.retryAfter, errors.New("negative tunnel desired revision"))
	}

	negotiated, compatible := negotiateProtocol(request.GetMinProtocol(), request.GetMaxProtocol())
	if !compatible {
		return Established{}, handler.fail(ctx, connection, control, protocolv1.ErrorCode_ERROR_CODE_VERSION_UNSUPPORTED, 0, errors.New("protocol version has no overlap"))
	}

	var sessionSecret [sessionSecretSize]byte
	if _, err := io.ReadFull(handler.random, sessionSecret[:]); err != nil {
		return Established{}, handler.fail(ctx, connection, control, protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR, handler.retryAfter, fmt.Errorf("generate session secret: %w", err))
	}
	committed := false
	defer func() {
		if !committed {
			clear(sessionSecret[:])
		}
	}()

	pending, err := handler.registry.ReserveAuthenticated(verified.TunnelID, request.GetConnectorId())
	if err != nil {
		if errors.Is(err, serverruntime.ErrTunnelRuntimeRevoked) {
			return Established{}, handler.fail(ctx, connection, control, protocolv1.ErrorCode_ERROR_CODE_TUNNEL_REVOKED, 0, err)
		}
		if errors.Is(err, serverlimits.ErrConnectorCapacity) {
			return Established{}, handler.fail(ctx, connection, control, protocolv1.ErrorCode_ERROR_CODE_SESSION_RESOURCE_EXHAUSTED, handler.retryAfter, err)
		}
		return Established{}, handler.fail(ctx, connection, control, protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR, handler.retryAfter, err)
	}
	registryCommitted := false
	defer func() {
		if !registryCommitted {
			handler.registry.CancelAuthenticated(pending)
		}
	}()

	result := &protocolv1.ConnectorAuthResult{Result: &protocolv1.ConnectorAuthResult_Success{
		Success: &protocolv1.ConnectorAuthSuccess{
			TunnelId: verified.TunnelID, SessionId: pending.SessionID(), SessionSecret: sessionSecret[:],
			ProtocolVersion: negotiated, DesiredRevision: uint64(verified.DesiredRevision), HeartbeatIntervalMs: handler.heartbeat,
		},
	}}
	if _, err := control.AcceptOutbound(result); err != nil {
		return Established{}, handler.closeWithError(connection, &HandleError{cause: err})
	}
	// 在任何 Success 字节写出前原子安装 Session，使完整 flush 后不再
	// 存在可被并发 Revoke 破坏的 Registry 提交步骤。后续任一失败都用
	// 完整 generation identity 回滚，不会误删并发重连已替换的 Session。
	install, err := handler.registry.InstallAuthenticated(pending)
	if err != nil {
		if errors.Is(err, serverruntime.ErrTunnelRuntimeRevoked) {
			return Established{}, handler.fail(ctx, connection, control, protocolv1.ErrorCode_ERROR_CODE_TUNNEL_REVOKED, 0, err)
		}
		if errors.Is(err, healthbudget.ErrTargetCapacity) {
			return Established{}, handler.fail(ctx, connection, control, protocolv1.ErrorCode_ERROR_CODE_HEALTH_BUDGET_EXCEEDED, handler.retryAfter, err)
		}
		return Established{}, handler.fail(ctx, connection, control, protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR, handler.retryAfter, err)
	}
	registryCommitted = true
	session := install.Session()
	sessionEstablished := false
	defer func() {
		if !sessionEstablished {
			install.Rollback()
		}
	}()
	writer := &contextWriter{
		ctx: ctx, conn: connection, deadline: handler.deadline(ctx, handler.options.WriteTimeout),
	}
	if err := frame.WriteAuthLimit(writer, result, handler.options.MaxFrameBytes); err != nil {
		// defer 用完整 generation identity 回滚本次预安装；并发重连已
		// 替换它时不会误删新 Session。
		return Established{}, handler.closeWithError(connection, &HandleError{cause: err})
	}
	// 完整 Success Frame 写出就是不可逆认证提交点：旧 Session 此刻不再
	// 可恢复。后续本地协议状态或交接失败只能清理已提交的新 Session。
	install.Finalize()
	if err := control.CommitAuthSuccessAfterFlush(result); err != nil {
		handler.registry.ClearIfCurrent(session)
		control.Close()
		return Established{}, handler.closeWithError(connection, &HandleError{cause: err})
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		// Session 已按 Success flush 提交。若连接交接前的最后清理失败，必须用完整
		// generation fencing 撤销本次 Session；并发重连已经替换它时不得误删新 Session。
		handler.registry.ClearIfCurrent(session)
		control.Close()
		return Established{}, handler.closeWithError(connection, &HandleError{cause: err})
	}
	sessionEstablished = true
	committed = true
	return Established{
		Session: session,
		ConnectorMetadata: serverruntime.ConnectorMetadata{
			Hostname: request.GetHostname(), OS: request.GetOs(), Arch: request.GetArch(), Version: request.GetVersion(),
		},
		SessionSecret: sessionSecret, ProtocolVersion: negotiated,
		DesiredRevision: uint64(verified.DesiredRevision), HeartbeatInterval: handler.options.HeartbeatInterval,
		Control: control,
	}, nil
}

func (handler *Handler) fail(ctx context.Context, connection net.Conn, control *state.Control, code protocolv1.ErrorCode, retryAfter uint32, cause error) error {
	result := &protocolv1.ConnectorAuthResult{Result: &protocolv1.ConnectorAuthResult_Failure{
		Failure: &protocolv1.ConnectorAuthFailure{ErrorCode: code, RetryAfterMs: retryAfter},
	}}
	if _, err := control.AcceptOutbound(result); err != nil {
		return handler.closeWithError(connection, &HandleError{code: code, cause: err})
	}
	writer := &contextWriter{
		ctx: ctx, conn: connection, deadline: handler.deadline(ctx, handler.options.WriteTimeout),
	}
	if err := frame.WriteAuthLimit(writer, result, handler.options.MaxFrameBytes); err != nil {
		return handler.closeWithError(connection, &HandleError{code: code, cause: err})
	}
	if err := control.CommitAuthFailureAfterFlush(result); err != nil {
		return handler.closeWithError(connection, &HandleError{code: code, failureSent: true, cause: err})
	}
	return handler.closeWithError(connection, &HandleError{code: code, failureSent: true, cause: cause})
}

func (handler *Handler) closeWithError(connection net.Conn, handleErr *HandleError) error {
	if err := connection.Close(); err != nil {
		return errors.Join(handleErr, fmt.Errorf("close control authentication connection: %w", err))
	}
	return handleErr
}

func (handler *Handler) classifyVerificationError(err error) (protocolv1.ErrorCode, uint32) {
	switch {
	case errors.Is(err, application.ErrConnectionTokenInactive):
		return protocolv1.ErrorCode_ERROR_CODE_TOKEN_REVOKED, 0
	case errors.Is(err, application.ErrConnectionTokenTunnelRevoked):
		return protocolv1.ErrorCode_ERROR_CODE_TUNNEL_REVOKED, 0
	case errors.Is(err, ErrAuthenticationCapacity):
		return protocolv1.ErrorCode_ERROR_CODE_SESSION_RESOURCE_EXHAUSTED, handler.retryAfter
	case errors.Is(err, application.ErrConnectionTokenInvalid),
		errors.Is(err, application.ErrConnectionTokenInput),
		errors.Is(err, application.ErrConnectionTokenIdentityMismatch),
		errors.Is(err, application.ErrConnectionTokenSecretMismatch),
		errors.Is(err, application.ErrConnectionTokenTunnelUnavailable):
		return protocolv1.ErrorCode_ERROR_CODE_TOKEN_INVALID, 0
	default:
		return protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR, handler.retryAfter
	}
}

func validateRequest(request *protocolv1.ConnectorAuthRequest) error {
	if request == nil || request.GetConnectionToken() == "" || len(request.GetConnectionToken()) > maxConnectionTokenBytes ||
		!utf8.ValidString(request.GetConnectionToken()) ||
		identity.ValidateConnectorID(request.GetConnectorId()) != nil ||
		!validText(request.GetHostname(), maxHostnameBytes) || !validText(request.GetVersion(), maxVersionBytes) ||
		!validText(request.GetOs(), maxPlatformFieldBytes) || !validText(request.GetArch(), maxPlatformFieldBytes) ||
		request.GetMinProtocol() == 0 || request.GetMaxProtocol() < request.GetMinProtocol() ||
		len(request.GetCapabilities()) > maxCapabilities {
		return errors.New("connector authentication request fields are invalid")
	}
	seen := make(map[string]struct{}, len(request.GetCapabilities()))
	for _, capability := range request.GetCapabilities() {
		if !validText(capability, maxCapabilityBytes) {
			return errors.New("connector capability is invalid")
		}
		if _, exists := seen[capability]; exists {
			return errors.New("connector capability is duplicated")
		}
		seen[capability] = struct{}{}
	}
	return nil
}

func validText(value string, maximumBytes int) bool {
	return value != "" && len(value) <= maximumBytes && utf8.ValidString(value) && strings.TrimSpace(value) == value
}

func negotiateProtocol(minimum, maximum uint32) (uint32, bool) {
	if minimum <= protocolVersionV1 && protocolVersionV1 <= maximum {
		return protocolVersionV1, true
	}
	return 0, false
}

func durationMilliseconds(value time.Duration, allowZero bool) (uint32, bool) {
	if value < 0 || (!allowZero && value == 0) || value%time.Millisecond != 0 {
		return 0, false
	}
	milliseconds := value / time.Millisecond
	if milliseconds > math.MaxUint32 {
		return 0, false
	}
	return uint32(milliseconds), true
}

func (handler *Handler) deadline(ctx context.Context, timeout time.Duration) time.Time {
	deadline := handler.now().Add(timeout)
	if contextDeadline, exists := ctx.Deadline(); exists && contextDeadline.Before(deadline) {
		return contextDeadline
	}
	return deadline
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
