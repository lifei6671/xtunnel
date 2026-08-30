package managementapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/application"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	serverrecenterror "github.com/lifei6671/xtunnel/internal/server/recenterror"
	serversnapshot "github.com/lifei6671/xtunnel/internal/server/snapshot"
	"github.com/lifei6671/xtunnel/internal/tcpport"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"
)

type managementOpenAPIContract struct {
	document map[string]any
}

func TestManagementSuccessfulResponsesMatchOpenAPI(t *testing.T) {
	contract := loadManagementOpenAPIContract(t)
	harness := newServiceAPIHarness(t)
	covered := make(map[string]struct{})
	assertSuccess := func(method, path string, status int, response *http.Response) []byte {
		t.Helper()
		body := contract.assertResponse(t, method, path, status, response)
		covered[method+" "+path] = struct{}{}
		return body
	}

	loginClient := &http.Client{Transport: harness.server.Client().Transport}
	login := postLogin(t, loginClient, harness.server.URL, harness.publicURL, "admin", "correct horse battery staple")
	assertSuccess(http.MethodPost, "/auth/login", http.StatusOK, login)

	authMe := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodGet, "/api/v1/auth/me", nil, "", false)
	assertSuccess(http.MethodGet, "/auth/me", http.StatusOK, authMe)

	createdResponse := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPost, "/api/v1/tunnels", CreateTunnelRequest{Name: "contract tunnel"}, "", true)
	tunnelETag := createdResponse.Header.Get("ETag")
	createdBody := assertSuccess(http.MethodPost, "/tunnels", http.StatusCreated, createdResponse)
	var created TunnelCredentialResponse
	if err := json.Unmarshal(createdBody, &created); err != nil {
		t.Fatalf("decode created Tunnel: %v", err)
	}
	if err := harness.budget.InitializeTunnel(created.Tunnel.Id, uint64(created.Tunnel.DesiredRevision), 0); err != nil {
		t.Fatalf("InitializeTunnel() error = %v", err)
	}

	listedTunnels := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodGet, "/api/v1/tunnels", nil, "", false)
	assertSuccess(http.MethodGet, "/tunnels", http.StatusOK, listedTunnels)

	tunnelPath := "/api/v1/tunnels/" + created.Tunnel.Id
	gotTunnel := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodGet, tunnelPath, nil, "", false)
	assertSuccess(http.MethodGet, "/tunnels/{tunnel_id}", http.StatusOK, gotTunnel)

	updatedTunnel := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPatch, tunnelPath,
		map[string]any{"name": "contract tunnel updated"}, tunnelETag, true)
	tunnelETag = updatedTunnel.Header.Get("ETag")
	assertSuccess(http.MethodPatch, "/tunnels/{tunnel_id}", http.StatusOK, updatedTunnel)

	connectors := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodGet, tunnelPath+"/connectors", nil, "", false)
	assertSuccess(http.MethodGet, "/tunnels/{tunnel_id}/connectors", http.StatusOK, connectors)

	revealed := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodGet, "/api/v1/tunnels/"+created.Tunnel.Id+"/token", nil, "", false)
	assertSuccess(http.MethodGet, "/tunnels/{tunnel_id}/token", http.StatusOK, revealed)

	rotated := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPost, tunnelPath+"/token/rotate", nil, tunnelETag, true)
	tunnelETag = rotated.Header.Get("ETag")
	assertSuccess(http.MethodPost, "/tunnels/{tunnel_id}/token/rotate", http.StatusOK, rotated)

	serviceBody := map[string]any{
		"tunnel_id": created.Tunnel.Id,
		"name":      "contract service",
		"origin": map[string]any{
			"scheme": "http", "host": "127.0.0.1", "port": 8080,
		},
		"exposure": map[string]any{"type": "http", "hostname": "contract.example.test"},
	}
	serviceResponse := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPost, "/api/v1/services", serviceBody, tunnelETag, true)
	serviceETag := serviceResponse.Header.Get("ETag")
	serviceJSON := assertSuccess(http.MethodPost, "/services", http.StatusCreated, serviceResponse)
	var service Service
	if err := json.Unmarshal(serviceJSON, &service); err != nil {
		t.Fatalf("decode created Service: %v", err)
	}

	serviceListPath := "/api/v1/services?tunnel_id=" + created.Tunnel.Id
	listedServices := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodGet, serviceListPath, nil, "", false)
	assertSuccess(http.MethodGet, "/services", http.StatusOK, listedServices)

	servicePath := "/api/v1/services/" + service.Id
	gotService := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodGet, servicePath, nil, "", false)
	assertSuccess(http.MethodGet, "/services/{service_id}", http.StatusOK, gotService)

	updatedService := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPatch, servicePath,
		map[string]any{"name": "contract service updated"}, serviceETag, true)
	serviceETag = updatedService.Header.Get("ETag")
	assertSuccess(http.MethodPatch, "/services/{service_id}", http.StatusOK, updatedService)

	disabledService := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPost, servicePath+"/disable", nil, serviceETag, true)
	serviceETag = disabledService.Header.Get("ETag")
	assertSuccess(http.MethodPost, "/services/{service_id}/disable", http.StatusOK, disabledService)

	enabledService := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPost, servicePath+"/enable", nil, serviceETag, true)
	serviceETag = enabledService.Header.Get("ETag")
	assertSuccess(http.MethodPost, "/services/{service_id}/enable", http.StatusOK, enabledService)

	deletedService := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodDelete, "/api/v1/services/"+service.Id, nil, serviceETag, true)
	assertSuccess(http.MethodDelete, "/services/{service_id}", http.StatusNoContent, deletedService)

	revokedToken := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPost, tunnelPath+"/token/revoke", nil, tunnelETag, true)
	tunnelETag = revokedToken.Header.Get("ETag")
	assertSuccess(http.MethodPost, "/tunnels/{tunnel_id}/token/revoke", http.StatusOK, revokedToken)

	revokedTunnel := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPost, tunnelPath+"/revoke", nil, tunnelETag, true)
	tunnelETag = revokedTunnel.Header.Get("ETag")
	assertSuccess(http.MethodPost, "/tunnels/{tunnel_id}/revoke", http.StatusOK, revokedTunnel)

	deletedTunnel := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodDelete, tunnelPath, nil, tunnelETag, true)
	assertSuccess(http.MethodDelete, "/tunnels/{tunnel_id}", http.StatusNoContent, deletedTunnel)

	logout := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPost, "/api/v1/auth/logout", nil, "", true)
	assertSuccess(http.MethodPost, "/auth/logout", http.StatusNoContent, logout)

	readHarness := newManagementReadContractHarness(t, contract)
	for _, test := range []struct {
		actualPath   string
		contractPath string
	}{
		{actualPath: "/api/v1/dashboard", contractPath: "/dashboard"},
		{actualPath: "/api/v1/system/info", contractPath: "/system/info"},
		{actualPath: "/api/v1/system/health", contractPath: "/system/health"},
		{actualPath: "/api/v1/system/config", contractPath: "/system/config"},
		{actualPath: "/api/v1/security-audit-events", contractPath: "/security-audit-events"},
		{actualPath: "/api/v1/security-audit-events/export", contractPath: "/security-audit-events/export"},
	} {
		response := doRequest(t, readHarness.client, http.MethodGet, readHarness.server.URL+test.actualPath, "", nil)
		assertSuccess(http.MethodGet, test.contractPath, http.StatusOK, response)
	}

	gotOperations := make([]string, 0, len(covered))
	for operation := range covered {
		gotOperations = append(gotOperations, operation)
	}
	sort.Strings(gotOperations)
	if wantOperations := contract.operations(t); !equalStrings(gotOperations, wantOperations) {
		t.Fatalf("successful HTTP operations = %v, OpenAPI operations = %v", gotOperations, wantOperations)
	}
}

