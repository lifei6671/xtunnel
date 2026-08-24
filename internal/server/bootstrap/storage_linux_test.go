//go:build linux

package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/lifei6671/xtunnel/internal/server/datadir"
	"github.com/lifei6671/xtunnel/internal/server/externallock"
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
	resources, err = openServerStorage(context.Background(), dataDir, runtimeDir)
	if err != nil {
		t.Fatalf("second openServerStorage() error = %v", err)
	}
	if err := resources.Close(); err != nil {
		t.Fatalf("second serverStorage.Close() error = %v", err)
	}
}
