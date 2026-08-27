// Package sessionruntime 负责把一条已认证 Control Session 装配为 Server 进程内的
// 单一运行时所有权，并向 WorkConn 认证路径提供 generation-fenced 查找。
package sessionruntime

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/lifei6671/xtunnel/internal/controlsession"
	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/safego"
	servercontrolauth "github.com/lifei6671/xtunnel/internal/server/controlauth"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
	serversnapshot "github.com/lifei6671/xtunnel/internal/server/snapshot"
	"github.com/lifei6671/xtunnel/internal/server/workauth"
	serverworkpool "github.com/lifei6671/xtunnel/internal/server/workpool"
)

// SnapshotProvider 返回 Tunnel 当前完整 Desired State。实现必须尊重 ctx 取消，且
// 返回值由 sessionruntime 只读使用并交给 Control Owner 深拷贝后发送。
type SnapshotProvider interface {
	Current(context.Context, string) (serversnapshot.Result, error)
}

var (
	// ErrInvalidOptions 表示 Manager 缺少 Registry 或使用了无界容量/超时。
	ErrInvalidOptions = errors.New("server session runtime options are invalid")
	// ErrInvalidSession 表示认证结果、连接或 Session 身份不完整。
	ErrInvalidSession = errors.New("server session runtime is invalid")
	// ErrSessionSuperseded 表示调用方尝试发布的 Session 已被更高 generation 取代。
	ErrSessionSuperseded = errors.New("server session runtime was superseded")
	// ErrSessionUnavailable 表示目标 Session 已经离线或不再是 Connector 的当前代。
	ErrSessionUnavailable = errors.New("server session runtime is unavailable")
	// ErrHeartbeatTimeout 表示当前 Session 在 Server 本地超时窗口内没有收到 Heartbeat。
	ErrHeartbeatTimeout = errors.New("server control session heartbeat timed out")
	// ErrReconcilerNotRunning 表示 Snapshot Reconcile Loop 尚未启动或已经停止接收 dirty。
	ErrReconcilerNotRunning = errors.New("server snapshot reconciler is not running")
	// ErrReconcilerAlreadyStarted 表示同一个 Manager 被重复启动。
	ErrReconcilerAlreadyStarted = errors.New("server snapshot reconciler is already started")
)

const (
	// V0.1 的默认 Pool 目标由技术方案冻结为 target_idle=8。初始 Session 没有
	// Opening，因此 desired_non_active 直接使用该绝对目标并由 Pool 容量再钳制。
	initialDesiredNonActive   = uint32(8)
	initialLeaseTTL           = 10 * time.Second
	defaultHeartbeatTimeout   = 30 * time.Second
	initialDemandFailedReason = "initial_demand_failed"
)

// Options 固定每条 Control Session 的队列、写超时、Replay 内存上限与心跳超时。
type Options struct {
	HighPriorityCapacity int
	NormalCapacity       int
	InboundCapacity      int
	WriteTimeout         time.Duration
	MaxReplayEntries     int
	MaxWorkTotal         uint32
	MaxWorkConnecting    uint32
	MaxControlFrameBytes uint64
	LimitManager         *serverlimits.Manager
	// SnapshotProvider 是每条 Session 第一条业务消息的唯一完整配置来源。
	SnapshotProvider SnapshotProvider
	// HeartbeatTimeout 使用 Server 本地单调时间关闭失联 Session；零值使用冻结的 30 秒默认值。
	HeartbeatTimeout time.Duration
	// Logger 接收稳定 Connector lifecycle 事件；nil 只用于不需要日志的隔离测试。
	Logger *slog.Logger
	// ReportRuntimeError 把 Reconcile goroutine panic 或不可恢复的不变量错误交给进程 owner。
	ReportRuntimeError func(error)
}

// MetricsSnapshot 是 M2 Runtime 的无 Label 聚合快照；M6 只负责把这些字段导出到
// 同名 Prometheus 指标，不在这里启动第二个 HTTP 或采集生命周期。
type MetricsSnapshot struct {
	XTunnelConnectorsOnline         uint64
	XTunnelControlSessionsOnline    uint64
	XTunnelActiveConnections        uint64
	XTunnelTCPIdleWorkConnections   uint64
	XTunnelTCPActiveWorkConnections uint64
}

// connectorKey 标识一个 Tunnel 内的稳定 Connector；generation 由映射值携带，
// 因而 replacement 可以原子替换同一个 key 的当前代。
type connectorKey struct {
	tunnelID    string
	connectorID string
}

// managedSession 聚合一条 Control Session 的全部进程内 owner。字段按职责分别由
// Manager.mu、terminationMu、configMu、reconcileMu 和 demandMu 保护；外部资源的
// 最终关闭只允许 cleanupManaged 经 cleanupOnce 执行。
type managedSession struct {
	session       serverruntime.Session
	metadata      serverruntime.ConnectorMetadata
	authenticator *workauth.SessionAuthenticator
	owner         *controlsession.Owner
	pool          *serverworkpool.Pool
	cancel        context.CancelFunc
	protocol      uint32

	terminationMu     sync.Mutex
	terminationReason string
	cleanupOnce       sync.Once
	cleanupDone       chan struct{}
	cleanupErr        error

	configMu            sync.Mutex
	lastHeartbeatAt     time.Duration
	configReady         bool
	hasObserved         bool
	observedRevision    uint64
	observedDigest      [sha256.Size]byte
	outstanding         *snapshotCandidate
	pending             *snapshotCandidate
	hasRejected         bool
	rejectedRevision    uint64
	initialMinimum      uint64
	initialDone         chan struct{}
	initialOnce         sync.Once
	initialErr          error
	serviceRequirements map[string]serviceRequirement
	serviceHealth       map[string]serviceHealthState
	healthGeneration    uint64

	reconcileMu          sync.Mutex
	demandMu             sync.Mutex
	pendingOpens         uint32
	demandDesired        uint32
	demandSlotsRemaining uint32
	demandExhausted      bool
	demandDeadline       time.Duration
}

// startupGroup 跟踪同一 Tunnel 尚在 Owner.Start 到 install 之间的短暂窗口。
// done 在最后一个预留结束时关闭，供 Revoke/Shutdown 在锁外等待。
type startupGroup struct {
	count int
	done  chan struct{}
}

// Manager 是 Control Owner、Work Authenticator 与 Runtime Registry 之间的唯一桥梁。
//
// byConnector 只发布每个 Connector 的最高 generation；bySession 只包含仍可接受新
// WorkHello 的当前代。替换 Session 时先在锁内撤下旧代，再在锁外 Cancel，避免旧代
// 的异步清理删除新代或让新 WorkConn 继续命中旧 Secret。
type Manager struct {
	mu sync.Mutex
	// 锁顺序固定为 Manager.mu -> snapshotMu -> managedSession.configMu。
	snapshotMu sync.Mutex

	registry       *serverruntime.Registry
	options        Options
	startedAt      time.Time
	demandLeaseTTL time.Duration
	byConnector    map[connectorKey]*managedSession
	bySession      map[string]*managedSession
	// liveSessions 保存从 install 成功到 Serve cleanup 完成之间的全部 generation。
	// byConnector/bySession 只承担 Current 查找，不能用来枚举 Revoke/Shutdown 的关闭目标。
	liveSessions                 map[*managedSession]struct{}
	startingByTunnel             map[string]*startupGroup
	revokedTunnels               map[string]struct{}
	logger                       *slog.Logger
	beforeInstallForTest         func(serverruntime.Session)
	afterConvergenceFenceForTest func(string)
	afterInstallForTest          func(serverruntime.Session)
	afterRemoveForTest           func(serverruntime.Session)
	beforeInitialDemandForTest   func(*managedSession)
	beforeCleanupForTest         func(serverruntime.Session)
	beforeStatusFenceForTest     func(serverruntime.Session)
	shutdownStarted              bool
	shutdownDone                 chan struct{}
	shutdownErr                  error
	snapshotStarted              bool
	snapshotAccepting            bool
	snapshotGeneration           uint64
	snapshotDirty                map[string]uint64
	snapshotFailures             map[string]error
	snapshotWake                 chan struct{}
	snapshotCancel               context.CancelFunc
	snapshotDone                 chan struct{}
	snapshotErr                  error
}

