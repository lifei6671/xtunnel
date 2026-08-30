//go:build linux

package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	agentgateway "github.com/lifei6671/xtunnel/internal/agent/gateway"
	agentsession "github.com/lifei6671/xtunnel/internal/agent/session"
	agentworkauth "github.com/lifei6671/xtunnel/internal/agent/workauth"
	"github.com/lifei6671/xtunnel/internal/controlsession"
	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	servergateway "github.com/lifei6671/xtunnel/internal/server/gateway"
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
// Token-only Agent、Gateway WorkConn 与 Origin Socket 验证 M6-05。容量、Origin
// 和 Connector/Tunnel 离线均来自真实运行时边沿；协议错误使用已认证测试 Agent
// 在真实 OPEN WorkConn 上返回非法状态/错误码组合，故障仍由生产 OPEN parser 归一。
func TestErrorStatusObservabilityEndToEnd(t *testing.T) {
	serverContext, cancelServer := context.WithCancel(context.Background())
	t.Cleanup(cancelServer)
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

	closer, err := openGatewayAndBootstrapWith(
		serverContext, config, resources,
		slog.New(slog.NewJSONHandler(io.Discard, nil)), runtimeDir,
		func(context.Context, string, string, *sqlite.Store, func() error, func(error)) (io.Closer, error) {
			return nil, errors.New("existing Admin unexpectedly opened Diagnostics E2E Bootstrap Socket")
		},
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
		t.Fatalf("initial Diagnostics projection = %+v, want AVAILABLE with non-nil empty items", initial.RecentErrors)
	}

	issuedToken := issueProductGateToken(t, serverContext, resources, runtime.gateway.Addr())
	stopAgent := startProductGateAgent(t, issuedToken, runtime, 1)
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
	rejected := dialProductGateTCP(t, publicAddress, "127.0.0.33")
	assertProductGateTCPRejected(t, rejected, "Diagnostics capacity")
	tcpOrigin.assertNoNext(t, "Diagnostics capacity")
	waitForDiagnosticsCodes(t, client, runtime, cookie, "NO_CAPACITY")
	if err := pending.Close(); err != nil {
		t.Fatalf("close Diagnostics Pending OPEN: %v", err)
	}
	finishProductGateTCP(t, active, activeEchoDone, "Diagnostics capacity holder")

	// 取消首个正常 Agent 让 Session Manager 完成 generation fencing。Bridge 从
	// 同一 connector_disconnected 事实发布 CONNECTOR_OFFLINE，并在确认 Tunnel
	// 已无 Current Connector 后发布 TUNNEL_OFFLINE；没有额外公网 OPEN 参与转换。
	stopAgent()
	agentStopped = true
	waitForUsageAgentOffline(t, runtime)
	waitForDiagnosticsCodes(
		t, client, runtime, cookie,
		"NO_CAPACITY", "CONNECTOR_OFFLINE", "TUNNEL_OFFLINE",
	)

	// 新代生产 Agent 提供一个干净的 IDLE Work，避免前一代 Pending OPEN 的取消
	// 收敛与随后 Origin 故障共享 WorkPool 时序。
	stopAgent = startProductGateAgent(t, issuedToken, runtime, 1)
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
	originResponse, err := client.Do(originRequest)
	if err != nil {
		t.Fatalf("execute Diagnostics Origin request: %v", err)
	}
	assertProductGateHTTPError(t, originResponse, http.StatusBadGateway, "ORIGIN_REFUSED")
	waitForDiagnosticsCodes(
		t, client, runtime, cookie,
		"NO_CAPACITY", "ORIGIN_DOWN", "CONNECTOR_OFFLINE", "TUNNEL_OFFLINE",
	)

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
	protocolResponse, err := client.Do(protocolRequest)
	if err != nil {
		t.Fatalf("execute Diagnostics Protocol request: %v", err)
	}
	assertProductGateHTTPError(t, protocolResponse, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE")
	protocolAgent.release()
	protocolAgent.waitDone(t)

	final, raw := waitForDiagnosticsCodes(
		t, client, runtime, cookie,
		"NO_CAPACITY", "ORIGIN_DOWN", "CONNECTOR_OFFLINE", "TUNNEL_OFFLINE", "PROTOCOL_ERROR",
	)
	assertDiagnosticsProjection(t, final, raw, issuedToken, httpOriginAddress)
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
		t.Fatalf("Diagnostics E2E login = status %d body=%s read=%v close=%v",
			response.StatusCode, responseBody, readErr, closeErr)
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
		t.Fatalf("Diagnostics Dashboard = status %d body=%s read=%v close=%v",
			response.StatusCode, body, readErr, closeErr)
	}
	var dashboard diagnosticsDashboard
	if err := json.Unmarshal(body, &dashboard); err != nil {
		t.Fatalf("decode Diagnostics Dashboard: %v; body=%s", err, body)
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
			t.Fatalf("Diagnostics Dashboard codes = %v, want %v; body=%s", observed, want, body)
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
	if dashboard.RecentErrors.Availability != "AVAILABLE" || len(dashboard.RecentErrors.Items) != 5 {
		t.Fatalf("final Diagnostics projection = %+v, want AVAILABLE with five fixed slots", dashboard.RecentErrors)
	}
	if dashboard.GeneratedAt.IsZero() || dashboard.GeneratedAt.Location() != time.UTC {
		t.Fatalf("Diagnostics generated_at = %v, want UTC", dashboard.GeneratedAt)
	}
	observed := make(map[string]diagnosticsErrorItem, len(dashboard.RecentErrors.Items))
	for _, item := range dashboard.RecentErrors.Items {
		message, known := diagnosticsGateMessages[item.Code]
		if !known || item.Message != message {
			t.Fatalf("Diagnostics item = %+v, want frozen code and message", item)
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
			t.Fatalf("Diagnostics projection missing %s: %+v", code, dashboard.RecentErrors.Items)
		}
	}
	for _, code := range []string{"ORIGIN_DOWN", "PROTOCOL_ERROR"} {
		requestID := observed[code].RequestID
		if requestID == nil || validate.ValidateID(*requestID, "req_") != nil {
			t.Fatalf("Diagnostics %s request_id = %v, want real req_ ULID", code, requestID)
		}
	}
	for _, code := range []string{"NO_CAPACITY", "CONNECTOR_OFFLINE", "TUNNEL_OFFLINE"} {
		if observed[code].RequestID != nil {
			t.Fatalf("Diagnostics %s request_id = %v, want null without real request context", code, observed[code].RequestID)
		}
	}
	for _, forbidden := range []string{
		diagnosticsGatePassword,
		diagnosticsGateHeaderSentinel,
		connectionToken,
		originAddress,
		productGateOriginHost,
		"127.0.0.1",
		"connection refused",
		"ECONNREFUSED",
		"Authorization",
	} {
		if forbidden != "" && bytes.Contains(body, []byte(forbidden)) {
			t.Fatalf("Diagnostics Dashboard leaked forbidden sentinel %q: %s", forbidden, body)
		}
	}
}

type diagnosticsProtocolAgent struct {
	ready       chan struct{}
	releaseWork chan struct{}
	done        chan error
}

func startDiagnosticsProtocolAgent(t *testing.T, connectionToken string) *diagnosticsProtocolAgent {
	t.Helper()
	connector, err := identity.NewConnector()
	if err != nil {
		t.Fatalf("create Diagnostics protocol Connector identity: %v", err)
	}
	runner, err := agentsession.NewRunner(agentsession.Config{
		ConnectionToken:  connectionToken,
		Connector:        connector,
		Hostname:         "diagnostics-protocol-agent.test",
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
		return fmt.Errorf("Diagnostics protocol OpenRequest = %#v", request)
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
