package runtime

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lifei6671/xtunnel/internal/healthbudget"
	"github.com/lifei6671/xtunnel/internal/identity"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
)

var (
	// ErrInvalidTunnelID 表示传入的逻辑 Tunnel ID 不符合固定格式。
	ErrInvalidTunnelID = errors.New("tunnel identifier is invalid")
	// ErrInvalidConnectorID 表示传入的运行时 Connector ID 不符合固定格式。
	ErrInvalidConnectorID = errors.New("connector identifier is invalid")
	// ErrSessionIDCollision 表示 CSPRNG 生成的 Session ID 已被当前 Registry 使用。
	ErrSessionIDCollision = errors.New("session identifier collision")
	// ErrPendingSessionNotFound 表示认证处理器提交了不存在或已取消的预留 Session。
	ErrPendingSessionNotFound = errors.New("pending authenticated session not found")
	// ErrNoAvailableConnector 表示指定 Tunnel 当前没有可参与选择的已认证 Connector。
	ErrNoAvailableConnector = errors.New("no available connector for tunnel")
)

const (
	connectorLeaseAcquired uint32 = iota
	connectorLeaseActiveOwned
	connectorLeaseReleased
)

type authenticatedInstallState uint8

const (
	authenticatedInstallPending authenticatedInstallState = iota
	authenticatedInstallFinalized
	authenticatedInstallFailed
	authenticatedInstallDiscarded
)

// Session 是一个已认证 Control Session 的不可变内存归属。
// Generation 仅对同一 (TunnelID, ConnectorID) 单调递增，用于拒绝旧连接的清理。
type Session struct {
	TunnelID    string
	ConnectorID string
	SessionID   string
	Generation  uint64
}

// PendingSession 是尚未发布到 Current Registry 的认证 Session 预留。
// 字段保持私有，防止其他包伪造一个未由 Registry 生成的预留。
type PendingSession struct {
	tunnelID    string
	connectorID string
	sessionID   string
}

// AuthenticatedSessionInstall 是 Success flush 前已预安装、但仍可回滚的
// Session 替换事务。Finalize 与 Rollback 均幂等，并按完整 Session
// identity 防止旧认证处理器覆盖并发替换。
type AuthenticatedSessionInstall struct {
	registry *Registry
	runtime  *TunnelRuntime
	node     *authenticatedInstallNode
}

type authenticatedInstallNode struct {
	session        Session
	connectorLimit *serverlimits.ConnectorLease
	healthTarget   *healthbudget.ConnectorLease
	previous       *authenticatedInstallNode
	state          authenticatedInstallState
}

type authenticatedInstallKey struct {
	tunnelID    string
	connectorID string
}

type authenticatedInstallReleases struct {
	connectorLimits []*serverlimits.ConnectorLease
	sessionIDs      []string
}

// SessionID 返回准备写入 ConnectorAuthSuccess 的 Session ID。
func (pending PendingSession) SessionID() string {
	return pending.sessionID
}

// Session 返回本次预安装的不可变 Session identity。
func (install *AuthenticatedSessionInstall) Session() Session {
	if install == nil || install.node == nil {
		return Session{}
	}
	return install.node.session
}

// ConnectorLease 是一次 Connector 选择对具体 Session 的活跃连接占用。
// Lease 绑定完整 Session，因此旧 generation 的 Release 不会扣减重连后的新 Session。
type ConnectorLease struct {
	runtime *TunnelRuntime
	session Session
	// lifecycle 使用共享原子三态，让误复制的 Lease 仍遵守同一所有权状态机：
	// acquired(0) → active-owned(1) → released(2)，或 acquired(0) → released(2)。
	lifecycle *atomic.Uint32
}

// Session 返回本次选择锁定的不可变 Tunnel、Connector 与 generation 身份。
func (lease *ConnectorLease) Session() Session {
	if lease == nil {
		return Session{}
	}
	return lease.session
}

// Release 归还本次选择增加的唯一负载计数。
// 重复或并发调用只会有一次返回 true，且计数绝不会下溢。
func (lease *ConnectorLease) Release() bool {
	if lease == nil || lease.runtime == nil || lease.lifecycle == nil ||
		!lease.lifecycle.CompareAndSwap(connectorLeaseAcquired, connectorLeaseReleased) {
		return false
	}
	return lease.runtime.releaseConnector(lease.session)
}

