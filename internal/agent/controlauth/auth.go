// Package controlauth 实现 Agent 在 Control TLS 建立后的 Protocol v1 认证握手。
//
// 本包只负责同步交换 AUTH 裸帧并把协议状态提交到 ESTABLISHED 或 CLOSED；它不负责
// TLS 拨号、重连、Control Session Owner、WorkPool，也不会记录长期 Token 或 Session Secret。
package controlauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/state"
	connectiontoken "github.com/lifei6671/xtunnel/internal/protocol/token"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
)

const (
	// protocolVersionV1 是当前 Agent 能够完成状态提交的唯一协议版本。
	protocolVersionV1 uint32 = 1
	// maxConnectionTokenBytes 与启动边界及 Server AUTH 校验保持一致。
	// 这里按 Go string 的字节长度检查，避免绕过 CLI 的调用方把超长 Credential
	// 带入解析、TLS 认证请求或后续错误路径。
	maxConnectionTokenBytes = 8192

	// cancellationPollInterval 让没有 Deadline 的 Context 也能中断阻塞网络 IO。
	// 轮询由当前调用 goroutine 完成，不为每次认证额外创建 goroutine。
	cancellationPollInterval = 50 * time.Millisecond
)

var (
	// ErrInvalidInput 表示调用方提供的 Token、Connector 或认证参数不合法。
	ErrInvalidInput = errors.New("connector control auth input is invalid")
	// ErrProtocol 表示对端返回了无法安全接受的 Protocol v1 认证结果。
	ErrProtocol = errors.New("connector control auth protocol violation")
	// ErrWriteTimeout 表示认证请求未能在配置的写入上限内完整发送。
	ErrWriteTimeout = errors.New("connector control auth write timeout")
	// ErrReadTimeout 表示认证结果未能在配置的读取上限内完整接收。
	ErrReadTimeout = errors.New("connector control auth read timeout")
)

// FailureClass 是 Server 显式认证失败对后续重连策略的稳定分类。
type FailureClass uint8

const (
	// FailurePermanent 表示重试同一凭据不会恢复，调用方必须停止快速重连。
	FailurePermanent FailureClass = iota + 1
	// FailureRetryable 表示 Server 短暂容量或内部状态失败，可进入有抖动退避。
	FailureRetryable
)

// Failure 保存 Server 返回的显式认证失败，不丢失错误码和 retry_after。
//
// 调用方必须通过 Retryable 判断是否允许重连，禁止只根据 RetryAfter 是否非零推断。
type Failure struct {
	// Code 是 Server 返回且经认证场景白名单校验的 Protocol v1 错误码。
	Code protocolv1.ErrorCode
	// Class 决定重连器是否允许重试，不能由 RetryAfter 反向推断。
	Class FailureClass
	// RetryAfter 是 Server 建议的最短等待时间；永久失败固定为零。
	RetryAfter time.Duration
}

// Error 返回不包含 Token、Secret 或其他敏感认证材料的稳定错误描述。
func (failure *Failure) Error() string {
	return fmt.Sprintf("connector control auth rejected: code=%s class=%s retry_after=%s",
		failure.Code.String(), failure.Class.String(), failure.RetryAfter)
}

// Retryable 返回该认证失败是否允许由重连器按退避策略重试。
func (failure *Failure) Retryable() bool {
	return failure != nil && failure.Class == FailureRetryable
}

// String 返回适合错误分类和结构化日志字段使用的固定文本。
func (class FailureClass) String() string {
	switch class {
	case FailurePermanent:
		return "permanent"
	case FailureRetryable:
		return "retryable"
	default:
		return "unknown"
	}
}

// Config 是一次 Connector Control 认证所需的全部进程与协议输入。
type Config struct {
	// ConnectionToken 是已经由启动边界取得的完整 xta_ 文本，认证前会再次严格解析。
	ConnectionToken string
	// Connector 是当前 Agent 进程内、跨重连复用的 ephemeral 身份。
	Connector identity.Connector
	// Hostname、Version、OS、Arch 是 Connector 自报的运行信息，不参与凭据身份判定。
	Hostname string
	Version  string
	OS       string
	Arch     string
	// MinProtocol 与 MaxProtocol 描述当前进程接受的协议区间；v0.1 必须包含版本 1。
	MinProtocol uint32
	MaxProtocol uint32
	// Capabilities 会复制进请求，避免调用方在同步写入期间修改消息切片。
	Capabilities []string
	// WriteTimeout 与 ReadTimeout 分别限制完整请求和完整结果 Frame 的网络阶段。
	WriteTimeout time.Duration
	ReadTimeout  time.Duration
}

