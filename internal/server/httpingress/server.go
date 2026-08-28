package httpingress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/lifei6671/xtunnel/internal/safego"
)

const readHeaderTimeout = 10 * time.Second

const requestBodyIdleTimeout = 60 * time.Second

type connectionContextKey struct{}

// ServerOptions 是 HTTP Ingress Listener 的固定生产依赖。
type ServerOptions struct {
	Listen             string
	Handler            http.Handler
	MaxHeaderBytes     int
	ReportRuntimeError func(error)

	// bodyIdleTimeout 只供包内真实 TCP 测试缩短冻结的 60s 窗口；生产零值固定使用
	// requestBodyIdleTimeout，不新增用户配置或第二套默认值。
	bodyIdleTimeout time.Duration
}

// Server 拥有 HTTP Listener、Serve owner 与全部被 net/http 跟踪的连接。
// StopAccepting 只关闭新入口；Shutdown 先排空请求，Deadline 到期后再强关残留连接。
type Server struct {
	options ServerOptions

	mu       sync.Mutex
	server   *http.Server
	listener net.Listener
	cancel   context.CancelFunc
	wait     sync.WaitGroup
	started  bool
	requests *requestTracker

	stopOnce     sync.Once
	shutdownOnce sync.Once
	closeOnce    sync.Once
	stopErr      error
	shutdownErr  error
	closeErr     error
}

// NewServer 创建尚未监听的 HTTP Ingress Server。
func NewServer(options ServerOptions) (*Server, error) {
	if options.Listen == "" || options.Handler == nil || options.MaxHeaderBytes <= 0 {
		return nil, ErrInvalidOptions
	}
	bodyIdleTimeout := options.bodyIdleTimeout
	if bodyIdleTimeout <= 0 {
		bodyIdleTimeout = requestBodyIdleTimeout
	}
	return &Server{
		options:  options,
		requests: newRequestTracker(options.Handler, bodyIdleTimeout),
	}, nil
}

// Start 绑定 Listener 并启动唯一 Serve owner；重复启动会快速失败。
func (server *Server) Start(parent context.Context) error {
	if server == nil || parent == nil {
		return ErrInvalidOptions
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.started {
		return errors.New("http ingress server has already started")
	}
	listener, err := net.Listen("tcp", server.options.Listen)
	if err != nil {
		return fmt.Errorf("listen for HTTP ingress: %w", err)
	}
	baseContext, cancel := context.WithCancel(parent)
	httpServer := &http.Server{
		Handler:           server.requests,
		ReadHeaderTimeout: readHeaderTimeout,
		MaxHeaderBytes:    server.options.MaxHeaderBytes,
		ErrorLog:          log.New(io.Discard, "", 0),
		BaseContext: func(net.Listener) context.Context {
			return baseContext
		},
		ConnContext: func(ctx context.Context, connection net.Conn) context.Context {
			return context.WithValue(ctx, connectionContextKey{}, connection)
		},
	}
	server.server = httpServer
	server.listener = listener
	server.cancel = cancel
	server.started = true
	server.wait.Add(1)
	safego.Go(server.handlePanic, server.wait.Done, func() {
		err := httpServer.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			server.reportRuntimeError(fmt.Errorf("serve HTTP ingress: %w", err))
		}
	})
	return nil
}

