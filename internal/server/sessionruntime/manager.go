// Package sessionruntime 负责把一条已认证 Control Session 装配为 Server 进程内的
// 单一运行时所有权，并向 WorkConn 认证路径提供 generation-fenced 查找。
package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/lifei6671/xtunnel/internal/controlsession"
	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	servercontrolauth "github.com/lifei6671/xtunnel/internal/server/controlauth"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
	"github.com/lifei6671/xtunnel/internal/server/workauth"
	serverworkpool "github.com/lifei6671/xtunnel/internal/server/workpool"
)

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
)

const (
	// V0.1 的默认 Pool 目标由技术方案冻结为 target_idle=8。初始 Session 没有
	// Opening，因此 desired_non_active 直接使用该绝对目标并由 Pool 容量再钳制。
	initialDesiredNonActive = uint32(8)
	initialLeaseTTL         = 10 * time.Second
	defaultHeartbeatTimeout = 30 * time.Second
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
	// HeartbeatTimeout 使用 Server 本地单调时间关闭失联 Session；零值使用冻结的 30 秒默认值。
	HeartbeatTimeout time.Duration
}

type connectorKey struct {
	tunnelID    string
	connectorID string
}

type managedSession struct {
	session       serverruntime.Session
	authenticator *workauth.SessionAuthenticator
	owner         *controlsession.Owner
	pool          *serverworkpool.Pool
	cancel        context.CancelFunc
	protocol      uint32

	reconcileMu          sync.Mutex
	demandMu             sync.Mutex
	pendingOpens         uint32
	demandDesired        uint32
	demandSlotsRemaining uint32
	demandExhausted      bool
	demandDeadline       time.Duration
}

