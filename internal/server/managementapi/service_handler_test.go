package managementapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lifei6671/xtunnel/internal/application"
	"github.com/lifei6671/xtunnel/internal/healthbudget"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/repository"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
	serversnapshot "github.com/lifei6671/xtunnel/internal/server/snapshot"
	serverstatus "github.com/lifei6671/xtunnel/internal/server/status"
	"github.com/lifei6671/xtunnel/internal/tcpport"
)

func TestServiceAPIHTTPExposureLifecycleOverTLS(t *testing.T) {
	harness := newServiceAPIHarness(t)
	tunnel := createTunnelForTest(t, harness.tunnelAPIHarness, "service API")
	if err := harness.budget.InitializeTunnel(tunnel.Tunnel.Id, uint64(tunnel.Tunnel.DesiredRevision), 0); err != nil {
		t.Fatalf("InitializeTunnel() error = %v", err)
	}

	createBody := map[string]any{
		"tunnel_id": tunnel.Tunnel.Id,
		"name":      "web",
		"origin": map[string]any{
			"scheme": "https", "host": "origin.example.test", "port": 8443,
			"tls_verify": true, "tls_server_name": "origin.example.test",
		},
		"exposure": map[string]any{"type": "http", "hostname": "public.example.test"},
	}
	createdResponse := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPost, "/api/v1/services", createBody, `"1"`, true)
	if createdResponse.StatusCode != http.StatusCreated || !strings.HasPrefix(createdResponse.Header.Get("ETag"), `"service:`) {
		body, _ := io.ReadAll(createdResponse.Body)
		createdResponse.Body.Close()
		t.Fatalf("create Service status/ETag/body = %d/%q/%s", createdResponse.StatusCode, createdResponse.Header.Get("ETag"), body)
	}
	etag := createdResponse.Header.Get("ETag")
	var created Service
	decodeSuccess(t, createdResponse, &created)
	if created.Status != ServiceStatusTUNNELOFFLINE || created.Usage.Availability != UsageSummaryAvailabilityUNAVAILABLE ||
		!created.Usage.ConnectionsToday.IsNull() || !created.Usage.IngressBytesToday.IsNull() || !created.Usage.EgressBytesToday.IsNull() {
		t.Fatalf("created status/usage = %s/%#v", created.Status, created.Usage)
	}
	httpsOrigin, err := created.Origin.AsHTTPSOrigin()
	if err != nil || !httpsOrigin.TlsVerify || httpsOrigin.TlsServerName == nil || *httpsOrigin.TlsServerName != "origin.example.test" {
		t.Fatalf("created HTTPS origin = %#v, error = %v", httpsOrigin, err)
	}
	exposureValue, err := created.Exposure.Get()
	if err != nil {
		t.Fatalf("created Exposure.Get() error = %v", err)
	}
	httpExposure, err := exposureValue.AsHTTPExposure()
	if err != nil || httpExposure.PathPrefix != "/" || !httpExposure.PreserveHost {
		t.Fatalf("created HTTP exposure = %#v, error = %v", httpExposure, err)
	}

	unauthenticated := &http.Client{Transport: harness.server.Client().Transport}
	unauthorized, err := unauthenticated.Get(harness.server.URL + "/api/v1/services/" + created.Id)
	if err != nil {
		t.Fatalf("unauthenticated GET error = %v", err)
	}
	assertAPIError(t, unauthorized, http.StatusUnauthorized, APIErrorCodeSESSIONEXPIRED)

	missingCSRF := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPatch, "/api/v1/services/"+created.Id, map[string]any{"name": "web v2"}, etag, false)
	assertAPIError(t, missingCSRF, http.StatusForbidden, APIErrorCodeORIGINNOTALLOWED)

	patchedResponse := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPatch, "/api/v1/services/"+created.Id,
		map[string]any{"name": "web v2", "exposure": map[string]any{"path_prefix": "/api"}}, etag, true)
	var patched Service
	decodeSuccess(t, patchedResponse, &patched)
	if patched.Name != "web v2" || patchedResponse.Header.Get("ETag") == etag {
		t.Fatalf("patched Service name/ETag = %q/%q", patched.Name, patchedResponse.Header.Get("ETag"))
	}
	etag = patchedResponse.Header.Get("ETag")

	listedResponse := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodGet, "/api/v1/services?tunnel_id="+tunnel.Tunnel.Id, nil, "", false)
	var listed ServiceList
	decodeSuccess(t, listedResponse, &listed)
	if len(listed.Items) != 1 || listed.Items[0].Id != created.Id {
		t.Fatalf("listed Services = %#v", listed.Items)
	}

	disabledResponse := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPost, "/api/v1/services/"+created.Id+"/disable", nil, etag, true)
	var disabled Service
	decodeSuccess(t, disabledResponse, &disabled)
	if disabled.Enabled || disabled.Status != ServiceStatusDISABLED {
		t.Fatalf("disabled Service = enabled:%t status:%s", disabled.Enabled, disabled.Status)
	}
	disabledETag := disabledResponse.Header.Get("ETag")
	if disabledETag == etag {
		t.Fatal("disable did not advance Service ETag")
	}

	enabledResponse := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPost, "/api/v1/services/"+created.Id+"/enable", nil, disabledETag, true)
	var enabled Service
	decodeSuccess(t, enabledResponse, &enabled)
	if !enabled.Enabled || enabled.Status != ServiceStatusTUNNELOFFLINE || enabledResponse.Header.Get("ETag") == disabledETag {
		t.Fatalf("enabled Service = enabled:%t status:%s ETag:%q", enabled.Enabled, enabled.Status, enabledResponse.Header.Get("ETag"))
	}

	deleted := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodDelete, "/api/v1/services/"+created.Id, nil, enabledResponse.Header.Get("ETag"), true)
	if deleted.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(deleted.Body)
		deleted.Body.Close()
		t.Fatalf("delete Service status/body = %d/%s", deleted.StatusCode, body)
	}
	deleted.Body.Close()
}

