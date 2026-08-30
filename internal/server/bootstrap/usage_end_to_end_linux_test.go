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

	"github.com/lifei6671/xtunnel/internal/repository"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
)

const usageGatePassword = "usage aggregation gate password"

// TestUsageAggregationEndToEnd 通过生产 Bootstrap、真实 Gateway、Token-only Agent、
// TCP Origin 与 Management Listener 验证 M6-04。Heartbeat 不参与持久化计数；
// 业务字节只从 OPEN_OK 后的 RAW 边界进入唯一 Usage Owner。
func TestUsageAggregationEndToEnd(t *testing.T) {
	serverContext, cancelServer := context.WithCancel(context.Background())
	defer cancelServer()
	httpOrigin := httptest.NewServer(http.NotFoundHandler())
	defer httpOrigin.Close()
	tcpOrigin := startProductGateTCPOrigin(t)
	publicAddress, publicPort := reserveProductGateTCPPort(t)
	runtimeDir := newRuntimeDirectory(t)
	dataDir := t.TempDir()

	resources, err := openServerStorage(serverContext, dataDir, runtimeDir)
	if err != nil {
		t.Fatalf("open Usage E2E Server storage: %v", err)
	}
	resourcesClosed := false
	defer func() {
		if !resourcesClosed {
			if closeErr := resources.Close(); closeErr != nil {
				t.Errorf("close Usage E2E Server storage: %v", closeErr)
			}
		}
	}()
	seedProductGateDesiredState(
		t, serverContext, resources, httpOrigin.Listener.Addr(), tcpOrigin.listener.Addr(), publicPort,
	)
	if err := resources.database.CreateFirstAdmin(serverContext, "admin", usageGatePassword); err != nil {
		t.Fatalf("create Usage E2E Admin: %v", err)
	}

	config := gatewayLifecycleTestConfig(dataDir, "127.0.0.1:0")
	config.TCPIngress.MinPort = int(publicPort)
	config.TCPIngress.MaxPort = int(publicPort)
	config.Limits.MaxWorkConnections = 8
	config.Limits.MaxIdleWorkConnections = 8
	config.Limits.MaxConnectingWorkConnections = 8
	config.Limits.MaxPendingTLSHandshakes = 16
	config.Limits.MaxPendingAuth = 16
	closer, err := openGatewayAndBootstrapWith(
		serverContext, config, resources,
		slog.New(slog.NewJSONHandler(io.Discard, nil)), runtimeDir,
		func(context.Context, string, string, *sqlite.Store, func() error, func(error)) (io.Closer, error) {
			return nil, errors.New("existing Admin unexpectedly opened Usage E2E Bootstrap Socket")
		},
	)
	if err != nil {
		t.Fatalf("start Usage E2E Server runtime: %v", err)
	}
	runtime := closer.(*gatewayBootstrapCloser)
	runtimeClosed := false
	defer func() {
		if !runtimeClosed {
			if closeErr := closer.Close(); closeErr != nil {
				t.Errorf("close Usage E2E Server runtime: %v", closeErr)
			}
		}
	}()

	issuedToken := issueProductGateToken(t, serverContext, resources, runtime.gateway.Addr())
	stopAgent := startProductGateAgent(t, issuedToken, runtime, 5)
	agentStopped := false
	defer func() {
		if !agentStopped {
			stopAgent()
		}
	}()

	ingressPayload := []byte("usage-ingress")
	egressPayload := []byte("usage-egress-is-longer")
	public := dialProductGateTCP(t, publicAddress, "127.0.0.7")
	originPeer := tcpOrigin.next(t, "Usage asymmetric payload")
	originResult := make(chan error, 1)
	go func() {
		received, readErr := io.ReadAll(originPeer)
		if readErr == nil && !bytes.Equal(received, ingressPayload) {
			readErr = fmt.Errorf("Origin ingress = %q, want %q", received, ingressPayload)
		}
		_, writeErr := originPeer.Write(egressPayload)
		var closeWriteErr error
		if tcpPeer, ok := originPeer.(*net.TCPConn); ok {
			closeWriteErr = tcpPeer.CloseWrite()
		}
		originResult <- errors.Join(readErr, writeErr, closeWriteErr, originPeer.Close())
	}()
	if _, err := public.Write(ingressPayload); err != nil {
		t.Fatalf("write Usage ingress payload: %v", err)
	}
	if err := public.CloseWrite(); err != nil {
		t.Fatalf("half-close Usage public connection: %v", err)
	}
	received, err := io.ReadAll(public)
	if err != nil || !bytes.Equal(received, egressPayload) {
		t.Fatalf("read Usage egress payload = (%q, %v), want %q", received, err, egressPayload)
	}
	if err := public.Close(); err != nil {
		t.Fatalf("close Usage public connection: %v", err)
	}
	if err := <-originResult; err != nil {
		t.Fatalf("complete Usage Origin exchange: %v", err)
	}

	if err := runtime.usage.Flush(t.Context()); err != nil {
		t.Fatalf("flush successful Usage exchange: %v", err)
	}
	want := repository.UsageTotals{
		Connections: 1, IngressBytes: uint64(len(ingressPayload)), EgressBytes: uint64(len(egressPayload)),
	}
	assertUsageRepositoryTotals(t, resources.database, want)
	assertUsageManagementAPI(t, runtime, want)

	stopAgent()
	agentStopped = true
	waitForUsageAgentOffline(t, runtime)
	failed := dialProductGateTCP(t, publicAddress, "127.0.0.8")
	if _, err := failed.Read(make([]byte, 1)); err == nil {
		_ = failed.Close()
		t.Fatal("offline Usage OPEN read error = nil, want final OPEN failure")
	}
	if err := failed.Close(); err != nil {
		t.Fatalf("close failed Usage public connection: %v", err)
	}
	if err := runtime.usage.Flush(t.Context()); err != nil {
		t.Fatalf("flush failed Usage OPEN: %v", err)
	}
	want.Errors = 1
	assertUsageRepositoryTotals(t, resources.database, want)

	if err := closer.Close(); err != nil {
		t.Fatalf("close Usage E2E runtime before restart: %v", err)
	}
	runtimeClosed = true
	if err := resources.Close(); err != nil {
		t.Fatalf("close Usage E2E storage before restart: %v", err)
	}
	resourcesClosed = true

	restartedResources, err := openServerStorage(serverContext, dataDir, runtimeDir)
	if err != nil {
		t.Fatalf("reopen Usage E2E Server storage: %v", err)
	}
	restartedClosed := false
	defer func() {
		if !restartedClosed {
			if closeErr := restartedResources.Close(); closeErr != nil {
				t.Errorf("close restarted Usage E2E storage: %v", closeErr)
			}
		}
	}()
	restartedCloser, err := openGatewayAndBootstrapWith(
		serverContext, config, restartedResources,
		slog.New(slog.NewJSONHandler(io.Discard, nil)), runtimeDir,
		func(context.Context, string, string, *sqlite.Store, func() error, func(error)) (io.Closer, error) {
			return nil, errors.New("restart unexpectedly opened Usage E2E Bootstrap Socket")
		},
	)
	if err != nil {
		t.Fatalf("restart Usage E2E Server runtime: %v", err)
	}
	restartedRuntime := restartedCloser.(*gatewayBootstrapCloser)
	assertUsageRepositoryTotals(t, restartedResources.database, want)
	if err := restartedCloser.Close(); err != nil {
		t.Fatalf("close restarted Usage E2E runtime: %v", err)
	}
	assertUsageRepositoryTotals(t, restartedResources.database, want)
	if err := restartedResources.Close(); err != nil {
		t.Fatalf("close restarted Usage E2E storage: %v", err)
	}
	restartedClosed = true
	_ = restartedRuntime
}

