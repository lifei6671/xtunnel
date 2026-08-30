// Package metrics 将 Server 各运行态 Owner 的聚合快照与逻辑操作结果导出为
// Prometheus 指标。该包只持有进程生命周期 Counter/Histogram，不复制业务运行态。
package metrics

import (
	"errors"
	"fmt"
	"time"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/prometheus/client_golang/prometheus"
)

var errInvalidOptions = errors.New("invalid metrics options")

var (
	openDurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}
	reconcileBuckets    = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}
	protocolErrorCodes  = []protocolv1.ErrorCode{
		protocolv1.ErrorCode_ERROR_CODE_SERVICE_NOT_FOUND,
		protocolv1.ErrorCode_ERROR_CODE_SERVICE_DISABLED,
		protocolv1.ErrorCode_ERROR_CODE_TUNNEL_OFFLINE,
		protocolv1.ErrorCode_ERROR_CODE_NO_HEALTHY_CONNECTOR,
		protocolv1.ErrorCode_ERROR_CODE_SERVICE_CONFIG_NOT_OBSERVED,
		protocolv1.ErrorCode_ERROR_CODE_ORIGIN_REFUSED,
		protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TIMEOUT,
		protocolv1.ErrorCode_ERROR_CODE_ORIGIN_UNREACHABLE,
		protocolv1.ErrorCode_ERROR_CODE_ORIGIN_RESET,
		protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TLS_ERROR,
		protocolv1.ErrorCode_ERROR_CODE_WORK_POOL_EXHAUSTED,
		protocolv1.ErrorCode_ERROR_CODE_CONNECTOR_BUSY,
		protocolv1.ErrorCode_ERROR_CODE_OPEN_DRAINING,
		protocolv1.ErrorCode_ERROR_CODE_HEALTH_BUDGET_EXCEEDED,
		protocolv1.ErrorCode_ERROR_CODE_TOKEN_INVALID,
		protocolv1.ErrorCode_ERROR_CODE_TOKEN_REVOKED,
		protocolv1.ErrorCode_ERROR_CODE_TUNNEL_REVOKED,
		protocolv1.ErrorCode_ERROR_CODE_SESSION_INVALID,
		protocolv1.ErrorCode_ERROR_CODE_SESSION_RESOURCE_EXHAUSTED,
		protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR,
		protocolv1.ErrorCode_ERROR_CODE_VERSION_UNSUPPORTED,
		protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR,
	}
)

// OwnerSnapshot 是一次抓取所需的九项 Gauge 和两项 Owner Counter 绝对值。
// 调用方只组合对应 Runtime/Owner 的聚合快照，禁止暴露或复制高基数 Label Map。
type OwnerSnapshot struct {
	ConnectorsOnline                uint64
	ControlSessionsOnline           uint64
	ActiveConnections               uint64
	TCPIdleWorkConnections          uint64
	TCPActiveWorkConnections        uint64
	HealthTargets                   uint64
	HealthBudgetRejectionsTotal     uint64
	GatewayCertificateExpirySeconds float64
	RouteSnapshotBytes              uint64
	RouteSnapshotRoutes             uint64
	ReconcileCoalescedTotal         uint64
}

// OwnerSource 提供一次抓取所需的 Server Owner 聚合快照；每个字段必须来自对应
// Owner 自身线性化的纯值快照，不要求跨多个独立 Owner 建立全局事务。
type OwnerSource interface {
	MetricsOwnerSnapshot() OwnerSnapshot
}

// Registry 拥有进程私有 Prometheus Registry 及其 Recorder。
type Registry struct {
	registry *prometheus.Registry
	recorder *recorder
}