func (lease *ConnectorLease) transferToActive() bool {
	return lease != nil && lease.lifecycle != nil &&
		lease.lifecycle.CompareAndSwap(connectorLeaseAcquired, connectorLeaseActiveOwned)
}

func (lease *ConnectorLease) rollbackActiveTransfer() bool {
	return lease != nil && lease.lifecycle != nil &&
		lease.lifecycle.CompareAndSwap(connectorLeaseActiveOwned, connectorLeaseAcquired)
}

func (lease *ConnectorLease) releaseFromActive() bool {
	if lease == nil || lease.runtime == nil || lease.lifecycle == nil ||
		!lease.lifecycle.CompareAndSwap(connectorLeaseActiveOwned, connectorLeaseReleased) {
		return false
	}
	return lease.runtime.releaseConnector(lease.session)
}

// Registry 是 Server 进程内 Tunnel Runtime 的顶层定位表。
//
// 锁规则固定为：
//   - mu 只定位/创建 TunnelRuntime，临界区内绝不获取 TunnelRuntime.mu；
//   - sessionIDsMu 只维护全局 Session ID 唯一集合；
//   - 每个 TunnelRuntime.mu 统一线性化该 Tunnel 的 Current、generation、pending、
//     Service Eligibility、Connector Lease 负载与 ActiveWork；
//   - Registry.mu、sessionIDsMu 与 TunnelRuntime.mu 永不嵌套；唯一允许的外部嵌套
//     是 TunnelRuntime.mu -> healthbudget.Manager.mu，用于把 Target 引用与 Current
//     发布/删除线性化。Budget Manager 不回调 Runtime，也不执行 IO 或等待。
//     其他跨锁操作拆成安全的单向阶段，允许短暂保守占用 Session ID，但绝不允许
//     同一 ID 被重复签发。
//
// 因此一个 Tunnel 的 Session/ActiveWork 慢路径不会阻塞其他 Tunnel，也不存在反向
// 锁序造成的跨 Tunnel 死锁。
type Registry struct {
	mu       sync.Mutex
	tunnels  map[string]*TunnelRuntime
	draining atomic.Bool

	sessionIDsMu sync.Mutex
	sessionIDs   map[string]struct{}
	// authenticatedInstalls 的每个 Key 只在对应 TunnelRuntime.mu 下更新。
	// sync.Map 只允许不同 Tunnel 在不共享 Runtime Lock 时安全存取各自 Key。
	authenticatedInstalls sync.Map

	newSession func() (string, error)
	now        func() time.Time
	limits     *serverlimits.Manager
	health     *healthbudget.Manager
}

// NewRegistry 创建空的 per-Tunnel Runtime Registry。
func NewRegistry() *Registry {
	return newRegistry(identity.NewSessionID)
}

// NewRegistryWithLimits 创建接入进程级 Connector 硬预算的 Registry。
// Control Auth 的 ReserveAuthenticated 在 Success flush 前调用，因此超限连接不会
// 被发布成 Current Session；同 Connector 的 generation replacement 只计一个身份。
func NewRegistryWithLimits(manager *serverlimits.Manager) *Registry {
	registry := newRegistry(identity.NewSessionID)
	registry.limits = manager
	return registry
}

// NewRegistryWithLimitsAndHealthBudget 创建同时接入 Connector 与 Health Target
// 进程级硬预算的 Registry。Health Target 在 InstallAuthenticated 的 Runtime
// 临界区内获取，使预算成功与 Current 发布共享同一个线性化顺序。
func NewRegistryWithLimitsAndHealthBudget(
	limitManager *serverlimits.Manager,
	healthManager *healthbudget.Manager,
) *Registry {
	registry := newRegistry(identity.NewSessionID)
	registry.limits = limitManager
	registry.health = healthManager
	return registry
}

func newRegistry(newSession func() (string, error)) *Registry {
	return newRegistryWithClock(newSession, time.Now)
}

