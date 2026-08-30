//go:build linux

package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	agentgateway "github.com/lifei6671/xtunnel/internal/agent/gateway"
	agentsession "github.com/lifei6671/xtunnel/internal/agent/session"
	agentworkauth "github.com/lifei6671/xtunnel/internal/agent/workauth"
	"github.com/lifei6671/xtunnel/internal/controlsession"
	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/logging"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	servergateway "github.com/lifei6671/xtunnel/internal/server/gateway"
	internaltracing "github.com/lifei6671/xtunnel/internal/tracing"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const (
	diagnosticsGatePassword       = "diagnostics gate secret password"
	diagnosticsGateHeaderSentinel = "diagnostics-header-must-not-leak"
)

var diagnosticsGateMessages = map[string]string{
	"TUNNEL_OFFLINE":    "Tunnel 已离线",
	"CONNECTOR_OFFLINE": "Connector 已离线",
	"ORIGIN_DOWN":       "Origin 当前不可用",
	"NO_CAPACITY":       "当前没有可用容量",
	"PROTOCOL_ERROR":    "检测到协议错误",
}

// TestErrorStatusObservabilityEndToEnd 使用生产 Bootstrap、Management API、
// Token-only Agent、Gateway WorkConn 与 Origin Socket，联合验证 M6-05 投影和
// M6-07 的 Dashboard、日志、Metric、Trace 一致性。容量、Origin 与 Offline 均来自
// 真实运行时边沿；协议错误只由已认证测试 Agent 替换 OPEN 响应，最终仍由生产
// OPEN parser、日志、指标和 Trace 路径归一。
func TestErrorStatusObservabilityEndToEnd(t *testing.T) {
	serverContext, cancelServer := context.WithCancel(context.Background())
	t.Cleanup(cancelServer)
	traceRecorder := tracetest.NewSpanRecorder()
	serverTracing := newTraceGateRuntime(t, traceRecorder, "xtunnel-server")
	agentTracing := newTraceGateRuntime(t, traceRecorder, "xtunnel-agent")
	serverLogs := new(traceGateLockedBuffer)
	agentLogs := new(traceGateLockedBuffer)
	serverLogger := newTraceGateLogger(t, serverLogs, "server")
	agentLogger := newTraceGateLogger(t, agentLogs, "agent")
	httpOrigin := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(httpOrigin.Close)
	httpOriginAddress := httpOrigin.Listener.Addr().String()
	tcpOrigin := startProductGateTCPOrigin(t)
	publicAddress, publicPort := reserveProductGateTCPPort(t)
	runtimeDir := newRuntimeDirectory(t)
	dataDir := t.TempDir()

	resources, err := openServerStorage(serverContext, dataDir, runtimeDir)
	if err != nil {
		t.Fatalf("open Diagnostics E2E Server storage: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := resources.Close(); closeErr != nil {
			t.Errorf("close Diagnostics E2E Server storage: %v", closeErr)
		}
	})
	seedProductGateDesiredState(
		t, serverContext, resources, httpOrigin.Listener.Addr(), tcpOrigin.listener.Addr(), publicPort,
	)
	if err := resources.database.CreateFirstAdmin(serverContext, "admin", diagnosticsGatePassword); err != nil {
		t.Fatalf("create Diagnostics E2E Admin: %v", err)
	}

	config := gatewayLifecycleTestConfig(dataDir, "127.0.0.1:0")
	config.TCPIngress.MinPort = int(publicPort)
	config.TCPIngress.MaxPort = int(publicPort)
	config.Limits.MaxWorkConnections = 1
	config.Limits.MaxIdleWorkConnections = 1
	config.Limits.MaxConnectingWorkConnections = 1
	config.Limits.MaxPendingOpens = 1
	config.Limits.MaxOpenRatePerSourceIP = 8
	config.Limits.MaxOpenBurstPerSourceIP = 8
	config.Limits.MaxActiveConnections = 8
	config.Limits.MaxConnectionsPerTunnel = 8
	config.Limits.MaxConnectionsPerService = 8
	config.Limits.MaxConnectionsPerSourceIP = 8
	config.Limits.MaxPendingTLSHandshakes = 16
	config.Limits.MaxPendingAuth = 16

	closer, err := openGatewayAndBootstrapWithStartedAtTracing(
		serverContext, config, resources, serverLogger, time.Now(), runtimeDir,
		func(context.Context, string, string, *sqlite.Store, func() error, func(error)) (io.Closer, error) {
			return nil, errors.New("existing Admin unexpectedly opened Diagnostics E2E Bootstrap Socket")
		},
		serverTracing,
	)
	if err != nil {
		t.Fatalf("start Diagnostics E2E Server runtime: %v", err)
	}
	runtime := closer.(*gatewayBootstrapCloser)
	t.Cleanup(func() {
		if closeErr := closer.Close(); closeErr != nil {
			t.Errorf("close Diagnostics E2E Server runtime: %v", closeErr)
		}
	})

	client, cookie := loginDiagnosticsGate(t, runtime)
	initial, _ := readDiagnosticsDashboard(t, client, runtime, cookie)
	if initial.RecentErrors.Availability != "AVAILABLE" || initial.RecentErrors.Items == nil ||
		len(initial.RecentErrors.Items) != 0 {
		t.Fatal("initial Diagnostics projection was not AVAILABLE with non-nil empty items")
	}

	issuedToken := issueProductGateToken(t, serverContext, resources, runtime.gateway.Addr())
	stopAgent := startTraceGateAgent(t, issuedToken, runtime, agentLogger, agentTracing)
	agentStopped := false
	t.Cleanup(func() {
		if !agentStopped {
			stopAgent()
		}
	})

	// 首条连接占住唯一 Work，第二条进入唯一 Pending 槽，第三条从生产
	// Limit Manager 快速失败并由最终逻辑 OPEN 投影为 NO_CAPACITY。
	active := dialProductGateTCP(t, publicAddress, "127.0.0.31")
	activeEchoDone := startProductGateOriginEcho(tcpOrigin.next(t, "Diagnostics capacity holder"))
	assertProductGateRoundTrip(t, active, []byte("hold-diagnostics-work"), "Diagnostics capacity holder")
	pending := dialProductGateTCP(t, publicAddress, "127.0.0.32")
	if _, err := pending.Write([]byte("pending-diagnostics-open")); err != nil {
		t.Fatalf("write Diagnostics Pending OPEN: %v", err)
	}
	assertProductGateTCPPending(t, pending)
	capacityMetrics := readDiagnosticsMetrics(t, runtime, config.Metrics.Path)
	capacitySpanStart := len(traceRecorder.Ended())
	capacityLogStart := len(diagnosticsLogRecords(t, serverLogs))
	rejected := dialProductGateTCP(t, publicAddress, "127.0.0.33")
	assertProductGateTCPRejected(t, rejected, "Diagnostics capacity")
	tcpOrigin.assertNoNext(t, "Diagnostics capacity")
	waitForDiagnosticsCodes(t, client, runtime, cookie, "NO_CAPACITY")
	waitForDiagnosticsMetricDelta(
		t, runtime, config.Metrics.Path, capacityMetrics,
		"xtunnel_open_errors_total", protocolv1.ErrorCode_ERROR_CODE_WORK_POOL_EXHAUSTED.String(), 1,
	)
	capacitySpans := waitForDiagnosticsErrorSpans(
		t, traceRecorder, capacitySpanStart, "WORK_POOL_EXHAUSTED", "ingress.Accept", "tunnel.DialContext",
	)
	capacityLog := waitForDiagnosticsLog(
		t, serverLogs, capacityLogStart, logging.EventTCPIngressConnectionFailed, "WORK_POOL_EXHAUSTED",
	)
	assertDiagnosticsTraceLink(t, capacitySpans, capacityLog)
	if err := pending.Close(); err != nil {
		t.Fatalf("close Diagnostics Pending OPEN: %v", err)
	}
	finishProductGateTCP(t, active, activeEchoDone, "Diagnostics capacity holder")
	waitForDiagnosticsMetricDelta(
		t, runtime, config.Metrics.Path, capacityMetrics, "xtunnel_open_total", "", 2,
	)
	waitForDiagnosticsSpanCount(t, traceRecorder, capacitySpanStart, "tunnel.DialContext", 2)
	settledCapacityMetrics := readDiagnosticsMetrics(t, runtime, config.Metrics.Path)
	if got, want := settledCapacityMetrics.value(
		"xtunnel_open_errors_total", protocolv1.ErrorCode_ERROR_CODE_WORK_POOL_EXHAUSTED.String(),
	), capacityMetrics.value(
		"xtunnel_open_errors_total", protocolv1.ErrorCode_ERROR_CODE_WORK_POOL_EXHAUSTED.String(),
	)+1; got != want {
		t.Fatalf("settled capacity counter = %v, want exactly one increment to %v", got, want)
	}

	// 正常 Agent 取消会先完成 Drain，必须证明该预期关闭不会制造离线诊断。
	drainSpanStart := len(traceRecorder.Ended())
	drainLogStart := len(diagnosticsLogRecords(t, serverLogs))
	drainMetrics := readDiagnosticsMetrics(t, runtime, config.Metrics.Path)
	stopAgent()
	agentStopped = true
	waitForUsageAgentOffline(t, runtime)
	waitForDiagnosticsGauge(t, runtime, config.Metrics.Path, "xtunnel_connectors_online", 0)
	afterDrain, _ := readDiagnosticsDashboard(t, client, runtime, cookie)
	if len(afterDrain.RecentErrors.Items) != 1 || afterDrain.RecentErrors.Items[0].Code != "NO_CAPACITY" {
		t.Fatalf("graceful Agent Drain produced %d Diagnostics items, want the existing NO_CAPACITY item only", len(afterDrain.RecentErrors.Items))
	}
	afterDrainMetrics := readDiagnosticsMetrics(t, runtime, config.Metrics.Path)
	if got, want := afterDrainMetrics.value(
		"xtunnel_open_errors_total", protocolv1.ErrorCode_ERROR_CODE_TUNNEL_OFFLINE.String(),
	), drainMetrics.value(
		"xtunnel_open_errors_total", protocolv1.ErrorCode_ERROR_CODE_TUNNEL_OFFLINE.String(),
	); got != want {
		t.Fatalf("graceful Agent Drain TUNNEL_OFFLINE counter = %v, want unchanged %v", got, want)
	}
	assertNoDiagnosticsErrorSpan(t, traceRecorder, drainSpanStart, "TUNNEL_OFFLINE")
	drainDisconnect := waitForDiagnosticsLog(
		t, serverLogs, drainLogStart, logging.EventConnectorDisconnected, "",
	)
	assertDiagnosticsLifecycleHasNoTrace(t, drainDisconnect)

	// 另起一条已认证 Control Session，不发送 Drain 而直接关闭底层 Session，模拟
	// 进程崩溃/网络中断。Bridge 从同一 generation-fenced connector_disconnected
	// 事实发布 CONNECTOR_OFFLINE，并在 Tunnel 已无 Current Connector 时发布
	// TUNNEL_OFFLINE；没有额外公网 OPEN 参与转换。
	abruptAgent := startDiagnosticsAbruptAgent(t, issuedToken, runtime)
	waitForDiagnosticsGauge(t, runtime, config.Metrics.Path, "xtunnel_connectors_online", 1)
	abruptStartedSpanStart := len(traceRecorder.Started())
	abruptEndedSpanStart := len(traceRecorder.Ended())
	abruptLogStart := len(diagnosticsLogRecords(t, serverLogs))
	abruptAgent.crash(t)
	waitForUsageAgentOffline(t, runtime)
	waitForDiagnosticsCodes(
		t, client, runtime, cookie,
		"NO_CAPACITY", "CONNECTOR_OFFLINE", "TUNNEL_OFFLINE",
	)
	waitForDiagnosticsGauge(t, runtime, config.Metrics.Path, "xtunnel_connectors_online", 0)
	abruptDisconnect := waitForDiagnosticsLog(
		t, serverLogs, abruptLogStart, logging.EventConnectorDisconnected, "",
	)
	assertDiagnosticsLifecycleHasNoTrace(t, abruptDisconnect)
	if got := len(traceRecorder.Started()); got != abruptStartedSpanStart {
		t.Fatalf("abrupt Offline lifecycle started %d request Spans, want zero", got-abruptStartedSpanStart)
	}
	if got := len(traceRecorder.Ended()); got != abruptEndedSpanStart {
		t.Fatalf("abrupt Offline lifecycle ended %d request Spans, want zero", got-abruptEndedSpanStart)
	}

	// Offline 生命周期只携带 generation-fenced 业务身份，不能伪造请求 Trace。
	// 随后的真实公网请求才创建 Root Span，并把同一次 TUNNEL_OFFLINE 失败关联到
	// Tunnel 日志与错误 Counter。
	offlineMetrics := readDiagnosticsMetrics(t, runtime, config.Metrics.Path)
	offlineSpanStart := len(traceRecorder.Ended())
	offlineLogStart := len(diagnosticsLogRecords(t, serverLogs))
	offlineRequest, err := http.NewRequestWithContext(
		t.Context(), http.MethodGet,
		"http://"+runtime.httpIngress.Addr().String()+"/gate/diagnostics-offline", nil,
	)
	if err != nil {
		t.Fatalf("construct Diagnostics Offline request: %v", err)
	}
	offlineRequest.Host = productGatePublicHost
	offlineResponse, err := client.Do(offlineRequest)
	if err != nil {
		t.Fatalf("execute Diagnostics Offline request: %v", err)
	}
	assertDiagnosticsHTTPError(t, offlineResponse, http.StatusServiceUnavailable, "SERVICE_CONFIG_NOT_OBSERVED")
	waitForDiagnosticsMetricDelta(
		t, runtime, config.Metrics.Path, offlineMetrics,
		"xtunnel_open_errors_total", protocolv1.ErrorCode_ERROR_CODE_TUNNEL_OFFLINE.String(), 1,
	)
	offlineSpans := waitForDiagnosticsErrorSpans(
		t, traceRecorder, offlineSpanStart, "TUNNEL_OFFLINE", "tunnel.DialContext",
	)
	offlineLog := waitForDiagnosticsLog(
		t, serverLogs, offlineLogStart, logging.EventTunnelConnectionFailed, "TUNNEL_OFFLINE",
	)
	assertDiagnosticsTraceLink(t, offlineSpans, offlineLog)

	// 新代生产 Agent 提供一个干净的 IDLE Work，避免前一代 Pending OPEN 的取消
	// 收敛与随后 Origin 故障共享 WorkPool 时序。
	stopAgent = startTraceGateAgent(t, issuedToken, runtime, agentLogger, agentTracing)
	agentStopped = false

	// Health 未启用，因此关闭真实 HTTP Origin 后，下一次公网请求必然进入
	// Agent Origin Dial，并把连接拒绝归一为 ORIGIN_DOWN，而不是健康门禁错误。
	httpOrigin.Close()
	originRequest, err := http.NewRequestWithContext(
		t.Context(), http.MethodGet,
		"http://"+runtime.httpIngress.Addr().String()+"/gate/diagnostics-origin", nil,
	)
	if err != nil {
		t.Fatalf("construct Diagnostics Origin request: %v", err)
	}
	originRequest.Host = productGatePublicHost
	originRequest.Header.Set("X-Diagnostics-Sentinel", diagnosticsGateHeaderSentinel)
	originMetrics := readDiagnosticsMetrics(t, runtime, config.Metrics.Path)
	originSpanStart := len(traceRecorder.Ended())
	originServerLogStart := len(diagnosticsLogRecords(t, serverLogs))
	originAgentLogStart := len(diagnosticsLogRecords(t, agentLogs))
	originResponse, err := client.Do(originRequest)
	if err != nil {
		t.Fatalf("execute Diagnostics Origin request: %v", err)
	}
	assertDiagnosticsHTTPError(t, originResponse, http.StatusBadGateway, "ORIGIN_REFUSED")
	waitForDiagnosticsCodes(
		t, client, runtime, cookie,
		"NO_CAPACITY", "ORIGIN_DOWN", "CONNECTOR_OFFLINE", "TUNNEL_OFFLINE",
	)
	waitForDiagnosticsMetricDelta(
		t, runtime, config.Metrics.Path, originMetrics,
		"xtunnel_open_errors_total", protocolv1.ErrorCode_ERROR_CODE_ORIGIN_REFUSED.String(), 1,
	)
	waitForDiagnosticsMetricDelta(
		t, runtime, config.Metrics.Path, originMetrics,
		"xtunnel_origin_errors_total", protocolv1.ErrorCode_ERROR_CODE_ORIGIN_REFUSED.String(), 1,
	)
	originSpans := waitForDiagnosticsErrorSpans(
		t, traceRecorder, originSpanStart, "ORIGIN_REFUSED", "tunnel.DialContext", "origin.Dial",
	)
	originServerLog := waitForDiagnosticsLog(
		t, serverLogs, originServerLogStart, logging.EventTunnelConnectionFailed, "ORIGIN_REFUSED",
	)
	originAgentLog := waitForDiagnosticsLog(
		t, agentLogs, originAgentLogStart, logging.EventAgentOriginConnectionFailed, "ORIGIN_REFUSED",
	)
	assertDiagnosticsTraceLink(t, originSpans, originServerLog, originAgentLog)

	// 结束第二代正常 Agent，确保协议故障测试只有一个 Current Connector。
	stopAgent()
	agentStopped = true
	waitForUsageAgentOffline(t, runtime)
	waitForDiagnosticsCodes(
		t, client, runtime, cookie,
		"NO_CAPACITY", "ORIGIN_DOWN", "CONNECTOR_OFFLINE", "TUNNEL_OFFLINE",
	)

	// 正常生产 Agent 不会主动违反协议。这里使用生产 Control/Work 认证建立
	// IDLE WorkConn，只把 OPEN 响应替换为非法 OK+PROTOCOL_ERROR 组合；Server
	// OPEN parser、Tunnel 最终分类和 Dashboard API 全部仍走生产实现。
	protocolAgent := startDiagnosticsProtocolAgent(t, issuedToken)
	protocolAgent.waitReady(t)
	protocolRequest, err := http.NewRequestWithContext(
		t.Context(), http.MethodGet,
		"http://"+runtime.httpIngress.Addr().String()+"/gate/diagnostics-protocol", nil,
	)
	if err != nil {
		t.Fatalf("construct Diagnostics Protocol request: %v", err)
	}
	protocolRequest.Host = productGatePublicHost
	protocolRequest.Header.Set("X-Diagnostics-Sentinel", diagnosticsGateHeaderSentinel)
	protocolMetrics := readDiagnosticsMetrics(t, runtime, config.Metrics.Path)
	protocolSpanStart := len(traceRecorder.Ended())
	protocolLogStart := len(diagnosticsLogRecords(t, serverLogs))
	protocolResponse, err := client.Do(protocolRequest)
	if err != nil {
		t.Fatalf("execute Diagnostics Protocol request: %v", err)
	}
	assertDiagnosticsHTTPError(t, protocolResponse, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE")
	protocolAgent.release()
	protocolAgent.waitDone(t)

	final, raw := waitForDiagnosticsCodes(
		t, client, runtime, cookie,
		"NO_CAPACITY", "ORIGIN_DOWN", "CONNECTOR_OFFLINE", "TUNNEL_OFFLINE", "PROTOCOL_ERROR",
	)
	waitForDiagnosticsMetricDelta(
		t, runtime, config.Metrics.Path, protocolMetrics,
		"xtunnel_open_errors_total", protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR.String(), 1,
	)
	protocolSpans := waitForDiagnosticsErrorSpans(
		t, traceRecorder, protocolSpanStart, "PROTOCOL_ERROR", "tunnel.DialContext",
	)
	protocolLog := waitForDiagnosticsLog(
		t, serverLogs, protocolLogStart, logging.EventTunnelConnectionFailed, "PROTOCOL_ERROR",
	)
	assertDiagnosticsTraceLink(t, protocolSpans, protocolLog)
	assertDiagnosticsProjection(t, final, raw, issuedToken, httpOriginAddress)
	assertDiagnosticsLogSafety(t, serverLogs, agentLogs, issuedToken, httpOriginAddress)
	assertDiagnosticsSpanSafety(t, traceRecorder, issuedToken, httpOriginAddress)
}

