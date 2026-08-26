package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	libsqlite "github.com/libtnb/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestOpenCreatesAndReusesMigratedDatabase(t *testing.T) {
	dataDir := t.TempDir()
	store, err := Open(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	var firstAppliedAt int64
	if err := store.database.Table("schema_migrations").Select("applied_at").Where("version = ?", 1).Scan(&firstAppliedAt).Error; err != nil {
		t.Fatalf("read first applied_at error = %v", err)
	}
	if firstAppliedAt == 0 {
		t.Fatal("first applied_at = 0")
	}
	if err := store.database.Exec("INSERT INTO schema_migrations(version, applied_at) VALUES (1, 1)").Error; err == nil {
		t.Fatal("schema_migrations accepted a duplicate primary key")
	}
	if err := store.database.Exec("INSERT INTO schema_migrations(version, applied_at) VALUES (6, NULL)").Error; err == nil {
		t.Fatal("schema_migrations accepted a NULL applied_at")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Store.Close() error = %v", err)
	}

	store, err = Open(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	var versions []int
	if err := store.database.Table("schema_migrations").Order("version").Pluck("version", &versions).Error; err != nil {
		t.Fatalf("read versions error = %v", err)
	}
	if len(versions) != 5 || versions[0] != 1 || versions[1] != 2 || versions[2] != 3 || versions[3] != 4 || versions[4] != 5 {
		t.Fatalf("versions = %#v, want [1 2 3 4 5]", versions)
	}
	var secondAppliedAt int64
	if err := store.database.Table("schema_migrations").Select("applied_at").Where("version = ?", 1).Scan(&secondAppliedAt).Error; err != nil {
		t.Fatalf("read second applied_at error = %v", err)
	}
	if secondAppliedAt != firstAppliedAt {
		t.Fatalf("second applied_at = %d, want unchanged %d", secondAppliedAt, firstAppliedAt)
	}
}

func TestOpenConfiguresEveryPooledConnection(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	if err := store.database.Exec("CREATE TABLE pragma_parent (id INTEGER PRIMARY KEY)").Error; err != nil {
		t.Fatalf("create pragma_parent error = %v", err)
	}
	if err := store.database.Exec("CREATE TABLE pragma_child (parent_id INTEGER REFERENCES pragma_parent(id))").Error; err != nil {
		t.Fatalf("create pragma_child error = %v", err)
	}

	connections := make([]*sql.Conn, 0, maxOpenConnections)
	defer func() {
		for _, connection := range connections {
			if err := connection.Close(); err != nil {
				t.Errorf("sql.Conn.Close() error = %v", err)
			}
		}
	}()
	for range maxOpenConnections {
		connection, err := store.pool.Conn(context.Background())
		if err != nil {
			t.Fatalf("sql.DB.Conn() error = %v", err)
		}
		connections = append(connections, connection)
	}

	checks := []struct {
		pragma string
		want   string
	}{
		{pragma: "foreign_keys", want: "1"},
		{pragma: "busy_timeout", want: "5000"},
		{pragma: "journal_mode", want: "wal"},
		{pragma: "synchronous", want: "1"},
	}
	for connectionIndex, connection := range connections {
		for _, check := range checks {
			var value string
			if err := connection.QueryRowContext(context.Background(), "PRAGMA "+check.pragma).Scan(&value); err != nil {
				t.Fatalf("connection %d PRAGMA %s error = %v", connectionIndex, check.pragma, err)
			}
			if strings.ToLower(value) != check.want {
				t.Fatalf("connection %d PRAGMA %s = %q, want %q", connectionIndex, check.pragma, value, check.want)
			}
		}
		if _, err := connection.ExecContext(
			context.Background(),
			"INSERT INTO pragma_child(parent_id) VALUES (?)",
			connectionIndex+1,
		); err == nil {
			t.Fatalf("connection %d accepted an invalid foreign key", connectionIndex)
		}
	}
}

func TestMigrationsDoNotPersistConnectorRuntime(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})

	var tables []string
	if err := store.database.Raw(
		"SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name",
	).Scan(&tables).Error; err != nil {
		t.Fatalf("list SQLite tables error = %v", err)
	}
	prohibited := map[string]struct{}{
		"active_work": {}, "agent_tokens": {}, "agents": {}, "connector_sessions": {},
		"connectors": {}, "sessions": {}, "work_connections": {},
	}
	for _, table := range tables {
		if _, exists := prohibited[table]; exists {
			t.Fatalf("runtime-only table %q exists in SQLite schema: %#v", table, tables)
		}
	}
}

