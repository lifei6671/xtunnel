// Package httpingress implements the public HTTP data-plane entry point.
package httpingress

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/repository"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
	serveropen "github.com/lifei6671/xtunnel/internal/server/open"
	serverroute "github.com/lifei6671/xtunnel/internal/server/route"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
	serverworkpool "github.com/lifei6671/xtunnel/internal/server/workpool"
	"github.com/lifei6671/xtunnel/internal/tunnel"
)

const flushInterval = 100 * time.Millisecond

var (
	// ErrInvalidOptions 表示 HTTP Ingress 缺少 Route Snapshot 或 Tunnel Dialer。
	ErrInvalidOptions = errors.New("http ingress options are invalid")
)

// RouteSource 提供一次请求只能读取一次的不可变 Route Snapshot。
type RouteSource interface {
	Current() *serverroute.Snapshot
}

// TunnelDialer 把 http.Transport 的一条 HTTP/1.1 连接映射为一条 ACTIVE WorkConn。
// 成功返回后，连接生命周期由 Transport 接管，不能继续绑定到触发 Dial 的单个请求。
type TunnelDialer interface {
	Dial(ctx context.Context, request tunnel.DialRequest) (net.Conn, error)
}

// ServiceConfigObserver 只在 Tunnel Dial 失败后区分“新配置尚未观察”与普通服务
// 不可用。生产 Tunnel Proxy 实现该接口；测试或替代 Dialer 可以省略。
type ServiceConfigObserver interface {
	ServiceConfigObserved(tunnelID, serviceID string, requiredRevision int64) bool
}

// HandlerOptions 固定 HTTP Ingress 的 Route、Tunnel 与公网入口限额依赖。
type HandlerOptions struct {
	Routes         RouteSource
	Dialer         TunnelDialer
	TrustedProxies []string
	Limits         *serverlimits.Manager
	MaxBodyBytes   int64
}

// Handler 为每个请求执行一次严格 Route Match，再把原始 HTTP 字节流交给按
// Tunnel、Service 与 RequiredRevision 隔离的 Transport。它不读取完整 Body，
// Request/Response 的背压与取消由 net/http 和 Tunnel 连接共同传播。
type Handler struct {
	routes               RouteSource
	pools                *transportPool
	observer             ServiceConfigObserver
	trustedProxies       trustedProxySet
	limits               *serverlimits.Manager
	maxBodyBytes         int64
	webSocketIdleTimeout time.Duration
}

// NewHandler 创建尚未绑定 Listener 的 HTTP Ingress Handler。
func NewHandler(options HandlerOptions) (*Handler, error) {
	if options.Routes == nil || options.Dialer == nil || options.Limits == nil || options.MaxBodyBytes <= 0 {
		return nil, ErrInvalidOptions
	}
	proxySet, err := newTrustedProxySet(options.TrustedProxies)
	if err != nil {
		return nil, errors.Join(ErrInvalidOptions, err)
	}
	observer, _ := options.Dialer.(ServiceConfigObserver)
	return &Handler{
		routes: options.Routes, pools: newTransportPool(options.Dialer), observer: observer,
		trustedProxies: proxySet, limits: options.Limits, maxBodyBytes: options.MaxBodyBytes,
		webSocketIdleTimeout: webSocketIdleTimeout,
	}, nil
}

