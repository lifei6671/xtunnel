package managementapi

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

const (
	managementReadHeaderTimeout = 10 * time.Second
	managementReadTimeout       = 30 * time.Second
	managementWriteTimeout      = 30 * time.Second
	managementIdleTimeout       = 90 * time.Second
)

var errInvalidServerOptions = errors.New("invalid management server options")

// ServerOptions 是 Management Listener 的固定生产依赖。
type ServerOptions struct {
	Listen             string
	Handler            http.Handler
	MaxHeaderBytes     int
	ReportRuntimeError func(error)

	readTimeout  time.Duration
	writeTimeout time.Duration
	idleTimeout  time.Duration
}

// Server 拥有 Management Listener、net/http Server 和唯一 Serve goroutine。
// Management 不允许 Hijack；Shutdown 排空所有已经准入的 HTTP 请求后才返回。
type Server struct {
	options  ServerOptions
	requests *managementRequestTracker

	mu       sync.Mutex
	server   *http.Server
	listener net.Listener
	cancel   context.CancelFunc
	wait     sync.WaitGroup
	started  bool

	stopOnce     sync.Once
	shutdownOnce sync.Once
	closeOnce    sync.Once
	stopErr      error
	shutdownErr  error
	closeErr     error
}

// NewServer 创建尚未绑定端口的 Management Server。
func NewServer(options ServerOptions) (*Server, error) {
	if options.Listen == "" || options.Handler == nil || options.MaxHeaderBytes <= 0 {
		return nil, errInvalidServerOptions
	}
	return &Server{options: options, requests: newManagementRequestTracker(options.Handler)}, nil
}

// Start 绑定 Management Listener，并启动唯一受保护的 Serve owner。
func (server *Server) Start(parent context.Context) error {
	if server == nil || parent == nil {
		return errInvalidServerOptions
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.started {
		return errors.New("management server has already started")
	}
	listener, err := net.Listen("tcp", server.options.Listen)
	if err != nil {
		return fmt.Errorf("listen for management API: %w", err)
	}
	baseContext, cancel := context.WithCancel(parent)
	readTimeout := server.options.readTimeout
	if readTimeout <= 0 {
		readTimeout = managementReadTimeout
	}
	writeTimeout := server.options.writeTimeout
	if writeTimeout <= 0 {
		writeTimeout = managementWriteTimeout
	}
	idleTimeout := server.options.idleTimeout
	if idleTimeout <= 0 {
		idleTimeout = managementIdleTimeout
	}
	httpServer := &http.Server{
		Handler:           server.requests,
		ReadHeaderTimeout: managementReadHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    server.options.MaxHeaderBytes,
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
			server.reportRuntimeError(fmt.Errorf("serve management API: %w", serveErr))
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

// StopAccepting 停止新 Management 连接，已进入 Handler 的请求继续排空。
func (server *Server) StopAccepting() error {
	if server == nil {
		return errInvalidServerOptions
	}
	server.stopOnce.Do(func() {
		// Listener.Close 之前先关闭请求准入，防止已经建立的 Keep-Alive 连接在
		// Shutdown 排空窗口中加入新的数据库工作。
		server.requests.stopAccepting()
		server.mu.Lock()
		listener := server.listener
		server.mu.Unlock()
		if listener != nil {
			server.stopErr = normalizeServerCloseError(listener.Close())
		}
	})
	return server.stopErr
}

// Shutdown 在调用方的绝对 Deadline 内排空已准入请求；超时后主动关闭残留连接，
// 并同步等待 Serve owner 退出，防止 SQLite 关闭后仍有 Management 请求访问仓储。
func (server *Server) Shutdown(ctx context.Context) error {
	if server == nil || ctx == nil {
		return errInvalidServerOptions
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
		drainErr := normalizeServerCloseError(httpServer.Shutdown(ctx))
		if drainErr == nil {
			drainErr = server.requests.waitContext(ctx)
		}
		if drainErr != nil {
			if cancel != nil {
				cancel()
			}
			drainErr = errors.Join(drainErr, normalizeServerCloseError(httpServer.Close()))
		}
		server.wait.Wait()
		// 强关连接只能解除 IO，不能假设 Handler 已经返回；必须等请求 Owner
		// 归零后，外层生命周期才能安全关闭 SQLite。
		server.requests.wait()
		server.shutdownErr = errors.Join(stopErr, drainErr)
	})
	return server.shutdownErr
}

// Close 立即取消所有请求、关闭 Listener 和连接，并等待 Serve owner 退出。
func (server *Server) Close() error {
	if server == nil {
		return errInvalidServerOptions
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
			closeErr = normalizeServerCloseError(httpServer.Close())
		}
		server.wait.Wait()
		server.requests.wait()
		server.closeErr = errors.Join(stopErr, closeErr)
	})
	return server.closeErr
}

func (server *Server) handlePanic(err error) {
	server.reportRuntimeError(fmt.Errorf("management serve owner: %w", err))
}

func (server *Server) reportRuntimeError(err error) {
	if err != nil && server.options.ReportRuntimeError != nil {
		server.options.ReportRuntimeError(err)
	}
}

func normalizeServerCloseError(err error) error {
	if errors.Is(err, net.ErrClosed) || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// managementRequestTracker 为 Management 请求提供 Listener 之外的准入 Fence，并
// 精确拥有全部已进入 Handler 的请求。Stop/Wait 共用一把锁，避免 Add/Wait 竞态。
type managementRequestTracker struct {
	next http.Handler

	mu        sync.Mutex
	accepting bool
	active    uint64
	drained   chan struct{}
}

func newManagementRequestTracker(next http.Handler) *managementRequestTracker {
	return &managementRequestTracker{next: next, accepting: true, drained: make(chan struct{})}
}

func (tracker *managementRequestTracker) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if !tracker.admit() {
		writer.Header().Set("Connection", "close")
		http.Error(writer, "server draining", http.StatusServiceUnavailable)
		return
	}
	defer tracker.finish()
	tracker.next.ServeHTTP(writer, request)
}

func (tracker *managementRequestTracker) admit() bool {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if !tracker.accepting {
		return false
	}
	tracker.active++
	return true
}

func (tracker *managementRequestTracker) finish() {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.active--
	if !tracker.accepting && tracker.active == 0 {
		close(tracker.drained)
	}
}

func (tracker *managementRequestTracker) stopAccepting() {
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

func (tracker *managementRequestTracker) waitContext(ctx context.Context) error {
	select {
	case <-tracker.drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (tracker *managementRequestTracker) wait() {
	<-tracker.drained
}
