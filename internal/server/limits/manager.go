// Package limits 统一管理 XTunnel Server 进程内的硬资源预算。
package limits

import (
	"errors"
	"net/netip"
	"sync"
	"time"

	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
)

var (
	// ErrInvalidOptions 表示一个或多个硬上限为零，或子上限大于总上限。
	ErrInvalidOptions = errors.New("server limit manager options are invalid")
	// ErrInvalidConnectionKey 表示公网连接缺少合法的 Tunnel、Service 或来源 IP。
	ErrInvalidConnectionKey = errors.New("server connection limit key is invalid")
	// ErrWorkCapacity 表示 Server 进程的 WorkConn 总量已满。
	ErrWorkCapacity = errors.New("server work connection capacity is exhausted")
	// ErrConnectingWorkCapacity 表示 CONNECTING WorkConn 总量已满。
	ErrConnectingWorkCapacity = errors.New("server connecting work connection capacity is exhausted")
	// ErrIdleWorkCapacity 表示 IDLE WorkConn 总量已满。
	ErrIdleWorkCapacity = errors.New("server idle work connection capacity is exhausted")
	// ErrPendingOpenCapacity 表示尚未完成 OPEN 的公网连接总量已满。
	ErrPendingOpenCapacity = errors.New("server pending OPEN capacity is exhausted")
	// ErrActiveConnectionCapacity 表示 ACTIVE 连接命中了全局或某一维度硬上限。
	ErrActiveConnectionCapacity = errors.New("server active connection capacity is exhausted")
	// ErrOpenRateExceeded 表示来源 IP 的新建 Tunnel WorkConn 速率已耗尽。
	ErrOpenRateExceeded = errors.New("server source OPEN rate limit is exceeded")
	// ErrHTTPRequestRateExceeded 表示来源 IP 的 HTTP 请求速率已耗尽。
	ErrHTTPRequestRateExceeded = errors.New("server source HTTP request rate limit is exceeded")
	// ErrConnectorCapacity 表示 Connector 身份命中了全局或 Tunnel 硬上限。
	ErrConnectorCapacity = errors.New("server connector capacity is exhausted")
	// ErrInvalidTransition 表示 Lease 的状态转换不符合冻结生命周期。
	ErrInvalidTransition = errors.New("server limit lease transition is invalid")
)

// Options 固定 Server 进程级 WorkConn、OPEN、ACTIVE 与来源 IP 速率上限。
type Options struct {
	MaxConnectors                       uint64
	MaxConnectorsPerTunnel              uint64
	MaxWorkConnections                  uint64
	MaxIdleWorkConnections              uint64
	MaxConnectingWorkConnections        uint64
	MaxPendingOpens                     uint64
	MaxActiveConnections                uint64
	MaxConnectionsPerTunnel             uint64
	MaxConnectionsPerService            uint64
	MaxConnectionsPerSourceIP           uint64
	MaxOpenRatePerSourceIP              uint64
	MaxOpenBurstPerSourceIP             uint64
	MaxHTTPRequestsPerSourceIPPerSecond uint64
}

// ConnectionKey 是 ACTIVE 配额使用的三个公平性维度。
type ConnectionKey struct {
	TunnelID  string
	ServiceID string
	SourceIP  netip.Addr
}

type serviceKey struct {
	tunnelID  string
	serviceID string
}

// Snapshot 是某一线性化时刻的资源计数副本。
type Snapshot struct {
	Connectors         uint64
	ConnectorsByTunnel map[string]uint64
	WorkTotal          uint64
	WorkConnecting     uint64
	WorkIdle           uint64
	PendingOpens       uint64
	ActiveTotal        uint64
	ActiveByTunnel     map[string]uint64
	ActiveByService    map[ConnectionService]uint64
	ActiveBySource     map[netip.Addr]uint64
}

// ConnectionService 避免只用 Service ID 时丢失其所属 Tunnel 维度。
type ConnectionService struct {
	TunnelID  string
	ServiceID string
}

