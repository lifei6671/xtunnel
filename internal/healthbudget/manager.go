// Package healthbudget 管理 Server 进程内的两级 Health Target 硬预算。
package healthbudget

import (
	"errors"
	"fmt"
	"sync"

	"github.com/lifei6671/xtunnel/internal/identity"
)

var (
	// ErrInvalidOptions 表示两级 Health Target 上限无效。
	ErrInvalidOptions = errors.New("health target budget options are invalid")
	// ErrInvalidTunnelID 表示 Tunnel ID 不符合 Protocol v1 标识符约束。
	ErrInvalidTunnelID = errors.New("health target budget tunnel ID is invalid")
	// ErrInvalidConnectorID 表示 Connector ID 不符合 Protocol v1 标识符约束。
	ErrInvalidConnectorID = errors.New("health target budget connector ID is invalid")
	// ErrTunnelNotInitialized 表示启动阶段尚未装载该 Tunnel 的已提交配置。
	ErrTunnelNotInitialized = errors.New("health target budget tunnel is not initialized")
	// ErrTunnelAlreadyInitialized 表示同一 Tunnel 被不同启动基线重复初始化。
	ErrTunnelAlreadyInitialized = errors.New("health target budget tunnel is already initialized")
	// ErrConfigurationRevision 表示 Candidate Revision 未前进。
	ErrConfigurationRevision = errors.New("health target budget configuration revision is stale")
	// ErrConfigurationConflict 表示同一 Tunnel 已有尚未终结的配置 Reservation。
	ErrConfigurationConflict = errors.New("health target budget configuration reservation conflicts")
	// ErrTargetCapacity 表示 Candidate 配置或新 Connector 会突破 Tunnel/Global 上限。
	ErrTargetCapacity = errors.New("health target budget capacity is exhausted")
)

// Options 固定单 Tunnel 与 Server 全局 Health Target 上限。
type Options struct {
	MaxTargetsPerTunnel uint64
	MaxTargetsGlobal    uint64
}

// ConnectorKey 是 Runtime Health Budget 的唯一所有权键。
// Session generation 只负责 fencing，不参与计费。
type ConnectorKey struct {
	TunnelID    string
	ConnectorID string
}

// TunnelSnapshot 是某一线性化时刻的单 Tunnel 预算状态。
type TunnelSnapshot struct {
	Revision                  uint64
	EnabledCount              uint64
	ConnectorCount            uint64
	Targets                   uint64
	ReservationActive         bool
	ReservationRevision       uint64
	ReservationCandidateCount uint64
}

// Snapshot 是预算总量、Tunnel 状态和 Connector 引用数的深拷贝。
type Snapshot struct {
	TargetsGlobal       uint64
	Tunnels             map[string]TunnelSnapshot
	ConnectorReferences map[ConnectorKey]uint64
}

// Manager 使用一把互斥锁线性化配置 Reservation 与 Connector Auth。
//
// 调用方需要同时持有 Tunnel Runtime 锁时，固定顺序必须是
// TunnelRuntime.mu -> Manager.mu。Manager 不回调、不执行 IO，也不等待 Channel，
// 因而不会在预算锁内反向获取 Runtime 锁或引入不可控阻塞。
type Manager struct {
	mu sync.Mutex

	options       Options
	targetsGlobal uint64
	tunnels       map[string]*tunnelState
	connectorRefs map[ConnectorKey]uint64
}

type tunnelState struct {
	revision       uint64
	enabledCount   uint64
	connectorCount uint64
	targets        uint64
	reservation    *configurationReservation
}

type configurationReservation struct {
	revision       uint64
	candidateCount uint64
	finalized      bool // 仅由 Manager.mu 保护，Lease 副本共享同一状态。
}

type leaseLifecycle struct {
	released bool // 仅由 Manager.mu 保护，Lease 副本共享同一状态。
}

// New 创建一个空的 Health Target Budget Manager。
func New(options Options) (*Manager, error) {
	if options.MaxTargetsPerTunnel == 0 || options.MaxTargetsGlobal == 0 ||
		options.MaxTargetsPerTunnel > options.MaxTargetsGlobal {
		return nil, ErrInvalidOptions
	}
	return &Manager{
		options:       options,
		tunnels:       make(map[string]*tunnelState),
		connectorRefs: make(map[ConnectorKey]uint64),
	}, nil
}