type diagnosticsDashboard struct {
	GeneratedAt  time.Time `json:"generated_at"`
	RecentErrors struct {
		Availability string                 `json:"availability"`
		Items        []diagnosticsErrorItem `json:"items"`
	} `json:"recent_errors"`
}

type diagnosticsErrorItem struct {
	Code       string    `json:"code"`
	Message    string    `json:"message"`
	OccurredAt time.Time `json:"occurred_at"`
	RequestID  *string   `json:"request_id"`
}

type diagnosticsMetricSnapshot struct {
	values map[string]float64
}

func (snapshot diagnosticsMetricSnapshot) value(family, errorCode string) float64 {
	return snapshot.values[family+"\x00"+errorCode]
}

func assertDiagnosticsHTTPError(t *testing.T, response *http.Response, status int, code string) {
	t.Helper()
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || response.StatusCode != status || strings.TrimSpace(string(body)) != code {
		t.Fatalf(
			"Diagnostics HTTP error = status %d bytes=%d, want %d %s; read=%v close=%v",
			response.StatusCode, len(body), status, code, readErr, closeErr,
		)
	}
}

// readDiagnosticsMetrics 通过生产 Metrics Listener 抓取同一私有 Registry，避免
// 测试绕过 Bootstrap 装配或直接调用 Recorder 制造不可复现的计数。
func readDiagnosticsMetrics(
	t *testing.T,
	runtime *gatewayBootstrapCloser,
	path string,
) diagnosticsMetricSnapshot {
	t.Helper()
	metricsClient := &http.Client{Timeout: 3 * time.Second}
	response, err := metricsClient.Get("http://" + runtime.metrics.Addr().String() + path)
	if err != nil {
		t.Fatalf("GET Diagnostics Metrics: %v", err)
	}
	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, parseErr := parser.TextToMetricFamilies(response.Body)
	closeErr := response.Body.Close()
	if response.StatusCode != http.StatusOK || parseErr != nil || closeErr != nil {
		t.Fatalf(
			"Diagnostics Metrics = status %d parse_failed=%t close_failed=%t",
			response.StatusCode, parseErr != nil, closeErr != nil,
		)
	}
	snapshot := diagnosticsMetricSnapshot{values: make(map[string]float64)}
	openTotal := families["xtunnel_open_total"]
	if openTotal == nil || len(openTotal.GetMetric()) != 1 {
		seriesCount := 0
		if openTotal != nil {
			seriesCount = len(openTotal.GetMetric())
		}
		t.Fatalf("Diagnostics Metrics xtunnel_open_total present=%t series=%d, want present with one series", openTotal != nil, seriesCount)
	}
	snapshot.values["xtunnel_open_total\x00"] = openTotal.GetMetric()[0].GetCounter().GetValue()
	connectors := families["xtunnel_connectors_online"]
	if connectors == nil || len(connectors.GetMetric()) != 1 {
		seriesCount := 0
		if connectors != nil {
			seriesCount = len(connectors.GetMetric())
		}
		t.Fatalf("Diagnostics Metrics xtunnel_connectors_online present=%t series=%d, want present with one series", connectors != nil, seriesCount)
	}
	snapshot.values["xtunnel_connectors_online\x00"] = connectors.GetMetric()[0].GetGauge().GetValue()
	for _, familyName := range []string{"xtunnel_open_errors_total", "xtunnel_origin_errors_total"} {
		family := families[familyName]
		if family == nil {
			t.Fatalf("Diagnostics Metrics missing %s", familyName)
		}
		for _, metric := range family.GetMetric() {
			errorCode := ""
			for _, label := range metric.GetLabel() {
				if label.GetName() == "error_code" {
					errorCode = label.GetValue()
					break
				}
			}
			if errorCode != "" {
				snapshot.values[familyName+"\x00"+errorCode] = metric.GetCounter().GetValue()
			}
		}
	}
	return snapshot
}

