package runtime

import "time"

// SessionStatusSnapshot 是 Session Manager 在 Current generation 所有权下组装的
// 联合只读快照。Lifecycle、Config/Health 与 WorkPool 均按完整 Session identity
// 关联；Services 和其中的值不再引用可变 owner 状态。
type SessionStatusSnapshot struct {
	Session
	ConnectorMetadata
	ConnectedAt           time.Time
	LastHeartbeatAt       time.Time
	CurrentControlSession bool
	HeartbeatFresh        bool
	LifecycleStatus       ConnectorStatus
	Config                SessionEligibility
	WorkPool              ConnectorWorkPoolSnapshot
}
