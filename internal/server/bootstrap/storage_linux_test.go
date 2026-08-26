//go:build linux

package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/repository"
	"github.com/lifei6671/xtunnel/internal/server/datadir"
	"github.com/lifei6671/xtunnel/internal/server/externallock"
	"github.com/lifei6671/xtunnel/internal/server/tokenkey"
)

func TestOpenServerStorageLocksBeforeOpeningSQLite(t *testing.T) {
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatalf("os.Mkdir(dataDir) error = %v", err)
	}
	runtimeDir := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatalf("os.Mkdir(runtimeDir) error = %v", err)
	}
	target, err := datadir.Resolve(dataDir)
	if err != nil {
		t.Fatalf("datadir.Resolve() error = %v", err)
	}
	heldLock, err := externallock.Acquire(runtimeDir, target.Hash)
	if err != nil {
		t.Fatalf("externallock.Acquire() error = %v", err)
	}
	t.Cleanup(func() {
		if err := heldLock.Close(); err != nil {
			t.Errorf("held Lock.Close() error = %v", err)
		}
	})

	if _, err := openServerStorage(context.Background(), dataDir, runtimeDir); !errors.Is(err, externallock.ErrAlreadyLocked) {
		t.Fatalf("openServerStorage() error = %v, want ErrAlreadyLocked", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "xtunnel.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SQLite was touched before lock rejection: os.Stat() error = %v", err)
	}
}

func TestOpenServerStorageChecksJournalBeforeLeaf(t *testing.T) {
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "data")
	runtimeDir := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatalf("os.Mkdir(runtimeDir) error = %v", err)
	}
	target, err := datadir.Resolve(dataDir)
	if err != nil {
		t.Fatalf("datadir.Resolve() error = %v", err)
	}
	journalPath := filepath.Join(parent, ".xtunnel-restore-"+target.Hash+".journal")
	if err := os.WriteFile(journalPath, []byte("pending"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	if _, err := openServerStorage(context.Background(), dataDir, runtimeDir); !errors.Is(err, datadir.ErrPendingRestore) {
		t.Fatalf("openServerStorage() error = %v, want ErrPendingRestore", err)
	}
}

func TestOpenServerStorageReleasesResources(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatalf("os.Mkdir(dataDir) error = %v", err)
	}
	runtimeDir := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatalf("os.Mkdir(runtimeDir) error = %v", err)
	}

	resources, err := openServerStorage(context.Background(), dataDir, runtimeDir)
	if err != nil {
		t.Fatalf("openServerStorage() error = %v", err)
	}
	if err := resources.Close(); err != nil {
		t.Fatalf("serverStorage.Close() error = %v", err)
	}
	firstKey := resources.tokenMasterKey
	resources, err = openServerStorage(context.Background(), dataDir, runtimeDir)
	if err != nil {
		t.Fatalf("second openServerStorage() error = %v", err)
	}
	if err := resources.Close(); err != nil {
		t.Fatalf("second serverStorage.Close() error = %v", err)
	}
	if resources.tokenMasterKey != firstKey {
		t.Fatal("second openServerStorage() changed Tunnel Token master key")
	}
}

func TestOpenServerStorageAllowsRegenerationOnlyWhileTokenTableIsEmpty(t *testing.T) {
	dataDir, runtimeDir := storageTestDirectories(t)
	resources, err := openServerStorage(context.Background(), dataDir, runtimeDir)
	if err != nil {
		t.Fatalf("openServerStorage(first) error = %v", err)
	}
	firstKey := resources.tokenMasterKey
	if err := resources.Close(); err != nil {
		t.Fatalf("serverStorage.Close(first) error = %v", err)
	}
	if err := os.Remove(tunnelTokenKeyPath(dataDir)); err != nil {
		t.Fatalf("os.Remove(key) error = %v", err)
	}

	resources, err = openServerStorage(context.Background(), dataDir, runtimeDir)
	if err != nil {
		t.Fatalf("openServerStorage(empty table) error = %v", err)
	}
	if resources.tokenMasterKey == firstKey {
		t.Fatal("empty-token-table recovery unexpectedly reused removed key bytes")
	}
	if err := resources.Close(); err != nil {
		t.Fatalf("serverStorage.Close(second) error = %v", err)
	}
}

