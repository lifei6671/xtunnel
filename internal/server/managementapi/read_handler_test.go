package managementapi

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/application"
	"github.com/lifei6671/xtunnel/internal/repository"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	serverrecenterror "github.com/lifei6671/xtunnel/internal/server/recenterror"
	serverstatus "github.com/lifei6671/xtunnel/internal/server/status"
)

type dashboardTunnelReaderFake struct{ views []application.TunnelView }

func (fake dashboardTunnelReaderFake) List(context.Context) ([]application.TunnelView, error) {
	return append([]application.TunnelView(nil), fake.views...), nil
}

type dashboardServiceReaderFake struct{ views []application.ServiceView }

func (fake dashboardServiceReaderFake) ListAll(context.Context) ([]application.ServiceView, error) {
	return append([]application.ServiceView(nil), fake.views...), nil
}

type dashboardUsageReaderFake struct{ totals repository.UsageTotals }

func (fake dashboardUsageReaderFake) UsageToday(context.Context, time.Time) (repository.UsageTotals, error) {
	return fake.totals, nil
}

type dashboardStatusOwnerFake struct {
	status application.DashboardServerStatus
}

var dashboardCertificateReaderFake = application.DashboardGatewayCertificateReaderFunc(
	func(context.Context) (application.DashboardGatewayCertificateSource, error) {
		return application.DashboardGatewayCertificateSource{
			TLSMode: "pinned", ExpiryUnixSeconds: time.Date(2027, time.August, 30, 0, 0, 0, 0, time.UTC).Unix(),
		}, nil
	},
)

func (fake dashboardStatusOwnerFake) DashboardServerStatus(context.Context) (application.DashboardServerStatus, error) {
	return fake.status, nil
}

