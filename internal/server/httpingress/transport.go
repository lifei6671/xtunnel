package httpingress

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	serverroute "github.com/lifei6671/xtunnel/internal/server/route"
	"github.com/lifei6671/xtunnel/internal/tunnel"
)

const transportAuthority = "xtunnel.invalid"

type clientAddressKey struct{}
type dialRequestContextKey struct{}

// transportKey 不能只使用 TunnelID：KeepAlive 不会再次执行 DialContext，如果跨
// Service 或 RequiredRevision 共享 Transport，旧 Origin 连接会被新配置静默复用。
type transportKey struct {
	tunnelID         string
	serviceID        string
	requiredRevision int64
}

// transportPool 只拥有 http.Transport 与其 idle 连接；ACTIVE 请求由 Transport 自身
// 管理。Snapshot generation 前进时整体淘汰旧 idle 池，避免历史 revision 永久累积。
type transportPool struct {
	dialer TunnelDialer

	mu          sync.Mutex
	initialized bool
	generation  uint64
	transports  map[transportKey]*http.Transport
}

func newTransportPool(dialer TunnelDialer) *transportPool {
	return &transportPool{dialer: dialer, transports: make(map[transportKey]*http.Transport)}
}

// transport 只允许 generation 单调前进。较低代请求说明它在较早 Snapshot 上已经
// 完成匹配、但晚于新代请求来到池边界；它仍可使用旧 Route 完成本次请求，却不能
// 关闭、替换或写回当前池。该分支返回一次性 RoundTripper，在响应体结束后释放 idle
// 连接，避免旧 Route 对应的 WorkConn 脱离池 owner 后长期存活。
func (pool *transportPool) transport(generation uint64, route serverroute.HTTPRoute) http.RoundTripper {
	key := transportKey{
		tunnelID: route.TunnelID, serviceID: route.ServiceID,
		requiredRevision: route.RequiredRevision,
	}

	pool.mu.Lock()
	if pool.initialized && generation < pool.generation {
		pool.mu.Unlock()
		return &uncachedTransport{transport: pool.newTransport(route)}
	}

	var retired map[transportKey]*http.Transport
	if !pool.initialized {
		pool.initialized = true
		pool.generation = generation
	} else if generation > pool.generation {
		// 先在锁内原子发布新代空池，再在锁外关闭旧 idle 连接；因此新请求不会
		// 等待 Close，旧代连接也无法在释放锁后重新写回 transports。
		retired = pool.transports
		pool.transports = make(map[transportKey]*http.Transport)
		pool.generation = generation
	}

	if transport := pool.transports[key]; transport != nil {
		pool.mu.Unlock()
		closeIdleTransports(retired)
		return transport
	}
	transport := pool.newTransport(route)
	pool.transports[key] = transport
	pool.mu.Unlock()
	closeIdleTransports(retired)
	return transport
}

func (pool *transportPool) newTransport(route serverroute.HTTPRoute) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			dialContext := ctx
			if requestContext, ok := ctx.Value(dialRequestContextKey{}).(context.Context); ok {
				dialContext = requestContext
			}
			clientAddr, _ := dialContext.Value(clientAddressKey{}).(string)
			requestID := ""
			if observation := requestLogObservationFrom(dialContext); observation != nil {
				requestID = observation.requestID
			}
			return pool.dialer.Dial(dialContext, tunnel.DialRequest{
				TunnelID:         route.TunnelID,
				ServiceID:        route.ServiceID,
				RequiredRevision: uint64(route.RequiredRevision),
				Ingress:          protocolv1.IngressType_INGRESS_TYPE_HTTP,
				ClientAddr:       clientAddr,
				RequestID:        requestID,
			})
		},
		// Origin TLS 在 Agent 端完成，且每个隔离键只有一个虚拟 authority；禁用
		// 自动解压，避免 Reverse Proxy 在客户端未声明 Accept-Encoding 时改写响应。
		DisableCompression:  true,
		ForceAttemptHTTP2:   false,
		MaxIdleConns:        int(route.ProxyOptions.MaxIdleConnections),
		MaxIdleConnsPerHost: int(route.ProxyOptions.MaxIdleConnections),
		IdleConnTimeout:     time.Duration(route.ProxyOptions.IdleConnectionTimeoutMS) * time.Millisecond,
	}
}

