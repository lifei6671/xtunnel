package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	libsqlite "github.com/libtnb/sqlite"

	"github.com/lifei6671/xtunnel/internal/repository"
)

func TestBackupBarrierQueuesAheadOfLaterWriter(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- store.WithTx(context.Background(), func(repository.TxStore) error {
			close(firstStarted)
			<-releaseFirst
			return nil
		})
	}()
	waitForSignal(t, firstStarted, "first writer start")

	barrierReady := make(chan *BackupBarrier, 1)
	barrierErrors := make(chan error, 1)
	go func() {
		barrier, err := store.AcquireBackupBarrier(context.Background())
		if err != nil {
			barrierErrors <- err
			return
		}
		barrierReady <- barrier
	}()
	waitForGateWaiters(t, store.writeGate, 1)

	laterStarted := make(chan struct{})
	laterDone := make(chan error, 1)
	go func() {
		laterDone <- store.WithTx(context.Background(), func(repository.TxStore) error {
			close(laterStarted)
			return nil
		})
	}()
	waitForGateWaiters(t, store.writeGate, 2)
	close(releaseFirst)
	if err := waitForResult(t, firstDone, "first writer completion"); err != nil {
		t.Fatalf("first WithTx() error = %v", err)
	}

	var barrier *BackupBarrier
	select {
	case barrier = <-barrierReady:
	case err := <-barrierErrors:
		t.Fatalf("AcquireBackupBarrier() error = %v", err)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for backup barrier")
	}
	select {
	case <-laterStarted:
		t.Fatal("later writer overtook held backup barrier")
	case <-time.After(50 * time.Millisecond):
	}

	barrier.Release()
	waitForSignal(t, laterStarted, "later writer start")
	if err := waitForResult(t, laterDone, "later writer completion"); err != nil {
		t.Fatalf("later WithTx() error = %v", err)
	}
	barrier.Release()
}