func TestOpenServerStorageRejectsMissingKeyWithEncryptedTokens(t *testing.T) {
	dataDir, runtimeDir := storageTestDirectories(t)
	resources, err := openServerStorage(context.Background(), dataDir, runtimeDir)
	if err != nil {
		t.Fatalf("openServerStorage(first) error = %v", err)
	}
	now := time.Now().UTC().Unix()
	err = resources.database.WithTx(context.Background(), func(transaction repository.TxStore) error {
		if err := transaction.Tunnels().Create(context.Background(), repository.Tunnel{
			ID: "tun_01J00000000000000000000000", Name: "storage key test", Version: 1,
			DesiredRevision: 0, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return err
		}
		return transaction.TunnelTokens().Create(context.Background(), repository.TunnelToken{
			ID: "tok_01J00000000000000000000000", TunnelID: "tun_01J00000000000000000000000",
			TokenCiphertext: make([]byte, 29), Version: 1,
			Status: repository.TunnelTokenStatusActive, CreatedAt: now,
		})
	})
	if err != nil {
		t.Fatalf("create encrypted token fixture error = %v", err)
	}
	if err := resources.Close(); err != nil {
		t.Fatalf("serverStorage.Close() error = %v", err)
	}
	if err := os.Remove(tunnelTokenKeyPath(dataDir)); err != nil {
		t.Fatalf("os.Remove(key) error = %v", err)
	}

	if _, err := openServerStorage(context.Background(), dataDir, runtimeDir); !errors.Is(err, tokenkey.ErrUnavailable) {
		t.Fatalf("openServerStorage() error = %v, want tokenkey.ErrUnavailable", err)
	}
	// 失败路径必须释放 SQLite 和 External Lock，避免修复密钥后仍需等待旧进程退出。
	if err := os.WriteFile(tunnelTokenKeyPath(dataDir), make([]byte, tokenkey.Size), 0o600); err != nil {
		t.Fatalf("os.WriteFile(repaired key) error = %v", err)
	}
	resources, err = openServerStorage(context.Background(), dataDir, runtimeDir)
	if err != nil {
		t.Fatalf("openServerStorage(after repair) error = %v", err)
	}
	if err := resources.Close(); err != nil {
		t.Fatalf("serverStorage.Close(after repair) error = %v", err)
	}
}

func TestOpenServerStorageRejectsCorruptKey(t *testing.T) {
	dataDir, runtimeDir := storageTestDirectories(t)
	resources, err := openServerStorage(context.Background(), dataDir, runtimeDir)
	if err != nil {
		t.Fatalf("openServerStorage(first) error = %v", err)
	}
	if err := resources.Close(); err != nil {
		t.Fatalf("serverStorage.Close() error = %v", err)
	}
	if err := os.WriteFile(tunnelTokenKeyPath(dataDir), []byte("truncated"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(corrupt key) error = %v", err)
	}

	if _, err := openServerStorage(context.Background(), dataDir, runtimeDir); !errors.Is(err, tokenkey.ErrUnavailable) {
		t.Fatalf("openServerStorage() error = %v, want tokenkey.ErrUnavailable", err)
	}
}

func storageTestDirectories(t *testing.T) (string, string) {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatalf("os.Mkdir(dataDir) error = %v", err)
	}
	runtimeDir := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatalf("os.Mkdir(runtimeDir) error = %v", err)
	}
	return dataDir, runtimeDir
}

func tunnelTokenKeyPath(dataDir string) string {
	return filepath.Join(dataDir, "credentials", "tunnel-token.key")
}