func TestDashboardResponseAlwaysIncludesCompleteGatewayCertificate(t *testing.T) {
	contract := loadManagementOpenAPIContract(t)
	harness := newManagementReadContractHarness(t, contract)
	response := doRequest(t, harness.client, http.MethodGet, harness.server.URL+"/api/v1/dashboard", "", nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	var dashboard map[string]json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&dashboard); err != nil {
		t.Fatalf("decode Dashboard response: %v", err)
	}
	certificateJSON, exists := dashboard["gateway_certificate"]
	if !exists || bytes.Equal(certificateJSON, []byte("null")) {
		t.Fatal("Dashboard response omitted gateway_certificate")
	}
	var certificateFields map[string]json.RawMessage
	if err := json.Unmarshal(certificateJSON, &certificateFields); err != nil {
		t.Fatalf("decode Dashboard gateway_certificate: %v", err)
	}
	wantFields := []string{
		"tls_mode", "expires_at", "remaining_seconds", "level",
		"recent_renewal_failed", "recent_renewal_error_code",
	}
	if len(certificateFields) != len(wantFields) {
		t.Fatalf("Dashboard gateway_certificate fields = %v, want %v", certificateFields, wantFields)
	}
	for _, field := range wantFields {
		if _, exists := certificateFields[field]; !exists {
			t.Errorf("Dashboard gateway_certificate omitted %s", field)
		}
	}
	var certificate GatewayCertificate
	if err := json.Unmarshal(certificateJSON, &certificate); err != nil {
		t.Fatalf("decode typed Dashboard gateway_certificate: %v", err)
	}
	if certificate.TlsMode != GatewayCertificateTlsModePinned || certificate.ExpiresAt.IsZero() ||
		certificate.RemainingSeconds <= 0 || certificate.Level != HEALTHY || certificate.RecentRenewalFailed {
		t.Fatalf("Dashboard gateway_certificate = %#v", certificate)
	}
	if _, err := certificate.RecentRenewalErrorCode.Get(); err == nil {
		t.Fatal("Dashboard gateway_certificate unexpectedly included a renewal failure code")
	}
}