func newRegistryWithClock(newSession func() (string, error), now func() time.Time) *Registry {
	return &Registry{
		tunnels: make(map[string]*TunnelRuntime), sessionIDs: make(map[string]struct{}),
		newSession: newSession, now: now,
	}
}

// Tunnel 返回或创建指定 Tunnel 的唯一 Runtime；顶层锁不会带入 Tunnel 临界区。
func (registry *Registry) Tunnel(tunnelID string) (*TunnelRuntime, error) {
	if registry == nil || registry.newSession == nil || registry.now == nil || identity.ValidateTunnelID(tunnelID) != nil {
		return nil, ErrInvalidTunnelID
	}
	return registry.tunnel(tunnelID, true), nil
}

func (registry *Registry) tunnel(tunnelID string, create bool) *TunnelRuntime {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	runtime := registry.tunnels[tunnelID]
	if runtime == nil && create {
		runtime = newTunnelRuntime(registry, tunnelID, registry.now)
		registry.tunnels[tunnelID] = runtime
	}
	return runtime
}

// ReserveAuthenticated 为已完成 Token 校验的连接预留一个全局唯一 Session ID，
// 但不修改 Current，也不递增 generation。
func (registry *Registry) ReserveAuthenticated(tunnelID, connectorID string) (PendingSession, error) {
	if err := identity.ValidateTunnelID(tunnelID); err != nil {
		return PendingSession{}, fmt.Errorf("%w: %w", ErrInvalidTunnelID, err)
	}
	if err := identity.ValidateConnectorID(connectorID); err != nil {
		return PendingSession{}, fmt.Errorf("%w: %w", ErrInvalidConnectorID, err)
	}
	if registry == nil || registry.newSession == nil {
		return PendingSession{}, errors.New("session registry is not initialized")
	}
	sessionID, err := registry.newSession()
	if err != nil {
		return PendingSession{}, fmt.Errorf("generate authenticated session identifier: %w", err)
	}
	if err := identity.ValidateSessionID(sessionID); err != nil {
		return PendingSession{}, fmt.Errorf("generate authenticated session identifier: %w", err)
	}
	if !registry.reserveSessionID(sessionID) {
		return PendingSession{}, ErrSessionIDCollision
	}
	var connectorLimit *serverlimits.ConnectorLease
	if registry.limits != nil {
		connectorLimit, err = registry.limits.AcquireConnector(tunnelID, connectorID)
		if err != nil {
			registry.releaseSessionID(sessionID)
			return PendingSession{}, err
		}
	}

	runtime := registry.tunnel(tunnelID, true)
	runtime.mu.Lock()
	if runtime.revoked {
		runtime.mu.Unlock()
		connectorLimit.Release()
		registry.releaseSessionID(sessionID)
		return PendingSession{}, ErrTunnelRuntimeRevoked
	}
	runtime.pending[sessionID] = connectorID
	runtime.pendingConnectorLimits[sessionID] = connectorLimit
	runtime.mu.Unlock()
	return PendingSession{tunnelID: tunnelID, connectorID: connectorID, sessionID: sessionID}, nil
}

// CommitAuthenticated 直接安装并终结一个预留 Session。需要跨 Success
// flush 保留旧 Current 的调用方必须使用 InstallAuthenticated。
func (registry *Registry) CommitAuthenticated(pending PendingSession) (Session, error) {
	install, err := registry.InstallAuthenticated(pending)
	if err != nil {
		return Session{}, err
	}
	install.Finalize()
	return install.Session(), nil
}

