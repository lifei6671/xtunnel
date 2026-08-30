package metrics

import "github.com/prometheus/client_golang/prometheus"

type gaugeCollector struct {
	source OwnerSource
	desc   gaugeDescriptors
}

type gaugeDescriptors struct {
	connectorsOnline                *prometheus.Desc
	controlSessionsOnline           *prometheus.Desc
	activeConnections               *prometheus.Desc
	tcpIdleWorkConnections          *prometheus.Desc
	tcpActiveWorkConnections        *prometheus.Desc
	healthTargets                   *prometheus.Desc
	healthBudgetRejectionsTotal     *prometheus.Desc
	gatewayCertificateExpirySeconds *prometheus.Desc
	routeSnapshotBytes              *prometheus.Desc
	routeSnapshotRoutes             *prometheus.Desc
	reconcileCoalescedTotal         *prometheus.Desc
}

func newGaugeCollector(source OwnerSource) *gaugeCollector {
	return &gaugeCollector{
		source: source,
		desc: gaugeDescriptors{
			connectorsOnline: prometheus.NewDesc(
				"xtunnel_connectors_online", "Current number of online Connectors.", nil, nil,
			),
			controlSessionsOnline: prometheus.NewDesc(
				"xtunnel_control_sessions_online", "Current number of online Control Sessions.", nil, nil,
			),
			activeConnections: prometheus.NewDesc(
				"xtunnel_active_connections", "Current number of active public connections.", nil, nil,
			),
			tcpIdleWorkConnections: prometheus.NewDesc(
				"xtunnel_tcp_idle_work_connections", "Current number of idle TCP Work Connections.", nil, nil,
			),
			tcpActiveWorkConnections: prometheus.NewDesc(
				"xtunnel_tcp_active_work_connections", "Current number of active TCP Work Connections.", nil, nil,
			),
			healthTargets: prometheus.NewDesc(
				"xtunnel_health_targets", "Current number of reserved health targets.", nil, nil,
			),
			healthBudgetRejectionsTotal: prometheus.NewDesc(
				"xtunnel_health_budget_rejections_total", "Total number of health target budget rejections.", nil, nil,
			),
			gatewayCertificateExpirySeconds: prometheus.NewDesc(
				"xtunnel_gateway_certificate_expiry_seconds", "Unix timestamp in seconds when the current Agent Gateway certificate expires.", nil, nil,
			),
			routeSnapshotBytes: prometheus.NewDesc(
				"xtunnel_route_snapshot_bytes", "Total size in bytes of all currently published deterministic route snapshots.", nil, nil,
			),
			routeSnapshotRoutes: prometheus.NewDesc(
				"xtunnel_route_snapshot_routes", "Total number of serialized routes in all currently published deterministic route snapshots.", nil, nil,
			),
			reconcileCoalescedTotal: prometheus.NewDesc(
				"xtunnel_reconcile_coalesced_total", "Total number of snapshot reconciliation updates coalesced into pending work.", nil, nil,
			),
		},
	}
}

func (collector *gaugeCollector) Describe(descriptions chan<- *prometheus.Desc) {
	descriptions <- collector.desc.connectorsOnline
	descriptions <- collector.desc.controlSessionsOnline
	descriptions <- collector.desc.activeConnections
	descriptions <- collector.desc.tcpIdleWorkConnections
	descriptions <- collector.desc.tcpActiveWorkConnections
	descriptions <- collector.desc.healthTargets
	descriptions <- collector.desc.healthBudgetRejectionsTotal
	descriptions <- collector.desc.gatewayCertificateExpirySeconds
	descriptions <- collector.desc.routeSnapshotBytes
	descriptions <- collector.desc.routeSnapshotRoutes
	descriptions <- collector.desc.reconcileCoalescedTotal
}

func (collector *gaugeCollector) Collect(metrics chan<- prometheus.Metric) {
	snapshot := collector.source.MetricsOwnerSnapshot()
	metrics <- prometheus.MustNewConstMetric(collector.desc.connectorsOnline, prometheus.GaugeValue, float64(snapshot.ConnectorsOnline))
	metrics <- prometheus.MustNewConstMetric(collector.desc.controlSessionsOnline, prometheus.GaugeValue, float64(snapshot.ControlSessionsOnline))
	metrics <- prometheus.MustNewConstMetric(collector.desc.activeConnections, prometheus.GaugeValue, float64(snapshot.ActiveConnections))
	metrics <- prometheus.MustNewConstMetric(collector.desc.tcpIdleWorkConnections, prometheus.GaugeValue, float64(snapshot.TCPIdleWorkConnections))
	metrics <- prometheus.MustNewConstMetric(collector.desc.tcpActiveWorkConnections, prometheus.GaugeValue, float64(snapshot.TCPActiveWorkConnections))
	metrics <- prometheus.MustNewConstMetric(collector.desc.healthTargets, prometheus.GaugeValue, float64(snapshot.HealthTargets))
	metrics <- prometheus.MustNewConstMetric(collector.desc.healthBudgetRejectionsTotal, prometheus.CounterValue, float64(snapshot.HealthBudgetRejectionsTotal))
	metrics <- prometheus.MustNewConstMetric(collector.desc.gatewayCertificateExpirySeconds, prometheus.GaugeValue, snapshot.GatewayCertificateExpirySeconds)
	metrics <- prometheus.MustNewConstMetric(collector.desc.routeSnapshotBytes, prometheus.GaugeValue, float64(snapshot.RouteSnapshotBytes))
	metrics <- prometheus.MustNewConstMetric(collector.desc.routeSnapshotRoutes, prometheus.GaugeValue, float64(snapshot.RouteSnapshotRoutes))
	metrics <- prometheus.MustNewConstMetric(collector.desc.reconcileCoalescedTotal, prometheus.CounterValue, float64(snapshot.ReconcileCoalescedTotal))
}