func TestManagementReadAPIsUseAuthenticatedFrozenProjections(t *testing.T) {
	store, err := sqlite.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
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
	recentErrors := serverrecenterror.NewOwner()
	recentRequestID := "req_01K4Z3JMESEMR8E7Z8AC9PKYYJ"
	recentOccurredAt := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	if err := recentErrors.Publish(serverrecenterror.Record{
		Code: serverrecenterror.CodeOriginDown, OccurredAt: recentOccurredAt, RequestID: &recentRequestID,
	}); err != nil {
		t.Fatalf("publish recent error: %v", err)
	}
	dashboard := application.NewDashboardService(
		dashboardTunnelReaderFake{views: []application.TunnelView{{
			Status: serverstatus.TunnelStatusOnline, ConnectorsOnline: 2, ActiveConnections: 7,
		}}},
		dashboardServiceReaderFake{views: []application.ServiceView{
			{Status: serverstatus.ServiceStatusReady},
			{Status: serverstatus.ServiceStatusApplyFailed},
			{Status: serverstatus.ServiceStatusOriginUnhealthy},
		}},
		dashboardStatusOwnerFake{status: application.DashboardServerStatusReady},
		dashboardUsageReaderFake{totals: repository.UsageTotals{Connections: 9, IngressBytes: 10, EgressBytes: 11}},
		recentErrors,
		dashboardCertificateReaderFake,
	)
	handler, err := NewHandler(HandlerOptions{
		Management: config.Management, Store: store, System: system,
		SecurityAudits: application.NewSecurityAuditQueryService(store), Dashboard: dashboard,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
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
	var session AuthSession
	decodeSuccess(t, login, &session)

	for _, eventID := range []string{"evt_01J00000000000000000000001", "evt_01J00000000000000000000002"} {
		event := repository.SecurityAuditEvent{
			EventID: eventID, OperationID: "op_" + eventID[4:],
			Event:     repository.SecurityAuditEventOperationResult,
			Action:    repository.SecurityAuditActionTokenReveal,
			ActorType: repository.SecurityAuditActorAdmin, ActorID: session.Admin.Id,
			SourceIP: "127.0.0.1", ResourceType: repository.SecurityAuditResourceTunnelToken,
			ResourceID: "tun_01J00000000000000000000000", Result: repository.SecurityAuditResultSucceeded,
			RequestID:         "req_01J00000000000000000000000",
			BeforeStateDigest: bytes.Repeat([]byte{0xab}, 32), OccurredAt: 1_777_777_777,
		}
		if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
			return transaction.SecurityAuditEvents().Append(context.Background(), event)
		}); err != nil {
			t.Fatalf("append audit event error = %v", err)
		}
	}

	unauthenticatedClient := &http.Client{Transport: server.Client().Transport}
	unauthenticated := doRequest(t, unauthenticatedClient, http.MethodGet, server.URL+"/api/v1/system/info", "", nil)
	assertAPIError(t, unauthenticated, http.StatusUnauthorized, APIErrorCodeSESSIONEXPIRED)

	dashboardResponse := doRequest(t, client, http.MethodGet, server.URL+"/api/v1/dashboard", "", nil)
	var dashboardBody Dashboard
	decodeSuccess(t, dashboardResponse, &dashboardBody)
	connections, connectionsErr := dashboardBody.Traffic.ConnectionsToday.Get()
	ingress, ingressErr := dashboardBody.Traffic.IngressBytesToday.Get()
	egress, egressErr := dashboardBody.Traffic.EgressBytesToday.Get()
	if dashboardBody.ServerStatus != DashboardServerStatusREADY ||
		dashboardBody.Counts.TunnelsOnline != 1 || dashboardBody.Counts.ConnectorsOnline != 2 ||
		dashboardBody.Counts.ServicesReady != 1 || dashboardBody.Counts.ServicesError != 2 ||
		dashboardBody.Counts.ActiveConnections != 7 ||
		dashboardBody.Traffic.Availability != UsageSummaryAvailabilityAVAILABLE ||
		connectionsErr != nil || ingressErr != nil || egressErr != nil ||
		connections != 9 || ingress != 10 || egress != 11 ||
		dashboardBody.RecentErrors.Availability != RecentErrorsSummaryAvailabilityAVAILABLE ||
		len(dashboardBody.RecentErrors.Items) != 1 ||
		dashboardBody.RecentErrors.Items[0].Code != RecentErrorCodeORIGINDOWN ||
		!dashboardBody.RecentErrors.Items[0].OccurredAt.Equal(recentOccurredAt) {
		t.Fatalf("dashboard response = %#v", dashboardBody)
	}
	gotRequestID, err := dashboardBody.RecentErrors.Items[0].RequestId.Get()
	if err != nil || string(gotRequestID) != recentRequestID {
		t.Fatalf("dashboard recent error request_id = %q/%v, want %q", gotRequestID, err, recentRequestID)
	}
	if dashboardBody.GatewayCertificate.TlsMode != GatewayCertificateTlsModePinned ||
		dashboardBody.GatewayCertificate.Level != HEALTHY ||
		dashboardBody.GatewayCertificate.RecentRenewalFailed ||
		dashboardBody.GatewayCertificate.RemainingSeconds <= 0 {
		t.Fatalf("dashboard gateway certificate = %#v", dashboardBody.GatewayCertificate)
	}
	if _, err := dashboardBody.GatewayCertificate.RecentRenewalErrorCode.Get(); err == nil {
		t.Fatal("dashboard gateway certificate renewal error code is present without a failure")
	}

	healthResponse := doRequest(t, client, http.MethodGet, server.URL+"/api/v1/system/health", "", nil)
	var health SystemHealth
	decodeSuccess(t, healthResponse, &health)
	if health.Status != SystemHealthStatusREADY || len(health.Checks) != 1 || health.Checks[0].Name != "sqlite" {
		t.Fatalf("system health = %#v", health)
	}

	configResponse := doRequest(t, client, http.MethodGet, server.URL+"/api/v1/system/config", "", nil)
	configBytes, err := io.ReadAll(configResponse.Body)
	configResponse.Body.Close()
	if err != nil || configResponse.StatusCode != http.StatusOK {
		t.Fatalf("system config status/read = %d/%v", configResponse.StatusCode, err)
	}
	for _, secret := range []string{config.Server.DataDir, config.AgentGateway.TLS.CertFile, config.AgentGateway.TLS.KeyFile} {
		if bytes.Contains(configBytes, []byte(secret)) {
			t.Fatalf("system config leaked %q: %s", secret, configBytes)
		}
	}
	var configBody SystemConfig
	if err := json.Unmarshal(configBytes, &configBody); err != nil || configBody.Management.PublicUrl != publicURL ||
		!bool(configBody.ChangesRequireRestart) || configBody.Limits.MaxTunnels != config.Limits.MaxTunnels {
		t.Fatalf("system config projection = %#v, error = %v", configBody, err)
	}

	first := doRequest(t, client, http.MethodGet,
		server.URL+"/api/v1/security-audit-events?page_size=1&action=CONNECTION_TOKEN_REVEAL", "", nil)
	var firstPage SecurityAuditEventList
	decodeSuccess(t, first, &firstPage)
	if len(firstPage.Items) != 1 || firstPage.Items[0].EventId != "evt_01J00000000000000000000002" || firstPage.NextPageToken == nil {
		t.Fatalf("first audit page = %#v", firstPage)
	}
	expectedDigest := hex.EncodeToString(bytes.Repeat([]byte{0xab}, 32))
	digest, err := firstPage.Items[0].BeforeStateDigest.Get()
	if err != nil || digest != expectedDigest {
		t.Fatalf("audit digest = %q/%v, want %q", digest, err, expectedDigest)
	}
	secondURL := server.URL + "/api/v1/security-audit-events?page_size=1&action=CONNECTION_TOKEN_REVEAL&page_token=" +
		url.QueryEscape(*firstPage.NextPageToken)
	second := doRequest(t, client, http.MethodGet, secondURL, "", nil)
	var secondPage SecurityAuditEventList
	decodeSuccess(t, second, &secondPage)
	if len(secondPage.Items) != 1 || secondPage.Items[0].EventId != "evt_01J00000000000000000000001" || secondPage.NextPageToken != nil {
		t.Fatalf("second audit page = %#v", secondPage)
	}

	export := doRequest(t, client, http.MethodGet,
		server.URL+"/api/v1/security-audit-events/export?action=CONNECTION_TOKEN_REVEAL", "", nil)
	exportBody, err := io.ReadAll(export.Body)
	export.Body.Close()
	if err != nil || export.StatusCode != http.StatusOK {
		t.Fatalf("audit export status/read = %d/%v", export.StatusCode, err)
	}
	if export.Header.Get("Cache-Control") != "no-store" ||
		export.Header.Get("Content-Disposition") != securityAuditExportFilename {
		t.Fatalf("audit export headers = %#v", export.Header)
	}
	lines := bytes.Split(bytes.TrimSpace(exportBody), []byte{'\n'})
	if len(lines) != 2 {
		t.Fatalf("audit export lines = %d, want 2; body = %s", len(lines), exportBody)
	}
	for index, wantID := range []string{"evt_01J00000000000000000000002", "evt_01J00000000000000000000001"} {
		var event SecurityAuditEvent
		if err := json.Unmarshal(lines[index], &event); err != nil || event.EventId != wantID {
			t.Fatalf("audit export line %d = %#v/%v, want %s", index, event, err, wantID)
		}
	}

	boundTokenURL := server.URL + "/api/v1/security-audit-events?page_size=1&action=CONNECTION_TOKEN_ROTATE&page_token=" +
		url.QueryEscape(*firstPage.NextPageToken)
	boundToken := doRequest(t, client, http.MethodGet, boundTokenURL, "", nil)
	assertAPIError(t, boundToken, http.StatusBadRequest, APIErrorCodeINVALIDPAGETOKEN)
	for _, invalidDate := range []string{"2026-08-29", "2026-08-29T00:00:00+00:00"} {
		response := doRequest(t, client, http.MethodGet,
			server.URL+"/api/v1/security-audit-events?occurred_from="+url.QueryEscape(invalidDate), "", nil)
		assertAPIError(t, response, http.StatusBadRequest, APIErrorCodeINVALIDREQUEST)
	}
	epoch := doRequest(t, client, http.MethodGet,
		server.URL+"/api/v1/security-audit-events?occurred_to="+url.QueryEscape("1970-01-01T00:00:00Z"), "", nil)
	var epochPage SecurityAuditEventList
	decodeSuccess(t, epoch, &epochPage)
	if len(epochPage.Items) != 0 {
		t.Fatalf("epoch-exclusive audit page = %#v, want empty", epochPage)
	}
	spacedResource := doRequest(t, client, http.MethodGet,
		server.URL+"/api/v1/security-audit-events?resource_id="+url.QueryEscape(" spaced "), "", nil)
	var spacedPage SecurityAuditEventList
	decodeSuccess(t, spacedResource, &spacedPage)
	if len(spacedPage.Items) != 0 {
		t.Fatalf("spaced resource audit page = %#v, want empty", spacedPage)
	}

	mutation := doRequest(t, client, http.MethodDelete, server.URL+"/api/v1/security-audit-events", publicURL, nil)
	if mutation.StatusCode != http.StatusMethodNotAllowed {
		mutation.Body.Close()
		t.Fatalf("security audit mutation status = %d, want 405", mutation.StatusCode)
	}
	mutation.Body.Close()
}

