package application

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/repository"
	serverrecenterror "github.com/lifei6671/xtunnel/internal/server/recenterror"
	serverstatus "github.com/lifei6671/xtunnel/internal/server/status"
)

var validDashboardCertificateReader = DashboardGatewayCertificateReaderFunc(func(context.Context) (DashboardGatewayCertificateSource, error) {
	return DashboardGatewayCertificateSource{
		TLSMode: "pinned", ExpiryUnixSeconds: time.Date(2027, time.August, 30, 0, 0, 0, 0, time.UTC).Unix(),
	}, nil
})

func newDashboardServiceForTest(
	tunnels DashboardTunnelReader,
	services DashboardServiceReader,
	status DashboardServerStatusOwner,
	usage DashboardUsageReader,
	errors DashboardRecentErrorReader,
) *DashboardService {
	return NewDashboardService(tunnels, services, status, usage, errors, validDashboardCertificateReader)
}

func TestDashboardSnapshotAggregatesAuthoritativeViews(t *testing.T) {
	generatedAt := time.Date(2026, time.August, 29, 16, 30, 0, 123, time.FixedZone("UTC+8", 8*60*60))
	tests := []struct {
		name         string
		serverStatus DashboardServerStatus
		tunnels      []TunnelView
		services     []ServiceView
		wantCounts   DashboardCounts
	}{
		{
			name:         "ready status is not recomputed from resource degradation",
			serverStatus: DashboardServerStatusReady,
			tunnels: []TunnelView{
				{Tunnel: repository.Tunnel{ID: "tun-online"}, Status: serverstatus.TunnelStatusOnline, ConnectorsOnline: 2, ActiveConnections: 3},
				{Tunnel: repository.Tunnel{ID: "tun-offline"}, Status: serverstatus.TunnelStatusOffline, ActiveConnections: 4},
				{Tunnel: repository.Tunnel{ID: "tun-degraded"}, Status: serverstatus.TunnelStatusDegraded, ConnectorsOnline: 1, ActiveConnections: 5},
				{Tunnel: repository.Tunnel{ID: "tun-pending"}, Status: serverstatus.TunnelStatusPending},
				{Tunnel: repository.Tunnel{ID: "tun-revoked"}, Status: serverstatus.TunnelStatusRevoked},
			},
			services: []ServiceView{
				{Status: serverstatus.ServiceStatusReady},
				{Status: serverstatus.ServiceStatusApplyFailed},
				{Status: serverstatus.ServiceStatusOriginUnhealthy},
				{Status: serverstatus.ServiceStatusTunnelOffline},
				{Status: serverstatus.ServiceStatusConfigSyncing},
				{Status: serverstatus.ServiceStatusNoCapacity},
				{Status: serverstatus.ServiceStatusDisabled},
			},
			wantCounts: DashboardCounts{
				TunnelsTotal: 5, TunnelsOnline: 1, TunnelsOffline: 1, ConnectorsOnline: 3,
				ServicesTotal: 7, ServicesReady: 1, ServicesError: 4, ActiveConnections: 12,
			},
		},
		{
			name:         "degraded status remains authoritative with empty counts",
			serverStatus: DashboardServerStatusDegraded,
			wantCounts:   DashboardCounts{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tunnels := &dashboardTunnelReaderFake{views: test.tunnels}
			services := &dashboardServiceReaderFake{views: test.services}
			usage := &dashboardUsageReaderFake{totals: repository.UsageTotals{
				Connections: 11, IngressBytes: 22, EgressBytes: 33,
			}}
			service := newDashboardServiceForTest(
				tunnels, services, dashboardServerStatusFake{status: test.serverStatus}, usage,
				serverrecenterror.NewOwner(),
			)
			service.now = func() time.Time { return generatedAt }

			got, err := service.Snapshot(context.Background())
			if err != nil {
				t.Fatalf("Snapshot() error = %v", err)
			}
			if got.ServerStatus != test.serverStatus {
				t.Fatalf("Snapshot().ServerStatus = %q, want %q", got.ServerStatus, test.serverStatus)
			}
			if !reflect.DeepEqual(got.Counts, test.wantCounts) {
				t.Fatalf("Snapshot().Counts = %+v, want %+v", got.Counts, test.wantCounts)
			}
			if got.Traffic.Availability != DashboardAvailabilityAvailable ||
				got.Traffic.ConnectionsToday == nil || *got.Traffic.ConnectionsToday != 11 ||
				got.Traffic.IngressBytesToday == nil || *got.Traffic.IngressBytesToday != 22 ||
				got.Traffic.EgressBytesToday == nil || *got.Traffic.EgressBytesToday != 33 {
				t.Fatalf("Snapshot().Traffic = %+v, want AVAILABLE usage", got.Traffic)
			}
			if got.RecentErrors.Availability != DashboardAvailabilityAvailable ||
				got.RecentErrors.Items == nil || len(got.RecentErrors.Items) != 0 {
				t.Fatalf("Snapshot().RecentErrors = %+v, want AVAILABLE with non-nil empty items", got.RecentErrors)
			}
			if want := generatedAt.UTC(); !got.GeneratedAt.Equal(want) || got.GeneratedAt.Location() != time.UTC {
				t.Fatalf("Snapshot().GeneratedAt = %v (%v), want %v (UTC)", got.GeneratedAt, got.GeneratedAt.Location(), want)
			}
			if tunnels.calls != 1 {
				t.Fatalf("Tunnel List() calls = %d, want 1", tunnels.calls)
			}
			if services.calls != 1 {
				t.Fatalf("Service ListAll() calls = %d, want 1", services.calls)
			}
			if usage.calls != 1 || !usage.at.Equal(generatedAt.UTC()) || usage.at.Location() != time.UTC {
				t.Fatalf("UsageToday() calls/time = %d/%v, want 1/%v UTC", usage.calls, usage.at, generatedAt.UTC())
			}
		})
	}
}

