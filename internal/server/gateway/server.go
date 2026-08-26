package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

const (
	// ControlALPN 是 Control Session 唯一允许的 ALPN 协商结果。
	ControlALPN = "xtunnel-control/1"
	// WorkALPN 是 WorkConn 唯一允许的 ALPN 协商结果。
	WorkALPN = "xtunnel-work/1"

	handshakeTimeout = 10 * time.Second

	// defaultRenewalCheckInterval 限制常驻 Server 对本地证书文件的检查频率。
	// pinned 身份启动时已检查一次；后续每天检查可确保进入 30 天窗口后自动续签。
	defaultRenewalCheckInterval = 24 * time.Hour
)

var (
	// ErrUnsupportedALPN 表示 TLS 已完成但协商结果不属于冻结的 Gateway 协议集合。
	ErrUnsupportedALPN = errors.New("gateway TLS ALPN is empty or unsupported")
	// ErrHandshakeLimitReached 表示有界握手预算已耗尽，连接在读取任何协议帧前关闭。
	ErrHandshakeLimitReached = errors.New("gateway pending TLS handshake limit reached")
)

// Protocol 表示由 ALPN 选择的连接类型。M1-04 只完成选择和边界，不实现其协议处理器。
type Protocol string

const (
	// ControlProtocol 对应 ControlALPN。
	ControlProtocol Protocol = "control"
	// WorkProtocol 对应 WorkALPN。
	WorkProtocol Protocol = "work"
)

// ServerOptions 是 Gateway Listener 的最小运行参数。
type ServerOptions struct {
	Listen                  string
	Identity                Identity
	MaxPendingTLSHandshakes int
	// Handle 在 TLS/ALPN 边界完成后接收连接。M1-04 不提供任何 Auth 或 Work 处理器；
	// 生产生命周期传入 nil 时连接会立即关闭，避免错误地把流量回落到某个默认协议。
	Handle func(context.Context, *tls.Conn, Protocol)

	// 下列未导出字段仅用于包内测试：生产路径固定使用默认检查周期和系统时钟，
	// 不新增用户配置项，也避免测试因真实时间等待而不稳定。
	renewalCheckInterval time.Duration
	now                  func() time.Time
}

// Server 持有一个 Agent Gateway Listener，并负责有界 TLS Handshake 生命周期。
type Server struct {
	options ServerOptions

	mu        sync.Mutex
	listener  net.Listener
	cancel    context.CancelFunc
	wait      sync.WaitGroup
	stopped   bool
	stopOnce  sync.Once
	closeOnce sync.Once
	stopErr   error
	closeErr  error

	// identity 受 identityMu 保护。握手只读锁读取当前证书；续签在磁盘操作结束后
	// 才短暂持写锁替换它，因此已完成 TLS 握手的连接不会受到影响。
	identityMu       sync.RWMutex
	identity         Identity
	lastRenewalError error
	renewalMu        sync.Mutex
	renewalInterval  time.Duration
	now              func() time.Time
}

// NewServer 校验静态限制并构造尚未监听的 Gateway。
func NewServer(options ServerOptions) (*Server, error) {
	if options.Listen == "" {
		return nil, errors.New("gateway listen address must not be empty")
	}
	if options.MaxPendingTLSHandshakes < 1 {
		return nil, errors.New("gateway pending TLS handshake limit must be greater than zero")
	}
	if options.Identity.Leaf() == nil || options.Identity.PrivateKey() == nil || len(options.Identity.CertificateChain()) == 0 {
		return nil, errors.New("gateway TLS identity is incomplete")
	}
	renewalInterval := options.renewalCheckInterval
	if renewalInterval <= 0 {
		renewalInterval = defaultRenewalCheckInterval
	}
	clock := options.now
	if clock == nil {
		clock = time.Now
	}
	return &Server{
		options:         options,
		identity:        options.Identity,
		renewalInterval: renewalInterval,
		now:             clock,
	}, nil
}

// Start 只允许启动一次；调用方应在首个 Admin 已存在或创建成功后调用。
func (server *Server) Start(parent context.Context) error {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.stopped {
		return errors.New("gateway server has already stopped")
	}
	if server.listener != nil {
		return errors.New("gateway server has already started")
	}
	listener, err := net.Listen("tcp", server.options.Listen)
	if err != nil {
		return fmt.Errorf("listen for agent gateway: %w", err)
	}
	ctx, cancel := context.WithCancel(parent)
	server.listener = listener
	server.cancel = cancel
	server.wait.Add(1)
	go server.accept(ctx)
	if server.hasPinnedRenewalSource() {
		server.wait.Add(1)
		go server.renewalLoop(ctx)
	}
	return nil
}

// Addr 返回已启动 Listener 的实际地址；未启动时返回 nil。
func (server *Server) Addr() net.Addr {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.listener == nil {
		return nil
	}
	return server.listener.Addr()
}