// Session 是成功认证后交给后续 Control Session Owner 的内存状态。
// SessionSecret 只应继续用于当前 Session 的 WorkHello HMAC，禁止持久化或记录。
type Session struct {
	// TunnelID 来自 Token 与 Server Success 的双向一致性校验。
	TunnelID string
	// ConnectorID 是本进程在认证请求中发送的 Connector 身份。
	ConnectorID string
	// SessionID 是 Server 为本次已认证 Control 连接签发的 ephemeral 身份。
	SessionID string
	// SessionSecret 是仅供当前 Session 的 WorkHello HMAC 使用的 32 字节密钥。
	SessionSecret [32]byte
	// ProtocolVersion 是 Server 选择且经本地协议区间验证的版本。
	ProtocolVersion uint32
	// DesiredRevision 是认证完成时 Server 声明的 Tunnel Desired Revision。
	DesiredRevision uint64
	// HeartbeatInterval 是后续 Session Owner 使用的心跳周期。
	HeartbeatInterval time.Duration
	// Control 已完成 Agent Decode Commit，返回时固定处于 ESTABLISHED。
	Control *state.Control
}

// Authenticate 在已经完成 Control ALPN TLS 握手的连接上同步完成 Connector AUTH。
//
// 成功返回的 Control 已经完成 Decode Commit 并处于 ESTABLISHED；显式 Failure 已经
// 完成失败提交并处于 CLOSED。网络或协议错误不会被误包装成 Server 认证失败。
func Authenticate(ctx context.Context, connection net.Conn, config Config) (session *Session, resultErr error) {
	if ctx == nil || connection == nil {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(config.ConnectionToken) == 0 || len(config.ConnectionToken) > maxConnectionTokenBytes ||
		config.WriteTimeout <= 0 || config.ReadTimeout <= 0 ||
		config.MinProtocol == 0 || config.MinProtocol > protocolVersionV1 || config.MaxProtocol < protocolVersionV1 {
		return nil, ErrInvalidInput
	}
	connectorID := config.Connector.ID()
	if err := identity.ValidateConnectorID(connectorID); err != nil {
		return nil, fmt.Errorf("%w: connector id: %v", ErrInvalidInput, err)
	}

	parsedToken, err := connectiontoken.Parse(config.ConnectionToken)
	if err != nil {
		return nil, fmt.Errorf("%w: connection token: %v", ErrInvalidInput, err)
	}
	// Parse 后只需要 Tunnel 身份；立即擦除当前对象中的 Secret 副本，避免扩大内存驻留范围。
	tunnelID := parsedToken.GetTunnelId()
	clear(parsedToken.AuthenticationSecret)

	control, err := state.NewControl(state.EndpointAgent, protocolVersionV1)
	if err != nil {
		return nil, fmt.Errorf("create connector auth protocol state: %w", err)
	}
	defer func() {
		// 任何未返回 Session 的路径都不能把半完成的 AUTH 状态继续交给后续逻辑。
		if resultErr != nil {
			control.Close()
		}
	}()
	request := &protocolv1.ConnectorAuthRequest{
		ConnectionToken: config.ConnectionToken,
		ConnectorId:     connectorID,
		Hostname:        config.Hostname,
		Version:         config.Version,
		Os:              config.OS,
		Arch:            config.Arch,
		MinProtocol:     config.MinProtocol,
		MaxProtocol:     config.MaxProtocol,
		Capabilities:    append([]string(nil), config.Capabilities...),
	}
	if _, err := control.AcceptOutbound(request); err != nil {
		return nil, fmt.Errorf("validate connector auth request state: %w", err)
	}

	// 每次 Read/Write 都由有界操作 Deadline 和 Context 轮询共同约束；认证结束后必须
	// 清除临时 Deadline，否则后续 ESTABLISHED Control IO 会继承已经过期的边界。
	defer func() {
		if err := connection.SetDeadline(time.Time{}); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("clear connector auth deadline: %w", err))
			if session != nil {
				clear(session.SessionSecret[:])
			}
			session = nil
		}
	}()

	writer := &contextWriter{
		ctx:      ctx,
		conn:     connection,
		deadline: time.Now().Add(config.WriteTimeout),
	}
	if err := frame.WriteAuth(writer, request); err != nil {
		return nil, classifyWriteError(err)
	}

	response := &protocolv1.ConnectorAuthResult{}
	reader := &contextReader{
		ctx:      ctx,
		conn:     connection,
		deadline: time.Now().Add(config.ReadTimeout),
	}
	if err := frame.ReadAuth(reader, response); err != nil {
		return nil, classifyReadError(err)
	}
	if err := validate.RejectUnknownFields(response); err != nil {
		control.Close()
		return nil, fmt.Errorf("%w: auth result unknown fields: %v", ErrProtocol, err)
	}
	if _, err := control.AcceptInbound(response); err != nil {
		control.Close()
		return nil, fmt.Errorf("%w: auth result state: %v", ErrProtocol, err)
	}

	if failureMessage := response.GetFailure(); failureMessage != nil {
		failure, err := parseFailure(failureMessage)
		if err != nil {
			control.Close()
			return nil, err
		}
		if err := control.CommitAuthFailureAfterDecode(response); err != nil {
			control.Close()
			return nil, fmt.Errorf("%w: commit auth failure: %v", ErrProtocol, err)
		}
		return nil, failure
	}

	success := response.GetSuccess()
	if err := validateSuccess(success, tunnelID, config.MinProtocol, config.MaxProtocol); err != nil {
		control.Close()
		return nil, err
	}
	if err := control.CommitAuthSuccessAfterDecode(response); err != nil {
		control.Close()
		return nil, fmt.Errorf("%w: commit auth success: %v", ErrProtocol, err)
	}

	session = &Session{
		TunnelID:          success.GetTunnelId(),
		ConnectorID:       connectorID,
		SessionID:         success.GetSessionId(),
		ProtocolVersion:   success.GetProtocolVersion(),
		DesiredRevision:   success.GetDesiredRevision(),
		HeartbeatInterval: time.Duration(success.GetHeartbeatIntervalMs()) * time.Millisecond,
		Control:           control,
	}
	copy(session.SessionSecret[:], success.GetSessionSecret())
	// Secret 已复制到固定长度 Session 内存后，不再保留响应消息中的第二份切片。
	clear(success.SessionSecret)
	return session, nil
}

