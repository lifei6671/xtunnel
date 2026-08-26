package sqlite

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/lifei6671/xtunnel/internal/repository"
)

const (
	serviceTestTunnelIDTwo = "tun_01J00000000000000000000001"
	serviceTestIDOne       = "svc_01J00000000000000000000001"
	serviceTestIDTwo       = "svc_01J00000000000000000000002"
	serviceTestIDThree     = "svc_01J00000000000000000000003"
)

func TestServiceRepositoryCRUDAndStableOrdering(t *testing.T) {
	store := openServiceTestStore(t)
	seedServiceTestTunnel(t, store, repositoryTestTunnelID)

	third := testService(serviceTestIDThree, repositoryTestTunnelID)
	second := testService(serviceTestIDTwo, repositoryTestTunnelID)
	first := testService(serviceTestIDOne, repositoryTestTunnelID)
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		services := transaction.Services()
		for _, service := range []repository.Service{third, second, first} {
			if err := services.Create(context.Background(), service); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("create Services error = %v", err)
	}

	services := serviceRepository{database: store.database}
	listed, err := services.ListByTunnel(context.Background(), repositoryTestTunnelID)
	if err != nil {
		t.Fatalf("ListByTunnel() error = %v", err)
	}
	if len(listed) != 3 {
		t.Fatalf("ListByTunnel() length = %d, want 3", len(listed))
	}
	if got := []string{listed[0].ID, listed[1].ID, listed[2].ID}; !reflect.DeepEqual(got, []string{serviceTestIDOne, serviceTestIDTwo, serviceTestIDThree}) {
		t.Fatalf("ListByTunnel() IDs = %v, want stable ascending order", got)
	}
	count, err := services.CountByTunnel(context.Background(), repositoryTestTunnelID)
	if err != nil || count != 3 {
		t.Fatalf("CountByTunnel() = (%d, %v), want (3, nil)", count, err)
	}
	got, err := services.Get(context.Background(), repositoryTestTunnelID, serviceTestIDOne)
	if err != nil || !reflect.DeepEqual(got, first) {
		t.Fatalf("Get() = (%#v, %v), want %#v", got, err, first)
	}

	first.Name = "updated"
	first.RequiredRevision = 2
	first.OriginScheme = repository.OriginSchemeHTTPS
	first.TLSVerify = true
	first.TLSServerName = "origin.example.test"
	first.OriginHTTPHost = "app.example.test"
	first.Health = &repository.HealthCheck{
		Type: repository.HealthTypeHTTP, Path: "/ready", IntervalMS: 10_000, TimeoutMS: 2_000,
		ExpectedStatusMin: 200, ExpectedStatusMax: 399, FailureThreshold: 3, SuccessThreshold: 2,
	}
	first.UpdatedAt = 2
	updated, err := services.Update(context.Background(), first, 1)
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	first.Version = 2
	if !reflect.DeepEqual(updated, first) {
		t.Fatalf("Update() = %#v, want %#v", updated, first)
	}
	if err := services.Delete(context.Background(), repositoryTestTunnelID, serviceTestIDOne, 1); !errors.Is(err, repository.ErrVersionConflict) {
		t.Fatalf("Delete(stale) error = %v, want ErrVersionConflict", err)
	}
	if err := services.Delete(context.Background(), repositoryTestTunnelID, serviceTestIDOne, 2); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := services.Get(context.Background(), repositoryTestTunnelID, serviceTestIDOne); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("Get(deleted) error = %v, want ErrNotFound", err)
	}
}