func TestServiceMigrationUpgradesV4AndPreservesData(t *testing.T) {
	database := openUnmigratedDatabase(t)
	if err := runMigrations(context.Background(), database, productionMigrations[:4], testNow); err != nil {
		t.Fatalf("run v4 migrations error = %v", err)
	}
	seedServiceMigrationV4Data(t, database)

	if err := runMigrations(context.Background(), database, productionMigrations, testNow); err != nil {
		t.Fatalf("upgrade to v5 error = %v", err)
	}

	for table, want := range map[string]int64{
		"tunnels":               1,
		"tunnel_tokens":         1,
		"security_audit_events": 1,
		"services":              0,
	} {
		var got int64
		if err := database.Table(table).Count(&got).Error; err != nil {
			t.Fatalf("count %s after v5 upgrade error = %v", table, err)
		}
		if got != want {
			t.Fatalf("%s count after v5 upgrade = %d, want %d", table, got, want)
		}
	}

	if err := database.Exec(
		`INSERT INTO services (
			id, tunnel_id, name, required_revision,
			origin_scheme, origin_host, origin_port,
			enabled, version, created_at, updated_at
		) VALUES (?, ?, ?, 1, 'tcp', '127.0.0.1', 8080, 1, 1, 1, 1)`,
		"svc_01J00000000000000000000000", "tun_01J00000000000000000000000", "echo",
	).Error; err != nil {
		t.Fatalf("insert valid disabled-health service error = %v", err)
	}
	if err := database.Exec(
		`INSERT INTO services (
			id, tunnel_id, name,
			origin_scheme, origin_host, origin_port,
			enabled, version, created_at, updated_at
		) VALUES (?, ?, ?, 'tcp', '127.0.0.1', 8081, 1, 1, 1, 1)`,
		"svc_01J00000000000000000000001", "tun_01J00000000000000000000000", "default-revision",
	).Error; err != nil {
		t.Fatalf("insert Service with default revision error = %v", err)
	}
	var defaultRevision int64
	if err := database.Table("services").Select("required_revision").
		Where("id = ?", "svc_01J00000000000000000000001").Scan(&defaultRevision).Error; err != nil {
		t.Fatalf("read default Service revision error = %v", err)
	}
	if defaultRevision != 0 {
		t.Fatalf("default Service revision = %d, want 0", defaultRevision)
	}
	if err := database.Exec("DELETE FROM tunnels WHERE id = ?", "tun_01J00000000000000000000000").Error; err == nil {
		t.Fatal("services foreign key allowed deleting its Tunnel")
	}

	var tunnelCount, serviceCount, bindingTableCount, versionCount int64
	if err := database.Table("tunnels").Count(&tunnelCount).Error; err != nil {
		t.Fatalf("count preserved Tunnel error = %v", err)
	}
	if err := database.Table("services").Count(&serviceCount).Error; err != nil {
		t.Fatalf("count inserted Service error = %v", err)
	}
	if err := database.Raw(
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'tunnel_bindings'",
	).Scan(&bindingTableCount).Error; err != nil {
		t.Fatalf("inspect tunnel_bindings error = %v", err)
	}
	if err := database.Table("schema_migrations").Count(&versionCount).Error; err != nil {
		t.Fatalf("count schema versions after v5 upgrade error = %v", err)
	}
	if tunnelCount != 1 || serviceCount != 2 || bindingTableCount != 0 || versionCount != 5 {
		t.Fatalf(
			"v5 state = tunnels:%d services:%d bindings:%d versions:%d, want 1/2/0/5",
			tunnelCount, serviceCount, bindingTableCount, versionCount,
		)
	}
}