func TestManagementAllCSRFSecuredMutationsRejectWithoutSideEffects(t *testing.T) {
	contract := loadManagementOpenAPIContract(t)
	wantOperations := []string{
		"DELETE /services/{service_id}",
		"DELETE /tunnels/{tunnel_id}",
		"PATCH /services/{service_id}",
		"PATCH /tunnels/{tunnel_id}",
		"POST /auth/logout",
		"POST /services",
		"POST /services/{service_id}/disable",
		"POST /services/{service_id}/enable",
		"POST /tunnels",
		"POST /tunnels/{tunnel_id}/revoke",
		"POST /tunnels/{tunnel_id}/token/revoke",
		"POST /tunnels/{tunnel_id}/token/rotate",
	}
	if got := contract.csrfSecuredOperations(t); !equalStrings(got, wantOperations) {
		t.Fatalf("CSRF secured operations = %v, want %v", got, wantOperations)
	}

	harness := newServiceAPIHarness(t)
	created := createTunnelForTest(t, harness.tunnelAPIHarness, "csrf tunnel")
	if err := harness.budget.InitializeTunnel(created.Tunnel.Id, uint64(created.Tunnel.DesiredRevision), 0); err != nil {
		t.Fatalf("InitializeTunnel() error = %v", err)
	}
	service, serviceETag := createHTTPServiceForTest(t, harness, created.Tunnel.Id, "csrf service", "csrf.example.test")
	tunnelPath := "/api/v1/tunnels/" + created.Tunnel.Id
	servicePath := "/api/v1/services/" + service.Id

	assertRejectedAndUnchanged := func(
		name, method, actualPath, contractPath string,
		body any, ifMatch, snapshotPath string,
	) {
		t.Helper()
		before := readManagementSnapshot(t, harness.tunnelAPIHarness, snapshotPath)
		response := doMissingCSRFRequest(t, harness.tunnelAPIHarness, method, actualPath, body, ifMatch)
		assertContractAPIError(t, contract, method, contractPath, response, http.StatusForbidden, APIErrorCodeCSRFINVALID)
		after := readManagementSnapshot(t, harness.tunnelAPIHarness, snapshotPath)
		if before.etag != after.etag || !bytes.Equal(before.body, after.body) {
			t.Fatalf("%s changed state without CSRF: before ETag/body = %q/%s, after = %q/%s", name, before.etag, before.body, after.etag, after.body)
		}
	}

	assertRejectedAndUnchanged("create Tunnel", http.MethodPost, "/api/v1/tunnels", "/tunnels",
		CreateTunnelRequest{Name: "must not exist"}, "", "/api/v1/tunnels")
	assertRejectedAndUnchanged("patch Tunnel", http.MethodPatch, tunnelPath, "/tunnels/{tunnel_id}",
		map[string]any{"name": "must not change"}, `"1"`, tunnelPath)
	assertRejectedAndUnchanged("delete Tunnel", http.MethodDelete, tunnelPath, "/tunnels/{tunnel_id}",
		nil, `"1"`, tunnelPath)
	assertRejectedAndUnchanged("rotate Token", http.MethodPost, tunnelPath+"/token/rotate", "/tunnels/{tunnel_id}/token/rotate",
		nil, `"1"`, tunnelPath+"/token")
	assertRejectedAndUnchanged("revoke Token", http.MethodPost, tunnelPath+"/token/revoke", "/tunnels/{tunnel_id}/token/revoke",
		nil, `"1"`, tunnelPath+"/token")
	assertRejectedAndUnchanged("revoke Tunnel", http.MethodPost, tunnelPath+"/revoke", "/tunnels/{tunnel_id}/revoke",
		nil, `"1"`, tunnelPath)

	createServiceBody := map[string]any{
		"tunnel_id": created.Tunnel.Id,
		"name":      "must not exist",
		"origin": map[string]any{
			"scheme": "http", "host": "127.0.0.1", "port": 8081,
		},
		"exposure": map[string]any{"type": "http", "hostname": "must-not-exist.example.test"},
	}
	serviceListPath := "/api/v1/services?tunnel_id=" + created.Tunnel.Id
	assertRejectedAndUnchanged("create Service", http.MethodPost, "/api/v1/services", "/services",
		createServiceBody, `"1"`, serviceListPath)
	assertRejectedAndUnchanged("patch Service", http.MethodPatch, servicePath, "/services/{service_id}",
		map[string]any{"name": "must not change"}, serviceETag, servicePath)
	assertRejectedAndUnchanged("delete Service", http.MethodDelete, servicePath, "/services/{service_id}",
		nil, serviceETag, servicePath)
	assertRejectedAndUnchanged("disable Service", http.MethodPost, servicePath+"/disable", "/services/{service_id}/disable",
		nil, serviceETag, servicePath)

	disabledResponse := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPost, servicePath+"/disable", nil, serviceETag, true)
	var disabled Service
	decodeSuccess(t, disabledResponse, &disabled)
	disabledETag := disabledResponse.Header.Get("ETag")
	assertRejectedAndUnchanged("enable Service", http.MethodPost, servicePath+"/enable", "/services/{service_id}/enable",
		nil, disabledETag, servicePath)

	beforeLogout := readManagementSnapshot(t, harness.tunnelAPIHarness, "/api/v1/auth/me")
	logout := doMissingCSRFRequest(t, harness.tunnelAPIHarness, http.MethodPost, "/api/v1/auth/logout", nil, "")
	assertContractAPIError(t, contract, http.MethodPost, "/auth/logout", logout, http.StatusForbidden, APIErrorCodeCSRFINVALID)
	afterLogout := readManagementSnapshot(t, harness.tunnelAPIHarness, "/api/v1/auth/me")
	if !bytes.Equal(beforeLogout.body, afterLogout.body) {
		t.Fatalf("logout without CSRF destroyed or changed Session: before = %s, after = %s", beforeLogout.body, afterLogout.body)
	}
}