func TestServiceRepositoryNullableHealthRoundTrip(t *testing.T) {
	store := openServiceTestStore(t)
	seedServiceTestTunnel(t, store, repositoryTestTunnelID)

	tests := []struct {
		name    string
		service repository.Service
		check   func(*testing.T, serviceRecord)
	}{
		{
			name:    "Disabled",
			service: testService(serviceTestIDOne, repositoryTestTunnelID),
			check: func(t *testing.T, record serviceRecord) {
				if record.HealthType != nil || record.HealthPath != nil || record.HealthIntervalMS != nil ||
					record.HealthTimeoutMS != nil || record.HealthExpectedStatusMin != nil || record.HealthExpectedStatusMax != nil ||
					record.HealthFailureThreshold != nil || record.HealthSuccessThreshold != nil {
					t.Fatalf("disabled Health record contains non-NULL fields: %#v", record)
				}
			},
		},
		{
			name: "TCP",
			service: func() repository.Service {
				service := testService(serviceTestIDTwo, repositoryTestTunnelID)
				service.OriginScheme = repository.OriginSchemeTCP
				service.Health = &repository.HealthCheck{
					Type: repository.HealthTypeTCP, IntervalMS: 10_000, TimeoutMS: 2_000,
					FailureThreshold: 3, SuccessThreshold: 2,
				}
				return service
			}(),
			check: func(t *testing.T, record serviceRecord) {
				if record.HealthType == nil || *record.HealthType != string(repository.HealthTypeTCP) || record.HealthPath != nil ||
					record.HealthExpectedStatusMin != nil || record.HealthExpectedStatusMax != nil {
					t.Fatalf("TCP Health nullable fields = %#v", record)
				}
			},
		},
		{
			name: "HTTP and optional Origin names",
			service: func() repository.Service {
				service := testService(serviceTestIDThree, repositoryTestTunnelID)
				service.OriginHTTPHost = "backend.example.test"
				service.Health = &repository.HealthCheck{
					Type: repository.HealthTypeHTTP, Path: "/health", IntervalMS: 10_000, TimeoutMS: 2_000,
					ExpectedStatusMin: 200, ExpectedStatusMax: 399, FailureThreshold: 3, SuccessThreshold: 2,
				}
				return service
			}(),
			check: func(t *testing.T, record serviceRecord) {
				if record.HealthType == nil || *record.HealthType != string(repository.HealthTypeHTTP) ||
					record.HealthPath == nil || *record.HealthPath != "/health" || record.OriginHTTPHost == nil {
					t.Fatalf("HTTP Health record = %#v", record)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			services := serviceRepository{database: store.database}
			if err := services.Create(context.Background(), test.service); err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			got, err := services.Get(context.Background(), test.service.TunnelID, test.service.ID)
			if err != nil || !reflect.DeepEqual(got, test.service) {
				t.Fatalf("Get() = (%#v, %v), want %#v", got, err, test.service)
			}
			var record serviceRecord
			if err := store.database.Where(ServiceColumns.ID+" = ?", test.service.ID).Take(&record).Error; err != nil {
				t.Fatalf("read service record error = %v", err)
			}
			test.check(t, record)
		})
	}
}

func TestServiceRepositoryFencesTunnelAndVersion(t *testing.T) {
	store := openServiceTestStore(t)
	seedServiceTestTunnel(t, store, repositoryTestTunnelID)
	seedServiceTestTunnel(t, store, serviceTestTunnelIDTwo)
	service := testService(serviceTestIDOne, repositoryTestTunnelID)
	services := serviceRepository{database: store.database}
	if err := services.Create(context.Background(), service); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if _, err := services.Get(context.Background(), serviceTestTunnelIDTwo, service.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("cross-Tunnel Get() error = %v, want ErrNotFound", err)
	}
	wrongTunnel := service
	wrongTunnel.TunnelID = serviceTestTunnelIDTwo
	if _, err := services.Update(context.Background(), wrongTunnel, 1); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("cross-Tunnel Update() error = %v, want ErrNotFound", err)
	}
	if err := services.Delete(context.Background(), serviceTestTunnelIDTwo, service.ID, 1); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("cross-Tunnel Delete() error = %v, want ErrNotFound", err)
	}

	stale := service
	stale.Version = 2
	stale.UpdatedAt = 2
	if _, err := services.Update(context.Background(), stale, 2); !errors.Is(err, repository.ErrVersionConflict) {
		t.Fatalf("Update(stale) error = %v, want ErrVersionConflict", err)
	}
	if _, err := services.Update(context.Background(), service, 2); !errors.Is(err, repository.ErrInvalidService) {
		t.Fatalf("Update(version mismatch) error = %v, want ErrInvalidService", err)
	}
	if err := services.Delete(context.Background(), repositoryTestTunnelID, service.ID, 2); !errors.Is(err, repository.ErrVersionConflict) {
		t.Fatalf("Delete(stale) error = %v, want ErrVersionConflict", err)
	}
	count, err := services.CountByTunnel(context.Background(), repositoryTestTunnelID)
	if err != nil || count != 1 {
		t.Fatalf("CountByTunnel() after fenced operations = (%d, %v), want (1, nil)", count, err)
	}
}

func TestServiceRepositoryRejectsInvalidDomainAndPreservesForeignKey(t *testing.T) {
	store := openServiceTestStore(t)
	services := serviceRepository{database: store.database}

	invalid := testService("svc_invalid", repositoryTestTunnelID)
	if err := services.Create(context.Background(), invalid); !errors.Is(err, repository.ErrInvalidService) {
		t.Fatalf("Create(invalid) error = %v, want ErrInvalidService", err)
	}
	missingParent := testService(serviceTestIDOne, repositoryTestTunnelID)
	if err := services.Create(context.Background(), missingParent); err == nil {
		t.Fatal("Create() with missing Tunnel error = nil")
	}

	seedServiceTestTunnel(t, store, repositoryTestTunnelID)
	if err := services.Create(context.Background(), missingParent); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	result := store.database.Where(TunnelColumns.ID+" = ?", repositoryTestTunnelID).Delete(&tunnelRecord{})
	if result.Error == nil {
		t.Fatal("deleting a Tunnel with a Service did not enforce ON DELETE RESTRICT")
	}
	if err := services.Delete(context.Background(), repositoryTestTunnelID, missingParent.ID, missingParent.Version); err != nil {
		t.Fatalf("Delete(Service) error = %v", err)
	}
	result = store.database.Where(TunnelColumns.ID+" = ?", repositoryTestTunnelID).Delete(&tunnelRecord{})
	if result.Error != nil || result.RowsAffected != 1 {
		t.Fatalf("delete empty Tunnel = (%d, %v), want (1, nil)", result.RowsAffected, result.Error)
	}
}

func openServiceTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	return store
}

func seedServiceTestTunnel(t *testing.T, store *Store, tunnelID string) {
	t.Helper()
	tunnel := testTunnel()
	tunnel.ID = tunnelID
	tunnel.Name = tunnelID
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		return transaction.Tunnels().Create(context.Background(), tunnel)
	}); err != nil {
		t.Fatalf("seed Tunnel %s error = %v", tunnelID, err)
	}
}

func testService(serviceID, tunnelID string) repository.Service {
	return repository.Service{
		ID: serviceID, TunnelID: tunnelID, Name: serviceID, RequiredRevision: 1,
		OriginScheme: repository.OriginSchemeHTTP, OriginHost: "127.0.0.1", OriginPort: 8080,
		ConnectTimeoutMS: 5_000, Enabled: true, Version: 1, CreatedAt: 1, UpdatedAt: 1,
	}
}
