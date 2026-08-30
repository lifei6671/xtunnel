package bootstrap

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/lifei6671/xtunnel/internal/application"
	"github.com/lifei6671/xtunnel/internal/buildinfo"
	"github.com/lifei6671/xtunnel/internal/healthbudget"
	"github.com/lifei6671/xtunnel/internal/logging"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	"github.com/lifei6671/xtunnel/internal/safego"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	servercontrolauth "github.com/lifei6671/xtunnel/internal/server/controlauth"
	"github.com/lifei6671/xtunnel/internal/server/externallock"
	"github.com/lifei6671/xtunnel/internal/server/gateway"
	serverhttpingress "github.com/lifei6671/xtunnel/internal/server/httpingress"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
	servermanagementapi "github.com/lifei6671/xtunnel/internal/server/managementapi"
	servermetrics "github.com/lifei6671/xtunnel/internal/server/metrics"
	serveropen "github.com/lifei6671/xtunnel/internal/server/open"
	serverroute "github.com/lifei6671/xtunnel/internal/server/route"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
	"github.com/lifei6671/xtunnel/internal/server/sessionruntime"
	"github.com/lifei6671/xtunnel/internal/server/snapshot"
	servertcpingress "github.com/lifei6671/xtunnel/internal/server/tcpingress"
	serverusage "github.com/lifei6671/xtunnel/internal/server/usage"
	serverworkauth "github.com/lifei6671/xtunnel/internal/server/workauth"
	"github.com/lifei6671/xtunnel/internal/tcpport"
	"github.com/lifei6671/xtunnel/internal/tracing"
	"github.com/lifei6671/xtunnel/internal/tunnel"
)

const (
	// Control AUTH 的固定 IO 窗口不新增配置项；它只覆盖首个有界 AUTH Frame，
	// 成功后 Control Owner 会切换到独立的逐帧写超时与 Session 生命周期。
	controlAuthReadTimeout  = 10 * time.Second
	controlAuthWriteTimeout = 10 * time.Second
	controlAuthRetryAfter   = time.Second
	workAuthReadTimeout     = 10 * time.Second
	workAuthWriteTimeout    = 10 * time.Second
	// OPEN 从首字节写出到完整 OpenResponse 提交共用一个 6 秒总预算；单次
	// Read/Write 也取相同上限，避免分阶段时限叠加成 12 秒。
	openHandshakeTimeout = 6 * time.Second
	// SQLite 预算覆盖数据库、WAL/SHM 与迁移期间的短暂文件；固定 Listener 预算覆盖
	// Admin Bootstrap、Backup Barrier、HTTP Ingress 与 Gateway 的启动重叠峰值。
	// TCP 逻辑端口池按全部可用端口再加一个原子换口候选统一预留，后续新增 Route
	// 不再依赖启动时数据库中恰好已有多少 Listener。
	// Management/Metrics 独立列项，安全余量吸收日志、DNS 与运行时临时 FD，
	// 避免把真实稳定 Listener 隐含塞入安全余量。
	fdSQLiteReserve    = uint64(8)
	fdListenerReserve  = uint64(4)
	fdSafetyMargin     = uint64(128)
	serverDrainTimeout = 30 * time.Second
)

func loadGatewayIdentity(config serverconfig.Config, resources *serverStorage) (gateway.Identity, error) {
	var (
		identity gateway.Identity
		err      error
	)
	switch config.AgentGateway.TLS.Mode {
	case gateway.PinnedMode:
		identity, err = gateway.LoadOrCreatePinnedIdentity(
			config.Server.DataDir,
			config.AgentGateway.PublicHostname,
			!resources.databaseExisted,
			time.Now(),
		)
	case gateway.PublicMode:
		identity, err = gateway.LoadPublicIdentity(config.AgentGateway.TLS.CertFile, config.AgentGateway.TLS.KeyFile)
	default:
		return gateway.Identity{}, fmt.Errorf("unsupported gateway TLS mode %q", config.AgentGateway.TLS.Mode)
	}
	if err != nil {
		return gateway.Identity{}, fmt.Errorf("load gateway TLS identity: %w", err)
	}
	return identity, nil
}

// managementGatewayConnectionDescription 把当前 Gateway 身份冻结为 Connection Token
// 描述。Public 模式信任系统 CA；Pinned 模式只发布当前身份的 SPKI SHA-256。
func managementGatewayConnectionDescription(
	config serverconfig.Config,
	identity gateway.Identity,
) (*protocolv1.GatewayEndpoint, *protocolv1.TlsTrustDescriptor, error) {
	_, portText, err := net.SplitHostPort(config.AgentGateway.Listen)
	if err != nil {
		return nil, nil, fmt.Errorf("parse agent gateway listen: %w", err)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return nil, nil, errors.New("agent gateway public port is invalid")
	}
	endpoint := &protocolv1.GatewayEndpoint{Host: config.AgentGateway.PublicHostname, Port: uint32(port)}
	trust := &protocolv1.TlsTrustDescriptor{}
	switch config.AgentGateway.TLS.Mode {
	case gateway.PublicMode:
		trust.Mode = &protocolv1.TlsTrustDescriptor_PublicCa{PublicCa: &protocolv1.PublicCATrust{}}
	case gateway.PinnedMode:
		hash := identity.SPKIHash()
		trust.Mode = &protocolv1.TlsTrustDescriptor_PinnedSpkiSha256{
			PinnedSpkiSha256: &protocolv1.PinnedSPKITrust{SpkiSha256: append([]byte(nil), hash[:]...)},
		}
	default:
		return nil, nil, fmt.Errorf("unsupported gateway TLS mode %q", config.AgentGateway.TLS.Mode)
	}
	return endpoint, trust, nil
}