func TestServiceAPIRejectsNestedUnknownFieldsOverTLS(t *testing.T) {
	harness := newServiceAPIHarness(t)
	tunnel := createTunnelForTest(t, harness.tunnelAPIHarness, "service unknown fields")
	if err := harness.budget.InitializeTunnel(tunnel.Tunnel.Id, uint64(tunnel.Tunnel.DesiredRevision), 0); err != nil {
		t.Fatalf("InitializeTunnel() error = %v", err)
	}
	validCreateBody := func() map[string]any {
		return map[string]any{
			"tunnel_id": tunnel.Tunnel.Id,
			"name":      "strict service",
			"origin": map[string]any{
				"scheme": "http", "host": "origin.example.test", "port": 8080,
			},
			"exposure": map[string]any{"type": "http", "hostname": "strict.example.test"},
		}
	}
	createTests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "origin", mutate: func(body map[string]any) { body["origin"].(map[string]any)["unknown"] = true }},
		{name: "proxy options", mutate: func(body map[string]any) {
			body["proxy_options"] = map[string]any{"unknown": true}
		}},
		{name: "health", mutate: func(body map[string]any) {
			body["health"] = map[string]any{"type": "TCP", "unknown": true}
		}},
		{name: "exposure", mutate: func(body map[string]any) { body["exposure"].(map[string]any)["unknown"] = true }},
	}
	for _, test := range createTests {
		t.Run("create "+test.name, func(t *testing.T) {
			body := validCreateBody()
			test.mutate(body)
			response := doTunnelRequest(
				t, harness.tunnelAPIHarness, http.MethodPost, "/api/v1/services", body, `"1"`, true,
			)
			assertAPIError(t, response, http.StatusBadRequest, APIErrorCodeINVALIDREQUEST)
		})
	}

	createdResponse := doTunnelRequest(
		t, harness.tunnelAPIHarness, http.MethodPost, "/api/v1/services", validCreateBody(), `"1"`, true,
	)
	if createdResponse.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(createdResponse.Body)
		createdResponse.Body.Close()
		t.Fatalf("create valid Service status/body = %d/%s", createdResponse.StatusCode, body)
	}
	etag := createdResponse.Header.Get("ETag")
	var created Service
	decodeSuccess(t, createdResponse, &created)
	patches := []struct {
		name string
		body map[string]any
	}{
		{name: "origin", body: map[string]any{"origin": map[string]any{"unknown": true}}},
		{name: "proxy options", body: map[string]any{"proxy_options": map[string]any{"unknown": true}}},
		{name: "health", body: map[string]any{"health": map[string]any{"unknown": true}}},
		{name: "exposure", body: map[string]any{"exposure": map[string]any{"unknown": true}}},
	}
	for _, test := range patches {
		t.Run("patch "+test.name, func(t *testing.T) {
			response := doTunnelRequest(
				t, harness.tunnelAPIHarness, http.MethodPatch, "/api/v1/services/"+created.Id, test.body, etag, true,
			)
			assertAPIError(t, response, http.StatusBadRequest, APIErrorCodeINVALIDREQUEST)
		})
	}
}