// InstallAuthenticated 在 AUTH Success Frame 写出前原子预安装 Session。
// 旧 Current 及其 Connector Lease/Session ID 在 Finalize 前保留，以便写入
// 失败时可以按 generation fencing 恢复。
func (registry *Registry) InstallAuthenticated(pending PendingSession) (*AuthenticatedSessionInstall, error) {
	if registry == nil {
		return nil, ErrPendingSessionNotFound
	}
	runtime := registry.tunnel(pending.tunnelID, false)
	if runtime == nil {
		return nil, ErrPendingSessionNotFound
	}

	runtime.mu.Lock()
	if runtime.revoked {
		runtime.mu.Unlock()
		return nil, ErrTunnelRuntimeRevoked
	}
	connectorID, exists := runtime.pending[pending.sessionID]
	if !exists || connectorID != pending.connectorID {
		runtime.mu.Unlock()
		return nil, ErrPendingSessionNotFound
	}
	// Health Target 的唯一 Owner Key 是 (Tunnel, Connector)，generation 只持有
	// 独立引用。固定锁序是 TunnelRuntime.mu -> healthbudget.Manager.mu；获取失败时
	// pending 尚未消费，Control Auth 可完整取消 Session/Connector 预留。
	var healthTarget *healthbudget.ConnectorLease
	var err error
	if registry.health != nil {
		healthTarget, err = registry.health.AcquireConnector(pending.tunnelID, pending.connectorID)
		if err != nil {
			runtime.mu.Unlock()
			return nil, err
		}
	}
	delete(runtime.pending, pending.sessionID)
	connectorLimit := runtime.pendingConnectorLimits[pending.sessionID]
	delete(runtime.pendingConnectorLimits, pending.sessionID)
	previous, hadPrevious := runtime.current[pending.connectorID]
	generation := runtime.generations[pending.connectorID] + 1
	runtime.generations[pending.connectorID] = generation
	session := Session{
		TunnelID: pending.tunnelID, ConnectorID: pending.connectorID,
		SessionID: pending.sessionID, Generation: generation,
	}
	key := authenticatedInstallKey{tunnelID: pending.tunnelID, connectorID: pending.connectorID}
	var previousNode *authenticatedInstallNode
	if currentInstall, found := registry.authenticatedInstalls.Load(key); found {
		previousNode = currentInstall.(*authenticatedInstallNode)
	} else if hadPrevious {
		previousNode = &authenticatedInstallNode{
			session: previous, connectorLimit: runtime.currentConnectorLimits[pending.connectorID],
			healthTarget: runtime.currentHealthTargets[pending.connectorID],
			state:        authenticatedInstallFinalized,
		}
	}
	node := &authenticatedInstallNode{
		session: session, connectorLimit: connectorLimit, healthTarget: healthTarget, previous: previousNode,
		state: authenticatedInstallPending,
	}
	runtime.current[pending.connectorID] = session
	runtime.currentConnectorLimits[pending.connectorID] = connectorLimit
	runtime.currentHealthTargets[pending.connectorID] = healthTarget
	registry.authenticatedInstalls.Store(key, node)
	// Current generation 的变化必须唤醒仍等待旧 Session 的 Pending OPEN。
	// Eligibility Map 按完整 Session 保留旧值，AUTH flush 失败回滚后可立即恢复。
	runtime.signalEligibilityLocked()
	runtime.mu.Unlock()
	return &AuthenticatedSessionInstall{registry: registry, runtime: runtime, node: node}, nil
}

// Finalize 在 Success Frame 完整 flush 后终结替换，释放不再可恢复的
// 旧 Session 链。后续本地协议状态或连接交接失败不得重新打开该链。
func (install *AuthenticatedSessionInstall) Finalize() bool {
	if install == nil || install.registry == nil || install.runtime == nil || install.node == nil {
		return false
	}
	runtime := install.runtime
	runtime.mu.Lock()
	if install.node.state != authenticatedInstallPending {
		runtime.mu.Unlock()
		return false
	}
	install.node.state = authenticatedInstallFinalized
	releases := install.registry.discardAuthenticatedInstallChainLocked(runtime, install.node.previous)
	install.node.previous = nil
	key := authenticatedInstallKey{tunnelID: install.node.session.TunnelID, connectorID: install.node.session.ConnectorID}
	if head, found := install.registry.authenticatedInstalls.Load(key); found && head == install.node {
		install.registry.authenticatedInstalls.Delete(key)
	}
	runtime.mu.Unlock()
	install.registry.releaseAuthenticatedInstalls(releases)
	return true
}