// openGatewayLifecycle 在 Server 已持有 External Lock 且 SQLite 已完成 Migration 后加载身份。
// 它只装配 Listener，不在这里监听；首个 Admin 成功前不得调用 Start。
func openGatewayLifecycle(
	config serverconfig.Config,
	resources storage,
	logger *slog.Logger,
	reportRuntimeError func(error),
	healthBudget *healthbudget.Manager,
	identity gateway.Identity,
	tokenService *application.ConnectionTokenService,
	metricsBridge *serverMetricsBridge,
	usageBridge *serverUsageBridge,
	traceRuntime *tracing.Runtime,
) (*gateway.Server, *sessionruntime.Manager, *tunnel.Proxy, *serverlimits.Manager, error) {
	if metricsBridge == nil {
		return nil, nil, nil, nil, errors.New("server metrics bridge is required")
	}
	if usageBridge == nil || usageBridge.owner == nil {
		return nil, nil, nil, nil, errors.New("server usage bridge is required")
	}
	if healthBudget == nil {
		return nil, nil, nil, nil, errors.New("health target budget manager is required")
	}
	if tokenService == nil || identity.Leaf() == nil {
		return nil, nil, nil, nil, errors.New("gateway identity and token service are required")
	}
	serverResources, ok := resources.(*serverStorage)
	if !ok {
		return nil, nil, nil, nil, errors.New("unexpected server storage implementation")
	}
	tcpListenerReserve := tcpListenerFDReserve(config.TCPIngress)
	if err := checkServerFDBudget(config, tcpListenerReserve); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("validate startup file descriptor budget: %w", err)
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
		MaxOpenRatePerSourceIP:       uint64(config.Limits.MaxOpenRatePerSourceIP),
		MaxOpenBurstPerSourceIP:      uint64(config.Limits.MaxOpenBurstPerSourceIP),
		MaxHTTPRequestsPerSourceIPPerSecond: uint64(
			config.Limits.MaxHTTPRequestsPerSourceIPPerSecond,
		),
	})
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("construct server resource limit manager: %w", err)
	}
	snapshotBuilder, err := snapshot.New(snapshot.Config{
		ProtocolVersion:      snapshotProtocolVersion,
		MaxServices:          config.Limits.MaxServicesPerTunnel,
		MaxSnapshotBytes:     config.Limits.MaxTunnelSnapshotBytes,
		MaxControlFrameBytes: config.Limits.MaxControlFrameBytes,
	})
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("construct gateway Snapshot builder: %w", err)
	}
	snapshotSource, err := snapshot.NewSource(serverResources.database, snapshotBuilder)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("construct gateway Snapshot source: %w", err)
	}
	registry := serverruntime.NewRegistryWithLimitsAndHealthBudget(limitManager, healthBudget)
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
		ReconcileObserver:    metricsBridge.ObserveReconcile,
	})
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("construct gateway Session runtime: %w", err)
	}
	controlAuthenticator, err := servercontrolauth.New(tokenService, registry, servercontrolauth.Options{
		AuthenticationRecorder:    serverResources.database,
		TunnelAdmissionController: sessions,
		ReadTimeout:               controlAuthReadTimeout, WriteTimeout: controlAuthWriteTimeout,
		HeartbeatInterval: config.ConnectorRuntime.HeartbeatInterval.Duration,
		RetryAfter:        controlAuthRetryAfter,
		MaxFrameBytes:     uint64(config.Limits.MaxAuthFrameBytes),
	})
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("construct gateway Control authenticator: %w", err)
	}
	workAuthenticator, err := serverworkauth.NewHandler(sessions, serverworkauth.HandlerOptions{
		ReadTimeout: workAuthReadTimeout, WriteTimeout: workAuthWriteTimeout,
		MaxFrameBytes: uint64(config.Limits.MaxWorkFrameBytes),
	})
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("construct gateway Work authenticator: %w", err)
	}
	openHandler, err := serveropen.NewHandler(serveropen.Options{
		HandshakeTimeout: openHandshakeTimeout,
		ReadTimeout:      openHandshakeTimeout,
		WriteTimeout:     openHandshakeTimeout,
		MaxFrameBytes:    uint64(config.Limits.MaxWorkFrameBytes),
	})
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("construct gateway OPEN handler: %w", err)
	}
	tunnelProxy, err := tunnel.NewProxy(tunnel.Options{
		Registry: registry, Sessions: sessions, OpenHandler: openHandler,
		AcquireTimeout: config.Transport.TCP.WorkAcquireTimeout.Duration,
		LimitManager:   limitManager,
		Metrics:        metricsBridge,
		Usage:          usageBridge,
		Logger:         logger,
		Tracing:        traceRuntime,
	})
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("construct Tunnel data-plane proxy: %w", err)
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
		AcquireMaintenanceBarrier: func(ctx context.Context) (func(), error) {
			barrier, err := serverResources.database.AcquireBackupBarrier(ctx)
			if err != nil {
				return nil, err
			}
			return barrier.Release, nil
		},
	})
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("construct agent gateway: %w", err)
	}
	return server, sessions, tunnelProxy, limitManager, nil
}

// serverMetricsBridge 解决 Runtime 构造与私有 Registry Source 之间的装配环：
// Session/Tunnel 在 Listener 启动前先持有该桥，随后 Bootstrap 完成全部 owner 构造并
// 一次性发布 Registry。发布发生在任何并发回调之前，运行期只做直接同步转发。
type serverMetricsBridge struct {
	registry *servermetrics.Registry
}

