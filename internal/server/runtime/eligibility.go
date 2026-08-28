package runtime

import (
	"time"

	"github.com/lifei6671/xtunnel/internal/identity"
)

// ServiceEligibility 是 Session Manager 发布给 Tunnel Runtime 的单 Service
// 不可变门禁值。HealthHealthy 只表示最近状态为 HEALTHY；最终放行仍需同时满足
// Revision、Enabled、Observed 与本机单调过期时间。
type ServiceEligibility struct {
	RequiredRevision uint64
	HealthRevision   uint64
	Enabled          bool
	HealthDisabled   bool
	HealthHealthy    bool
	HealthyUntil     time.Time
}

// SessionEligibility 是一个完整 Session generation 当前配置与 Health 的值型快照。
// Services Map 在发布时会被复制，调用方后续修改不会污染 Runtime。
type SessionEligibility struct {
	ConfigReady      bool
	HasObserved      bool
	ObservedRevision uint64
	Services         map[string]ServiceEligibility
}

// EligibilityWatch 是 Pending OPEN 对一个已选 Session 的无泄漏等待门闩。
// Changed 会在 Current、Revision 或 Health 快照变化时关闭；健康检查启用时，
// HasExpiry/ExpiresAfter 还要求等待者在 Stale TTL 到达后主动复核。
type EligibilityWatch struct {
	Changed      <-chan struct{}
	ExpiresAfter time.Duration
	HasExpiry    bool
}

// serviceRevisionConstraint 显式区分“接受 Session 当前 Service Revision”与
// “精确匹配 Route Revision”。Revision 0 是合法值，不能承担模式哨兵职责。
type serviceRevisionConstraint struct {
	exact    bool
	revision uint64
}

var closedEligibilitySignal = func() <-chan struct{} {
	closed := make(chan struct{})
	close(closed)
	return closed
}()

