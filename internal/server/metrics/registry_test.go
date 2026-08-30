package metrics

import (
	"errors"
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	dto "github.com/prometheus/client_model/go"
)

var expectedMetricTypes = map[string]dto.MetricType{
	"xtunnel_connectors_online":                  dto.MetricType_GAUGE,
	"xtunnel_control_sessions_online":            dto.MetricType_GAUGE,
	"xtunnel_active_connections":                 dto.MetricType_GAUGE,
	"xtunnel_tcp_idle_work_connections":          dto.MetricType_GAUGE,
	"xtunnel_tcp_active_work_connections":        dto.MetricType_GAUGE,
	"xtunnel_open_total":                         dto.MetricType_COUNTER,
	"xtunnel_open_errors_total":                  dto.MetricType_COUNTER,
	"xtunnel_ingress_bytes_total":                dto.MetricType_COUNTER,
	"xtunnel_egress_bytes_total":                 dto.MetricType_COUNTER,
	"xtunnel_origin_errors_total":                dto.MetricType_COUNTER,
	"xtunnel_health_targets":                     dto.MetricType_GAUGE,
	"xtunnel_health_budget_rejections_total":     dto.MetricType_COUNTER,
	"xtunnel_gateway_certificate_expiry_seconds": dto.MetricType_GAUGE,
	"xtunnel_open_duration_seconds":              dto.MetricType_HISTOGRAM,
	"xtunnel_origin_connect_duration_seconds":    dto.MetricType_HISTOGRAM,
	"xtunnel_reconcile_duration_seconds":         dto.MetricType_HISTOGRAM,
	"xtunnel_reconcile_errors_total":             dto.MetricType_COUNTER,
	"xtunnel_route_snapshot_bytes":               dto.MetricType_GAUGE,
	"xtunnel_route_snapshot_routes":              dto.MetricType_GAUGE,
	"xtunnel_reconcile_coalesced_total":          dto.MetricType_COUNTER,
}

type staticOwnerSource struct {
	snapshot OwnerSnapshot
}

func (source staticOwnerSource) MetricsOwnerSnapshot() OwnerSnapshot {
	return source.snapshot
}

func TestRegistryContractAndCardinality(t *testing.T) {
	source := staticOwnerSource{snapshot: OwnerSnapshot{
		ConnectorsOnline: 1, ControlSessionsOnline: 2, ActiveConnections: 3,
		TCPIdleWorkConnections: 4, TCPActiveWorkConnections: 5,
		HealthTargets: 6, HealthBudgetRejectionsTotal: 7,
		GatewayCertificateExpirySeconds: 1_800_000_000,
		RouteSnapshotBytes:              8, RouteSnapshotRoutes: 9, ReconcileCoalescedTotal: 10,
	}}
	registry, err := NewRegistry(source)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	registry.ObserveOpen(25*time.Millisecond, protocolv1.ErrorCode_ERROR_CODE_OK)
	registry.ObserveOpen(50*time.Millisecond, protocolv1.ErrorCode(0x7fffffff))
	registry.ObserveOriginConnect(100 * time.Millisecond)
	registry.ObserveOriginError(protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TIMEOUT)
	registry.ObserveReconcile(5*time.Millisecond, protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR)
	registry.AddIngressBytes(11)
	registry.AddEgressBytes(12)

	families, err := registry.registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	if len(families) != len(expectedMetricTypes) {
		t.Fatalf("Gather() families = %d, want %d", len(families), len(expectedMetricTypes))
	}

	for _, family := range families {
		name := family.GetName()
		wantType, ok := expectedMetricTypes[name]
		if !ok {
			t.Errorf("unexpected metric family %q", name)
			continue
		}
		if family.GetType() != wantType {
			t.Errorf("%s type = %s, want %s", name, family.GetType(), wantType)
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() != "error_code" {
					t.Errorf("%s has forbidden label %q", name, label.GetName())
				}
			}
			if name != "xtunnel_open_errors_total" && name != "xtunnel_origin_errors_total" && name != "xtunnel_reconcile_errors_total" && len(metric.GetLabel()) != 0 {
				t.Errorf("%s labels = %v, want none", name, metric.GetLabel())
			}
		}
		if name == "xtunnel_open_errors_total" || name == "xtunnel_origin_errors_total" || name == "xtunnel_reconcile_errors_total" {
			assertFiniteErrorCodeSeries(t, family)
		}
	}

	assertCounterValue(t, families, "xtunnel_open_total", "", 2)
	assertCounterValue(t, families, "xtunnel_open_errors_total", protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR.String(), 1)
	assertCounterValue(t, families, "xtunnel_origin_errors_total", protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TIMEOUT.String(), 1)
	assertCounterValue(t, families, "xtunnel_reconcile_errors_total", protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR.String(), 1)
	assertCounterValue(t, families, "xtunnel_health_budget_rejections_total", "", 7)
	assertCounterValue(t, families, "xtunnel_reconcile_coalesced_total", "", 10)
	assertGaugeValue(t, families, "xtunnel_gateway_certificate_expiry_seconds", 1_800_000_000)
	assertHistogramBuckets(t, families, "xtunnel_open_duration_seconds", openDurationBuckets)
	assertHistogramBuckets(t, families, "xtunnel_origin_connect_duration_seconds", openDurationBuckets)
	assertHistogramBuckets(t, families, "xtunnel_reconcile_duration_seconds", reconcileBuckets)
}