// Manager 在一把互斥锁下线性化全部资源计数。
//
// WorkPool 若需要同时持有自己的锁，固定使用 WorkPool.mu -> Manager.mu 的顺序；
// Manager 从不回调 WorkPool，也不在锁内执行网络 IO，因此不会形成反向锁序或把慢
// 连接带入全局临界区。单锁还保证 PendingOpen -> Active 的多维校验与计数迁移原子化。
type Manager struct {
	mu sync.Mutex

	options            Options
	connectors         uint64
	connectorRefs      map[connectorKey]uint64
	connectorsByTunnel map[string]uint64

	workTotal       uint64
	workConnecting  uint64
	workIdle        uint64
	pendingOpens    uint64
	activeTotal     uint64
	activeByTunnel  map[string]uint64
	activeByService map[serviceKey]uint64
	activeBySource  map[netip.Addr]uint64
	openRate        *sourceRateLimiter
	httpRequestRate *sourceRateLimiter
}

type connectorKey struct {
	tunnelID    string
	connectorID string
}

// New 创建一个空的进程级资源管理器。
func New(options Options) (*Manager, error) {
	const maxDurationSeconds = uint64((1<<63 - 1) / 1_000_000_000)
	if options.MaxActiveConnections > ^uint64(0)-options.MaxPendingOpens {
		return nil, ErrInvalidOptions
	}
	var openRefillSeconds uint64
	if options.MaxOpenRatePerSourceIP != 0 {
		openRefillSeconds = options.MaxOpenBurstPerSourceIP / options.MaxOpenRatePerSourceIP
		if options.MaxOpenBurstPerSourceIP%options.MaxOpenRatePerSourceIP != 0 {
			openRefillSeconds++
		}
	}
	if options.MaxConnectors == 0 || options.MaxConnectorsPerTunnel == 0 ||
		options.MaxConnectorsPerTunnel > options.MaxConnectors ||
		options.MaxWorkConnections == 0 || options.MaxIdleWorkConnections == 0 ||
		options.MaxConnectingWorkConnections == 0 || options.MaxPendingOpens == 0 ||
		options.MaxActiveConnections == 0 || options.MaxConnectionsPerTunnel == 0 ||
		options.MaxConnectionsPerService == 0 || options.MaxConnectionsPerSourceIP == 0 ||
		options.MaxOpenRatePerSourceIP == 0 || options.MaxOpenBurstPerSourceIP == 0 ||
		options.MaxHTTPRequestsPerSourceIPPerSecond == 0 || openRefillSeconds > maxDurationSeconds ||
		options.MaxIdleWorkConnections > options.MaxWorkConnections ||
		options.MaxConnectingWorkConnections > options.MaxWorkConnections {
		return nil, ErrInvalidOptions
	}
	sourceCapacity := options.MaxActiveConnections + options.MaxPendingOpens
	return &Manager{
		options:            options,
		connectorRefs:      make(map[connectorKey]uint64),
		connectorsByTunnel: make(map[string]uint64),
		activeByTunnel:     make(map[string]uint64),
		activeByService:    make(map[serviceKey]uint64),
		activeBySource:     make(map[netip.Addr]uint64),
		openRate: newSourceRateLimiter(
			options.MaxOpenRatePerSourceIP,
			options.MaxOpenBurstPerSourceIP,
			sourceCapacity,
			time.Now,
		),
		httpRequestRate: newSourceRateLimiter(
			options.MaxHTTPRequestsPerSourceIPPerSecond,
			options.MaxHTTPRequestsPerSourceIPPerSecond,
			sourceCapacity,
			time.Now,
		),
	}, nil
}

// ConnectorLease 表示一个待提交或已提交 Control Session 对 Connector 身份的引用。
// 同一 Tunnel/Connector 的 generation replacement 会短暂持有两个 Lease，但只计
// 一个 Connector；最后一个引用释放时才归还全局与 Tunnel 槽位。
type ConnectorLease struct {
	manager  *Manager
	key      connectorKey
	released bool // 仅由 manager.mu 保护。
}