func TestServiceMigrationRollsBackAtomically(t *testing.T) {
	database := openUnmigratedDatabase(t)
	if err := runMigrations(context.Background(), database, productionMigrations[:4], testNow); err != nil {
		t.Fatalf("run v4 migrations error = %v", err)
	}
	seedServiceMigrationV4Data(t, database)

	failedV5 := append([]migration{}, productionMigrations[:4]...)
	failedStatements := append([]string{}, productionMigrations[4].statements...)
	failedStatements = append(failedStatements, "THIS IS NOT VALID SQL")
	failedV5 = append(failedV5, migration{version: 5, statements: failedStatements})
	if err := runMigrations(context.Background(), database, failedV5, testNow); err == nil {
		t.Fatal("failed v5 migration error = nil")
	}

	var serviceTableCount, versionCount, tunnelCount int64
	if err := database.Raw(
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'services'",
	).Scan(&serviceTableCount).Error; err != nil {
		t.Fatalf("inspect rolled-back services table error = %v", err)
	}
	if err := database.Table("schema_migrations").Count(&versionCount).Error; err != nil {
		t.Fatalf("count schema versions after failed v5 error = %v", err)
	}
	if err := database.Table("tunnels").Count(&tunnelCount).Error; err != nil {
		t.Fatalf("count preserved v4 Tunnel error = %v", err)
	}
	if serviceTableCount != 0 || versionCount != 4 || tunnelCount != 1 {
		t.Fatalf(
			"failed v5 state = services:%d versions:%d tunnels:%d, want 0/4/1",
			serviceTableCount, versionCount, tunnelCount,
		)
	}
}

