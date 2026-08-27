package connector

import (
	"errors"
	"math"
	"sort"
	"strings"
	"time"

	agenthealth "github.com/lifei6671/xtunnel/internal/agent/health"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"google.golang.org/protobuf/proto"
)

const (
	healthReportFlushInterval = time.Second
	healthReportBatchSize     = 128
)

var errHealthReportTooLarge = errors.New("agent health report exceeds control frame limit")

type healthStateSource interface {
	Snapshot() map[string]agenthealth.State
	Changed() <-chan struct{}
}

type serviceHealthSpec struct {
	revision uint64
	disabled bool
}

type healthReportValue struct {
	status      protocolv1.HealthStatus
	latencyMS   uint32
	errorCode   string
	checkedAtMS uint64
	revision    uint64
}

type healthFullReport struct {
	services map[string]serviceHealthSpec
	values   map[string]healthReportValue
	items    []*protocolv1.ServiceHealth
}

// configAckHealthSink 把 configruntime 生成的 APPLIED Ack 与预先计算的
// 完整 Health 集合交给 Session 一次原子提交。REJECTED Ack 仍走普通入队，
// 不会发布未应用 Snapshot 的 Health。
type configAckHealthSink struct {
	session   establishedSession
	reporter  *healthReporter
	snapshot  *protocolv1.TunnelSnapshot
	full      *healthFullReport
	committed bool
}

func (sink *configAckHealthSink) Enqueue(envelope *protocolv1.ControlEnvelope) error {
	ack := envelope.GetConfigAck()
	if sink.reporter != nil && ack != nil &&
		ack.GetApplyStatus() == protocolv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_APPLIED {
		full := sink.reporter.prepareFull(sink.snapshot)
		if err := sink.session.EnqueueConfigAckAndReplaceHealth(envelope, full.items); err != nil {
			return err
		}
		sink.full = full
		sink.committed = true
		return nil
	}
	return sink.session.Enqueue(envelope)
}

// healthReporter 属于单个 Control Session，由该 Session 的事件循环串行调用。
// Health Scheduler 只发布快照和合并通知；Reporter 管理 pending accumulator，真正的
// Batch generation 则由本代 Session Outbox 在 dequeue 冻结 Frame 时独占分配。
type healthReporter struct {
	source  healthStateSource
	session establishedSession

	configured bool
	services   map[string]serviceHealthSpec
	last       map[string]healthReportValue
	pending    map[string]healthReportValue
}

func newHealthReporter(source healthStateSource, session establishedSession) *healthReporter {
	if source == nil || session == nil {
		return nil
	}
	return &healthReporter{
		source: source, session: session,
		services: make(map[string]serviceHealthSpec), last: make(map[string]healthReportValue),
		pending: make(map[string]healthReportValue),
	}
}

func (reporter *healthReporter) changed() <-chan struct{} {
	if reporter == nil {
		return nil
	}
	return reporter.source.Changed()
}

// publishFull 只能在 ConfigAck(APPLIED) 已成功入队后调用。它以本次完整 Snapshot
// 定义 Service 集合；Health Owner 尚未发布状态时，启用检查的 Service fail closed 为
// UNKNOWN，Health Disabled 则按冻结契约报告 HEALTHY。
func (reporter *healthReporter) publishFull(snapshot *protocolv1.TunnelSnapshot) error {
	if reporter == nil || snapshot == nil {
		return nil
	}
	full := reporter.prepareFull(snapshot)
	if err := reporter.session.ReplaceHealth(full.items); err != nil {
		return err
	}
	reporter.commitFull(full)
	return nil
}

func (reporter *healthReporter) prepareFull(snapshot *protocolv1.TunnelSnapshot) *healthFullReport {
	if reporter == nil || snapshot == nil {
		return nil
	}
	services := make(map[string]serviceHealthSpec, len(snapshot.GetServices()))
	for _, service := range snapshot.GetServices() {
		services[service.GetServiceId()] = serviceHealthSpec{
			revision: service.GetRequiredRevision(),
			disabled: service.GetHealth().GetType() == protocolv1.HealthType_HEALTH_TYPE_DISABLED,
		}
	}

	states := reporter.source.Snapshot()
	values := make(map[string]healthReportValue, len(services))
	for serviceID, spec := range services {
		state, exists := states[serviceID]
		values[serviceID] = currentHealthValue(spec, state, exists)
	}
	serviceIDs := sortedHealthServiceIDs(values)
	items := make([]*protocolv1.ServiceHealth, 0, len(serviceIDs))
	for _, serviceID := range serviceIDs {
		items = append(items, healthEnvelopeItem(serviceID, values[serviceID]))
	}
	return &healthFullReport{services: services, values: values, items: items}
}

func (reporter *healthReporter) commitFull(full *healthFullReport) {
	if reporter == nil || full == nil {
		return
	}
	reporter.services = full.services
	reporter.configured = true
	reporter.last = full.values
	clear(reporter.pending)
}

