// Package open 实现 Agent 侧 WorkConn 的 OPEN、Origin Dial 与 RAW 交接。
package open

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"strings"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/workauth"
	"github.com/lifei6671/xtunnel/internal/logging"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/state"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
	"github.com/lifei6671/xtunnel/internal/proxy"
	"github.com/lifei6671/xtunnel/internal/tracing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

var (
	// ErrInvalidOptions 表示 Handler 缺少 Origin Dialer、超时或 RAW 代理。
	ErrInvalidOptions = errors.New("agent open handler options are invalid")
	// ErrProtocol 表示 OpenRequest/状态违反冻结 Protocol v1。
	ErrProtocol = errors.New("agent open protocol violation")
	// ErrOrigin 表示 Origin 解析或连接失败，公开错误码由 Dialer 明确提供。
	ErrOrigin = errors.New("agent origin connection failed")
)

// OriginDialer 只按已验证 Snapshot 中的 service_id 解析并连接 Origin。
// 返回错误时 code 必须是非 OK 的公开 Origin/Service 错误码，禁止把地址放回协议。
type OriginDialer interface {
	DialOrigin(context.Context, string) (net.Conn, protocolv1.ErrorCode, error)
}

// OriginDialerFunc 是静态 M1 Fixture 与后续 Snapshot Resolver 的轻量适配器。
type OriginDialerFunc func(context.Context, string) (net.Conn, protocolv1.ErrorCode, error)

func (dial OriginDialerFunc) DialOrigin(ctx context.Context, serviceID string) (net.Conn, protocolv1.ErrorCode, error) {
	return dial(ctx, serviceID)
}

// RawProxy 在 OPEN_OK 完整写出后接管 WorkConn 与 Origin。
type RawProxy func(context.Context, net.Conn, net.Conn) error

// Options 固定 OPEN Frame、Origin Dial 与 RAW 交接边界。每个 Service 的连接超时
// 由 Snapshot OriginDialer 统一覆盖 DNS、TCP 与 TLS，不在 Handler 叠加固定上限。
type Options struct {
	// ReadTimeout 从首字节到达起限制 OpenRequest Frame，不限制 IDLE 等待。
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	Dialer       OriginDialer
	Proxy        RawProxy
	Logger       *slog.Logger
	Tracing      *tracing.Runtime
}

// Handler 处理一个已经通过 WorkHello、处于 IDLE 的 WorkConn。
type Handler struct {
	options    Options
	tracer     trace.Tracer
	propagator propagation.TextMapPropagator
}

// NewHandler 创建生产 OPEN Handler；Proxy 为空时使用统一双向 RAW 实现。
func NewHandler(options Options) (*Handler, error) {
	if options.ReadTimeout <= 0 || options.WriteTimeout <= 0 || !validDialer(options.Dialer) || options.Logger == nil {
		return nil, ErrInvalidOptions
	}
	if options.Proxy == nil {
		options.Proxy = proxy.ProxyBidirectional
	}
	provider := trace.NewNoopTracerProvider()
	propagator := propagation.TextMapPropagator(propagation.TraceContext{})
	if options.Tracing != nil {
		provider = options.Tracing.TracerProvider()
		propagator = options.Tracing.Propagator()
	}
	return &Handler{
		options: options, tracer: provider.Tracer("github.com/lifei6671/xtunnel/internal/agent/open"),
		propagator: propagator,
	}, nil
}

// Handle 读取唯一 OpenRequest，连接 service_id 对应 Origin，完整写出 OpenResponse，
// 然后进入 RAW。函数拥有两个连接并保证所有退出路径最终关闭它们。
// 未提供观察器时保持旧调用方兼容，由调用方保守地把整个 Handle 生命周期计作 IDLE。
func (handler *Handler) Handle(ctx context.Context, workConnection net.Conn, ready *workauth.Ready) error {
	return handler.handle(ctx, workConnection, ready, nil)
}

// HandleObserved 在与 Handle 相同的协议路径上，把 IDLE→OPENING 和
// OPENING→ACTIVE 的纯状态提交交给 transition 线性化。
//
// transition 必须同步执行 commit，且不得在回调返回后保存 commit；commit 只修改当前
// Work 状态，不做网络 IO。该闭包形状只依赖 protocol/state，因此 WorkPool 可以实现
// 可选观察而无需 open 包反向依赖 WorkPool。
func (handler *Handler) HandleObserved(
	ctx context.Context,
	workConnection net.Conn,
	ready *workauth.Ready,
	transition func(state.WorkPhase, func() error) error,
) error {
	if transition == nil {
		if workConnection != nil {
			_ = workConnection.Close()
		}
		return ErrInvalidOptions
	}
	return handler.handle(ctx, workConnection, ready, transition)
}

