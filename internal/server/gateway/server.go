package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/lifei6671/xtunnel/internal/safego"
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
	// ReportRuntimeError 接收后台 goroutine 的致命错误。回调必须保持非阻塞；
	// Gateway 会先停止接收新连接并取消仍在运行的连接处理器，再调用该回调。
	ReportRuntimeError func(error)
	// AcquireMaintenanceBarrier 把 pinned 身份的运行期续签纳入 Server 的
	// durable-state 写屏障。返回的 release 必须可在续签结束后无阻塞调用；
	// 等待过程必须尊重 ctx，确保 Shutdown 能解除仍在排队的续签。
	AcquireMaintenanceBarrier func(context.Context) (release func(), err error)

	// 下列未导出字段仅用于包内测试：生产路径固定使用默认检查周期和系统时钟，
	// 不新增用户配置项，也避免测试因真实时间等待而不稳定。
	renewalCheckInterval time.Duration
	now                  func() time.Time
}

// Server 持有一个 Agent Gateway Listener，并负责有界 TLS Handshake 生命周期。
type Server struct {
	options ServerOptions

	mu         sync.Mutex
	listener   net.Listener
	cancel     context.CancelFunc
	wait       sync.WaitGroup
	stopped    bool
	stopOnce   sync.Once
	closeOnce  sync.Once
	stopErr    error
	closeErr   error
	runtimeErr error

	// identity 受 identityMu 保护。握手只读锁读取当前证书；续签在磁盘操作结束后
	// 才短暂持写锁替换它，因此已完成 TLS 握手的连接不会受到影响。
	identityMu       sync.RWMutex
	identity         Identity
	lastRenewalError error
	renewalMu        sync.Mutex
	renewalInterval  time.Duration
	now              func() time.Time
}

// MetricsSnapshot 只暴露当前热加载叶证书的到期 Unix 时间戳。
// 它不返回证书链、SPKI 或私钥对象，避免采集方取得 TLS 身份所有权。
type MetricsSnapshot struct {
	CertificateExpiryUnixSeconds int64
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
	safego.Go(server.handlePanic("gateway accept loop"), server.wait.Done, func() {
		server.accept(ctx)
	})
	if server.hasPinnedRenewalSource() {
		server.wait.Add(1)
		safego.Go(server.handlePanic("gateway certificate renewal loop"), server.wait.Done, func() {
			server.renewalLoop(ctx)
		})
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
		server.mu.Lock()
		runtimeErr := server.runtimeErr
		server.mu.Unlock()
		server.closeErr = errors.Join(stopErr, runtimeErr)
	})
	return server.closeErr
}

// accept 是 Listener 的唯一 Accept owner。每个已接收连接都由 wait group 跟踪，
// TLS 预算满时立即关闭；Listener 关闭只结束新入口，不取消已经认证的连接。
func (server *Server) accept(ctx context.Context) {
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
			safego.Go(server.handlePanic("gateway connection"), server.wait.Done, func() {
				defer connection.Close()
				// TLS 握手预算只覆盖 Handshake，不能覆盖认证后的整个 Control/Work
				// 生命周期。Control Session 和 ACTIVE Work 可能持续数小时；若一直
				// 占用该槽位，少量长连接就会永久阻止新 Connector 完成 TLS 握手。
				tlsConnection, protocol, ok := func() (*tls.Conn, Protocol, bool) {
					defer func() { <-limit }()
					return server.handshake(ctx, connection)
				}()
				if !ok {
					return
				}
				server.handle(ctx, tlsConnection, protocol)
			})
		default:
			// 预算耗尽时不读取 TLS 或 Protocol 字节，直接关掉新连接。
			_ = connection.Close()
		}
	}
}

// handshake 在有界超时内完成 TLS 和 ALPN 协商；失败时返回 false，连接仍由
// accept 为该连接创建的外层 goroutine 统一关闭。
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

// handle 把已认证前的 TLS 连接交给协议处理器，并保证处理器返回后关闭底层 FD。
func (server *Server) handle(ctx context.Context, connection *tls.Conn, protocol Protocol) {
	if server.options.Handle == nil {
		return
	}
	server.options.Handle(ctx, connection, protocol)
}

// tlsConfig 为每次握手返回独立配置；共享证书只经 getCertificate 原子读取，
// 已发布的 *tls.Config 不在并发连接之间原地修改。
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

// MetricsSnapshot 在 identityMu 下读取当前身份，因此 pinned 运行期续签成功后下次
// 采集会立即看到新到期时间；public 与 pinned 身份使用同一只读边界。
func (server *Server) MetricsSnapshot() MetricsSnapshot {
	if server == nil {
		return MetricsSnapshot{}
	}
	server.identityMu.RLock()
	defer server.identityMu.RUnlock()
	leaf := server.identity.Leaf()
	if leaf == nil {
		return MetricsSnapshot{}
	}
	return MetricsSnapshot{CertificateExpiryUnixSeconds: leaf.NotAfter.Unix()}
}

// getCertificate 在读锁下复制当前证书值，使后台续签发布与握手读取互不竞态。
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

// hasPinnedRenewalSource 判断当前身份是否支持进程内 pinned 证书续签。
func (server *Server) hasPinnedRenewalSource() bool {
	server.identityMu.RLock()
	defer server.identityMu.RUnlock()
	return server.identity.pinnedRenewal != nil
}

// renewalLoop 由 Server.Start 唯一拥有，按固定周期检查续签条件，随 Server Context
// 取消退出，并由 wait group 保证 Close 返回前 goroutine 已停止。
func (server *Server) renewalLoop(ctx context.Context) {
	ticker := time.NewTicker(server.renewalInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			server.renewPinnedIdentity(ctx)
		}
	}
}

// handlePanic 把受保护 goroutine 的 panic 转成进程级运行错误。互斥状态只保留首错，
// 首错会停止新连接、取消 Server Context 并调用运行时错误上报入口。
func (server *Server) handlePanic(operation string) func(error) {
	return func(err error) {
		runtimeErr := fmt.Errorf("%s: %w", operation, err)
		server.mu.Lock()
		first := server.runtimeErr == nil
		if first {
			server.runtimeErr = runtimeErr
		}
		cancel := server.cancel
		report := server.options.ReportRuntimeError
		server.mu.Unlock()
		if !first {
			return
		}
		_ = server.StopAccepting()
		if cancel != nil {
			cancel()
		}
		if report != nil {
			report(runtimeErr)
		}
	}
}

// renewPinnedIdentity 在不持有 identityMu 的情况下完成签发与文件替换。
// 这样新握手可以继续读取旧有效证书；只有替换成功后才用写锁发布新身份。
func (server *Server) renewPinnedIdentity(ctx context.Context) {
	server.renewalMu.Lock()
	defer server.renewalMu.Unlock()
	if acquire := server.options.AcquireMaintenanceBarrier; acquire != nil {
		release, err := acquire(ctx)
		if err != nil {
			server.identityMu.Lock()
			server.lastRenewalError = fmt.Errorf("acquire gateway identity maintenance barrier: %w", err)
			server.identityMu.Unlock()
			return
		}
		defer release()
	}

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

// protocolFromALPN 只接受冻结的 Control/Work 协议，未知或空 ALPN 快速失败。
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