// Manager 是 Control Owner、Work Authenticator 与 Runtime Registry 之间的唯一桥梁。
//
// byConnector 只发布每个 Connector 的最高 generation；bySession 只包含仍可接受新
// WorkHello 的当前代。替换 Session 时先在锁内撤下旧代，再在锁外 Cancel，避免旧代
// 的异步清理删除新代或让新 WorkConn 继续命中旧 Secret。
type Manager struct {
	mu sync.Mutex

	registry        *serverruntime.Registry
	options         Options
	startedAt       time.Time
	demandLeaseTTL  time.Duration
	byConnector     map[connectorKey]*managedSession
	bySession       map[string]*managedSession
	shutdownStarted bool
	shutdownDone    chan struct{}
	shutdownErr     error
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
		options.HeartbeatTimeout <= 0 {
		return nil, ErrInvalidOptions
	}
	return &Manager{
		registry: registry, options: options, startedAt: time.Now(), demandLeaseTTL: initialLeaseTTL,
		byConnector:  make(map[connectorKey]*managedSession),
		bySession:    make(map[string]*managedSession),
		shutdownDone: make(chan struct{}),
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

	authenticator, err := workauth.New(workauth.Session{
		TunnelID: established.Session.TunnelID, ConnectorID: established.Session.ConnectorID,
		SessionID: established.Session.SessionID, Generation: established.Session.Generation,
		Secret: established.SessionSecret[:],
	}, manager.options.MaxReplayEntries, func() time.Duration {
		return time.Since(manager.startedAt)
	})
	// Session Secret 只允许在 Control AUTH 返回值与 Authenticator 内存中短暂停留；
	// 无论构造成功与否，都立即清掉调用方数组，避免生命周期上层意外长期持有。
	clear(established.SessionSecret[:])
	if err != nil {
		manager.registry.ClearIfCurrent(established.Session)
		return errors.Join(fmt.Errorf("construct server Work authenticator: %w", err), connection.Close())
	}
	pool, err := serverworkpool.New(serverworkpool.Options{
		Session: serverworkpool.Session{
			TunnelID: established.Session.TunnelID, ConnectorID: established.Session.ConnectorID,
			SessionID: established.Session.SessionID, Generation: established.Session.Generation,
		},
		MaxTotal: manager.options.MaxWorkTotal, MaxConnecting: manager.options.MaxWorkConnecting,
		LimitManager: manager.options.LimitManager,
		Clock:        func() time.Duration { return time.Since(manager.startedAt) }, DeadlineNow: time.Now,
	})
	if err != nil {
		authenticator.Close()
		manager.registry.ClearIfCurrent(established.Session)
		return errors.Join(fmt.Errorf("construct server WorkPool: %w", err), connection.Close())
	}

	owner, err := controlsession.NewOwner(connection, established.Control, controlsession.Options{
		ProtocolVersion: established.ProtocolVersion, HighPriorityCapacity: manager.options.HighPriorityCapacity,
		NormalCapacity: manager.options.NormalCapacity, InboundCapacity: manager.options.InboundCapacity,
		WriteTimeout: manager.options.WriteTimeout, MaxFrameBytes: manager.options.MaxControlFrameBytes,
	})
	if err != nil {
		authenticator.Close()
		_ = pool.Close()
		manager.registry.ClearIfCurrent(established.Session)
		return errors.Join(fmt.Errorf("construct server Control Session owner: %w", err), connection.Close())
	}

	sessionContext, cancel := context.WithCancel(ctx)
	managed := &managedSession{
		session: established.Session, authenticator: authenticator, owner: owner, pool: pool, cancel: cancel,
		protocol: established.ProtocolVersion,
	}
	if err := owner.Start(sessionContext); err != nil {
		cancel()
		authenticator.Close()
		manager.registry.ClearIfCurrent(established.Session)
		return errors.Join(fmt.Errorf("start server Control Session owner: %w", err), connection.Close())
	}

	previous, installErr := manager.install(managed)
	if installErr != nil {
		cancel()
		ownerErr := owner.Wait()
		authenticator.Close()
		_ = pool.Close()
		manager.registry.ClearIfCurrent(established.Session)
		return errors.Join(installErr, ownerErr)
	}
	if previous != nil {
		// Cancel 只负责解除旧 Owner 的网络 IO；旧 Serve 会自行关闭 Authenticator，
		// 且其 cleanup 会用指针和 generation 双重条件避免删除当前新代。
		previous.cancel()
		_ = previous.pool.CloseNonActive()
	}
	if err := manager.enqueueInitialDemand(managed, established.ProtocolVersion); err != nil {
		cancel()
		ownerErr := owner.Wait()
		manager.remove(managed)
		authenticator.Close()
		poolErr := pool.Close()
		manager.registry.ClearIfCurrent(established.Session)
		return errors.Join(err, ownerErr, poolErr)
	}

	// Manager 持续消费已通过状态机校验的入站消息。DrainRequest 必须在同一
	// Session Owner 内完成摘流、等待 OPENING 与 Ack，不能像普通 Heartbeat 一样丢弃。
	inboundDrained := make(chan struct{})
	inboundErrors := make(chan error, 1)
	go func() {
		defer close(inboundDrained)
		heartbeatTimer := time.NewTimer(manager.options.HeartbeatTimeout)
		defer heartbeatTimer.Stop()
		var drainAck *protocolv1.ControlEnvelope
		for {
			var inbound controlsession.Inbound
			var ok bool
			select {
			case <-heartbeatTimer.C:
				inboundErrors <- ErrHeartbeatTimeout
				cancel()
				return
			case inbound, ok = <-owner.Inbound():
				if !ok {
					return
				}
			}
			if inbound.Envelope.GetHeartbeat() != nil {
				resetTimer(heartbeatTimer, manager.options.HeartbeatTimeout)
				if _, err := manager.reconcileDemand(managed, established.ProtocolVersion); err != nil {
					inboundErrors <- fmt.Errorf("reconcile WorkDemand after Heartbeat: %w", err)
					cancel()
					return
				}
				continue
			}
			request := inbound.Envelope.GetDrainRequest()
			if request == nil {
				continue
			}
			if inbound.Duplicate && drainAck != nil {
				if err := owner.Enqueue(drainAck); err != nil {
					inboundErrors <- fmt.Errorf("enqueue duplicate DrainAck: %w", err)
					cancel()
					return
				}
				continue
			}
			manager.markDraining(managed)
			if err := pool.BeginDrain(); err != nil && !errors.Is(err, serverworkpool.ErrPoolDraining) {
				inboundErrors <- fmt.Errorf("begin server WorkPool drain: %w", err)
				cancel()
				return
			}
			stopTimer(heartbeatTimer)
			drainContext, cancelDrain := context.WithTimeout(sessionContext, time.Duration(request.GetDrainTimeoutMs())*time.Millisecond)
			remainingActive, err := pool.WaitOpeningAndCloseNonActive(drainContext)
			cancelDrain()
			if err != nil {
				inboundErrors <- fmt.Errorf("close non-active WorkConn during drain: %w", err)
				cancel()
				return
			}
			drainAck = &protocolv1.ControlEnvelope{
				ProtocolVersion: established.ProtocolVersion,
				Payload: &protocolv1.ControlEnvelope_DrainAck{DrainAck: &protocolv1.DrainAck{
					DrainId: request.GetDrainId(), RemainingActive: remainingActive,
				}},
			}
			if err := owner.Enqueue(drainAck); err != nil {
				inboundErrors <- fmt.Errorf("enqueue DrainAck: %w", err)
				cancel()
				return
			}
			heartbeatTimer.Reset(manager.options.HeartbeatTimeout)
		}
	}()

	ownerErr := owner.Wait()
	// Owner 已经终止时不再可能写出 DrainAck；先取消 Session Context，立即打断
	// 可能正按对端 drain_timeout_ms 等待 OPENING 的入站消费者，再等待其退出。
	cancel()
	<-inboundDrained
	var inboundErr error
	select {
	case inboundErr = <-inboundErrors:
	default:
	}
	manager.remove(managed)
	authenticator.Close()
	var poolErr error
	if ctx.Err() != nil {
		poolErr = pool.Close()
	} else {
		// 普通 Control 断开或重连只关闭旧代尚未进入 ACTIVE 的 WorkConn；
		// ACTIVE 由其数据面所有者自然结束，不能因 Session Replacement 被截断。
		poolErr = pool.CloseNonActive()
	}
	manager.registry.ClearIfCurrent(established.Session)
	return errors.Join(ownerErr, inboundErr, poolErr)
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
	managedSessions := make([]*managedSession, 0, len(manager.byConnector))
	for _, managed := range manager.byConnector {
		managedSessions = append(managedSessions, managed)
	}
	clear(manager.bySession)
	manager.mu.Unlock()

	var shutdownErr error
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
		// 归还 Pool Work。这里只结束 Control Session，避免对同一 ACTIVE socket
		// 再执行一套 Close；Gateway.Close 会等待对应 Handler 全部退出。
		managed.cancel()
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
	if !exists {
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
		managed.session.ConnectorID != idle.ConnectorID {
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
	if managed == nil || managed.session != session {
		return nil, false
	}
	return managed.pool, true
}

// SetPendingOpens 发布 Tunnel Proxy 已按 Tunnel 聚合到当前 Connector 的绝对等待数。
// 零值表示最后一个等待者已离开；Demand generation 与 Lease 合并仍由 WorkPool 负责。
func (manager *Manager) SetPendingOpens(session serverruntime.Session, pending uint32) error {
	if manager == nil || !validSession(session) {
		return ErrSessionUnavailable
	}
	manager.mu.Lock()
	managed := manager.bySession[session.SessionID]
	if managed == nil || managed.session != session {
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

func (manager *Manager) install(managed *managedSession) (*managedSession, error) {
	key := connectorKey{tunnelID: managed.session.TunnelID, connectorID: managed.session.ConnectorID}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.shutdownStarted {
		return nil, ErrSessionUnavailable
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
	return previous, nil
}

func (manager *Manager) enqueueInitialDemand(managed *managedSession, protocolVersion uint32) error {
	emitted, err := manager.reconcileDemand(managed, protocolVersion)
	if err != nil {
		return err
	}
	if !emitted {
		return errors.New("initial WorkDemand did not reserve a Budget Lease")
	}
	return nil
}

func (manager *Manager) reconcileDemand(managed *managedSession, protocolVersion uint32) (bool, error) {
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

func (managed *managedSession) setPendingOpens(pending uint32) {
	managed.demandMu.Lock()
	defer managed.demandMu.Unlock()
	managed.pendingOpens = pending
}

func (managed *managedSession) desiredNonActive() uint32 {
	managed.demandMu.Lock()
	defer managed.demandMu.Unlock()
	return max(initialDesiredNonActive, managed.pendingOpens)
}

func (managed *managedSession) installDemand(slots, desired uint32, deadline time.Duration) {
	managed.demandMu.Lock()
	defer managed.demandMu.Unlock()
	managed.demandDesired = desired
	managed.demandSlotsRemaining = slots
	managed.demandExhausted = slots == 0
	managed.demandDeadline = deadline
}

func (managed *managedSession) consumeDemandSlot() {
	managed.demandMu.Lock()
	defer managed.demandMu.Unlock()
	if managed.demandSlotsRemaining == 0 {
		return
	}
	managed.demandSlotsRemaining--
	managed.demandExhausted = managed.demandSlotsRemaining == 0
}

func (managed *managedSession) demandChange(desired uint32, now time.Duration) (decreasing, deferChange bool) {
	managed.demandMu.Lock()
	defer managed.demandMu.Unlock()
	return desired < managed.demandDesired,
		desired > managed.demandDesired && managed.demandSlotsRemaining > 0 && now < managed.demandDeadline
}

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

func resetTimer(timer *time.Timer, timeout time.Duration) {
	stopTimer(timer)
	timer.Reset(timeout)
}

func stopTimer(timer *time.Timer) {
	if timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

func (manager *Manager) remove(managed *managedSession) {
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

// markDraining 立即撤下 WorkAuth/Pool 查找入口；Registry 仍保留 Current 身份供
// generation fencing，但 Tunnel eligibility predicate 会因 Pool 不可见而排除它。
func (manager *Manager) markDraining(managed *managedSession) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.bySession[managed.session.SessionID] == managed {
		delete(manager.bySession, managed.session.SessionID)
	}
}

func validSession(session serverruntime.Session) bool {
	return identity.ValidateTunnelID(session.TunnelID) == nil &&
		identity.ValidateConnectorID(session.ConnectorID) == nil &&
		identity.ValidateSessionID(session.SessionID) == nil && session.Generation > 0
}