// Addr 返回已启动 Listener 的地址；未启动时返回 nil。
func (server *Server) Addr() net.Addr {
	if server == nil {
		return nil
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.listener == nil {
		return nil
	}
	return server.listener.Addr()
}

// StopAccepting 只解除 Listener.Accept，不取消已经进入 Handler 的请求。
func (server *Server) StopAccepting() error {
	if server == nil {
		return ErrInvalidOptions
	}
	server.stopOnce.Do(func() {
		// admission fence 必须先于 Listener.Close：已经建立的 KeepAlive 连接也
		// 不能在停止入口后再把下一条请求加入 Handler owner。
		server.requests.stopAccepting()
		server.mu.Lock()
		listener := server.listener
		server.mu.Unlock()
		if listener != nil {
			server.stopErr = listener.Close()
		}
	})
	return normalizeClosedError(server.stopErr)
}

// Shutdown 在调用方 Deadline 内等待 ACTIVE Handler 完成；超时后调用 Close 主动
// 解除 socket IO 并等待 Serve owner 退出，不能只取消 Context 留下阻塞连接。
func (server *Server) Shutdown(ctx context.Context) error {
	if server == nil || ctx == nil {
		return ErrInvalidOptions
	}
	server.shutdownOnce.Do(func() {
		stopErr := server.StopAccepting()
		server.mu.Lock()
		httpServer := server.server
		server.mu.Unlock()
		if httpServer == nil {
			server.shutdownErr = stopErr
			return
		}
		drainErr := httpServer.Shutdown(ctx)
		if drainErr != nil {
			drainErr = errors.Join(drainErr, normalizeClosedError(httpServer.Close()))
		}
		server.wait.Wait()
		// http.Server.Close 只关闭连接，不保证 Handler 已返回。请求 owner 在 socket
		// 强关后继续等待全部已准入 Handler，Route/SQLite 才能安全进入后续关闭。
		server.requests.wait()
		server.shutdownErr = errors.Join(stopErr, drainErr)
	})
	return server.shutdownErr
}

// Close 立即取消基础 Context、关闭 Listener/连接并等待 Serve owner。它用于启动
// 回滚和 Shutdown Deadline 后的强制收敛，不替代正常的 Shutdown 排空路径。
func (server *Server) Close() error {
	if server == nil {
		return ErrInvalidOptions
	}
	server.closeOnce.Do(func() {
		stopErr := server.StopAccepting()
		server.mu.Lock()
		httpServer := server.server
		cancel := server.cancel
		server.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		var closeErr error
		if httpServer != nil {
			closeErr = normalizeClosedError(httpServer.Close())
		}
		server.wait.Wait()
		server.requests.wait()
		server.closeErr = errors.Join(stopErr, closeErr)
	})
	return server.closeErr
}

func (server *Server) handlePanic(err error) {
	server.reportRuntimeError(fmt.Errorf("HTTP ingress serve owner: %w", err))
}

func (server *Server) reportRuntimeError(err error) {
	if err != nil && server.options.ReportRuntimeError != nil {
		server.options.ReportRuntimeError(err)
	}
}

func normalizeClosedError(err error) error {
	if errors.Is(err, net.ErrClosed) || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// requestTracker 在 HTTP Listener 之外提供第二道 admission fence，并拥有全部已
// 进入业务 Handler 的请求。stopAccepting 与 admit 共用一把锁，因此开始 Wait 后
// 不再发生 WaitGroup.Add/Wait 竞态；drained 只在 fence 已关闭且 active 归零时关闭。
type requestTracker struct {
	next            http.Handler
	bodyIdleTimeout time.Duration

	mu        sync.Mutex
	accepting bool
	active    uint64
	drained   chan struct{}
}

func newRequestTracker(next http.Handler, bodyIdleTimeout time.Duration) *requestTracker {
	return &requestTracker{
		next: next, bodyIdleTimeout: bodyIdleTimeout,
		accepting: true, drained: make(chan struct{}),
	}
}

func (tracker *requestTracker) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if !tracker.admit() {
		writer.Header().Set("Connection", "close")
		writeError(writer, http.StatusServiceUnavailable, "SERVER_DRAINING")
		return
	}
	defer tracker.finish()
	if request.Body != nil && request.Body != http.NoBody {
		if connection, ok := request.Context().Value(connectionContextKey{}).(net.Conn); ok {
			request.Body = &idleRequestBody{
				ReadCloser: request.Body, connection: connection, timeout: tracker.bodyIdleTimeout,
			}
		}
	}
	tracker.next.ServeHTTP(writer, request)
}

func (tracker *requestTracker) admit() bool {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if !tracker.accepting {
		return false
	}
	tracker.active++
	return true
}

func (tracker *requestTracker) finish() {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.active--
	if !tracker.accepting && tracker.active == 0 {
		close(tracker.drained)
	}
}

func (tracker *requestTracker) stopAccepting() {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if !tracker.accepting {
		return
	}
	tracker.accepting = false
	if tracker.active == 0 {
		close(tracker.drained)
	}
}

func (tracker *requestTracker) wait() {
	<-tracker.drained
}

// idleRequestBody 只在调用方实际读取 Body 时设置 ReadDeadline；一次 Read 返回后
// 立即清除，Origin 背压或业务暂不读取不会被误判为客户端空闲。下一次 Read 重新
// 推进 60s 窗口，从而对慢速上传形成 sliding timeout，且不设置完整请求总超时。
type idleRequestBody struct {
	io.ReadCloser
	connection net.Conn
	timeout    time.Duration
}

func (body *idleRequestBody) Read(buffer []byte) (int, error) {
	if err := body.connection.SetReadDeadline(time.Now().Add(body.timeout)); err != nil {
		return 0, err
	}
	count, readErr := body.ReadCloser.Read(buffer)
	clearErr := body.connection.SetReadDeadline(time.Time{})
	if clearErr != nil {
		return count, errors.Join(readErr, clearErr)
	}
	return count, readErr
}

func (body *idleRequestBody) Close() error {
	clearErr := body.connection.SetReadDeadline(time.Time{})
	closeErr := body.ReadCloser.Close()
	return errors.Join(clearErr, closeErr)
}