func assertUsageRepositoryTotals(t *testing.T, store *sqlite.Store, want repository.UsageTotals) {
	t.Helper()
	var got repository.UsageTotals
	err := store.Read(t.Context(), func(view repository.RepositoryView) error {
		var readErr error
		got, readErr = view.Usage().Today(t.Context(), time.Now().UTC(), productGateTunnelID, productGateTCPServiceID)
		return readErr
	})
	if err != nil {
		t.Fatalf("read Usage repository totals: %v", err)
	}
	if got != want {
		t.Fatalf("Usage repository totals = %+v, want %+v", got, want)
	}
}

type usageAPIEnvelope struct {
	Usage   usageAPISummary `json:"usage"`
	Traffic usageAPISummary `json:"traffic"`
}

type usageAPISummary struct {
	Availability      string `json:"availability"`
	ConnectionsToday  *int64 `json:"connections_today"`
	IngressBytesToday *int64 `json:"ingress_bytes_today"`
	EgressBytesToday  *int64 `json:"egress_bytes_today"`
}

func assertUsageManagementAPI(t *testing.T, runtime *gatewayBootstrapCloser, want repository.UsageTotals) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	password := usageGatePassword
	loginBody, err := json.Marshal(map[string]any{"username": "admin", "password": &password})
	if err != nil {
		t.Fatalf("marshal Usage E2E login: %v", err)
	}
	login := usageManagementRequest(t, client, runtime, http.MethodPost, "/api/v1/auth/login", loginBody, nil)
	defer login.Body.Close()
	if login.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(login.Body)
		t.Fatalf("Usage E2E login status = %d, body=%s", login.StatusCode, body)
	}
	cookies := login.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("Usage E2E login cookies = %d, want 1", len(cookies))
	}
	paths := []struct {
		path  string
		field string
	}{
		{path: "/api/v1/services/" + productGateTCPServiceID, field: "usage"},
		{path: "/api/v1/dashboard", field: "traffic"},
	}
	for _, item := range paths {
		response := usageManagementRequest(t, client, runtime, http.MethodGet, item.path, nil, cookies[0])
		var envelope usageAPIEnvelope
		decodeErr := json.NewDecoder(response.Body).Decode(&envelope)
		closeErr := response.Body.Close()
		if response.StatusCode != http.StatusOK || decodeErr != nil || closeErr != nil {
			t.Fatalf("GET %s = status %d, decode=%v, close=%v", item.path, response.StatusCode, decodeErr, closeErr)
		}
		summary := envelope.Usage
		if item.field == "traffic" {
			summary = envelope.Traffic
		}
		if summary.Availability != "AVAILABLE" || summary.ConnectionsToday == nil ||
			summary.IngressBytesToday == nil || summary.EgressBytesToday == nil ||
			*summary.ConnectionsToday != int64(want.Connections) ||
			*summary.IngressBytesToday != int64(want.IngressBytes) ||
			*summary.EgressBytesToday != int64(want.EgressBytes) {
			t.Fatalf("GET %s Usage = %+v, want AVAILABLE %+v", item.path, summary, want)
		}
	}
}

func usageManagementRequest(
	t *testing.T,
	client *http.Client,
	runtime *gatewayBootstrapCloser,
	method string,
	path string,
	body []byte,
	cookie *http.Cookie,
) *http.Response {
	t.Helper()
	target := "http://" + runtime.management.Addr().String() + path
	request, err := http.NewRequestWithContext(t.Context(), method, target, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("construct Usage Management request: %v", err)
	}
	request.Host = "admin.example.test"
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("X-Forwarded-Host", "admin.example.test")
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "https://admin.example.test")
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("execute Usage Management request: %v", err)
	}
	return response
}

func waitForUsageAgentOffline(t *testing.T, runtime *gatewayBootstrapCloser) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		online := false
		for _, snapshot := range runtime.sessions.RuntimeStatusSnapshots() {
			online = online || snapshot.TunnelID == productGateTunnelID && snapshot.CurrentControlSession
		}
		if !online {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("Usage E2E Agent remained online after shutdown")
		case <-ticker.C:
		}
	}
}
