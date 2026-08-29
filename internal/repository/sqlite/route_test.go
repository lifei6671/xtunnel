package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lifei6671/xtunnel/internal/repository"
	"gorm.io/gorm"
)

const (
	repositoryRouteTestServiceID       = "svc_01J00000000000000000000000"
	repositoryRouteSecondTestServiceID = "svc_01J00000000000000000000001"
	repositoryRouteThirdTestServiceID  = "svc_01J00000000000000000000003"
)

func TestRouteMigrationUpgradesV6AndIsIdempotent(t *testing.T) {
	database := openUnmigratedDatabase(t)
	if err := runMigrations(context.Background(), database, productionMigrations[:6], testNow); err != nil {
		t.Fatalf("run v6 migrations error = %v", err)
	}
	if err := database.Create(tunnelRecordFromDomain(testTunnel())).Error; err != nil {
		t.Fatalf("seed v6 Tunnel error = %v", err)
	}
	if err := database.Exec(
		`INSERT INTO services(id, tunnel_id, name, origin_scheme, origin_host, origin_port, created_at, updated_at)
		 VALUES (?, ?, 'api', 'http', '127.0.0.1', 8080, 1, 1)`,
		repositoryRouteTestServiceID, repositoryTestTunnelID,
	).Error; err != nil {
		t.Fatalf("seed v6 Service error = %v", err)
	}

	if err := runMigrations(context.Background(), database, productionMigrations[:7], testNow); err != nil {
		t.Fatalf("upgrade to v7 error = %v", err)
	}
	for _, table := range []string{RouteConfigStateTable, HTTPRouteTable, TCPRouteTable} {
		var count int64
		if err := database.Raw(
			"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table,
		).Scan(&count).Error; err != nil {
			t.Fatalf("inspect table %q error = %v", table, err)
		}
		if count != 1 {
			t.Fatalf("table %q count = %d, want 1", table, count)
		}
	}
	var state routeConfigStateRecord
	if err := database.Take(&state, singletonRouteConfigStateID).Error; err != nil {
		t.Fatalf("read initial Route Generation error = %v", err)
	}
	if state.Generation != 0 {
		t.Fatalf("initial Route Generation = %d, want 0", state.Generation)
	}
	var tunnelCount, serviceCount int64
	if err := database.Model(&tunnelRecord{}).Count(&tunnelCount).Error; err != nil {
		t.Fatalf("count preserved Tunnel error = %v", err)
	}
	if err := database.Model(&serviceRecord{}).Count(&serviceCount).Error; err != nil {
		t.Fatalf("count preserved Service error = %v", err)
	}
	if tunnelCount != 1 || serviceCount != 1 {
		t.Fatalf("preserved desired state = tunnels:%d services:%d, want 1/1", tunnelCount, serviceCount)
	}

	if err := runMigrations(context.Background(), database, productionMigrations[:7], testNow); err != nil {
		t.Fatalf("idempotent v7 rerun error = %v", err)
	}
	var versionCount int64
	if err := database.Table("schema_migrations").Count(&versionCount).Error; err != nil {
		t.Fatalf("count versions error = %v", err)
	}
	if versionCount != 7 {
		t.Fatalf("version count = %d, want 7", versionCount)
	}
}

func TestRouteMigrationRollsBackAtomically(t *testing.T) {
	database := openUnmigratedDatabase(t)
	if err := runMigrations(context.Background(), database, productionMigrations[:6], testNow); err != nil {
		t.Fatalf("run v6 migrations error = %v", err)
	}

	failed := append([]migration{}, productionMigrations[:6]...)
	statements := append([]string{}, productionMigrations[6].statements...)
	statements = append(statements, "THIS IS NOT VALID SQL")
	failed = append(failed, migration{version: 7, statements: statements})
	if err := runMigrations(context.Background(), database, failed, testNow); err == nil {
		t.Fatal("failed v7 migration error = nil")
	}

	for _, table := range []string{RouteConfigStateTable, HTTPRouteTable, TCPRouteTable} {
		var count int64
		if err := database.Raw(
			"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table,
		).Scan(&count).Error; err != nil {
			t.Fatalf("inspect rolled-back table %q error = %v", table, err)
		}
		if count != 0 {
			t.Fatalf("failed migration left table %q behind", table)
		}
	}
	var versionCount int64
	if err := database.Table("schema_migrations").Count(&versionCount).Error; err != nil {
		t.Fatalf("count versions after failed v7 error = %v", err)
	}
	if versionCount != 6 {
		t.Fatalf("version count after failed v7 = %d, want 6", versionCount)
	}
}

