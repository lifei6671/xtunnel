package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lifei6671/xtunnel/internal/repository"
)

const repositoryRouteTestServiceID = "svc_01J00000000000000000000000"

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

func TestRouteMigrationEnforcesDesiredStateConstraints(t *testing.T) {
	store := openRouteTestStore(t)
	seedRouteTestService(t, store)

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
		repositoryRouteTestServiceID,
	).Error; err != nil {
		t.Fatalf("insert valid TCP Route error = %v", err)
	}

	tests := []struct {
		name string
		sql  string
		args []any
	}{
		{
			name: "duplicate HTTP match key",
			sql:  `INSERT INTO http_routes(id, service_id, hostname, path_prefix, created_at, updated_at) VALUES ('http-duplicate', ?, 'example.test', '/', 1, 1)`,
			args: []any{repositoryRouteTestServiceID},
		},
		{
			name: "HTTP route missing Service",
			sql:  `INSERT INTO http_routes(id, service_id, hostname, path_prefix, created_at, updated_at) VALUES ('http-orphan', 'svc_01J00000000000000000000001', 'orphan.test', '/', 1, 1)`,
		},
		{
			name: "HTTP path is not absolute",
			sql:  `INSERT INTO http_routes(id, service_id, hostname, path_prefix, created_at, updated_at) VALUES ('http-path', ?, 'path.test', 'api', 1, 1)`,
			args: []any{repositoryRouteTestServiceID},
		},
		{
			name: "HTTP boolean out of range",
			sql:  `INSERT INTO http_routes(id, service_id, hostname, path_prefix, preserve_host, created_at, updated_at) VALUES ('http-bool', ?, 'bool.test', '/', 2, 1, 1)`,
			args: []any{repositoryRouteTestServiceID},
		},
		{
			name: "duplicate TCP public port",
			sql:  `INSERT INTO tcp_routes(id, service_id, public_port, created_at, updated_at) VALUES ('tcp-duplicate', ?, 8443, 1, 1)`,
			args: []any{repositoryRouteTestServiceID},
		},
		{
			name: "TCP port out of range",
			sql:  `INSERT INTO tcp_routes(id, service_id, public_port, created_at, updated_at) VALUES ('tcp-port', ?, 65536, 1, 1)`,
			args: []any{repositoryRouteTestServiceID},
		},
		{
			name: "TCP route missing Service",
			sql:  `INSERT INTO tcp_routes(id, service_id, public_port, created_at, updated_at) VALUES ('tcp-orphan', 'svc_01J00000000000000000000001', 9443, 1, 1)`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := store.database.Exec(test.sql, test.args...).Error; err == nil {
				t.Fatal("invalid Route row was accepted")
			}
		})
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
	if err := store.database.Create(&httpRouteRecord{
		ID: "http-main", ServiceID: repositoryRouteTestServiceID, Hostname: "example.test",
		PathPrefix: "/api", PreserveHost: true, Enabled: true, CreatedAt: 1, UpdatedAt: 2,
	}).Error; err != nil {
		t.Fatalf("seed HTTP Route error = %v", err)
	}
	if err := store.database.Create(&tcpRouteRecord{
		ID: "tcp-main", ServiceID: repositoryRouteTestServiceID, PublicPort: 8443,
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
	if state.Generation != 3 || len(state.Tunnels) != 1 || len(state.Services) != 1 ||
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