// Rollback 在 Success flush 前失败时按 Current Session identity 做 CAS 回滚。
// 更新的并发替换已获胜时只标记本节点失败；后续回滚会跳过
// 所有失败节点，恢复最近仍有效的 Session。
func (install *AuthenticatedSessionInstall) Rollback() bool {
	if install == nil || install.registry == nil || install.runtime == nil || install.node == nil {
		return false
	}
	runtime := install.runtime
	runtime.mu.Lock()
	if install.node.state != authenticatedInstallPending {
		runtime.mu.Unlock()
		return false
	}
	install.node.state = authenticatedInstallFailed
	key := authenticatedInstallKey{tunnelID: install.node.session.TunnelID, connectorID: install.node.session.ConnectorID}
	head, found := install.registry.authenticatedInstalls.Load(key)
	if !found || head != install.node {
		runtime.mu.Unlock()
		return false
	}

	candidate := install.node.previous
	for candidate != nil && (candidate.state == authenticatedInstallFailed || candidate.state == authenticatedInstallDiscarded) {
		candidate = candidate.previous
	}
	if candidate == nil {
		delete(runtime.current, install.node.session.ConnectorID)
		delete(runtime.currentConnectorLimits, install.node.session.ConnectorID)
		delete(runtime.currentHealthTargets, install.node.session.ConnectorID)
		if len(runtime.current) == 0 {
			runtime.lastPicked = ""
		}
		install.registry.authenticatedInstalls.Delete(key)
	} else {
		runtime.current[install.node.session.ConnectorID] = candidate.session
		runtime.currentConnectorLimits[install.node.session.ConnectorID] = candidate.connectorLimit
		runtime.currentHealthTargets[install.node.session.ConnectorID] = candidate.healthTarget
		if candidate.state == authenticatedInstallPending {
			install.registry.authenticatedInstalls.Store(key, candidate)
		} else {
			install.registry.authenticatedInstalls.Delete(key)
		}
	}
	runtime.signalEligibilityLocked()
	releases := install.registry.discardAuthenticatedInstallPathLocked(runtime, install.node, candidate)
	runtime.mu.Unlock()
	install.registry.releaseAuthenticatedInstalls(releases)
	return true
}

// CancelAuthenticated 撤销尚未提交的 Session 预留。
func (registry *Registry) CancelAuthenticated(pending PendingSession) bool {
	if registry == nil {
		return false
	}
	runtime := registry.tunnel(pending.tunnelID, false)
	if runtime == nil {
		return false
	}
	runtime.mu.Lock()
	connectorID, exists := runtime.pending[pending.sessionID]
	if !exists || connectorID != pending.connectorID {
		runtime.mu.Unlock()
		return false
	}
	delete(runtime.pending, pending.sessionID)
	connectorLimit := runtime.pendingConnectorLimits[pending.sessionID]
	delete(runtime.pendingConnectorLimits, pending.sessionID)
	runtime.mu.Unlock()
	connectorLimit.Release()
	registry.releaseSessionID(pending.sessionID)
	return true
}

func (registry *Registry) discardAuthenticatedInstallPathLocked(
	runtime *TunnelRuntime,
	first *authenticatedInstallNode,
	keep *authenticatedInstallNode,
) authenticatedInstallReleases {
	var releases authenticatedInstallReleases
	for node := first; node != nil && node != keep; {
		next := node.previous
		node.previous = nil
		registry.discardAuthenticatedInstallNodeLocked(runtime, node, &releases)
		node = next
	}
	return releases
}

func (registry *Registry) discardAuthenticatedInstallChainLocked(
	runtime *TunnelRuntime,
	first *authenticatedInstallNode,
) authenticatedInstallReleases {
	return registry.discardAuthenticatedInstallPathLocked(runtime, first, nil)
}

func (registry *Registry) discardAuthenticatedInstallNodeLocked(
	runtime *TunnelRuntime,
	node *authenticatedInstallNode,
	releases *authenticatedInstallReleases,
) {
	if node == nil || node.state == authenticatedInstallDiscarded {
		return
	}
	node.state = authenticatedInstallDiscarded
	runtime.discardEligibilityLocked(node.session)
	releases.connectorLimits = append(releases.connectorLimits, node.connectorLimit)
	if sessionID := runtime.retireSessionLocked(node.session, node.healthTarget); sessionID != "" {
		releases.sessionIDs = append(releases.sessionIDs, sessionID)
	}
}