// PublishEligibility 以完整 Session identity 发布最新门禁快照。旧 generation 或
// 已被摘除的 Session 只能返回 false，不能覆盖 Current 的选择状态。
func (registry *Registry) PublishEligibility(session Session, state SessionEligibility) bool {
	if registry == nil || !validEligibilitySession(session) {
		return false
	}
	owned, valid := cloneEligibility(state)
	if !valid {
		return false
	}
	runtime := registry.tunnel(session.TunnelID, false)
	if runtime == nil {
		return false
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.revoked || runtime.current[session.ConnectorID] != session {
		return false
	}
	if previous, exists := runtime.eligibility[session]; exists && equalEligibility(previous, owned) {
		return true
	}
	runtime.eligibility[session] = owned
	runtime.signalEligibilityLocked()
	return true
}

// Eligible 在 TunnelRuntime.mu 下按 Session 当前 Service Revision、Health 与
// Stale TTL 一次性裁决，不获取 Session Manager 或 configMu。
func (registry *Registry) Eligible(session Session, serviceID string) bool {
	return registry.eligible(session, serviceID, serviceRevisionConstraint{})
}

// EligibleAtRevision 在 Eligible 的基础上要求 Service 精确匹配调用方持有的
// Route Revision；Revision 0 也执行精确匹配。
func (registry *Registry) EligibleAtRevision(session Session, serviceID string, requiredRevision uint64) bool {
	return registry.eligible(session, serviceID, serviceRevisionConstraint{
		exact: true, revision: requiredRevision,
	})
}

func (registry *Registry) eligible(
	session Session,
	serviceID string,
	revision serviceRevisionConstraint,
) bool {
	if registry == nil || !validEligibilitySession(session) || identity.ValidateServiceID(serviceID) != nil {
		return false
	}
	runtime := registry.tunnel(session.TunnelID, false)
	if runtime == nil {
		return false
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.sessionEligibleLocked(session, serviceID, revision, runtime.now())
}

// ServiceConfigObserved 判断当前 Tunnel 是否至少有一个 Current Connector 已完整
// 应用指定 Service Revision。它只回答配置可见性，不要求 Health Healthy 或 Work
// Capacity，因此调用方可把“配置尚未观察”与普通暂时不可用区分开。
func (registry *Registry) ServiceConfigObserved(tunnelID, serviceID string, requiredRevision uint64) bool {
	if registry == nil || identity.ValidateTunnelID(tunnelID) != nil || identity.ValidateServiceID(serviceID) != nil {
		return false
	}
	runtime := registry.tunnel(tunnelID, false)
	if runtime == nil {
		return false
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.revoked {
		return false
	}
	for _, session := range runtime.current {
		state, exists := runtime.eligibility[session]
		if !exists || !state.ConfigReady || !state.HasObserved || state.ObservedRevision < requiredRevision {
			continue
		}
		service, exists := state.Services[serviceID]
		if exists && service.RequiredRevision == requiredRevision {
			return true
		}
	}
	return false
}

// WatchEligibility 返回当前 Eligible Session 的变更广播与健康过期剩余时间。
// 返回 false 时 Changed 已关闭，调用方应立即释放旧选择并重新进入 Tunnel 级选择。
func (registry *Registry) WatchEligibility(
	session Session,
	serviceID string,
) (EligibilityWatch, bool) {
	return registry.watchEligibility(session, serviceID, serviceRevisionConstraint{})
}

// WatchEligibilityAtRevision 返回精确匹配 Route Revision 的 Eligibility Watch；
// Revision 0 也不会退化成当前版本查询。
func (registry *Registry) WatchEligibilityAtRevision(
	session Session,
	serviceID string,
	requiredRevision uint64,
) (EligibilityWatch, bool) {
	return registry.watchEligibility(session, serviceID, serviceRevisionConstraint{
		exact: true, revision: requiredRevision,
	})
}

func (registry *Registry) watchEligibility(
	session Session,
	serviceID string,
	revision serviceRevisionConstraint,
) (EligibilityWatch, bool) {
	if registry == nil || !validEligibilitySession(session) || identity.ValidateServiceID(serviceID) != nil {
		return EligibilityWatch{Changed: closedEligibilitySignal}, false
	}
	runtime := registry.tunnel(session.TunnelID, false)
	if runtime == nil {
		return EligibilityWatch{Changed: closedEligibilitySignal}, false
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	now := runtime.now()
	if !runtime.sessionEligibleLocked(session, serviceID, revision, now) {
		return EligibilityWatch{Changed: closedEligibilitySignal}, false
	}
	watch := EligibilityWatch{Changed: runtime.eligibilityChanged}
	service := runtime.eligibility[session].Services[serviceID]
	if !service.HealthDisabled {
		watch.HasExpiry = true
		watch.ExpiresAfter = service.HealthyUntil.Sub(now)
		if watch.ExpiresAfter < 0 {
			watch.ExpiresAfter = 0
		}
	}
	return watch, true
}

func (runtime *TunnelRuntime) sessionEligibleLocked(
	session Session,
	serviceID string,
	revision serviceRevisionConstraint,
	now time.Time,
) bool {
	if runtime.revoked || runtime.current[session.ConnectorID] != session {
		return false
	}
	observation, online := runtime.connectors[session.ConnectorID]
	if !online || observation.session != session || observation.tombstone || observation.status != ConnectorStatusOnline {
		return false
	}
	state, exists := runtime.eligibility[session]
	if !exists || !state.ConfigReady || !state.HasObserved {
		return false
	}
	service, exists := state.Services[serviceID]
	if !exists || !service.Enabled || state.ObservedRevision < service.RequiredRevision ||
		(revision.exact && service.RequiredRevision != revision.revision) {
		return false
	}
	if service.HealthDisabled {
		return true
	}
	return service.HealthHealthy && service.HealthRevision == service.RequiredRevision &&
		!service.HealthyUntil.IsZero() && !now.After(service.HealthyUntil)
}

func (runtime *TunnelRuntime) signalEligibilityLocked() {
	if runtime.eligibilityChanged != nil {
		close(runtime.eligibilityChanged)
	}
	runtime.eligibilityChanged = make(chan struct{})
}

func (runtime *TunnelRuntime) discardEligibilityLocked(session Session) {
	if _, exists := runtime.eligibility[session]; !exists {
		return
	}
	delete(runtime.eligibility, session)
	runtime.signalEligibilityLocked()
}

func cloneEligibility(state SessionEligibility) (SessionEligibility, bool) {
	owned := state
	owned.Services = make(map[string]ServiceEligibility, len(state.Services))
	for serviceID, service := range state.Services {
		if identity.ValidateServiceID(serviceID) != nil {
			return SessionEligibility{}, false
		}
		owned.Services[serviceID] = service
	}
	return owned, true
}

func equalEligibility(left, right SessionEligibility) bool {
	if left.ConfigReady != right.ConfigReady || left.HasObserved != right.HasObserved ||
		left.ObservedRevision != right.ObservedRevision || len(left.Services) != len(right.Services) {
		return false
	}
	for serviceID, service := range left.Services {
		other, exists := right.Services[serviceID]
		if !exists || other != service {
			return false
		}
	}
	return true
}

func validEligibilitySession(session Session) bool {
	return identity.ValidateTunnelID(session.TunnelID) == nil &&
		identity.ValidateConnectorID(session.ConnectorID) == nil &&
		identity.ValidateSessionID(session.SessionID) == nil && session.Generation > 0
}