func TestServiceMigrationConstraints(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	if err := store.database.Exec(
		"INSERT INTO tunnels(id, name, version, desired_revision, created_at, updated_at) VALUES (?, 'office', 1, 0, 1, 1)",
		"tun_01J00000000000000000000000",
	).Error; err != nil {
		t.Fatalf("seed Tunnel error = %v", err)
	}

	valid := serviceMigrationRow{
		tunnelID: "tun_01J00000000000000000000000", name: "echo", requiredRevision: 1,
		originScheme: "tcp", originHost: "127.0.0.1", originPort: 8080, tlsVerify: 1,
		connectTimeoutMS: 5000, enabled: 1, version: 1, createdAt: 1, updatedAt: 1,
	}
	if err := store.database.Exec(
		`INSERT INTO services (
			id, tunnel_id, name, required_revision, origin_scheme, origin_host, origin_port,
			tls_verify, connect_timeout_ms, enabled, version, created_at, updated_at
		) VALUES (NULL, ?, 'null-id', 0, 'tcp', '127.0.0.1', 8080, 1, 5000, 1, 1, 1, 1)`,
		valid.tunnelID,
	).Error; err == nil {
		t.Fatal("services accepted a NULL primary key")
	}
	tests := []struct {
		name    string
		mutate  func(*serviceMigrationRow)
		wantErr bool
	}{
		{name: "valid disabled health"},
		{name: "valid TCP health", mutate: func(row *serviceMigrationRow) {
			row.healthType, row.healthIntervalMS, row.healthTimeoutMS = "TCP", int64(1000), int64(100)
			row.healthFailureThreshold, row.healthSuccessThreshold = int64(1), int64(1)
		}},
		{name: "valid HTTP health", mutate: func(row *serviceMigrationRow) {
			row.healthType, row.healthPath = "HTTP", "/health"
			row.healthIntervalMS, row.healthTimeoutMS = int64(1000), int64(100)
			row.healthExpectedStatusMin, row.healthExpectedStatusMax = int64(200), int64(399)
			row.healthFailureThreshold, row.healthSuccessThreshold = int64(1), int64(1)
		}},
		{name: "valid HTTPS optional names", mutate: func(row *serviceMigrationRow) {
			row.originScheme = "https"
			row.tlsServerName, row.originHTTPHost = "origin.example.test", "origin.example.test:8080"
		}},
		{name: "valid HTTP optional host", mutate: func(row *serviceMigrationRow) {
			row.originScheme, row.originHTTPHost = "http", "origin.example.test:8080"
		}},
		{name: "control-only TLS server name", mutate: func(row *serviceMigrationRow) {
			row.originScheme, row.tlsServerName = "https", "\t\r\n"
		}, wantErr: true},
		{name: "control-only HTTP host", mutate: func(row *serviceMigrationRow) {
			row.originScheme, row.originHTTPHost = "http", "\t\r\n"
		}, wantErr: true},
		{name: "invalid Service ID", mutate: func(row *serviceMigrationRow) { row.id = "svc_invalid" }, wantErr: true},
		{name: "tab-only name", mutate: func(row *serviceMigrationRow) { row.name = "\t" }, wantErr: true},
		{name: "control-only origin host", mutate: func(row *serviceMigrationRow) { row.originHost = "\r\n" }, wantErr: true},
		{name: "unknown Tunnel", mutate: func(row *serviceMigrationRow) { row.tunnelID = "tun_01J00000000000000000000001" }, wantErr: true},
		{name: "invalid origin scheme", mutate: func(row *serviceMigrationRow) { row.originScheme = "udp" }, wantErr: true},
		{name: "zero origin port", mutate: func(row *serviceMigrationRow) { row.originPort = 0 }, wantErr: true},
		{name: "oversized origin port", mutate: func(row *serviceMigrationRow) { row.originPort = 65536 }, wantErr: true},
		{name: "invalid TLS verify", mutate: func(row *serviceMigrationRow) { row.tlsVerify = 2 }, wantErr: true},
		{name: "invalid enabled", mutate: func(row *serviceMigrationRow) { row.enabled = 2 }, wantErr: true},
		{name: "negative revision", mutate: func(row *serviceMigrationRow) { row.requiredRevision = -1 }, wantErr: true},
		{name: "zero version", mutate: func(row *serviceMigrationRow) { row.version = 0 }, wantErr: true},
		{name: "zero created time", mutate: func(row *serviceMigrationRow) { row.createdAt = 0 }, wantErr: true},
		{name: "zero updated time", mutate: func(row *serviceMigrationRow) { row.updatedAt = 0 }, wantErr: true},
		{name: "TCP origin retains TLS server name", mutate: func(row *serviceMigrationRow) {
			row.tlsServerName = "origin.example.test"
		}, wantErr: true},
		{name: "TCP origin retains HTTP host", mutate: func(row *serviceMigrationRow) {
			row.originHTTPHost = "origin.example.test:8080"
		}, wantErr: true},
		{name: "HTTP origin retains TLS server name", mutate: func(row *serviceMigrationRow) {
			row.originScheme, row.tlsServerName = "http", "origin.example.test"
		}, wantErr: true},
		{name: "disabled health retains interval", mutate: func(row *serviceMigrationRow) { row.healthIntervalMS = int64(1000) }, wantErr: true},
		{name: "unknown health type", mutate: func(row *serviceMigrationRow) { row.healthType = "UDP" }, wantErr: true},
		{name: "TCP health missing interval", mutate: func(row *serviceMigrationRow) {
			row.healthType, row.healthTimeoutMS = "TCP", int64(100)
			row.healthFailureThreshold, row.healthSuccessThreshold = int64(1), int64(1)
		}, wantErr: true},
		{name: "TCP health missing timeout", mutate: func(row *serviceMigrationRow) {
			row.healthType, row.healthIntervalMS = "TCP", int64(1000)
			row.healthFailureThreshold, row.healthSuccessThreshold = int64(1), int64(1)
		}, wantErr: true},
		{name: "TCP health missing failure threshold", mutate: func(row *serviceMigrationRow) {
			row.healthType, row.healthIntervalMS, row.healthTimeoutMS = "TCP", int64(1000), int64(100)
			row.healthSuccessThreshold = int64(1)
		}, wantErr: true},
		{name: "TCP health missing success threshold", mutate: func(row *serviceMigrationRow) {
			row.healthType, row.healthIntervalMS, row.healthTimeoutMS = "TCP", int64(1000), int64(100)
			row.healthFailureThreshold = int64(1)
		}, wantErr: true},
		{name: "TCP health retains path", mutate: func(row *serviceMigrationRow) {
			row.healthType, row.healthPath = "TCP", "/health"
			row.healthIntervalMS, row.healthTimeoutMS = int64(1000), int64(100)
			row.healthFailureThreshold, row.healthSuccessThreshold = int64(1), int64(1)
		}, wantErr: true},
		{name: "TCP health retains HTTP status", mutate: func(row *serviceMigrationRow) {
			row.healthType, row.healthIntervalMS, row.healthTimeoutMS = "TCP", int64(1000), int64(100)
			row.healthExpectedStatusMin = int64(200)
			row.healthFailureThreshold, row.healthSuccessThreshold = int64(1), int64(1)
		}, wantErr: true},
		{name: "health interval below minimum", mutate: func(row *serviceMigrationRow) {
			row.healthType, row.healthIntervalMS, row.healthTimeoutMS = "TCP", int64(999), int64(100)
			row.healthFailureThreshold, row.healthSuccessThreshold = int64(1), int64(1)
		}, wantErr: true},
		{name: "health timeout equals interval", mutate: func(row *serviceMigrationRow) {
			row.healthType, row.healthIntervalMS, row.healthTimeoutMS = "TCP", int64(1000), int64(1000)
			row.healthFailureThreshold, row.healthSuccessThreshold = int64(1), int64(1)
		}, wantErr: true},
		{name: "health threshold below minimum", mutate: func(row *serviceMigrationRow) {
			row.healthType, row.healthIntervalMS, row.healthTimeoutMS = "TCP", int64(1000), int64(100)
			row.healthFailureThreshold, row.healthSuccessThreshold = int64(0), int64(1)
		}, wantErr: true},
		{name: "HTTP health missing path", mutate: func(row *serviceMigrationRow) {
			row.healthType = "HTTP"
			row.healthIntervalMS, row.healthTimeoutMS = int64(1000), int64(100)
			row.healthExpectedStatusMin, row.healthExpectedStatusMax = int64(200), int64(399)
			row.healthFailureThreshold, row.healthSuccessThreshold = int64(1), int64(1)
		}, wantErr: true},
		{name: "HTTP health missing status minimum", mutate: func(row *serviceMigrationRow) {
			row.healthType, row.healthPath = "HTTP", "/health"
			row.healthIntervalMS, row.healthTimeoutMS = int64(1000), int64(100)
			row.healthExpectedStatusMax = int64(399)
			row.healthFailureThreshold, row.healthSuccessThreshold = int64(1), int64(1)
		}, wantErr: true},
		{name: "HTTP health missing status maximum", mutate: func(row *serviceMigrationRow) {
			row.healthType, row.healthPath = "HTTP", "/health"
			row.healthIntervalMS, row.healthTimeoutMS = int64(1000), int64(100)
			row.healthExpectedStatusMin = int64(200)
			row.healthFailureThreshold, row.healthSuccessThreshold = int64(1), int64(1)
		}, wantErr: true},
		{name: "HTTP health path is not absolute", mutate: func(row *serviceMigrationRow) {
			row.healthType, row.healthPath = "HTTP", "health"
			row.healthIntervalMS, row.healthTimeoutMS = int64(1000), int64(100)
			row.healthExpectedStatusMin, row.healthExpectedStatusMax = int64(200), int64(399)
			row.healthFailureThreshold, row.healthSuccessThreshold = int64(1), int64(1)
		}, wantErr: true},
		{name: "HTTP status range is reversed", mutate: func(row *serviceMigrationRow) {
			row.healthType, row.healthPath = "HTTP", "/health"
			row.healthIntervalMS, row.healthTimeoutMS = int64(1000), int64(100)
			row.healthExpectedStatusMin, row.healthExpectedStatusMax = int64(400), int64(399)
			row.healthFailureThreshold, row.healthSuccessThreshold = int64(1), int64(1)
		}, wantErr: true},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			candidate.id = serviceMigrationTestID(index)
			if test.mutate != nil {
				test.mutate(&candidate)
			}
			err := insertServiceMigrationRow(store.database, candidate)
			if test.wantErr && err == nil {
				t.Fatal("invalid Service row was accepted")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("valid Service row error = %v", err)
			}
		})
	}
}