func waitForDiagnosticsMetricDelta(
	t *testing.T,
	runtime *gatewayBootstrapCloser,
	path string,
	baseline diagnosticsMetricSnapshot,
	family string,
	errorCode string,
	wantDelta float64,
) diagnosticsMetricSnapshot {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	want := baseline.value(family, errorCode) + wantDelta
	for {
		snapshot := readDiagnosticsMetrics(t, runtime, path)
		got := snapshot.value(family, errorCode)
		if got == want {
			return snapshot
		}
		if got > want {
			t.Fatalf("%s{%q} = %v, want baseline %v + %v", family, errorCode, got, want-wantDelta, wantDelta)
		}
		select {
		case <-deadline.C:
			t.Fatalf("%s{%q} = %v, want baseline %v + %v", family, errorCode, got, want-wantDelta, wantDelta)
		case <-ticker.C:
		}
	}
}

func waitForDiagnosticsGauge(
	t *testing.T,
	runtime *gatewayBootstrapCloser,
	path string,
	family string,
	want float64,
) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		snapshot := readDiagnosticsMetrics(t, runtime, path)
		if got := snapshot.value(family, ""); got == want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("%s = %v, want %v", family, snapshot.value(family, ""), want)
		case <-ticker.C:
		}
	}
}

func diagnosticsLogRecords(t *testing.T, output *traceGateLockedBuffer) []map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(output.snapshot()))
	var records []map[string]any
	for {
		record := make(map[string]any)
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			return records
		}
		if err != nil {
			t.Fatalf("decode Diagnostics log: %v", err)
		}
		records = append(records, record)
	}
}