// AcquireConnector 为 Control Auth 在 Success flush 前预留 Connector 身份预算。
func (manager *Manager) AcquireConnector(tunnelID, connectorID string) (*ConnectorLease, error) {
	if manager == nil || identity.ValidateTunnelID(tunnelID) != nil ||
		identity.ValidateConnectorID(connectorID) != nil {
		return nil, ErrInvalidConnectionKey
	}
	key := connectorKey{tunnelID: tunnelID, connectorID: connectorID}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if references := manager.connectorRefs[key]; references > 0 {
		manager.connectorRefs[key] = references + 1
		return &ConnectorLease{manager: manager, key: key}, nil
	}
	if manager.connectors >= manager.options.MaxConnectors ||
		manager.connectorsByTunnel[tunnelID] >= manager.options.MaxConnectorsPerTunnel {
		return nil, ErrConnectorCapacity
	}
	manager.connectors++
	manager.connectorsByTunnel[tunnelID]++
	manager.connectorRefs[key] = 1
	return &ConnectorLease{manager: manager, key: key}, nil
}

// Release 归还一个待提交/已提交 Session 引用；并发重复调用只执行一次。
func (lease *ConnectorLease) Release() {
	if lease == nil || lease.manager == nil {
		return
	}
	manager := lease.manager
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if lease.released {
		return
	}
	references := manager.connectorRefs[lease.key]
	if references == 0 {
		panic("server connector limit counter invariant violated")
	}
	if references > 1 {
		manager.connectorRefs[lease.key] = references - 1
	} else {
		delete(manager.connectorRefs, lease.key)
		manager.connectors--
		decrementMap(manager.connectorsByTunnel, lease.key.tunnelID)
	}
	lease.released = true
}

type workState uint8

const (
	workConnecting workState = iota + 1
	workIdle
	workOpening
	workActive
	workReleased
)

// WorkLease 覆盖一条 WorkConn 从 CONNECTING 到 ACTIVE/CLOSED 的完整生命周期。
// Release 可并发、重复调用，但全局计数只归还一次。
type WorkLease struct {
	manager *Manager
	state   workState // 仅由 manager.mu 保护。
}