type requestObservedTransport struct {
	next http.RoundTripper
}

// observeRequestConnections 用标准库 GotConn 观察本次请求最终取得的连接。
// 该回调对新 Dial 与 KeepAlive 复用都会触发，因此不会把后续请求错误关联到空连接。
func observeRequestConnections(next http.RoundTripper) http.RoundTripper {
	return &requestObservedTransport{next: next}
}

func (transport *requestObservedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if transport == nil || transport.next == nil || request == nil {
		return nil, ErrInvalidOptions
	}
	observation := requestLogObservationFrom(request.Context())
	if observation == nil {
		return transport.next.RoundTrip(request)
	}
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		identified, ok := info.Conn.(interface{ ConnectionID() string })
		if !ok {
			return
		}
		observation.observeConnection(identified.ConnectionID())
	}}
	request = request.Clone(httptrace.WithClientTrace(request.Context(), trace))
	return transport.next.RoundTrip(request)
}

// webSocketTransport 为一次 Upgrade 创建 fresh Transport。WebSocket 握手一旦写入
// WorkConn 就不能在另一条连接上静默重放；101 后该连接也已经脱离普通 KeepAlive
// 池，因此每次只允许一次请求，并用冻结的 Header 预算限制 Origin 握手等待。
func (pool *transportPool) webSocketTransport(
	route serverroute.HTTPRoute,
	idle *webSocketIdleOwner,
) http.RoundTripper {
	transport := pool.newTransport(route)
	dialContext := transport.DialContext
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		connection, err := dialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		wrapped := &webSocketActivityConn{Conn: connection, idle: idle}
		if err := idle.bindBackend(wrapped); err != nil {
			return nil, errors.Join(err, connection.Close())
		}
		return wrapped, nil
	}
	transport.DisableKeepAlives = true
	transport.ResponseHeaderTimeout = readHeaderTimeout
	return transport
}

func (pool *transportPool) closeIdleConnections() {
	pool.mu.Lock()
	transports := pool.transports
	pool.transports = make(map[transportKey]*http.Transport)
	pool.mu.Unlock()
	closeIdleTransports(transports)
}

func closeIdleTransports(transports map[transportKey]*http.Transport) {
	for _, transport := range transports {
		transport.CloseIdleConnections()
	}
}

// uncachedTransport 只服务一个落后 generation 的请求。RoundTrip 返回时响应体可能
// 仍在流式读取，因此不能立即关闭连接；等 Body 到达终态或调用方 Close 后再清理，
// 才能同时保证旧请求排空和旧 WorkConn 不进入长期 KeepAlive。
type uncachedTransport struct {
	transport *http.Transport
}

func (transport *uncachedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.transport.RoundTrip(request)
	if err != nil {
		transport.transport.CloseIdleConnections()
		return nil, err
	}
	response.Body = &closeIdleBody{
		ReadCloser: response.Body,
		closeIdle:  transport.transport.CloseIdleConnections,
	}
	return response, nil
}

// closeIdleBody 把响应流的终态转换为一次且仅一次的 idle 清理。Read 返回任意错误
// 或 Close 都表示本次响应不会再消费；sync.Once 防止 EOF 后 ReverseProxy 再 Close
// 时重复执行清理。
type closeIdleBody struct {
	io.ReadCloser
	closeIdle func()
	once      sync.Once
}

func (body *closeIdleBody) Read(buffer []byte) (int, error) {
	read, err := body.ReadCloser.Read(buffer)
	if err != nil {
		body.once.Do(body.closeIdle)
	}
	return read, err
}

func (body *closeIdleBody) Close() error {
	err := body.ReadCloser.Close()
	body.once.Do(body.closeIdle)
	return err
}
