package application

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/repository"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
	serverroute "github.com/lifei6671/xtunnel/internal/server/route"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
	serverstatus "github.com/lifei6671/xtunnel/internal/server/status"
	"github.com/lifei6671/xtunnel/internal/tcpport"
)

func TestServiceAPICreateHTTPCommitsAggregateVersionsOnce(t *testing.T) {
	store := openServiceManagementStore(t)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
	tunnelNotifier := &recordingSnapshotNotifier{}
	routeNotifier := &recordingRouteNotifier{}
	service := newServiceAPITestService(
		t, store, &recordingSnapshotGate{}, tunnelNotifier, routeNotifier,
		serviceManagementIDOne,
	)

	result, err := service.Create(context.Background(), CreateServiceAPIInput{
		Service:  validCreateServiceInput(serviceManagementTunnelID, "console"),
		Exposure: ServiceExposureInput{Type: ServiceExposureHTTP, Hostname: "API.Example.Test"},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if result.Service.ID != serviceManagementIDOne || result.Service.Version != 1 ||
		result.Service.RequiredRevision != 1 || result.TunnelVersion != 1 ||
		result.TunnelRevision != 1 || result.Generation != 1 || !result.Changed {
		t.Fatalf("Create() aggregate result = %+v", result)
	}
	if result.Exposure.HTTP == nil || result.Exposure.TCP != nil ||
		result.Exposure.HTTP.Hostname != "api.example.test" ||
		result.Exposure.HTTP.PathPrefix != "/" || !result.Exposure.HTTP.PreserveHost {
		t.Fatalf("Create() HTTP Exposure = %+v", result.Exposure)
	}
	state := readServiceAPIDesiredState(t, store)
	if state.Generation != 1 || len(state.Services) != 1 || len(state.HTTPRoutes) != 1 || len(state.TCPRoutes) != 0 ||
		state.Tunnels[0].DesiredRevision != 1 {
		t.Fatalf("committed Desired State = %+v", state)
	}
	if got := tunnelNotifier.snapshot(); !reflect.DeepEqual(got, []string{serviceManagementTunnelID}) {
		t.Fatalf("Snapshot dirty calls = %v, want one Tunnel call", got)
	}
	if got := routeNotifier.snapshot(); !reflect.DeepEqual(got, []uint64{1}) {
		t.Fatalf("Route dirty calls = %v, want [1]", got)
	}
}

func TestServiceAPIExposureLifecycleIsAtomic(t *testing.T) {
	store := openServiceManagementStore(t)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
	service := newServiceAPITestService(
		t, store, &recordingSnapshotGate{}, &recordingSnapshotNotifier{}, &recordingRouteNotifier{},
		serviceManagementIDOne,
	)

	created, err := service.Create(context.Background(), CreateServiceAPIInput{
		Service:  validCreateServiceInput(serviceManagementTunnelID, "origin"),
		Exposure: ServiceExposureInput{Type: ServiceExposureTCP},
	})
	if err != nil {
		t.Fatalf("Create(TCP auto allocation) error = %v", err)
	}
	if created.Exposure.TCP == nil || created.Exposure.TCP.PublicPort != 10000 {
		t.Fatalf("Create(TCP auto allocation) Exposure = %+v", created.Exposure)
	}

	httpType := ServiceExposureHTTP
	hostname := "web.example.test"
	switched, err := service.Update(context.Background(), UpdateServiceAPIInput{
		TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
		ExpectedTunnelVersion: 1, ExpectedServiceVersion: 1,
		ExposureSet: true,
		Exposure:    &ServiceExposurePatchInput{Type: &httpType, Hostname: &hostname},
	})
	if err != nil {
		t.Fatalf("Update(TCP to HTTP) error = %v", err)
	}
	if switched.Exposure.HTTP == nil || switched.Exposure.TCP != nil ||
		switched.Service.Version != 2 || switched.TunnelRevision != 2 || switched.Generation != 2 {
		t.Fatalf("Update(TCP to HTTP) result = %+v", switched)
	}
	state := readServiceAPIDesiredState(t, store)
	if len(state.HTTPRoutes) != 1 || len(state.TCPRoutes) != 0 {
		t.Fatalf("Desired State after type switch = %+v", state)
	}

	removed, err := service.Update(context.Background(), UpdateServiceAPIInput{
		TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
		ExpectedTunnelVersion: 1, ExpectedServiceVersion: 2,
		ExposureSet: true, Exposure: nil,
	})
	if err != nil {
		t.Fatalf("Update(exposure null) error = %v", err)
	}
	if removed.Exposure != (repository.ServiceExposure{}) || removed.Service.Version != 3 ||
		removed.TunnelRevision != 3 || removed.Generation != 3 {
		t.Fatalf("Update(exposure null) result = %+v", removed)
	}
	state = readServiceAPIDesiredState(t, store)
	if len(state.HTTPRoutes) != 0 || len(state.TCPRoutes) != 0 {
		t.Fatalf("Desired State after exposure null = %+v", state)
	}
	deleted, err := service.Delete(context.Background(), DeleteServiceInput{
		TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
		ExpectedTunnelVersion: 1, ExpectedServiceVersion: 3,
	})
	if err != nil {
		t.Fatalf("Delete(without Exposure) error = %v", err)
	}
	if !deleted.Deleted || deleted.TunnelRevision != 4 || deleted.Generation != 4 {
		t.Fatalf("Delete(without Exposure) result = %+v", deleted)
	}
	state = readServiceAPIDesiredState(t, store)
	if len(state.Services) != 0 || state.Generation != 4 || state.Tunnels[0].DesiredRevision != 4 {
		t.Fatalf("Desired State after Service-only Delete = %+v", state)
	}
}

func TestServiceAPIEnableDisablePreservesExposureAndGatesRouteSnapshot(t *testing.T) {
	store := openServiceManagementStore(t)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
	tunnelNotifier := &recordingSnapshotNotifier{}
	routeManager, err := serverroute.NewManager(store)
	if err != nil {
		t.Fatalf("route.NewManager() error = %v", err)
	}
	service := newServiceAPITestService(
		t, store, &recordingSnapshotGate{}, tunnelNotifier, routeManager,
		serviceManagementIDOne,
	)
	created, err := service.Create(context.Background(), CreateServiceAPIInput{
		Service:  validCreateServiceInput(serviceManagementTunnelID, "toggle"),
		Exposure: ServiceExposureInput{Type: ServiceExposureHTTP, Hostname: "toggle.example.test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	routeContext, cancelRoutes := context.WithCancel(context.Background())
	if err := routeManager.Start(routeContext); err != nil {
		cancelRoutes()
		t.Fatalf("Route Manager Start() error = %v", err)
	}
	defer func() {
		cancelRoutes()
		routeManager.Wait()
	}()
	if routes, published := routeManager.Current().HTTP("toggle.example.test"); !published ||
		len(routes.Routes()) != 1 || routes.Routes()[0].RequiredRevision != 1 {
		t.Fatalf("initial Route Snapshot = routes:%+v published:%t", routes.Routes(), published)
	}

	disabled := false
	result, err := service.Update(context.Background(), UpdateServiceAPIInput{
		TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
		ExpectedTunnelVersion: 1, ExpectedServiceVersion: 1, Enabled: &disabled,
	})
	if err != nil {
		t.Fatalf("Update(disable) error = %v", err)
	}
	if result.Exposure.HTTP == nil || !result.Exposure.HTTP.Enabled ||
		result.TunnelRevision != 2 || result.Generation != 2 {
		t.Fatalf("Update(disable) did not preserve Exposure = %+v", result)
	}
	view, err := service.Get(context.Background(), created.Service.ID)
	if err != nil || view.Status != serverstatus.ServiceStatusDisabled {
		t.Fatalf("Get(disabled) = status:%q error:%v", view.Status, err)
	}
	disabledSnapshot := waitServiceAPIRouteGeneration(t, routeManager, 2)
	if _, published := disabledSnapshot.HTTP("toggle.example.test"); published {
		t.Fatal("disabled Service Exposure was published to Route Snapshot")
	}

	enabled := true
	result, err = service.Update(context.Background(), UpdateServiceAPIInput{
		TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
		ExpectedTunnelVersion: 1, ExpectedServiceVersion: 2, Enabled: &enabled,
	})
	if err != nil {
		t.Fatalf("Update(enable) error = %v", err)
	}
	if result.Exposure.HTTP == nil || !result.Exposure.HTTP.Enabled ||
		result.TunnelRevision != 3 || result.Generation != 3 {
		t.Fatalf("Update(enable) did not preserve Exposure = %+v", result)
	}
	enabledSnapshot := waitServiceAPIRouteGeneration(t, routeManager, 3)
	routes, published := enabledSnapshot.HTTP("toggle.example.test")
	if !published || len(routes.Routes()) != 1 || routes.Routes()[0].RequiredRevision != 3 {
		t.Fatalf("enabled Route Snapshot = routes:%+v published:%t", routes.Routes(), published)
	}

	originHost := "10.0.0.8"
	result, err = service.Update(context.Background(), UpdateServiceAPIInput{
		TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
		ExpectedTunnelVersion: 1, ExpectedServiceVersion: 3,
		Origin: &ServiceOriginPatchInput{Host: &originHost},
	})
	if err != nil {
		t.Fatalf("Update(origin) error = %v", err)
	}
	if result.TunnelRevision != 4 || result.Generation != 4 {
		t.Fatalf("Update(origin) result = %+v", result)
	}
	originSnapshot := waitServiceAPIRouteGeneration(t, routeManager, 4)
	routes, published = originSnapshot.HTTP("toggle.example.test")
	if !published || len(routes.Routes()) != 1 || routes.Routes()[0].OriginHost != originHost ||
		routes.Routes()[0].RequiredRevision != 4 {
		t.Fatalf("updated Origin Route Snapshot = routes:%+v published:%t", routes.Routes(), published)
	}
	if got := tunnelNotifier.snapshot(); !reflect.DeepEqual(got, []string{
		serviceManagementTunnelID, serviceManagementTunnelID,
		serviceManagementTunnelID, serviceManagementTunnelID,
	}) {
		t.Fatalf("Snapshot dirty calls = %v, want one per snapshot-changing mutation", got)
	}
}

func waitServiceAPIRouteGeneration(t *testing.T, manager *serverroute.Manager, want uint64) *serverroute.Snapshot {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		snapshot := manager.Current()
		if snapshot != nil && snapshot.Generation() == want {
			return snapshot
		}
		if time.Now().After(deadline) {
			if snapshot == nil {
				t.Fatalf("Route Snapshot generation = nil, want %d", want)
			}
			t.Fatalf("Route Snapshot generation = %d, want %d", snapshot.Generation(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestServiceAPIDeleteRemovesExposureBeforeService(t *testing.T) {
	store := openServiceManagementStore(t)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
	service := newServiceAPITestService(
		t, store, &recordingSnapshotGate{}, &recordingSnapshotNotifier{}, &recordingRouteNotifier{},
		serviceManagementIDOne,
	)
	created, err := service.Create(context.Background(), CreateServiceAPIInput{
		Service:  validCreateServiceInput(serviceManagementTunnelID, "delete"),
		Exposure: ServiceExposureInput{Type: ServiceExposureHTTP, Hostname: "delete.example.test"},
	})
	if err != nil {
		t.Fatal(err)
	}

	deleted, err := service.Delete(context.Background(), DeleteServiceInput{
		TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID,
		ExpectedTunnelVersion: 1, ExpectedServiceVersion: 1,
	})
	if err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if !deleted.Deleted || deleted.TunnelRevision != 2 || deleted.Generation != 2 {
		t.Fatalf("Delete() result = %+v", deleted)
	}
	state := readServiceAPIDesiredState(t, store)
	if len(state.Services) != 0 || len(state.HTTPRoutes) != 0 || len(state.TCPRoutes) != 0 ||
		state.Tunnels[0].DesiredRevision != 2 || state.Generation != 2 {
		t.Fatalf("Desired State after Delete = %+v", state)
	}
}

func TestServiceAPIOriginPatchValidatesFieldsAgainstEffectiveScheme(t *testing.T) {
	current, err := newServiceCandidate(
		serviceManagementIDOne,
		validCreateServiceInput(serviceManagementTunnelID, "origin patch"),
		1,
	)
	if err != nil {
		t.Fatalf("newServiceCandidate() error = %v", err)
	}
	tlsVerify := true
	tlsServerName := "origin.example.test"
	httpHost := "upstream.example.test"
	httpsScheme := repository.OriginSchemeHTTPS
	tcpScheme := repository.OriginSchemeTCP
	tests := []struct {
		name      string
		patch     ServiceOriginPatchInput
		wantError bool
	}{
		{name: "HTTP rejects TLS fields", patch: ServiceOriginPatchInput{TLSVerify: &tlsVerify}, wantError: true},
		{name: "HTTPS accepts TLS and HTTP fields", patch: ServiceOriginPatchInput{
			Scheme: &httpsScheme, TLSVerify: &tlsVerify, TLSServerName: &tlsServerName, HTTPHost: &httpHost,
		}},
		{name: "TCP rejects HTTP fields", patch: ServiceOriginPatchInput{
			Scheme: &tcpScheme, HTTPHost: &httpHost,
		}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate, _, _, err := applyServiceAPIPatch(current, UpdateServiceAPIInput{Origin: &test.patch})
			if test.wantError {
				if !errors.Is(err, ErrServiceManagementInput) {
					t.Fatalf("applyServiceAPIPatch() error = %v, want ErrServiceManagementInput", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("applyServiceAPIPatch() error = %v", err)
			}
			if candidate.OriginScheme != repository.OriginSchemeHTTPS || !candidate.TLSVerify ||
				candidate.TLSServerName != tlsServerName || candidate.OriginHTTPHost != httpHost {
				t.Fatalf("HTTPS Origin Patch result = %+v", candidate)
			}
		})
	}
}

func TestServiceAPIFailuresRollBackWholeAggregate(t *testing.T) {
	tests := []struct {
		name     string
		prepare  func(*testing.T, *ServiceAPIService, *recordingSnapshotGate)
		exposure ServiceExposureInput
	}{
		{
			name: "Snapshot Gate",
			prepare: func(_ *testing.T, _ *ServiceAPIService, gate *recordingSnapshotGate) {
				gate.setError(errors.New("gate rejected"))
			},
			exposure: ServiceExposureInput{Type: ServiceExposureHTTP, Hostname: "gate.example.test"},
		},
		{
			name:     "TCP port",
			prepare:  func(_ *testing.T, _ *ServiceAPIService, _ *recordingSnapshotGate) {},
			exposure: ServiceExposureInput{Type: ServiceExposureTCP, PublicPort: serviceAPIUint16(9999)},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openServiceManagementStore(t)
			seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
			gate := &recordingSnapshotGate{}
			service := newServiceAPITestService(
				t, store, gate, &recordingSnapshotNotifier{}, &recordingRouteNotifier{},
				serviceManagementIDOne,
			)
			test.prepare(t, service, gate)
			if _, err := service.Create(context.Background(), CreateServiceAPIInput{
				Service: validCreateServiceInput(serviceManagementTunnelID, "rollback"), Exposure: test.exposure,
			}); err == nil {
				t.Fatal("Create() error = nil")
			}
			assertServiceAPIAggregateState(t, store, 0, 0, 0, 0)
		})
	}

	t.Run("HTTP conflict", func(t *testing.T) {
		store := openServiceManagementStore(t)
		seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
		service := newServiceAPITestService(
			t, store, &recordingSnapshotGate{}, &recordingSnapshotNotifier{}, &recordingRouteNotifier{},
			serviceManagementIDOne, serviceManagementIDTwo,
		)
		input := CreateServiceAPIInput{
			Service:  validCreateServiceInput(serviceManagementTunnelID, "first"),
			Exposure: ServiceExposureInput{Type: ServiceExposureHTTP, Hostname: "same.example.test"},
		}
		if _, err := service.Create(context.Background(), input); err != nil {
			t.Fatal(err)
		}
		input.Service.Name = "second"
		if _, err := service.Create(context.Background(), input); err == nil {
			t.Fatal("Create(conflicting HTTP Exposure) error = nil")
		}
		assertServiceAPIAggregateState(t, store, 1, 1, 1, 1)
	})
}

func TestServiceAPIProjectsReadyApplyFailureAndActiveConnections(t *testing.T) {
	store := openServiceManagementStore(t)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
	runtime := &serviceAPIRuntimeFake{}
	limits := &serviceAPILimitsFake{}
	failures := &serviceAPIApplyFailuresFake{}
	service := newServiceAPITestServiceWithProjection(
		t, store, &recordingSnapshotGate{}, &recordingSnapshotNotifier{}, &recordingRouteNotifier{},
		runtime, limits, failures, serviceManagementIDOne,
	)
	created, err := service.Create(context.Background(), CreateServiceAPIInput{
		Service:  validCreateServiceInput(serviceManagementTunnelID, "status"),
		Exposure: ServiceExposureInput{Type: ServiceExposureHTTP, Hostname: "status.example.test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime.snapshots = []serverruntime.SessionStatusSnapshot{{
		Session:               serverruntime.Session{TunnelID: serviceManagementTunnelID},
		CurrentControlSession: true,
		HeartbeatFresh:        true,
		LifecycleStatus:       serverruntime.ConnectorStatusOnline,
		Config: serverruntime.SessionEligibility{
			ConfigReady: true, HasObserved: true, ObservedRevision: 1,
			Services: map[string]serverruntime.ServiceEligibility{
				created.Service.ID: {RequiredRevision: 1, Enabled: true, HealthDisabled: true},
			},
		},
		WorkPool: serverruntime.ConnectorWorkPoolSnapshot{Idle: 1},
	}}
	limits.snapshot = serverlimits.Snapshot{ActiveByService: map[serverlimits.ConnectionService]uint64{
		{TunnelID: serviceManagementTunnelID, ServiceID: created.Service.ID}: 3,
	}}

	view, err := service.Get(context.Background(), created.Service.ID)
	if err != nil {
		t.Fatalf("Get(READY) error = %v", err)
	}
	if view.Status != serverstatus.ServiceStatusReady || view.HealthyConnectors != 1 || view.ActiveConnections != 3 {
		t.Fatalf("Get(READY) projection = %+v", view)
	}
	failure := &serverstatus.ApplyFailure{RequiredRevision: 1, ErrorCode: "LISTEN_FAILED", FailedAt: time.Unix(210, 0)}
	failures.failure = failure
	view, err = service.Get(context.Background(), created.Service.ID)
	if err != nil {
		t.Fatalf("Get(APPLY_FAILED) error = %v", err)
	}
	if view.Status != serverstatus.ServiceStatusApplyFailed || view.ApplyFailure != failure || view.ActiveConnections != 3 {
		t.Fatalf("Get(APPLY_FAILED) projection = %+v", view)
	}
}

func TestServiceAPINotifierFailurePreservesCommittedFact(t *testing.T) {
	store := openServiceManagementStore(t)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
	notifier := &recordingSnapshotNotifier{err: errors.New("notify failed")}
	service := newServiceAPITestService(
		t, store, &recordingSnapshotGate{}, notifier, &recordingRouteNotifier{},
		serviceManagementIDOne,
	)
	result, err := service.Create(context.Background(), CreateServiceAPIInput{
		Service:  validCreateServiceInput(serviceManagementTunnelID, "committed"),
		Exposure: ServiceExposureInput{Type: ServiceExposureHTTP, Hostname: "committed.example.test"},
	})
	if !errors.Is(err, ErrServiceRuntimeConvergence) {
		t.Fatalf("Create() error = %v, want ErrServiceRuntimeConvergence", err)
	}
	if !result.Changed || result.Service.ID != serviceManagementIDOne || result.Generation != 1 {
		t.Fatalf("Create() lost committed result = %+v", result)
	}
	assertServiceAPIAggregateState(t, store, 1, 1, 1, 1)
}

type serviceAPIRuntimeFake struct {
	snapshots []serverruntime.SessionStatusSnapshot
}

func (runtime *serviceAPIRuntimeFake) RuntimeStatusSnapshots() []serverruntime.SessionStatusSnapshot {
	return runtime.snapshots
}

type serviceAPILimitsFake struct {
	snapshot serverlimits.Snapshot
}

func (limits *serviceAPILimitsFake) Snapshot() serverlimits.Snapshot { return limits.snapshot }

type serviceAPIApplyFailuresFake struct {
	failure *serverstatus.ApplyFailure
}

func (failures *serviceAPIApplyFailuresFake) ServiceApplyFailure(string, uint64) *serverstatus.ApplyFailure {
	return failures.failure
}

func newServiceAPITestService(
	t *testing.T,
	store repository.Store,
	gate TunnelSnapshotGate,
	tunnelNotifier SnapshotReconcileNotifier,
	routeNotifier RouteReconcileNotifier,
	identifiers ...string,
) *ServiceAPIService {
	t.Helper()
	return newServiceAPITestServiceWithProjection(
		t, store, gate, tunnelNotifier, routeNotifier,
		&serviceAPIRuntimeFake{}, &serviceAPILimitsFake{}, &serviceAPIApplyFailuresFake{}, identifiers...,
	)
}

func newServiceAPITestServiceWithProjection(
	t *testing.T,
	store repository.Store,
	gate TunnelSnapshotGate,
	tunnelNotifier SnapshotReconcileNotifier,
	routeNotifier RouteReconcileNotifier,
	runtime ServiceRuntimeOwner,
	limits ServiceLimitOwner,
	failures ServiceApplyFailureOwner,
	identifiers ...string,
) *ServiceAPIService {
	t.Helper()
	owner := newServiceManagementTestServiceWithNotifier(store, gate, tunnelNotifier, identifiers...)
	policy, err := tcpport.New(10000, 10002, nil)
	if err != nil {
		t.Fatalf("tcpport.New() error = %v", err)
	}
	service := NewServiceAPIService(owner, policy, routeNotifier, runtime, limits, failures)
	service.now = func() time.Time { return time.Unix(200, 0) }
	return service
}

func readServiceAPIDesiredState(t *testing.T, store repository.Store) repository.RouteDesiredState {
	t.Helper()
	var state repository.RouteDesiredState
	if err := store.Read(context.Background(), func(view repository.RepositoryView) error {
		var err error
		state, err = view.Routes().LoadDesiredState(context.Background())
		return err
	}); err != nil {
		t.Fatalf("read Service API Desired State error = %v", err)
	}
	return state
}

func assertServiceAPIAggregateState(
	t *testing.T,
	store repository.Store,
	wantRevision, wantServices int64,
	wantHTTPRoutes int,
	wantGeneration uint64,
) {
	t.Helper()
	state := readServiceAPIDesiredState(t, store)
	if len(state.Tunnels) != 1 || state.Tunnels[0].DesiredRevision != wantRevision ||
		int64(len(state.Services)) != wantServices || len(state.HTTPRoutes) != wantHTTPRoutes ||
		len(state.TCPRoutes) != 0 || state.Generation != wantGeneration {
		t.Fatalf("Service API aggregate state = %+v", state)
	}
}

func serviceAPIUint16(value uint16) *uint16 { return &value }