func TestSecurityAuditExportAbortsCommittedStreamOnLaterPageFailure(t *testing.T) {
	event := repository.SecurityAuditEvent{
		EventID: "evt_01J00000000000000000000001", OperationID: "op_01J00000000000000000000001",
		Event: repository.SecurityAuditEventOperationResult, Action: repository.SecurityAuditActionGatewayKeyRotate,
		ActorType: repository.SecurityAuditActorLocalOperator, ResourceType: repository.SecurityAuditResourceGatewayIdentity,
		ResourceID: "gateway.example.test", Result: repository.SecurityAuditResultSucceeded, OccurredAt: 100,
	}
	queryErr := errors.New("next page failed")
	service := application.NewSecurityAuditQueryService(&auditExportQueryStoreFake{err: queryErr})
	response := securityAuditExportResponse{
		ctx: context.Background(), handler: &ManagementHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))},
		requestID: "req_01J00000000000000000000000", service: service,
		query: repository.SecurityAuditEventQuery{Limit: repository.MaxSecurityAuditEventQueryLimit},
		first: repository.SecurityAuditEventPage{
			Events: []repository.SecurityAuditEvent{event},
			Next:   &repository.SecurityAuditEventCursor{OccurredAt: event.OccurredAt, EventID: event.EventID},
		},
	}
	recorder := httptest.NewRecorder()
	defer func() {
		if recovered := recover(); recovered != http.ErrAbortHandler {
			t.Fatalf("VisitExportSecurityAuditEventsResponse() panic = %v, want http.ErrAbortHandler", recovered)
		}
		if recorder.Code != http.StatusOK || bytes.Contains(recorder.Body.Bytes(), []byte(`"error"`)) {
			t.Fatalf("committed export response = %d/%s, want truncated NDJSON without appended error envelope", recorder.Code, recorder.Body.Bytes())
		}
	}()
	_ = response.VisitExportSecurityAuditEventsResponse(recorder)
	t.Fatal("VisitExportSecurityAuditEventsResponse() returned, want connection abort")
}