// InitializeTunnel 从 SQLite Desired State 装载一个 Tunnel 的已提交启动基线。
// 相同 Revision 与计数的重复调用幂等；不同基线快速失败。
func (manager *Manager) InitializeTunnel(tunnelID string, revision, enabledCount uint64) error {
	if manager == nil {
		return ErrInvalidOptions
	}
	if err := identity.ValidateTunnelID(tunnelID); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidTunnelID, err)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if current, exists := manager.tunnels[tunnelID]; exists {
		if current.revision == revision && current.enabledCount == enabledCount && current.reservation == nil {
			return nil
		}
		return ErrTunnelAlreadyInitialized
	}
	manager.tunnels[tunnelID] = &tunnelState{revision: revision, enabledCount: enabledCount}
	return nil
}

// ConfigurationLease 表示一个尚未提交或释放的配置 Target Reservation。
// Commit 与 Release 可并发或重复调用，但只有首个终结操作返回 true 并改变计数。
type ConfigurationLease struct {
	manager     *Manager
	tunnelID    string
	reservation *configurationReservation
}

// ReserveConfiguration 按 Candidate 配置对当前唯一 Connector 集合预留 Target。
// 增容立即占槽；减容在 Commit 前仍按旧配置计费，使事务失败 Release 后不会超额。
func (manager *Manager) ReserveConfiguration(
	tunnelID string,
	revision uint64,
	candidateCount uint64,
) (*ConfigurationLease, error) {
	if manager == nil {
		return nil, ErrInvalidOptions
	}
	if err := identity.ValidateTunnelID(tunnelID); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidTunnelID, err)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	state := manager.tunnels[tunnelID]
	if state == nil {
		return nil, ErrTunnelNotInitialized
	}
	if state.reservation != nil {
		return nil, ErrConfigurationConflict
	}
	if revision <= state.revision {
		return nil, ErrConfigurationRevision
	}
	projectedEnabled := max(state.enabledCount, candidateCount)
	projectedTargets, ok := targetsWithin(state.connectorCount, projectedEnabled, manager.options.MaxTargetsPerTunnel)
	if !ok || projectedTargets-state.targets > manager.options.MaxTargetsGlobal-manager.targetsGlobal {
		return nil, ErrTargetCapacity
	}
	reservation := &configurationReservation{revision: revision, candidateCount: candidateCount}
	manager.targetsGlobal += projectedTargets - state.targets
	state.targets = projectedTargets
	state.reservation = reservation
	return &ConfigurationLease{manager: manager, tunnelID: tunnelID, reservation: reservation}, nil
}

// Commit 提交 Candidate Revision 与 health-enabled Service 数量。
func (lease *ConfigurationLease) Commit() bool {
	return lease.finalize(true)
}

// Release 放弃 Candidate，并归还只为本次 Candidate 增加的 Target。
func (lease *ConfigurationLease) Release() bool {
	return lease.finalize(false)
}

func (lease *ConfigurationLease) finalize(commit bool) bool {
	if lease == nil || lease.manager == nil || lease.reservation == nil {
		return false
	}
	manager := lease.manager
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if lease.reservation.finalized {
		return false
	}
	state := manager.tunnels[lease.tunnelID]
	if state == nil || state.reservation != lease.reservation {
		panic("health target budget reservation fencing invariant violated")
	}
	enabledCount := state.enabledCount
	if commit {
		enabledCount = lease.reservation.candidateCount
		state.revision = lease.reservation.revision
		state.enabledCount = enabledCount
	}
	committedTargets := state.connectorCount * enabledCount
	manager.targetsGlobal -= state.targets - committedTargets
	state.targets = committedTargets
	state.reservation = nil
	lease.reservation.finalized = true
	return true
}

// ConnectorLease 表示一个 Control Session 对唯一 Runtime Owner Key 的引用。
// 同 Key replacement 可同时持有多个引用，但只计一个 Connector 的 Target。
type ConnectorLease struct {
	manager   *Manager
	key       ConnectorKey
	lifecycle *leaseLifecycle
}