func TestServiceETagRoundTripAndBinding(t *testing.T) {
	view := application.ServiceView{Service: repository.Service{ID: "svc_01ARZ3NDEKTSV4RRFFQ69G5FAV", TunnelID: "tun_01ARZ3NDEKTSV4RRFFQ69G5FAV", Version: 7}, TunnelVersion: 11}
	etag := serviceETag(view)
	identity, err := parseServiceIfMatch(etag, view.Service.ID)
	if err != nil || identity.tunnelID != view.Service.TunnelID || identity.serviceVersion != 7 || identity.tunnelVersion != 11 {
		t.Fatalf("parseServiceIfMatch(%q) = %#v, %v", etag, identity, err)
	}
	for _, value := range []string{"W/" + etag, `"service:svc_other:tun_01ARZ3NDEKTSV4RRFFQ69G5FAV:7:11"`, `"7"`, `*`} {
		if _, err := parseServiceIfMatch(value, view.Service.ID); err == nil {
			t.Fatalf("parseServiceIfMatch(%q) accepted invalid ETag", value)
		}
	}
}

func TestServiceUnionRejectsUnknownAndOverflowFields(t *testing.T) {
	var origin OriginInput
	if err := json.Unmarshal([]byte(`{"scheme":"http","host":"origin.example","port":80,"unknown":true}`), &origin); err != nil {
		t.Fatalf("json.Unmarshal(origin) error = %v", err)
	}
	if _, err := serviceOriginInput(origin, nil); err == nil {
		t.Fatal("serviceOriginInput() accepted unknown union field")
	}

	var exposure ExposureInput
	if err := json.Unmarshal([]byte(`{"type":"tcp","public_port":65536}`), &exposure); err != nil {
		t.Fatalf("json.Unmarshal(exposure) error = %v", err)
	}
	if _, err := createServiceExposure(exposure); err == nil {
		t.Fatal("createServiceExposure() accepted overflowing public_port")
	}
}

func TestMapServiceErrorContract(t *testing.T) {
	handler := &ManagementHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	tests := []struct {
		name   string
		err    error
		status int
		code   APIErrorCode
	}{
		{name: "TCP pool exhausted", err: tcpport.ErrPoolExhausted, status: 409, code: APIErrorCodeTCPPORTUNAVAILABLE},
		{name: "Exposure conflict", err: application.ErrServiceExposureConflict, status: 409, code: APIErrorCodeROUTECONFLICT},
		{name: "Service limit", err: serversnapshot.ErrServiceLimit, status: 422, code: APIErrorCodeTUNNELSERVICELIMIT},
		{name: "Health target budget", err: healthbudget.ErrTargetCapacity, status: 422, code: APIErrorCodeTUNNELSERVICELIMIT},
		{name: "Snapshot too large", err: serversnapshot.ErrSnapshotTooLarge, status: 422, code: APIErrorCodeSNAPSHOTTOOLARGE},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			failure := handler.mapServiceError(context.Background(), nil, test.err)
			if failure.status != test.status || failure.code != test.code {
				t.Fatalf("mapServiceError() = %d/%s, want %d/%s", failure.status, failure.code, test.status, test.code)
			}
		})
	}
}