// New 创建空的 Session 运行时管理器。startedAt 仅用于构造单调时钟，不会参与
// Agent/Server wall clock 比较。
func New(registry *serverruntime.Registry, options Options) (*Manager, error) {
	if options.MaxControlFrameBytes == 0 {
		options.MaxControlFrameBytes = frame.MaxControlFrameSize
	}
	if options.HeartbeatTimeout == 0 {
		options.HeartbeatTimeout = defaultHeartbeatTimeout
	}
	if registry == nil || options.HighPriorityCapacity <= 0 || options.NormalCapacity <= 0 ||
		options.InboundCapacity <= 0 || options.WriteTimeout <= 0 || options.MaxReplayEntries <= 0 ||
		options.MaxWorkTotal == 0 || options.MaxWorkConnecting == 0 ||
		options.MaxWorkConnecting > options.MaxWorkTotal || options.MaxControlFrameBytes > frame.MaxControlFrameSize ||
		options.HeartbeatTimeout <= 0 || nilSnapshotProvider(options.SnapshotProvider) {
		return nil, ErrInvalidOptions
	}
	return &Manager{
		registry: registry, options: options, startedAt: time.Now(), demandLeaseTTL: initialLeaseTTL,
		byConnector:      make(map[connectorKey]*managedSession),
		bySession:        make(map[string]*managedSession),
		liveSessions:     make(map[*managedSession]struct{}),
		startingByTunnel: make(map[string]*startupGroup),
		revokedTunnels:   make(map[string]struct{}), logger: options.Logger,
		shutdownDone:     make(chan struct{}),
		snapshotDirty:    make(map[string]uint64),
		snapshotFailures: make(map[string]error),
		snapshotWake:     make(chan struct{}, 1),
	}, nil
}

// Serve 接管一次已完成 Success flush 的 Control 连接，直到 Owner 退出。
//
// established 使用指针传入，是为了在 Work Authenticator 复制 Secret 后立即清除
// 调用方持有的那一份数组。成功启动 Owner 后连接所有权转移给 Owner；任何安装失败
// 都会关闭连接并执行 generation-fenced Registry 清理。
func (manager *Manager) Serve(ctx context.Context, connection net.Conn, established *servercontrolauth.Established) error {
	if manager == nil || ctx == nil || connection == nil || established == nil ||
		!validSession(established.Session) || established.Control == nil || established.ProtocolVersion == 0 {
		return ErrInvalidSession
	}
	sessionContext, managed, err := manager.prepareSession(ctx, connection, established)
	if err != nil {
		return err
	}
	previous, err := manager.startSession(sessionContext, connection, managed)
	if err != nil {
		return err
	}
	if previous != nil {
		// previous 已从 Current/WorkAuth 查找入口摘除，但仍由 liveSessions 跟踪，
		// 因而并发 Revoke/Shutdown 也能关闭它。这里必须在任何后续失败前接管替换清理。
		previous.setTerminationReason("session_replaced")
		_ = manager.cleanupManaged(previous, cleanupNonActive)
	}
	if err := manager.awaitInitialSnapshot(sessionContext, managed); err != nil {
		return manager.abortInstalledSession(managed, err)
	}
	return manager.waitForSession(ctx, sessionContext, managed)
}

// prepareSession 从认证结果构造 Work Authenticator、WorkPool 与 Control Owner，
// 并在每个失败分支逆序关闭已创建资源。Session Secret 在复制后立即从认证结果清零。
func (manager *Manager) prepareSession(
	ctx context.Context,
	connection net.Conn,
	established *servercontrolauth.Established,
) (context.Context, *managedSession, error) {
	authenticator, err := workauth.New(workauth.Session{
		TunnelID:    established.Session.TunnelID,
		ConnectorID: established.Session.ConnectorID,
		SessionID:   established.Session.SessionID,
		Generation:  established.Session.Generation,
		Secret:      established.SessionSecret[:],
	}, manager.options.MaxReplayEntries, func() time.Duration {
		return time.Since(manager.startedAt)
	})
	// Session Secret 只允许在 Control AUTH 返回值与 Authenticator 内存中短暂停留；
	// 无论构造成功与否，都立即清掉调用方数组，避免生命周期上层意外长期持有。
	clear(established.SessionSecret[:])
	if err != nil {
		manager.registry.ClearIfCurrent(established.Session)
		return nil, nil, errors.Join(fmt.Errorf("construct server Work authenticator: %w", err), connection.Close())
	}
	pool, err := serverworkpool.New(serverworkpool.Options{
		Session: serverworkpool.Session{
			TunnelID:    established.Session.TunnelID,
			ConnectorID: established.Session.ConnectorID,
			SessionID:   established.Session.SessionID,
			Generation:  established.Session.Generation,
		},
		MaxTotal:      manager.options.MaxWorkTotal,
		MaxConnecting: manager.options.MaxWorkConnecting,
		LimitManager:  manager.options.LimitManager,
		Clock:         func() time.Duration { return time.Since(manager.startedAt) },
		DeadlineNow:   time.Now,
	})
	if err != nil {
		authenticator.Close()
		manager.registry.ClearIfCurrent(established.Session)
		return nil, nil, errors.Join(fmt.Errorf("construct server WorkPool: %w", err), connection.Close())
	}

	owner, err := controlsession.NewOwner(connection, established.Control, controlsession.Options{
		ProtocolVersion:      established.ProtocolVersion,
		HighPriorityCapacity: manager.options.HighPriorityCapacity,
		NormalCapacity:       manager.options.NormalCapacity,
		InboundCapacity:      manager.options.InboundCapacity,
		WriteTimeout:         manager.options.WriteTimeout,
		MaxFrameBytes:        manager.options.MaxControlFrameBytes,
	})
	if err != nil {
		authenticator.Close()
		_ = pool.Close()
		manager.registry.ClearIfCurrent(established.Session)
		return nil, nil, errors.Join(fmt.Errorf("construct server Control Session owner: %w", err), connection.Close())
	}

	sessionContext, cancel := context.WithCancel(ctx)
	managed := &managedSession{
		session:         established.Session,
		metadata:        established.ConnectorMetadata,
		authenticator:   authenticator,
		owner:           owner,
		pool:            pool,
		cancel:          cancel,
		protocol:        established.ProtocolVersion,
		cleanupDone:     make(chan struct{}),
		initialDone:     make(chan struct{}),
		initialMinimum:  established.DesiredRevision,
		lastHeartbeatAt: time.Since(manager.startedAt),
	}
	return sessionContext, managed, nil
}

