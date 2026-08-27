package sessionruntime

import (
	"errors"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/lifei6671/xtunnel/internal/controlsession"
	"github.com/lifei6671/xtunnel/internal/identity"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
)

var errInvalidHealthBatch = errors.New("invalid ServiceHealthBatch")

// maxAcceptedHealthBatchItems 镜像 V0.1 冻结的 report_batch_size。Server 必须在
// 按对端声明的 items 数分配校验 Map 前先拒绝超限 Frame。
const maxAcceptedHealthBatchItems = 128

// serviceRequirement 是 Server 当前 Desired Snapshot 对单个 Service 的运行门禁。
// health 使用自有深拷贝，避免 Snapshot 后续所有权变化污染已发布要求。
type serviceRequirement struct {
	requiredRevision uint64
	enabled          bool
	health           *protocolv1.HealthCheckConfig
}

// serviceHealthState 保存 Agent wall clock 仅供展示；新鲜度只使用 Server 进程内
// 单调 receivedAt，禁止跨机器比较 checkedAtMS。
type serviceHealthState struct {
	status          protocolv1.HealthStatus
	latencyMS       uint32
	errorCode       string
	checkedAtMS     uint64
	serviceRevision uint64
	receivedAt      time.Duration
}

// handleHealthBatch 校验并接收当前 generation 的健康批次，再把值型 Eligibility 发布
// 给 Runtime。重复 Frame 和已被替换的 Session 都不能重新产生副作用。
func (manager *Manager) handleHealthBatch(managed *managedSession, inbound controlsession.Inbound) error {
	if inbound.Duplicate {
		return nil
	}
	batch := inbound.Envelope.GetServiceHealthBatch()
	if err := validateHealthBatch(batch); err != nil {
		return err
	}
	if manager.registry == nil {
		return ErrSessionUnavailable
	}
	current, exists := manager.registry.Current(managed.session.TunnelID, managed.session.ConnectorID)
	if !exists || current != managed.session {
		return nil
	}

	managed.acceptHealthBatch(batch, time.Since(manager.startedAt))
	// managed 只保留协议接收侧的 generation/字段镜像；真正参与选择的值型快照
	// 在 configMu 释放后发布给 TunnelRuntime，并由完整 Session identity 再次 fencing。
	manager.publishEligibility(managed)
	return nil
}

// validateHealthBatch 在建立去重 Map 和写入 Session 状态前限制批量大小、Service ID、
// 重复项与枚举值，避免不受控分配或未知状态进入选择门禁。
func validateHealthBatch(batch *protocolv1.ServiceHealthBatch) error {
	if batch == nil || batch.GetGeneration() == 0 || len(batch.GetItems()) == 0 ||
		len(batch.GetItems()) > maxAcceptedHealthBatchItems {
		return errInvalidHealthBatch
	}
	seen := make(map[string]struct{}, len(batch.GetItems()))
	for _, item := range batch.GetItems() {
		if item == nil || identity.ValidateServiceID(item.GetServiceId()) != nil {
			return errInvalidHealthBatch
		}
		if _, duplicate := seen[item.GetServiceId()]; duplicate {
			return errInvalidHealthBatch
		}
		seen[item.GetServiceId()] = struct{}{}
		switch item.GetStatus() {
		case protocolv1.HealthStatus_HEALTH_STATUS_UNKNOWN,
			protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY,
			protocolv1.HealthStatus_HEALTH_STATUS_UNHEALTHY:
		default:
			return errInvalidHealthBatch
		}
	}
	return nil
}

// acceptHealthBatch 在 configMu 下先完成 generation/config-ready 门禁，再一次性
// 提交所有仍匹配当前 Service Revision 的项。未知或旧 Revision 项只被丢弃；整批
// generation 仍会推进，避免它们在未来配置变化后重新生效。
func (managed *managedSession) acceptHealthBatch(batch *protocolv1.ServiceHealthBatch, receivedAt time.Duration) {
	managed.configMu.Lock()
	defer managed.configMu.Unlock()
	if !managed.configReady || batch.GetGeneration() <= managed.healthGeneration {
		return
	}
	managed.healthGeneration = batch.GetGeneration()
	if managed.serviceHealth == nil {
		managed.serviceHealth = make(map[string]serviceHealthState)
	}
	for _, item := range batch.GetItems() {
		requirement, exists := managed.serviceRequirements[item.GetServiceId()]
		if !exists || item.GetServiceRevision() != requirement.requiredRevision {
			continue
		}
		managed.serviceHealth[item.GetServiceId()] = serviceHealthState{
			status: item.GetStatus(), latencyMS: item.GetLatencyMs(), errorCode: item.GetErrorCode(),
			checkedAtMS: item.GetCheckedAtMs(), serviceRevision: item.GetServiceRevision(), receivedAt: receivedAt,
		}
	}
}