func waitForDiagnosticsLog(
	t *testing.T,
	output *traceGateLockedBuffer,
	start int,
	event string,
	errorCode string,
) map[string]any {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		records := diagnosticsLogRecords(t, output)
		if start > len(records) {
			t.Fatalf("Diagnostics log baseline %d exceeds current records %d", start, len(records))
		}
		for _, record := range records[start:] {
			if record[logging.EventKey] == event &&
				(errorCode == "" || record[logging.ErrorCodeKey] == errorCode) {
				return record
			}
		}
		select {
		case <-deadline.C:
			t.Fatalf("Diagnostics log missing event=%q error_code=%q after record %d", event, errorCode, start)
		case <-ticker.C:
		}
	}
}

func waitForDiagnosticsErrorSpans(
	t *testing.T,
	recorder *tracetest.SpanRecorder,
	start int,
	errorCode string,
	names ...string,
) map[string]sdktrace.ReadOnlySpan {
	t.Helper()
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		wanted[name] = struct{}{}
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		ended := recorder.Ended()
		if start > len(ended) {
			t.Fatalf("Diagnostics Span baseline %d exceeds ended spans %d", start, len(ended))
		}
		observed := make(map[string]sdktrace.ReadOnlySpan, len(wanted))
		for _, span := range ended[start:] {
			if _, ok := wanted[span.Name()]; !ok || diagnosticsSpanErrorCode(span) != errorCode {
				continue
			}
			if span.Status().Code != codes.Error {
				t.Fatalf("Diagnostics Span %q did not have Error status", span.Name())
			}
			observed[span.Name()] = span
		}
		if len(observed) == len(wanted) {
			return observed
		}
		select {
		case <-deadline.C:
			observedNames := make([]string, 0, len(observed))
			for name := range observed {
				observedNames = append(observedNames, name)
			}
			t.Fatalf("Diagnostics error Span names = %v, want %v with error_code=%s", observedNames, names, errorCode)
		case <-ticker.C:
		}
	}
}