func (handler *Handler) handle(
	ctx context.Context,
	workConnection net.Conn,
	ready *workauth.Ready,
	transition func(state.WorkPhase, func() error) error,
) (resultErr error) {
	if handler == nil || ctx == nil || workConnection == nil || ready == nil || ready.State == nil ||
		ready.State.Phase() != state.WorkIdle {
		if workConnection != nil {
			_ = workConnection.Close()
		}
		return ErrInvalidOptions
	}
	connectionStartedAt := time.Now()
	connectionLogger := handler.options.Logger
	opened := false
	stage := "idle_wait"
	originFailureCode := ""
	defer func() {
		attributes := []any{"duration_ms", time.Since(connectionStartedAt).Milliseconds(), "stage", stage}
		if resultErr != nil {
			attributes = append(attributes, "error", logging.ErrorDetail(resultErr, "work connection failed"))
		}
		if opened {
			if resultErr == nil || errors.Is(resultErr, context.Canceled) {
				connectionLogger.InfoContext(ctx, logging.EventAgentConnectionClosed, attributes...)
				return
			}
			attributes = append(attributes, logging.ErrorCodeKey, "RAW_PROXY_FAILED")
			connectionLogger.WarnContext(ctx, logging.EventAgentConnectionClosed, attributes...)
			return
		}
		if resultErr == nil {
			return
		}
		if originFailureCode != "" {
			attributes = append(attributes, logging.ErrorCodeKey, originFailureCode)
			connectionLogger.WarnContext(ctx, logging.EventAgentOriginConnectionFailed, attributes...)
			return
		}
		if errors.Is(resultErr, context.Canceled) {
			attributes = append(attributes, logging.ErrorCodeKey, "CANCELED")
			connectionLogger.DebugContext(ctx, logging.EventAgentConnectionFailed, attributes...)
			return
		}
		var networkError net.Error
		if stage == "idle_wait" && !errors.Is(resultErr, ErrProtocol) && (errors.Is(resultErr, io.EOF) ||
			errors.Is(resultErr, net.ErrClosed) || errors.As(resultErr, &networkError)) {
			attributes = append(attributes, logging.ErrorCodeKey, "CONNECTION_CLOSED")
			connectionLogger.DebugContext(ctx, logging.EventAgentConnectionClosed, attributes...)
			return
		}
		code := "INTERNAL_ERROR"
		if errors.Is(resultErr, ErrProtocol) {
			code = "PROTOCOL_ERROR"
		}
		attributes = append(attributes, logging.ErrorCodeKey, code)
		connectionLogger.ErrorContext(ctx, logging.EventAgentConnectionFailed, attributes...)
	}()
	defer func() {
		ready.State.Close()
		if err := workConnection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			resultErr = errors.Join(resultErr, fmt.Errorf("close Agent WorkConn: %w", err))
		}
	}()
	// READY 后的 IDLE 可以长期等待业务，不属于 OPEN Frame 的读取预算。
	// 先按会话 Context 设置等待边界，再注册取消回调，避免清零覆盖已触发的取消。
	idleDeadline, _ := ctx.Deadline()
	if err := workConnection.SetReadDeadline(idleDeadline); err != nil {
		return fmt.Errorf("set idle WorkConn read deadline: %w", err)
	}
	contextIODone := make(chan struct{})
	stopContextIO := context.AfterFunc(ctx, func() {
		defer close(contextIODone)
		_ = workConnection.SetDeadline(time.Now())
	})
	defer func() {
		// 取消回调已开始时等待其结束，再交由外层关闭 WorkConn。
		if !stopContextIO() {
			<-contextIODone
		}
	}()
	var firstByte [1]byte
	if _, err := io.ReadFull(workConnection, firstByte[:]); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("wait for OpenRequest: %w", err)
	}
	stage = "open_request"
	// 首字节到达后才限制完整帧的剩余读取；只预读一个字节，不消费后续 RAW 数据。
	if err := workConnection.SetReadDeadline(operationDeadline(ctx, handler.options.ReadTimeout)); err != nil {
		return fmt.Errorf("set OpenRequest read deadline: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	request := &protocolv1.OpenRequest{}
	if err := frame.ReadWork(io.MultiReader(bytes.NewReader(firstByte[:]), workConnection), request); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: read OpenRequest: %w", ErrProtocol, err)
	}
	if err := validate.RejectUnknownFields(request); err != nil {
		return fmt.Errorf("%w: unknown fields", ErrProtocol)
	}
	if request.GetProtocolVersion() != 1 || request.GetIngressType() == protocolv1.IngressType_INGRESS_TYPE_UNSPECIFIED {
		return fmt.Errorf("%w: invalid OpenRequest fields", ErrProtocol)
	}
	commitOpening := func() error { return ready.State.AcceptInbound(request) }
	if err := commitTransition(transition, state.WorkOpening, commitOpening); err != nil {
		return fmt.Errorf("%w: accept OpenRequest: %v", ErrProtocol, err)
	}
	traceContext, err := handler.extractTraceContext(ctx, request)
	if err != nil {
		return err
	}
	ctx = traceContext
	connectionLogger = logging.WithCorrelationFields(handler.options.Logger, logging.Correlation{
		TraceID: tracing.TraceID(ctx), ServiceID: request.GetServiceId(), ConnectionID: request.GetConnectionId(),
	})

	stage = "origin_dial"
	originStartedAt := time.Now()
	originContext, originSpan := handler.tracer.Start(ctx, "origin.Dial")
	origin, code, dialErr := handler.options.Dialer.DialOrigin(originContext, request.GetServiceId())
	latency := time.Since(originStartedAt)
	if dialErr != nil {
		if code == protocolv1.ErrorCode_ERROR_CODE_OK {
			code = protocolv1.ErrorCode_ERROR_CODE_ORIGIN_UNREACHABLE
		}
		recordSpanFailure(originSpan, strings.TrimPrefix(code.String(), "ERROR_CODE_"))
		originSpan.End()
		response := &protocolv1.OpenResponse{
			ConnectionId: request.GetConnectionId(), Status: protocolv1.OpenStatus_OPEN_STATUS_ERROR,
			ErrorCode: code, OriginConnectLatencyMs: durationMilliseconds(latency),
		}
		if err := handler.writeResponse(ctx, workConnection, ready.State, response, transition); err != nil {
			originFailureCode = strings.TrimPrefix(code.String(), "ERROR_CODE_")
			return errors.Join(fmt.Errorf("%w: code=%s", ErrOrigin, code.String()), err)
		}
		originFailureCode = strings.TrimPrefix(code.String(), "ERROR_CODE_")
		return fmt.Errorf("%w: code=%s", ErrOrigin, code.String())
	}
	if origin == nil {
		recordSpanFailure(originSpan, "ORIGIN_UNREACHABLE")
		originSpan.End()
		return fmt.Errorf("%w: DialOrigin returned nil connection", ErrOrigin)
	}
	originSpan.End()
	defer func() {
		if err := origin.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			resultErr = errors.Join(resultErr, fmt.Errorf("close Agent Origin connection: %w", err))
		}
	}()

	response := &protocolv1.OpenResponse{
		ConnectionId: request.GetConnectionId(), Status: protocolv1.OpenStatus_OPEN_STATUS_OK,
		ErrorCode: protocolv1.ErrorCode_ERROR_CODE_OK, OriginConnectLatencyMs: durationMilliseconds(latency),
	}
	if err := handler.writeResponse(ctx, workConnection, ready.State, response, transition); err != nil {
		return err
	}
	if err := ready.State.AcceptRaw(); err != nil {
		return fmt.Errorf("%w: RAW handoff: %v", ErrProtocol, err)
	}
	if err := workConnection.SetDeadline(time.Time{}); err != nil {
		return fmt.Errorf("clear Agent WorkConn OPEN deadline: %w", err)
	}
	stage = "raw_proxy"
	opened = true
	connectionLogger.InfoContext(ctx, logging.EventAgentConnectionOpened,
		"origin_connect_latency_ms", durationMilliseconds(latency),
	)
	// frame.ReadWork 精确停在 OpenRequest Frame 边界；若同一次底层 Read 中已经到达
	// 后续 RAW 字节，它们仍留在 socket 中，由统一 Proxy 原样读取，不会丢失或重复。
	proxyContext, proxySpan := handler.tracer.Start(originContext, "proxy.Bidirectional")
	proxyErr := handler.options.Proxy(proxyContext, workConnection, origin)
	if proxyErr != nil && !errors.Is(proxyErr, context.Canceled) {
		recordSpanFailure(proxySpan, "INTERNAL_ERROR")
	}
	proxySpan.End()
	return proxyErr
}

