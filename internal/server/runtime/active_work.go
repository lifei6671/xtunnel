package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
)

var (
	// ErrInvalidActiveWork 表示 ACTIVE WorkConn 的身份、连接或生命周期句柄不完整。
	ErrInvalidActiveWork = errors.New("active work is invalid")
	// ErrActiveWorkExists 表示同一 Tunnel 已经注册相同 connection_id。
	ErrActiveWorkExists = errors.New("active work already exists")
	// ErrTunnelRuntimeRevoked 表示 Tunnel 已被线性化撤销，禁止再注册新的 ACTIVE Work。
	ErrTunnelRuntimeRevoked = errors.New("tunnel runtime is revoked")
	// ErrServerRuntimeDraining 表示 Server 已停止新的 Tunnel OPEN，不再接受 ACTIVE 注册。
	ErrServerRuntimeDraining = errors.New("server runtime is draining")
	// ErrActiveWorkLeaseReleased 表示 ACTIVE 终止前 Connector Lease 已被其他路径释放。
	ErrActiveWorkLeaseReleased = errors.New("active work connector lease was already released")
)

const activeDrainPollInterval = 5 * time.Millisecond

// ActiveWorkSpec 是 OPEN_OK 线性化注册 ACTIVE WorkConn 所需的完整所有权。
//
// Lease 必须来自同一 Session 的 Connector 选择；注册成功后由 ActiveWork 接管它。
// 注册失败时所有句柄仍归调用方，调用方必须自行 Release、Cancel 和关闭连接。
type ActiveWorkSpec struct {
	// Session 固定 ACTIVE Work 所属 Tunnel、Connector、Session 与 generation。
	Session Session
	// WorkID 是承载本次业务连接的 WorkConn 标识。
	WorkID string
	// ConnectionID 是 Tunnel 级 ActiveWork Registry 的唯一键。
	ConnectionID string
	// Cancel 解除代理 goroutine 的上下文阻塞。
	Cancel context.CancelFunc
	// WorkConn 是连接 Connector 的数据面连接。
	WorkConn net.Conn
	// PeerConn 是 Public Listener 已接受的对端连接。
	PeerConn net.Conn
	// Lease 保存选择阶段增加的活跃计数，终止路径只释放一次。
	Lease *ConnectorLease
}

// ActiveWorkIdentity 是 ACTIVE 生命周期内不可变的归属快照。
type ActiveWorkIdentity struct {
	ConnectionID string
	TunnelID     string
	ConnectorID  string
	SessionID    string
	Generation   uint64
	WorkID       string
}

// ActiveWork 是独立于 Current Control Session 保存的 ACTIVE 业务连接。
//
// Session fencing 或清理不会关闭它；只有自然 Finish、Tunnel Revoke 或后续 Drain
// Timeout 可以终止。所有终止入口共享 closeOnce，保证 Cancel、连接关闭和 Lease
// 释放 exactly-once。
type ActiveWork struct {
	runtime  *TunnelRuntime
	identity ActiveWorkIdentity
	cancel   context.CancelFunc
	workConn net.Conn
	peerConn net.Conn
	lease    *ConnectorLease

	closeOnce sync.Once
	closeErr  error
}

// Identity 返回不可变 ACTIVE Work 归属。
func (work *ActiveWork) Identity() ActiveWorkIdentity {
	if work == nil {
		return ActiveWorkIdentity{}
	}
	return work.identity
}

// Finish 完成自然结束路径：先从 Tunnel Registry 线性化摘除，再在锁外解除 IO。
func (work *ActiveWork) Finish() error {
	if work == nil || work.runtime == nil {
		return ErrInvalidActiveWork
	}
	work.runtime.detach(work)
	return work.closeOutsideLock()
}

// Close 与 Finish 使用同一个 exactly-once 终止路径，使 ActiveWork 可由统一资源清理器关闭。
func (work *ActiveWork) Close() error {
	return work.Finish()
}

func (work *ActiveWork) closeOutsideLock() error {
	work.closeOnce.Do(func() {
		// 固定顺序是 Cancel → 所有连接 SetDeadline(now) → 所有连接 Close。
		// 这些操作都可能触发调度或系统调用，绝不能在 TunnelRuntime.mu 内执行。
		work.cancel()
		now := work.runtime.now()
		workDeadlineErr := work.workConn.SetDeadline(now)
		peerDeadlineErr := work.peerConn.SetDeadline(now)
		workCloseErr := work.workConn.Close()
		peerCloseErr := work.peerConn.Close()
		var leaseErr error
		if !work.lease.releaseFromActive() {
			leaseErr = ErrActiveWorkLeaseReleased
		}
		work.closeErr = errors.Join(
			wrapCloseError("set WorkConn deadline", workDeadlineErr),
			wrapCloseError("set PeerConn deadline", peerDeadlineErr),
			wrapCloseError("close WorkConn", workCloseErr),
			wrapCloseError("close PeerConn", peerCloseErr),
			leaseErr,
		)
	})
	return work.closeErr
}