func (bridge *serverMetricsBridge) ObserveOpen(duration time.Duration, code protocolv1.ErrorCode) {
	bridge.registry.ObserveOpen(duration, code)
}

func (bridge *serverMetricsBridge) ObserveOriginError(code protocolv1.ErrorCode) {
	bridge.registry.ObserveOriginError(code)
}

func (bridge *serverMetricsBridge) ObserveOriginConnect(duration time.Duration) {
	bridge.registry.ObserveOriginConnect(duration)
}

func (bridge *serverMetricsBridge) ObserveReconcile(duration time.Duration, code protocolv1.ErrorCode) {
	bridge.registry.ObserveReconcile(duration, code)
}

func (bridge *serverMetricsBridge) AddIngressBytes(bytes uint64) {
	bridge.registry.AddIngressBytes(bytes)
}

func (bridge *serverMetricsBridge) AddEgressBytes(bytes uint64) {
	bridge.registry.AddEgressBytes(bytes)
}

// serverMetricsSource 每次抓取只组合三个 owner 的聚合快照，不向 Collector 暴露
// 高基数 Map。Owner 指针在 Metrics Listener 启动前完成发布且之后不可变；采集过程
// 不执行数据库或网络 IO。
type serverMetricsSource struct {
	sessions     *sessionruntime.Manager
	healthBudget *healthbudget.Manager
	gateway      *gateway.Server
}

