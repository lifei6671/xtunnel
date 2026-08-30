package metrics

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
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 90 * time.Second
)

// ServerOptions 是独立 Prometheus Listener 的固定生产依赖。
type ServerOptions struct {
	Listen             string
	Path               string
	Registry           *Registry
	ReportRuntimeError func(error)
}

// Server 拥有 Metrics Listener、Serve owner 和全部已准入抓取请求。
// StopAccepting 先关闭准入 Fence；Shutdown 在调用方 Deadline 内排空现有抓取。
type Server struct {
	options  ServerOptions
	requests *requestTracker

	mu       sync.Mutex
	server   *http.Server
	listener net.Listener
	cancel   context.CancelFunc
	wait     sync.WaitGroup
	started  bool
	stopped  bool

	stopOnce     sync.Once
	shutdownOnce sync.Once
	closeOnce    sync.Once
	stopErr      error
	shutdownErr  error
	closeErr     error
}

// NewServer 创建尚未绑定端口的独立 Metrics Server。
func NewServer(options ServerOptions) (*Server, error) {
	if options.Listen == "" || options.Path == "" || options.Path[0] != '/' || options.Registry == nil {
		return nil, errInvalidOptions
	}
	metricsHandler := promhttp.HandlerFor(options.Registry.registry, promhttp.HandlerOpts{})
	mux := http.NewServeMux()
	if err := registerExactPath(mux, options.Path, metricsHandler); err != nil {
		return nil, err
	}
	// ServeMux 会在路由前规范化路径并产生重定向；外层 Fence 先做逐字节 Path
	// 判断，确保任何非配置路径都稳定返回 404，而不是被清理后命中指标端点。
	exactHandler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != options.Path {
			http.NotFound(writer, request)
			return
		}
		mux.ServeHTTP(writer, request)
	})
	return &Server{options: options, requests: newRequestTracker(exactHandler)}, nil
}

// Start 绑定独立 Metrics Listener 并启动唯一 Serve owner；重复启动快速失败。
func (server *Server) Start(parent context.Context) error {
	if server == nil || parent == nil {
		return errInvalidOptions
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.started {
		return errors.New("metrics server has already started")
	}
	if server.stopped {
		return errors.New("metrics server has already stopped")
	}
	listener, err := net.Listen("tcp", server.options.Listen)
	if err != nil {
		return fmt.Errorf("listen for Prometheus metrics: %w", err)
	}
	baseContext, cancel := context.WithCancel(parent)
	httpServer := &http.Server{
		Handler:           server.requests,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          log.New(io.Discard, "", 0),
		BaseContext: func(net.Listener) context.Context {
			return baseContext
		},
	}
	server.server = httpServer
	server.listener = listener
	server.cancel = cancel
	server.started = true
	server.wait.Add(1)
	safego.Go(server.handlePanic, server.wait.Done, func() {
		serveErr := httpServer.Serve(listener)
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) && !errors.Is(serveErr, net.ErrClosed) {
			server.reportRuntimeError(fmt.Errorf("serve Prometheus metrics: %w", serveErr))
		}
	})
	return nil
}

// Addr 返回已启动 Listener 的实际地址；未启动时返回 nil。
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

// StopAccepting 关闭新连接和 KeepAlive 请求准入，但保留已进入 Collector 的抓取。
func (server *Server) StopAccepting() error {
	if server == nil {
		return errInvalidOptions
	}
	server.stopOnce.Do(func() {
		server.requests.stopAccepting()
		server.mu.Lock()
		server.stopped = true
		listener := server.listener
		server.mu.Unlock()
		if listener != nil {
			server.stopErr = normalizeClosedError(listener.Close())
		}
	})
	return server.stopErr
}

// Shutdown 在调用方 Deadline 内排空已准入抓取；Deadline 到期后主动关闭连接，
// 并等待 Serve owner 与请求 owner 全部退出。
func (server *Server) Shutdown(ctx context.Context) error {
	if server == nil || ctx == nil {
		return errInvalidOptions
	}
	server.shutdownOnce.Do(func() {
		stopErr := server.StopAccepting()
		server.mu.Lock()
		httpServer := server.server
		cancel := server.cancel
		server.mu.Unlock()
		if httpServer == nil {
			server.shutdownErr = stopErr
			return
		}
		drainErr := normalizeClosedError(httpServer.Shutdown(ctx))
		if drainErr == nil {
			drainErr = server.requests.waitContext(ctx)
		}
		if drainErr != nil {
			if cancel != nil {
				cancel()
			}
			drainErr = errors.Join(drainErr, normalizeClosedError(httpServer.Close()))
		}
		server.wait.Wait()
		server.requests.wait()
		server.shutdownErr = errors.Join(stopErr, drainErr)
	})
	return server.shutdownErr
}

// Close 立即取消抓取 Context、关闭 Listener/连接并等待全部 owner 退出。
func (server *Server) Close() error {
	if server == nil {
		return errInvalidOptions
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
	server.reportRuntimeError(fmt.Errorf("metrics serve owner: %w", err))
}

func (server *Server) reportRuntimeError(err error) {
	if err != nil && server.options.ReportRuntimeError != nil {
		server.options.ReportRuntimeError(err)
	}
}

func registerExactPath(mux *http.ServeMux, path string, handler http.Handler) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("invalid metrics path %q: %v", path, recovered)
		}
	}()
	mux.Handle(path, handler)
	return nil
}

func normalizeClosedError(err error) error {
	if errors.Is(err, net.ErrClosed) || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

type requestTracker struct {
	next http.Handler

	mu        sync.Mutex
	accepting bool
	active    uint64
	drained   chan struct{}
}

func newRequestTracker(next http.Handler) *requestTracker {
	return &requestTracker{next: next, accepting: true, drained: make(chan struct{})}
}

func (tracker *requestTracker) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if !tracker.admit() {
		writer.Header().Set("Connection", "close")
		http.Error(writer, "server draining", http.StatusServiceUnavailable)
		return
	}
	defer tracker.finish()
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
	tracker.closeDrainedLocked()
}

func (tracker *requestTracker) stopAccepting() {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.accepting = false
	tracker.closeDrainedLocked()
}

func (tracker *requestTracker) waitContext(ctx context.Context) error {
	select {
	case <-tracker.drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (tracker *requestTracker) wait() {
	<-tracker.drained
}

func (tracker *requestTracker) closeDrainedLocked() {
	if !tracker.accepting && tracker.active == 0 {
		select {
		case <-tracker.drained:
		default:
			close(tracker.drained)
		}
	}
}
