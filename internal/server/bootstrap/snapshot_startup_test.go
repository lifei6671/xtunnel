package bootstrap

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/lifei6671/xtunnel/internal/healthbudget"
	"github.com/lifei6671/xtunnel/internal/repository"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	"github.com/lifei6671/xtunnel/internal/server/snapshot"
)

const (
	startupFirstTunnelID   = "tun_01ARZ3NDEKTSV4RRFFQ69G5FAV"
	startupSecondTunnelID  = "tun_01ARZ3NDEKTSV4RRFFQ69G5FAW"
	startupFirstServiceID  = "svc_01ARZ3NDEKTSV4RRFFQ69G5FAV"
	startupSecondServiceID = "svc_01ARZ3NDEKTSV4RRFFQ69G5FAW"
	startupThirdServiceID  = "svc_01ARZ3NDEKTSV4RRFFQ69G5FAX"
	startupFourthServiceID = "svc_01ARZ3NDEKTSV4RRFFQ69G5FAY"
	startupFifthServiceID  = "svc_01ARZ3NDEKTSV4RRFFQ69G5FAZ"
	startupFirstConnector  = "con_01ARZ3NDEKTSV4RRFFQ69G5FAV"
	startupSecondConnector = "con_01ARZ3NDEKTSV4RRFFQ69G5FAW"
)

func validateStoredSnapshots(ctx context.Context, config serverconfig.Config, store repository.Store) error {
	_, err := initializeStoredSnapshotsAndHealthBudget(ctx, config, store)
	return err
}

func TestValidateStoredSnapshotsAllowsEmptyStore(t *testing.T) {
	fixture := newSnapshotStartupFixture(nil, nil)

	if err := validateStoredSnapshots(context.Background(), validSnapshotStartupConfig(), fixture.store); err != nil {
		t.Fatalf("validateStoredSnapshots() error = %v", err)
	}
	if fixture.store.readCalls != 1 {
		t.Fatalf("Store.Read() calls = %d, want 1", fixture.store.readCalls)
	}
	if len(fixture.services.listCalls) != 0 {
		t.Fatalf("Services.ListByTunnel() calls = %v, want none", fixture.services.listCalls)
	}
}

func TestValidateStoredSnapshotsUsesStableTunnelOrderWithoutMutatingList(t *testing.T) {
	tunnels := []repository.Tunnel{
		{ID: startupSecondTunnelID, DesiredRevision: 8},
		{ID: startupFirstTunnelID, DesiredRevision: 5},
	}
	original := append([]repository.Tunnel(nil), tunnels...)
	services := map[string][]repository.Service{
		startupFirstTunnelID:  {validStartupService(startupFirstTunnelID, startupFirstServiceID, 5)},
		startupSecondTunnelID: {validStartupService(startupSecondTunnelID, startupSecondServiceID, 8)},
	}
	fixture := newSnapshotStartupFixture(tunnels, services)

	if err := validateStoredSnapshots(context.Background(), validSnapshotStartupConfig(), fixture.store); err != nil {
		t.Fatalf("validateStoredSnapshots() error = %v", err)
	}
	wantOrder := []string{startupFirstTunnelID, startupSecondTunnelID}
	if !reflect.DeepEqual(fixture.services.listCalls, wantOrder) {
		t.Fatalf("Services.ListByTunnel() calls = %v, want %v", fixture.services.listCalls, wantOrder)
	}
	if !reflect.DeepEqual(tunnels, original) {
		t.Fatalf("validator mutated TunnelRepository.List result: got %#v, want %#v", tunnels, original)
	}
}

