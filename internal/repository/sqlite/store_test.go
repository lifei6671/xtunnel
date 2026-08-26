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
	if err := store.database.Exec("INSERT INTO schema_migrations(version, applied_at) VALUES (4, NULL)").Error; err == nil {
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
	if len(versions) != 3 || versions[0] != 1 || versions[1] != 2 || versions[2] != 3 {
		t.Fatalf("versions = %#v, want [1 2 3]", versions)
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

func TestRunMigrationsRollsBackFailedMigration(t *testing.T) {
	database := openUnmigratedDatabase(t)
	if err := runMigrations(context.Background(), database, productionMigrations, testNow); err != nil {
		t.Fatalf("initial runMigrations() error = %v", err)
	}

	available := append([]migration{}, productionMigrations...)
	available = append(available, migration{
		version: 4,
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
	if versionCount != 3 {
		t.Fatalf("version count = %d, want 3", versionCount)
	}

	available[len(available)-1] = migration{
		version:    4,
		statements: []string{"CREATE TABLE resumed_migration (id INTEGER PRIMARY KEY)"},
	}
	if err := runMigrations(context.Background(), database, available, testNow); err != nil {
		t.Fatalf("runMigrations() after repair error = %v", err)
	}
	var versions []int
	if err := database.Table("schema_migrations").Order("version").Pluck("version", &versions).Error; err != nil {
		t.Fatalf("read repaired versions error = %v", err)
	}
	if len(versions) != 4 || versions[0] != 1 || versions[1] != 2 || versions[2] != 3 || versions[3] != 4 {
		t.Fatalf("repaired versions = %#v, want [1 2 3 4]", versions)
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
	if err := store.database.Exec("INSERT INTO schema_migrations(version, applied_at) VALUES (4, 1)").Error; err != nil {
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