func TestDashboardSnapshotFreezesUTCMidnightOnce(t *testing.T) {
	usage := &dashboardUsageReaderFake{}
	service := newDashboardServiceForTest(
		&dashboardTunnelReaderFake{}, &dashboardServiceReaderFake{},
		dashboardServerStatusFake{status: DashboardServerStatusReady}, usage, serverrecenterror.NewOwner(),
	)
	first := time.Date(2026, time.August, 30, 0, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	second := first.Add(24 * time.Hour)
	nowCalls := 0
	service.now = func() time.Time {
		nowCalls++
		if nowCalls == 1 {
			return first
		}
		return second
	}

	got, err := service.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if nowCalls != 1 || !usage.at.Equal(first.UTC()) || !got.GeneratedAt.Equal(first.UTC()) ||
		usage.at.Location() != time.UTC || got.GeneratedAt.Location() != time.UTC {
		t.Fatalf("frozen now calls/usage/generated = %d/%v/%v", nowCalls, usage.at, got.GeneratedAt)
	}
}

func TestDashboardSnapshotRejectsUsageOverflow(t *testing.T) {
	service := newDashboardServiceForTest(
		&dashboardTunnelReaderFake{}, &dashboardServiceReaderFake{},
		dashboardServerStatusFake{status: DashboardServerStatusReady},
		&dashboardUsageReaderFake{totals: repository.UsageTotals{Connections: uint64(^uint64(0)>>1) + 1}},
		serverrecenterror.NewOwner(),
	)
	if _, err := service.Snapshot(context.Background()); !errors.Is(err, repository.ErrUsageOverflow) {
		t.Fatalf("Snapshot() overflow error = %v, want ErrUsageOverflow", err)
	}
}

func TestServiceAPIListAllUsesExistingProjectionAcrossTunnels(t *testing.T) {
	store := openServiceManagementStore(t)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelIDTwo)
	service := newServiceAPITestService(
		t, store, &recordingSnapshotGate{}, &recordingSnapshotNotifier{}, &recordingRouteNotifier{},
		serviceManagementIDTwo, serviceManagementIDOne,
	)
	for _, input := range []CreateServiceAPIInput{
		{
			Service:  validCreateServiceInput(serviceManagementTunnelID, "one"),
			Exposure: ServiceExposureInput{Type: ServiceExposureHTTP, Hostname: "one.example.test"},
		},
		{
			Service:  validCreateServiceInput(serviceManagementTunnelIDTwo, "two"),
			Exposure: ServiceExposureInput{Type: ServiceExposureHTTP, Hostname: "two.example.test"},
		},
	} {
		if _, err := service.Create(context.Background(), input); err != nil {
			t.Fatalf("Create(%q) error = %v", input.Service.TunnelID, err)
		}
	}

	got, err := service.ListAll(context.Background())
	if err != nil {
		t.Fatalf("ListAll() error = %v", err)
	}
	gotIDs := make([]string, 0, len(got))
	for _, view := range got {
		gotIDs = append(gotIDs, view.Service.ID)
	}
	if want := []string{serviceManagementIDOne, serviceManagementIDTwo}; !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("ListAll() IDs = %v, want stable all-tunnel projection %v", gotIDs, want)
	}
	for _, view := range got {
		if view.Status != serverstatus.ServiceStatusTunnelOffline {
			t.Fatalf("ListAll() service %q status = %q, want existing TUNNEL_OFFLINE projection", view.Service.ID, view.Status)
		}
	}
}