func TestRunMigrationsRollsBackFailedMigration(t *testing.T) {
	database := openUnmigratedDatabase(t)
	if err := runMigrations(context.Background(), database, productionMigrations, testNow); err != nil {
		t.Fatalf("initial runMigrations() error = %v", err)
	}

	available := append([]migration{}, productionMigrations...)
	available = append(available, migration{
		version: 6,
		statements: []string{
			"CREATE TABLE interrupted_migration (id INTEGER PRIMARY KEY)",
			"THIS IS NOT VALID SQL",
		},
	})
	if err := runMigrations(context.Background(), database, available, testNow); err == nil {
		t.Fatal("runMigrations() error = nil")
	}

	var tableCount int64
	if err := database.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'interrupted_migration'").Scan(&tableCount).Error; err != nil {
		t.Fatalf("inspect interrupted table error = %v", err)
	}
	if tableCount != 0 {
		t.Fatalf("interrupted table count = %d, want 0", tableCount)
	}
	var versionCount int64
	if err := database.Table("schema_migrations").Count(&versionCount).Error; err != nil {
		t.Fatalf("count schema versions error = %v", err)
	}
	if versionCount != 5 {
		t.Fatalf("version count = %d, want 5", versionCount)
	}

	available[len(available)-1] = migration{
		version:    6,
		statements: []string{"CREATE TABLE resumed_migration (id INTEGER PRIMARY KEY)"},
	}
	if err := runMigrations(context.Background(), database, available, testNow); err != nil {
		t.Fatalf("runMigrations() after repair error = %v", err)
	}
	var versions []int
	if err := database.Table("schema_migrations").Order("version").Pluck("version", &versions).Error; err != nil {
		t.Fatalf("read repaired versions error = %v", err)
	}
	if len(versions) != 6 || versions[0] != 1 || versions[1] != 2 || versions[2] != 3 || versions[3] != 4 || versions[4] != 5 || versions[5] != 6 {
		t.Fatalf("repaired versions = %#v, want [1 2 3 4 5 6]", versions)
	}
}