// collectChanges 从最新权威快照重新计算当前 Service 状态。Changed 通知可以被合并，
// 但本方法从不依赖通知数量，因此 Scheduler 快速连续发布时仍不会丢失最终结果。
func (reporter *healthReporter) collectChanges() error {
	if reporter == nil || !reporter.configured {
		return nil
	}
	states := reporter.source.Snapshot()
	for serviceID, spec := range reporter.services {
		state, exists := states[serviceID]
		value := currentHealthValue(spec, state, exists)
		if previous, exists := reporter.last[serviceID]; exists && previous == value {
			continue
		}
		reporter.last[serviceID] = value
		reporter.pending[serviceID] = value
	}
	if len(reporter.pending) >= healthReportBatchSize {
		return reporter.flush()
	}
	return nil
}

func (reporter *healthReporter) flush() error {
	if reporter == nil || len(reporter.pending) == 0 {
		return nil
	}
	values := reporter.pending
	reporter.pending = make(map[string]healthReportValue)
	return reporter.enqueueValues(values)
}

func (reporter *healthReporter) enqueueValues(values map[string]healthReportValue) error {
	serviceIDs := sortedHealthServiceIDs(values)
	if len(serviceIDs) == 0 {
		return nil
	}

	for start := 0; start < len(serviceIDs); {
		end := min(start+healthReportBatchSize, len(serviceIDs))
		items := make([]*protocolv1.ServiceHealth, 0, end-start)
		for _, serviceID := range serviceIDs[start:end] {
			items = append(items, healthEnvelopeItem(serviceID, values[serviceID]))
		}
		// Reporter 交给 Outbox 的 generation 必须为零；真实序列只允许在 Outbox
		// dequeue 冻结 Frame 时分配。预检按 uint64 最长编码估算，避免正式分配后越界。
		for len(items) > 0 && healthBatchSize(math.MaxUint64, items) > int(frame.MaxControlFrameSize) {
			items = items[:len(items)-1]
			end--
		}
		if len(items) == 0 {
			return errHealthReportTooLarge
		}
		if err := reporter.enqueueBatch(items); err != nil {
			return err
		}
		start = end
	}
	return nil
}

func sortedHealthServiceIDs(values map[string]healthReportValue) []string {
	serviceIDs := make([]string, 0, len(values))
	for serviceID := range values {
		serviceIDs = append(serviceIDs, serviceID)
	}
	sort.Strings(serviceIDs)
	return serviceIDs
}

func (reporter *healthReporter) enqueueBatch(items []*protocolv1.ServiceHealth) error {
	envelope := healthBatchEnvelope(0, items)
	if healthBatchSize(math.MaxUint64, items) > int(frame.MaxControlFrameSize) {
		return errHealthReportTooLarge
	}
	return reporter.session.Enqueue(envelope)
}

func healthBatchSize(generation uint64, items []*protocolv1.ServiceHealth) int {
	return proto.Size(healthBatchEnvelope(generation, items))
}

func healthBatchEnvelope(generation uint64, items []*protocolv1.ServiceHealth) *protocolv1.ControlEnvelope {
	return &protocolv1.ControlEnvelope{
		ProtocolVersion: 1,
		Payload: &protocolv1.ControlEnvelope_ServiceHealthBatch{ServiceHealthBatch: &protocolv1.ServiceHealthBatch{
			Generation: generation,
			Items:      items,
		}},
	}
}

func currentHealthValue(spec serviceHealthSpec, state agenthealth.State, exists bool) healthReportValue {
	if exists && state.ServiceRevision == spec.revision {
		return healthValueFromState(state)
	}
	status := protocolv1.HealthStatus_HEALTH_STATUS_UNKNOWN
	if spec.disabled {
		status = protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY
	}
	return healthReportValue{status: status, revision: spec.revision}
}

func healthValueFromState(state agenthealth.State) healthReportValue {
	latencyMS := state.Latency.Milliseconds()
	if latencyMS < 0 {
		latencyMS = 0
	}
	if latencyMS > math.MaxUint32 {
		latencyMS = math.MaxUint32
	}
	checkedAtMS := state.CheckedAt.UnixMilli()
	if state.CheckedAt.IsZero() || checkedAtMS < 0 {
		checkedAtMS = 0
	}
	errorCode := ""
	if state.OriginErrorCode != protocolv1.ErrorCode_ERROR_CODE_OK {
		errorCode = strings.TrimPrefix(state.OriginErrorCode.String(), "ERROR_CODE_")
	}
	return healthReportValue{
		status: state.Status, latencyMS: uint32(latencyMS), errorCode: errorCode,
		checkedAtMS: uint64(checkedAtMS), revision: state.ServiceRevision,
	}
}

func healthEnvelopeItem(serviceID string, value healthReportValue) *protocolv1.ServiceHealth {
	return &protocolv1.ServiceHealth{
		ServiceId: serviceID, Status: value.status, LatencyMs: value.latencyMS,
		ErrorCode: value.errorCode, CheckedAtMs: value.checkedAtMS, ServiceRevision: value.revision,
	}
}