func (source *serverMetricsSource) MetricsOwnerSnapshot() servermetrics.OwnerSnapshot {
	sessions := source.sessions.MetricsSnapshot()
	health := source.healthBudget.MetricsSnapshot()
	gatewaySnapshot := source.gateway.MetricsSnapshot()
	return servermetrics.OwnerSnapshot{
		ConnectorsOnline:                sessions.XTunnelConnectorsOnline,
		ControlSessionsOnline:           sessions.XTunnelControlSessionsOnline,
		ActiveConnections:               sessions.XTunnelActiveConnections,
		TCPIdleWorkConnections:          sessions.XTunnelTCPIdleWorkConnections,
		TCPActiveWorkConnections:        sessions.XTunnelTCPActiveWorkConnections,
		HealthTargets:                   health.HealthTargets,
		HealthBudgetRejectionsTotal:     health.HealthBudgetRejectionsTotal,
		GatewayCertificateExpirySeconds: float64(gatewaySnapshot.CertificateExpiryUnixSeconds),
		RouteSnapshotBytes:              sessions.XTunnelRouteSnapshotBytes,
		RouteSnapshotRoutes:             sessions.XTunnelRouteSnapshotRoutes,
		ReconcileCoalescedTotal:         sessions.XTunnelReconcileCoalescedTotal,
	}
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

// handle 在 TLS/ALPN 成功后执行有界 AUTH 并把连接所有权交给对应 Control Owner
// 或 WorkPool。返回即表示 Gateway 可以关闭底层连接，因此 Work 路径必须等待 Done。
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

// newAuthGate 创建固定容量、无等待队列的认证预算。
func newAuthGate(capacity int) *authGate {
	return &authGate{slots: make(chan struct{}, capacity)}
}

// tryAcquire 非阻塞取得一次 AUTH 槽位；成功返回的 release 必须且只能调用一次。
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

// gatewayBootstrapCloser 将 Route owner、运行中的 Gateway 与只在 SETUP_REQUIRED
// 存在的 Bootstrap Socket 一起收束，并保证它们都先于 SQLite 退出。
type gatewayBootstrapCloser struct {
	backupBarrier   io.Closer
	bootstrap       io.Closer
	gateway         *gateway.Server
	management      *servermanagementapi.Server
	metrics         *servermetrics.Server
	metricsRegistry *servermetrics.Registry
	httpIngress     *serverhttpingress.Server
	httpHandler     *serverhttpingress.Handler
	tcpIngress      *servertcpingress.Manager
	sessions        *sessionruntime.Manager
	routes          *serverroute.Manager
	usage           *serverusage.Owner
	cancelRoutes    context.CancelFunc
	runtimeErrors   chan error
	drainTimeout    time.Duration
	once            sync.Once
	result          error
}

// Close 以幂等方式执行冻结的关闭顺序：先停止 Metrics、Management 和
// TCP/HTTP/Gateway 新连接，再让抓取、请求、Session 与公网连接共用绝对 Deadline
// 排空；到期由各 owner 主动关闭残留连接，最后关闭 Gateway 连接 goroutine。
func (closer *gatewayBootstrapCloser) Close() error {
	closer.once.Do(func() {
		var backupErr error
		if closer.backupBarrier != nil {
			// Backup Barrier 必须先停止并解除现有 Lease，随后才能排空
			// Gateway/Session 并最终关闭 SQLite。
			backupErr = closer.backupBarrier.Close()
		}
		var bootstrapErr error
		if closer.bootstrap != nil {
			bootstrapErr = closer.bootstrap.Close()
		}
		// 同时关闭 Metrics、Management 与三类公网入口后才开始排空。TCP/HTTP
		// Listener 先停可避免新的 OPEN/RoundTrip 继续申请 WorkConn；Gateway
		// 随后停止新的 Control/Work。
		var metricsStopErr error
		if closer.metrics != nil {
			metricsStopErr = closer.metrics.StopAccepting()
		}
		var managementStopErr error
		if closer.management != nil {
			managementStopErr = closer.management.StopAccepting()
		}
		var tcpStopErr error
		if closer.tcpIngress != nil {
			tcpStopErr = closer.tcpIngress.StopAccepting()
		}
		var httpStopErr error
		if closer.httpIngress != nil {
			httpStopErr = closer.httpIngress.StopAccepting()
		}
		stopErr := closer.gateway.StopAccepting()
		timeout := closer.drainTimeout
		if timeout <= 0 {
			timeout = serverDrainTimeout
		}
		drainContext, cancelDrain := context.WithTimeout(context.Background(), timeout)
		// 先关闭当前 idle Transport，避免没有请求的池化 WorkConn 占满排空窗口。
		// Session、TCP 与 HTTP 使用同一个绝对 Deadline 并行排空：Session 立即建立
		// 禁止新 OPEN 的 fence，TCP 连接和 HTTP 请求则允许已准入工作自然完成；
		// Deadline 到期后，各 owner 主动关闭残留连接以解阻 IO。
		if closer.httpHandler != nil {
			closer.httpHandler.CloseIdleConnections()
		}
		sessionResult := make(chan error, 1)
		safego.Go(
			func(err error) { sessionResult <- err },
			nil,
			func() { sessionResult <- closer.sessions.Shutdown(drainContext) },
		)
		tcpResult := make(chan error, 1)
		if closer.tcpIngress != nil {
			safego.Go(
				func(err error) { tcpResult <- err },
				nil,
				func() { tcpResult <- closer.tcpIngress.Shutdown(drainContext) },
			)
		} else {
			tcpResult <- nil
		}
		managementResult := make(chan error, 1)
		if closer.management != nil {
			safego.Go(
				func(err error) { managementResult <- err },
				nil,
				func() { managementResult <- closer.management.Shutdown(drainContext) },
			)
		} else {
			managementResult <- nil
		}
		metricsResult := make(chan error, 1)
		if closer.metrics != nil {
			safego.Go(
				func(err error) { metricsResult <- err },
				nil,
				func() { metricsResult <- closer.metrics.Shutdown(drainContext) },
			)
		} else {
			metricsResult <- nil
		}
		var httpDrainErr error
		if closer.httpIngress != nil {
			httpDrainErr = closer.httpIngress.Shutdown(drainContext)
		}
		// 在途请求完成后连接可能刚被 Transport 归还为 idle；再次同步关闭，
		// 让并行等待的 Session Shutdown 立即观察到 ACTIVE 归零。
		if closer.httpHandler != nil {
			closer.httpHandler.CloseIdleConnections()
		}
		drainErr := <-sessionResult
		tcpDrainErr := <-tcpResult
		managementDrainErr := <-managementResult
		metricsDrainErr := <-metricsResult
		cancelDrain()
		var metricsCloseErr error
		if closer.metrics != nil {
			metricsCloseErr = closer.metrics.Close()
		}
		var managementCloseErr error
		if closer.management != nil {
			managementCloseErr = closer.management.Close()
		}
		var httpCloseErr error
		if closer.httpIngress != nil {
			httpCloseErr = closer.httpIngress.Close()
		}
		var tcpCloseErr error
		if closer.tcpIngress != nil {
			tcpCloseErr = closer.tcpIngress.Close()
		}
		gatewayErr := closer.gateway.Close()
		// Route owner 必须晚于全部 Listener/Session 停止、早于 SQLite 关闭。
		// 取消后同步 Wait，确保它不再持有数据库读取或遗留孤儿 goroutine。
		if closer.cancelRoutes != nil {
			closer.cancelRoutes()
		}
		if closer.routes != nil {
			closer.routes.Wait()
		}
		// Usage 必须晚于全部数据面与 Session 排空、早于 SQLite 关闭。使用独立的
		// 有界 Context，避免进程取消信号跳过最后一批 Flush/Rollup。
		var usageErr error
		if closer.usage != nil {
			usageContext, cancelUsage := context.WithTimeout(context.Background(), 5*time.Second)
			usageErr = closer.usage.Shutdown(usageContext)
			cancelUsage()
		}
		closer.result = errors.Join(
			backupErr, bootstrapErr, metricsStopErr, managementStopErr, tcpStopErr, httpStopErr, stopErr,
			metricsDrainErr, managementDrainErr, tcpDrainErr, httpDrainErr, drainErr,
			metricsCloseErr, managementCloseErr, tcpCloseErr, httpCloseErr, gatewayErr, usageErr,
		)
	})
	return closer.result
}

// RuntimeErrors 返回需要终止当前 Server 进程的异步运行错误，包括 Admin 事务
// 提交后的 Gateway 启动失败以及受保护 goroutine 的 panic。
func (closer *gatewayBootstrapCloser) RuntimeErrors() <-chan error {
	return closer.runtimeErrors
}

// openGatewayAndBootstrap 在通用 Gateway 生命周期之外接入生产 Backup Barrier Socket。
// Barrier 必须与 Server 共用同一个 SQLite Store 和 target hash，不能另开配置来源。
func openGatewayAndBootstrap(ctx context.Context, config serverconfig.Config, resources storage, logger *slog.Logger) (io.Closer, error) {
	return openGatewayAndBootstrapAt(ctx, config, resources, logger, time.Now())
}

func openGatewayAndBootstrapAt(
	ctx context.Context,
	config serverconfig.Config,
	resources storage,
	logger *slog.Logger,
	startedAt time.Time,
) (io.Closer, error) {
	return openGatewayAndBootstrapAtTracing(ctx, config, resources, logger, startedAt, nil)
}

func openGatewayAndBootstrapAtTracing(
	ctx context.Context,
	config serverconfig.Config,
	resources storage,
	logger *slog.Logger,
	startedAt time.Time,
	traceRuntime *tracing.Runtime,
) (io.Closer, error) {
	lifecycle, err := openGatewayAndBootstrapWithStartedAtTracing(ctx, config, resources, logger, startedAt, externallock.RuntimeDirectory, func(ctx context.Context, runtimeDir, targetHash string, store *sqlite.Store, afterCreate func() error, reportRuntimeError func(error)) (io.Closer, error) {
		return openAdminBootstrapSocketAfter(ctx, runtimeDir, targetHash, store, afterCreate, reportRuntimeError)
	}, traceRuntime)
	if err != nil {
		return nil, err
	}
	closer, ok := lifecycle.(*gatewayBootstrapCloser)
	if !ok {
		return nil, errors.Join(errors.New("unexpected gateway lifecycle implementation"), lifecycle.Close())
	}
	serverResources, ok := resources.(*serverStorage)
	if !ok {
		return nil, errors.Join(errors.New("unexpected server storage implementation"), lifecycle.Close())
	}
	reportRuntimeError := func(runtimeErr error) {
		select {
		case closer.runtimeErrors <- runtimeErr:
		default:
		}
	}
	barrier, err := openBackupBarrierSocket(
		ctx,
		externallock.RuntimeDirectory,
		serverResources.targetHash,
		serverResources.database,
		reportRuntimeError,
	)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("initialize backup barrier socket: %w", err), lifecycle.Close())
	}
	closer.backupBarrier = barrier
	return closer, nil
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
	return openGatewayAndBootstrapWithStartedAt(ctx, config, resources, logger, time.Now(), runtimeDir, openBootstrapSocket)
}