// TunnelRuntime 是单个 Tunnel 的线性化所有权边界。
// 不同实例使用不同 mutex；禁止在持有本锁时获取其他 Tunnel 的锁。
type TunnelRuntime struct {
	mu sync.Mutex

	TunnelID string
	registry *Registry

	current                map[string]Session
	generations            map[string]uint64
	pending                map[string]string
	pendingConnectorLimits map[string]*serverlimits.ConnectorLease
	currentConnectorLimits map[string]*serverlimits.ConnectorLease
	connectorActive        map[Session]uint64
	// retired 保存已不再是 Current、但仍被 Lease 或 ActiveWork 引用的 Session。
	// 对应 Session ID 必须继续占用全局索引，避免极端随机碰撞复用正在运行的身份。
	retired     map[Session]struct{}
	lastPicked  string
	activeWorks map[string]*ActiveWork
	revoked     bool
	now         func() time.Time
}

func newTunnelRuntime(registry *Registry, tunnelID string, now func() time.Time) *TunnelRuntime {
	return &TunnelRuntime{
		TunnelID: tunnelID, registry: registry, current: make(map[string]Session), generations: make(map[string]uint64),
		pending: make(map[string]string), pendingConnectorLimits: make(map[string]*serverlimits.ConnectorLease),
		currentConnectorLimits: make(map[string]*serverlimits.ConnectorLease), connectorActive: make(map[Session]uint64),
		retired: make(map[Session]struct{}), activeWorks: make(map[string]*ActiveWork), now: now,
	}
}

// RegisterActiveWork 在 OPEN_OK 后把连接从调用方所有权转为 Tunnel 级 ACTIVE 所有权。
// Map 写入是线性化点；方法返回后 Session 即使被 fencing，ActiveWork 仍独立存在。
func (runtime *TunnelRuntime) RegisterActiveWork(spec ActiveWorkSpec) (*ActiveWork, error) {
	if runtime == nil || !validActiveWorkSpec(runtime, spec) {
		return nil, ErrInvalidActiveWork
	}
	// 先以 CAS 把 Lease 从调用方所有权转为 ACTIVE 专属所有权。这样公共 Release
	// 与注册只能有一个获胜，不会出现“计数已释放、Work 却注册成功”的悬空状态。
	if !spec.Lease.transferToActive() {
		return nil, ErrInvalidActiveWork
	}
	work := &ActiveWork{
		runtime: runtime,
		identity: ActiveWorkIdentity{
			ConnectionID: spec.ConnectionID, TunnelID: spec.Session.TunnelID,
			ConnectorID: spec.Session.ConnectorID, SessionID: spec.Session.SessionID,
			Generation: spec.Session.Generation, WorkID: spec.WorkID,
		},
		cancel: spec.Cancel, workConn: spec.WorkConn, peerConn: spec.PeerConn, lease: spec.Lease,
	}

	runtime.mu.Lock()
	if runtime.registry.draining.Load() {
		runtime.mu.Unlock()
		if !spec.Lease.rollbackActiveTransfer() {
			return nil, errors.Join(ErrServerRuntimeDraining, ErrActiveWorkLeaseReleased)
		}
		return nil, ErrServerRuntimeDraining
	}
	if runtime.revoked {
		runtime.mu.Unlock()
		if !spec.Lease.rollbackActiveTransfer() {
			return nil, errors.Join(ErrTunnelRuntimeRevoked, ErrActiveWorkLeaseReleased)
		}
		return nil, ErrTunnelRuntimeRevoked
	}
	if _, exists := runtime.activeWorks[spec.ConnectionID]; exists {
		runtime.mu.Unlock()
		if !spec.Lease.rollbackActiveTransfer() {
			return nil, errors.Join(ErrActiveWorkExists, ErrActiveWorkLeaseReleased)
		}
		return nil, ErrActiveWorkExists
	}
	runtime.activeWorks[spec.ConnectionID] = work
	runtime.mu.Unlock()
	return work, nil
}

// DrainActive 永久停止新的 ACTIVE 注册，并等待所有 generation 的现有 ACTIVE
// 自然结束。ctx 到期后会在各 Tunnel 锁外强制关闭残留连接，再返回关闭错误。
func (registry *Registry) DrainActive(ctx context.Context) error {
	if registry == nil || ctx == nil {
		return ErrInvalidActiveWork
	}
	registry.draining.Store(true)
	ticker := time.NewTicker(activeDrainPollInterval)
	defer ticker.Stop()
	for {
		if registry.activeCount() == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return registry.closeActive()
		case <-ticker.C:
		}
	}
}