func TestManagementReachableErrorResponsesMatchOpenAPI(t *testing.T) {
	contract := loadManagementOpenAPIContract(t)
	harness := newTunnelAPIHarness(t)
	created := createTunnelForTest(t, harness, "error contract tunnel")
	path := "/api/v1/tunnels/" + created.Tunnel.Id
	covered := make(map[APIErrorCode]struct{})
	assertError := func(method, contractPath string, response *http.Response, status int, code APIErrorCode) {
		t.Helper()
		assertContractAPIError(t, contract, method, contractPath, response, status, code)
		covered[code] = struct{}{}
	}

	tests := []struct {
		name         string
		method       string
		actualPath   string
		contractPath string
		body         any
		ifMatch      string
		status       int
		code         APIErrorCode
	}{
		{name: "bad request", method: http.MethodPatch, actualPath: path, contractPath: "/tunnels/{tunnel_id}", body: map[string]any{"unknown": true}, ifMatch: `"1"`, status: http.StatusBadRequest, code: APIErrorCodeINVALIDREQUEST},
		{name: "not found", method: http.MethodGet, actualPath: "/api/v1/tunnels/tun_00000000000000000000000000", contractPath: "/tunnels/{tunnel_id}", status: http.StatusNotFound, code: APIErrorCodeRESOURCENOTFOUND},
		{name: "precondition failed", method: http.MethodPatch, actualPath: path, contractPath: "/tunnels/{tunnel_id}", body: map[string]any{"name": "stale"}, ifMatch: `"9"`, status: http.StatusPreconditionFailed, code: APIErrorCodeRESOURCEVERSIONCONFLICT},
		{name: "validation failed", method: http.MethodPatch, actualPath: path, contractPath: "/tunnels/{tunnel_id}", body: map[string]any{"name": nil}, ifMatch: `"1"`, status: http.StatusUnprocessableEntity, code: APIErrorCodeVALIDATIONFAILED},
		{name: "precondition required", method: http.MethodPatch, actualPath: path, contractPath: "/tunnels/{tunnel_id}", body: map[string]any{"name": "missing"}, status: http.StatusPreconditionRequired, code: APIErrorCodePRECONDITIONREQUIRED},
		{name: "invalid If-Match", method: http.MethodPatch, actualPath: path, contractPath: "/tunnels/{tunnel_id}", body: map[string]any{"name": "invalid tag"}, ifMatch: `W/"1"`, status: http.StatusBadRequest, code: APIErrorCodeINVALIDIFMATCH},
		{name: "invalid page token", method: http.MethodGet, actualPath: "/api/v1/tunnels?page_token=not-a-token", contractPath: "/tunnels", status: http.StatusBadRequest, code: APIErrorCodeINVALIDPAGETOKEN},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := doTunnelRequest(t, harness, test.method, test.actualPath, test.body, test.ifMatch, test.method != http.MethodGet)
			assertError(test.method, test.contractPath, response, test.status, test.code)
		})
	}

	unauthenticated := &http.Client{Transport: harness.server.Client().Transport}
	response, err := unauthenticated.Get(harness.server.URL + "/api/v1/auth/me")
	if err != nil {
		t.Fatalf("unauthenticated auth/me error = %v", err)
	}
	assertError(http.MethodGet, "/auth/me", response, http.StatusUnauthorized, APIErrorCodeSESSIONEXPIRED)

	response = postLogin(t, harness.client, harness.server.URL, "https://attacker.example", "admin", "correct horse battery staple")
	assertError(http.MethodPost, "/auth/login", response, http.StatusForbidden, APIErrorCodeORIGINNOTALLOWED)

	response = doMissingCSRFRequest(t, harness, http.MethodPost, "/api/v1/tunnels", CreateTunnelRequest{Name: "missing csrf"}, "")
	assertError(http.MethodPost, "/tunnels", response, http.StatusForbidden, APIErrorCodeCSRFINVALID)

	t.Run("setup required", func(t *testing.T) {
		store, err := sqlite.Open(context.Background(), t.TempDir())
		if err != nil {
			t.Fatalf("sqlite.Open() error = %v", err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Errorf("Store.Close() error = %v", err)
			}
		})
		server := httptest.NewUnstartedServer(nil)
		publicURL := "https://" + server.Listener.Addr().String()
		handler, err := NewHandler(HandlerOptions{
			Management: managementReadTestConfig(publicURL).Management,
			Store:      store,
			Logger:     slog.New(slog.NewJSONHandler(io.Discard, nil)),
		})
		if err != nil {
			t.Fatalf("NewHandler() error = %v", err)
		}
		server.Config.Handler = handler
		server.StartTLS()
		t.Cleanup(server.Close)
		response := postLogin(t, server.Client(), server.URL, publicURL, "admin", "correct horse battery staple")
		assertError(http.MethodPost, "/auth/login", response, http.StatusConflict, APIErrorCodeSETUPREQUIRED)
	})

	t.Run("conflict", func(t *testing.T) {
		serviceHarness := newServiceAPIHarness(t)
		tunnel := createTunnelForTest(t, serviceHarness.tunnelAPIHarness, "in-use contract tunnel")
		if err := serviceHarness.budget.InitializeTunnel(tunnel.Tunnel.Id, uint64(tunnel.Tunnel.DesiredRevision), 0); err != nil {
			t.Fatalf("InitializeTunnel() error = %v", err)
		}
		createHTTPServiceForTest(t, serviceHarness, tunnel.Tunnel.Id, "in-use service", "in-use.example.test")
		response := doTunnelRequest(
			t, serviceHarness.tunnelAPIHarness, http.MethodDelete,
			"/api/v1/tunnels/"+tunnel.Tunnel.Id, nil, `"1"`, true,
		)
		assertError(http.MethodDelete, "/tunnels/{tunnel_id}", response, http.StatusConflict, APIErrorCodeTUNNELINUSE)
	})

	t.Run("rate limited", func(t *testing.T) {
		for range loginPairFailureLimit {
			response := postLogin(t, harness.client, harness.server.URL, harness.publicURL, "rate-limited-admin", "wrong password")
			assertError(http.MethodPost, "/auth/login", response, http.StatusUnauthorized, APIErrorCodeAUTHENTICATIONFAILED)
		}
		response := postLogin(t, harness.client, harness.server.URL, harness.publicURL, "rate-limited-admin", "wrong password")
		assertError(http.MethodPost, "/auth/login", response, http.StatusTooManyRequests, APIErrorCodeRATELIMITED)
	})

	t.Run("internal error", func(t *testing.T) {
		// 省略 Dashboard owner 是 HandlerOptions 支持的可控装配状态；API Fence 必须返回
		// OpenAPI 声明的结构化 500，不能落入 nil owner 或伪造生产错误。
		response := doTunnelRequest(t, harness, http.MethodGet, "/api/v1/dashboard", nil, "", false)
		assertError(http.MethodGet, "/dashboard", response, http.StatusInternalServerError, APIErrorCodeINTERNALERROR)
	})

	t.Run("credential unavailable and Tunnel revoked", func(t *testing.T) {
		credentialHarness := newTunnelAPIHarness(t)
		tunnel := createTunnelForTest(t, credentialHarness, "revoked error contract tunnel")
		tunnelPath := "/api/v1/tunnels/" + tunnel.Tunnel.Id
		revokedToken := doTunnelRequest(t, credentialHarness, http.MethodPost, tunnelPath+"/token/revoke", nil, `"1"`, true)
		tunnelETag := revokedToken.Header.Get("ETag")
		contract.assertResponse(t, http.MethodPost, "/tunnels/{tunnel_id}/token/revoke", http.StatusOK, revokedToken)

		response := doTunnelRequest(t, credentialHarness, http.MethodGet, tunnelPath+"/token", nil, "", false)
		assertError(http.MethodGet, "/tunnels/{tunnel_id}/token", response, http.StatusConflict, APIErrorCodeCONNECTIONTOKENUNAVAILABLE)

		revokedTunnel := doTunnelRequest(t, credentialHarness, http.MethodPost, tunnelPath+"/revoke", nil, tunnelETag, true)
		tunnelETag = revokedTunnel.Header.Get("ETag")
		contract.assertResponse(t, http.MethodPost, "/tunnels/{tunnel_id}/revoke", http.StatusOK, revokedTunnel)

		response = doTunnelRequest(t, credentialHarness, http.MethodPost, tunnelPath+"/token/rotate", nil, tunnelETag, true)
		assertError(http.MethodPost, "/tunnels/{tunnel_id}/token/rotate", response, http.StatusConflict, APIErrorCodeTUNNELREVOKED)
	})

	handler := &ManagementHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	for _, test := range []struct {
		name   string
		err    error
		status int
		code   APIErrorCode
	}{
		{name: "route conflict", err: application.ErrServiceExposureConflict, status: http.StatusConflict, code: APIErrorCodeROUTECONFLICT},
		{name: "TCP port unavailable", err: tcpport.ErrPoolExhausted, status: http.StatusConflict, code: APIErrorCodeTCPPORTUNAVAILABLE},
		{name: "Tunnel Service limit", err: serversnapshot.ErrServiceLimit, status: http.StatusUnprocessableEntity, code: APIErrorCodeTUNNELSERVICELIMIT},
		{name: "Snapshot too large", err: serversnapshot.ErrSnapshotTooLarge, status: http.StatusUnprocessableEntity, code: APIErrorCodeSNAPSHOTTOOLARGE},
	} {
		t.Run(test.name+" mapping", func(t *testing.T) {
			failure := handler.mapServiceError(context.Background(), nil, test.err)
			if failure.status != test.status || failure.code != test.code {
				t.Fatalf("mapServiceError() = %d/%s, want %d/%s", failure.status, failure.code, test.status, test.code)
			}
			covered[failure.code] = struct{}{}
		})
	}
	t.Run("runtime convergence mapping", func(t *testing.T) {
		failure := handler.mapTunnelError(context.Background(), nil, application.ErrTunnelRuntimeConvergence)
		if failure.status != http.StatusConflict || failure.code != APIErrorCodeRUNTIMECONVERGENCEFAILED {
			t.Fatalf("mapTunnelError() = %d/%s, want 409/%s", failure.status, failure.code, APIErrorCodeRUNTIMECONVERGENCEFAILED)
		}
		covered[failure.code] = struct{}{}
	})

	// V0.1 只有单一管理员身份，没有角色或权限边界；实际的 403 只细分 Origin 与 CSRF。
	// FORBIDDEN 是冻结给未来权限拒绝的保留码，不能为了覆盖率伪造生产路由行为。
	unreachable := map[APIErrorCode]string{
		APIErrorCodeFORBIDDEN: "V0.1 single-admin API has no role boundary",
	}
	for code, reason := range unreachable {
		if reason == "" {
			t.Fatalf("unreachable API error code %s has no frozen-boundary reason", code)
		}
		covered[code] = struct{}{}
	}

	coveredCodes := make([]string, 0, len(covered))
	for code := range covered {
		if !code.Valid() {
			t.Errorf("covered unknown generated API error code %q", code)
		}
		coveredCodes = append(coveredCodes, string(code))
	}
	sort.Strings(coveredCodes)
	if contractCodes := contract.apiErrorCodes(t); !equalStrings(coveredCodes, contractCodes) {
		t.Fatalf("executed/unreachable API error codes = %v, OpenAPI enum = %v", coveredCodes, contractCodes)
	}
}

