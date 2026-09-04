//go:build windows

package windowsservergate

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	api "github.com/lifei6671/xtunnel/internal/server/managementapi"
)

// managementClient 使用生成的 OpenAPI 请求/响应模型；Secret 只保留在内存，
// 失败仅报告操作和状态码，不输出响应体、Cookie、CSRF 或 Connection Token。
type managementClient struct {
	audit  *secretAudit
	base   string
	client *http.Client
	cookie *http.Cookie
	csrf   string
}

func newManagement(t *testing.T, port int, audit *secretAudit) *managementClient {
	transport := &http.Transport{DisableKeepAlives: true}
	t.Cleanup(transport.CloseIdleConnections)
	return &managementClient{audit: audit, base: fmt.Sprintf("http://127.0.0.1:%d/api/v1", port), client: &http.Client{Timeout: 5 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *managementClient) request(t *testing.T, method, path string, body any, want int, result any, csrf bool, ifMatch ...string) *http.Response {
	t.Helper()
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		must(t, err, "encode management request")
	}
	req, err := http.NewRequestWithContext(t.Context(), method, c.base+path, bytes.NewReader(data))
	must(t, err, "create management request")
	req.Host = "admin.gate.test"
	req.Header.Set("Origin", "https://admin.gate.test")
	req.Header.Set("X-Forwarded-Proto", "https")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cookie != nil {
		req.AddCookie(c.cookie)
	}
	if csrf {
		req.Header.Set("X-XTunnel-CSRF", c.csrf)
	}
	if len(ifMatch) != 0 {
		req.Header.Set("If-Match", ifMatch[0])
	}
	response, err := c.client.Do(req)
	must(t, err, "management "+method+" "+path)
	content, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	closeErr := response.Body.Close()
	must(t, readErr, "read management response")
	must(t, closeErr, "close management response")
	if response.StatusCode != want {
		t.Fatalf("management %s %s status=%d want=%d", method, path, response.StatusCode, want)
	}
	if result != nil {
		if err := json.Unmarshal(content, result); err != nil {
			t.Fatal("management response violates JSON contract")
		}
	}
	clear(content)
	clear(data)
	return response
}
func (c *managementClient) waitReady(t *testing.T, candidates ...*candidateProcess) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	lastStatus := 0
	transportFailed := false
	for time.Now().Before(deadline) {
		if len(candidates) != 0 && candidates[0] != nil {
			if exited, code := candidates[0].exitStatus(); exited {
				t.Fatalf("candidate exited before Management readiness: pid=%d exit_code=%d last_status=%d", candidates[0].command.Process.Pid, code, lastStatus)
			}
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, c.base+"/auth/me", nil)
		must(t, err, "readiness request")
		req.Host = "admin.gate.test"
		// 与实际管理请求使用同一受信 HTTPS 前置代理语义，Host 才规范化为 :443。
		req.Header.Set("X-Forwarded-Proto", "https")
		r, e := c.client.Do(req)
		transportFailed = e != nil
		if e == nil {
			code := r.StatusCode
			lastStatus = code
			must(t, r.Body.Close(), "close readiness")
			if code == http.StatusUnauthorized {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("candidate Management listener did not become ready: last_status=%d transport_failed=%t", lastStatus, transportFailed)
}
func (c *managementClient) login(t *testing.T, password string) {
	t.Helper()
	c.cookie = nil
	c.csrf = ""
	var session api.AuthSession
	response := c.request(t, http.MethodPost, "/auth/login", api.LoginRequest{Username: "gate-admin", Password: &password}, http.StatusOK, &session, false)
	for _, cookie := range response.Cookies() {
		if cookie.Name == "xtunnel_admin_session" {
			c.cookie = cookie
		}
	}
	if c.cookie == nil || !c.cookie.Secure || !c.cookie.HttpOnly || c.cookie.Path != "/api/v1" || len(session.CsrfToken) != 43 {
		t.Fatal("login session cookie/CSRF contract mismatch")
	}
	c.csrf = session.CsrfToken
	c.audit.values = append(c.audit.values, c.cookie.Value, c.csrf)
}
func (c *managementClient) systemInfo(t *testing.T, version string) {
	t.Helper()
	var info api.SystemInfo
	c.request(t, http.MethodGet, "/system/info", nil, http.StatusOK, &info, false)
	if info.Version != version || info.Os != "windows" || info.Arch != "amd64" {
		t.Fatal("authenticated running candidate version/platform mismatch")
	}
}
func (c *managementClient) configure(t *testing.T, origin *origins, publicPort int) (string, string, string, string) {
	t.Helper()
	c.request(t, http.MethodPost, "/tunnels", api.CreateTunnelRequest{Name: "csrf-rejected"}, http.StatusForbidden, nil, false)
	var credential api.TunnelCredentialResponse
	c.request(t, http.MethodPost, "/tunnels", api.CreateTunnelRequest{Name: "windows-product-gate"}, http.StatusCreated, &credential, true)
	if credential.Tunnel.Id == "" || credential.Credential.ConnectionToken == "" || credential.Credential.TunnelId != credential.Tunnel.Id {
		t.Fatal("Tunnel creation did not issue its credential")
	}
	tunnel := credential.Tunnel.Id
	var httpOrigin, tcpOrigin api.OriginInput
	var httpExposure, tcpExposure api.ExposureInput
	must(t, httpOrigin.FromHTTPOriginInput(api.HTTPOriginInput{Scheme: "http", Host: "127.0.0.1", Port: origin.httpPort}), "HTTP Origin union")
	must(t, tcpOrigin.FromTCPOriginInput(api.TCPOriginInput{Scheme: "tcp", Host: "127.0.0.1", Port: origin.tcpPort}), "TCP Origin union")
	must(t, httpExposure.FromHTTPExposureInput(api.HTTPExposureInput{Type: "http", Hostname: "public.gate.test"}), "HTTP exposure union")
	must(t, tcpExposure.FromTCPExposureInput(api.TCPExposureInput{Type: "tcp", PublicPort: &publicPort}), "TCP exposure union")
	var httpService, tcpService api.Service
	httpInput := api.CreateServiceRequest{Name: "gate-http", TunnelId: tunnel, Origin: httpOrigin, Exposure: httpExposure}
	var rejected api.ErrorResponse
	c.request(t, http.MethodPost, "/services", httpInput, http.StatusPreconditionRequired, &rejected, true)
	if rejected.Error.Code != api.APIErrorCodePRECONDITIONREQUIRED {
		t.Fatal("missing If-Match did not report PRECONDITION_REQUIRED")
	}
	// Service 创建修改父 Tunnel 的版本。每次 Mutation 前读取当前强 ETag，不能
	// 用第一个创建响应中的 Service ETag 或旧 Tunnel ETag 为第二次创建授权。
	before := c.tunnelETag(t, tunnel)
	c.request(t, http.MethodPost, "/services", httpInput, http.StatusCreated, &httpService, true, before)
	after := c.tunnelETag(t, tunnel)
	if before == after {
		t.Fatal("Service creation did not advance parent Tunnel ETag")
	}
	c.request(t, http.MethodPost, "/services", api.CreateServiceRequest{Name: "gate-tcp", TunnelId: tunnel, Origin: tcpOrigin, Exposure: tcpExposure}, http.StatusCreated, &tcpService, true, after)
	if httpService.Id == "" || tcpService.Id == "" || !httpService.Enabled || !tcpService.Enabled {
		t.Fatal("created services missing identity/enabled state")
	}
	return tunnel, httpService.Id, tcpService.Id, credential.Credential.ConnectionToken
}
func (c *managementClient) waitRoutes(t *testing.T, tunnel, httpService, tcpService string) {
	t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		var connectors api.ConnectorList
		c.request(t, http.MethodGet, "/tunnels/"+tunnel+"/connectors", nil, http.StatusOK, &connectors, false)
		var hs, ts api.Service
		c.request(t, http.MethodGet, "/services/"+httpService, nil, http.StatusOK, &hs, false)
		c.request(t, http.MethodGet, "/services/"+tcpService, nil, http.StatusOK, &ts, false)
		if len(connectors.Items) == 1 && connectors.Items[0].ConfigReady && connectors.Items[0].IdleWorkConnections > 0 && hs.Status == "READY" && ts.Status == "READY" {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("real Agent Gateway and service configuration did not become ready")
}
func (c *managementClient) assertPersisted(t *testing.T, tunnel, httpService, tcpService string) {
	t.Helper()
	var item api.Tunnel
	c.request(t, http.MethodGet, "/tunnels/"+tunnel, nil, http.StatusOK, &item, false)
	if item.Name != "windows-product-gate" || item.ServicesCount != 2 {
		t.Fatal("restart lost Tunnel/Service desired state")
	}
	for _, id := range []string{httpService, tcpService} {
		var s api.Service
		c.request(t, http.MethodGet, "/services/"+id, nil, http.StatusOK, &s, false)
		if s.Id != id || s.TunnelId != tunnel || !s.Enabled {
			t.Fatal("restart changed Service identity or enabled state")
		}
	}
}
func (c *managementClient) waitActive(t *testing.T, tunnel string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var state api.Tunnel
		c.request(t, http.MethodGet, "/tunnels/"+tunnel, nil, http.StatusOK, &state, false)
		if state.ActiveConnections > 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("active TCP did not appear in product counters")
}

// 使用生产 Handler 验证 readiness 的代理元数据；SQLite 只在临时目录初始化，
// 不启动候选或接触任何固定 Profile。401 仍是唯一就绪状态，400 不能被当作 ready。
func TestManagementReadinessTrustedHTTPSProxy(t *testing.T) {
	store, err := sqlite.Open(t.Context(), t.TempDir())
	must(t, err, "open readiness fixture")
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error("close readiness fixture", err)
		}
	})
	handler, err := api.NewHandler(api.HandlerOptions{
		Management: serverconfig.Management{PublicURL: "https://admin.gate.test", TrustedProxies: []string{"127.0.0.1/32"}},
		Store:      store, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	must(t, err, "construct production Management handler")
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := newManagement(t, 0, &secretAudit{})
	client.base = server.URL + "/api/v1"
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, client.base+"/auth/me", nil)
	must(t, err, "construct missing-proto request")
	request.Host = "admin.gate.test"
	response, err := client.client.Do(request)
	must(t, err, "request missing proxy protocol")
	must(t, response.Body.Close(), "close rejected readiness response")
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing proxy protocol status=%d want=400", response.StatusCode)
	}
	client.waitReady(t)
	client.assertSetupRequired(t, rand.Text()+rand.Text())
}

// 首次运行的生产 Management 明确返回 SETUP_REQUIRED，证明受支持的 bootstrap
// 阶段已可服务；管理员仍由干净停止后的离线命令创建。
func (c *managementClient) assertSetupRequired(t *testing.T, password string) {
	t.Helper()
	var response api.ErrorResponse
	c.request(t, http.MethodPost, "/auth/login", api.LoginRequest{Username: "gate-admin", Password: &password}, http.StatusConflict, &response, false)
	if response.Error.Code != api.APIErrorCodeSETUPREQUIRED || response.Error.RequestId == "" {
		t.Fatal("initial runtime did not report SETUP_REQUIRED")
	}
}

func (c *managementClient) tunnelETag(t *testing.T, tunnel string) string {
	t.Helper()
	var parent api.Tunnel
	response := c.request(t, http.MethodGet, "/tunnels/"+tunnel, nil, http.StatusOK, &parent, false)
	etag := response.Header.Get("ETag")
	if parent.Id != tunnel || len(etag) < 2 || etag[0] != '"' || etag[len(etag)-1] != '"' {
		t.Fatal("parent Tunnel did not return its strong ETag")
	}
	return etag
}