func (registry *Registry) activeCount() int {
	total := 0
	for _, runtime := range registry.runtimeSnapshot() {
		total += runtime.ActiveCount()
	}
	return total
}

func (registry *Registry) closeActive() error {
	var closeErrors []error
	for _, runtime := range registry.runtimeSnapshot() {
		runtime.mu.Lock()
		works := make([]*ActiveWork, 0, len(runtime.activeWorks))
		for connectionID, work := range runtime.activeWorks {
			works = append(works, work)
			delete(runtime.activeWorks, connectionID)
		}
		runtime.mu.Unlock()
		for _, work := range works {
			if err := work.closeOutsideLock(); err != nil {
				closeErrors = append(closeErrors, fmt.Errorf("close active work %s: %w", work.identity.ConnectionID, err))
			}
		}
	}
	return errors.Join(closeErrors...)
}

func (registry *Registry) runtimeSnapshot() []*TunnelRuntime {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	runtimes := make([]*TunnelRuntime, 0, len(registry.tunnels))
	for _, runtime := range registry.tunnels {
		runtimes = append(runtimes, runtime)
	}
	return runtimes
}

// ActiveCount 返回当前 Tunnel Registry 中尚未线性化终止的 ACTIVE 数。
func (runtime *TunnelRuntime) ActiveCount() int {
	if runtime == nil {
		return 0
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return len(runtime.activeWorks)
}

func (runtime *TunnelRuntime) detach(work *ActiveWork) bool {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	current, exists := runtime.activeWorks[work.identity.ConnectionID]
	if !exists || current != work {
		return false
	}
	delete(runtime.activeWorks, work.identity.ConnectionID)
	return true
}

func (runtime *TunnelRuntime) revoke() ([]*ActiveWork, []*serverlimits.ConnectorLease, []string) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.revoked = true
	works := make([]*ActiveWork, 0, len(runtime.activeWorks))
	for connectionID, work := range runtime.activeWorks {
		works = append(works, work)
		delete(runtime.activeWorks, connectionID)
	}
	connectorLimits := make([]*serverlimits.ConnectorLease, 0, len(runtime.currentConnectorLimits)+len(runtime.pendingConnectorLimits))
	for _, lease := range runtime.currentConnectorLimits {
		connectorLimits = append(connectorLimits, lease)
	}
	for _, lease := range runtime.pendingConnectorLimits {
		connectorLimits = append(connectorLimits, lease)
	}
	retained := runtime.registry.discardAuthenticatedInstallsLocked(runtime)
	connectorLimits = append(connectorLimits, retained.connectorLimits...)
	return works, connectorLimits, retained.sessionIDs
}

// TunnelRuntimeRegistry 是 Registry 的语义别名。Session、Connector Lease 与
// ActiveWork 必须使用同一个实例，不能创建两套互不线性化的 Runtime Registry。
type TunnelRuntimeRegistry = Registry

// NewTunnelRuntimeRegistry 创建空的 Tunnel 运行时所有权表。
func NewTunnelRuntimeRegistry() *TunnelRuntimeRegistry {
	return NewRegistry()
}

// RevokeTunnel 先在目标 Tunnel 锁内撤销并摘除所有 generation 的 ActiveWork，
// 再在锁外逐一执行 Cancel、Deadline、Close 和 Lease Release。
func (registry *Registry) RevokeTunnel(tunnelID string) error {
	runtime, err := registry.Tunnel(tunnelID)
	if err != nil {
		return err
	}
	works, connectorLimits, sessionIDs := runtime.revoke()
	for _, lease := range connectorLimits {
		lease.Release()
	}
	for _, sessionID := range sessionIDs {
		registry.releaseSessionID(sessionID)
	}
	closeErrors := make([]error, 0, len(works))
	for _, work := range works {
		if err := work.closeOutsideLock(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close active work %s: %w", work.identity.ConnectionID, err))
		}
	}
	return errors.Join(closeErrors...)
}

func validActiveWorkSpec(runtime *TunnelRuntime, spec ActiveWorkSpec) bool {
	return identity.ValidTunnelID(spec.Session.TunnelID) && spec.Session.TunnelID == runtime.TunnelID &&
		identity.ValidConnectorID(spec.Session.ConnectorID) && identity.ValidSessionID(spec.Session.SessionID) &&
		spec.Session.Generation > 0 && validate.ValidID(spec.WorkID, "work_") &&
		validate.ValidID(spec.ConnectionID, "conn_") && spec.Cancel != nil && spec.WorkConn != nil &&
		spec.PeerConn != nil && spec.Lease != nil && spec.Lease.runtime == runtime &&
		spec.Lease.lifecycle != nil &&
		spec.Lease.Session() == spec.Session
}

func wrapCloseError(operation string, err error) error {
	if err == nil || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}
