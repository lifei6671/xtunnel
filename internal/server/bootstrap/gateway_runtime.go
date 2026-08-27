package bootstrap

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/lifei6671/xtunnel/internal/application"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	servercontrolauth "github.com/lifei6671/xtunnel/internal/server/controlauth"
	"github.com/lifei6671/xtunnel/internal/server/externallock"
	"github.com/lifei6671/xtunnel/internal/server/gateway"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
	"github.com/lifei6671/xtunnel/internal/server/sessionruntime"
	"github.com/lifei6671/xtunnel/internal/server/snapshot"
	serverworkauth "github.com/lifei6671/xtunnel/internal/server/workauth"
)

const (
	// Control AUTH 的固定 IO 窗口不新增配置项；它只覆盖首个有界 AUTH Frame，
	// 成功后 Control Owner 会切换到独立的逐帧写超时与 Session 生命周期。
	controlAuthReadTimeout  = 10 * time.Second
	controlAuthWriteTimeout = 10 * time.Second
	controlAuthRetryAfter   = time.Second
	workAuthReadTimeout     = 10 * time.Second
	workAuthWriteTimeout    = 10 * time.Second
	// SQLite 预算覆盖数据库、WAL/SHM 与迁移期间的短暂文件；Listener 预算覆盖
	// Gateway 和产品入口基座。Management/Metrics 独立列项，安全余量吸收日志、
	// DNS 与运行时临时 FD，避免把理论上限顶到 RLIMIT_NOFILE。
	fdSQLiteReserve    = uint64(8)
	fdListenerReserve  = uint64(2)
	fdSafetyMargin     = uint64(128)
	serverDrainTimeout = 30 * time.Second
)