// AcquireWork 原子预留 WorkConn 总量与 CONNECTING 两个槽位。
func (manager *Manager) AcquireWork() (*WorkLease, error) {
	if manager == nil {
		return nil, ErrInvalidOptions
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.workTotal >= manager.options.MaxWorkConnections {
		return nil, ErrWorkCapacity
	}
	if manager.workConnecting >= manager.options.MaxConnectingWorkConnections {
		return nil, ErrConnectingWorkCapacity
	}
	manager.workTotal++
	manager.workConnecting++
	return &WorkLease{manager: manager, state: workConnecting}, nil
}

// MarkIdle 原子提交 CONNECTING -> IDLE，并在提交前校验全局 IDLE 上限。
func (lease *WorkLease) MarkIdle() error {
	if lease == nil || lease.manager == nil {
		return ErrInvalidTransition
	}
	manager := lease.manager
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if lease.state != workConnecting {
		return ErrInvalidTransition
	}
	if manager.workIdle >= manager.options.MaxIdleWorkConnections {
		return ErrIdleWorkCapacity
	}
	manager.workConnecting--
	manager.workIdle++
	lease.state = workIdle
	return nil
}

// MarkOpening 提交 IDLE -> OPENING；Work 总量仍被 Lease 持有。
func (lease *WorkLease) MarkOpening() error {
	if lease == nil || lease.manager == nil {
		return ErrInvalidTransition
	}
	manager := lease.manager
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if lease.state != workIdle {
		return ErrInvalidTransition
	}
	manager.workIdle--
	lease.state = workOpening
	return nil
}

// MarkActive 提交 OPENING -> ACTIVE；Work 总量继续计入全局硬上限。
func (lease *WorkLease) MarkActive() error {
	if lease == nil || lease.manager == nil {
		return ErrInvalidTransition
	}
	manager := lease.manager
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if lease.state != workOpening {
		return ErrInvalidTransition
	}
	lease.state = workActive
	return nil
}

// Release 归还 WorkConn 当前状态占用的全部槽位；重复调用是安全的空操作。
func (lease *WorkLease) Release() {
	if lease == nil || lease.manager == nil {
		return
	}
	manager := lease.manager
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if lease.state == workReleased {
		return
	}
	switch lease.state {
	case workConnecting:
		manager.workConnecting--
	case workIdle:
		manager.workIdle--
	case workOpening, workActive:
	default:
		return
	}
	manager.workTotal--
	lease.state = workReleased
}

type openState uint8

const (
	openPending openState = iota + 1
	openActive
	openReleased
)

// OpenLease 覆盖公网连接从等待 OPEN 到 ACTIVE/CLOSED 的完整生命周期。
// Activate 与 Release 都由 Manager 单锁线性化，避免多个限制维度部分提交。
type OpenLease struct {
	manager *Manager
	key     ConnectionKey
	state   openState // 仅由 manager.mu 保护。
}

// ActiveLease 表示不经过 Pending OPEN 的独立 ACTIVE 生命周期。HTTP 请求与
// WebSocket Handler 用它计量请求处理时段，不把可复用的 Tunnel WorkConn 错算成
// 首个来源 IP 的长期占用。
type ActiveLease struct {
	manager  *Manager
	key      ConnectionKey
	released bool // 仅由 manager.mu 保护。
}

// AllowOpen 消耗来源 IP 的一个新建 Tunnel WorkConn Token。Token 一经消费不会因
// 后续 OPEN 失败而退还，避免失败流量绕开入口速率上限。
func (manager *Manager) AllowOpen(sourceIP netip.Addr) error {
	if manager == nil {
		return ErrInvalidOptions
	}
	if !sourceIP.IsValid() {
		return ErrInvalidConnectionKey
	}
	if !manager.openRate.allow(sourceIP) {
		return ErrOpenRateExceeded
	}
	return nil
}

// AllowHTTPRequest 消耗来源 IP 的一个 HTTP 请求 Token。
func (manager *Manager) AllowHTTPRequest(sourceIP netip.Addr) error {
	if manager == nil {
		return ErrInvalidOptions
	}
	if !sourceIP.IsValid() {
		return ErrInvalidConnectionKey
	}
	if !manager.httpRequestRate.allow(sourceIP) {
		return ErrHTTPRequestRateExceeded
	}
	return nil
}

// AcquirePendingOpen 为一条已接受、尚未完成 OPEN 的公网连接预留全局槽位。
func (manager *Manager) AcquirePendingOpen(key ConnectionKey) (*OpenLease, error) {
	if manager == nil {
		return nil, ErrInvalidConnectionKey
	}
	key, valid := normalizeConnectionKey(key)
	if !valid {
		return nil, ErrInvalidConnectionKey
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.pendingOpens >= manager.options.MaxPendingOpens {
		return nil, ErrPendingOpenCapacity
	}
	manager.pendingOpens++
	return &OpenLease{manager: manager, key: key, state: openPending}, nil
}

// AcquireActive 原子校验并占用全局、Tunnel、Service 与来源 IP 四级 ACTIVE
// 配额。它不占用 Pending OPEN，供已经在 HTTP Handler 生命周期中完成路由解析的
// 请求使用。
func (manager *Manager) AcquireActive(key ConnectionKey) (*ActiveLease, error) {
	if manager == nil {
		return nil, ErrInvalidConnectionKey
	}
	key, valid := normalizeConnectionKey(key)
	if !valid {
		return nil, ErrInvalidConnectionKey
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if !manager.acquireActiveLocked(key) {
		return nil, ErrActiveConnectionCapacity
	}
	return &ActiveLease{manager: manager, key: key}, nil
}

// Activate 原子执行 PendingOpen -> Active，并同时校验全局、Tunnel、Service 与来源 IP。
func (lease *OpenLease) Activate() error {
	if lease == nil || lease.manager == nil {
		return ErrInvalidTransition
	}
	manager := lease.manager
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if lease.state != openPending {
		return ErrInvalidTransition
	}
	if !manager.acquireActiveLocked(lease.key) {
		return ErrActiveConnectionCapacity
	}
	manager.pendingOpens--
	lease.state = openActive
	return nil
}

// Release 归还 Pending 或 Active 槽位；重复调用是安全的空操作。
func (lease *OpenLease) Release() {
	if lease == nil || lease.manager == nil {
		return
	}
	manager := lease.manager
	manager.mu.Lock()
	defer manager.mu.Unlock()
	switch lease.state {
	case openPending:
		manager.pendingOpens--
	case openActive:
		manager.releaseActiveLocked(lease.key)
	case openReleased:
		return
	default:
		return
	}
	lease.state = openReleased
}

// Release 归还四级 ACTIVE 配额；并发重复调用只释放一次。
func (lease *ActiveLease) Release() {
	if lease == nil || lease.manager == nil {
		return
	}
	manager := lease.manager
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if lease.released {
		return
	}
	manager.releaseActiveLocked(lease.key)
	lease.released = true
}

func normalizeConnectionKey(key ConnectionKey) (ConnectionKey, bool) {
	if identity.ValidateTunnelID(key.TunnelID) != nil ||
		validate.ValidateID(key.ServiceID, "svc_") != nil || !key.SourceIP.IsValid() {
		return ConnectionKey{}, false
	}
	key.SourceIP = key.SourceIP.Unmap()
	return key, true
}

func (manager *Manager) acquireActiveLocked(key ConnectionKey) bool {
	service := serviceKey{tunnelID: key.TunnelID, serviceID: key.ServiceID}
	if manager.activeTotal >= manager.options.MaxActiveConnections ||
		manager.activeByTunnel[key.TunnelID] >= manager.options.MaxConnectionsPerTunnel ||
		manager.activeByService[service] >= manager.options.MaxConnectionsPerService ||
		manager.activeBySource[key.SourceIP] >= manager.options.MaxConnectionsPerSourceIP {
		return false
	}
	manager.activeTotal++
	manager.activeByTunnel[key.TunnelID]++
	manager.activeByService[service]++
	manager.activeBySource[key.SourceIP]++
	return true
}

func (manager *Manager) releaseActiveLocked(key ConnectionKey) {
	service := serviceKey{tunnelID: key.TunnelID, serviceID: key.ServiceID}
	manager.activeTotal--
	decrementMap(manager.activeByTunnel, key.TunnelID)
	decrementMap(manager.activeByService, service)
	decrementMap(manager.activeBySource, key.SourceIP)
}

// Snapshot 返回计数与各 ACTIVE 维度 Map 的深拷贝。
func (manager *Manager) Snapshot() Snapshot {
	if manager == nil {
		return Snapshot{}
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	snapshot := Snapshot{
		Connectors: manager.connectors,
		WorkTotal:  manager.workTotal, WorkConnecting: manager.workConnecting,
		WorkIdle: manager.workIdle, PendingOpens: manager.pendingOpens,
		ActiveTotal:        manager.activeTotal,
		ConnectorsByTunnel: make(map[string]uint64, len(manager.connectorsByTunnel)),
		ActiveByTunnel:     make(map[string]uint64, len(manager.activeByTunnel)),
		ActiveByService:    make(map[ConnectionService]uint64, len(manager.activeByService)),
		ActiveBySource:     make(map[netip.Addr]uint64, len(manager.activeBySource)),
	}
	for key, count := range manager.connectorsByTunnel {
		snapshot.ConnectorsByTunnel[key] = count
	}
	for key, count := range manager.activeByTunnel {
		snapshot.ActiveByTunnel[key] = count
	}
	for key, count := range manager.activeByService {
		snapshot.ActiveByService[ConnectionService{TunnelID: key.tunnelID, ServiceID: key.serviceID}] = count
	}
	for key, count := range manager.activeBySource {
		snapshot.ActiveBySource[key] = count
	}
	return snapshot
}

func decrementMap[K comparable](counts map[K]uint64, key K) {
	count := counts[key]
	// 所有调用都来自一个仍持有的 Active Lease；零值代表内部不变量已经损坏，
	// 不应以静默钳制掩盖计数泄漏。
	if count == 0 {
		panic("server limit manager counter invariant violated")
	}
	if count == 1 {
		delete(counts, key)
		return
	}
	counts[key] = count - 1
}