// discardAuthenticatedInstallsLocked 在 Tunnel Revoke 的同一 Runtime 临界区内
// 切断所有可回滚历史。Current 的 Lease 已由 revoke 主路径收集，这里
// 只返回被事务保留的旧代资源；所有 Release 仍在锁外执行。
func (registry *Registry) discardAuthenticatedInstallsLocked(runtime *TunnelRuntime) authenticatedInstallReleases {
	var releases authenticatedInstallReleases
	registry.authenticatedInstalls.Range(func(rawKey, rawHead any) bool {
		key := rawKey.(authenticatedInstallKey)
		if key.tunnelID != runtime.TunnelID {
			return true
		}
		head := rawHead.(*authenticatedInstallNode)
		registry.authenticatedInstalls.Delete(key)
		chainReleases := registry.discardAuthenticatedInstallChainLocked(runtime, head.previous)
		releases.connectorLimits = append(releases.connectorLimits, chainReleases.connectorLimits...)
		releases.sessionIDs = append(releases.sessionIDs, chainReleases.sessionIDs...)
		head.previous = nil
		head.state = authenticatedInstallDiscarded
		return true
	})
	return releases
}

func (registry *Registry) releaseAuthenticatedInstalls(releases authenticatedInstallReleases) {
	for _, connectorLimit := range releases.connectorLimits {
		connectorLimit.Release()
	}
	for _, sessionID := range releases.sessionIDs {
		registry.releaseSessionID(sessionID)
	}
}

// Current 返回指定 Tunnel Connector 当前拥有的 Session。
func (registry *Registry) Current(tunnelID, connectorID string) (Session, bool) {
	if registry == nil {
		return Session{}, false
	}
	runtime := registry.tunnel(tunnelID, false)
	if runtime == nil {
		return Session{}, false
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	session, exists := runtime.current[connectorID]
	return session, exists
}

// AcquireConnector 以“最少活跃 + 稳定 RR”从指定 Tunnel 选择 Current Connector。
// 选择与唯一负载计数递增都在该 TunnelRuntime.mu 内线性化。
func (registry *Registry) AcquireConnector(tunnelID string) (*ConnectorLease, error) {
	return registry.acquireConnectorWhere(tunnelID, "", nil)
}

// AcquireConnectorWhere 只在调用方判定 eligible 的 Current Session 中执行最少活跃
// + 稳定 RR。predicate 必须是快速、只读且不回调 Registry 的函数；Tunnel 数据面
// 用它排除没有 IDLE WorkConn 的 Session，避免空 Connector 饿死可用 Connector。
func (registry *Registry) AcquireConnectorWhere(tunnelID string, predicate func(Session) bool) (*ConnectorLease, error) {
	if predicate == nil {
		return nil, ErrNoAvailableConnector
	}
	return registry.acquireConnectorWhere(tunnelID, "", predicate)
}

// AcquireEligibleConnectorWhere 在同一 TunnelRuntime.mu 临界区内先执行
// Current/Revision/Health/Stale TTL 门禁，再使用 predicate 检查调用方预先取得的
// Pool 状态。predicate 不得回调 Registry 或 Session Manager。
func (registry *Registry) AcquireEligibleConnectorWhere(
	tunnelID, serviceID string,
	predicate func(Session) bool,
) (*ConnectorLease, error) {
	if predicate == nil || identity.ValidateServiceID(serviceID) != nil {
		return nil, ErrNoAvailableConnector
	}
	return registry.acquireConnectorWhere(tunnelID, serviceID, predicate)
}

func (registry *Registry) acquireConnectorWhere(tunnelID, serviceID string, predicate func(Session) bool) (*ConnectorLease, error) {
	if err := identity.ValidateTunnelID(tunnelID); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidTunnelID, err)
	}
	if registry == nil {
		return nil, ErrNoAvailableConnector
	}
	runtime := registry.tunnel(tunnelID, false)
	if runtime == nil {
		return nil, ErrNoAvailableConnector
	}

	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.revoked {
		return nil, ErrTunnelRuntimeRevoked
	}
	candidates := make([]Session, 0, len(runtime.current))
	for _, session := range runtime.current {
		if serviceID != "" && !runtime.sessionEligibleLocked(session, serviceID, runtime.now()) {
			continue
		}
		if predicate != nil && !predicate(session) {
			continue
		}
		candidates = append(candidates, session)
	}
	if len(candidates) == 0 {
		return nil, ErrNoAvailableConnector
	}
	slices.SortFunc(candidates, func(left, right Session) int {
		return cmp.Compare(left.ConnectorID, right.ConnectorID)
	})

	// 旧 generation ACTIVE 仍消耗同一 Connector 容量，因此先按 Connector 聚合。
	connectorLoads := make(map[string]uint64, len(candidates))
	for session, count := range runtime.connectorActive {
		connectorLoads[session.ConnectorID] += count
	}
	minimum := connectorLoads[candidates[0].ConnectorID]
	for _, candidate := range candidates[1:] {
		if count := connectorLoads[candidate.ConnectorID]; count < minimum {
			minimum = count
		}
	}
	start := 0
	if runtime.lastPicked != "" {
		for index, candidate := range candidates {
			if candidate.ConnectorID > runtime.lastPicked {
				start = index
				break
			}
		}
	}
	selected := candidates[0]
	for offset := range len(candidates) {
		candidate := candidates[(start+offset)%len(candidates)]
		if connectorLoads[candidate.ConnectorID] == minimum {
			selected = candidate
			break
		}
	}
	runtime.connectorActive[selected]++
	runtime.lastPicked = selected.ConnectorID
	return &ConnectorLease{runtime: runtime, session: selected, lifecycle: new(atomic.Uint32)}, nil
}

