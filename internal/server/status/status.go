// Package status 集中计算 Server 对外展示的 Tunnel、Connector 和 Service 状态。
// 本包只接受调用方在线性化点取得的值型快照，不读取运行时所有者、持久化或 Origin。
package status

import (
	"time"

	"github.com/lifei6671/xtunnel/internal/repository"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
)

// TunnelStatus 是 Tunnel 的 Server 权威展示状态。
type TunnelStatus string

const (
	TunnelStatusPending  TunnelStatus = "PENDING"
	TunnelStatusOnline   TunnelStatus = "ONLINE"
	TunnelStatusDegraded TunnelStatus = "DEGRADED"
	TunnelStatusOffline  TunnelStatus = "OFFLINE"
	TunnelStatusRevoked  TunnelStatus = "REVOKED"
)

// ConnectorStatus 是仍有 Current Control Session 的 Connector 展示状态。
type ConnectorStatus string

const (
	ConnectorStatusOnline   ConnectorStatus = "ONLINE"
	ConnectorStatusDegraded ConnectorStatus = "DEGRADED"
	ConnectorStatusDraining ConnectorStatus = "DRAINING"
)

// ServiceStatus 是 Service 的 Server 权威展示状态。
type ServiceStatus string

const (
	ServiceStatusDisabled        ServiceStatus = "DISABLED"
	ServiceStatusApplyFailed     ServiceStatus = "APPLY_FAILED"
	ServiceStatusTunnelOffline   ServiceStatus = "TUNNEL_OFFLINE"
	ServiceStatusConfigSyncing   ServiceStatus = "CONFIG_SYNCING"
	ServiceStatusOriginUnhealthy ServiceStatus = "ORIGIN_UNHEALTHY"
	ServiceStatusNoCapacity      ServiceStatus = "NO_CAPACITY"
	ServiceStatusReady           ServiceStatus = "READY"
)

// TunnelInput 是计算 Tunnel 状态所需的完整值型输入。
// ConnectorStatuses 只包含仍有 Current Control Session 的 Connector；断开的
// Connector 和仍有 ActiveWork 的 Tombstone 都不参与 Tunnel 可用性聚合。
type TunnelInput struct {
	Revoked bool
	// EverAuthenticated 必须来自跨 Server 重启保留的事实，不能从当前 Runtime
	// 是否为空反推，否则重启会把 OFFLINE 错报为 PENDING。
	EverAuthenticated bool
	ConnectorStatuses []ConnectorStatus
}

// TunnelInputFromRepository 把持久化 Tunnel 与当前 Connector 状态组装为唯一的
// Tunnel 状态输入。PENDING/OFFLINE 必须只读取 FirstAuthenticatedAt，不能由当前
// Runtime 是否为空猜测；Revoked tombstone 同样来自持久化聚合。
func TunnelInputFromRepository(tunnel repository.Tunnel, connectorStatuses []ConnectorStatus) TunnelInput {
	return TunnelInput{
		Revoked:           tunnel.RevokedAt != nil,
		EverAuthenticated: tunnel.FirstAuthenticatedAt != nil,
		ConnectorStatuses: connectorStatuses,
	}
}

// CalculateTunnel 按 REVOKED、PENDING、OFFLINE、ONLINE、DEGRADED 的规则计算状态。
// 这里故意不接收 Service 或 Origin Health，防止单个 Origin 故障污染 Tunnel 状态。
func CalculateTunnel(input TunnelInput) TunnelStatus {
	if input.Revoked {
		return TunnelStatusRevoked
	}
	for _, connectorStatus := range input.ConnectorStatuses {
		if connectorStatus == ConnectorStatusOnline {
			return TunnelStatusOnline
		}
	}
	if len(input.ConnectorStatuses) > 0 {
		return TunnelStatusDegraded
	}
	if input.EverAuthenticated {
		return TunnelStatusOffline
	}
	return TunnelStatusPending
}

// ConnectorInput 是计算当前 Connector 状态所需的完整值型输入。
type ConnectorInput struct {
	CurrentControlSession bool
	HeartbeatFresh        bool
	ConfigReady           bool
	Draining              bool
	TransportAcceptsWork  bool
}

// CalculateConnector 计算当前 Connector 状态。第二个返回值表示该 Connector
// 是否仍有可展示状态；Control Session 关闭或 Heartbeat 过期时返回 false，调用方应
// 删除运行态或按 ActiveWork 规则保留无状态 Tombstone，而不是伪造 OFFLINE 状态。
func CalculateConnector(input ConnectorInput) (ConnectorStatus, bool) {
	if !input.CurrentControlSession || !input.HeartbeatFresh {
		return "", false
	}
	if input.Draining {
		return ConnectorStatusDraining, true
	}
	if input.ConfigReady && input.TransportAcceptsWork {
		return ConnectorStatusOnline, true
	}
	return ConnectorStatusDegraded, true
}