// NewRegistry 注册冻结的二十项 Server 指标。每次调用都创建全新的私有 Registry，
// 不使用 prometheus.DefaultRegisterer，避免测试和同进程多实例相互污染。
func NewRegistry(source OwnerSource) (*Registry, error) {
	if source == nil {
		return nil, errInvalidOptions
	}

	prometheusRegistry := prometheus.NewRegistry()
	recorder := newRecorder()
	collectors := []prometheus.Collector{
		newGaugeCollector(source),
		recorder.openTotal,
		recorder.openErrors,
		recorder.ingressBytes,
		recorder.egressBytes,
		recorder.originErrors,
		recorder.openDuration,
		recorder.originConnectDuration,
		recorder.reconcileDuration,
		recorder.reconcileErrors,
	}
	for _, collector := range collectors {
		if err := prometheusRegistry.Register(collector); err != nil {
			return nil, fmt.Errorf("register Server metric: %w", err)
		}
	}

	return &Registry{registry: prometheusRegistry, recorder: recorder}, nil
}

// ObserveOpen 让 Registry 直接满足 Tunnel 数据面的最小 Metrics 契约。
func (registry *Registry) ObserveOpen(duration time.Duration, code protocolv1.ErrorCode) {
	registry.recorder.observeOpen(duration, code)
}

// ObserveOriginError 累计最终 Origin 连接失败；成功码不会产生错误序列。
func (registry *Registry) ObserveOriginError(code protocolv1.ErrorCode) {
	registry.recorder.observeOriginError(code)
}

// ObserveOriginConnect 记录 Agent 返回的最终 Origin 连接时延。
func (registry *Registry) ObserveOriginConnect(duration time.Duration) {
	registry.recorder.originConnectDuration.Observe(duration.Seconds())
}

// ObserveReconcile 记录一次实际 TunnelSnapshot Reconcile 的最终时延和结果。
func (registry *Registry) ObserveReconcile(duration time.Duration, code protocolv1.ErrorCode) {
	registry.recorder.observeReconcile(duration, code)
}

// AddIngressBytes 累计本 Server 进程观察到的入口字节数。
func (registry *Registry) AddIngressBytes(bytes uint64) {
	registry.recorder.ingressBytes.Add(float64(bytes))
}

// AddEgressBytes 累计本 Server 进程观察到的出口字节数。
func (registry *Registry) AddEgressBytes(bytes uint64) {
	registry.recorder.egressBytes.Add(float64(bytes))
}

// recorder 持有进程生命周期 Counter 与 Histogram。调用方必须在最外层逻辑操作
// 完成时恰好调用一次，不能按内部 Failover 或 Wire attempt 重复记录。
type recorder struct {
	openTotal             prometheus.Counter
	openErrors            *prometheus.CounterVec
	ingressBytes          prometheus.Counter
	egressBytes           prometheus.Counter
	originErrors          *prometheus.CounterVec
	openDuration          prometheus.Histogram
	originConnectDuration prometheus.Histogram
	reconcileDuration     prometheus.Histogram
	reconcileErrors       *prometheus.CounterVec
}

func newRecorder() *recorder {
	recorder := &recorder{
		openTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "xtunnel_open_total",
			Help: "Total number of logical public OPEN operations.",
		}),
		openErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "xtunnel_open_errors_total",
			Help: "Total number of failed logical public OPEN operations by stable protocol error code.",
		}, []string{"error_code"}),
		ingressBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "xtunnel_ingress_bytes_total",
			Help: "Total ingress bytes observed during this Server process lifetime.",
		}),
		egressBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "xtunnel_egress_bytes_total",
			Help: "Total egress bytes observed during this Server process lifetime.",
		}),
		originErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "xtunnel_origin_errors_total",
			Help: "Total number of final origin connection failures by stable protocol error code.",
		}, []string{"error_code"}),
		openDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "xtunnel_open_duration_seconds",
			Help:    "Duration in seconds of logical public OPEN operations.",
			Buckets: openDurationBuckets,
		}),
		originConnectDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "xtunnel_origin_connect_duration_seconds",
			Help:    "Duration in seconds of final Agent origin connection attempts.",
			Buckets: openDurationBuckets,
		}),
		reconcileDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "xtunnel_reconcile_duration_seconds",
			Help:    "Duration in seconds of Server snapshot reconciliation operations.",
			Buckets: reconcileBuckets,
		}),
		reconcileErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "xtunnel_reconcile_errors_total",
			Help: "Total number of failed Server snapshot reconciliation operations by stable protocol error code.",
		}, []string{"error_code"}),
	}
	// 冷启动也物化完整的冻结错误枚举，使三个 Metric Family 始终存在，且时序
	// 基数严格受 Protocol v1 词汇表约束。
	for _, code := range protocolErrorCodes {
		label := code.String()
		recorder.openErrors.WithLabelValues(label)
		recorder.originErrors.WithLabelValues(label)
		recorder.reconcileErrors.WithLabelValues(label)
	}
	return recorder
}