// installServiceRequirementsLocked 发布最新 Desired Snapshot 的服务级门禁。只有
// required_revision、enabled 或 Health Policy 改变的 Service 才清除旧 Health；
// 未变化 Service 的当前 Revision Health 跨 Tunnel Snapshot 更新继续保留。
func (managed *managedSession) installServiceRequirementsLocked(snapshot *protocolv1.TunnelSnapshot) {
	next := make(map[string]serviceRequirement, len(snapshot.GetServices()))
	for _, service := range snapshot.GetServices() {
		var health *protocolv1.HealthCheckConfig
		if service.GetHealth() != nil {
			health = proto.Clone(service.GetHealth()).(*protocolv1.HealthCheckConfig)
		}
		requirement := serviceRequirement{
			requiredRevision: service.GetRequiredRevision(), enabled: service.GetEnabled(), health: health,
		}
		previous, exists := managed.serviceRequirements[service.GetServiceId()]
		if !exists || previous.requiredRevision != requirement.requiredRevision ||
			previous.enabled != requirement.enabled || !proto.Equal(previous.health, requirement.health) {
			delete(managed.serviceHealth, service.GetServiceId())
		}
		next[service.GetServiceId()] = requirement
	}
	for serviceID := range managed.serviceRequirements {
		if _, exists := next[serviceID]; !exists {
			delete(managed.serviceHealth, serviceID)
		}
	}
	managed.serviceRequirements = next
}

// publishEligibility 先从协议侧 owner 复制不可变快照，再由 Runtime 按完整 Session
// identity 做最终 fencing；返回 false 表示该 generation 已不再 Current。
func (manager *Manager) publishEligibility(managed *managedSession) bool {
	if manager == nil || manager.registry == nil || managed == nil {
		return false
	}
	return manager.registry.PublishEligibility(managed.session, managed.eligibilitySnapshot(manager.startedAt))
}

// eligibilitySnapshot 在 configMu 下复制当前配置、Health 与新鲜度截止时间。
func (managed *managedSession) eligibilitySnapshot(startedAt time.Time) serverruntime.SessionEligibility {
	managed.configMu.Lock()
	defer managed.configMu.Unlock()
	return managed.eligibilitySnapshotLocked(startedAt)
}

// eligibilitySnapshotLocked 要求调用方持有 configMu，并为每个 Service 构造独立值，
// 避免 Runtime 保存对 managedSession 可变 Map 或 protobuf 的引用。
func (managed *managedSession) eligibilitySnapshotLocked(startedAt time.Time) serverruntime.SessionEligibility {
	state := serverruntime.SessionEligibility{
		ConfigReady: managed.configReady, HasObserved: managed.hasObserved,
		ObservedRevision: managed.observedRevision,
		Services:         make(map[string]serverruntime.ServiceEligibility, len(managed.serviceRequirements)),
	}
	for serviceID, requirement := range managed.serviceRequirements {
		service := serverruntime.ServiceEligibility{
			RequiredRevision: requirement.requiredRevision,
			Enabled:          requirement.enabled,
			HealthDisabled: requirement.health != nil &&
				requirement.health.GetType() == protocolv1.HealthType_HEALTH_TYPE_DISABLED,
		}
		if health, exists := managed.serviceHealth[serviceID]; exists {
			service.HealthRevision = health.serviceRevision
			service.HealthHealthy = health.status == protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY
			if requirement.health != nil && requirement.health.GetIntervalMs() > 0 {
				staleAfter := 2 * time.Duration(requirement.health.GetIntervalMs()) * time.Millisecond
				service.HealthyUntil = startedAt.Add(health.receivedAt).Add(staleAfter)
			}
		}
		state.Services[serviceID] = service
	}
	return state
}

// heartbeatFresh 使用本地单调 receipt time 判断 Control Session 是否仍新鲜。
// Config/Health 状态不能从本对象读取给 Status；它们必须来自 TunnelRuntime 已发布
// 的 Eligibility，避免展示先于数据面裁决。
func (managed *managedSession) heartbeatFresh(
	now time.Duration,
	heartbeatTimeout time.Duration,
) bool {
	managed.configMu.Lock()
	defer managed.configMu.Unlock()
	return now >= managed.lastHeartbeatAt && now-managed.lastHeartbeatAt <= heartbeatTimeout
}

// observeHeartbeat 记录 Server 进程内单调接收时刻，不信任 Agent wall clock。
func (managed *managedSession) observeHeartbeat(receivedAt time.Duration) {
	managed.configMu.Lock()
	managed.lastHeartbeatAt = receivedAt
	managed.configMu.Unlock()
}
