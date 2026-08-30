//go:build linux

package bootstrap

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

var metricsEndpointFamilies = []string{
	"xtunnel_connectors_online",
	"xtunnel_control_sessions_online",
	"xtunnel_active_connections",
	"xtunnel_tcp_idle_work_connections",
	"xtunnel_tcp_active_work_connections",
	"xtunnel_open_total",
	"xtunnel_open_errors_total",
	"xtunnel_ingress_bytes_total",
	"xtunnel_egress_bytes_total",
	"xtunnel_origin_errors_total",
	"xtunnel_health_targets",
	"xtunnel_health_budget_rejections_total",
	"xtunnel_gateway_certificate_expiry_seconds",
	"xtunnel_open_duration_seconds",
	"xtunnel_origin_connect_duration_seconds",
	"xtunnel_reconcile_duration_seconds",
	"xtunnel_reconcile_errors_total",
	"xtunnel_route_snapshot_bytes",
	"xtunnel_route_snapshot_routes",
	"xtunnel_reconcile_coalesced_total",
}

// TestMetricsEndpointEndToEnd 通过生产 Bootstrap 的真实 Listener 验证 M6-02：
// 私有 Registry 的 20 项契约可被 Prometheus parser 读取，精确 Path 不泄漏到
// Default Mux，关闭完整生命周期后端口可立即重新绑定。
func TestMetricsEndpointEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runtimeDir := newRuntimeDirectory(t)
	dataDir := t.TempDir()
	resources, err := openServerStorage(ctx, dataDir, runtimeDir)
	if err != nil {
		t.Fatalf("open Metrics E2E Server storage: %v", err)
	}
	resourcesClosed := false
	t.Cleanup(func() {
		if !resourcesClosed {
			_ = resources.Close()
		}
	})
	if err := resources.database.CreateFirstAdmin(ctx, "admin", "metrics endpoint gate password"); err != nil {
		t.Fatalf("create Metrics E2E Admin: %v", err)
	}

	config := gatewayLifecycleTestConfig(dataDir, "127.0.0.1:0")
	config.Metrics.Listen = "127.0.0.1:0"
	config.Metrics.Path = "/internal/metrics"
	closer, err := openGatewayAndBootstrapWith(
		ctx,
		config,
		resources,
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		runtimeDir,
		func(context.Context, string, string, *sqlite.Store, func() error, func(error)) (io.Closer, error) {
			return nil, errors.New("existing Admin unexpectedly opened Bootstrap Socket")
		},
	)
	if err != nil {
		t.Fatalf("start Metrics E2E Server runtime: %v", err)
	}
	runtime := closer.(*gatewayBootstrapCloser)
	runtimeClosed := false
	t.Cleanup(func() {
		if !runtimeClosed {
			_ = closer.Close()
		}
	})
	if runtime.metrics.Addr() == nil {
		t.Fatal("Metrics listener did not start")
	}

	// Tunnel/Owner 定向测试验证真实埋点；这里先改变私有 Registry 状态，再从真实
	// Socket 抓取，证明 Bootstrap 使用的是同一个生产 Registry 而非静态文本。
	runtime.metricsRegistry.ObserveOpen(25*time.Millisecond, protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TIMEOUT)
	runtime.metricsRegistry.ObserveOriginError(protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TIMEOUT)
	runtime.metricsRegistry.ObserveOriginConnect(15 * time.Millisecond)
	runtime.metricsRegistry.AddIngressBytes(11)
	runtime.metricsRegistry.AddEgressBytes(13)

	baseURL := "http://" + runtime.metrics.Addr().String()
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(baseURL + config.Metrics.Path)
	if err != nil {
		t.Fatalf("GET production Metrics endpoint: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		t.Fatalf("Metrics status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.Contains(contentType, "text/plain") {
		_ = response.Body.Close()
		t.Fatalf("Metrics Content-Type = %q, want Prometheus text", contentType)
	}
	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(response.Body)
	closeErr := response.Body.Close()
	if err != nil {
		t.Fatalf("parse production Metrics response: %v", err)
	}
	if closeErr != nil {
		t.Fatalf("close production Metrics response: %v", closeErr)
	}
	for _, name := range metricsEndpointFamilies {
		if families[name] == nil {
			t.Errorf("Metrics response missing family %q", name)
		}
	}
	if got := families["xtunnel_open_total"].Metric[0].Counter.GetValue(); got != 1 {
		t.Errorf("xtunnel_open_total = %v, want 1", got)
	}
	if got := families["xtunnel_ingress_bytes_total"].Metric[0].Counter.GetValue(); got != 11 {
		t.Errorf("xtunnel_ingress_bytes_total = %v, want 11", got)
	}
	if expiry := families["xtunnel_gateway_certificate_expiry_seconds"].Metric[0].Gauge.GetValue(); expiry <= float64(time.Now().Unix()) {
		t.Errorf("gateway certificate expiry = %v, want a future Unix timestamp", expiry)
	}

	for _, path := range []string{"/", config.Metrics.Path + "/"} {
		other, requestErr := client.Get(baseURL + path)
		if requestErr != nil {
			t.Fatalf("GET non-Metrics path %q: %v", path, requestErr)
		}
		_ = other.Body.Close()
		if other.StatusCode != http.StatusNotFound {
			t.Errorf("non-Metrics path %q status = %d, want %d", path, other.StatusCode, http.StatusNotFound)
		}
	}

	metricsAddress := runtime.metrics.Addr().String()
	client.CloseIdleConnections()
	if err := closer.Close(); err != nil {
		t.Fatalf("close Metrics E2E Server runtime: %v", err)
	}
	runtimeClosed = true
	listener, err := net.Listen("tcp", metricsAddress)
	if err != nil {
		t.Fatalf("Metrics port was not released after Close: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close Metrics port release probe: %v", err)
	}
	if err := resources.Close(); err != nil {
		t.Fatalf("close Metrics E2E Server storage: %v", err)
	}
	resourcesClosed = true
}

// TestMetricsEndpointBindFailureRollsBackBootstrap 证明独立 Metrics Listener 绑定失败
// 会停止已构造的 owner；释放冲突端口后，同一存储可立即完整重启。
func TestMetricsEndpointBindFailureRollsBackBootstrap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	blocked, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve conflicting Metrics port: %v", err)
	}
	blockedAddress := blocked.Addr().String()
	runtimeDir := newRuntimeDirectory(t)
	dataDir := t.TempDir()
	resources, err := openServerStorage(ctx, dataDir, runtimeDir)
	if err != nil {
		_ = blocked.Close()
		t.Fatalf("open Metrics rollback Server storage: %v", err)
	}
	defer func() { _ = resources.Close() }()
	if err := resources.database.CreateFirstAdmin(ctx, "admin", "metrics rollback gate password"); err != nil {
		_ = blocked.Close()
		t.Fatalf("create Metrics rollback Admin: %v", err)
	}
	config := gatewayLifecycleTestConfig(dataDir, "127.0.0.1:0")
	config.Metrics.Listen = blockedAddress
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	openSocket := func(context.Context, string, string, *sqlite.Store, func() error, func(error)) (io.Closer, error) {
		return nil, errors.New("existing Admin unexpectedly opened Bootstrap Socket")
	}

	closer, err := openGatewayAndBootstrapWith(ctx, config, resources, logger, runtimeDir, openSocket)
	if closer != nil || err == nil || !strings.Contains(err.Error(), "start Prometheus metrics listener") {
		_ = blocked.Close()
		t.Fatalf("conflicting Metrics startup = (%T, %v), want nil bind failure", closer, err)
	}
	if err := blocked.Close(); err != nil {
		t.Fatalf("release conflicting Metrics port: %v", err)
	}

	restarted, err := openGatewayAndBootstrapWith(ctx, config, resources, logger, runtimeDir, openSocket)
	if err != nil {
		t.Fatalf("restart after Metrics bind rollback: %v", err)
	}
	runtime := restarted.(*gatewayBootstrapCloser)
	if runtime.metrics.Addr() == nil || runtime.metrics.Addr().String() != blockedAddress {
		_ = restarted.Close()
		t.Fatalf("restarted Metrics address = %v, want %s", runtime.metrics.Addr(), blockedAddress)
	}
	if err := restarted.Close(); err != nil {
		t.Fatalf("close restarted Metrics runtime: %v", err)
	}
}