func loadManagementOpenAPIContract(t *testing.T) *managementOpenAPIContract {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "api", "openapi", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read OpenAPI contract: %v", err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode OpenAPI contract: %v", err)
	}
	return &managementOpenAPIContract{document: document}
}

func (contract *managementOpenAPIContract) assertResponse(t *testing.T, method, path string, expectedStatus int, response *http.Response) []byte {
	t.Helper()
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s %s response: %v", method, path, err)
	}
	if response.StatusCode != expectedStatus {
		t.Fatalf("%s %s status = %d, want %d; body = %s", method, path, response.StatusCode, expectedStatus, responseBody)
	}
	definition := contract.response(t, method, path, expectedStatus)

	for name, rawHeader := range optionalMap(t, definition, "headers") {
		header := contract.resolve(t, object(t, rawHeader, "response header "+name))
		value := response.Header.Get(name)
		if value == "" {
			t.Errorf("%s %s status %d missing declared %s header", method, path, response.StatusCode, name)
			continue
		}
		headerSchema := objectField(t, header, "schema")
		contract.validateSchema(t, method+" "+path+" header "+name, headerSchema, contract.headerInstance(t, name, headerSchema, value))
	}

	content := optionalMap(t, definition, "content")
	if len(content) == 0 {
		if len(responseBody) != 0 {
			t.Errorf("%s %s status %d body = %q, want empty body", method, path, response.StatusCode, responseBody)
		}
		if contentType := response.Header.Get("Content-Type"); contentType != "" {
			t.Errorf("%s %s status %d Content-Type = %q, want absent", method, path, response.StatusCode, contentType)
		}
		return responseBody
	}

	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("%s %s status %d Content-Type = %q", method, path, response.StatusCode, response.Header.Get("Content-Type"))
	}
	if mediaType == "application/x-ndjson" {
		media := object(t, content[mediaType], mediaType+" response")
		contract.validateSchema(t, method+" "+path+" response", objectField(t, media, "schema"), string(responseBody))
		return responseBody
	}
	if mediaType != "application/json" {
		t.Fatalf("%s %s status %d Content-Type = %q, want a declared response media type", method, path, response.StatusCode, response.Header.Get("Content-Type"))
	}
	media := object(t, content["application/json"], "application/json response")
	var instance any
	if err := json.Unmarshal(responseBody, &instance); err != nil {
		t.Fatalf("decode %s %s status %d response JSON: %v; body = %s", method, path, response.StatusCode, err, responseBody)
	}
	contract.validateSchema(t, method+" "+path+" response", objectField(t, media, "schema"), instance)
	return responseBody
}