// extractTraceContext 在 OPENING 提交后验证 Server 传来的唯一 Trace Context。
// 三个字段全空时保留调用链原 Context；否则 trace_id 与 traceparent
// 必须同时存在且指向同一 Trace，tracestate 也只能依附于合法 Parent。
func (handler *Handler) extractTraceContext(ctx context.Context, request *protocolv1.OpenRequest) (context.Context, error) {
	traceID := request.GetTraceId()
	traceparent := request.GetTraceparent()
	tracestate := request.GetTracestate()
	if traceID == "" && traceparent == "" && tracestate == "" {
		return ctx, nil
	}
	if traceID == "" || traceparent == "" {
		return nil, fmt.Errorf("%w: incomplete OpenRequest trace context", ErrProtocol)
	}
	if _, err := trace.ParseTraceState(tracestate); err != nil {
		return nil, fmt.Errorf("%w: invalid OpenRequest tracestate", ErrProtocol)
	}

	// 提取基底保留原 Context 的取消、Deadline 和值，但先清除可能存在的
	// 本地 SpanContext。否则非法 traceparent 被 Propagator 忽略时，不能误把
	// 调用方 Context 中的旧 Parent 当成本次 Wire 输入已验证。
	extractionBase := trace.ContextWithSpanContext(ctx, trace.SpanContext{})
	extracted := handler.propagator.Extract(extractionBase, propagation.MapCarrier{
		"traceparent": traceparent,
		"tracestate":  tracestate,
	})
	spanContext := trace.SpanContextFromContext(extracted)
	parsedTraceID, err := trace.TraceIDFromHex(traceID)
	if err != nil || !spanContext.IsValid() || !spanContext.IsRemote() || spanContext.TraceID() != parsedTraceID {
		return nil, fmt.Errorf("%w: invalid OpenRequest trace context", ErrProtocol)
	}
	return extracted, nil
}