func TestDashboardSnapshotRejectsInvalidInputsAndStatus(t *testing.T) {
	validTunnels := &dashboardTunnelReaderFake{}
	validServices := &dashboardServiceReaderFake{}
	validUsage := &dashboardUsageReaderFake{}
	tests := []struct {
		name    string
		service *DashboardService
		ctx     context.Context
		wantErr error
	}{
		{name: "nil receiver", ctx: context.Background(), wantErr: ErrDashboardInput},
		{name: "nil context", service: newDashboardServiceForTest(validTunnels, validServices, dashboardServerStatusFake{status: DashboardServerStatusReady}, validUsage, serverrecenterror.NewOwner()), wantErr: ErrDashboardInput},
		{name: "nil tunnel reader", service: newDashboardServiceForTest(nil, validServices, dashboardServerStatusFake{status: DashboardServerStatusReady}, validUsage, serverrecenterror.NewOwner()), ctx: context.Background(), wantErr: ErrDashboardInput},
		{name: "nil service reader", service: newDashboardServiceForTest(validTunnels, nil, dashboardServerStatusFake{status: DashboardServerStatusReady}, validUsage, serverrecenterror.NewOwner()), ctx: context.Background(), wantErr: ErrDashboardInput},
		{name: "nil status owner", service: newDashboardServiceForTest(validTunnels, validServices, nil, validUsage, serverrecenterror.NewOwner()), ctx: context.Background(), wantErr: ErrDashboardInput},
		{name: "nil usage reader", service: newDashboardServiceForTest(validTunnels, validServices, dashboardServerStatusFake{status: DashboardServerStatusReady}, nil, serverrecenterror.NewOwner()), ctx: context.Background(), wantErr: ErrDashboardInput},
		{name: "nil recent error reader", service: newDashboardServiceForTest(validTunnels, validServices, dashboardServerStatusFake{status: DashboardServerStatusReady}, validUsage, nil), ctx: context.Background(), wantErr: ErrDashboardInput},
		{name: "nil certificate reader", service: NewDashboardService(validTunnels, validServices, dashboardServerStatusFake{status: DashboardServerStatusReady}, validUsage, serverrecenterror.NewOwner(), nil), ctx: context.Background(), wantErr: ErrDashboardInput},
		{name: "unknown status", service: newDashboardServiceForTest(validTunnels, validServices, dashboardServerStatusFake{status: "STARTING"}, validUsage, serverrecenterror.NewOwner()), ctx: context.Background(), wantErr: ErrDashboardServerStatus},
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tests = append(tests, struct {
		name    string
		service *DashboardService
		ctx     context.Context
		wantErr error
	}{
		name: "canceled context", service: newDashboardServiceForTest(validTunnels, validServices, dashboardServerStatusFake{status: DashboardServerStatusReady}, validUsage, serverrecenterror.NewOwner()),
		ctx: canceled, wantErr: context.Canceled,
	})
	deadline, cancelDeadline := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer cancelDeadline()
	tests = append(tests, struct {
		name    string
		service *DashboardService
		ctx     context.Context
		wantErr error
	}{
		name: "expired deadline", service: newDashboardServiceForTest(validTunnels, validServices, dashboardServerStatusFake{status: DashboardServerStatusReady}, validUsage, serverrecenterror.NewOwner()),
		ctx: deadline, wantErr: context.DeadlineExceeded,
	})

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := test.service.Snapshot(test.ctx)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Snapshot() error = %v, want errors.Is(_, %v)", err, test.wantErr)
			}
		})
	}
}