func TestBackupBarrierCancellationDoesNotBlockWrites(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	held, err := store.AcquireBackupBarrier(context.Background())
	if err != nil {
		t.Fatalf("AcquireBackupBarrier() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := store.AcquireBackupBarrier(ctx)
		result <- err
	}()
	waitForGateWaiters(t, store.writeGate, 1)
	cancel()
	if err := waitForResult(t, result, "canceled barrier completion"); !errors.Is(err, context.Canceled) {
		t.Fatalf("AcquireBackupBarrier(canceled) error = %v, want context.Canceled", err)
	}
	waitForGateWaiters(t, store.writeGate, 0)
	held.Release()

	writeContext, cancelWrite := context.WithTimeout(context.Background(), time.Second)
	defer cancelWrite()
	if err := store.WithTx(writeContext, func(repository.TxStore) error { return nil }); err != nil {
		t.Fatalf("WithTx() after canceled barrier error = %v", err)
	}
}

func TestBackupBarrierBlocksCreateFirstAdmin(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	barrier, err := store.AcquireBackupBarrier(context.Background())
	if err != nil {
		t.Fatalf("AcquireBackupBarrier() error = %v", err)
	}

	createContext, cancelCreate := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelCreate()
	result := make(chan error, 1)
	go func() {
		result <- store.CreateFirstAdmin(createContext, "admin", "test-password")
	}()
	waitForGateWaiters(t, store.writeGate, 1)
	select {
	case err := <-result:
		barrier.Release()
		t.Fatalf("CreateFirstAdmin() completed while barrier held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	barrier.Release()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("CreateFirstAdmin() error = %v", err)
		}
	case <-createContext.Done():
		t.Fatalf("timed out waiting for first-admin creation: %v", createContext.Err())
	}
}

func TestBackupBarrierDoesNotBlockReads(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	barrier, err := store.AcquireBackupBarrier(context.Background())
	if err != nil {
		t.Fatalf("AcquireBackupBarrier() error = %v", err)
	}
	defer barrier.Release()
	readContext, cancelRead := context.WithTimeout(context.Background(), time.Second)
	defer cancelRead()
	if _, err := store.HasAdmin(readContext); err != nil {
		t.Fatalf("HasAdmin() while barrier held error = %v", err)
	}
}

func TestReadViewsRejectWritesOutsideWriteGate(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	tests := []struct {
		name  string
		write func(repository.RepositoryView) error
	}{
		{name: "Tunnel", write: func(view repository.RepositoryView) error {
			return view.Tunnels().Create(context.Background(), repository.Tunnel{})
		}},
		{name: "Tunnel Token", write: func(view repository.RepositoryView) error {
			return view.TunnelTokens().Create(context.Background(), repository.TunnelToken{})
		}},
		{name: "Service", write: func(view repository.RepositoryView) error {
			return view.Services().Create(context.Background(), repository.Service{})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := store.Read(context.Background(), test.write)
			if !errors.Is(err, errRepositoryWriteOutsideTransaction) {
				t.Fatalf("Read(write) error = %v, want write-transaction rejection", err)
			}
		})
	}
	if err := store.ReadConsistent(context.Background(), func(view repository.RepositoryView) error {
		return view.Tunnels().Create(context.Background(), repository.Tunnel{})
	}); !errors.Is(err, errRepositoryWriteOutsideTransaction) {
		t.Fatalf("ReadConsistent(write) error = %v, want write-transaction rejection", err)
	}
}

func TestBackupSQLiteIncludesUncheckpointedWALAndCreatesPrivateFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows SQLite backup remains unavailable pending M8-04")
	}
	dataDir := t.TempDir()
	store, err := Open(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	reader, err := store.pool.Conn(context.Background())
	if err != nil {
		t.Fatalf("acquire snapshot reader error = %v", err)
	}
	defer func() { _ = reader.Close() }()
	if _, err := reader.ExecContext(context.Background(), "BEGIN"); err != nil {
		t.Fatalf("begin snapshot reader error = %v", err)
	}
	defer func() { _, _ = reader.ExecContext(context.Background(), "ROLLBACK") }()
	var before int
	if err := reader.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM tunnels").Scan(&before); err != nil {
		t.Fatalf("establish snapshot reader error = %v", err)
	}
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		return transaction.Tunnels().Create(context.Background(), testTunnel())
	}); err != nil {
		t.Fatalf("seed Tunnel in WAL error = %v", err)
	}
	waldInfo, err := os.Stat(filepath.Join(dataDir, databaseFilename+"-wal"))
	if err != nil || waldInfo.Size() == 0 {
		t.Fatalf("source WAL state = info:%v error:%v, want non-empty WAL", waldInfo, err)
	}

	destinationPath := filepath.Join(t.TempDir(), "backup.db")
	barrier, err := store.AcquireBackupBarrier(context.Background())
	if err != nil {
		t.Fatalf("AcquireBackupBarrier() error = %v", err)
	}
	if err := barrier.BackupSQLite(context.Background(), destinationPath); err != nil {
		barrier.Release()
		t.Fatalf("BackupSQLite() error = %v", err)
	}
	barrier.Release()

	if runtime.GOOS != "windows" {
		info, err := os.Stat(destinationPath)
		if err != nil {
			t.Fatalf("stat backup error = %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("backup mode = %o, want 600", got)
		}
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(destinationPath + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("backup companion %q exists or stat failed: %v", suffix, err)
		}
	}

	backupPool, err := sql.Open(libsqlite.DriverName, readOnlyDatabaseDSN(destinationPath))
	if err != nil {
		t.Fatalf("open backup error = %v", err)
	}
	defer func() { _ = backupPool.Close() }()
	var tunnelCount int
	if err := backupPool.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM tunnels").Scan(&tunnelCount); err != nil {
		t.Fatalf("read backup Tunnel count error = %v", err)
	}
	if tunnelCount != 1 {
		t.Fatalf("backup Tunnel count = %d, want 1", tunnelCount)
	}
}

func TestBackupSQLiteCancellationCleansDestination(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows SQLite backup remains unavailable pending M8-04")
	}
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	barrier, err := store.AcquireBackupBarrier(context.Background())
	if err != nil {
		t.Fatalf("AcquireBackupBarrier() error = %v", err)
	}
	defer barrier.Release()

	destinationPath := filepath.Join(t.TempDir(), "canceled.db")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := barrier.BackupSQLite(ctx, destinationPath); !errors.Is(err, context.Canceled) {
		t.Fatalf("BackupSQLite(canceled) error = %v, want context.Canceled", err)
	}
	if _, err := os.Lstat(destinationPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled backup destination still exists or stat failed: %v", err)
	}
}

func TestBackupSQLiteRefusesExistingDestination(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	barrier, err := store.AcquireBackupBarrier(context.Background())
	if err != nil {
		t.Fatalf("AcquireBackupBarrier() error = %v", err)
	}
	defer barrier.Release()

	destinationPath := filepath.Join(t.TempDir(), "existing.db")
	const sentinel = "do-not-overwrite"
	if err := os.WriteFile(destinationPath, []byte(sentinel), 0o600); err != nil {
		t.Fatalf("seed existing destination error = %v", err)
	}
	if err := barrier.BackupSQLite(context.Background(), destinationPath); err == nil {
		t.Fatal("BackupSQLite(existing destination) error = nil")
	}
	content, err := os.ReadFile(destinationPath)
	if err != nil {
		t.Fatalf("read existing destination error = %v", err)
	}
	if string(content) != sentinel {
		t.Fatalf("existing destination content = %q, want unchanged sentinel", content)
	}
}