func TestValidateStoredSnapshotsRejectsConfiguredLimits(t *testing.T) {
	first := validStartupService(startupFirstTunnelID, startupFirstServiceID, 3)
	second := validStartupService(startupFirstTunnelID, startupSecondServiceID, 3)
	tests := []struct {
		name      string
		services  []repository.Service
		mutate    func(*serverconfig.Config)
		wantErr   error
		wantCount int
	}{
		{
			name:      "service count",
			services:  []repository.Service{first, second},
			mutate:    func(config *serverconfig.Config) { config.Limits.MaxServicesPerTunnel = 1 },
			wantErr:   snapshot.ErrServiceLimit,
			wantCount: 2,
		},
		{
			name:      "snapshot bytes",
			services:  []repository.Service{first},
			mutate:    func(config *serverconfig.Config) { config.Limits.MaxTunnelSnapshotBytes = 1 },
			wantErr:   snapshot.ErrSnapshotTooLarge,
			wantCount: 1,
		},
		{
			name:      "control envelope bytes",
			services:  []repository.Service{first},
			mutate:    func(config *serverconfig.Config) { config.Limits.MaxControlFrameBytes = 1 },
			wantErr:   snapshot.ErrControlFrameTooLarge,
			wantCount: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validSnapshotStartupConfig()
			test.mutate(&config)
			fixture := newSnapshotStartupFixture(
				[]repository.Tunnel{{ID: startupFirstTunnelID, DesiredRevision: 3}},
				map[string][]repository.Service{startupFirstTunnelID: test.services},
			)

			err := validateStoredSnapshots(context.Background(), config, fixture.store)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("validateStoredSnapshots() error = %v, want %v", err, test.wantErr)
			}
			if !strings.Contains(err.Error(), "tunnel_id="+startupFirstTunnelID) ||
				!strings.Contains(err.Error(), "service_count="+strconv.Itoa(test.wantCount)) {
				t.Fatalf("error lacks safe Tunnel/count context: %v", err)
			}
		})
	}
}

func TestValidateStoredSnapshotsRejectsInvalidRevisionState(t *testing.T) {
	tests := []struct {
		name     string
		revision int64
		service  func() repository.Service
	}{
		{
			name:     "invalid durable service",
			revision: 3,
			service: func() repository.Service {
				service := validStartupService(startupFirstTunnelID, startupFirstServiceID, 3)
				service.Name = " "
				return service
			},
		},
		{
			name:     "future required revision",
			revision: 2,
			service: func() repository.Service {
				return validStartupService(startupFirstTunnelID, startupFirstServiceID, 3)
			},
		},
		{
			name:     "negative desired revision",
			revision: -1,
			service: func() repository.Service {
				return validStartupService(startupFirstTunnelID, startupFirstServiceID, 0)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSnapshotStartupFixture(
				[]repository.Tunnel{{ID: startupFirstTunnelID, DesiredRevision: test.revision}},
				map[string][]repository.Service{startupFirstTunnelID: {test.service()}},
			)
			err := validateStoredSnapshots(context.Background(), validSnapshotStartupConfig(), fixture.store)
			if !errors.Is(err, snapshot.ErrInvalidSnapshot) {
				t.Fatalf("validateStoredSnapshots() error = %v, want ErrInvalidSnapshot", err)
			}
			if !strings.Contains(err.Error(), "tunnel_id="+startupFirstTunnelID) ||
				!strings.Contains(err.Error(), "service_count=1") {
				t.Fatalf("error lacks safe Tunnel/count context: %v", err)
			}
		})
	}
}