func openGatewayAndBootstrapWithStartedAt(
	ctx context.Context,
	config serverconfig.Config,
	resources storage,
	logger *slog.Logger,
	startedAt time.Time,
	runtimeDir string,
	openBootstrapSocket func(context.Context, string, string, *sqlite.Store, func() error, func(error)) (io.Closer, error),
) (io.Closer, error) {
	return openGatewayAndBootstrapWithStartedAtTracing(
		ctx, config, resources, logger, startedAt, runtimeDir, openBootstrapSocket, nil,
	)
}

func openGatewayAndBootstrapWithStartedAtTracing(
	ctx context.Context,
	config serverconfig.Config,
	resources storage,
	logger *slog.Logger,
	startedAt time.Time,
	runtimeDir string,
	openBootstrapSocket func(context.Context, string, string, *sqlite.Store, func() error, func(error)) (io.Closer, error),
	traceRuntime *tracing.Runtime,
) (io.Closer, error) {
	serverResources, ok := resources.(*serverStorage)
	if !ok {
		return nil, errors.New("unexpected server storage implementation")
	}
	// SQLite 已在 openServerStorage 中完成 Migration。任何 Listener 或本机
	// Bootstrap Socket 建立前先验证全部存量 Desired State，避免 Connector
	// 在超大 Snapshot 上进入永久重连循环。
	healthBudget, err := initializeStoredSnapshotsAndHealthBudget(ctx, config, serverResources.database)
	if err != nil {
		return nil, fmt.Errorf("initialize stored state before gateway startup: %w", err)
	}
	// Gateway 身份文件仍必须晚于全部只读启动 Gate；避免 FD 预算已知不满足时
	// 在 Data Directory 留下本可避免的新身份。Lifecycle 内保留同一检查作为防线。
	if err := checkServerFDBudget(config, tcpListenerFDReserve(config.TCPIngress)); err != nil {
		return nil, fmt.Errorf("validate startup file descriptor budget: %w", err)
	}
	identity, err := loadGatewayIdentity(config, serverResources)
	if err != nil {
		return nil, err
	}
	protector, err := application.NewAES256GCMTokenProtector(serverResources.tokenMasterKey[:])
	if err != nil {
		return nil, fmt.Errorf("construct Tunnel Token protector for gateway authentication: %w", err)
	}
	tokenService := application.NewConnectionTokenService(serverResources.database, protector)
	runtimeErrors := make(chan error, 1)
	reportRuntimeError := func(err error) {
		select {
		case runtimeErrors <- err:
		default:
		}
	}
	metricsBridge := &serverMetricsBridge{}
	usageOwner, err := serverusage.New(serverusage.Options{
		Repository: &serverUsageRepository{store: serverResources.database},
		ReportError: func(error) {
			logger.Warn("usage_flush_failed", logging.ErrorCodeKey, "USAGE_FLUSH_FAILED")
		},
	})
	if err != nil {
		return nil, fmt.Errorf("construct server Usage owner: %w", err)
	}
	usageBridge := &serverUsageBridge{owner: usageOwner}
	gatewayServer, sessions, tunnelProxy, limitManager, err := openGatewayLifecycle(
		config, resources, logger, reportRuntimeError, healthBudget, identity, tokenService, metricsBridge, usageBridge, traceRuntime,
	)
	if err != nil {
		return nil, err
	}
	metricsRegistry, err := servermetrics.NewRegistry(&serverMetricsSource{
		sessions: sessions, healthBudget: healthBudget, gateway: gatewayServer,
	})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("construct Prometheus metric registry: %w", err), gatewayServer.Close())
	}
	// Registry 必须在任一 Listener/Session owner 启动前一次性发布，确保 Bridge 的
	// 数据面回调永远不会观察到 nil 或半构造状态。
	metricsBridge.registry = metricsRegistry
	routes, err := serverroute.NewManager(serverResources.database)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("construct immutable route snapshot manager: %w", err), gatewayServer.Close())
	}
	// Route owner 继承进程取消信号，使阻塞中的 SQLite 读取能及时解阻塞；取消不会
	// 清除已经原子发布的不可变快照。closer 仍会在 Listener/Session 停止后 Wait，
	// 保证 SQLite 关闭前 owner 已完整退出。
	routeContext, cancelRoutes := context.WithCancel(ctx)
	if err := routes.Start(routeContext); err != nil {
		cancelRoutes()
		routes.Wait()
		return nil, errors.Join(fmt.Errorf("load immutable route snapshot before listener startup: %w", err), gatewayServer.Close())
	}
	tcpIngress, err := newTCPIngressManager(
		config, routes, tunnelProxy, limitManager, logger, reportRuntimeError, traceRuntime,
	)
	if err != nil {
		cancelRoutes()
		routes.Wait()
		return nil, errors.Join(fmt.Errorf("construct TCP ingress listener manager: %w", err), gatewayServer.Close())
	}
	httpHandler, err := serverhttpingress.NewHandler(serverhttpingress.HandlerOptions{
		Routes: routes, Dialer: tunnelProxy, TrustedProxies: config.HTTPIngress.TrustedProxies,
		Limits: limitManager, MaxBodyBytes: config.Limits.MaxHTTPBodyBytes, Logger: logger,
		Tracing: traceRuntime,
	})
	if err != nil {
		cancelRoutes()
		routes.Wait()
		return nil, errors.Join(
			fmt.Errorf("construct HTTP ingress handler: %w", err),
			tcpIngress.Close(),
			gatewayServer.Close(),
		)
	}
	httpIngress, err := serverhttpingress.NewServer(serverhttpingress.ServerOptions{
		Listen: config.HTTPIngress.Listen, Handler: httpHandler,
		MaxHeaderBytes:     config.Limits.MaxHTTPHeaderBytes,
		ReportRuntimeError: reportRuntimeError,
	})
	if err != nil {
		cancelRoutes()
		routes.Wait()
		return nil, errors.Join(fmt.Errorf("construct HTTP ingress listener: %w", err), tcpIngress.Close(), gatewayServer.Close())
	}
	endpoint, tlsTrust, err := managementGatewayConnectionDescription(config, identity)
	if err != nil {
		cancelRoutes()
		routes.Wait()
		return nil, errors.Join(
			fmt.Errorf("construct management gateway connection description: %w", err),
			httpIngress.Close(),
			tcpIngress.Close(),
			gatewayServer.Close(),
		)
	}
	auditWriter := application.NewSecurityAuditWriter(serverResources.database, logger)
	serviceSnapshotGate, err := snapshot.New(snapshot.Config{
		ProtocolVersion:      snapshotProtocolVersion,
		MaxServices:          config.Limits.MaxServicesPerTunnel,
		MaxSnapshotBytes:     config.Limits.MaxTunnelSnapshotBytes,
		MaxControlFrameBytes: config.Limits.MaxControlFrameBytes,
	})
	if err != nil {
		cancelRoutes()
		routes.Wait()
		return nil, errors.Join(
			fmt.Errorf("construct management Service Snapshot gate: %w", err),
			httpIngress.Close(),
			tcpIngress.Close(),
			gatewayServer.Close(),
		)
	}
	reservedPorts, err := reservedTCPPorts(config)
	if err != nil {
		cancelRoutes()
		routes.Wait()
		return nil, errors.Join(
			fmt.Errorf("construct management TCP port policy: %w", err),
			httpIngress.Close(),
			tcpIngress.Close(),
			gatewayServer.Close(),
		)
	}
	tcpPolicy, err := tcpport.New(config.TCPIngress.MinPort, config.TCPIngress.MaxPort, reservedPorts)
	if err != nil {
		cancelRoutes()
		routes.Wait()
		return nil, errors.Join(
			fmt.Errorf("construct management TCP port policy: %w", err),
			httpIngress.Close(),
			tcpIngress.Close(),
			gatewayServer.Close(),
		)
	}
	serviceOwner := application.NewServiceManagementService(
		serverResources.database, serviceSnapshotGate, sessions, healthBudget,
	)
	serviceAPI := application.NewServiceAPIService(
		serviceOwner, tcpPolicy, routes, sessions, limitManager, tcpIngress,
	)
	tunnels := application.NewTunnelManagementService(
		serverResources.database, tokenService, sessions, healthBudget, endpoint, tlsTrust, config.Limits.MaxTunnels,
	)
	systemRead, err := application.NewSystemReadService(
		buildinfo.Version(),
		startedAt,
		config,
		func(checkContext context.Context) application.SystemHealthCheckResult {
			if _, checkErr := serverResources.database.HasAdmin(checkContext); checkErr != nil {
				message := "SQLite 只读检查失败"
				return application.SystemHealthCheckResult{
					Name: "sqlite", Status: application.SystemHealthDegraded, Message: &message,
				}
			}
			return application.SystemHealthCheckResult{Name: "sqlite", Status: application.SystemHealthReady}
		},
	)
	if err != nil {
		cancelRoutes()
		routes.Wait()
		return nil, errors.Join(
			fmt.Errorf("construct management system read service: %w", err),
			httpIngress.Close(),
			tcpIngress.Close(),
			gatewayServer.Close(),
		)
	}
	managementHandler, err := servermanagementapi.NewHandler(servermanagementapi.HandlerOptions{
		Management:      config.Management,
		Store:           serverResources.database,
		Tunnels:         tunnels,
		Credentials:     application.NewCredentialLifecycleService(tokenService, auditWriter),
		TunnelLifecycle: application.NewTunnelLifecycleService(serverResources.database, auditWriter, sessions),
		Services:        serviceAPI,
		System:          systemRead,
		SecurityAudits:  application.NewSecurityAuditQueryService(serverResources.database),
		Dashboard:       application.NewDashboardService(tunnels, serviceAPI, systemRead, serviceAPI),
		Logger:          logger,
	})
	if err != nil {
		cancelRoutes()
		routes.Wait()
		return nil, errors.Join(
			fmt.Errorf("construct management handler: %w", err),
			httpIngress.Close(),
			tcpIngress.Close(),
			gatewayServer.Close(),
		)
	}
	managementServer, err := servermanagementapi.NewServer(servermanagementapi.ServerOptions{
		Listen: config.Management.Listen, Handler: managementHandler,
		MaxHeaderBytes:     config.Limits.MaxHTTPHeaderBytes,
		ReportRuntimeError: reportRuntimeError,
	})
	if err != nil {
		cancelRoutes()
		routes.Wait()
		return nil, errors.Join(
			fmt.Errorf("construct management listener: %w", err),
			httpIngress.Close(),
			tcpIngress.Close(),
			gatewayServer.Close(),
		)
	}
	metricsServer, err := servermetrics.NewServer(servermetrics.ServerOptions{
		Listen: config.Metrics.Listen, Path: config.Metrics.Path,
		Registry: metricsRegistry, ReportRuntimeError: reportRuntimeError,
	})
	if err != nil {
		cancelRoutes()
		routes.Wait()
		return nil, errors.Join(
			fmt.Errorf("construct Prometheus metrics listener: %w", err),
			managementServer.Close(),
			httpIngress.Close(),
			tcpIngress.Close(),
			gatewayServer.Close(),
		)
	}
	cleanupBeforeOwnershipTransfer := func() error {
		httpHandler.CloseIdleConnections()
		metricsErr := metricsServer.Close()
		managementErr := managementServer.Close()
		httpErr := httpIngress.Close()
		tcpErr := tcpIngress.Close()
		cancelRoutes()
		routes.Wait()
		usageContext, cancelUsage := context.WithTimeout(context.Background(), 5*time.Second)
		usageErr := usageOwner.Shutdown(usageContext)
		cancelUsage()
		return errors.Join(metricsErr, managementErr, tcpErr, httpErr, gatewayServer.Close(), usageErr)
	}
	// Usage、Metrics 与 Management 必须早于 Admin Bootstrap State 检查启动。
	// SETUP_REQUIRED 时 Usage Owner 继续负责已持久化数据的 Rollup，且只保留
	// 两个运维 Listener；本机 Bootstrap 事务提交后再启动三个公网入口。
	lifecycleContext := context.WithoutCancel(ctx)
	if err := usageOwner.Start(lifecycleContext); err != nil {
		return nil, errors.Join(fmt.Errorf("start server Usage owner: %w", err), cleanupBeforeOwnershipTransfer())
	}
	if err := metricsServer.Start(lifecycleContext); err != nil {
		return nil, errors.Join(fmt.Errorf("start Prometheus metrics listener: %w", err), cleanupBeforeOwnershipTransfer())
	}
	if err := managementServer.Start(lifecycleContext); err != nil {
		return nil, errors.Join(fmt.Errorf("start management listener: %w", err), cleanupBeforeOwnershipTransfer())
	}
	startGateway := func() error {
		// 冻结启动顺序是 Route Snapshot → TCP Listener Restore → HTTP Ingress →
		// Gateway → Runtime Reconciler。TCP Handler 按准入时 Route 建立精确
		// Revision Tunnel；进程取消只触发外层关闭 owner，不能直接杀死
		// 已经准入的请求或 Session。
		if err := tcpIngress.Start(lifecycleContext); err != nil {
			startErr := errors.Join(
				fmt.Errorf("start TCP ingress after admin bootstrap: %w", err),
				tcpIngress.Close(),
				gatewayServer.Close(),
			)
			reportRuntimeError(startErr)
			return startErr
		}
		if err := httpIngress.Start(lifecycleContext); err != nil {
			startErr := errors.Join(
				fmt.Errorf("start HTTP ingress after admin bootstrap: %w", err),
				tcpIngress.Close(),
				httpIngress.Close(),
				gatewayServer.Close(),
			)
			reportRuntimeError(startErr)
			return startErr
		}
		if err := gatewayServer.Start(lifecycleContext); err != nil {
			httpHandler.CloseIdleConnections()
			startErr := errors.Join(
				fmt.Errorf("start agent gateway after HTTP ingress: %w", err),
				tcpIngress.Close(),
				httpIngress.Close(),
				gatewayServer.Close(),
			)
			reportRuntimeError(startErr)
			return startErr
		}
		// 极短窗口内到达的 HTTP 或 Control 请求因 Reconciler 未启动而 fail-closed；
		// 不查询 SQLite 热路径，也不回落到本地或旧配置。
		if err := sessions.Start(lifecycleContext); err != nil {
			drainContext, cancelDrain := context.WithTimeout(context.Background(), serverDrainTimeout)
			tcpStopErr := tcpIngress.StopAccepting()
			httpStopErr := httpIngress.StopAccepting()
			stopErr := gatewayServer.StopAccepting()
			httpHandler.CloseIdleConnections()
			shutdownErr := sessions.Shutdown(drainContext)
			tcpShutdownErr := tcpIngress.Shutdown(drainContext)
			cancelDrain()
			startErr := errors.Join(
				fmt.Errorf("start server Snapshot reconciler: %w", err),
				tcpStopErr,
				httpStopErr,
				stopErr,
				httpIngress.Close(),
				tcpShutdownErr,
				tcpIngress.Close(),
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
		return nil, errors.Join(fmt.Errorf("check admin bootstrap state before gateway start: %w", err), cleanupBeforeOwnershipTransfer())
	}
	if hasAdmin {
		if err := startGateway(); err != nil {
			return nil, errors.Join(err, cleanupBeforeOwnershipTransfer())
		}
		return &gatewayBootstrapCloser{
			gateway: gatewayServer, management: managementServer, metrics: metricsServer,
			metricsRegistry: metricsRegistry,
			httpIngress:     httpIngress, httpHandler: httpHandler, tcpIngress: tcpIngress,
			sessions: sessions, routes: routes, usage: usageOwner, cancelRoutes: cancelRoutes,
			runtimeErrors: runtimeErrors,
		}, nil
	}
	socket, err := openBootstrapSocket(ctx, runtimeDir, serverResources.targetHash, serverResources.database, startGateway, reportRuntimeError)
	if err != nil {
		return nil, errors.Join(err, cleanupBeforeOwnershipTransfer())
	}
	return &gatewayBootstrapCloser{
		bootstrap: socket, gateway: gatewayServer, management: managementServer, metrics: metricsServer,
		metricsRegistry: metricsRegistry,
		httpIngress:     httpIngress, httpHandler: httpHandler, tcpIngress: tcpIngress,
		sessions: sessions, routes: routes, usage: usageOwner, cancelRoutes: cancelRoutes,
		runtimeErrors: runtimeErrors,
	}, nil
}

func newTCPIngressManager(
	config serverconfig.Config,
	routes *serverroute.Manager,
	tunnelDialer tcpTunnelProxy,
	limitManager *serverlimits.Manager,
	logger *slog.Logger,
	reportRuntimeError func(error),
	traceRuntime *tracing.Runtime,
) (*servertcpingress.Manager, error) {
	if limitManager == nil {
		return nil, errors.New("TCP ingress source limit manager is required")
	}
	bind, err := netip.ParseAddr(config.TCPIngress.Bind)
	if err != nil {
		return nil, fmt.Errorf("parse tcp_ingress.bind: %w", err)
	}
	reserved, err := reservedTCPPorts(config)
	if err != nil {
		return nil, err
	}
	handler, err := newTCPIngressHandler(tunnelDialer, logger, traceRuntime)
	if err != nil {
		return nil, fmt.Errorf("construct TCP ingress data-plane handler: %w", err)
	}
	manager, err := servertcpingress.NewManager(servertcpingress.Options{
		Bind: bind, MinPort: config.TCPIngress.MinPort, MaxPort: config.TCPIngress.MaxPort,
		Reserved: reserved, Routes: routes, SourceLimiter: limitManager,
		Handler: handler, ReportRuntimeError: reportRuntimeError,
	})
	if err != nil {
		return nil, err
	}
	if err := routes.ObservePublished(manager.MarkDirty); err != nil {
		return nil, fmt.Errorf("observe published Route Snapshot for TCP ingress: %w", err)
	}
	return manager, nil
}

func reservedTCPPorts(config serverconfig.Config) ([]uint16, error) {
	reserved := []uint16{80, 443}
	for name, address := range map[string]string{
		"management.listen":    config.Management.Listen,
		"http_ingress.listen":  config.HTTPIngress.Listen,
		"agent_gateway.listen": config.AgentGateway.Listen,
		"metrics.listen":       config.Metrics.Listen,
	} {
		_, portText, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("parse reserved %s: %w", name, err)
		}
		port, err := strconv.ParseUint(portText, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("parse reserved %s port", name)
		}
		// 测试和显式动态绑定可以使用 :0；它不会落入合法 TCP Route 端口池，
		// 因此没有需要保留的具体端口。生产 Schema 仍禁止这些入口配置为 0。
		if port == 0 {
			continue
		}
		reserved = append(reserved, uint16(port))
	}
	return reserved, nil
}