// StopAccepting 关闭 Listener 解除 Accept，但保留已经完成认证的连接 Context。
// Server 优雅退出先调用本方法停止新入口，完成 ACTIVE 排空后再调用 Close。
func (server *Server) StopAccepting() error {
	server.stopOnce.Do(func() {
		server.mu.Lock()
		server.stopped = true
		listener := server.listener
		server.mu.Unlock()
		if listener != nil {
			server.stopErr = listener.Close()
		}
	})
	return server.stopErr
}

// Close 停止新入口、取消全部剩余连接并等待所有 Gateway goroutine 退出。
func (server *Server) Close() error {
	server.closeOnce.Do(func() {
		stopErr := server.StopAccepting()
		server.mu.Lock()
		cancel := server.cancel
		server.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		server.wait.Wait()
		server.closeErr = stopErr
	})
	return server.closeErr
}

func (server *Server) accept(ctx context.Context) {
	defer server.wait.Done()
	limit := make(chan struct{}, server.options.MaxPendingTLSHandshakes)
	for {
		connection, err := server.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			continue
		}
		select {
		case limit <- struct{}{}:
			server.wait.Add(1)
			go func() {
				defer server.wait.Done()
				// TLS 握手预算只覆盖 Handshake，不能覆盖认证后的整个 Control/Work
				// 生命周期。Control Session 和 ACTIVE Work 可能持续数小时；若一直
				// 占用该槽位，少量长连接就会永久阻止新 Connector 完成 TLS 握手。
				tlsConnection, protocol, ok := server.handshake(ctx, connection)
				<-limit
				if !ok {
					_ = connection.Close()
					return
				}
				server.handle(ctx, tlsConnection, protocol)
			}()
		default:
			// 预算耗尽时不读取 TLS 或 Protocol 字节，直接关掉新连接。
			_ = connection.Close()
		}
	}
}

func (server *Server) handshake(ctx context.Context, connection net.Conn) (*tls.Conn, Protocol, bool) {
	tlsConnection := tls.Server(connection, server.tlsConfig())
	handshakeContext, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	if err := tlsConnection.HandshakeContext(handshakeContext); err != nil {
		return nil, "", false
	}
	protocol, err := protocolFromALPN(tlsConnection.ConnectionState().NegotiatedProtocol)
	if err != nil {
		return nil, "", false
	}
	return tlsConnection, protocol, true
}

func (server *Server) handle(ctx context.Context, connection *tls.Conn, protocol Protocol) {
	defer connection.Close()
	if server.options.Handle == nil {
		return
	}
	server.options.Handle(ctx, connection, protocol)
}

func (server *Server) tlsConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS13,
		GetCertificate: server.getCertificate,
		NextProtos:     []string{ControlALPN, WorkALPN},
	}
}

// LastRenewalError 返回最近一次后台 pinned 证书续签失败。
// 返回 nil 表示尚未发生失败或最近一次续签已经成功；调用方可据此接入后续 Metrics/Logging。
func (server *Server) LastRenewalError() error {
	server.identityMu.RLock()
	defer server.identityMu.RUnlock()
	return server.lastRenewalError
}

func (server *Server) getCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	server.identityMu.RLock()
	identity := server.identity
	server.identityMu.RUnlock()
	if identity.Leaf() == nil || identity.PrivateKey() == nil || len(identity.CertificateChain()) == 0 {
		return nil, errors.New("gateway TLS identity is incomplete")
	}
	return &tls.Certificate{
		Certificate: identity.CertificateChain(),
		PrivateKey:  identity.PrivateKey(),
		Leaf:        identity.Leaf(),
	}, nil
}

func (server *Server) hasPinnedRenewalSource() bool {
	server.identityMu.RLock()
	defer server.identityMu.RUnlock()
	return server.identity.pinnedRenewal != nil
}

func (server *Server) renewalLoop(ctx context.Context) {
	defer server.wait.Done()
	ticker := time.NewTicker(server.renewalInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			server.renewPinnedIdentity()
		}
	}
}

// renewPinnedIdentity 在不持有 identityMu 的情况下完成签发与文件替换。
// 这样新握手可以继续读取旧有效证书；只有替换成功后才用写锁发布新身份。
func (server *Server) renewPinnedIdentity() {
	server.renewalMu.Lock()
	defer server.renewalMu.Unlock()

	server.identityMu.RLock()
	identity := server.identity
	server.identityMu.RUnlock()
	renewed, err := identity.renewIfNecessary(server.now())
	server.identityMu.Lock()
	defer server.identityMu.Unlock()
	if err != nil {
		// 续签失败时保留内存中的旧有效身份，供当前和后续 TLS 握手继续使用。
		server.lastRenewalError = err
		return
	}
	server.identity = renewed
	server.lastRenewalError = nil
}

func protocolFromALPN(alpn string) (Protocol, error) {
	switch alpn {
	case ControlALPN:
		return ControlProtocol, nil
	case WorkALPN:
		return WorkProtocol, nil
	default:
		return "", ErrUnsupportedALPN
	}
}