func TestRegistryInstancesAreIsolated(t *testing.T) {
	first, err := NewRegistry(staticOwnerSource{})
	if err != nil {
		t.Fatalf("first NewRegistry() error = %v", err)
	}
	second, err := NewRegistry(staticOwnerSource{})
	if err != nil {
		t.Fatalf("second NewRegistry() error = %v", err)
	}
	first.ObserveOpen(time.Millisecond, protocolv1.ErrorCode_ERROR_CODE_OK)

	firstFamilies, err := first.registry.Gather()
	if err != nil {
		t.Fatalf("first Gather() error = %v", err)
	}
	secondFamilies, err := second.registry.Gather()
	if err != nil {
		t.Fatalf("second Gather() error = %v", err)
	}
	assertCounterValue(t, firstFamilies, "xtunnel_open_total", "", 1)
	assertCounterValue(t, secondFamilies, "xtunnel_open_total", "", 0)
}

func TestRegistryConcurrentRecordAndGather(t *testing.T) {
	registry, err := NewRegistry(staticOwnerSource{})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	const workers = 16
	const iterations = 200
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				registry.ObserveOpen(time.Millisecond, protocolv1.ErrorCode_ERROR_CODE_ORIGIN_REFUSED)
				registry.ObserveOriginError(protocolv1.ErrorCode_ERROR_CODE_ORIGIN_REFUSED)
				registry.ObserveOriginConnect(time.Millisecond)
				registry.ObserveReconcile(time.Millisecond, protocolv1.ErrorCode_ERROR_CODE_OK)
				registry.AddIngressBytes(1)
				registry.AddEgressBytes(2)
			}
		}()
	}
	for gatherer := 0; gatherer < 4; gatherer++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				if _, gatherErr := registry.registry.Gather(); gatherErr != nil {
					t.Errorf("Gather() error = %v", gatherErr)
					return
				}
			}
		}()
	}
	wait.Wait()

	families, err := registry.registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	assertCounterValue(t, families, "xtunnel_open_total", "", workers*iterations)
	assertCounterValue(t, families, "xtunnel_ingress_bytes_total", "", workers*iterations)
	assertCounterValue(t, families, "xtunnel_egress_bytes_total", "", 2*workers*iterations)
}

func TestNewRegistryRejectsMissingSource(t *testing.T) {
	if _, err := NewRegistry(nil); !errors.Is(err, errInvalidOptions) {
		t.Fatalf("NewRegistry(nil) error = %v, want %v", err, errInvalidOptions)
	}
}

func assertCounterValue(t *testing.T, families []*dto.MetricFamily, name string, errorCode string, want float64) {
	t.Helper()
	family := findFamily(t, families, name)
	for _, metric := range family.GetMetric() {
		if metricErrorCode(metric) == errorCode {
			if got := metric.GetCounter().GetValue(); got != want {
				t.Errorf("%s{%q} = %v, want %v", name, errorCode, got, want)
			}
			return
		}
	}
	t.Errorf("%s{%q} not found", name, errorCode)
}

func assertGaugeValue(t *testing.T, families []*dto.MetricFamily, name string, want float64) {
	t.Helper()
	family := findFamily(t, families, name)
	if got := family.GetMetric()[0].GetGauge().GetValue(); got != want {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

func assertHistogramBuckets(t *testing.T, families []*dto.MetricFamily, name string, want []float64) {
	t.Helper()
	family := findFamily(t, families, name)
	buckets := family.GetMetric()[0].GetHistogram().GetBucket()
	got := make([]float64, 0, len(buckets))
	for _, bucket := range buckets {
		got = append(got, bucket.GetUpperBound())
	}
	if !slices.EqualFunc(got, want, func(left, right float64) bool {
		return math.Abs(left-right) < 1e-12
	}) {
		t.Errorf("%s buckets = %v, want %v", name, got, want)
	}
}

func findFamily(t *testing.T, families []*dto.MetricFamily, name string) *dto.MetricFamily {
	t.Helper()
	for _, family := range families {
		if family.GetName() == name {
			return family
		}
	}
	t.Fatalf("metric family %q not found", name)
	return nil
}

func metricErrorCode(metric *dto.Metric) string {
	for _, label := range metric.GetLabel() {
		if label.GetName() == "error_code" {
			return label.GetValue()
		}
	}
	return ""
}

func assertFiniteErrorCodeSeries(t *testing.T, family *dto.MetricFamily) {
	t.Helper()
	allowed := make(map[string]struct{}, len(protocolErrorCodes))
	for _, code := range protocolErrorCodes {
		allowed[code.String()] = struct{}{}
	}
	if len(family.GetMetric()) != len(allowed) {
		t.Errorf("%s series = %d, want %d", family.GetName(), len(family.GetMetric()), len(allowed))
	}
	for _, metric := range family.GetMetric() {
		code := metricErrorCode(metric)
		if _, ok := allowed[code]; !ok {
			t.Errorf("%s has non-protocol error_code %q", family.GetName(), code)
		}
	}
}