// ConnectorInputFromRuntime 把 Session Manager 的联合快照转换为 Connector
// 状态输入。Origin Health 不在转换路径中，不能污染 Connector 状态。
func ConnectorInputFromRuntime(snapshot serverruntime.SessionStatusSnapshot) ConnectorInput {
	return ConnectorInput{
		CurrentControlSession: snapshot.CurrentControlSession,
		HeartbeatFresh:        snapshot.HeartbeatFresh,
		ConfigReady:           snapshot.Config.ConfigReady,
		Draining: snapshot.LifecycleStatus == serverruntime.ConnectorStatusDraining ||
			snapshot.WorkPool.Draining,
		TransportAcceptsWork: !snapshot.WorkPool.Closed && !snapshot.WorkPool.Draining,
	}
}

// ServiceConnector 是同一个 Current Connector 对单个 Service 的值型运行快照。
// Status 模块必须沿着同一元素依次检查 Revision、Health 和 Capacity，禁止把不同
// Connector 的局部门禁拼成一个并不存在的 READY 候选。
type ServiceConnector struct {
	Current              bool
	Tombstone            bool
	ControlLive          bool
	HeartbeatFresh       bool
	ConfigReady          bool
	HasObserved          bool
	ObservedRevision     uint64
	HealthRevision       uint64
	HealthHealthy        bool
	HealthFresh          bool
	Draining             bool
	TransportAcceptsWork bool
	CapacityAvailable    bool
}

// ApplyFailure 是当前 Service Runtime 副作用失败的展示详情。RequiredRevision
// 用于淘汰已经被更高 Desired State 取代的旧失败。
type ApplyFailure struct {
	RequiredRevision uint64
	ErrorCode        string
	FailedAt         time.Time
}

// ServiceInput 是计算 Service 状态所需的完整值型输入。Connectors 只包含仍有
// Current Control Session 的 Connector；Tombstone 和已断开的 Session 不得传入。
type ServiceInput struct {
	Enabled          bool
	RequiredRevision uint64
	HealthEnabled    bool
	ApplyFailure     *ApplyFailure
	Connectors       []ServiceConnector
}

// ServiceConnectorFromRuntime 从一个完整 Session 快照构造单 Service 输入。
// Revision、Health 与 Capacity 始终来自同一个 Connector generation，调用方不能
// 把不同 Connector 的局部事实合并为一个候选。
func ServiceConnectorFromRuntime(
	snapshot serverruntime.SessionStatusSnapshot,
	serviceID string,
	requiredRevision uint64,
	now time.Time,
) ServiceConnector {
	connector := ServiceConnector{
		Current: snapshot.CurrentControlSession, ControlLive: snapshot.CurrentControlSession,
		HeartbeatFresh: snapshot.HeartbeatFresh, ConfigReady: snapshot.Config.ConfigReady,
		Draining: snapshot.LifecycleStatus == serverruntime.ConnectorStatusDraining ||
			snapshot.WorkPool.Draining,
		TransportAcceptsWork: !snapshot.WorkPool.Closed && !snapshot.WorkPool.Draining,
		CapacityAvailable:    snapshot.WorkPool.Idle > 0,
	}
	service, exists := snapshot.Config.Services[serviceID]
	if !exists || service.RequiredRevision != requiredRevision {
		return connector
	}
	connector.HasObserved = snapshot.Config.HasObserved
	connector.ObservedRevision = snapshot.Config.ObservedRevision
	connector.HealthRevision = service.HealthRevision
	connector.HealthHealthy = service.HealthHealthy
	connector.HealthFresh = service.HealthDisabled ||
		(!service.HealthyUntil.IsZero() && !now.After(service.HealthyUntil))
	return connector
}

// CalculateService 按冻结优先级计算 Service 状态。Revision 门禁先于 Health，
// 使旧 Revision 的 Health 不能把 CONFIG_SYNCING 错报为 ORIGIN_UNHEALTHY。
func CalculateService(input ServiceInput) ServiceStatus {
	switch {
	case !input.Enabled:
		return ServiceStatusDisabled
	case input.ApplyFailure != nil && input.ApplyFailure.RequiredRevision == input.RequiredRevision:
		return ServiceStatusApplyFailed
	}

	hasConnectedConnector := false
	hasObservedRequiredRevision := false
	hasHealthyRevisionConnector := false
	for _, connector := range input.Connectors {
		if !connector.Current || connector.Tombstone || !connector.ControlLive || !connector.HeartbeatFresh {
			continue
		}
		hasConnectedConnector = true
		if !connector.ConfigReady || !connector.HasObserved ||
			connector.ObservedRevision < input.RequiredRevision {
			continue
		}
		hasObservedRequiredRevision = true
		if input.HealthEnabled &&
			(connector.HealthRevision != input.RequiredRevision || !connector.HealthHealthy || !connector.HealthFresh) {
			continue
		}
		hasHealthyRevisionConnector = true
		if !connector.Draining && connector.TransportAcceptsWork && connector.CapacityAvailable {
			return ServiceStatusReady
		}
	}

	switch {
	case !hasConnectedConnector:
		return ServiceStatusTunnelOffline
	case !hasObservedRequiredRevision:
		return ServiceStatusConfigSyncing
	case input.HealthEnabled && !hasHealthyRevisionConnector:
		return ServiceStatusOriginUnhealthy
	default:
		return ServiceStatusNoCapacity
	}
}