func (runtime *TunnelRuntime) releaseConnector(session Session) bool {
	runtime.mu.Lock()
	count, exists := runtime.connectorActive[session]
	if !exists || count == 0 {
		runtime.mu.Unlock()
		return false
	}
	if count == 1 {
		delete(runtime.connectorActive, session)
	} else {
		runtime.connectorActive[session] = count - 1
	}
	releaseSessionID := runtime.releaseRetiredSessionIfUnusedLocked(session)
	runtime.mu.Unlock()
	if releaseSessionID != "" {
		runtime.registry.releaseSessionID(releaseSessionID)
	}
	return true
}

// retireSessionLocked 在 Session 离开 Current 后决定能否立即释放其全局 ID 与
// Health Target 引用。调用方必须持有 runtime.mu；Health Release 遵循固定的
// TunnelRuntime.mu -> healthbudget.Manager.mu 锁序，全局 Session ID 仍在锁外清理。
func (runtime *TunnelRuntime) retireSessionLocked(
	session Session,
	healthTarget *healthbudget.ConnectorLease,
) string {
	if runtime.sessionReferencedLocked(session) {
		runtime.retired[session] = struct{}{}
		runtime.retiredHealthTargets[session] = healthTarget
		return ""
	}
	runtime.releaseHealthTargetLocked(healthTarget)
	return session.SessionID
}

// releaseRetiredSessionIfUnusedLocked 在最后一个运行时引用归零时摘除 retired。
// Health Target 与摘除在同一 Runtime 临界区内线性化；返回的 ID 由调用方在释放
// runtime.mu 后清理，以保持 Tunnel 锁与全局索引锁不嵌套。
func (runtime *TunnelRuntime) releaseRetiredSessionIfUnusedLocked(session Session) string {
	if _, exists := runtime.retired[session]; !exists || runtime.sessionReferencedLocked(session) {
		return ""
	}
	delete(runtime.retired, session)
	healthTarget := runtime.retiredHealthTargets[session]
	delete(runtime.retiredHealthTargets, session)
	runtime.releaseHealthTargetLocked(healthTarget)
	return session.SessionID
}

func (runtime *TunnelRuntime) releaseHealthTargetLocked(lease *healthbudget.ConnectorLease) {
	if lease != nil && !lease.Release() {
		panic("runtime health target lease released more than once")
	}
}