// startSession 先登记 startup fence，再启动 Owner 并原子发布最高 generation。
// install 失败时必须等待 Owner 退出后才释放预留，防止 Revoke/Shutdown 越过连接。
func (manager *Manager) startSession(
	sessionContext context.Context,
	connection net.Conn,
	managed *managedSession,
) (*managedSession, error) {
	if err := manager.beginStartup(managed.session.TunnelID); err != nil {
		managed.cancel()
		managed.authenticator.Close()
		_ = managed.pool.Close()
		manager.registry.ClearIfCurrent(managed.session)
		return nil, errors.Join(err, connection.Close())
	}
	if err := managed.owner.Start(sessionContext); err != nil {
		managed.cancel()
		managed.authenticator.Close()
		_ = managed.pool.Close()
		manager.registry.ClearIfCurrent(managed.session)
		startErr := errors.Join(fmt.Errorf("start server Control Session owner: %w", err), connection.Close())
		manager.endStartup(managed.session.TunnelID)
		return nil, startErr
	}
	if manager.beforeInstallForTest != nil {
		manager.beforeInstallForTest(managed.session)
	}

	previous, installErr := manager.install(managed)
	if installErr != nil {
		managed.cancel()
		ownerErr := managed.owner.Wait()
		managed.authenticator.Close()
		_ = managed.pool.Close()
		manager.registry.ClearIfCurrent(managed.session)
		manager.endStartup(managed.session.TunnelID)
		return nil, errors.Join(installErr, ownerErr)
	}
	manager.endStartup(managed.session.TunnelID)
	if manager.afterInstallForTest != nil {
		manager.afterInstallForTest(managed.session)
	}
	return previous, nil
}

// abortInstalledSession 收敛已经发布、但初始 Snapshot 或 Demand 建立失败的 Session。
// 它先停止 Owner，再撤下查找入口，最后关闭全部 Work 资源。
func (manager *Manager) abortInstalledSession(managed *managedSession, cause error) error {
	managed.cancel()
	ownerErr := managed.owner.Wait()
	manager.removeLookup(managed)
	cleanupErr := manager.cleanupManaged(managed, cleanupAll)
	return errors.Join(cause, ownerErr, cleanupErr)
}

// waitForSession 同时等待唯一 Control Owner 与唯一入站消费者退出，并在二者都停止后
// 撤下 Current 查找、触发 Snapshot 重算和 generation-fenced 资源清理。
func (manager *Manager) waitForSession(
	ctx context.Context,
	sessionContext context.Context,
	managed *managedSession,
) error {
	// Manager 持续消费已通过状态机校验的入站消息。DrainRequest 必须在同一
	// Session Owner 内完成摘流、等待 OPENING 与 Ack，不能像普通 Heartbeat 一样丢弃。
	inboundDrained := make(chan struct{})
	inboundErrors := make(chan error, 1)
	safego.Go(func(err error) {
		managed.setTerminationReason("inbound_panic")
		inboundErrors <- fmt.Errorf("consume server Control inbound: %w", err)
		managed.cancel()
	}, func() {
		close(inboundDrained)
	}, func() {
		if err := manager.consumeInbound(sessionContext, managed); err != nil {
			inboundErrors <- err
			managed.cancel()
		}
	})

	ownerErr := managed.owner.Wait()
	// Owner 已经终止时不再可能写出 DrainAck；先取消 Session Context，立即打断
	// 可能正按对端 drain_timeout_ms 等待 OPENING 的入站消费者，再等待其退出。
	managed.cancel()
	<-inboundDrained
	var inboundErr error
	select {
	case inboundErr = <-inboundErrors:
	default:
	}
	manager.removeLookup(managed)
	if err := manager.MarkDirty(managed.session.TunnelID); err != nil && !errors.Is(err, ErrReconcilerNotRunning) {
		manager.reportSnapshotRuntimeError(err)
	}
	if manager.afterRemoveForTest != nil {
		manager.afterRemoveForTest(managed.session)
	}
	cleanupMode := cleanupNonActive
	if ctx.Err() != nil {
		cleanupMode = cleanupAll
	}
	cleanupErr := manager.cleanupManaged(managed, cleanupMode)
	return errors.Join(ownerErr, inboundErr, cleanupErr)
}

// consumeInbound 是 Control Owner 入站队列的唯一消费者。它串行处理 Heartbeat、
// ConfigAck、Health 与 Drain，从而保持协议顺序，并用本地 Timer 关闭失联 Session。
func (manager *Manager) consumeInbound(sessionContext context.Context, managed *managedSession) error {
	heartbeatTimer := time.NewTimer(manager.options.HeartbeatTimeout)
	defer heartbeatTimer.Stop()
	var drainAck *protocolv1.ControlEnvelope
	for {
		select {
		case <-heartbeatTimer.C:
			managed.setTerminationReason("heartbeat_timeout")
			return ErrHeartbeatTimeout
		case inbound, ok := <-managed.owner.Inbound():
			if !ok {
				return nil
			}
			var err error
			drainAck, err = manager.handleInbound(sessionContext, managed, heartbeatTimer, drainAck, inbound)
			if err != nil {
				return err
			}
		}
	}
}

// handleInbound 按互斥 Payload 类型分发已通过 Control 状态机校验的消息。
// DrainAck 会保留给重复 DrainRequest 重放，其他重复副作用由各处理器显式忽略。
func (manager *Manager) handleInbound(
	sessionContext context.Context,
	managed *managedSession,
	heartbeatTimer *time.Timer,
	drainAck *protocolv1.ControlEnvelope,
	inbound controlsession.Inbound,
) (*protocolv1.ControlEnvelope, error) {
	if inbound.Envelope.GetHeartbeat() != nil {
		managed.observeHeartbeat(time.Since(manager.startedAt))
		resetTimer(heartbeatTimer, manager.options.HeartbeatTimeout)
		return drainAck, manager.handleHeartbeat(managed)
	}
	if inbound.Envelope.GetConfigAck() != nil {
		return drainAck, manager.handleConfigAck(managed, inbound)
	}
	if inbound.Envelope.GetServiceHealthBatch() != nil {
		return drainAck, manager.handleHealthBatch(managed, inbound)
	}
	request := inbound.Envelope.GetDrainRequest()
	if request == nil {
		return drainAck, nil
	}
	if inbound.Duplicate && drainAck != nil {
		return drainAck, enqueueDrainAck(managed.owner, drainAck, true)
	}
	return manager.handleDrain(sessionContext, managed, heartbeatTimer, request)
}

// handleHeartbeat 只在配置已经 APPLIED 后刷新 Runtime 观测并调整 WorkDemand。
// Registry fence 失败表示当前代已被替换，必须结束旧 Owner。
func (manager *Manager) handleHeartbeat(managed *managedSession) error {
	if !managed.isConfigReady() {
		return nil
	}
	if !manager.registry.ObserveHeartbeat(managed.session) {
		managed.setTerminationReason("session_replaced")
		return ErrSessionSuperseded
	}
	if _, err := manager.reconcileDemand(managed, managed.protocol); err != nil {
		return fmt.Errorf("reconcile WorkDemand after Heartbeat: %w", err)
	}
	return nil
}

// handleConfigAck 把 Ack 关联到唯一 outstanding Snapshot；首次 APPLIED 必须先发布
// Lifecycle，再开放 configReady、Eligibility 与 WorkDemand，避免数据面抢跑。
func (manager *Manager) handleConfigAck(managed *managedSession, inbound controlsession.Inbound) error {
	// Owner 已经通过 message_id 确认这是一个已处理 Ack 的重放。即使此时已经
	// 下发了更高 Revision，也不能让旧 Ack 与新 outstanding 建立错误关联。
	if inbound.Duplicate {
		return nil
	}
	applied, becameReady, next, err := managed.acceptConfigAck(inbound.Envelope.GetConfigAck())
	if err != nil {
		return err
	}
	if next != nil {
		if err := manager.enqueueSnapshot(managed, next); err != nil {
			return err
		}
	}
	if !applied {
		return nil
	}
	if !becameReady {
		if !manager.publishEligibility(managed) {
			return ErrSessionUnavailable
		}
		return nil
	}
	lifecycleEvent, observed := manager.registry.ObserveConnected(managed.session, managed.metadata)
	if !observed {
		return ErrSessionUnavailable
	}
	manager.logLifecycle(lifecycleEvent)
	managed.markConfigReady()
	if !manager.publishEligibility(managed) {
		return ErrSessionUnavailable
	}
	if manager.beforeInitialDemandForTest != nil {
		manager.beforeInitialDemandForTest(managed)
	}
	if _, err := manager.reconcileDemand(managed, managed.protocol); err != nil {
		managed.setTerminationReason(initialDemandFailedReason)
		return err
	}
	return nil
}

