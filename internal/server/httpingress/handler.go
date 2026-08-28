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

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/repository"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
	serveropen "github.com/lifei6671/xtunnel/internal/server/open"
	serverroute "github.com/lifei6671/xtunnel/internal/server/route"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
	serverworkpool "github.com/lifei6671/xtunnel/internal/server/workpool"
	"golang.org/x/net/http/httpguts"
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
	Dial(
		ctx context.Context,
		tunnelID, serviceID string,
		requiredRevision uint64,
		ingress protocolv1.IngressType,
		clientAddr string,
	) (net.Conn, error)
}

// ServiceConfigObserver 只在 Tunnel Dial 失败后区分“新配置尚未观察”与普通服务
// 不可用。生产 Tunnel Proxy 实现该接口；测试或替代 Dialer 可以省略。
type ServiceConfigObserver interface {
	ServiceConfigObserved(tunnelID, serviceID string, requiredRevision int64) bool
}

// Handler 为每个请求执行一次严格 Route Match，再把原始 HTTP 字节流交给按
// Tunnel、Service 与 RequiredRevision 隔离的 Transport。它不读取完整 Body，
// Request/Response 的背压与取消由 net/http 和 Tunnel 连接共同传播。
type Handler struct {
	routes   RouteSource
	pools    *transportPool
	observer ServiceConfigObserver
}

// NewHandler 创建尚未绑定 Listener 的 HTTP Ingress Handler。
func NewHandler(routes RouteSource, dialer TunnelDialer) (*Handler, error) {
	if routes == nil || dialer == nil {
		return nil, ErrInvalidOptions
	}
	observer, _ := dialer.(ServiceConfigObserver)
	return &Handler{routes: routes, pools: newTransportPool(dialer), observer: observer}, nil
}

// ServeHTTP 只使用入口时取得的同一份 Snapshot，避免一次请求跨代读取 Route 与
// Transport 参数。新 Snapshot 会作用于后续请求，已经进入 Origin 的请求继续排空。
func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if handler == nil || handler.routes == nil || handler.pools == nil || request == nil {
		writeError(writer, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE")
		return
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
	if httpguts.HeaderValuesContainsToken(request.Header.Values("Connection"), "upgrade") ||
		strings.TrimSpace(request.Header.Get("Upgrade")) != "" {
		// M4-05 接管 Upgrade 的双向流生命周期前必须快速失败，不能让 ReverseProxy
		// Hijack 一条不受当前 HTTP Server Shutdown/Drain 所有权约束的连接。
		writeError(writer, http.StatusNotImplemented, "UPGRADE_NOT_SUPPORTED")
		return
	}
	if match.Route.ProxyOptions.DisableChunkedEncoding && requestLengthUnknown(request) {
		// 禁用 Chunked 时只接受入口已经可信声明的 Content-Length，或明确无 Body。
		// 为推导长度而读取 Body 会破坏 1GB Streaming 与取消语义，因此直接拒绝。
		writeError(writer, http.StatusBadRequest, "CONTENT_LENGTH_REQUIRED")
		return
	}

	transport := handler.pools.transport(snapshot.Generation(), match.Route)
	requestContext := context.WithValue(request.Context(), clientAddressKey{}, request.RemoteAddr)
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
			if match.Route.ProxyOptions.DisableChunkedEncoding {
				outbound.TransferEncoding = nil
				if inbound.Body == nil || inbound.Body == http.NoBody {
					outbound.Body = http.NoBody
					outbound.ContentLength = 0
				}
			}
			// Rewrite 会先删除外部 Forwarded/X-Forwarded Header。本阶段故意不调用
			// SetXForwarded；M4-04 将在可信代理解析完成后从受信元数据重新生成。
		},
		ErrorHandler: func(writer http.ResponseWriter, request *http.Request, err error) {
			status, code := handler.proxyError(match.Route, err)
			writeError(writer, status, code)
		},
	}
	proxy.ServeHTTP(writer, request)
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