func TestRunMigrationsRejectsInvalidRegistry(t *testing.T) {
	tests := []struct {
		name      string
		available []migration
	}{
		{
			name: "gap",
			available: []migration{
				{version: 1, statements: []string{"CREATE TABLE first_migration (id INTEGER PRIMARY KEY)"}},
				{version: 3, statements: []string{"CREATE TABLE invalid_migration (id INTEGER PRIMARY KEY)"}},
			},
		},
		{
			name: "duplicate",
			available: []migration{
				{version: 1, statements: []string{"CREATE TABLE first_migration (id INTEGER PRIMARY KEY)"}},
				{version: 1, statements: []string{"CREATE TABLE invalid_migration (id INTEGER PRIMARY KEY)"}},
			},
		},
		{
			name: "out of order",
			available: []migration{
				{version: 2, statements: []string{"CREATE TABLE invalid_migration (id INTEGER PRIMARY KEY)"}},
				{version: 1, statements: []string{"CREATE TABLE first_migration (id INTEGER PRIMARY KEY)"}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := openUnmigratedDatabase(t)
			if err := runMigrations(context.Background(), database, test.available, testNow); err == nil {
				t.Fatal("runMigrations() error = nil")
			}
			var tableCount int64
			if err := database.Raw("SELECT COUNT(*) FROM sqlite_master WHERE name IN ('first_migration', 'invalid_migration', 'schema_migrations')").Scan(&tableCount).Error; err != nil {
				t.Fatalf("inspect tables error = %v", err)
			}
			if tableCount != 0 {
				t.Fatalf("created table count = %d, want 0", tableCount)
			}
		})
	}
}

func TestRunMigrationsRollsBackCanceledTransaction(t *testing.T) {
	database := openUnmigratedDatabase(t)
	ctx, cancel := context.WithCancel(context.Background())
	err := runMigrations(ctx, database, productionMigrations, func() time.Time {
		cancel()
		return testNow()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runMigrations() error = %v, want context.Canceled", err)
	}

	var tableCount int64
	if err := database.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'").Scan(&tableCount).Error; err != nil {
		t.Fatalf("inspect schema_migrations error = %v", err)
	}
	if tableCount != 0 {
		t.Fatalf("schema_migrations table count = %d, want 0", tableCount)
	}
}

func TestOpenRejectsNewerDatabaseVersion(t *testing.T) {
	dataDir := t.TempDir()
	store, err := Open(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := store.database.Exec("INSERT INTO schema_migrations(version, applied_at) VALUES (6, 1)").Error; err != nil {
		t.Fatalf("insert newer version error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Store.Close() error = %v", err)
	}

	if _, err := Open(context.Background(), dataDir); err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("Open() error = %v, want newer-version rejection", err)
	}
}

func TestOpenHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Open(ctx, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Open() error = %v, want context.Canceled", err)
	}
}