func recordSpanFailure(span trace.Span, errorCode string) {
	span.SetAttributes(attribute.String(tracing.AttributeErrorCode, errorCode))
	span.SetStatus(codes.Error, "")
}

func (handler *Handler) writeResponse(
	ctx context.Context,
	connection net.Conn,
	workState *state.Work,
	response *protocolv1.OpenResponse,
	transition func(state.WorkPhase, func() error) error,
) error {
	if err := connection.SetWriteDeadline(operationDeadline(ctx, handler.options.WriteTimeout)); err != nil {
		return fmt.Errorf("set OpenResponse write deadline: %w", err)
	}
	if err := frame.WriteWork(connection, response); err != nil {
		return fmt.Errorf("write OpenResponse: %w", err)
	}
	// 完整 Frame flush 才提交 OPENING→ACTIVE/CLOSED；半写绝不进入 RAW。
	commit := func() error { return workState.AcceptOutbound(response) }
	var err error
	if response.GetStatus() == protocolv1.OpenStatus_OPEN_STATUS_OK {
		err = commitTransition(transition, state.WorkActive, commit)
	} else {
		// 失败响应进入 CLOSED，不参与 WorkPool 的普通 Demand 目标；Handler 返回后由
		// Pool 唯一终止路径删除仍登记为 OPENING 的项即可。
		err = commit()
	}
	if err != nil {
		return fmt.Errorf("%w: commit OpenResponse: %v", ErrProtocol, err)
	}
	return nil
}

func commitTransition(
	transition func(state.WorkPhase, func() error) error,
	phase state.WorkPhase,
	commit func() error,
) error {
	if transition == nil {
		return commit()
	}
	return transition(phase, commit)
}

func operationDeadline(ctx context.Context, timeout time.Duration) time.Time {
	deadline := time.Now().Add(timeout)
	if contextDeadline, exists := ctx.Deadline(); exists && contextDeadline.Before(deadline) {
		return contextDeadline
	}
	return deadline
}

func durationMilliseconds(duration time.Duration) uint32 {
	if duration <= 0 {
		return 0
	}
	milliseconds := duration / time.Millisecond
	if milliseconds > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(milliseconds)
}

func validDialer(dialer OriginDialer) bool {
	if dialer == nil {
		return false
	}
	if function, ok := dialer.(OriginDialerFunc); ok {
		return function != nil
	}
	return true
}
