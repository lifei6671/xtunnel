package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"

	"github.com/lifei6671/xtunnel/internal/logging"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
	serveropen "github.com/lifei6671/xtunnel/internal/server/open"
	serverroute "github.com/lifei6671/xtunnel/internal/server/route"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
	servertcpingress "github.com/lifei6671/xtunnel/internal/server/tcpingress"
	serverworkpool "github.com/lifei6671/xtunnel/internal/server/workpool"
	internaltracing "github.com/lifei6671/xtunnel/internal/tracing"
	"github.com/lifei6671/xtunnel/internal/tunnel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var errInvalidTCPIngressConnection = errors.New("TCP ingress connection is invalid")

// tcpTunnelProxy 是生产 TCP Listener 到 Tunnel OPEN/RAW 生命周期的最小边界。
// Tunnel Proxy 接管公网 Peer，使 Revoke、Drain 与 Manager Cancel 都能主动解阻 IO。
type tcpTunnelProxy interface {
	Serve(context.Context, tunnel.DialRequest, net.Conn) error
	ServiceConfigObserved(string, string, int64) bool
}

func newTCPIngressHandler(
	dialer tcpTunnelProxy,
	logger *slog.Logger,
	traceRuntime *internaltracing.Runtime,
) (servertcpingress.Handler, error) {
	if dialer == nil || logger == nil {
		return nil, errInvalidTCPIngressConnection
	}
	return func(ctx context.Context, peer net.Conn, route serverroute.TCPRoute) {
		if traceRuntime != nil && ctx != nil {
			var ingressSpan trace.Span
			ctx, ingressSpan = traceRuntime.Tracer("xtunnel/server/tcpingress").Start(
				ctx, "ingress.Accept", trace.WithNewRoot(),
			)
			defer ingressSpan.End()
		}
		if err := serveTCPRoute(ctx, dialer, peer, route); err != nil {
			// Raw TCP 没有安全的带内错误响应；Manager 会在 Handler 返回后
			// 关闭公网连接。日志只记录 Proto 稳定码与路由标识，底层错误可能
			// 包含 Origin 地址，不得记录其文本。进程排空取消是预期收敛，不记为单连接失败。
			if ctx != nil && ctx.Err() != nil && errors.Is(err, ctx.Err()) {
				return
			}
			code := tcpIngressErrorCode(dialer, route, err)
			if span := trace.SpanFromContext(ctx); span.IsRecording() {
				span.SetAttributes(
					attribute.String(internaltracing.AttributeErrorCode, code),
					attribute.Int("server.port", int(route.PublicPort)),
				)
				span.SetStatus(codes.Error, code)
			}
			correlatedLogger := logging.WithCorrelationFields(logger, logging.Correlation{
				TraceID: internaltracing.TraceID(ctx),
			})
			correlatedLogger.WarnContext(
				ctx,
				"tcp_ingress_connection_failed",
				"error_code", code,
				"tunnel_id", route.TunnelID,
				"service_id", route.ServiceID,
				"public_port", route.PublicPort,
			)
			return
		}
	}, nil
}

// tcpIngressErrorCode 与 HTTP Ingress 共享同一组已冻结失败语义。已认证
// Agent 返回的 Proto 码优先级最高；本地容量和可见性只在没有更精确码时分类。
func tcpIngressErrorCode(dialer tcpTunnelProxy, route serverroute.TCPRoute, err error) string {
	var rejected *serveropen.Rejected
	if errors.As(err, &rejected) {
		name, known := protocolv1.ErrorCode_name[int32(rejected.Code)]
		if known && rejected.Code != protocolv1.ErrorCode_ERROR_CODE_OK {
			return strings.TrimPrefix(name, "ERROR_CODE_")
		}
	}
	if errors.Is(err, serverlimits.ErrPendingOpenCapacity) ||
		errors.Is(err, serverlimits.ErrActiveConnectionCapacity) ||
		errors.Is(err, serverlimits.ErrWorkCapacity) ||
		errors.Is(err, serverlimits.ErrConnectingWorkCapacity) ||
		errors.Is(err, serverlimits.ErrIdleWorkCapacity) ||
		errors.Is(err, serverworkpool.ErrAcquireTimeout) ||
		errors.Is(err, serverworkpool.ErrPoolCapacity) ||
		errors.Is(err, serverworkpool.ErrConnectingCapacity) {
		return "WORK_POOL_EXHAUSTED"
	}
	if errors.Is(err, serverruntime.ErrNoAvailableConnector) {
		if !dialer.ServiceConfigObserved(route.TunnelID, route.ServiceID, route.RequiredRevision) {
			return "SERVICE_CONFIG_NOT_OBSERVED"
		}
		return "TUNNEL_OFFLINE"
	}
	if errors.Is(err, serveropen.ErrProtocol) {
		return "PROTOCOL_ERROR"
	}
	return "INTERNAL_ERROR"
}

// serveTCPRoute 把 Route 的精确 Revision 和公网 Peer 一次性交给 Tunnel Proxy。
// Proxy 内部用有界 Pre-OPEN Context 建立 WorkConn，进入 RAW 后则回到
// Manager Context；因此 OPEN 超时不会误伤长连接，Revoke 也能关闭两端。
func serveTCPRoute(
	ctx context.Context,
	dialer tcpTunnelProxy,
	peer net.Conn,
	route serverroute.TCPRoute,
) error {
	if ctx == nil || dialer == nil || peer == nil || peer.RemoteAddr() == nil || route.RequiredRevision < 0 {
		return errInvalidTCPIngressConnection
	}
	return dialer.Serve(ctx, tunnel.DialRequest{
		TunnelID:         route.TunnelID,
		ServiceID:        route.ServiceID,
		RequiredRevision: uint64(route.RequiredRevision),
		Ingress:          protocolv1.IngressType_INGRESS_TYPE_TCP,
	}, peer)
}