func TestValidateStoredSnapshotsPropagatesRepositoryErrorsAndStops(t *testing.T) {
	errStoreRead := errors.New("read failed")
	errTunnelList := errors.New("tunnel list failed")
	errServiceList := errors.New("service list failed")

	t.Run("store read", func(t *testing.T) {
		fixture := newSnapshotStartupFixture(nil, nil)
		fixture.store.readErr = errStoreRead
		if err := validateStoredSnapshots(context.Background(), validSnapshotStartupConfig(), fixture.store); !errors.Is(err, errStoreRead) {
			t.Fatalf("validateStoredSnapshots() error = %v, want read error", err)
		}
	})

	t.Run("tunnel list", func(t *testing.T) {
		fixture := newSnapshotStartupFixture(nil, nil)
		fixture.tunnels.listErr = errTunnelList
		if err := validateStoredSnapshots(context.Background(), validSnapshotStartupConfig(), fixture.store); !errors.Is(err, errTunnelList) {
			t.Fatalf("validateStoredSnapshots() error = %v, want Tunnel List error", err)
		}
		if len(fixture.services.listCalls) != 0 {
			t.Fatalf("Services.ListByTunnel() called after Tunnel List error: %v", fixture.services.listCalls)
		}
	})

	t.Run("service list", func(t *testing.T) {
		fixture := newSnapshotStartupFixture(
			[]repository.Tunnel{
				{ID: startupSecondTunnelID, DesiredRevision: 0},
				{ID: startupFirstTunnelID, DesiredRevision: 0},
			},
			nil,
		)
		fixture.services.listErrors = map[string]error{startupFirstTunnelID: errServiceList}
		err := validateStoredSnapshots(context.Background(), validSnapshotStartupConfig(), fixture.store)
		if !errors.Is(err, errServiceList) {
			t.Fatalf("validateStoredSnapshots() error = %v, want Service List error", err)
		}
		if want := []string{startupFirstTunnelID}; !reflect.DeepEqual(fixture.services.listCalls, want) {
			t.Fatalf("Services.ListByTunnel() calls = %v, want fast failure %v", fixture.services.listCalls, want)
		}
	})
}

func TestValidateStoredSnapshotsHonorsContextCancellation(t *testing.T) {
	t.Run("before read", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		fixture := newSnapshotStartupFixture(nil, nil)
		err := validateStoredSnapshots(ctx, validSnapshotStartupConfig(), fixture.store)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("validateStoredSnapshots() error = %v, want context.Canceled", err)
		}
		if fixture.store.readCalls != 0 {
			t.Fatalf("Store.Read() calls = %d after pre-cancel, want 0", fixture.store.readCalls)
		}
	})

	t.Run("after tunnel list", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		fixture := newSnapshotStartupFixture(
			[]repository.Tunnel{{ID: startupFirstTunnelID, DesiredRevision: 0}},
			nil,
		)
		fixture.tunnels.afterList = cancel
		err := validateStoredSnapshots(ctx, validSnapshotStartupConfig(), fixture.store)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("validateStoredSnapshots() error = %v, want context.Canceled", err)
		}
		if len(fixture.services.listCalls) != 0 {
			t.Fatalf("Services.ListByTunnel() called after cancellation: %v", fixture.services.listCalls)
		}
	})
}