type auditExportQueryStoreFake struct{ err error }

func (fake *auditExportQueryStoreFake) QuerySecurityAuditEvents(
	context.Context,
	repository.SecurityAuditEventQuery,
) (repository.SecurityAuditEventPage, error) {
	return repository.SecurityAuditEventPage{}, fake.err
}

func (fake *auditExportQueryStoreFake) SecurityAuditEventExportBoundary(
	context.Context,
	repository.SecurityAuditEventQuery,
) (repository.SecurityAuditEventExportBoundary, bool, error) {
	return repository.SecurityAuditEventExportBoundary{}, false, fake.err
}

func managementReadTestConfig(publicURL string) serverconfig.Config {
	return serverconfig.Config{
		Server:     serverconfig.Server{DataDir: "C:/private/xtunnel-data"},
		Management: serverconfig.Management{PublicURL: publicURL},
		AgentGateway: serverconfig.AgentGateway{
			PublicHostname: "gateway.example", TLS: serverconfig.AgentGatewayTLS{
				Mode: "pinned", CertFile: "C:/private/gateway.crt", KeyFile: "C:/private/gateway.key",
			},
		},
		TCPIngress: serverconfig.TCPIngress{MinPort: 20000, MaxPort: 20100},
		Logging:    serverconfig.Logging{Level: "info"},
		Limits: serverconfig.Limits{
			MaxTunnels: 10, MaxConnectors: 20, MaxConnectorsPerTunnel: 5, MaxServicesPerTunnel: 30,
			MaxActiveConnections: 100, MaxConnectionsPerTunnel: 50, MaxConnectionsPerService: 25,
			MaxConnectionsPerSourceIP: 10, MaxOpenRatePerSourceIP: 9, MaxOpenBurstPerSourceIP: 8,
			MaxHTTPRequestsPerSourceIPPerSecond: 7, MaxHTTPHeaderBytes: 65536, MaxHTTPBodyBytes: 1048576,
		},
	}
}