func TestOfflineBackupSQLiteUsesReadOnlySource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows SQLite backup remains unavailable pending M8-04")
	}
	dataDir := t.TempDir()
	store, err := Open(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		return transaction.Tunnels().Create(context.Background(), testTunnel())
	}); err != nil {
		_ = store.Close()
		t.Fatalf("seed Tunnel error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Store.Close() error = %v", err)
	}

	destinationPath := filepath.Join(t.TempDir(), "offline.db")
	if err := BackupSQLite(
		context.Background(), filepath.Join(dataDir, databaseFilename), destinationPath,
	); err != nil {
		t.Fatalf("BackupSQLite() error = %v", err)
	}
	if err := ValidateBackupDatabase(context.Background(), destinationPath, CurrentSchemaVersion()); err != nil {
		t.Fatalf("ValidateBackupDatabase() error = %v", err)
	}
	for _, sidecar := range []string{destinationPath + "-wal", destinationPath + "-shm"} {
		if _, err := os.Lstat(sidecar); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("immutable backup validation created sidecar %q: %v", sidecar, err)
		}
	}
	version, err := InspectSchemaVersion(context.Background(), destinationPath)
	if err != nil {
		t.Fatalf("InspectSchemaVersion(offline backup) error = %v", err)
	}
	if version != CurrentSchemaVersion() {
		t.Fatalf("offline backup schema version = %d, want %d", version, CurrentSchemaVersion())
	}
}

func TestInspectSchemaVersionRejectsNewerDatabaseWithoutMigration(t *testing.T) {
	dataDir := t.TempDir()
	store, err := Open(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Store.Close() error = %v", err)
	}
	databasePath := filepath.Join(dataDir, databaseFilename)

	version, err := InspectSchemaVersion(context.Background(), databasePath)
	if err != nil {
		t.Fatalf("InspectSchemaVersion() error = %v", err)
	}
	if version != CurrentSchemaVersion() {
		t.Fatalf("InspectSchemaVersion() = %d, want %d", version, CurrentSchemaVersion())
	}

	pool, err := sql.Open(libsqlite.DriverName, databaseDSN(databasePath))
	if err != nil {
		t.Fatalf("open database for newer version seed error = %v", err)
	}
	if _, err := pool.ExecContext(context.Background(),
		"INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)",
		CurrentSchemaVersion()+1, time.Now().UTC().Unix(),
	); err != nil {
		_ = pool.Close()
		t.Fatalf("seed newer schema version error = %v", err)
	}
	if err := pool.Close(); err != nil {
		t.Fatalf("close newer-version seed pool error = %v", err)
	}

	if _, err := InspectSchemaVersion(context.Background(), databasePath); err == nil {
		t.Fatal("InspectSchemaVersion(newer database) error = nil")
	}
}

func TestInspectSchemaVersionDoesNotMigrateEmptyDatabase(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "empty.db")
	pool, err := sql.Open(libsqlite.DriverName, databaseDSN(databasePath))
	if err != nil {
		t.Fatalf("open empty database error = %v", err)
	}
	if err := pool.PingContext(context.Background()); err != nil {
		_ = pool.Close()
		t.Fatalf("create empty database error = %v", err)
	}
	if err := pool.Close(); err != nil {
		t.Fatalf("close empty database error = %v", err)
	}

	version, err := InspectSchemaVersion(context.Background(), databasePath)
	if err != nil {
		t.Fatalf("InspectSchemaVersion(empty database) error = %v", err)
	}
	if version != 0 {
		t.Fatalf("InspectSchemaVersion(empty database) = %d, want 0", version)
	}
	readPool, err := sql.Open(libsqlite.DriverName, readOnlyDatabaseDSN(databasePath))
	if err != nil {
		t.Fatalf("reopen empty database error = %v", err)
	}
	defer func() { _ = readPool.Close() }()
	var tableCount int
	if err := readPool.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'",
	).Scan(&tableCount); err != nil {
		t.Fatalf("inspect empty database after schema inspection error = %v", err)
	}
	if tableCount != 0 {
		t.Fatalf("schema inspection created schema_migrations: count = %d", tableCount)
	}
}

func waitForGateWaiters(t *testing.T, gate *writeGate, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		gate.mu.Lock()
		got := len(gate.waiters)
		gate.mu.Unlock()
		if got == want {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("write gate waiter count did not reach %d", want)
}

func waitForSignal(t *testing.T, signal <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func waitForResult(t *testing.T, result <-chan error, label string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", label)
		return nil
	}
}
