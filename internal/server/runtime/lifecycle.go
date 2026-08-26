package runtime

import (
	"cmp"
	"slices"
	"time"

	"github.com/lifei6671/xtunnel/internal/identity"
)

const (
	// ConnectorEventConnected 表示一个新的临时 Connector 已发布为 Current。
	ConnectorEventConnected = "connector_connected"
	// ConnectorEventSessionReplaced 表示同一 Connector 已发布更高 generation。
	ConnectorEventSessionReplaced = "connector_session_replaced"
	// ConnectorEventDraining 表示 Current Connector 已从新连接选择集合摘除。
	ConnectorEventDraining = "connector_draining"
	// ConnectorEventDisconnected 表示 Current Control Session 已完成 fencing。
	ConnectorEventDisconnected = "connector_disconnected"
)

// ConnectorStatus 是 Connector 当前可查询的运行状态。断开后的 Tombstone 由
// ConnectorSnapshot.Tombstone 表达，不伪造永久 OFFLINE Connector 状态。
type ConnectorStatus string

const (
	ConnectorStatusOnline   ConnectorStatus = "ONLINE"
	ConnectorStatusDegraded ConnectorStatus = "DEGRADED"
	ConnectorStatusDraining ConnectorStatus = "DRAINING"
)

// ConnectorMetadata 是 Auth 已验证、但不包含任何 Credential 的进程元数据。
type ConnectorMetadata struct {
	Hostname string
	OS       string
	Arch     string
	Version  string
}

// ConnectorWorkPoolSnapshot 是 Manager 按精确 Session identity 叠加的 WorkPool 快照。
type ConnectorWorkPoolSnapshot struct {
	Connecting uint32
	Idle       uint32
	Opening    uint32
	Active     uint32
	Total      uint32
	Closed     bool
	Draining   bool
}

// ConnectorSnapshot 是当前在线或仍有 ActiveWork 的 Tombstone Connector 快照。
// 所有时间均来自 Server 本地时钟；快照不持久化，也不形成机器历史。
type ConnectorSnapshot struct {
	Session
	ConnectorMetadata
	ConnectedAt     time.Time
	LastHeartbeatAt time.Time
	Status          ConnectorStatus
	Tombstone       bool
	ActiveWork      uint64
	WorkPool        ConnectorWorkPoolSnapshot
}

// ConnectorLifecycleEvent 是锁内生成、锁外记录的不可变生命周期事件。
type ConnectorLifecycleEvent struct {
	Name     string
	Snapshot ConnectorSnapshot
	Reason   string
}

type connectorObservation struct {
	session         Session
	metadata        ConnectorMetadata
	connectedAt     time.Time
	lastHeartbeatAt time.Time
	status          ConnectorStatus
	tombstone       bool
}

// ObserveConnected 在完整 Session identity 仍为 Current 时发布 Connector 观测。
// 返回值为 false 表示该 Session 已被 replacement 或 revoke fencing。
func (registry *Registry) ObserveConnected(session Session, metadata ConnectorMetadata) (ConnectorLifecycleEvent, bool) {
	if registry == nil || !validLifecycleSession(session) {
		return ConnectorLifecycleEvent{}, false
	}
	runtime := registry.tunnel(session.TunnelID, false)
	if runtime == nil {
		return ConnectorLifecycleEvent{}, false
	}

	runtime.mu.Lock()
	if runtime.revoked || runtime.current[session.ConnectorID] != session {
		runtime.mu.Unlock()
		return ConnectorLifecycleEvent{}, false
	}
	now := runtime.now().UTC()
	previous, existed := runtime.connectors[session.ConnectorID]
	if existed && previous.session == session && !previous.tombstone {
		previous.metadata = metadata
		previous.lastHeartbeatAt = now
		runtime.connectors[session.ConnectorID] = previous
		runtime.mu.Unlock()
		return ConnectorLifecycleEvent{}, false
	}
	connectedAt := now
	if existed {
		connectedAt = previous.connectedAt
	}
	observation := connectorObservation{
		session: session, metadata: metadata, connectedAt: connectedAt,
		lastHeartbeatAt: now, status: ConnectorStatusOnline,
	}
	runtime.connectors[session.ConnectorID] = observation
	eventName := ConnectorEventConnected
	if existed {
		eventName = ConnectorEventSessionReplaced
	}
	snapshot := runtime.connectorSnapshotLocked(observation)
	runtime.mu.Unlock()
	return ConnectorLifecycleEvent{Name: eventName, Snapshot: snapshot}, true
}