// ServeHTTP 只使用入口时取得的同一份 Snapshot，避免一次请求跨代读取 Route 与
// Transport 参数。新 Snapshot 会作用于后续请求，已经进入 Origin 的请求继续排空。
func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if handler == nil || handler.routes == nil || handler.pools == nil || handler.limits == nil ||
		handler.maxBodyBytes <= 0 || request == nil {
		writeError(writer, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE")
		return
	}
	forwarded, err := handler.trustedProxies.normalizeForwarded(request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "INVALID_FORWARDED_HEADER")
		return
	}
	if err := handler.limits.AllowHTTPRequest(forwarded.clientIP); err != nil {
		if errors.Is(err, serverlimits.ErrHTTPRequestRateExceeded) {
			writeRateLimited(writer)
			return
		}
		writeError(writer, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE")
		return
	}
	// 已知超限是所有 HTTP 请求共享的入口边界，优先于 Route 与 Upgrade 分类；
	// 因此超大 WebSocket 握手同样稳定返回 413，而不是后续的 501。
	if request.ContentLength > handler.maxBodyBytes {
		writeBodyTooLarge(writer, request)
		return
	}
	if request.Body != nil && request.Body != http.NoBody {
		// MaxBytesReader 只在 Origin/Transport 实际拉取 Body 时计数，既不缓冲完整上传，
		// 也不会破坏既有背压与取消链。超限后必须关闭客户端复用，避免剩余字节被
		// 当成同一连接上的下一条请求。
		request.Body = http.MaxBytesReader(writer, request.Body, handler.maxBodyBytes)
	}
	snapshot := handler.routes.Current()
	match, found, err := snapshot.MatchHTTP(request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "INVALID_PATH")
		return
	}
	if !found {
		writeError(writer, http.StatusNotFound, "ROUTE_NOT_FOUND")
		return
	}
	webSocket := isWebSocketUpgrade(request)
	if webSocket && (request.ContentLength != 0 ||
		(request.Body != nil && request.Body != http.NoBody) || len(request.TransferEncoding) != 0) {
		// WebSocket 握手没有 Request Body。若 Origin 在上传完成前返回 101，
		// http.Transport 的 Body writer 可能与升级后的双向复制同时写同一 WorkConn。
		// 因此必须在取得 ACTIVE Lease 和 Tunnel Dial 前拒绝这类请求，并关闭客户端
		// 复用，避免 net/http 为复用连接继续排空这个不受 ACTIVE Lease 保护的 Body。
		writer.Header().Set("Connection", "close")
		request.Close = true
		writeError(writer, http.StatusNotImplemented, "UPGRADE_NOT_SUPPORTED")
		return
	}
	if hasUpgradeSignal(request) && !webSocket {
		// V0.1 只开放 HTTP/1.1 WebSocket。h2c 或含糊的 Upgrade 仍沿用 M4-03
		// 已冻结的公开失败，不进入 Tunnel Dial，也不发明新的错误码。
		writeError(writer, http.StatusNotImplemented, "UPGRADE_NOT_SUPPORTED")
		return
	}
	if match.Route.ProxyOptions.DisableChunkedEncoding && requestLengthUnknown(request) {
		// 禁用 Chunked 时只接受入口已经可信声明的 Content-Length，或明确无 Body。
		// 为推导长度而读取 Body 会破坏 1GB Streaming 与取消语义，因此直接拒绝。
		writeError(writer, http.StatusBadRequest, "CONTENT_LENGTH_REQUIRED")
		return
	}
	activeLease, err := handler.limits.AcquireActive(serverlimits.ConnectionKey{
		TunnelID: match.Route.TunnelID, ServiceID: match.Route.ServiceID, SourceIP: forwarded.clientIP,
	})
	if err != nil {
		if errors.Is(err, serverlimits.ErrActiveConnectionCapacity) {
			writeError(writer, http.StatusServiceUnavailable, "WORK_POOL_EXHAUSTED")
			return
		}
		writeError(writer, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE")
		return
	}
	// 普通 HTTP 的 ACTIVE 是一次在途请求；WebSocket 的 Handler 在升级连接关闭前
	// 不会返回，因此同一个 Lease 也覆盖完整双向会话。KeepAlive WorkConn 可被下一
	// 个来源复用，但上一请求的 Source 配额会在这里及时归还。
	defer activeLease.Release()

	var transport http.RoundTripper
	proxyWriter := writer
	if webSocket {
		// Upgrade 使用 fresh、不可重试的单请求 Transport；普通请求继续使用按
		// Tunnel/Service/Revision 隔离的 HTTP/1.1 KeepAlive 池。
		idle := newWebSocketIdleOwner(handler.webSocketIdleTimeout)
		transport = handler.pools.webSocketTransport(match.Route, idle)
		proxyWriter = &webSocketResponseWriter{ResponseWriter: writer, idle: idle}
	} else {
		transport = handler.pools.transport(snapshot.Generation(), match.Route)
	}
	requestContext := context.WithValue(request.Context(), clientAddressKey{}, forwarded.clientIP.String())
	// net/http.Transport 允许请求取消后继续完成一条可能供后续请求复用的 Dial。
	// Tunnel OPEN 会占用 WorkConn/限额，不能沿用该默认；把原始请求 Context 作为
	// 不透明值传到 DialContext，使获取/OPEN 始终随触发它的请求取消。
	requestContext = context.WithValue(requestContext, dialRequestContextKey{}, requestContext)
	request = request.WithContext(requestContext)
	proxy := &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: flushInterval,
		// ReverseProxy 的默认日志器会把底层错误写到进程全局 stderr。数据面错误由
		// 统一结构化日志与指标任务接管前先静默处理，避免意外暴露内部错误文本。
		ErrorLog: log.New(io.Discard, "", 0),
		Rewrite: func(proxyRequest *httputil.ProxyRequest) {
			outbound := proxyRequest.Out
			inbound := proxyRequest.In
			// Agent 根据 service_id 负责 HTTP/HTTPS Origin TLS；Server 到 Agent 始终
			// 发送明文 HTTP 字节流，所以这里固定使用 http，并忽略 opaque authority。
			outbound.URL.Scheme = "http"
			outbound.URL.Host = transportAuthority
			outbound.URL.Path = inbound.URL.Path
			outbound.URL.RawPath = inbound.URL.RawPath
			outbound.URL.RawQuery = inbound.URL.RawQuery
			outbound.URL.ForceQuery = inbound.URL.ForceQuery
			outbound.Host = originHost(match.Route, inbound.Host)
			rewriteForwardedHeaders(outbound.Header, forwarded)
			if match.Route.ProxyOptions.DisableChunkedEncoding {
				outbound.TransferEncoding = nil
				if inbound.Body == nil || inbound.Body == http.NoBody {
					outbound.Body = http.NoBody
					outbound.ContentLength = 0
				}
			}
		},
		ErrorHandler: func(writer http.ResponseWriter, request *http.Request, err error) {
			var maxBytesError *http.MaxBytesError
			if errors.As(err, &maxBytesError) {
				writeBodyTooLarge(writer, request)
				return
			}
			if errors.Is(err, serverlimits.ErrOpenRateExceeded) ||
				errors.Is(err, serverlimits.ErrHTTPRequestRateExceeded) {
				writeRateLimited(writer)
				return
			}
			status, code := handler.proxyError(match.Route, err)
			writeError(writer, status, code)
		},
	}
	proxy.ServeHTTP(proxyWriter, request)
}