// openGatewayLifecycle 在 Server 已持有 External Lock 且 SQLite 已完成 Migration 后加载身份。
// 它只装配 Listener，不在这里监听；首个 Admin 成功前不得调用 Start。
func openGatewayLifecycle(config serverconfig.Config, resources storage, logger *slog.Logger, reportRuntimeError func(error)) (*gateway.Server, *sessionruntime.Manager, error) {
	serverResources, ok := resources.(*serverStorage)
	if !ok {
		return nil, nil, errors.New("unexpected server storage implementation")
	}
	if err := serverlimits.CheckFDBudget(serverlimits.FDBudget{
		WorkConnections:         uint64(config.Limits.MaxWorkConnections),
		PublicActiveConnections: uint64(config.Limits.MaxActiveConnections),
		PendingOpenConnections:  uint64(config.Limits.MaxPendingOpens),
		ConnectorControls:       uint64(config.Limits.MaxConnectors),
		PendingTLSHandshakes:    uint64(config.Limits.MaxPendingTLSHandshakes),
		PendingAuth:             uint64(config.Limits.MaxPendingAuth),
		Listeners:               fdListenerReserve, SQLite: fdSQLiteReserve,
		Management: 1, Metrics: 1, SafetyMargin: fdSafetyMargin,
	}); err != nil {
		return nil, nil, fmt.Errorf("validate startup file descriptor budget: %w", err)
	}
	limitManager, err := serverlimits.New(serverlimits.Options{
		MaxConnectors:                uint64(config.Limits.MaxConnectors),
		MaxConnectorsPerTunnel:       uint64(config.Limits.MaxConnectorsPerTunnel),
		MaxWorkConnections:           uint64(config.Limits.MaxWorkConnections),
		MaxIdleWorkConnections:       uint64(config.Limits.MaxIdleWorkConnections),
		MaxConnectingWorkConnections: uint64(config.Limits.MaxConnectingWorkConnections),
		MaxPendingOpens:              uint64(config.Limits.MaxPendingOpens),
		MaxActiveConnections:         uint64(config.Limits.MaxActiveConnections),
		MaxConnectionsPerTunnel:      uint64(config.Limits.MaxConnectionsPerTunnel),
		MaxConnectionsPerService:     uint64(config.Limits.MaxConnectionsPerService),
		MaxConnectionsPerSourceIP:    uint64(config.Limits.MaxConnectionsPerSourceIP),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("construct server resource limit manager: %w", err)
	}
	var (
		identity    gateway.Identity
		identityErr error
	)
	switch config.AgentGateway.TLS.Mode {
	case gateway.PinnedMode:
		identity, identityErr = gateway.LoadOrCreatePinnedIdentity(
			config.Server.DataDir,
			config.AgentGateway.PublicHostname,
			!serverResources.databaseExisted,
			time.Now(),
		)
	case gateway.PublicMode:
		identity, identityErr = gateway.LoadPublicIdentity(config.AgentGateway.TLS.CertFile, config.AgentGateway.TLS.KeyFile)
	default:
		return nil, nil, fmt.Errorf("unsupported gateway TLS mode %q", config.AgentGateway.TLS.Mode)
	}
	if identityErr != nil {
		return nil, nil, fmt.Errorf("load gateway TLS identity: %w", identityErr)
	}
	protector, err := application.NewAES256GCMTokenProtector(serverResources.tokenMasterKey[:])
	if err != nil {
		return nil, nil, fmt.Errorf("construct Tunnel Token protector for gateway authentication: %w", err)
	}
	tokenService := application.NewConnectionTokenService(serverResources.database, protector)
	snapshotBuilder, err := snapshot.New(snapshot.Config{
		ProtocolVersion:      snapshotProtocolVersion,
		MaxServices:          config.Limits.MaxServicesPerTunnel,
		MaxSnapshotBytes:     config.Limits.MaxTunnelSnapshotBytes,
		MaxControlFrameBytes: config.Limits.MaxControlFrameBytes,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("construct gateway Snapshot builder: %w", err)
	}
	snapshotSource, err := snapshot.NewSource(serverResources.database, snapshotBuilder)
	if err != nil {
		return nil, nil, fmt.Errorf("construct gateway Snapshot source: %w", err)
	}
	registry := serverruntime.NewRegistryWithLimits(limitManager)
	sessions, err := sessionruntime.New(registry, sessionruntime.Options{
		HighPriorityCapacity: config.Control.HighPriorityQueue,
		NormalCapacity:       config.Control.NormalQueue,
		// Control Schema 目前没有单独的入站队列配置。使用已冻结的普通队列容量
		// 作为同阶有界预算，既不引入新配置，也不会形成无界内存路径。
		InboundCapacity:      config.Control.NormalQueue,
		WriteTimeout:         config.Control.WriteTimeout.Duration,
		MaxReplayEntries:     config.Limits.MaxReplayEntriesPerSession,
		MaxWorkTotal:         uint32(config.Limits.MaxWorkConnections),
		MaxWorkConnecting:    uint32(config.Limits.MaxConnectingWorkConnections),
		MaxControlFrameBytes: uint64(config.Limits.MaxControlFrameBytes),
		LimitManager:         limitManager,
		SnapshotProvider:     snapshotSource,
		HeartbeatTimeout:     config.ConnectorRuntime.HeartbeatTimeout.Duration,
		Logger:               logger,
		ReportRuntimeError:   reportRuntimeError,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("construct gateway Session runtime: %w", err)
	}
	controlAuthenticator, err := servercontrolauth.New(tokenService, registry, servercontrolauth.Options{
		ReadTimeout: controlAuthReadTimeout, WriteTimeout: controlAuthWriteTimeout,
		HeartbeatInterval: config.ConnectorRuntime.HeartbeatInterval.Duration,
		RetryAfter:        controlAuthRetryAfter,
		MaxFrameBytes:     uint64(config.Limits.MaxAuthFrameBytes),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("construct gateway Control authenticator: %w", err)
	}
	workAuthenticator, err := serverworkauth.NewHandler(sessions, serverworkauth.HandlerOptions{
		ReadTimeout: workAuthReadTimeout, WriteTimeout: workAuthWriteTimeout,
		MaxFrameBytes: uint64(config.Limits.MaxWorkFrameBytes),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("construct gateway Work authenticator: %w", err)
	}
	protocolHandler := &gatewayProtocolHandler{
		controlAuthenticator: controlAuthenticator,
		workAuthenticator:    workAuthenticator,
		sessions:             sessions,
		authGate:             newAuthGate(config.Limits.MaxPendingAuth),
	}
	server, err := gateway.NewServer(gateway.ServerOptions{
		Listen:                  config.AgentGateway.Listen,
		Identity:                identity,
		MaxPendingTLSHandshakes: config.Limits.MaxPendingTLSHandshakes,
		Handle:                  protocolHandler.handle,
		ReportRuntimeError:      reportRuntimeError,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("construct agent gateway: %w", err)
	}
	return server, sessions, nil
}

// gatewayProtocolHandler 是 TLS/ALPN 之后唯一的生产协议分发点。
//
// Control 路径同步运行到 Owner 退出；Work 路径同步运行到 Pool 关闭对应 Work，
// 让 Gateway 的连接 goroutine 与真实所有权生命周期完全一致。两种 ALPN 都必须先
// 认证，绝不把字节回落到未认证的 RAW 连接。
type gatewayProtocolHandler struct {
	controlAuthenticator *servercontrolauth.Handler
	workAuthenticator    *serverworkauth.Handler
	sessions             *sessionruntime.Manager
	authGate             *authGate
}

func (handler *gatewayProtocolHandler) handle(ctx context.Context, connection *tls.Conn, protocol gateway.Protocol) {
	if handler == nil || connection == nil {
		return
	}
	// TLS Handshake 已由 Gateway 的独立预算保护。这里再限制尚未完成 AUTH 的
	// 连接数量；认证一旦完整提交就立即释放槽位，不能让长寿命 Control/Work
	// 连接一直占用 pending-auth 预算。
	releaseAuth, acquired := handler.authGate.tryAcquire()
	if !acquired {
		return
	}
	switch protocol {
	case gateway.ControlProtocol:
		established, err := handler.controlAuthenticator.Handle(ctx, connection)
		releaseAuth()
		if err != nil {
			return
		}
		// Serve 会复制后立即清除 Session Secret，并接管连接直到 Owner 结束。
		_ = handler.sessions.Serve(ctx, connection, &established)
	case gateway.WorkProtocol:
		idle, err := handler.workAuthenticator.Handle(ctx, connection)
		releaseAuth()
		if err != nil {
			return
		}
		work, err := handler.sessions.RegisterIdle(connection, idle)
		if err != nil {
			idle.State.Close()
			return
		}
		// gateway.Server 在 Handle 返回后关闭 TLS 连接，因此这里必须等待 Pool
		// 的真正关闭完成，不能在 IDLE 所有权刚转移后提前返回。
		<-work.Done()
		idle.State.Close()
	default:
		releaseAuth()
	}
}

// authGate 是 Gateway TLS 之后、AUTH 提交之前的非阻塞并发预算。
// 它故意不排队：队列会额外持有未认证 socket 和内存，预算满时立即关闭连接，
// Agent 将其视为瞬时网络/容量失败并按 M1-13 的抖动退避重连。
type authGate struct {
	slots chan struct{}
}

func newAuthGate(capacity int) *authGate {
	return &authGate{slots: make(chan struct{}, capacity)}
}

func (gate *authGate) tryAcquire() (release func(), acquired bool) {
	if gate == nil || cap(gate.slots) == 0 {
		return func() {}, false
	}
	select {
	case gate.slots <- struct{}{}:
		return func() { <-gate.slots }, true
	default:
		return func() {}, false
	}
}

// gatewayBootstrapCloser 将运行中的 Gateway 与只在 SETUP_REQUIRED 存在的 Bootstrap Socket 一起收束。
type gatewayBootstrapCloser struct {
	bootstrap     io.Closer
	gateway       *gateway.Server
	sessions      *sessionruntime.Manager
	runtimeErrors <-chan error
	drainTimeout  time.Duration
	once          sync.Once
	result        error
}

func (closer *gatewayBootstrapCloser) Close() error {
	closer.once.Do(func() {
		var bootstrapErr error
		if closer.bootstrap != nil {
			bootstrapErr = closer.bootstrap.Close()
		}
		stopErr := closer.gateway.StopAccepting()
		timeout := closer.drainTimeout
		if timeout <= 0 {
			timeout = serverDrainTimeout
		}
		drainContext, cancelDrain := context.WithTimeout(context.Background(), timeout)
		drainErr := closer.sessions.Shutdown(drainContext)
		cancelDrain()
		closer.result = errors.Join(bootstrapErr, stopErr, drainErr, closer.gateway.Close())
	})
	return closer.result
}

// RuntimeErrors 返回需要终止当前 Server 进程的异步运行错误，包括 Admin 事务
// 提交后的 Gateway 启动失败以及受保护 goroutine 的 panic。
func (closer *gatewayBootstrapCloser) RuntimeErrors() <-chan error {
	return closer.runtimeErrors
}

func openGatewayAndBootstrap(ctx context.Context, config serverconfig.Config, resources storage, logger *slog.Logger) (io.Closer, error) {
	return openGatewayAndBootstrapWith(ctx, config, resources, logger, externallock.RuntimeDirectory, func(ctx context.Context, runtimeDir, targetHash string, store *sqlite.Store, afterCreate func() error, reportRuntimeError func(error)) (io.Closer, error) {
		return openAdminBootstrapSocketAfter(ctx, runtimeDir, targetHash, store, afterCreate, reportRuntimeError)
	})
}

// openGatewayAndBootstrapWith 保持生产生命周期的唯一装配路径，并允许测试替换
// Bootstrap Socket 的对等方授权策略，而不改变真实 Server 的 root-only 约束。
func openGatewayAndBootstrapWith(
	ctx context.Context,
	config serverconfig.Config,
	resources storage,
	logger *slog.Logger,
	runtimeDir string,
	openBootstrapSocket func(context.Context, string, string, *sqlite.Store, func() error, func(error)) (io.Closer, error),
) (io.Closer, error) {
	serverResources, ok := resources.(*serverStorage)
	if !ok {
		return nil, errors.New("unexpected server storage implementation")
	}
	// SQLite 已在 openServerStorage 中完成 Migration。任何 Listener 或本机
	// Bootstrap Socket 建立前先验证全部存量 Desired State，避免 Connector
	// 在超大 Snapshot 上进入永久重连循环。
	if err := validateStoredSnapshots(ctx, config, serverResources.database); err != nil {
		return nil, fmt.Errorf("validate stored tunnel snapshots before gateway startup: %w", err)
	}
	runtimeErrors := make(chan error, 1)
	reportRuntimeError := func(err error) {
		select {
		case runtimeErrors <- err:
		default:
		}
	}
	gatewayServer, sessions, err := openGatewayLifecycle(config, resources, logger, reportRuntimeError)
	if err != nil {
		return nil, err
	}
	startGateway := func() error {
		// 进程 Context 取消只触发外层冻结的排空顺序；已认证 Gateway 连接必须继续
		// 存活到 ACTIVE 自然结束或 Server Drain Deadline。
		if err := gatewayServer.Start(context.WithoutCancel(ctx)); err != nil {
			startErr := errors.Join(fmt.Errorf("start agent gateway after admin bootstrap: %w", err), gatewayServer.Close())
			reportRuntimeError(startErr)
			return startErr
		}
		// 冻结的 Server 启动顺序是 Gateway 后紧接唯一 Runtime Reconciler。
		// 极短窗口内到达的 Control Session 因 Reconciler 未启动而 fail-closed，
		// Agent 使用现有 backoff 重连，不会回落到本地配置。
		if err := sessions.Start(context.WithoutCancel(ctx)); err != nil {
			drainContext, cancelDrain := context.WithTimeout(context.Background(), serverDrainTimeout)
			stopErr := gatewayServer.StopAccepting()
			shutdownErr := sessions.Shutdown(drainContext)
			cancelDrain()
			startErr := errors.Join(
				fmt.Errorf("start server Snapshot reconciler: %w", err),
				stopErr,
				shutdownErr,
				gatewayServer.Close(),
			)
			reportRuntimeError(startErr)
			return startErr
		}
		return nil
	}
	hasAdmin, err := serverResources.database.HasAdmin(ctx)
	if err != nil {
		return nil, fmt.Errorf("check admin bootstrap state before gateway start: %w", err)
	}
	if hasAdmin {
		if err := startGateway(); err != nil {
			return nil, err
		}
		return &gatewayBootstrapCloser{gateway: gatewayServer, sessions: sessions, runtimeErrors: runtimeErrors}, nil
	}
	socket, err := openBootstrapSocket(ctx, runtimeDir, serverResources.targetHash, serverResources.database, startGateway, reportRuntimeError)
	if err != nil {
		return nil, errors.Join(err, gatewayServer.Close())
	}
	return &gatewayBootstrapCloser{bootstrap: socket, gateway: gatewayServer, sessions: sessions, runtimeErrors: runtimeErrors}, nil
}