func TestDatabaseDSNEncodesSpecialPath(t *testing.T) {
	filename := "空 格#question?.db"
	if runtime.GOOS == "windows" {
		// Windows 文件名不能包含问号；空格、中文和井号仍能覆盖 URI 路径转义。
		filename = "空 格#question.db"
	}
	path := filepath.Join(t.TempDir(), filename)
	dsn := databaseDSN(path)
	if strings.Contains(dsn, "空 格") || strings.Contains(dsn, "#question") {
		t.Fatalf("databaseDSN() did not encode path: %q", dsn)
	}

	database, err := gorm.Open(libsqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("gorm.Open(%q) error = %v", dsn, err)
	}
	pool, err := database.DB()
	if err != nil {
		t.Fatalf("database.DB() error = %v", err)
	}
	if err := pool.Close(); err != nil {
		t.Fatalf("sql.DB.Close() error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("os.Stat(%q) error = %v", path, err)
	}
}

func openUnmigratedDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	database, err := gorm.Open(
		libsqlite.Open(databaseDSN(filepath.Join(t.TempDir(), databaseFilename))),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)},
	)
	if err != nil {
		t.Fatalf("gorm.Open() error = %v", err)
	}
	pool, err := database.DB()
	if err != nil {
		t.Fatalf("database.DB() error = %v", err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Errorf("sql.DB.Close() error = %v", err)
		}
	})
	return database
}

func testNow() time.Time {
	return time.Unix(1_700_000_000, 0)
}

type serviceMigrationRow struct {
	id, tunnelID, name, originScheme, originHost     string
	requiredRevision, originPort, tlsVerify          int64
	tlsServerName, originHTTPHost                    any
	connectTimeoutMS                                 int64
	healthType, healthPath                           any
	healthIntervalMS, healthTimeoutMS                any
	healthExpectedStatusMin, healthExpectedStatusMax any
	healthFailureThreshold, healthSuccessThreshold   any
	enabled, version, createdAt, updatedAt           int64
}

func insertServiceMigrationRow(database *gorm.DB, row serviceMigrationRow) error {
	return database.Exec(
		`INSERT INTO services (
			id, tunnel_id, name, required_revision,
			origin_scheme, origin_host, origin_port, tls_verify,
			tls_server_name, origin_http_host, connect_timeout_ms,
			health_type, health_path, health_interval_ms, health_timeout_ms,
			health_expected_status_min, health_expected_status_max,
			health_failure_threshold, health_success_threshold,
			enabled, version, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.id, row.tunnelID, row.name, row.requiredRevision,
		row.originScheme, row.originHost, row.originPort, row.tlsVerify,
		row.tlsServerName, row.originHTTPHost, row.connectTimeoutMS,
		row.healthType, row.healthPath, row.healthIntervalMS, row.healthTimeoutMS,
		row.healthExpectedStatusMin, row.healthExpectedStatusMax,
		row.healthFailureThreshold, row.healthSuccessThreshold,
		row.enabled, row.version, row.createdAt, row.updatedAt,
	).Error
}

func serviceMigrationTestID(index int) string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	identifier := []byte("svc_01J00000000000000000000000")
	identifier[len(identifier)-2] = alphabet[index/len(alphabet)]
	identifier[len(identifier)-1] = alphabet[index%len(alphabet)]
	return string(identifier)
}

func seedServiceMigrationV4Data(t *testing.T, database *gorm.DB) {
	t.Helper()
	if err := database.Exec(
		"INSERT INTO tunnels(id, name, version, desired_revision, created_at, updated_at) VALUES (?, 'office', 1, 0, 1, 1)",
		"tun_01J00000000000000000000000",
	).Error; err != nil {
		t.Fatalf("seed v4 Tunnel error = %v", err)
	}
	if err := database.Exec(
		`INSERT INTO tunnel_tokens (
			id, tunnel_id, secret_hash, token_ciphertext, version, status, created_at
		) VALUES (?, ?, zeroblob(32), zeroblob(29), 1, 'ACTIVE', 1)`,
		"tok_01J00000000000000000000000", "tun_01J00000000000000000000000",
	).Error; err != nil {
		t.Fatalf("seed v4 Tunnel Token error = %v", err)
	}
	if err := database.Exec(
		`INSERT INTO security_audit_events (
			event_id, operation_id, event, action, actor_type,
			resource_type, resource_id, result, occurred_at
		) VALUES (?, ?, 'SECURITY_OPERATION_RESULT', 'GATEWAY_KEY_ROTATE', 'LOCAL_OPERATOR',
			'GATEWAY_IDENTITY', 'gateway.example.test', 'SUCCEEDED', 1)`,
		"evt_01J00000000000000000000000", "op_01J00000000000000000000000",
	).Error; err != nil {
		t.Fatalf("seed v4 Security Audit Event error = %v", err)
	}
}