func TestDashboardSnapshotPropagatesOwnerErrors(t *testing.T) {
	errRead := errors.New("read failed")
	tests := []struct {
		name        string
		tunnels     *dashboardTunnelReaderFake
		services    *dashboardServiceReaderFake
		usage       *dashboardUsageReaderFake
		wantMessage string
	}{
		{
			name:    "tunnel read failure",
			tunnels: &dashboardTunnelReaderFake{err: errRead}, services: &dashboardServiceReaderFake{}, usage: &dashboardUsageReaderFake{},
			wantMessage: "read dashboard tunnels",
		},
		{
			name:        "service read failure",
			tunnels:     &dashboardTunnelReaderFake{views: []TunnelView{{Tunnel: repository.Tunnel{ID: "tun-broken"}}}},
			services:    &dashboardServiceReaderFake{err: errRead},
			usage:       &dashboardUsageReaderFake{},
			wantMessage: "read dashboard services",
		},
		{
			name: "usage read failure", tunnels: &dashboardTunnelReaderFake{}, services: &dashboardServiceReaderFake{},
			usage: &dashboardUsageReaderFake{err: errRead}, wantMessage: "read dashboard usage",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := newDashboardServiceForTest(test.tunnels, test.services, dashboardServerStatusFake{status: DashboardServerStatusReady}, test.usage, serverrecenterror.NewOwner())
			_, err := service.Snapshot(context.Background())
			if !errors.Is(err, errRead) || !strings.Contains(err.Error(), test.wantMessage) {
				t.Fatalf("Snapshot() error = %v, want wrapped source error containing %q", err, test.wantMessage)
			}
		})
	}
}

func TestDashboardSnapshotPropagatesServerStatusError(t *testing.T) {
	errStatus := errors.New("health snapshot failed")
	tunnels := &dashboardTunnelReaderFake{}
	services := &dashboardServiceReaderFake{}
	service := newDashboardServiceForTest(
		tunnels, services,
		dashboardServerStatusFake{status: DashboardServerStatusReady, err: errStatus},
		&dashboardUsageReaderFake{},
		serverrecenterror.NewOwner(),
	)

	_, err := service.Snapshot(context.Background())
	if !errors.Is(err, errStatus) || !strings.Contains(err.Error(), "read dashboard server status") {
		t.Fatalf("Snapshot() error = %v, want wrapped Server Status error", err)
	}
	if tunnels.calls != 0 || services.calls != 0 {
		t.Fatalf("resource readers called after Server Status failure: tunnels=%d services=%d", tunnels.calls, services.calls)
	}
}

