package application

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/repository"
	serverstatus "github.com/lifei6671/xtunnel/internal/server/status"
)

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
				ServicesTotal: 7, ServicesReady: 1, ServicesError: 1, ActiveConnections: 12,
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
			service := NewDashboardService(tunnels, services, dashboardServerStatusFake{status: test.serverStatus}, usage)
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
			if got.RecentErrors.Availability != DashboardAvailabilityUnavailable ||
				got.RecentErrors.Items == nil || len(got.RecentErrors.Items) != 0 {
				t.Fatalf("Snapshot().RecentErrors = %+v, want UNAVAILABLE with non-nil empty items", got.RecentErrors)
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
	service := NewDashboardService(
		&dashboardTunnelReaderFake{}, &dashboardServiceReaderFake{},
		dashboardServerStatusFake{status: DashboardServerStatusReady}, usage,
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
	service := NewDashboardService(
		&dashboardTunnelReaderFake{}, &dashboardServiceReaderFake{},
		dashboardServerStatusFake{status: DashboardServerStatusReady},
		&dashboardUsageReaderFake{totals: repository.UsageTotals{Connections: uint64(^uint64(0)>>1) + 1}},
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
		{name: "nil context", service: NewDashboardService(validTunnels, validServices, dashboardServerStatusFake{status: DashboardServerStatusReady}, validUsage), wantErr: ErrDashboardInput},
		{name: "nil tunnel reader", service: NewDashboardService(nil, validServices, dashboardServerStatusFake{status: DashboardServerStatusReady}, validUsage), ctx: context.Background(), wantErr: ErrDashboardInput},
		{name: "nil service reader", service: NewDashboardService(validTunnels, nil, dashboardServerStatusFake{status: DashboardServerStatusReady}, validUsage), ctx: context.Background(), wantErr: ErrDashboardInput},
		{name: "nil status owner", service: NewDashboardService(validTunnels, validServices, nil, validUsage), ctx: context.Background(), wantErr: ErrDashboardInput},
		{name: "nil usage reader", service: NewDashboardService(validTunnels, validServices, dashboardServerStatusFake{status: DashboardServerStatusReady}, nil), ctx: context.Background(), wantErr: ErrDashboardInput},
		{name: "unknown status", service: NewDashboardService(validTunnels, validServices, dashboardServerStatusFake{status: "STARTING"}, validUsage), ctx: context.Background(), wantErr: ErrDashboardServerStatus},
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tests = append(tests, struct {
		name    string
		service *DashboardService
		ctx     context.Context
		wantErr error
	}{
		name: "canceled context", service: NewDashboardService(validTunnels, validServices, dashboardServerStatusFake{status: DashboardServerStatusReady}, validUsage),
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
		name: "expired deadline", service: NewDashboardService(validTunnels, validServices, dashboardServerStatusFake{status: DashboardServerStatusReady}, validUsage),
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
			service := NewDashboardService(test.tunnels, test.services, dashboardServerStatusFake{status: DashboardServerStatusReady}, test.usage)
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
	service := NewDashboardService(
		tunnels, services,
		dashboardServerStatusFake{status: DashboardServerStatusReady, err: errStatus},
		&dashboardUsageReaderFake{},
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
	service := NewDashboardService(
		&dashboardTunnelReaderFake{}, &dashboardServiceReaderFake{},
		dashboardServerStatusFake{status: DashboardServerStatusReady},
		&dashboardUsageReaderFake{},
	)
	first, err := service.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("first Snapshot() error = %v", err)
	}
	first.RecentErrors.Items = append(first.RecentErrors.Items, DashboardRecentError{Code: "MUTATED"})
	second, err := service.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("second Snapshot() error = %v", err)
	}
	if len(second.RecentErrors.Items) != 0 {
		t.Fatalf("second Snapshot().RecentErrors.Items = %+v, want an independent empty slice", second.RecentErrors.Items)
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