// ObserveHeartbeat 按完整 generation 更新 Server receipt time；旧代 Heartbeat 被忽略。
func (registry *Registry) ObserveHeartbeat(session Session) bool {
	if registry == nil || !validLifecycleSession(session) {
		return false
	}
	runtime := registry.tunnel(session.TunnelID, false)
	if runtime == nil {
		return false
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	observation, exists := runtime.connectors[session.ConnectorID]
	if runtime.revoked || !exists || observation.session != session || observation.tombstone ||
		runtime.current[session.ConnectorID] != session {
		return false
	}
	observation.lastHeartbeatAt = runtime.now().UTC()
	runtime.connectors[session.ConnectorID] = observation
	return true
}

// ObserveDraining 按完整 generation 把 Current Connector 摘出新连接选择状态。
func (registry *Registry) ObserveDraining(session Session) (ConnectorLifecycleEvent, bool) {
	if registry == nil || !validLifecycleSession(session) {
		return ConnectorLifecycleEvent{}, false
	}
	runtime := registry.tunnel(session.TunnelID, false)
	if runtime == nil {
		return ConnectorLifecycleEvent{}, false
	}
	runtime.mu.Lock()
	observation, exists := runtime.connectors[session.ConnectorID]
	if runtime.revoked || !exists || observation.session != session || observation.tombstone ||
		runtime.current[session.ConnectorID] != session {
		runtime.mu.Unlock()
		return ConnectorLifecycleEvent{}, false
	}
	if observation.status == ConnectorStatusDraining {
		runtime.mu.Unlock()
		return ConnectorLifecycleEvent{}, false
	}
	observation.status = ConnectorStatusDraining
	runtime.connectors[session.ConnectorID] = observation
	snapshot := runtime.connectorSnapshotLocked(observation)
	runtime.mu.Unlock()
	return ConnectorLifecycleEvent{Name: ConnectorEventDraining, Snapshot: snapshot, Reason: "drain_requested"}, true
}

// ConnectorSnapshots 返回所有 Tunnel 当前在线或 Tombstone Connector 的确定性快照。
func (registry *Registry) ConnectorSnapshots() []ConnectorSnapshot {
	if registry == nil {
		return nil
	}
	runtimes := registry.runtimeSnapshot()
	snapshots := make([]ConnectorSnapshot, 0)
	for _, runtime := range runtimes {
		runtime.mu.Lock()
		for _, observation := range runtime.connectors {
			snapshots = append(snapshots, runtime.connectorSnapshotLocked(observation))
		}
		runtime.mu.Unlock()
	}
	slices.SortFunc(snapshots, func(left, right ConnectorSnapshot) int {
		if order := cmp.Compare(left.TunnelID, right.TunnelID); order != 0 {
			return order
		}
		return cmp.Compare(left.ConnectorID, right.ConnectorID)
	})
	return snapshots
}

func (runtime *TunnelRuntime) connectorSnapshotLocked(observation connectorObservation) ConnectorSnapshot {
	active := uint64(0)
	for _, work := range runtime.activeWorks {
		if work.identity.ConnectorID == observation.session.ConnectorID {
			active++
		}
	}
	return ConnectorSnapshot{
		Session: observation.session, ConnectorMetadata: observation.metadata,
		ConnectedAt: observation.connectedAt, LastHeartbeatAt: observation.lastHeartbeatAt,
		Status: observation.status, Tombstone: observation.tombstone, ActiveWork: active,
	}
}

func (runtime *TunnelRuntime) disconnectObservationLocked(session Session, reason string) ConnectorLifecycleEvent {
	observation, exists := runtime.connectors[session.ConnectorID]
	if !exists || observation.session != session {
		return ConnectorLifecycleEvent{}
	}
	snapshot := runtime.connectorSnapshotLocked(observation)
	if snapshot.ActiveWork > 0 {
		observation.status = ""
		observation.tombstone = true
		runtime.connectors[session.ConnectorID] = observation
		snapshot.Status = ""
		snapshot.Tombstone = true
	} else {
		delete(runtime.connectors, session.ConnectorID)
	}
	return ConnectorLifecycleEvent{Name: ConnectorEventDisconnected, Snapshot: snapshot, Reason: reason}
}

func (runtime *TunnelRuntime) removeFinishedTombstoneLocked(connectorID string) {
	observation, exists := runtime.connectors[connectorID]
	if !exists || !observation.tombstone {
		return
	}
	for _, work := range runtime.activeWorks {
		if work.identity.ConnectorID == connectorID {
			return
		}
	}
	delete(runtime.connectors, connectorID)
}

func validLifecycleSession(session Session) bool {
	return identity.ValidTunnelID(session.TunnelID) && identity.ValidConnectorID(session.ConnectorID) &&
		identity.ValidSessionID(session.SessionID) && session.Generation > 0
}
