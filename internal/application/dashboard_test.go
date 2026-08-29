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
			service := NewDashboardService(tunnels, services, dashboardServerStatusFake{status: test.serverStatus})
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
			if got.Traffic.Availability != DashboardAvailabilityUnavailable ||
				got.Traffic.ConnectionsToday != nil || got.Traffic.IngressBytesToday != nil ||
				got.Traffic.EgressBytesToday != nil {
				t.Fatalf("Snapshot().Traffic = %+v, want UNAVAILABLE with nil counters", got.Traffic)
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
		})
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
	tests := []struct {
		name    string
		service *DashboardService
		ctx     context.Context
		wantErr error
	}{
		{name: "nil receiver", ctx: context.Background(), wantErr: ErrDashboardInput},
		{name: "nil context", service: NewDashboardService(validTunnels, validServices, dashboardServerStatusFake{status: DashboardServerStatusReady}), wantErr: ErrDashboardInput},
		{name: "nil tunnel reader", service: NewDashboardService(nil, validServices, dashboardServerStatusFake{status: DashboardServerStatusReady}), ctx: context.Background(), wantErr: ErrDashboardInput},
		{name: "nil service reader", service: NewDashboardService(validTunnels, nil, dashboardServerStatusFake{status: DashboardServerStatusReady}), ctx: context.Background(), wantErr: ErrDashboardInput},
		{name: "nil status owner", service: NewDashboardService(validTunnels, validServices, nil), ctx: context.Background(), wantErr: ErrDashboardInput},
		{name: "unknown status", service: NewDashboardService(validTunnels, validServices, dashboardServerStatusFake{status: "STARTING"}), ctx: context.Background(), wantErr: ErrDashboardServerStatus},
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tests = append(tests, struct {
		name    string
		service *DashboardService
		ctx     context.Context
		wantErr error
	}{
		name: "canceled context", service: NewDashboardService(validTunnels, validServices, dashboardServerStatusFake{status: DashboardServerStatusReady}),
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
		name: "expired deadline", service: NewDashboardService(validTunnels, validServices, dashboardServerStatusFake{status: DashboardServerStatusReady}),
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
		wantMessage string
	}{
		{
			name:    "tunnel read failure",
			tunnels: &dashboardTunnelReaderFake{err: errRead}, services: &dashboardServiceReaderFake{},
			wantMessage: "read dashboard tunnels",
		},
		{
			name:        "service read failure",
			tunnels:     &dashboardTunnelReaderFake{views: []TunnelView{{Tunnel: repository.Tunnel{ID: "tun-broken"}}}},
			services:    &dashboardServiceReaderFake{err: errRead},
			wantMessage: "read dashboard services",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := NewDashboardService(test.tunnels, test.services, dashboardServerStatusFake{status: DashboardServerStatusReady})
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
