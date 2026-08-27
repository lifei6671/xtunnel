//go:build linux

package bootstrap

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	"github.com/lifei6671/xtunnel/internal/server/gateway"
)

func TestBackupCommandOfflineCreateAndRestore(t *testing.T) {
	requireRootTest(t)
	ctx := context.Background()
	runtimeDir := newShortRuntimeDirectory(t)
	dataDir := t.TempDir()
	resources := initializeBackupCommandData(t, ctx, dataDir, runtimeDir)
	if err := resources.Close(); err != nil {
		t.Fatalf("serverStorage.Close(initialization) error = %v", err)
	}
	configPath := writeConfig(t, "management:\n  public_url: https://admin.example.com\nagent_gateway:\n  public_hostname: gateway.example.test\n")
	archivePath := filepath.Join(t.TempDir(), "backup.tar")
	var logs bytes.Buffer
	common := []string{"--config", configPath, "--set", "server.data_dir=" + dataDir}
	createArgs := append([]string{"--output", archivePath}, common...)
	if err := runBackupCreate(ctx, "xtunnel-server", createArgs, nil, &logs, runtimeDir); err != nil {
		t.Fatalf("runBackupCreate() error = %v", err)
	}
	if !strings.Contains(logs.String(), `"event":"backup_create_completed"`) ||
		!strings.Contains(logs.String(), `"mode":"offline"`) {
		t.Fatalf("offline backup log = %s", logs.String())
	}
	archiveInfo, err := os.Lstat(archivePath)
	if err != nil || !archiveInfo.Mode().IsRegular() || archiveInfo.Mode().Perm() != 0o600 {
		t.Fatalf("backup archive info = %v error = %v, want regular 0600", archiveInfo, err)
	}

	store, err := sqlite.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("sqlite.Open(after backup) error = %v", err)
	}
	if err := store.CreateFirstAdmin(ctx, "after-backup", "password-after-backup"); err != nil {
		t.Fatalf("CreateFirstAdmin(after backup) error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Store.Close(after mutation) error = %v", err)
	}

	logs.Reset()
	restoreArgs := append([]string{"--input", archivePath}, common...)
	if err := runBackupRestore(ctx, "xtunnel-server", restoreArgs, nil, &logs, runtimeDir); err != nil {
		t.Fatalf("runBackupRestore() error = %v", err)
	}
	if !strings.Contains(logs.String(), `"event":"backup_restore_completed"`) ||
		!strings.Contains(logs.String(), `"mode":"offline"`) {
		t.Fatalf("restore log = %s", logs.String())
	}
	restored, err := sqlite.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("sqlite.Open(restored) error = %v", err)
	}
	t.Cleanup(func() { _ = restored.Close() })
	hasAdmin, err := restored.HasAdmin(ctx)
	if err != nil {
		t.Fatalf("HasAdmin(restored) error = %v", err)
	}
	if hasAdmin {
		t.Fatal("restore retained a database mutation made after backup")
	}
	if _, err := gateway.LoadPinnedIdentity(dataDir); err != nil {
		t.Fatalf("LoadPinnedIdentity(restored) error = %v", err)
	}
}

func TestBackupCommandUsesRunningServerBarrier(t *testing.T) {
	requireRootTest(t)
	ctx := context.Background()
	runtimeDir := newShortRuntimeDirectory(t)
	dataDir := t.TempDir()
	resources := initializeBackupCommandData(t, ctx, dataDir, runtimeDir)
	t.Cleanup(func() { _ = resources.Close() })
	socket, err := openBackupBarrierSocket(ctx, runtimeDir, resources.targetHash, resources.database, nil)
	if err != nil {
		t.Fatalf("openBackupBarrierSocket() error = %v", err)
	}
	t.Cleanup(func() { _ = socket.Close() })

	configPath := writeConfig(t, "management:\n  public_url: https://admin.example.com\nagent_gateway:\n  public_hostname: gateway.example.test\n")
	archivePath := filepath.Join(t.TempDir(), "online-backup.tar")
	args := []string{
		"--output", archivePath,
		"--config", configPath,
		"--set", "server.data_dir=" + dataDir,
	}
	var logs bytes.Buffer
	if err := runBackupCreate(ctx, "xtunnel-server", args, nil, &logs, runtimeDir); err != nil {
		t.Fatalf("runBackupCreate(online) error = %v", err)
	}
	if !strings.Contains(logs.String(), `"mode":"online"`) {
		t.Fatalf("online backup log = %s", logs.String())
	}
}

func initializeBackupCommandData(t *testing.T, ctx context.Context, dataDir, runtimeDir string) *serverStorage {
	t.Helper()
	resources, err := openServerStorage(ctx, dataDir, runtimeDir)
	if err != nil {
		t.Fatalf("openServerStorage() error = %v", err)
	}
	if _, err := gateway.LoadOrCreatePinnedIdentity(
		dataDir,
		"gateway.example.test",
		!resources.databaseExisted,
		time.Date(2026, time.August, 27, 0, 0, 0, 0, time.UTC),
	); err != nil {
		_ = resources.Close()
		t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
	}
	return resources
}

func requireRootTest(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("backup maintenance command requires root")
	}
}