// AcquireConnector 为 Connector Auth 预留该 Tunnel 当前或在途配置的 Target。
func (manager *Manager) AcquireConnector(tunnelID, connectorID string) (*ConnectorLease, error) {
	if manager == nil {
		return nil, ErrInvalidOptions
	}
	if err := identity.ValidateTunnelID(tunnelID); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidTunnelID, err)
	}
	if err := identity.ValidateConnectorID(connectorID); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConnectorID, err)
	}
	key := ConnectorKey{TunnelID: tunnelID, ConnectorID: connectorID}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	state := manager.tunnels[tunnelID]
	if state == nil {
		return nil, ErrTunnelNotInitialized
	}
	if references := manager.connectorRefs[key]; references > 0 {
		manager.connectorRefs[key] = references + 1
		return &ConnectorLease{manager: manager, key: key, lifecycle: &leaseLifecycle{}}, nil
	}
	enabledCount := state.enabledCount
	if state.reservation != nil {
		enabledCount = max(enabledCount, state.reservation.candidateCount)
	}
	if state.connectorCount == ^uint64(0) {
		return nil, ErrTargetCapacity
	}
	projectedTargets, ok := targetsWithin(state.connectorCount+1, enabledCount, manager.options.MaxTargetsPerTunnel)
	if !ok || projectedTargets-state.targets > manager.options.MaxTargetsGlobal-manager.targetsGlobal {
		return nil, ErrTargetCapacity
	}
	manager.targetsGlobal += projectedTargets - state.targets
	state.connectorCount++
	state.targets = projectedTargets
	manager.connectorRefs[key] = 1
	return &ConnectorLease{manager: manager, key: key, lifecycle: &leaseLifecycle{}}, nil
}

// Release 归还一个 Session 引用。旧 generation 的 Lease 只能归还自己的引用；
// 仍有 replacement 引用时不会删除 Owner Key 或释放 Target。
func (lease *ConnectorLease) Release() bool {
	if lease == nil || lease.manager == nil || lease.lifecycle == nil {
		return false
	}
	manager := lease.manager
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if lease.lifecycle.released {
		return false
	}
	references := manager.connectorRefs[lease.key]
	state := manager.tunnels[lease.key.TunnelID]
	if references == 0 || state == nil || state.connectorCount == 0 {
		panic("health target budget connector reference invariant violated")
	}
	if references > 1 {
		manager.connectorRefs[lease.key] = references - 1
	} else {
		delete(manager.connectorRefs, lease.key)
		enabledCount := state.enabledCount
		if state.reservation != nil {
			enabledCount = max(enabledCount, state.reservation.candidateCount)
		}
		state.connectorCount--
		remainingTargets := state.connectorCount * enabledCount
		manager.targetsGlobal -= state.targets - remainingTargets
		state.targets = remainingTargets
	}
	lease.lifecycle.released = true
	return true
}

// Snapshot 返回全部计数的深拷贝；调用方修改 Map 不会污染 Manager。
func (manager *Manager) Snapshot() Snapshot {
	if manager == nil {
		return Snapshot{}
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	snapshot := Snapshot{
		TargetsGlobal:       manager.targetsGlobal,
		Tunnels:             make(map[string]TunnelSnapshot, len(manager.tunnels)),
		ConnectorReferences: make(map[ConnectorKey]uint64, len(manager.connectorRefs)),
	}
	for tunnelID, state := range manager.tunnels {
		item := TunnelSnapshot{
			Revision: state.revision, EnabledCount: state.enabledCount,
			ConnectorCount: state.connectorCount, Targets: state.targets,
		}
		if state.reservation != nil {
			item.ReservationActive = true
			item.ReservationRevision = state.reservation.revision
			item.ReservationCandidateCount = state.reservation.candidateCount
		}
		snapshot.Tunnels[tunnelID] = item
	}
	for key, references := range manager.connectorRefs {
		snapshot.ConnectorReferences[key] = references
	}
	return snapshot
}

func targetsWithin(connectors, enabledCount, limit uint64) (uint64, bool) {
	if enabledCount != 0 && connectors > limit/enabledCount {
		return 0, false
	}
	return connectors * enabledCount, true
}