func checkServerFDBudget(config serverconfig.Config, tcpListeners int) error {
	budget, err := serverFDBudget(config, tcpListeners)
	if err != nil {
		return err
	}
	return serverlimits.CheckFDBudget(budget)
}

// tcpListenerFDReserve 按包含两端的逻辑池容量，再加一个 A→B 原子换口候选。
// 该值只参与启动 FD Gate；实际启动仍只监听 Desired Route 中的具体端口。
func tcpListenerFDReserve(config serverconfig.TCPIngress) int {
	return config.MaxPort - config.MinPort + 2
}

func serverFDBudget(config serverconfig.Config, tcpListeners int) (serverlimits.FDBudget, error) {
	if tcpListeners < 0 {
		return serverlimits.FDBudget{}, errors.New("TCP listener count must not be negative")
	}
	return serverlimits.FDBudget{
		WorkConnections:         uint64(config.Limits.MaxWorkConnections),
		PublicActiveConnections: uint64(config.Limits.MaxActiveConnections),
		PendingOpenConnections:  uint64(config.Limits.MaxPendingOpens),
		ConnectorControls:       uint64(config.Limits.MaxConnectors),
		PendingTLSHandshakes:    uint64(config.Limits.MaxPendingTLSHandshakes),
		PendingAuth:             uint64(config.Limits.MaxPendingAuth),
		Listeners:               fdListenerReserve + uint64(tcpListeners),
		SQLite:                  fdSQLiteReserve, Management: 1, Metrics: 1, SafetyMargin: fdSafetyMargin,
	}, nil
}