type serviceAPIHarness struct {
	*tunnelAPIHarness
	budget *healthbudget.Manager
}

type serviceSnapshotGateFake struct{}

func (serviceSnapshotGateFake) Validate(string, int64, []repository.Service) error { return nil }

type serviceSnapshotNotifierFake struct{}

func (serviceSnapshotNotifierFake) MarkDirty(string) error { return nil }

type serviceRouteNotifierFake struct{}

func (serviceRouteNotifierFake) MarkDirty(uint64) {}

type serviceRuntimeFake struct{}

func (serviceRuntimeFake) RuntimeStatusSnapshots() []serverruntime.SessionStatusSnapshot { return nil }

type serviceLimitsFake struct{}

func (serviceLimitsFake) Snapshot() serverlimits.Snapshot {
	return serverlimits.Snapshot{ActiveByService: make(map[serverlimits.ConnectionService]uint64)}
}

type serviceApplyFailuresFake struct{}

func (serviceApplyFailuresFake) ServiceApplyFailure(string, uint64) *serverstatus.ApplyFailure {
	return nil
}

func newServiceAPIHarness(t *testing.T) *serviceAPIHarness {
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
	protector, err := application.NewAES256GCMTokenProtector(bytes.Repeat([]byte{0x5a}, 32))
	if err != nil {
		t.Fatalf("NewAES256GCMTokenProtector() error = %v", err)
	}
	tokens := application.NewConnectionTokenService(store, protector)
	runtime := &tunnelRuntimeFake{}
	endpoint := &protocolv1.GatewayEndpoint{Host: "gateway.example.test", Port: 443}
	trust := &protocolv1.TlsTrustDescriptor{Mode: &protocolv1.TlsTrustDescriptor_PublicCa{PublicCa: &protocolv1.PublicCATrust{}}}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	audit := application.NewSecurityAuditWriter(store, logger)
	budget, err := healthbudget.New(healthbudget.Options{MaxTargetsPerTunnel: 100, MaxTargetsGlobal: 1000})
	if err != nil {
		t.Fatalf("healthbudget.New() error = %v", err)
	}
	policy, err := tcpport.New(20000, 20100, nil)
	if err != nil {
		t.Fatalf("tcpport.New() error = %v", err)
	}
	serviceOwner := application.NewServiceManagementService(store, serviceSnapshotGateFake{}, serviceSnapshotNotifierFake{}, budget)
	services := application.NewServiceAPIService(
		serviceOwner, policy, serviceRouteNotifierFake{}, serviceRuntimeFake{}, serviceLimitsFake{}, serviceApplyFailuresFake{},
	)

	server := httptest.NewUnstartedServer(nil)
	publicURL := "https://" + server.Listener.Addr().String()
	handler, err := NewHandler(HandlerOptions{
		Management: serverconfig.Management{PublicURL: publicURL}, Store: store,
		Tunnels:         application.NewTunnelManagementService(store, tokens, runtime, endpoint, trust, 1000),
		Credentials:     application.NewCredentialLifecycleService(tokens, audit),
		TunnelLifecycle: application.NewTunnelLifecycleService(store, audit, runtime),
		Services:        services,
		Logger:          logger,
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
	if login.StatusCode != http.StatusOK {
		login.Body.Close()
		t.Fatalf("login status = %d, want 200", login.StatusCode)
	}
	var session AuthSession
	decodeSuccess(t, login, &session)
	return &serviceAPIHarness{tunnelAPIHarness: &tunnelAPIHarness{
		server: server, client: client, publicURL: publicURL, csrf: session.CsrfToken,
		runtime: runtime, store: store,
	}, budget: budget}
}