func (contract *managementOpenAPIContract) response(t *testing.T, method, path string, status int) map[string]any {
	t.Helper()
	paths := objectField(t, contract.document, "paths")
	pathItem := object(t, paths[path], "OpenAPI path "+path)
	operation := object(t, pathItem[strings.ToLower(method)], method+" "+path)
	responses := objectField(t, operation, "responses")
	definition, exists := responses[strconv.Itoa(status)]
	if !exists {
		t.Fatalf("OpenAPI does not declare %s %s status %d", method, path, status)
	}
	return contract.resolve(t, object(t, definition, fmt.Sprintf("%s %s status %d", method, path, status)))
}

func (contract *managementOpenAPIContract) resolve(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	ref, exists := value["$ref"].(string)
	if !exists {
		return value
	}
	if !strings.HasPrefix(ref, "#/") {
		t.Fatalf("unsupported non-local OpenAPI reference %q", ref)
	}
	var current any = contract.document
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		current = object(t, current, "OpenAPI reference "+ref)[part]
	}
	return object(t, current, "OpenAPI reference "+ref)
}

func (contract *managementOpenAPIContract) validateSchema(t *testing.T, name string, schema map[string]any, instance any) {
	t.Helper()
	wrapper := map[string]any{
		"$schema":    "https://json-schema.org/draft/2020-12/schema",
		"$ref":       "#/$defs/assertion",
		"$defs":      map[string]any{"assertion": schema},
		"components": contract.document["components"],
	}
	encoded, err := json.Marshal(wrapper)
	if err != nil {
		t.Fatalf("encode %s schema: %v", name, err)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("decode %s schema: %v", name, err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	const schemaURL = "https://xtunnel.test/management-response.json"
	if err := compiler.AddResource(schemaURL, document); err != nil {
		t.Fatalf("register %s schema: %v", name, err)
	}
	compiled, err := compiler.Compile(schemaURL)
	if err != nil {
		t.Fatalf("compile %s schema: %v", name, err)
	}
	if err := compiled.Validate(instance); err != nil {
		t.Errorf("%s does not match OpenAPI: %v", name, err)
	}
}

func (contract *managementOpenAPIContract) headerInstance(t *testing.T, name string, schema map[string]any, value string) any {
	t.Helper()
	schema = contract.resolve(t, schema)
	switch schema["type"] {
	case "integer":
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			t.Fatalf("parse integer response header %s=%q: %v", name, value, err)
		}
		return parsed
	case "number":
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			t.Fatalf("parse number response header %s=%q: %v", name, value, err)
		}
		return parsed
	case "boolean":
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			t.Fatalf("parse boolean response header %s=%q: %v", name, value, err)
		}
		return parsed
	default:
		return value
	}
}