func diagnosticsSpanErrorCode(span sdktrace.ReadOnlySpan) string {
	for _, item := range span.Attributes() {
		if item.Key == internaltracing.AttributeErrorCode {
			return item.Value.AsString()
		}
	}
	return ""
}

func waitForDiagnosticsSpanCount(
	t *testing.T,
	recorder *tracetest.SpanRecorder,
	start int,
	name string,
	want int,
) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		ended := recorder.Ended()
		if start > len(ended) {
			t.Fatalf("Diagnostics Span baseline %d exceeds ended spans %d", start, len(ended))
		}
		got := 0
		for _, span := range ended[start:] {
			if span.Name() == name {
				got++
			}
		}
		if got == want {
			return
		}
		if got > want {
			t.Fatalf("Diagnostics Span %q count after baseline = %d, want %d", name, got, want)
		}
		select {
		case <-deadline.C:
			t.Fatalf("Diagnostics Span %q count after baseline = %d, want %d", name, got, want)
		case <-ticker.C:
		}
	}
}

func assertNoDiagnosticsErrorSpan(
	t *testing.T,
	recorder *tracetest.SpanRecorder,
	start int,
	errorCode string,
) {
	t.Helper()
	ended := recorder.Ended()
	if start > len(ended) {
		t.Fatalf("Diagnostics Span baseline %d exceeds ended spans %d", start, len(ended))
	}
	for _, span := range ended[start:] {
		if diagnosticsSpanErrorCode(span) == errorCode {
			t.Fatalf("unexpected Diagnostics error Span with error_code=%s", errorCode)
		}
	}
}

func assertDiagnosticsLifecycleHasNoTrace(t *testing.T, record map[string]any) {
	t.Helper()
	if _, exists := record[logging.TraceIDKey]; exists {
		t.Fatal("Connector lifecycle log unexpectedly contained trace_id")
	}
}

func assertDiagnosticsTraceLink(
	t *testing.T,
	spans map[string]sdktrace.ReadOnlySpan,
	records ...map[string]any,
) {
	t.Helper()
	traceID := ""
	for name, span := range spans {
		got := span.SpanContext().TraceID().String()
		if !span.SpanContext().TraceID().IsValid() {
			t.Fatalf("Diagnostics Span %q has invalid TraceID", name)
		}
		if traceID == "" {
			traceID = got
		} else if got != traceID {
			t.Fatalf("Diagnostics Span %q TraceID = %s, want %s", name, got, traceID)
		}
	}
	for _, record := range records {
		if got := record[logging.TraceIDKey]; got != traceID {
			t.Fatal("Diagnostics log trace_id did not match its error Span")
		}
	}
}