func TestServiceExposureMigrationUpgradesV9AndIsIdempotent(t *testing.T) {
	database := openUnmigratedDatabase(t)
	if err := runMigrations(context.Background(), database, productionMigrations[:9], testNow); err != nil {
		t.Fatalf("run v9 migrations error = %v", err)
	}
	seedServiceExposureMigrationRelations(t, database)
	if err := database.Create(&httpRouteRecord{
		ID: "http-main", ServiceID: repositoryRouteTestServiceID,
		Hostname: "example.test", PathPrefix: "/", PreserveHost: true,
		Enabled: true, CreatedAt: 1, UpdatedAt: 1,
	}).Error; err != nil {
		t.Fatalf("seed v9 HTTP exposure error = %v", err)
	}
	if err := database.Create(&tcpRouteRecord{
		ID: "tcp-main", ServiceID: repositoryRouteSecondTestServiceID,
		PublicPort: 8443, Enabled: true, CreatedAt: 1, UpdatedAt: 1,
	}).Error; err != nil {
		t.Fatalf("seed v9 TCP exposure error = %v", err)
	}

	if err := runMigrations(context.Background(), database, productionMigrations, testNow); err != nil {
		t.Fatalf("upgrade to v10 error = %v", err)
	}
	var indexCount, triggerCount int64
	if err := database.Raw(
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name IN (?, ?)",
		"http_routes_unique_service_exposure", "tcp_routes_unique_service_exposure",
	).Scan(&indexCount).Error; err != nil {
		t.Fatalf("inspect Exposure indexes error = %v", err)
	}
	if err := database.Raw(
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name LIKE '%_reject_%_exposure_%'",
	).Scan(&triggerCount).Error; err != nil {
		t.Fatalf("inspect Exposure triggers error = %v", err)
	}
	if indexCount != 2 || triggerCount != 4 {
		t.Fatalf("Exposure constraints = indexes:%d triggers:%d, want 2/4", indexCount, triggerCount)
	}
	if err := runMigrations(context.Background(), database, productionMigrations, testNow); err != nil {
		t.Fatalf("idempotent v10 rerun error = %v", err)
	}
	var versionCount int64
	if err := database.Table("schema_migrations").Count(&versionCount).Error; err != nil {
		t.Fatalf("count versions error = %v", err)
	}
	if versionCount != 10 {
		t.Fatalf("version count = %d, want 10", versionCount)
	}
}