func hasUpgradeSignal(request *http.Request) bool {
	if httpguts.HeaderValuesContainsToken(request.Header.Values("Connection"), "upgrade") {
		return true
	}
	_, present, err := singleHeaderValue(request.Header, "Upgrade")
	return present || err != nil
}

func isWebSocketUpgrade(request *http.Request) bool {
	if request.ProtoMajor != 1 || request.ProtoMinor != 1 || request.Method != http.MethodGet {
		return false
	}
	if !httpguts.HeaderValuesContainsToken(request.Header.Values("Connection"), "upgrade") {
		return false
	}
	upgrade, present, err := singleHeaderValue(request.Header, "Upgrade")
	return err == nil && present && !strings.Contains(upgrade, ",") && strings.EqualFold(upgrade, "websocket")
}

// CloseIdleConnections 在停止入口或切换完整 Snapshot 时释放所有空闲 WorkConn。
// 正在处理请求的连接不在这里强关，由 http.Server 的排空 Deadline 决定。
func (handler *Handler) CloseIdleConnections() {
	if handler == nil || handler.pools == nil {
		return
	}
	handler.pools.closeIdleConnections()
}

func requestLengthUnknown(request *http.Request) bool {
	if request.Body == nil || request.Body == http.NoBody {
		return false
	}
	// 对 net/http 来说，非空 Body 配合 ContentLength==0 仍表示长度未知，并会在
	// 发送时自动切换为 chunked。只有正数 Content-Length 才是可转发的已知长度。
	if request.ContentLength <= 0 {
		return true
	}
	// Content-Length 与 Transfer-Encoding 同时存在时不能把长度视为可信；Go Server
	// 正常解析会消解该冲突，这个分支负责中间件/测试手工构造的请求边界。
	return len(request.TransferEncoding) != 0
}