func TestDashboardSnapshotDoesNotReuseRecentErrorStorage(t *testing.T) {
	owner := serverrecenterror.NewOwner()
	requestID := "req_01K4Z3JMESEMR8E7Z8AC9PKYYJ"
	if err := owner.Publish(serverrecenterror.Record{
		Code: serverrecenterror.CodeProtocolError, OccurredAt: time.Now(), RequestID: &requestID,
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	service := newDashboardServiceForTest(
		&dashboardTunnelReaderFake{}, &dashboardServiceReaderFake{},
		dashboardServerStatusFake{status: DashboardServerStatusReady},
		&dashboardUsageReaderFake{},
		owner,
	)
	first, err := service.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("first Snapshot() error = %v", err)
	}
	first.RecentErrors.Items[0].Message = "MUTATED"
	*first.RecentErrors.Items[0].RequestID = "req_mutated"
	second, err := service.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("second Snapshot() error = %v", err)
	}
	if len(second.RecentErrors.Items) != 1 || second.RecentErrors.Items[0].Message == "MUTATED" ||
		second.RecentErrors.Items[0].RequestID == nil || *second.RecentErrors.Items[0].RequestID != requestID {
		t.Fatalf("second Snapshot().RecentErrors.Items = %+v, want an independent owner snapshot", second.RecentErrors.Items)
	}
}

func TestDashboardGatewayCertificateFreezesThirtySevenOneDayLevels(t *testing.T) {
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		remaining time.Duration
		want      DashboardGatewayCertificateLevel
	}{
		{name: "above thirty days", remaining: 30*24*time.Hour + time.Second, want: DashboardGatewayCertificateHealthy},
		{name: "thirty days", remaining: 30 * 24 * time.Hour, want: DashboardGatewayCertificateWarning},
		{name: "seven days", remaining: 7 * 24 * time.Hour, want: DashboardGatewayCertificateCritical},
		{name: "one day", remaining: 24 * time.Hour, want: DashboardGatewayCertificateEmergency},
		{name: "expired", remaining: 0, want: DashboardGatewayCertificateExpired},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := dashboardGatewayCertificate(DashboardGatewayCertificateSource{
				TLSMode: "pinned", ExpiryUnixSeconds: now.Add(test.remaining).Unix(), RenewalFailed: true,
			}, now)
			if err != nil {
				t.Fatalf("dashboardGatewayCertificate() error = %v", err)
			}
			if got.Level != test.want || got.RemainingSeconds != int64(test.remaining/time.Second) ||
				!got.RecentRenewalFailed || got.RecentRenewalErrorCode == nil ||
				*got.RecentRenewalErrorCode != DashboardGatewayCertificateRenewalFailedCode {
				t.Fatalf("dashboardGatewayCertificate() = %#v, want level %q and stable renewal failure", got, test.want)
			}
		})
	}
}

func TestDashboardGatewayCertificateRejectsInvalidSource(t *testing.T) {
	now := time.Now().UTC()
	for _, source := range []DashboardGatewayCertificateSource{
		{TLSMode: "unknown", ExpiryUnixSeconds: now.Add(time.Hour).Unix()},
		{TLSMode: "public"},
	} {
		if _, err := dashboardGatewayCertificate(source, now); !errors.Is(err, ErrDashboardGatewayCertificate) {
			t.Fatalf("dashboardGatewayCertificate(%#v) error = %v, want ErrDashboardGatewayCertificate", source, err)
		}
	}
}

type dashboardTunnelReaderFake struct {
	views []TunnelView
	err   error
	calls int
}

func (fake *dashboardTunnelReaderFake) List(context.Context) ([]TunnelView, error) {
	fake.calls++
	return fake.views, fake.err
}

type dashboardServiceReaderFake struct {
	views []ServiceView
	err   error
	calls int
}

type dashboardUsageReaderFake struct {
	totals repository.UsageTotals
	err    error
	calls  int
	at     time.Time
}

func (fake *dashboardUsageReaderFake) UsageToday(_ context.Context, at time.Time) (repository.UsageTotals, error) {
	fake.calls++
	fake.at = at
	return fake.totals, fake.err
}

func (fake *dashboardServiceReaderFake) ListAll(context.Context) ([]ServiceView, error) {
	fake.calls++
	return fake.views, fake.err
}

type dashboardServerStatusFake struct {
	status DashboardServerStatus
	err    error
}

func (fake dashboardServerStatusFake) DashboardServerStatus(context.Context) (DashboardServerStatus, error) {
	return fake.status, fake.err
}