// handleDrain 先从新 Work 选择路径摘除 Session，再等待 OPENING 收敛并关闭非 ACTIVE。
// DrainAck 反映等待完成时的剩余 ACTIVE 数；ACTIVE 继续由其数据面 owner 自然关闭。
func (manager *Manager) handleDrain(
	sessionContext context.Context,
	managed *managedSession,
	heartbeatTimer *time.Timer,
	request *protocolv1.DrainRequest,
) (*protocolv1.ControlEnvelope, error) {
	manager.markDraining(managed)
	if err := managed.pool.BeginDrain(); err != nil && !errors.Is(err, serverworkpool.ErrPoolDraining) {
		return nil, fmt.Errorf("begin server WorkPool drain: %w", err)
	}
	stopTimer(heartbeatTimer)
	drainContext, cancelDrain := context.WithTimeout(sessionContext, time.Duration(request.GetDrainTimeoutMs())*time.Millisecond)
	remainingActive, err := managed.pool.WaitOpeningAndCloseNonActive(drainContext)
	cancelDrain()
	if err != nil {
		return nil, fmt.Errorf("close non-active WorkConn during drain: %w", err)
	}
	drainAck := &protocolv1.ControlEnvelope{
		ProtocolVersion: managed.protocol,
		Payload: &protocolv1.ControlEnvelope_DrainAck{DrainAck: &protocolv1.DrainAck{
			DrainId: request.GetDrainId(), RemainingActive: remainingActive,
		}},
	}
	if err := enqueueDrainAck(managed.owner, drainAck, false); err != nil {
		return nil, err
	}
	heartbeatTimer.Reset(manager.options.HeartbeatTimeout)
	return drainAck, nil
}

// enqueueDrainAck 统一普通与重复 DrainRequest 的发送错误上下文。
func enqueueDrainAck(owner *controlsession.Owner, drainAck *protocolv1.ControlEnvelope, duplicate bool) error {
	if err := owner.Enqueue(drainAck); err != nil {
		if duplicate {
			return fmt.Errorf("enqueue duplicate DrainAck: %w", err)
		}
		return fmt.Errorf("enqueue DrainAck: %w", err)
	}
	return nil
}

// Shutdown 停止发布新的 Session/Work，收束当前 Session 的非 ACTIVE Work，等待所有
// generation 的 ACTIVE 自然结束，并在 ctx 到期后强制关闭残留 ACTIVE。调用方应先
// 停止 Gateway Listener；本方法返回后再关闭 Control Session 和 Gateway handlers。
func (manager *Manager) Shutdown(ctx context.Context) error {
	if manager == nil || ctx == nil {
		return ErrInvalidOptions
	}

	manager.mu.Lock()
	if manager.shutdownStarted {
		done := manager.shutdownDone
		manager.mu.Unlock()
		select {
		case <-done:
			manager.mu.Lock()
			err := manager.shutdownErr
			manager.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	manager.shutdownStarted = true
	manager.snapshotMu.Lock()
	manager.snapshotAccepting = false
	reconcileCancel := manager.snapshotCancel
	reconcileDone := manager.snapshotDone
	manager.snapshotMu.Unlock()
	managedSessions := make([]*managedSession, 0, len(manager.liveSessions))
	for managed := range manager.liveSessions {
		managed.setConvergenceReason("server_shutdown")
		managedSessions = append(managedSessions, managed)
	}
	startupDone := make([]<-chan struct{}, 0, len(manager.startingByTunnel))
	for _, group := range manager.startingByTunnel {
		startupDone = append(startupDone, group.done)
	}
	clear(manager.bySession)
	manager.mu.Unlock()
	if reconcileCancel != nil {
		reconcileCancel()
	}
	var shutdownErr error
	if reconcileDone != nil {
		select {
		case <-reconcileDone:
		case <-ctx.Done():
			shutdownErr = errors.Join(shutdownErr, ctx.Err())
			<-reconcileDone
		}
		manager.snapshotMu.Lock()
		shutdownErr = errors.Join(shutdownErr, manager.snapshotErr)
		manager.snapshotMu.Unlock()
	}
	if manager.afterConvergenceFenceForTest != nil {
		manager.afterConvergenceFenceForTest("shutdown")
	}
	for _, done := range startupDone {
		<-done
	}

	for _, managed := range managedSessions {
		managed.authenticator.Close()
		if err := managed.pool.BeginDrain(); err != nil && !errors.Is(err, serverworkpool.ErrPoolDraining) &&
			!errors.Is(err, serverworkpool.ErrPoolClosed) {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("begin server WorkPool shutdown drain: %w", err))
		}
	}
	for _, managed := range managedSessions {
		if _, err := managed.pool.WaitOpeningAndCloseNonActive(ctx); err != nil &&
			!errors.Is(err, serverworkpool.ErrPoolClosed) {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("drain server WorkPool before shutdown: %w", err))
		}
	}
	shutdownErr = errors.Join(shutdownErr, manager.registry.DrainActive(ctx))
	for _, managed := range managedSessions {
		// DrainActive 已关闭到期的 ACTIVE Transport；自然完成的 Handler 也会自行
		// 归还 Pool Work。统一 cleanup 只关闭非 ACTIVE，并让并发 Serve/Revoke
		// 等待同一个完成点，避免第二个收敛调用在首个 Close 尚未结束时提前成功。
		shutdownErr = errors.Join(shutdownErr, manager.cleanupManaged(managed, cleanupNonActive))
	}

	manager.mu.Lock()
	manager.shutdownErr = shutdownErr
	close(manager.shutdownDone)
	manager.mu.Unlock()
	return shutdownErr
}