func (runtime *TunnelRuntime) sessionReferencedLocked(session Session) bool {
	if runtime.connectorActive[session] > 0 {
		return true
	}
	for _, work := range runtime.activeWorks {
		identity := work.identity
		if identity.TunnelID == session.TunnelID && identity.ConnectorID == session.ConnectorID &&
			identity.SessionID == session.SessionID && identity.Generation == session.Generation {
			return true
		}
	}
	return false
}

// ClearIfCurrent 只在完整 identity 匹配 Current 时摘除当前代；若更新 replacement
// 已预安装，则只把匹配的历史候选标记为不可恢复。已进入 ACTIVE 的旧 Work 及
// Connector Lease tombstone 不受影响。
func (registry *Registry) ClearIfCurrent(session Session) bool {
	_, cleared := registry.disconnectIfCurrent(session, "")
	return cleared
}

// DisconnectIfCurrent 把 Current Session 的摘除、Tombstone 决策和断开事件在同一
// TunnelRuntime 临界区内线性化。reason 只能是调用方定义的非敏感稳定原因。
func (registry *Registry) DisconnectIfCurrent(session Session, reason string) (ConnectorLifecycleEvent, bool) {
	return registry.disconnectIfCurrent(session, reason)
}

func (registry *Registry) disconnectIfCurrent(session Session, reason string) (ConnectorLifecycleEvent, bool) {
	if registry == nil || !identity.ValidTunnelID(session.TunnelID) || !identity.ValidConnectorID(session.ConnectorID) ||
		!identity.ValidSessionID(session.SessionID) || session.Generation == 0 {
		return ConnectorLifecycleEvent{}, false
	}
	runtime := registry.tunnel(session.TunnelID, false)
	if runtime == nil {
		return ConnectorLifecycleEvent{}, false
	}
	runtime.mu.Lock()
	current, exists := runtime.current[session.ConnectorID]
	if !exists || current != session {
		// replacement 预安装后，旧 Session 已不是 Current，但仍可能是
		// rollback 候选。旧 Owner 结束时必须把它标记为不可恢复；
		// 当前更新 Session 不受影响，资源由 head 的 rollback/finalize 统一释放。
		key := authenticatedInstallKey{tunnelID: session.TunnelID, connectorID: session.ConnectorID}
		if rawHead, found := registry.authenticatedInstalls.Load(key); found {
			for node := rawHead.(*authenticatedInstallNode).previous; node != nil; node = node.previous {
				if node.session == session && node.state != authenticatedInstallFailed && node.state != authenticatedInstallDiscarded {
					node.state = authenticatedInstallFailed
					runtime.discardEligibilityLocked(session)
					runtime.mu.Unlock()
					return ConnectorLifecycleEvent{}, true
				}
			}
		}
		runtime.mu.Unlock()
		return ConnectorLifecycleEvent{}, false
	}
	delete(runtime.current, session.ConnectorID)
	delete(runtime.eligibility, session)
	// 即使该 Session 尚未发布 Eligibility，Current 摘除也必须唤醒旧 waiter。
	runtime.signalEligibilityLocked()
	connectorLimit := runtime.currentConnectorLimits[session.ConnectorID]
	delete(runtime.currentConnectorLimits, session.ConnectorID)
	healthTarget := runtime.currentHealthTargets[session.ConnectorID]
	delete(runtime.currentHealthTargets, session.ConnectorID)
	if len(runtime.current) == 0 {
		runtime.lastPicked = ""
	}
	event := runtime.disconnectObservationLocked(session, reason)
	releaseSessionID := runtime.retireSessionLocked(session, healthTarget)
	runtime.mu.Unlock()
	connectorLimit.Release()
	if releaseSessionID != "" {
		registry.releaseSessionID(releaseSessionID)
	}
	return event, true
}

func (registry *Registry) reserveSessionID(sessionID string) bool {
	registry.sessionIDsMu.Lock()
	defer registry.sessionIDsMu.Unlock()
	if _, exists := registry.sessionIDs[sessionID]; exists {
		return false
	}
	registry.sessionIDs[sessionID] = struct{}{}
	return true
}

func (registry *Registry) releaseSessionID(sessionID string) {
	registry.sessionIDsMu.Lock()
	defer registry.sessionIDsMu.Unlock()
	delete(registry.sessionIDs, sessionID)
}