func validateSuccess(success *protocolv1.ConnectorAuthSuccess, tunnelID string, minimum, maximum uint32) error {
	if success == nil {
		return fmt.Errorf("%w: missing auth success", ErrProtocol)
	}
	if err := identity.ValidateTunnelID(success.GetTunnelId()); err != nil || success.GetTunnelId() != tunnelID {
		return fmt.Errorf("%w: tunnel identity mismatch", ErrProtocol)
	}
	if err := identity.ValidateSessionID(success.GetSessionId()); err != nil {
		return fmt.Errorf("%w: invalid session id", ErrProtocol)
	}
	if len(success.GetSessionSecret()) != len(Session{}.SessionSecret) {
		return fmt.Errorf("%w: invalid session secret length", ErrProtocol)
	}
	version := success.GetProtocolVersion()
	if version != protocolVersionV1 || version < minimum || version > maximum {
		return fmt.Errorf("%w: invalid negotiated protocol version", ErrProtocol)
	}
	if success.GetHeartbeatIntervalMs() == 0 {
		return fmt.Errorf("%w: invalid heartbeat interval", ErrProtocol)
	}
	return nil
}

func parseFailure(message *protocolv1.ConnectorAuthFailure) (*Failure, error) {
	if message == nil {
		return nil, fmt.Errorf("%w: missing auth failure", ErrProtocol)
	}

	class, valid := classifyFailureCode(message.GetErrorCode())
	if !valid {
		return nil, fmt.Errorf("%w: invalid auth failure code %d", ErrProtocol, message.GetErrorCode())
	}
	if class == FailurePermanent && message.GetRetryAfterMs() != 0 {
		return nil, fmt.Errorf("%w: permanent auth failure carries retry_after", ErrProtocol)
	}
	return &Failure{
		Code:       message.GetErrorCode(),
		Class:      class,
		RetryAfter: time.Duration(message.GetRetryAfterMs()) * time.Millisecond,
	}, nil
}

func classifyFailureCode(code protocolv1.ErrorCode) (FailureClass, bool) {
	switch code {
	case protocolv1.ErrorCode_ERROR_CODE_TOKEN_INVALID,
		protocolv1.ErrorCode_ERROR_CODE_TOKEN_REVOKED,
		protocolv1.ErrorCode_ERROR_CODE_TUNNEL_REVOKED,
		protocolv1.ErrorCode_ERROR_CODE_VERSION_UNSUPPORTED,
		protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR:
		return FailurePermanent, true
	case protocolv1.ErrorCode_ERROR_CODE_SESSION_RESOURCE_EXHAUSTED,
		protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR:
		return FailureRetryable, true
	default:
		return 0, false
	}
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

func classifyWriteError(err error) error {
	if errors.Is(err, frame.ErrFrameTooLarge) || errors.Is(err, frame.ErrNilMessage) {
		return fmt.Errorf("%w: auth request frame: %v", ErrInvalidInput, err)
	}
	return classifyIOError("write connector auth request", err, ErrWriteTimeout)
}

func classifyReadError(err error) error {
	classified := classifyIOError("read connector auth result", err, ErrReadTimeout)
	if errors.Is(classified, ErrReadTimeout) || errors.Is(classified, context.Canceled) ||
		errors.Is(classified, context.DeadlineExceeded) {
		return classified
	}
	if errors.Is(err, frame.ErrInvalidLength) || errors.Is(err, frame.ErrTruncatedFrame) ||
		errors.Is(err, frame.ErrFrameTooLarge) || errors.Is(err, frame.ErrMalformedMessage) {
		return fmt.Errorf("%w: auth result frame: %v", ErrProtocol, err)
	}
	return classified
}

// contextReader 通过短 Deadline 轮询 Context，使无 Deadline 的取消也能中断 Read。
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
		deadline := nextDeadline(reader.ctx, reader.deadline)
		if err := reader.conn.SetReadDeadline(deadline); err != nil {
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

// contextWriter 与 contextReader 使用相同边界，但只影响当前 AUTH 写入阶段。
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
		deadline := nextDeadline(writer.ctx, writer.deadline)
		if err := writer.conn.SetWriteDeadline(deadline); err != nil {
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