func assertDiagnosticsLogSafety(
	t *testing.T,
	serverLogs *traceGateLockedBuffer,
	agentLogs *traceGateLockedBuffer,
	connectionToken string,
	originAddress string,
) {
	t.Helper()
	combined := strings.ToLower(string(serverLogs.snapshot()) + string(agentLogs.snapshot()))
	for _, forbidden := range []struct {
		label string
		value string
	}{
		{label: "admin password", value: diagnosticsGatePassword},
		{label: "header sentinel", value: diagnosticsGateHeaderSentinel},
		{label: "connection token", value: connectionToken},
		{label: "origin address", value: originAddress},
		{label: "origin host", value: productGateOriginHost},
		{label: "connection refused detail", value: "connection refused"},
		{label: "Windows connection detail", value: "connectex"},
		{label: "POSIX connection detail", value: "econnrefused"},
		{label: "authorization header", value: "authorization"},
	} {
		if forbidden.value != "" && strings.Contains(combined, strings.ToLower(forbidden.value)) {
			t.Fatalf("Diagnostics logs leaked %s", forbidden.label)
		}
	}
}

func assertDiagnosticsSpanSafety(
	t *testing.T,
	recorder *tracetest.SpanRecorder,
	connectionToken string,
	originAddress string,
) {
	t.Helper()
	forbidden := []struct {
		label string
		value string
	}{
		{label: "admin password", value: diagnosticsGatePassword},
		{label: "header sentinel", value: diagnosticsGateHeaderSentinel},
		{label: "connection token", value: connectionToken},
		{label: "origin address", value: originAddress},
		{label: "origin host", value: productGateOriginHost},
		{label: "connection refused detail", value: "connection refused"},
		{label: "Windows connection detail", value: "connectex"},
		{label: "POSIX connection detail", value: "econnrefused"},
		{label: "authorization header", value: "authorization"},
	}
	for _, span := range recorder.Ended() {
		fields := []string{span.Name(), span.Status().Description}
		for _, item := range span.Attributes() {
			fields = append(fields, fmt.Sprint(item.Value.AsInterface()))
		}
		for _, event := range span.Events() {
			fields = append(fields, event.Name)
			for _, item := range event.Attributes {
				fields = append(fields, fmt.Sprint(item.Value.AsInterface()))
			}
		}
		for _, link := range span.Links() {
			for _, item := range link.Attributes {
				fields = append(fields, fmt.Sprint(item.Value.AsInterface()))
			}
		}
		for _, candidate := range fields {
			candidate = strings.ToLower(candidate)
			for _, item := range forbidden {
				if item.value != "" && strings.Contains(candidate, strings.ToLower(item.value)) {
					t.Fatalf("Diagnostics Spans leaked %s", item.label)
				}
			}
		}
	}
}

func loginDiagnosticsGate(t *testing.T, runtime *gatewayBootstrapCloser) (*http.Client, *http.Cookie) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	password := diagnosticsGatePassword
	body, err := json.Marshal(map[string]any{"username": "admin", "password": &password})
	if err != nil {
		t.Fatalf("marshal Diagnostics E2E login: %v", err)
	}
	response := usageManagementRequest(t, client, runtime, http.MethodPost, "/api/v1/auth/login", body, nil)
	responseBody, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if response.StatusCode != http.StatusOK || readErr != nil || closeErr != nil {
		t.Fatalf("Diagnostics E2E login = status %d bytes=%d read=%v close=%v",
			response.StatusCode, len(responseBody), readErr, closeErr)
	}
	cookies := response.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("Diagnostics E2E login cookies = %d, want 1", len(cookies))
	}
	return client, cookies[0]
}

func readDiagnosticsDashboard(
	t *testing.T,
	client *http.Client,
	runtime *gatewayBootstrapCloser,
	cookie *http.Cookie,
) (diagnosticsDashboard, []byte) {
	t.Helper()
	response := usageManagementRequest(t, client, runtime, http.MethodGet, "/api/v1/dashboard", nil, cookie)
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if response.StatusCode != http.StatusOK || readErr != nil || closeErr != nil {
		t.Fatalf("Diagnostics Dashboard = status %d bytes=%d read=%v close=%v",
			response.StatusCode, len(body), readErr, closeErr)
	}
	var dashboard diagnosticsDashboard
	if err := json.Unmarshal(body, &dashboard); err != nil {
		t.Fatalf("decode Diagnostics Dashboard (%d bytes): %v", len(body), err)
	}
	return dashboard, body
}

func waitForDiagnosticsCodes(
	t *testing.T,
	client *http.Client,
	runtime *gatewayBootstrapCloser,
	cookie *http.Cookie,
	want ...string,
) (diagnosticsDashboard, []byte) {
	t.Helper()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		dashboard, body := readDiagnosticsDashboard(t, client, runtime, cookie)
		observed := make(map[string]bool, len(dashboard.RecentErrors.Items))
		for _, item := range dashboard.RecentErrors.Items {
			observed[item.Code] = true
		}
		complete := true
		for _, code := range want {
			complete = complete && observed[code]
		}
		if complete {
			return dashboard, body
		}
		select {
		case <-deadline.C:
			t.Fatalf("Diagnostics Dashboard observed %d codes, want %v; response_bytes=%d", len(observed), want, len(body))
		case <-ticker.C:
		}
	}
}

