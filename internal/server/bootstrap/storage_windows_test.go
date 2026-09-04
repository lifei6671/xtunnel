//go:build windows

package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/lifei6671/xtunnel/internal/server/datadir"
	"github.com/lifei6671/xtunnel/internal/server/externallock"
	"github.com/lifei6671/xtunnel/internal/server/winsecurity"
)

func TestWindowsOpenServerStorageAllowsCleanManagedTarget(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "server")
	directorySecurity, err := winsecurity.NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := winsecurity.CreateForegroundDirectory(parent, directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectory(parent) error = %v", err)
	}
	dataDir := filepath.Join(parent, "data")
	if err := winsecurity.CreateForegroundDirectory(dataDir, directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectory(data) error = %v", err)
	}
	runtimeDir := filepath.Join(t.TempDir(), "runtime")
	if err := winsecurity.CreateForegroundDirectory(runtimeDir, directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectory(runtime) error = %v", err)
	}

	storage, err := openServerStorage(context.Background(), dataDir, runtimeDir)
	if err != nil {
		t.Fatalf("openServerStorage() error = %v", err)
	}
	if err := storage.Close(); err != nil {
		t.Fatalf("serverStorage.Close() error = %v", err)
	}
}

func TestWindowsOpenServerStorageLocksBeforeDurableState(t *testing.T) {
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatalf("os.Mkdir(data) error = %v", err)
	}
	runtimeDir := filepath.Join(t.TempDir(), "runtime")
	directorySecurity, err := winsecurity.NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := winsecurity.CreateForegroundDirectory(runtimeDir, directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectory(runtime) error = %v", err)
	}
	target, err := datadir.Resolve(dataDir)
	if err != nil {
		t.Fatalf("datadir.Resolve() error = %v", err)
	}
	journalPath := filepath.Join(parent, ".xtunnel-restore-"+target.Hash+".journal")
	journalContent := []byte("malformed pending journal")
	if err := os.WriteFile(journalPath, journalContent, 0o600); err != nil {
		t.Fatalf("os.WriteFile(journal) error = %v", err)
	}
	journalInfo, err := os.Stat(journalPath)
	if err != nil {
		t.Fatalf("os.Stat(journal before lock rejection) error = %v", err)
	}
	heldLock, err := externallock.Acquire(runtimeDir, target.Hash)
	if err != nil {
		t.Fatalf("externallock.Acquire() error = %v", err)
	}
	defer func() {
		if err := heldLock.Close(); err != nil {
			t.Errorf("held Lock.Close() error = %v", err)
		}
	}()

	if _, err := openServerStorage(context.Background(), dataDir, runtimeDir); !errors.Is(err, externallock.ErrAlreadyLocked) {
		t.Fatalf("openServerStorage() error = %v, want ErrAlreadyLocked", err)
	}
	for _, path := range []string{
		filepath.Join(dataDir, "xtunnel.db"),
		filepath.Join(dataDir, "credentials", "tunnel-token.key"),
	} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("durable state %q was touched before lock rejection: %v", path, err)
		}
	}
	gotJournal, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatalf("os.ReadFile(journal) error = %v", err)
	}
	if string(gotJournal) != string(journalContent) {
		t.Fatalf("Restore Journal changed before lock rejection: got %q, want %q", gotJournal, journalContent)
	}
	afterJournalInfo, err := os.Stat(journalPath)
	if err != nil {
		t.Fatalf("os.Stat(journal after lock rejection) error = %v", err)
	}
	if !os.SameFile(journalInfo, afterJournalInfo) {
		t.Fatal("Restore Journal file identity changed before lock rejection")
	}
}