func TestInitializeStoredSnapshotsAndHealthBudget(t *testing.T) {
	firstScheduled := validStartupService(startupFirstTunnelID, startupFirstServiceID, 5)
	firstScheduled.Health = validStartupHealth()
	secondScheduled := validStartupService(startupFirstTunnelID, startupSecondServiceID, 5)
	secondScheduled.Health = validStartupHealth()
	withoutHealth := validStartupService(startupFirstTunnelID, startupThirdServiceID, 5)
	disabled := validStartupService(startupFirstTunnelID, startupFourthServiceID, 5)
	disabled.Health = validStartupHealth()
	disabled.Enabled = false
	otherTunnel := validStartupService(startupSecondTunnelID, startupFifthServiceID, 8)
	otherTunnel.Health = validStartupHealth()

	tests := []struct {
		name       string
		perTunnel  int
		global     int
		acquire    []healthbudget.ConnectorKey
		fail       *healthbudget.ConnectorKey
		wantGlobal uint64
	}{
		{
			name: "multiple tunnels aggregate only scheduled services", perTunnel: 4, global: 5,
			acquire: []healthbudget.ConnectorKey{
				{TunnelID: startupFirstTunnelID, ConnectorID: startupFirstConnector},
				{TunnelID: startupSecondTunnelID, ConnectorID: startupFirstConnector},
			},
			wantGlobal: 3,
		},
		{
			name: "per tunnel exceeded", perTunnel: 3, global: 10,
			acquire: []healthbudget.ConnectorKey{
				{TunnelID: startupFirstTunnelID, ConnectorID: startupFirstConnector},
			},
			fail:       &healthbudget.ConnectorKey{TunnelID: startupFirstTunnelID, ConnectorID: startupSecondConnector},
			wantGlobal: 2,
		},
		{
			name: "global exceeded across tunnels", perTunnel: 4, global: 4,
			acquire: []healthbudget.ConnectorKey{
				{TunnelID: startupFirstTunnelID, ConnectorID: startupFirstConnector},
				{TunnelID: startupSecondTunnelID, ConnectorID: startupFirstConnector},
			},
			fail:       &healthbudget.ConnectorKey{TunnelID: startupFirstTunnelID, ConnectorID: startupSecondConnector},
			wantGlobal: 3,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSnapshotStartupFixture(
				[]repository.Tunnel{
					{ID: startupSecondTunnelID, DesiredRevision: 8},
					{ID: startupFirstTunnelID, DesiredRevision: 5},
				},
				map[string][]repository.Service{
					startupFirstTunnelID:  {firstScheduled, secondScheduled, withoutHealth, disabled},
					startupSecondTunnelID: {otherTunnel},
				},
			)
			config := validSnapshotStartupConfig()
			config.Limits.MaxHealthTargetsPerTunnel = test.perTunnel
			config.Limits.MaxHealthTargetsGlobal = test.global
			budget, err := initializeStoredSnapshotsAndHealthBudget(context.Background(), config, fixture.store)
			if err != nil {
				t.Fatal(err)
			}
			if fixture.store.readCalls != 1 {
				t.Fatalf("Store.Read() calls = %d, want one shared startup view", fixture.store.readCalls)
			}
			initial := budget.Snapshot()
			if initial.Tunnels[startupFirstTunnelID].Revision != 5 ||
				initial.Tunnels[startupFirstTunnelID].EnabledCount != 2 ||
				initial.Tunnels[startupSecondTunnelID].Revision != 8 ||
				initial.Tunnels[startupSecondTunnelID].EnabledCount != 1 {
				t.Fatalf("initialized Health budget = %+v", initial)
			}
			leases := make([]*healthbudget.ConnectorLease, 0, len(test.acquire))
			for _, key := range test.acquire {
				lease, err := budget.AcquireConnector(key.TunnelID, key.ConnectorID)
				if err != nil {
					t.Fatalf("AcquireConnector(%+v) error = %v", key, err)
				}
				leases = append(leases, lease)
			}
			if test.fail != nil {
				if _, err := budget.AcquireConnector(test.fail.TunnelID, test.fail.ConnectorID); !errors.Is(err, healthbudget.ErrTargetCapacity) {
					t.Fatalf("AcquireConnector(over capacity %+v) error = %v, want ErrTargetCapacity", *test.fail, err)
				}
			}
			if got := budget.Snapshot().TargetsGlobal; got != test.wantGlobal {
				t.Fatalf("TargetsGlobal = %d, want %d", got, test.wantGlobal)
			}
			for _, lease := range leases {
				lease.Release()
			}
		})
	}
}

func TestInitializeStoredSnapshotsAndHealthBudgetReturnsNoManagerOnFailure(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*snapshotStartupFixture) context.Context
		want  error
	}{
		{
			name: "repository", want: errSnapshotStartupRead,
			setup: func(fixture *snapshotStartupFixture) context.Context {
				fixture.store.readErr = errSnapshotStartupRead
				return context.Background()
			},
		},
		{
			name: "context", want: context.Canceled,
			setup: func(*snapshotStartupFixture) context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSnapshotStartupFixture(nil, nil)
			ctx := test.setup(&fixture)
			manager, err := initializeStoredSnapshotsAndHealthBudget(ctx, validSnapshotStartupConfig(), fixture.store)
			if manager != nil || !errors.Is(err, test.want) {
				t.Fatalf("initializeStoredSnapshotsAndHealthBudget() = (%v, %v), want (nil, %v)", manager, err, test.want)
			}
		})
	}
}