func assertDiagnosticsProjection(
	t *testing.T,
	dashboard diagnosticsDashboard,
	body []byte,
	connectionToken string,
	originAddress string,
) {
	t.Helper()
	for _, forbidden := range []struct {
		label string
		value string
	}{
		{label: "admin password", value: diagnosticsGatePassword},
		{label: "header sentinel", value: diagnosticsGateHeaderSentinel},
		{label: "connection token", value: connectionToken},
		{label: "origin address", value: originAddress},
		{label: "origin host", value: productGateOriginHost},
		{label: "loopback address", value: "127.0.0.1"},
		{label: "connection refused detail", value: "connection refused"},
		{label: "POSIX connection detail", value: "ECONNREFUSED"},
		{label: "authorization header", value: "Authorization"},
	} {
		if forbidden.value != "" && bytes.Contains(body, []byte(forbidden.value)) {
			t.Fatalf("Diagnostics Dashboard leaked %s", forbidden.label)
		}
	}
	if dashboard.RecentErrors.Availability != "AVAILABLE" || len(dashboard.RecentErrors.Items) != 5 {
		t.Fatalf("final Diagnostics projection did not contain AVAILABLE with five fixed slots")
	}
	if dashboard.GeneratedAt.IsZero() || dashboard.GeneratedAt.Location() != time.UTC {
		t.Fatalf("Diagnostics generated_at = %v, want UTC", dashboard.GeneratedAt)
	}
	observed := make(map[string]diagnosticsErrorItem, len(dashboard.RecentErrors.Items))
	for index, item := range dashboard.RecentErrors.Items {
		message, known := diagnosticsGateMessages[item.Code]
		if !known || item.Message != message {
			t.Fatalf("Diagnostics item %d did not contain a frozen code and message", index)
		}
		if item.OccurredAt.IsZero() || item.OccurredAt.Location() != time.UTC {
			t.Fatalf("Diagnostics %s occurred_at = %v, want UTC", item.Code, item.OccurredAt)
		}
		if _, duplicate := observed[item.Code]; duplicate {
			t.Fatalf("Diagnostics code %s occupied more than one slot", item.Code)
		}
		observed[item.Code] = item
	}
	for code := range diagnosticsGateMessages {
		if _, ok := observed[code]; !ok {
			t.Fatalf("Diagnostics projection missing expected code %s", code)
		}
	}
	for _, code := range []string{"ORIGIN_DOWN", "PROTOCOL_ERROR"} {
		requestID := observed[code].RequestID
		if requestID == nil || validate.ValidateID(*requestID, "req_") != nil {
			t.Fatalf("Diagnostics %s request_id was not a real req_ ULID", code)
		}
	}
	for _, code := range []string{"NO_CAPACITY", "CONNECTOR_OFFLINE", "TUNNEL_OFFLINE"} {
		if observed[code].RequestID != nil {
			t.Fatalf("Diagnostics %s request_id was not null without real request context", code)
		}
	}
}

type diagnosticsProtocolAgent struct {
	ready       chan struct{}
	releaseWork chan struct{}
	done        chan error
}

type diagnosticsAbruptAgent struct {
	session     *agentsession.Session
	inboundDone chan error
	crashOnce   sync.Once
}

func startDiagnosticsAbruptAgent(
	t *testing.T,
	connectionToken string,
	runtime *gatewayBootstrapCloser,
) *diagnosticsAbruptAgent {
	t.Helper()
	connector, err := identity.NewConnector()
	if err != nil {
		t.Fatalf("create Diagnostics abrupt Connector identity: %v", err)
	}
	runner, err := newDiagnosticsSessionRunner(connectionToken, connector, "diagnostics-abrupt-agent.test")
	if err != nil {
		t.Fatalf("construct Diagnostics abrupt Agent session: %v", err)
	}
	session, err := runner.Start(t.Context())
	if err != nil {
		t.Fatalf("start Diagnostics abrupt Control Session: %v", err)
	}
	agent := &diagnosticsAbruptAgent{session: session, inboundDone: make(chan error, 1)}
	go func() { agent.inboundDone <- acknowledgeDiagnosticsSnapshots(t.Context(), session) }()
	t.Cleanup(func() { agent.crash(t) })

	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		for _, snapshot := range runtime.sessions.RuntimeStatusSnapshots() {
			if snapshot.TunnelID == productGateTunnelID && snapshot.CurrentControlSession &&
				snapshot.Config.ConfigReady && snapshot.Config.HasObserved &&
				snapshot.Config.ObservedRevision == 1 {
				return agent
			}
		}
		select {
		case runErr := <-agent.inboundDone:
			agent.session.Close()
			_ = agent.session.Wait()
			t.Fatalf("Diagnostics abrupt Agent stopped before ready: %v", runErr)
		case <-deadline.C:
			agent.crash(t)
			t.Fatal("Diagnostics abrupt Agent did not publish a ready Snapshot")
		case <-ticker.C:
		}
	}
}

func newDiagnosticsSessionRunner(
	connectionToken string,
	connector identity.Connector,
	hostname string,
) (*agentsession.Runner, error) {
	return agentsession.NewRunner(agentsession.Config{
		ConnectionToken:  connectionToken,
		Connector:        connector,
		Hostname:         hostname,
		Version:          "v0.1.0-m6-diagnostics-gate",
		OS:               "linux",
		Arch:             "test",
		Capabilities:     []string{"tcp"},
		AuthWriteTimeout: 2 * time.Second,
		AuthReadTimeout:  2 * time.Second,
		OwnerOptions: controlsession.Options{
			HighPriorityCapacity: 8,
			NormalCapacity:       8,
			InboundCapacity:      8,
			WriteTimeout:         2 * time.Second,
		},
	})
}

func acknowledgeDiagnosticsSnapshots(ctx context.Context, session *agentsession.Session) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case inbound, ok := <-session.Inbound():
			if !ok {
				return nil
			}
			envelope := inbound.Envelope
			snapshot := envelope.GetConfigSnapshot()
			if snapshot == nil {
				continue
			}
			if err := session.Enqueue(&protocolv1.ControlEnvelope{
				ProtocolVersion: envelope.GetProtocolVersion(),
				Payload: &protocolv1.ControlEnvelope_ConfigAck{ConfigAck: &protocolv1.ConfigAck{
					ObservedRevision: snapshot.GetRevision(),
					ApplyStatus:      protocolv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_APPLIED,
					ErrorCode:        protocolv1.ErrorCode_ERROR_CODE_OK,
				}},
			}); err != nil {
				return fmt.Errorf("ack Diagnostics abrupt Snapshot: %w", err)
			}
		}
	}
}

