//go:build linux

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	libsqlite "github.com/libtnb/sqlite"

	"github.com/lifei6671/xtunnel/internal/repository"
)

func TestOpenBackupSourceRejectsSymlink(t *testing.T) {
	dataDir := t.TempDir()
	realDatabase := filepath.Join(dataDir, "real.db")
	if err := os.WriteFile(realDatabase, []byte("not-used"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(real database) error = %v", err)
	}
	link := filepath.Join(dataDir, databaseFilename)
	if err := os.Symlink(realDatabase, link); err != nil {
		t.Fatalf("os.Symlink(database) error = %v", err)
	}
	if source, err := OpenBackupSource(link); err == nil {
		_ = source.Close()
		t.Fatal("OpenBackupSource() followed a symbolic link")
	}
}

func TestBackupSourceRejectsPathReplacement(t *testing.T) {
	dataDir := t.TempDir()
	store, err := Open(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("Open(original) error = %v", err)
	}
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		return transaction.Tunnels().Create(context.Background(), testTunnel())
	}); err != nil {
		_ = store.Close()
		t.Fatalf("seed original Tunnel error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Store.Close(original) error = %v", err)
	}
	normalizeBackupSourceTestDatabase(t, dataDir)

	databasePath := filepath.Join(dataDir, databaseFilename)
	source, err := OpenBackupSource(databasePath)
	if err != nil {
		t.Fatalf("OpenBackupSource() error = %v", err)
	}
	defer func() { _ = source.Close() }()
	movedPath := filepath.Join(dataDir, "original.db")
	if err := os.Rename(databasePath, movedPath); err != nil {
		t.Fatalf("os.Rename(original database) error = %v", err)
	}
	if err := os.Symlink(movedPath, databasePath); err != nil {
		t.Fatalf("os.Symlink(replacement database) error = %v", err)
	}
	if _, err := source.InspectSchemaVersion(context.Background()); err == nil {
		t.Fatal("InspectSchemaVersion(replaced path) error = nil")
	}
	destination := filepath.Join(t.TempDir(), "backup.db")
	if err := source.BackupSQLite(context.Background(), destination); err == nil {
		t.Fatal("BackupSQLite(replaced path) error = nil")
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected backup destination exists or stat failed: %v", err)
	}
}

func TestBackupSourceIncludesHeldWAL(t *testing.T) {
	dataDir := t.TempDir()
	store, err := Open(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = store.Close() }()
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		return transaction.Tunnels().Create(context.Background(), testTunnel())
	}); err != nil {
		t.Fatalf("seed WAL Tunnel error = %v", err)
	}
	barrier, err := store.AcquireBackupBarrier(context.Background())
	if err != nil {
		t.Fatalf("AcquireBackupBarrier() error = %v", err)
	}
	defer barrier.Release()
	source, err := OpenBackupSource(filepath.Join(dataDir, databaseFilename))
	if err != nil {
		t.Fatalf("OpenBackupSource(WAL) error = %v", err)
	}
	defer func() { _ = source.Close() }()
	destination := filepath.Join(t.TempDir(), "backup.db")
	if err := source.BackupSQLite(context.Background(), destination); err != nil {
		t.Fatalf("BackupSQLite(WAL source) error = %v", err)
	}
	if got := backupSourceTestTunnelCount(t, destination); got != 1 {
		t.Fatalf("WAL backup Tunnel count = %d, want 1", got)
	}
}

func TestBackupSourceRejectsRenameWhileWALIsHeld(t *testing.T) {
	dataDir := t.TempDir()
	store, err := Open(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = store.Close() }()
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		return transaction.Tunnels().Create(context.Background(), testTunnel())
	}); err != nil {
		t.Fatalf("seed WAL Tunnel error = %v", err)
	}
	barrier, err := store.AcquireBackupBarrier(context.Background())
	if err != nil {
		t.Fatalf("AcquireBackupBarrier() error = %v", err)
	}
	defer barrier.Release()
	databasePath := filepath.Join(dataDir, databaseFilename)
	source, err := OpenBackupSource(databasePath)
	if err != nil {
		t.Fatalf("OpenBackupSource(WAL rename) error = %v", err)
	}
	defer func() { _ = source.Close() }()
	movedPath := filepath.Join(dataDir, "renamed.db")
	if err := os.Rename(databasePath, movedPath); err != nil {
		t.Fatalf("os.Rename(WAL database) error = %v", err)
	}
	destination := filepath.Join(t.TempDir(), "backup.db")
	if err := source.BackupSQLite(context.Background(), destination); err == nil {
		t.Fatal("BackupSQLite(renamed WAL source) error = nil")
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("renamed WAL backup destination exists or stat failed: %v", err)
	}
	if err := os.Rename(movedPath, databasePath); err != nil {
		t.Fatalf("restore WAL database path error = %v", err)
	}
}

func normalizeBackupSourceTestDatabase(t *testing.T, dataDir string) {
	t.Helper()
	databasePath := filepath.Join(dataDir, databaseFilename)
	normalizedPath := filepath.Join(dataDir, "normalized.db")
	if err := BackupSQLite(context.Background(), databasePath, normalizedPath); err != nil {
		t.Fatalf("BackupSQLite(normalize) error = %v", err)
	}
	for _, path := range []string{databasePath, databasePath + "-wal", databasePath + "-shm"} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("os.Remove(normalize source) error = %v", err)
		}
	}
	if err := os.Rename(normalizedPath, databasePath); err != nil {
		t.Fatalf("os.Rename(normalized database) error = %v", err)
	}
}

func backupSourceTestTunnelCount(t *testing.T, databasePath string) int {
	t.Helper()
	pool, err := sql.Open(libsqlite.DriverName, immutableDatabaseDSN(databasePath))
	if err != nil {
		t.Fatalf("sql.Open(backup) error = %v", err)
	}
	defer func() { _ = pool.Close() }()
	var count int
	if err := pool.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM tunnels").Scan(&count); err != nil {
		t.Fatalf("read backup Tunnel count error = %v", err)
	}
	return count
}