// Resolve 返回仍是当前代且可接收新 WorkHello 的 Authenticator。
// 返回的对象可并发使用；Session 被替换后，后续 Resolve 会立即失败，已取得对象的
// 并发调用则由 Authenticator.Close 与其内部锁安全收束。
func (manager *Manager) Resolve(sessionID string) (*workauth.SessionAuthenticator, bool) {
	if manager == nil || identity.ValidateSessionID(sessionID) != nil {
		return nil, false
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	managed, exists := manager.bySession[sessionID]
	if !exists || !managed.isConfigReady() {
		return nil, false
	}
	return managed.authenticator, true
}

// ResolveSessionAuthenticator 直接满足 workauth.Resolver，使 Gateway Work Handler
// 无需预读 WorkHello，也无需通过 bootstrap 闭包暴露 Manager 的内部 Map。
func (manager *Manager) ResolveSessionAuthenticator(sessionID string) (*workauth.SessionAuthenticator, bool) {
	return manager.Resolve(sessionID)
}

// GrantLease 把 Control Owner 已确认的 Budget Lease 发布给指定当前 Session。
func (manager *Manager) GrantLease(sessionID, leaseID string, slots uint32, ttl time.Duration) error {
	authenticator, exists := manager.Resolve(sessionID)
	if !exists {
		return ErrSessionUnavailable
	}
	return authenticator.GrantLease(leaseID, slots, ttl)
}

// RegisterIdle 把 READY 成功的 WorkConn 发布进其当前 Session 的 IDLE FIFO。
// Session 在认证与发布之间被替换时会快速失败，调用方仍拥有并必须关闭连接。
func (manager *Manager) RegisterIdle(connection net.Conn, idle workauth.Idle) (*serverworkpool.Work, error) {
	if manager == nil || connection == nil || idle.State == nil {
		return nil, ErrSessionUnavailable
	}
	manager.mu.Lock()
	managed := manager.bySession[idle.SessionID]
	if managed == nil || managed.session.TunnelID != idle.TunnelID ||
		managed.session.ConnectorID != idle.ConnectorID || !managed.isConfigReady() {
		manager.mu.Unlock()
		return nil, ErrSessionUnavailable
	}
	pool := managed.pool
	manager.mu.Unlock()
	work, err := pool.RegisterConnecting(idle.WorkID, connection)
	if err != nil {
		return nil, err
	}
	if err := work.AttachProtocolState(idle.State); err != nil {
		return nil, errors.Join(err, work.Close())
	}
	if err := work.MarkIdle(); err != nil {
		return nil, errors.Join(err, work.Close())
	}
	managed.consumeDemandSlot()
	return work, nil
}

// Pool 返回完整身份仍为当前代的 per-Session WorkPool。
func (manager *Manager) Pool(session serverruntime.Session) (*serverworkpool.Pool, bool) {
	if manager == nil || !validSession(session) {
		return nil, false
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	managed := manager.bySession[session.SessionID]
	if managed == nil || managed.session != session || !managed.isConfigReady() {
		return nil, false
	}
	return managed.pool, true
}

// Pools 返回当前已 Ready Session 到 WorkPool 的不可变索引副本。Tunnel Proxy 在
// 进入 TunnelRuntime.mu 前取得该副本，使选择 predicate 只读取 Pool，不回调 Manager。
func (manager *Manager) Pools() map[serverruntime.Session]*serverworkpool.Pool {
	if manager == nil {
		return nil
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	pools := make(map[serverruntime.Session]*serverworkpool.Pool, len(manager.bySession))
	for _, managed := range manager.bySession {
		if managed.isConfigReady() {
			pools[managed.session] = managed.pool
		}
	}
	return pools
}

// Eligible 转发到 TunnelRuntime 的单一线性化门禁。Manager
// 只负责把 Control Session 观测到的配置与 Health 发布为值型快照。
func (manager *Manager) Eligible(session serverruntime.Session, serviceID string) bool {
	if manager == nil || manager.registry == nil || !validSession(session) || identity.ValidateServiceID(serviceID) != nil {
		return false
	}
	return manager.registry.Eligible(session, serviceID)
}

// ConnectorSnapshots 返回 Registry 生命周期快照，并只用完整 Session identity
// 叠加对应 WorkPool。replacement 前后的 Pool 永远不会混入同一个快照。
func (manager *Manager) ConnectorSnapshots() []serverruntime.ConnectorSnapshot {
	if manager == nil || manager.registry == nil {
		return nil
	}
	snapshots := manager.registry.ConnectorSnapshots()
	manager.mu.Lock()
	pools := make(map[serverruntime.Session]*serverworkpool.Pool, len(manager.byConnector))
	for _, managed := range manager.byConnector {
		if !managed.isConfigReady() {
			continue
		}
		pools[managed.session] = managed.pool
	}
	manager.mu.Unlock()
	for index := range snapshots {
		pool := pools[snapshots[index].Session]
		if pool == nil || snapshots[index].Tombstone {
			continue
		}
		counts := pool.Snapshot()
		snapshots[index].WorkPool = serverruntime.ConnectorWorkPoolSnapshot{
			Connecting: counts.Connecting, Idle: counts.Idle, Opening: counts.Opening,
			Active: counts.Active, Total: counts.Total, Closed: counts.Closed, Draining: counts.Draining,
		}
		switch {
		case counts.Draining:
			snapshots[index].Status = serverruntime.ConnectorStatusDraining
		case counts.Closed:
			snapshots[index].Status = serverruntime.ConnectorStatusDegraded
		}
	}
	return snapshots
}

// RuntimeStatusSnapshots 先在 Manager.mu 下复制候选 Session 引用，随后读取本地
// Heartbeat 与 Pool，最后由 Registry 在同一个 Runtime 临界区取得 Lifecycle、已发布
// Eligibility 并按完整 Session identity 执行 Current fence。Manager 锁不跨其他
// owner；replacement/revoke 窗口中的旧代在最终 fence 被丢弃，返回值只包含某个
// 真实 Current generation 的自有状态。
func (manager *Manager) RuntimeStatusSnapshots() []serverruntime.SessionStatusSnapshot {
	if manager == nil || manager.registry == nil {
		return nil
	}
	now := time.Since(manager.startedAt)
	manager.mu.Lock()
	managedSessions := make([]*managedSession, 0, len(manager.byConnector))
	for _, managed := range manager.byConnector {
		managedSessions = append(managedSessions, managed)
	}
	manager.mu.Unlock()
	snapshots := make([]serverruntime.SessionStatusSnapshot, 0, len(managedSessions))
	for _, managed := range managedSessions {
		heartbeatFresh := managed.heartbeatFresh(now, manager.options.HeartbeatTimeout)
		counts := managed.pool.Snapshot()
		if manager.beforeStatusFenceForTest != nil {
			manager.beforeStatusFenceForTest(managed.session)
		}
		lifecycle, config, current, observed := manager.registry.CurrentConnectorSnapshot(managed.session)
		if !current {
			continue
		}
		snapshot := serverruntime.SessionStatusSnapshot{
			Session: managed.session, ConnectorMetadata: managed.metadata,
			CurrentControlSession: true, HeartbeatFresh: heartbeatFresh,
			LifecycleStatus: lifecycle.Status, Config: config,
			WorkPool: serverruntime.ConnectorWorkPoolSnapshot{
				Connecting: counts.Connecting, Idle: counts.Idle, Opening: counts.Opening,
				Active: counts.Active, Total: counts.Total, Closed: counts.Closed, Draining: counts.Draining,
			},
		}
		if observed {
			snapshot.ConnectorMetadata = lifecycle.ConnectorMetadata
			snapshot.ConnectedAt = lifecycle.ConnectedAt
			snapshot.LastHeartbeatAt = lifecycle.LastHeartbeatAt
		}
		snapshots = append(snapshots, snapshot)
	}
	slices.SortFunc(snapshots, func(left, right serverruntime.SessionStatusSnapshot) int {
		if order := cmp.Compare(left.TunnelID, right.TunnelID); order != 0 {
			return order
		}
		return cmp.Compare(left.ConnectorID, right.ConnectorID)
	})
	return snapshots
}

// MetricsSnapshot 返回 M2 冻结的五个无高基数 Label Gauge 输入。
func (manager *Manager) MetricsSnapshot() MetricsSnapshot {
	var metrics MetricsSnapshot
	for _, snapshot := range manager.ConnectorSnapshots() {
		metrics.XTunnelActiveConnections += snapshot.ActiveWork
		metrics.XTunnelTCPActiveWorkConnections += snapshot.ActiveWork
		if snapshot.Tombstone {
			continue
		}
		metrics.XTunnelControlSessionsOnline++
		if snapshot.Status == serverruntime.ConnectorStatusOnline {
			metrics.XTunnelConnectorsOnline++
		}
		metrics.XTunnelTCPIdleWorkConnections += uint64(snapshot.WorkPool.Idle)
	}
	return metrics
}

// RevokeTunnel 先在 Manager 锁内建立永久 fence 并摘除全部 Session 查找入口，
// 再在锁外撤销 Runtime、Authenticator、Pool 和 Control Owner。
func (manager *Manager) RevokeTunnel(tunnelID string) error {
	if manager == nil || identity.ValidateTunnelID(tunnelID) != nil {
		return ErrSessionUnavailable
	}
	manager.mu.Lock()
	manager.revokedTunnels[tunnelID] = struct{}{}
	startupDone := manager.startupDoneLocked(tunnelID)
	managedSessions := make([]*managedSession, 0)
	for managed := range manager.liveSessions {
		if managed.session.TunnelID != tunnelID {
			continue
		}
		managed.setConvergenceReason("tunnel_revoked")
		managedSessions = append(managedSessions, managed)
		key := connectorKey{tunnelID: managed.session.TunnelID, connectorID: managed.session.ConnectorID}
		if manager.byConnector[key] == managed {
			delete(manager.byConnector, key)
		}
		if manager.bySession[managed.session.SessionID] == managed {
			delete(manager.bySession, managed.session.SessionID)
		}
	}
	manager.mu.Unlock()
	if manager.afterConvergenceFenceForTest != nil {
		manager.afterConvergenceFenceForTest("revoke")
	}
	if startupDone != nil {
		<-startupDone
	}
	disconnectEvents, revokeErr := manager.registry.RevokeTunnelWithLifecycle(tunnelID)
	for _, event := range disconnectEvents {
		manager.logLifecycle(event)
	}
	for _, managed := range managedSessions {
		revokeErr = errors.Join(revokeErr, manager.cleanupManaged(managed, cleanupNonActive))
	}
	return revokeErr
}

// SetPendingOpens 发布 Tunnel Proxy 已按 Tunnel 聚合到当前 Connector 的绝对等待数。
// 零值表示最后一个等待者已离开；Demand generation 与 Lease 合并仍由 WorkPool 负责。
func (manager *Manager) SetPendingOpens(session serverruntime.Session, pending uint32) error {
	if manager == nil || !validSession(session) {
		return ErrSessionUnavailable
	}
	manager.mu.Lock()
	managed := manager.bySession[session.SessionID]
	if managed == nil || managed.session != session || !managed.isConfigReady() {
		manager.mu.Unlock()
		return ErrSessionUnavailable
	}
	manager.mu.Unlock()
	managed.setPendingOpens(pending)
	if _, err := manager.reconcileDemand(managed, managed.protocol); err != nil {
		managed.cancel()
		return fmt.Errorf("reconcile WorkDemand for Pending OPEN: %w", err)
	}
	return nil
}

// install 在 Manager.mu 下执行 shutdown/revoke/generation 三重 fence，并同时更新
// Connector 与 Session 两个查找索引；旧代只撤出索引，真正关闭留给锁外 cleanup。
func (manager *Manager) install(managed *managedSession) (*managedSession, error) {
	key := connectorKey{tunnelID: managed.session.TunnelID, connectorID: managed.session.ConnectorID}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.shutdownStarted {
		return nil, ErrSessionUnavailable
	}
	if _, revoked := manager.revokedTunnels[managed.session.TunnelID]; revoked {
		return nil, serverruntime.ErrTunnelRuntimeRevoked
	}
	previous := manager.byConnector[key]
	if previous != nil && previous.session.Generation >= managed.session.Generation {
		return nil, ErrSessionSuperseded
	}
	if previous != nil {
		delete(manager.bySession, previous.session.SessionID)
	}
	manager.byConnector[key] = managed
	manager.bySession[managed.session.SessionID] = managed
	manager.liveSessions[managed] = struct{}{}
	return previous, nil
}

// beginStartup 在 Owner 启动前登记短生命周期预留。Revoke/Shutdown 先设置 fence，
// 再在锁外等待预留完成，因此不会越过一个已经获准启动、但尚未发布到 liveSessions
// 的 Control Owner。预留期间不持有 Manager 锁执行 Start、Wait、Close 或网络 IO。
func (manager *Manager) beginStartup(tunnelID string) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.shutdownStarted {
		return ErrSessionUnavailable
	}
	if _, revoked := manager.revokedTunnels[tunnelID]; revoked {
		return serverruntime.ErrTunnelRuntimeRevoked
	}
	group := manager.startingByTunnel[tunnelID]
	if group == nil {
		group = &startupGroup{done: make(chan struct{})}
		manager.startingByTunnel[tunnelID] = group
	}
	group.count++
	return nil
}

// endStartup 释放一次 startup 预留，并在计数归零时唤醒所有等待收敛者。
func (manager *Manager) endStartup(tunnelID string) {
	manager.mu.Lock()
	group := manager.startingByTunnel[tunnelID]
	if group == nil || group.count <= 0 {
		manager.mu.Unlock()
		panic("server session startup ownership invariant violated")
	}
	group.count--
	if group.count == 0 {
		delete(manager.startingByTunnel, tunnelID)
		close(group.done)
	}
	manager.mu.Unlock()
}

// startupDoneLocked 返回当前 Tunnel 的启动栅栏；调用方必须持有 Manager.mu，
// 并在释放锁后等待返回的 channel。
func (manager *Manager) startupDoneLocked(tunnelID string) <-chan struct{} {
	group := manager.startingByTunnel[tunnelID]
	if group == nil {
		return nil
	}
	return group.done
}

// reconcileDemand 串行计算一个 Session 的绝对非 ACTIVE 目标、Lease 预算与 generation，
// 先在 WorkPool/Authenticator 完成授权交接，再向 Control Owner 下发同一结果。
// 任一步失败都向上返回并关闭 Session，不能留下双重 Lease 或静默丢失 Demand。
func (manager *Manager) reconcileDemand(managed *managedSession, protocolVersion uint32) (bool, error) {
	if managed == nil || !managed.isConfigReady() {
		return false, nil
	}
	managed.reconcileMu.Lock()
	defer managed.reconcileMu.Unlock()
	counts := managed.pool.Snapshot()
	if counts.Closed || counts.Draining {
		return false, nil
	}
	desired := managed.desiredNonActive()
	now := time.Since(manager.startedAt)
	decreasing, deferChange := managed.demandChange(desired, now)
	if deferChange {
		return false, nil
	}
	if managed.shouldRolloverDemand(counts, manager.options.MaxWorkTotal, desired) {
		// 当前 Lease 的槽位已全部转入 Pool，但随后有 Work 被消费或关闭。先在
		// Pool 内结束旧的绝对目标，再发布更高 generation 的缺口 Demand；旧
		// Lease 已耗尽，因此这里没有未消费预算需要归还，也不会形成双重授权。
		cancelHandoff, _, err := managed.pool.DecideDemand(serverworkpool.DemandRequest{})
		if err != nil {
			return false, fmt.Errorf("roll over exhausted WorkDemand: %w", err)
		}
		if cancelHandoff.ReplacedLeaseID != "" {
			if err := managed.authenticator.RevokeLease(cancelHandoff.ReplacedLeaseID); err != nil {
				return false, fmt.Errorf("revoke exhausted WorkDemand lease: %w", err)
			}
		}
	}
	leaseID, err := identity.NewLeaseID()
	if err != nil {
		return false, fmt.Errorf("generate WorkDemand lease: %w", err)
	}
	budgetSlots := min(desired, manager.options.MaxWorkConnecting)
	if decreasing {
		budgetSlots = 0
	}
	handoff, emitted, err := managed.pool.DecideDemand(serverworkpool.DemandRequest{
		DesiredNonActive: desired,
		BudgetSlots:      budgetSlots,
		LeaseID:          leaseID,
		LeaseTTL:         manager.demandLeaseTTL,
	})
	if err != nil {
		return false, fmt.Errorf("decide WorkDemand: %w", err)
	}
	if !emitted {
		return false, nil
	}
	if handoff.ReplacedLeaseID != "" {
		if err := managed.authenticator.RevokeLease(handoff.ReplacedLeaseID); err != nil {
			return false, fmt.Errorf("revoke replaced WorkDemand lease: %w", err)
		}
	}
	if handoff.Grant != nil {
		if err := managed.authenticator.GrantLease(
			handoff.Grant.LeaseID, handoff.Grant.Slots, handoff.Grant.TTL,
		); err != nil {
			return false, fmt.Errorf("grant WorkDemand lease: %w", err)
		}
		managed.installDemand(handoff.Grant.Slots, handoff.Demand.DesiredNonActive, now+handoff.Grant.TTL)
	} else {
		managed.installDemand(0, handoff.Demand.DesiredNonActive, 0)
	}
	demand := handoff.Demand
	envelope := &protocolv1.ControlEnvelope{
		ProtocolVersion: protocolVersion,
		Payload: &protocolv1.ControlEnvelope_WorkDemand{WorkDemand: &protocolv1.WorkDemand{
			BudgetLeaseId: demand.BudgetLeaseID, DesiredNonActive: demand.DesiredNonActive,
			MaxNewConnections: demand.MaxNewConnections, LeaseTtlMs: demand.LeaseTTLMillis,
			DemandGeneration: demand.Generation,
		}},
	}
	if err := managed.owner.Enqueue(envelope); err != nil {
		return false, fmt.Errorf("enqueue WorkDemand: %w", err)
	}
	return true, nil
}

// acceptConfigAck 只完成 Ack 与唯一 outstanding Snapshot 的业务关联。APPLIED 的
// ONLINE 发布由调用方先提交 Registry，再通过 markConfigReady 开放数据面入口。
func (managed *managedSession) acceptConfigAck(
	ack *protocolv1.ConfigAck,
) (applied bool, becameReady bool, next *snapshotCandidate, err error) {
	managed.configMu.Lock()
	defer managed.configMu.Unlock()
	if ack == nil || managed.outstanding == nil {
		return false, false, nil, errors.New("ConfigAck has no outstanding TunnelSnapshot")
	}
	target := managed.outstanding
	switch ack.GetApplyStatus() {
	case protocolv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_APPLIED:
		if ack.GetErrorCode() != protocolv1.ErrorCode_ERROR_CODE_OK ||
			ack.GetObservedRevision() != target.revision {
			return false, false, nil, errors.New("ConfigAck APPLIED does not match outstanding TunnelSnapshot")
		}
		managed.hasObserved = true
		managed.observedRevision = target.revision
		managed.observedDigest = target.digest
		managed.outstanding = nil
		becameReady = !managed.configReady
		if managed.hasRejected && managed.rejectedRevision <= managed.observedRevision {
			managed.hasRejected = false
		}
		applied = true
	case protocolv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_REJECTED:
		if ack.GetErrorCode() == protocolv1.ErrorCode_ERROR_CODE_OK ||
			ack.GetObservedRevision() != managed.observedRevision {
			return false, false, nil, errors.New("ConfigAck REJECTED does not preserve observed revision")
		}
		managed.outstanding = nil
		managed.hasRejected = true
		managed.rejectedRevision = target.revision
	default:
		return false, false, nil, errors.New("ConfigAck apply status is invalid")
	}
	if managed.pending != nil {
		minimum := managed.observedRevision
		if managed.hasRejected && managed.rejectedRevision > minimum {
			minimum = managed.rejectedRevision
		}
		if managed.pending.revision > minimum {
			next = managed.pending
			managed.outstanding = next
		}
		managed.pending = nil
	}
	return applied, becameReady, next, nil
}

// markConfigReady 在首个 APPLIED Snapshot 已发布到 Registry 后开放数据面门禁。
func (managed *managedSession) markConfigReady() {
	managed.configMu.Lock()
	managed.configReady = true
	managed.configMu.Unlock()
}

// hasOutstandingSnapshot 返回是否仍有等待 Ack 的唯一 Snapshot。
func (managed *managedSession) hasOutstandingSnapshot() bool {
	managed.configMu.Lock()
	outstanding := managed.outstanding != nil
	managed.configMu.Unlock()
	return outstanding
}

// isConfigReady 在线程安全的前提下读取首份配置是否已成功应用。
func (managed *managedSession) isConfigReady() bool {
	if managed == nil {
		return false
	}
	managed.configMu.Lock()
	ready := managed.configReady
	managed.configMu.Unlock()
	return ready
}

// setPendingOpens 记录数据面当前绝对等待数，后续 Demand 不累加历史增量。
func (managed *managedSession) setPendingOpens(pending uint32) {
	managed.demandMu.Lock()
	defer managed.demandMu.Unlock()
	managed.pendingOpens = pending
}

// desiredNonActive 取冻结的基础空闲目标与真实 Pending OPEN 的较大值。
func (managed *managedSession) desiredNonActive() uint32 {
	managed.demandMu.Lock()
	defer managed.demandMu.Unlock()
	return max(initialDesiredNonActive, managed.pendingOpens)
}

// installDemand 保存已成功授权并准备下发的 Demand 镜像，供后续抑制重复扩容。
func (managed *managedSession) installDemand(slots, desired uint32, deadline time.Duration) {
	managed.demandMu.Lock()
	defer managed.demandMu.Unlock()
	managed.demandDesired = desired
	managed.demandSlotsRemaining = slots
	managed.demandExhausted = slots == 0
	managed.demandDeadline = deadline
}

// consumeDemandSlot 在一个 READY WorkConn 消耗 Lease 后递减本地剩余槽位。
func (managed *managedSession) consumeDemandSlot() {
	managed.demandMu.Lock()
	defer managed.demandMu.Unlock()
	if managed.demandSlotsRemaining == 0 {
		return
	}
	managed.demandSlotsRemaining--
	managed.demandExhausted = managed.demandSlotsRemaining == 0
}

// demandChange 判断目标是否下降，或上升是否应等当前未过期 Lease 用尽后再发布。
func (managed *managedSession) demandChange(desired uint32, now time.Duration) (decreasing, deferChange bool) {
	managed.demandMu.Lock()
	defer managed.demandMu.Unlock()
	return desired < managed.demandDesired,
		desired > managed.demandDesired && managed.demandSlotsRemaining > 0 && now < managed.demandDeadline
}

// shouldRolloverDemand 判断已耗尽 Lease 对应的非 ACTIVE 存量是否再次低于目标，
// 此时需要结束旧 generation 并签发新的缺口 Lease。
func (managed *managedSession) shouldRolloverDemand(
	counts serverworkpool.Counts,
	maxWorkTotal, desired uint32,
) bool {
	managed.demandMu.Lock()
	defer managed.demandMu.Unlock()
	if !managed.demandExhausted || desired != managed.demandDesired {
		return false
	}
	if counts.Active >= maxWorkTotal {
		desired = 0
	} else {
		desired = min(desired, maxWorkTotal-counts.Active)
	}
	nonActive := counts.Connecting + counts.Idle + counts.Opening
	return nonActive < desired
}

// setTerminationReason 只记录第一个具体终止原因，避免后续清理覆盖根因。
func (managed *managedSession) setTerminationReason(reason string) {
	if managed == nil || reason == "" {
		return
	}
	managed.terminationMu.Lock()
	if managed.terminationReason == "" {
		managed.terminationReason = reason
	}
	managed.terminationMu.Unlock()
}

// setConvergenceReason 让已经取得 Manager fence 的 Revoke/Shutdown 覆盖 cleanup
// 尚未提交 Disconnected 事件时写入的默认原因。Heartbeat、replacement 等更具体的
// 先行原因仍保留；因此并发终止路径有稳定且可解释的优先级。
func (managed *managedSession) setConvergenceReason(reason string) {
	if managed == nil || reason == "" {
		return
	}
	managed.terminationMu.Lock()
	if managed.terminationReason == "" || managed.terminationReason == "control_session_closed" {
		managed.terminationReason = reason
	}
	managed.terminationMu.Unlock()
}

// termination 返回稳定终止原因；没有更具体原因时使用普通 Control 关闭。
func (managed *managedSession) termination() string {
	if managed == nil {
		return "control_session_closed"
	}
	managed.terminationMu.Lock()
	reason := managed.terminationReason
	managed.terminationMu.Unlock()
	if reason == "" {
		return "control_session_closed"
	}
	return reason
}

// resetTimer 先排空可能已经触发的 Timer，再建立新的完整超时窗口。
func resetTimer(timer *time.Timer, timeout time.Duration) {
	stopTimer(timer)
	timer.Reset(timeout)
}

// stopTimer 停止并非阻塞地排空 Timer，避免旧 tick 被下一次 Reset 误消费。
func stopTimer(timer *time.Timer) {
	if timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

// cleanupMode 区分普通 Session 退出保留 ACTIVE 与强制收敛全部 Work 的路径。
type cleanupMode uint8

const (
	cleanupNonActive cleanupMode = iota
	cleanupAll
)

// removeLookup 只撤下 Current/WorkAuth 查找入口。liveSessions 必须继续保留 managed，
// 直到 cleanupManaged 已完成全部外部资源操作并关闭 cleanupDone。
func (manager *Manager) removeLookup(managed *managedSession) {
	key := connectorKey{tunnelID: managed.session.TunnelID, connectorID: managed.session.ConnectorID}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.byConnector[key] == managed {
		delete(manager.byConnector, key)
	}
	if manager.bySession[managed.session.SessionID] == managed {
		delete(manager.bySession, managed.session.SessionID)
	}
}

// finishCleanup 只在全部外部资源已经关闭后移除 liveSessions 所有权记录。
func (manager *Manager) finishCleanup(managed *managedSession) {
	manager.mu.Lock()
	delete(manager.liveSessions, managed)
	manager.mu.Unlock()
}

// cleanupManaged 是 Authenticator、WorkPool 和 Registry cleanup 的唯一 owner。
// sync.Once 让并发 Serve/Revoke/Shutdown 在锁外等待同一次 Close；cleanupDone
// 关闭时 Control Owner 和外部资源已完成收敛，liveSessions 也已删除。
func (manager *Manager) cleanupManaged(managed *managedSession, mode cleanupMode) error {
	if manager.beforeCleanupForTest != nil {
		manager.beforeCleanupForTest(managed.session)
	}
	managed.cleanupOnce.Do(func() {
		// Serve 的正常退出路径已等待 Owner；重复 Wait 只会读取已关闭
		// done。Revoke、Shutdown 和 replacement 则必须在返回前真正等待
		// Owner 的 Control socket 与内部 goroutine 全部退出。
		managed.setTerminationReason("control_session_closed")
		manager.removeLookup(managed)
		managed.cancel()
		ownerErr := cleanupOwnerError(managed.owner.Wait())
		managed.authenticator.Close()
		var poolErr error
		if mode == cleanupAll {
			poolErr = managed.pool.Close()
		} else {
			// 普通 Control 断开或 replacement 只关闭旧代尚未进入 ACTIVE 的
			// WorkConn；ACTIVE 由数据面 owner、Revoke 或 Shutdown 收敛。
			poolErr = managed.pool.CloseNonActive()
		}
		disconnectEvent, disconnected := manager.registry.DisconnectIfCurrent(managed.session, managed.termination())
		if disconnected {
			manager.logLifecycle(disconnectEvent)
		}
		managed.cleanupErr = errors.Join(ownerErr, poolErr)
		manager.finishCleanup(managed)
		close(managed.cleanupDone)
	})
	<-managed.cleanupDone
	return managed.cleanupErr
}

// cleanupOwnerError 只消除 cleanup 主动 cancel 产生的预期终止原因。
// Owner 在同一 errors.Join 中返回的 Deadline/Close 错误仍必须传播。
func cleanupOwnerError(err error) error {
	if err == nil || err == context.Canceled {
		return nil
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return err
	}
	remaining := make([]error, 0, len(joined.Unwrap()))
	for _, nested := range joined.Unwrap() {
		if nested == context.Canceled {
			continue
		}
		remaining = append(remaining, nested)
	}
	return errors.Join(remaining...)
}

// markDraining 立即撤下 WorkAuth/Pool 查找入口；Registry 仍保留 Current 身份供
// generation fencing，但 Tunnel eligibility predicate 会因 Pool 不可见而排除它。
func (manager *Manager) markDraining(managed *managedSession) {
	key := connectorKey{tunnelID: managed.session.TunnelID, connectorID: managed.session.ConnectorID}
	manager.mu.Lock()
	if manager.byConnector[key] != managed {
		manager.mu.Unlock()
		return
	}
	if manager.bySession[managed.session.SessionID] == managed {
		delete(manager.bySession, managed.session.SessionID)
	}
	manager.mu.Unlock()
	if event, changed := manager.registry.ObserveDraining(managed.session); changed {
		manager.logLifecycle(event)
	}
}

// logLifecycle 在锁外记录不含 Credential 的值型事件；空字段不会伪造日志属性。
func (manager *Manager) logLifecycle(event serverruntime.ConnectorLifecycleEvent) {
	if manager == nil || manager.logger == nil || event.Name == "" {
		return
	}
	snapshot := event.Snapshot
	attributes := make([]any, 0, 20)
	if snapshot.TunnelID != "" {
		attributes = append(attributes, slog.String("tunnel_id", snapshot.TunnelID))
	}
	if snapshot.ConnectorID != "" {
		attributes = append(attributes, slog.String("connector_id", snapshot.ConnectorID))
	}
	if snapshot.SessionID != "" {
		attributes = append(attributes, slog.String("session_id", snapshot.SessionID))
	}
	if snapshot.Generation != 0 {
		attributes = append(attributes, slog.Uint64("generation", snapshot.Generation))
	}
	if snapshot.Status != "" {
		attributes = append(attributes, slog.String("connector_status", string(snapshot.Status)))
	}
	if snapshot.Hostname != "" {
		attributes = append(attributes, slog.String("hostname", snapshot.Hostname))
	}
	if snapshot.OS != "" {
		attributes = append(attributes, slog.String("os", snapshot.OS))
	}
	if snapshot.Arch != "" {
		attributes = append(attributes, slog.String("arch", snapshot.Arch))
	}
	if snapshot.Version != "" {
		attributes = append(attributes, slog.String("version", snapshot.Version))
	}
	if event.Reason != "" {
		attributes = append(attributes, slog.String("reason", event.Reason))
	}
	manager.logger.Info(event.Name, attributes...)
}

// validSession 校验所有 generation fence 都依赖的完整 Session identity。
func validSession(session serverruntime.Session) bool {
	return identity.ValidateTunnelID(session.TunnelID) == nil &&
		identity.ValidateConnectorID(session.ConnectorID) == nil &&
		identity.ValidateSessionID(session.SessionID) == nil && session.Generation > 0
}

// nilSnapshotProvider 同时识别 nil 接口与装入接口的 typed nil 实现。
func nilSnapshotProvider(provider SnapshotProvider) bool {
	if provider == nil {
		return true
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