func TestServiceExposureMigrationRejectsLegacyDuplicatesAtomically(t *testing.T) {
	tests := []struct {
		name string
		seed func(*testing.T, *gorm.DB)
	}{
		{
			name: "duplicate HTTP",
			seed: func(t *testing.T, database *gorm.DB) {
				t.Helper()
				for _, route := range []httpRouteRecord{
					{ID: "http-one", ServiceID: repositoryRouteTestServiceID, Hostname: "one.test", PathPrefix: "/", CreatedAt: 1, UpdatedAt: 1},
					{ID: "http-two", ServiceID: repositoryRouteTestServiceID, Hostname: "two.test", PathPrefix: "/", CreatedAt: 1, UpdatedAt: 1},
				} {
					if err := database.Create(&route).Error; err != nil {
						t.Fatalf("seed duplicate HTTP exposure error = %v", err)
					}
				}
			},
		},
		{
			name: "duplicate TCP",
			seed: func(t *testing.T, database *gorm.DB) {
				t.Helper()
				for _, route := range []tcpRouteRecord{
					{ID: "tcp-one", ServiceID: repositoryRouteTestServiceID, PublicPort: 8443, CreatedAt: 1, UpdatedAt: 1},
					{ID: "tcp-two", ServiceID: repositoryRouteTestServiceID, PublicPort: 9443, CreatedAt: 1, UpdatedAt: 1},
				} {
					if err := database.Create(&route).Error; err != nil {
						t.Fatalf("seed duplicate TCP exposure error = %v", err)
					}
				}
			},
		},
		{
			name: "cross type",
			seed: func(t *testing.T, database *gorm.DB) {
				t.Helper()
				if err := database.Create(&httpRouteRecord{
					ID: "http-main", ServiceID: repositoryRouteTestServiceID,
					Hostname: "example.test", PathPrefix: "/", CreatedAt: 1, UpdatedAt: 1,
				}).Error; err != nil {
					t.Fatalf("seed HTTP exposure error = %v", err)
				}
				if err := database.Create(&tcpRouteRecord{
					ID: "tcp-main", ServiceID: repositoryRouteTestServiceID,
					PublicPort: 8443, CreatedAt: 1, UpdatedAt: 1,
				}).Error; err != nil {
					t.Fatalf("seed TCP exposure error = %v", err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := openUnmigratedDatabase(t)
			if err := runMigrations(context.Background(), database, productionMigrations[:9], testNow); err != nil {
				t.Fatalf("run v9 migrations error = %v", err)
			}
			seedServiceExposureMigrationRelations(t, database)
			test.seed(t, database)

			if err := runMigrations(context.Background(), database, productionMigrations, testNow); err == nil {
				t.Fatal("upgrade with duplicate Exposure error = nil")
			}
			var versionCount, constraintCount int64
			if err := database.Table("schema_migrations").Count(&versionCount).Error; err != nil {
				t.Fatalf("count versions after rejected v10 error = %v", err)
			}
			if err := database.Raw(
				"SELECT COUNT(*) FROM sqlite_master WHERE name IN (?, ?, ?)",
				"http_routes_unique_service_exposure",
				"tcp_routes_unique_service_exposure",
				"http_routes_reject_tcp_exposure_insert",
			).Scan(&constraintCount).Error; err != nil {
				t.Fatalf("inspect rolled-back Exposure constraints error = %v", err)
			}
			if versionCount != 9 || constraintCount != 0 {
				t.Fatalf("rejected v10 state = versions:%d constraints:%d, want 9/0", versionCount, constraintCount)
			}
		})
	}
}

func TestServiceExposureMigrationRollsBackConstraintsAtomically(t *testing.T) {
	database := openUnmigratedDatabase(t)
	if err := runMigrations(context.Background(), database, productionMigrations[:9], testNow); err != nil {
		t.Fatalf("run v9 migrations error = %v", err)
	}
	failed := append([]migration{}, productionMigrations[:9]...)
	statements := append([]string{}, productionMigrations[9].statements...)
	statements = append(statements, "THIS IS NOT VALID SQL")
	failed = append(failed, migration{
		version: 10, prepare: productionMigrations[9].prepare, statements: statements,
	})
	if err := runMigrations(context.Background(), database, failed, testNow); err == nil {
		t.Fatal("interrupted v10 migration error = nil")
	}

	var versionCount, constraintCount int64
	if err := database.Table("schema_migrations").Count(&versionCount).Error; err != nil {
		t.Fatalf("count versions after interrupted v10 error = %v", err)
	}
	if err := database.Raw(
		`SELECT COUNT(*) FROM sqlite_master
		 WHERE name IN (?, ?, ?, ?, ?, ?)`,
		"http_routes_unique_service_exposure",
		"tcp_routes_unique_service_exposure",
		"http_routes_reject_tcp_exposure_insert",
		"http_routes_reject_tcp_exposure_update",
		"tcp_routes_reject_http_exposure_insert",
		"tcp_routes_reject_http_exposure_update",
	).Scan(&constraintCount).Error; err != nil {
		t.Fatalf("inspect rolled-back v10 constraints error = %v", err)
	}
	if versionCount != 9 || constraintCount != 0 {
		t.Fatalf("interrupted v10 state = versions:%d constraints:%d, want 9/0", versionCount, constraintCount)
	}
}

func TestRouteMigrationEnforcesDesiredStateConstraints(t *testing.T) {
	store := openRouteTestStore(t)
	seedRouteTestService(t, store)
	seedAdditionalRouteTestService(t, store, repositoryRouteSecondTestServiceID)
	seedAdditionalRouteTestService(t, store, repositoryRouteThirdTestServiceID)

	if err := store.database.Exec(
		`INSERT INTO http_routes(id, service_id, hostname, path_prefix, preserve_host, enabled, created_at, updated_at)
		 VALUES ('http-main', ?, 'example.test', '/', 1, 1, 1, 1)`,
		repositoryRouteTestServiceID,
	).Error; err != nil {
		t.Fatalf("insert valid HTTP Route error = %v", err)
	}
	if err := store.database.Exec(
		`INSERT INTO tcp_routes(id, service_id, public_port, enabled, created_at, updated_at)
		 VALUES ('tcp-main', ?, 8443, 1, 1, 1)`,
		repositoryRouteSecondTestServiceID,
	).Error; err != nil {
		t.Fatalf("insert valid TCP Route error = %v", err)
	}

	tests := []struct {
		name string
		sql  string
		args []any
	}{
		{
			name: "duplicate HTTP service",
			sql:  `INSERT INTO http_routes(id, service_id, hostname, path_prefix, created_at, updated_at) VALUES ('http-duplicate', ?, 'another.test', '/', 1, 1)`,
			args: []any{repositoryRouteTestServiceID},
		},
		{
			name: "HTTP conflicts with TCP service",
			sql:  `INSERT INTO http_routes(id, service_id, hostname, path_prefix, created_at, updated_at) VALUES ('http-cross', ?, 'cross.test', '/', 1, 1)`,
			args: []any{repositoryRouteSecondTestServiceID},
		},
		{
			name: "HTTP route missing Service",
			sql:  `INSERT INTO http_routes(id, service_id, hostname, path_prefix, created_at, updated_at) VALUES ('http-orphan', 'svc_01J00000000000000000000002', 'orphan.test', '/', 1, 1)`,
		},
		{
			name: "HTTP path is not absolute",
			sql:  `INSERT INTO http_routes(id, service_id, hostname, path_prefix, created_at, updated_at) VALUES ('http-path', ?, 'path.test', 'api', 1, 1)`,
			args: []any{repositoryRouteThirdTestServiceID},
		},
		{
			name: "HTTP boolean out of range",
			sql:  `INSERT INTO http_routes(id, service_id, hostname, path_prefix, preserve_host, created_at, updated_at) VALUES ('http-bool', ?, 'bool.test', '/', 2, 1, 1)`,
			args: []any{repositoryRouteThirdTestServiceID},
		},
		{
			name: "duplicate TCP public port",
			sql:  `INSERT INTO tcp_routes(id, service_id, public_port, created_at, updated_at) VALUES ('tcp-duplicate', ?, 8443, 1, 1)`,
			args: []any{repositoryRouteThirdTestServiceID},
		},
		{
			name: "duplicate TCP service",
			sql:  `INSERT INTO tcp_routes(id, service_id, public_port, created_at, updated_at) VALUES ('tcp-service', ?, 9443, 1, 1)`,
			args: []any{repositoryRouteSecondTestServiceID},
		},
		{
			name: "TCP conflicts with HTTP service",
			sql:  `INSERT INTO tcp_routes(id, service_id, public_port, created_at, updated_at) VALUES ('tcp-cross', ?, 10443, 1, 1)`,
			args: []any{repositoryRouteTestServiceID},
		},
		{
			name: "TCP port out of range",
			sql:  `INSERT INTO tcp_routes(id, service_id, public_port, created_at, updated_at) VALUES ('tcp-port', ?, 65536, 1, 1)`,
			args: []any{repositoryRouteThirdTestServiceID},
		},
		{
			name: "TCP route missing Service",
			sql:  `INSERT INTO tcp_routes(id, service_id, public_port, created_at, updated_at) VALUES ('tcp-orphan', 'svc_01J00000000000000000000002', 9443, 1, 1)`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := store.database.Exec(test.sql, test.args...).Error; err == nil {
				t.Fatal("invalid Route row was accepted")
			}
		})
	}
	if err := store.database.Model(&httpRouteRecord{}).Where("id = ?", "http-main").
		Update("service_id", repositoryRouteSecondTestServiceID).Error; err == nil {
		t.Fatal("HTTP Exposure update accepted a Service with TCP Exposure")
	}
	if err := store.database.Model(&tcpRouteRecord{}).Where("id = ?", "tcp-main").
		Update("service_id", repositoryRouteTestServiceID).Error; err == nil {
		t.Fatal("TCP Exposure update accepted a Service with HTTP Exposure")
	}

	if err := store.database.Model(&routeConfigStateRecord{}).
		Where("id = ?", singletonRouteConfigStateID).
		Update("generation", -1).Error; err == nil {
		t.Fatal("route_config_state accepted a negative generation")
	}
	if err := store.database.Create(&routeConfigStateRecord{ID: 2, Generation: 0}).Error; err == nil {
		t.Fatal("route_config_state accepted a second generation row")
	}
	if err := store.database.Delete(&serviceRecord{}, "id = ?", repositoryRouteTestServiceID).Error; err == nil {
		t.Fatal("Service referenced by Routes was deleted")
	}
}

func TestLoadRouteDesiredStateReadsCompleteIndependentValues(t *testing.T) {
	store := openRouteTestStore(t)
	seedRouteTestService(t, store)
	seedAdditionalRouteTestService(t, store, repositoryRouteSecondTestServiceID)
	if err := store.database.Create(&httpRouteRecord{
		ID: "http-main", ServiceID: repositoryRouteTestServiceID, Hostname: "example.test",
		PathPrefix: "/api", PreserveHost: true, Enabled: true, CreatedAt: 1, UpdatedAt: 2,
	}).Error; err != nil {
		t.Fatalf("seed HTTP Route error = %v", err)
	}
	if err := store.database.Create(&tcpRouteRecord{
		ID: "tcp-main", ServiceID: repositoryRouteSecondTestServiceID, PublicPort: 8443,
		Enabled: true, CreatedAt: 1, UpdatedAt: 2,
	}).Error; err != nil {
		t.Fatalf("seed TCP Route error = %v", err)
	}
	if err := store.database.Model(&routeConfigStateRecord{}).
		Where("id = ?", singletonRouteConfigStateID).
		Update("generation", 3).Error; err != nil {
		t.Fatalf("advance Route Generation error = %v", err)
	}

	state, err := store.LoadRouteDesiredState(context.Background())
	if err != nil {
		t.Fatalf("LoadRouteDesiredState() error = %v", err)
	}
	if state.Generation != 3 || len(state.Tunnels) != 1 || len(state.Services) != 2 ||
		len(state.HTTPRoutes) != 1 || len(state.TCPRoutes) != 1 {
		t.Fatalf("LoadRouteDesiredState() = %#v", state)
	}
	if state.Tunnels[0].ID != repositoryTestTunnelID || state.Services[0].ID != repositoryRouteTestServiceID {
		t.Fatalf("loaded relation = tunnel:%q service:%q", state.Tunnels[0].ID, state.Services[0].ID)
	}
	if state.HTTPRoutes[0].Hostname != "example.test" || state.HTTPRoutes[0].PathPrefix != "/api" ||
		!state.HTTPRoutes[0].PreserveHost || state.TCPRoutes[0].PublicPort != 8443 {
		t.Fatalf("loaded routes = HTTP:%#v TCP:%#v", state.HTTPRoutes[0], state.TCPRoutes[0])
	}
	if generation, err := store.CurrentRouteGeneration(context.Background()); err != nil || generation != 3 {
		t.Fatalf("CurrentRouteGeneration() = %d, %v, want 3, nil", generation, err)
	}

	// Repository 每次完整读取都重新分配切片，候选构建修改自己的输入不能污染后续读取。
	state.Tunnels[0].Name = "mutated"
	state.Services[0].Name = "mutated"
	state.HTTPRoutes[0].Hostname = "mutated.test"
	state.TCPRoutes[0].PublicPort = 1
	next, err := store.LoadRouteDesiredState(context.Background())
	if err != nil {
		t.Fatalf("second LoadRouteDesiredState() error = %v", err)
	}
	if next.Tunnels[0].Name == "mutated" || next.Services[0].Name == "mutated" ||
		next.HTTPRoutes[0].Hostname == "mutated.test" || next.TCPRoutes[0].PublicPort == 1 {
		t.Fatal("one desired-state read mutated a later read")
	}
}

func TestLoadRouteDesiredStateRejectsInvalidStoredRouteAndCancellation(t *testing.T) {
	store := openRouteTestStore(t)
	seedRouteTestService(t, store)
	if err := store.database.Exec(
		`INSERT INTO http_routes(id, service_id, hostname, path_prefix, created_at, updated_at)
		 VALUES (' ', ?, 'example.test', '/', 1, 1)`,
		repositoryRouteTestServiceID,
	).Error; err != nil {
		t.Fatalf("seed externally unconstrained Route ID error = %v", err)
	}
	if _, err := store.LoadRouteDesiredState(context.Background()); !errors.Is(err, repository.ErrInvalidRoute) {
		t.Fatalf("LoadRouteDesiredState() error = %v, want ErrInvalidRoute", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.LoadRouteDesiredState(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("LoadRouteDesiredState(canceled) error = %v, want context.Canceled", err)
	}
	if _, err := store.CurrentRouteGeneration(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("CurrentRouteGeneration(canceled) error = %v, want context.Canceled", err)
	}
}

func TestRouteRepositoryFailsWhenGenerationRowIsMissing(t *testing.T) {
	store := openRouteTestStore(t)
	if err := store.database.Delete(&routeConfigStateRecord{}, singletonRouteConfigStateID).Error; err != nil {
		t.Fatalf("delete generation row error = %v", err)
	}
	if _, err := store.CurrentRouteGeneration(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "read route config generation") {
		t.Fatalf("CurrentRouteGeneration() error = %v, want missing-authority failure", err)
	}
}

func TestTCPRouteRepositoryCRUDGenerationAndReadOnlyBoundary(t *testing.T) {
	store := openRouteTestStore(t)
	seedRouteTestService(t, store)
	ctx := context.Background()
	route := repository.TCPRoute{
		ID: "tcp-main", ServiceID: repositoryRouteTestServiceID, PublicPort: 8443,
		Enabled: true, CreatedAt: 1, UpdatedAt: 1,
	}
	if err := store.Read(ctx, func(view repository.RepositoryView) error {
		if err := view.Routes().CreateTCP(ctx, route); !errors.Is(err, errRepositoryWriteOutsideTransaction) {
			t.Fatalf("CreateTCP(read view) error = %v", err)
		}
		if err := view.Routes().UpdateTCP(ctx, route); !errors.Is(err, errRepositoryWriteOutsideTransaction) {
			t.Fatalf("UpdateTCP(read view) error = %v", err)
		}
		if err := view.Routes().DeleteTCP(ctx, route.ID); !errors.Is(err, errRepositoryWriteOutsideTransaction) {
			t.Fatalf("DeleteTCP(read view) error = %v", err)
		}
		if _, err := view.Routes().AdvanceGeneration(ctx, 0); !errors.Is(err, errRepositoryWriteOutsideTransaction) {
			t.Fatalf("AdvanceGeneration(read view) error = %v", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	if err := store.WithTx(ctx, func(transaction repository.TxStore) error {
		if err := transaction.Routes().CreateTCP(ctx, route); err != nil {
			return err
		}
		generation, err := transaction.Routes().AdvanceGeneration(ctx, 0)
		if err != nil {
			return err
		}
		if generation != 1 {
			t.Fatalf("created generation = %d, want 1", generation)
		}
		return nil
	}); err != nil {
		t.Fatalf("create Route transaction error = %v", err)
	}

	stale := route
	stale.PublicPort = 9443
	stale.UpdatedAt = 2
	if err := store.WithTx(ctx, func(transaction repository.TxStore) error {
		if err := transaction.Routes().UpdateTCP(ctx, stale); err != nil {
			return err
		}
		_, err := transaction.Routes().AdvanceGeneration(ctx, 0)
		return err
	}); !errors.Is(err, repository.ErrVersionConflict) {
		t.Fatalf("stale generation transaction error = %v, want ErrVersionConflict", err)
	}
	if err := store.Read(ctx, func(view repository.RepositoryView) error {
		got, err := view.Routes().GetTCP(ctx, route.ID)
		if err != nil {
			return err
		}
		if got != route {
			t.Fatalf("rolled-back Route = %+v, want %+v", got, route)
		}
		listed, err := view.Routes().ListTCP(ctx)
		if err != nil {
			return err
		}
		if len(listed) != 1 || listed[0] != route {
			t.Fatalf("ListTCP() = %+v, want one original Route", listed)
		}
		return nil
	}); err != nil {
		t.Fatalf("read rolled-back Route error = %v", err)
	}

	if err := store.WithTx(ctx, func(transaction repository.TxStore) error {
		if err := transaction.Routes().UpdateTCP(ctx, stale); err != nil {
			return err
		}
		_, err := transaction.Routes().AdvanceGeneration(ctx, 1)
		return err
	}); err != nil {
		t.Fatalf("update Route transaction error = %v", err)
	}
	if err := store.WithTx(ctx, func(transaction repository.TxStore) error {
		if err := transaction.Routes().DeleteTCP(ctx, route.ID); err != nil {
			return err
		}
		_, err := transaction.Routes().AdvanceGeneration(ctx, 2)
		return err
	}); err != nil {
		t.Fatalf("delete Route transaction error = %v", err)
	}
	state, err := store.LoadRouteDesiredState(ctx)
	if err != nil {
		t.Fatalf("LoadRouteDesiredState() error = %v", err)
	}
	if state.Generation != 3 || len(state.TCPRoutes) != 0 {
		t.Fatalf("final Route state = %+v, want generation 3 without TCP Routes", state)
	}
}

func TestHTTPRouteRepositoryCRUDExposureUniquenessAndRollback(t *testing.T) {
	store := openRouteTestStore(t)
	seedRouteTestService(t, store)
	ctx := context.Background()
	httpRoute := repository.HTTPRoute{
		ID: "http-main", ServiceID: repositoryRouteTestServiceID,
		Hostname: "example.test", PathPrefix: "/api", PreserveHost: true,
		Enabled: true, CreatedAt: 1, UpdatedAt: 1,
	}
	if err := store.Read(ctx, func(view repository.RepositoryView) error {
		if err := view.Routes().CreateHTTP(ctx, httpRoute); !errors.Is(err, errRepositoryWriteOutsideTransaction) {
			t.Fatalf("CreateHTTP(read view) error = %v", err)
		}
		if err := view.Routes().UpdateHTTP(ctx, httpRoute); !errors.Is(err, errRepositoryWriteOutsideTransaction) {
			t.Fatalf("UpdateHTTP(read view) error = %v", err)
		}
		if err := view.Routes().DeleteHTTP(ctx, httpRoute.ID); !errors.Is(err, errRepositoryWriteOutsideTransaction) {
			t.Fatalf("DeleteHTTP(read view) error = %v", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	if err := store.WithTx(ctx, func(transaction repository.TxStore) error {
		if err := transaction.Routes().CreateHTTP(ctx, httpRoute); err != nil {
			return err
		}
		_, err := transaction.Routes().AdvanceGeneration(ctx, 0)
		return err
	}); err != nil {
		t.Fatalf("create HTTP Exposure transaction error = %v", err)
	}
	if err := store.Read(ctx, func(view repository.RepositoryView) error {
		got, err := view.Routes().GetHTTP(ctx, httpRoute.ID)
		if err != nil {
			return err
		}
		if got != httpRoute {
			t.Fatalf("GetHTTP() = %+v, want %+v", got, httpRoute)
		}
		exposure, err := view.Routes().GetExposureByService(ctx, repositoryRouteTestServiceID)
		if err != nil {
			return err
		}
		if exposure.HTTP == nil || *exposure.HTTP != httpRoute || exposure.TCP != nil {
			t.Fatalf("GetExposureByService() = %+v, want HTTP only", exposure)
		}
		return nil
	}); err != nil {
		t.Fatalf("read HTTP Exposure error = %v", err)
	}

	if err := store.WithTx(ctx, func(transaction repository.TxStore) error {
		return transaction.Routes().CreateTCP(ctx, repository.TCPRoute{
			ID: "tcp-conflict", ServiceID: repositoryRouteTestServiceID,
			PublicPort: 8443, Enabled: true, CreatedAt: 1, UpdatedAt: 1,
		})
	}); err == nil {
		t.Fatal("cross-type duplicate Exposure error = nil")
	}

	updated := httpRoute
	updated.Hostname = "updated.test"
	updated.UpdatedAt = 2
	if err := store.WithTx(ctx, func(transaction repository.TxStore) error {
		if err := transaction.Routes().UpdateHTTP(ctx, updated); err != nil {
			return err
		}
		_, err := transaction.Routes().AdvanceGeneration(ctx, 0)
		return err
	}); !errors.Is(err, repository.ErrVersionConflict) {
		t.Fatalf("stale HTTP Exposure transaction error = %v, want ErrVersionConflict", err)
	}
	if err := store.Read(ctx, func(view repository.RepositoryView) error {
		got, err := view.Routes().GetHTTP(ctx, httpRoute.ID)
		if err != nil {
			return err
		}
		if got != httpRoute {
			t.Fatalf("rolled-back HTTP Exposure = %+v, want %+v", got, httpRoute)
		}
		return nil
	}); err != nil {
		t.Fatalf("read rolled-back HTTP Exposure error = %v", err)
	}

	tcpRoute := repository.TCPRoute{
		ID: "tcp-main", ServiceID: repositoryRouteTestServiceID,
		PublicPort: 8443, Enabled: true, CreatedAt: 2, UpdatedAt: 2,
	}
	if err := store.WithTx(ctx, func(transaction repository.TxStore) error {
		if err := transaction.Routes().DeleteHTTP(ctx, httpRoute.ID); err != nil {
			return err
		}
		if err := transaction.Routes().CreateTCP(ctx, tcpRoute); err != nil {
			return err
		}
		_, err := transaction.Routes().AdvanceGeneration(ctx, 1)
		return err
	}); err != nil {
		t.Fatalf("switch Exposure type transaction error = %v", err)
	}
	if err := store.Read(ctx, func(view repository.RepositoryView) error {
		exposure, err := view.Routes().GetExposureByService(ctx, repositoryRouteTestServiceID)
		if err != nil {
			return err
		}
		if exposure.TCP == nil || *exposure.TCP != tcpRoute || exposure.HTTP != nil {
			t.Fatalf("switched Exposure = %+v, want TCP only", exposure)
		}
		return nil
	}); err != nil {
		t.Fatalf("read switched Exposure error = %v", err)
	}
	if err := store.WithTx(ctx, func(transaction repository.TxStore) error {
		return transaction.Routes().DeleteTCP(ctx, tcpRoute.ID)
	}); err != nil {
		t.Fatalf("delete switched TCP Exposure error = %v", err)
	}
	if err := store.Read(ctx, func(view repository.RepositoryView) error {
		_, err := view.Routes().GetExposureByService(ctx, repositoryRouteTestServiceID)
		return err
	}); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("GetExposureByService(without Exposure) error = %v, want ErrNotFound", err)
	}
	if err := store.Read(ctx, func(view repository.RepositoryView) error {
		if _, err := view.Routes().GetExposureByService(ctx, "invalid"); !errors.Is(err, repository.ErrInvalidRoute) {
			t.Fatalf("GetExposureByService(invalid) error = %v, want ErrInvalidRoute", err)
		}
		if _, err := view.Routes().GetHTTP(ctx, "missing"); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("GetHTTP(missing) error = %v, want ErrNotFound", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("read missing Exposure error = %v", err)
	}
	if err := store.WithTx(ctx, func(transaction repository.TxStore) error {
		if err := transaction.Routes().UpdateHTTP(ctx, httpRoute); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("UpdateHTTP(missing) error = %v, want ErrNotFound", err)
		}
		if err := transaction.Routes().DeleteHTTP(ctx, httpRoute.ID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("DeleteHTTP(missing) error = %v, want ErrNotFound", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("missing HTTP mutation transaction error = %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.Read(canceled, func(view repository.RepositoryView) error {
		_, err := view.Routes().GetExposureByService(canceled, repositoryRouteTestServiceID)
		return err
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("GetExposureByService(canceled) error = %v, want context.Canceled", err)
	}
}

func openRouteTestStore(t *testing.T) *Store {
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

func seedRouteTestService(t *testing.T, store *Store) {
	t.Helper()
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		if err := transaction.Tunnels().Create(context.Background(), testTunnel()); err != nil {
			return err
		}
		return transaction.Services().Create(
			context.Background(),
			testService(repositoryRouteTestServiceID, repositoryTestTunnelID),
		)
	}); err != nil {
		t.Fatalf("seed Route relation error = %v", err)
	}
}

func seedAdditionalRouteTestService(t *testing.T, store *Store, serviceID string) {
	t.Helper()
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		return transaction.Services().Create(
			context.Background(),
			testService(serviceID, repositoryTestTunnelID),
		)
	}); err != nil {
		t.Fatalf("seed additional Route relation error = %v", err)
	}
}

func seedServiceExposureMigrationRelations(t *testing.T, database *gorm.DB) {
	t.Helper()
	if err := database.Create(tunnelRecordFromDomain(testTunnel())).Error; err != nil {
		t.Fatalf("seed v9 Tunnel error = %v", err)
	}
	for _, serviceID := range []string{repositoryRouteTestServiceID, repositoryRouteSecondTestServiceID} {
		if err := database.Create(serviceRecordFromDomain(testService(serviceID, repositoryTestTunnelID))).Error; err != nil {
			t.Fatalf("seed v9 Service %q error = %v", serviceID, err)
		}
	}
}