func (agent *diagnosticsAbruptAgent) crash(t *testing.T) {
	t.Helper()
	agent.crashOnce.Do(func() {
		agent.session.Close()
		waitErr := agent.session.Wait()
		select {
		case inboundErr := <-agent.inboundDone:
			if inboundErr != nil && !errors.Is(inboundErr, context.Canceled) && !errors.Is(inboundErr, net.ErrClosed) {
				t.Errorf("stop Diagnostics abrupt inbound owner: %v", inboundErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("Diagnostics abrupt inbound owner did not stop")
		}
		if waitErr != nil && !errors.Is(waitErr, context.Canceled) && !errors.Is(waitErr, net.ErrClosed) {
			t.Errorf("stop Diagnostics abrupt Control Session: %v", waitErr)
		}
	})
}

func startDiagnosticsProtocolAgent(t *testing.T, connectionToken string) *diagnosticsProtocolAgent {
	t.Helper()
	connector, err := identity.NewConnector()
	if err != nil {
		t.Fatalf("create Diagnostics protocol Connector identity: %v", err)
	}
	runner, err := newDiagnosticsSessionRunner(connectionToken, connector, "diagnostics-protocol-agent.test")
	if err != nil {
		t.Fatalf("construct Diagnostics protocol Agent session: %v", err)
	}
	agent := &diagnosticsProtocolAgent{
		ready: make(chan struct{}), releaseWork: make(chan struct{}), done: make(chan error, 1),
	}
	go func() {
		agent.done <- runDiagnosticsProtocolAgent(
			t.Context(), runner, connectionToken, agent.ready, agent.releaseWork,
		)
	}()
	return agent
}

func runDiagnosticsProtocolAgent(
	ctx context.Context,
	runner *agentsession.Runner,
	connectionToken string,
	readySignal chan<- struct{},
	releaseWork <-chan struct{},
) (resultErr error) {
	session, err := runner.Start(ctx)
	if err != nil {
		return fmt.Errorf("start Diagnostics protocol Control Session: %w", err)
	}
	defer func() {
		session.Close()
		if waitErr := session.Wait(); waitErr != nil && !errors.Is(waitErr, context.Canceled) &&
			!errors.Is(waitErr, net.ErrClosed) {
			resultErr = errors.Join(resultErr, fmt.Errorf("wait Diagnostics protocol Control Session: %w", waitErr))
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case inbound, ok := <-session.Inbound():
			if !ok {
				return errors.New("Diagnostics protocol Control Session closed before WorkDemand")
			}
			envelope := inbound.Envelope
			if snapshot := envelope.GetConfigSnapshot(); snapshot != nil {
				ack := &protocolv1.ControlEnvelope{
					ProtocolVersion: envelope.GetProtocolVersion(),
					Payload: &protocolv1.ControlEnvelope_ConfigAck{ConfigAck: &protocolv1.ConfigAck{
						ObservedRevision: snapshot.GetRevision(),
						ApplyStatus:      protocolv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_APPLIED,
						ErrorCode:        protocolv1.ErrorCode_ERROR_CODE_OK,
					}},
				}
				if err := session.Enqueue(ack); err != nil {
					return fmt.Errorf("ack Diagnostics protocol Snapshot: %w", err)
				}
				continue
			}
			demand := envelope.GetWorkDemand()
			if demand == nil || demand.GetMaxNewConnections() == 0 {
				continue
			}
			return serveDiagnosticsMalformedOpen(
				ctx, session, connectionToken, demand.GetBudgetLeaseId(), readySignal, releaseWork,
			)
		}
	}
}

func serveDiagnosticsMalformedOpen(
	ctx context.Context,
	session *agentsession.Session,
	connectionToken string,
	leaseID string,
	readySignal chan<- struct{},
	releaseWork <-chan struct{},
) (resultErr error) {
	connection, err := agentgateway.DialContext(ctx, connectionToken, servergateway.WorkALPN)
	if err != nil {
		return fmt.Errorf("dial Diagnostics protocol WorkConn: %w", err)
	}
	defer func() {
		if closeErr := connection.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			resultErr = errors.Join(resultErr, fmt.Errorf("close Diagnostics protocol WorkConn: %w", closeErr))
		}
	}()
	authentication, err := session.WorkAuthSession()
	if err != nil {
		return fmt.Errorf("copy Diagnostics protocol Work authentication: %w", err)
	}
	_, err = agentworkauth.Authenticate(ctx, connection, agentworkauth.Config{
		Session: authentication, BudgetLeaseID: leaseID,
		WriteTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second,
	})
	clear(authentication.SessionSecret[:])
	if err != nil {
		return fmt.Errorf("authenticate Diagnostics protocol WorkConn: %w", err)
	}
	close(readySignal)
	stopClose := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopClose()
	if err := connection.SetDeadline(time.Now().Add(8 * time.Second)); err != nil {
		return fmt.Errorf("set Diagnostics protocol OPEN deadline: %w", err)
	}
	request := &protocolv1.OpenRequest{}
	if err := frame.ReadWork(connection, request); err != nil {
		return fmt.Errorf("read Diagnostics protocol OpenRequest: %w", err)
	}
	if request.GetProtocolVersion() != 1 || request.GetServiceId() != productGateHTTPServiceID ||
		request.GetIngressType() != protocolv1.IngressType_INGRESS_TYPE_HTTP {
		return errors.New("Diagnostics protocol OpenRequest did not match the injected malformed response case")
	}
	// OPEN_STATUS_OK 携带非 OK code 明确违反 Protocol v1。响应保持合法编码和
	// connection_id 关联，使唯一故障点位于生产 OPEN 语义校验，而非测试 IO。
	if err := frame.WriteWork(connection, &protocolv1.OpenResponse{
		ConnectionId: request.GetConnectionId(),
		Status:       protocolv1.OpenStatus_OPEN_STATUS_OK,
		ErrorCode:    protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR,
	}); err != nil {
		return fmt.Errorf("write Diagnostics malformed OpenResponse: %w", err)
	}
	select {
	case <-releaseWork:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (agent *diagnosticsProtocolAgent) release() { close(agent.releaseWork) }

func (agent *diagnosticsProtocolAgent) waitReady(t *testing.T) {
	t.Helper()
	select {
	case <-agent.ready:
	case err := <-agent.done:
		t.Fatalf("Diagnostics protocol Agent stopped before IDLE Work: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatal("Diagnostics protocol Agent did not publish IDLE Work")
	}
}

func (agent *diagnosticsProtocolAgent) waitDone(t *testing.T) {
	t.Helper()
	select {
	case err := <-agent.done:
		if err != nil {
			t.Fatalf("Diagnostics protocol Agent failed: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("Diagnostics protocol Agent did not finish malformed OPEN")
	}
}