func (recorder *recorder) observeOpen(duration time.Duration, code protocolv1.ErrorCode) {
	recorder.openTotal.Inc()
	recorder.openDuration.Observe(duration.Seconds())
	if code != protocolv1.ErrorCode_ERROR_CODE_OK {
		recorder.openErrors.WithLabelValues(normalizeErrorCode(code)).Inc()
	}
}

func (recorder *recorder) observeOriginError(code protocolv1.ErrorCode) {
	if code != protocolv1.ErrorCode_ERROR_CODE_OK {
		recorder.originErrors.WithLabelValues(normalizeErrorCode(code)).Inc()
	}
}

func (recorder *recorder) observeReconcile(duration time.Duration, code protocolv1.ErrorCode) {
	recorder.reconcileDuration.Observe(duration.Seconds())
	if code != protocolv1.ErrorCode_ERROR_CODE_OK {
		recorder.reconcileErrors.WithLabelValues(normalizeErrorCode(code)).Inc()
	}
}

func normalizeErrorCode(code protocolv1.ErrorCode) string {
	switch code {
	case protocolv1.ErrorCode_ERROR_CODE_SERVICE_NOT_FOUND,
		protocolv1.ErrorCode_ERROR_CODE_SERVICE_DISABLED,
		protocolv1.ErrorCode_ERROR_CODE_TUNNEL_OFFLINE,
		protocolv1.ErrorCode_ERROR_CODE_NO_HEALTHY_CONNECTOR,
		protocolv1.ErrorCode_ERROR_CODE_SERVICE_CONFIG_NOT_OBSERVED,
		protocolv1.ErrorCode_ERROR_CODE_ORIGIN_REFUSED,
		protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TIMEOUT,
		protocolv1.ErrorCode_ERROR_CODE_ORIGIN_UNREACHABLE,
		protocolv1.ErrorCode_ERROR_CODE_ORIGIN_RESET,
		protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TLS_ERROR,
		protocolv1.ErrorCode_ERROR_CODE_WORK_POOL_EXHAUSTED,
		protocolv1.ErrorCode_ERROR_CODE_CONNECTOR_BUSY,
		protocolv1.ErrorCode_ERROR_CODE_OPEN_DRAINING,
		protocolv1.ErrorCode_ERROR_CODE_HEALTH_BUDGET_EXCEEDED,
		protocolv1.ErrorCode_ERROR_CODE_TOKEN_INVALID,
		protocolv1.ErrorCode_ERROR_CODE_TOKEN_REVOKED,
		protocolv1.ErrorCode_ERROR_CODE_TUNNEL_REVOKED,
		protocolv1.ErrorCode_ERROR_CODE_SESSION_INVALID,
		protocolv1.ErrorCode_ERROR_CODE_SESSION_RESOURCE_EXHAUSTED,
		protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR,
		protocolv1.ErrorCode_ERROR_CODE_VERSION_UNSUPPORTED,
		protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR:
		return code.String()
	default:
		return protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR.String()
	}
}