func (contract *managementOpenAPIContract) csrfSecuredOperations(t *testing.T) []string {
	t.Helper()
	var operations []string
	for path, rawPath := range objectField(t, contract.document, "paths") {
		pathItem := object(t, rawPath, "OpenAPI path "+path)
		for _, method := range []string{"delete", "patch", "post", "put"} {
			rawOperation, exists := pathItem[method]
			if !exists {
				continue
			}
			operation := object(t, rawOperation, strings.ToUpper(method)+" "+path)
			for _, rawSecurity := range optionalSlice(t, operation, "security") {
				if _, exists := object(t, rawSecurity, "security requirement")["csrfToken"]; exists {
					operations = append(operations, strings.ToUpper(method)+" "+path)
					break
				}
			}
		}
	}
	sort.Strings(operations)
	return operations
}

func (contract *managementOpenAPIContract) apiErrorCodes(t *testing.T) []string {
	t.Helper()
	components := objectField(t, contract.document, "components")
	schemas := objectField(t, components, "schemas")
	definition := object(t, schemas["APIErrorCode"], "APIErrorCode schema")
	rawCodes := optionalSlice(t, definition, "enum")
	codes := make([]string, 0, len(rawCodes))
	for _, rawCode := range rawCodes {
		code, ok := rawCode.(string)
		if !ok {
			t.Fatalf("APIErrorCode enum member has type %T, want string", rawCode)
		}
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}

func (contract *managementOpenAPIContract) operations(t *testing.T) []string {
	t.Helper()
	var operations []string
	for path, rawPath := range objectField(t, contract.document, "paths") {
		pathItem := object(t, rawPath, "OpenAPI path "+path)
		for _, method := range []string{"delete", "get", "patch", "post", "put"} {
			if _, exists := pathItem[method]; exists {
				operations = append(operations, strings.ToUpper(method)+" "+path)
			}
		}
	}
	sort.Strings(operations)
	return operations
}

type managementReadContractHarness struct {
	server *httptest.Server
	client *http.Client
}

func newManagementReadContractHarness(t *testing.T, contract *managementOpenAPIContract) *managementReadContractHarness {
	t.Helper()
	store, err := sqlite.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	if err := store.CreateFirstAdmin(context.Background(), "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin() error = %v", err)
	}

	server := httptest.NewUnstartedServer(nil)
	publicURL := "https://" + server.Listener.Addr().String()
	config := managementReadTestConfig(publicURL)
	system, err := application.NewSystemReadService(
		"v0.1.0", time.Now().Add(-time.Minute), config,
		func(context.Context) application.SystemHealthCheckResult {
			return application.SystemHealthCheckResult{Name: "sqlite", Status: application.SystemHealthReady}
		},
	)
	if err != nil {
		t.Fatalf("NewSystemReadService() error = %v", err)
	}
	dashboard := application.NewDashboardService(
		dashboardTunnelReaderFake{}, dashboardServiceReaderFake{},
		dashboardStatusOwnerFake{status: application.DashboardServerStatusReady},
		dashboardUsageReaderFake{},
		serverrecenterror.NewOwner(),
		dashboardCertificateReaderFake,
	)
	handler, err := NewHandler(HandlerOptions{
		Management:     config.Management,
		Store:          store,
		System:         system,
		SecurityAudits: application.NewSecurityAuditQueryService(store),
		Dashboard:      dashboard,
		Logger:         slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	server.Config.Handler = handler
	server.StartTLS()
	t.Cleanup(server.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New() error = %v", err)
	}
	client := server.Client()
	client.Jar = jar
	login := postLogin(t, client, server.URL, publicURL, "admin", "correct horse battery staple")
	contract.assertResponse(t, http.MethodPost, "/auth/login", http.StatusOK, login)
	return &managementReadContractHarness{server: server, client: client}
}

type managementSnapshot struct {
	etag string
	body []byte
}

func readManagementSnapshot(t *testing.T, harness *tunnelAPIHarness, path string) managementSnapshot {
	t.Helper()
	response := doTunnelRequest(t, harness, http.MethodGet, path, nil, "", false)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read snapshot %s: %v", path, err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("snapshot %s status/body = %d/%s", path, response.StatusCode, body)
	}
	return managementSnapshot{etag: response.Header.Get("ETag"), body: body}
}

func doMissingCSRFRequest(t *testing.T, harness *tunnelAPIHarness, method, path string, body any, ifMatch string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("json.Marshal(%s %s) error = %v", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, harness.server.URL+path, reader)
	if err != nil {
		t.Fatalf("http.NewRequest(%s %s) error = %v", method, path, err)
	}
	request.Header.Set("Origin", harness.publicURL)
	if body != nil {
		if method == http.MethodPatch {
			request.Header.Set("Content-Type", "application/merge-patch+json")
		} else {
			request.Header.Set("Content-Type", "application/json")
		}
	}
	if ifMatch != "" {
		request.Header.Set("If-Match", ifMatch)
	}
	response, err := harness.client.Do(request)
	if err != nil {
		t.Fatalf("client.Do(%s %s) error = %v", method, path, err)
	}
	return response
}

func assertContractAPIError(
	t *testing.T,
	contract *managementOpenAPIContract,
	method, path string,
	response *http.Response,
	status int,
	code APIErrorCode,
) {
	t.Helper()
	if response.StatusCode != status {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("%s %s status = %d, want %d; body = %s", method, path, response.StatusCode, status, body)
	}
	body := contract.assertResponse(t, method, path, status, response)
	var failure ErrorResponse
	if err := json.Unmarshal(body, &failure); err != nil {
		t.Fatalf("decode %s %s API error: %v", method, path, err)
	}
	if failure.Error.Code != code || failure.Error.RequestId == "" {
		t.Fatalf("%s %s API error = %#v, want %s with request_id", method, path, failure.Error, code)
	}
}

func objectField(t *testing.T, objectValue map[string]any, name string) map[string]any {
	t.Helper()
	return object(t, objectValue[name], name)
}

func object(t *testing.T, value any, name string) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s has type %T, want object", name, value)
	}
	return result
}

func optionalMap(t *testing.T, objectValue map[string]any, name string) map[string]any {
	t.Helper()
	value, exists := objectValue[name]
	if !exists {
		return nil
	}
	return object(t, value, name)
}

func optionalSlice(t *testing.T, objectValue map[string]any, name string) []any {
	t.Helper()
	value, exists := objectValue[name]
	if !exists {
		return nil
	}
	result, ok := value.([]any)
	if !ok {
		t.Fatalf("%s has type %T, want array", name, value)
	}
	return result
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