// proxyError 把 Tunnel/OPEN 的类型化失败收敛为冻结 HTTP 契约。响应永远只包含
// 稳定公开码；底层地址、Token、连接 ID 和错误文本不能通过 ReverseProxy 泄漏。
func (handler *Handler) proxyError(route serverroute.HTTPRoute, err error) (int, string) {
	var rejected *serveropen.Rejected
	if errors.As(err, &rejected) {
		switch rejected.Code {
		case protocolv1.ErrorCode_ERROR_CODE_TUNNEL_OFFLINE:
			return http.StatusServiceUnavailable, "TUNNEL_OFFLINE"
		case protocolv1.ErrorCode_ERROR_CODE_ORIGIN_REFUSED:
			return http.StatusBadGateway, "ORIGIN_REFUSED"
		case protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TIMEOUT:
			return http.StatusGatewayTimeout, "ORIGIN_TIMEOUT"
		case protocolv1.ErrorCode_ERROR_CODE_WORK_POOL_EXHAUSTED:
			return http.StatusServiceUnavailable, "WORK_POOL_EXHAUSTED"
		case protocolv1.ErrorCode_ERROR_CODE_SERVICE_CONFIG_NOT_OBSERVED:
			return http.StatusServiceUnavailable, "SERVICE_CONFIG_NOT_OBSERVED"
		case protocolv1.ErrorCode_ERROR_CODE_SERVICE_DISABLED:
			return http.StatusServiceUnavailable, "SERVICE_DISABLED"
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
		return http.StatusServiceUnavailable, "WORK_POOL_EXHAUSTED"
	}
	// 只有没有更精确类型化错误时才读取观察门禁；否则配置刷新竞态可能覆盖 Agent
	// 已经明确返回的 Origin/Service 失败类别。
	if handler.observer != nil && !handler.observer.ServiceConfigObserved(
		route.TunnelID, route.ServiceID, route.RequiredRevision,
	) {
		return http.StatusServiceUnavailable, "SERVICE_CONFIG_NOT_OBSERVED"
	}
	if errors.Is(err, serverruntime.ErrNoAvailableConnector) {
		return http.StatusServiceUnavailable, "TUNNEL_OFFLINE"
	}
	return http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE"
}

func originHost(route serverroute.HTTPRoute, publicHost string) string {
	if route.OriginHTTPHost != "" {
		return route.OriginHTTPHost
	}
	if route.PreserveHost {
		return publicHost
	}
	if defaultOriginPort(route.OriginScheme) == route.OriginPort {
		if strings.Contains(route.OriginHost, ":") {
			return "[" + route.OriginHost + "]"
		}
		return route.OriginHost
	}
	return net.JoinHostPort(route.OriginHost, strconv.FormatUint(uint64(route.OriginPort), 10))
}

func defaultOriginPort(scheme repository.OriginScheme) uint16 {
	switch scheme {
	case repository.OriginSchemeHTTP:
		return 80
	case repository.OriginSchemeHTTPS:
		return 443
	default:
		return 0
	}
}

func writeError(writer http.ResponseWriter, status int, code string) {
	if writer == nil {
		return
	}
	http.Error(writer, code, status)
}

func writeRateLimited(writer http.ResponseWriter) {
	if writer == nil {
		return
	}
	writer.Header().Set("Retry-After", "1")
	writeError(writer, http.StatusTooManyRequests, "RATE_LIMITED")
}

func writeBodyTooLarge(writer http.ResponseWriter, request *http.Request) {
	if writer == nil {
		return
	}
	// 已知长度可在 Dial 前拒绝；未知长度则可能已向 Origin 发送前缀。两条路径都
	// 禁止复用客户端连接，以便 net/http 在响应后丢弃尚未消费的请求体字节。
	writer.Header().Set("Connection", "close")
	if request != nil {
		request.Close = true
	}
	writeError(writer, http.StatusRequestEntityTooLarge, "REQUEST_BODY_TOO_LARGE")
}