type snapshotStartupFixture struct {
	store    *snapshotStartupStore
	tunnels  *snapshotStartupTunnelRepository
	services *snapshotStartupServiceRepository
}

func newSnapshotStartupFixture(
	tunnels []repository.Tunnel,
	services map[string][]repository.Service,
) snapshotStartupFixture {
	tunnelRepository := &snapshotStartupTunnelRepository{tunnels: tunnels}
	serviceRepository := &snapshotStartupServiceRepository{services: services}
	view := snapshotStartupView{tunnels: tunnelRepository, services: serviceRepository}
	return snapshotStartupFixture{
		store:    &snapshotStartupStore{view: view},
		tunnels:  tunnelRepository,
		services: serviceRepository,
	}
}

type snapshotStartupStore struct {
	repository.Store
	view      repository.RepositoryView
	readErr   error
	readCalls int
}

func (store *snapshotStartupStore) Read(_ context.Context, callback func(repository.RepositoryView) error) error {
	store.readCalls++
	if store.readErr != nil {
		return store.readErr
	}
	return callback(store.view)
}

type snapshotStartupView struct {
	tunnels  repository.TunnelRepository
	services repository.ServiceRepository
}

func (view snapshotStartupView) Tunnels() repository.TunnelRepository {
	return view.tunnels
}

func (snapshotStartupView) TunnelTokens() repository.TunnelTokenRepository {
	return nil
}

func (view snapshotStartupView) Services() repository.ServiceRepository {
	return view.services
}

func (snapshotStartupView) Routes() repository.RouteRepository {
	return nil
}

func (snapshotStartupView) Usage() repository.UsageRepository {
	return nil
}

type snapshotStartupTunnelRepository struct {
	repository.TunnelRepository
	tunnels   []repository.Tunnel
	listErr   error
	afterList func()
}

func (repo *snapshotStartupTunnelRepository) List(context.Context) ([]repository.Tunnel, error) {
	if repo.afterList != nil {
		repo.afterList()
	}
	return repo.tunnels, repo.listErr
}

type snapshotStartupServiceRepository struct {
	repository.ServiceRepository
	services   map[string][]repository.Service
	listErrors map[string]error
	listCalls  []string
}

func (repo *snapshotStartupServiceRepository) ListByTunnel(_ context.Context, tunnelID string) ([]repository.Service, error) {
	repo.listCalls = append(repo.listCalls, tunnelID)
	if err := repo.listErrors[tunnelID]; err != nil {
		return nil, err
	}
	return repo.services[tunnelID], nil
}

func validSnapshotStartupConfig() serverconfig.Config {
	return serverconfig.Config{Limits: serverconfig.Limits{
		MaxServicesPerTunnel:      10,
		MaxTunnelSnapshotBytes:    snapshot.MaxTunnelSnapshotSize,
		MaxControlFrameBytes:      1 << 20,
		MaxHealthTargetsPerTunnel: 10,
		MaxHealthTargetsGlobal:    20,
	}}
}

var errSnapshotStartupRead = errors.New("startup repository read failed")

func validStartupHealth() *repository.HealthCheck {
	return &repository.HealthCheck{
		Type: repository.HealthTypeTCP, IntervalMS: 1_000, TimeoutMS: 100,
		FailureThreshold: 1, SuccessThreshold: 1,
	}
}

func validStartupService(tunnelID, serviceID string, requiredRevision int64) repository.Service {
	return repository.Service{
		ID:               serviceID,
		TunnelID:         tunnelID,
		Name:             "origin",
		RequiredRevision: requiredRevision,
		OriginScheme:     repository.OriginSchemeHTTP,
		OriginHost:       "127.0.0.1",
		OriginPort:       8080,
		ConnectTimeoutMS: 5_000,
		Enabled:          true,
		Version:          1,
		CreatedAt:        1,
		UpdatedAt:        1,
	}
}
