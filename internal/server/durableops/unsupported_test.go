//go:build !linux && !windows

package durableops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/lifei6671/xtunnel/internal/server/datadir"
)

func TestUnsupportedOperationsFailBeforeDiskMutation(t *testing.T) {
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatalf("os.Mkdir(dataDir) error = %v", err)
	}
	target, err := datadir.Resolve(dataDir)
	if err != nil {
		t.Fatalf("datadir.Resolve() error = %v", err)
	}
	outputPath := filepath.Join(parent, "backup.tar")
	callbackCalled := false
	_, err = Create(context.Background(), CreateOptions{
		DataDir: dataDir, TLSMode: TLSModePublic, OutputPath: outputPath,
		BackupDatabase: func(context.Context, string) (int, error) {
			callbackCalled = true
			return 1, nil
		},
	})
	if !errors.Is(err, ErrUnsupported) || callbackCalled {
		t.Fatalf("Create() error = %v, callbackCalled = %t", err, callbackCalled)
	}
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsupported Create() changed output path: %v", err)
	}
	if _, err := Restore(context.Background(), target, filepath.Join(parent, "missing.tar"), 1, TLSModePublic); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Restore() error = %v, want ErrUnsupported", err)
	}
	if recovered, err := RecoverPendingRestore(context.Background(), target); !errors.Is(err, ErrUnsupported) || recovered {
		t.Fatalf("RecoverPendingRestore() = (%t, %v), want (false, ErrUnsupported)", recovered, err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("os.ReadDir(parent) error = %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "data" {
		t.Fatalf("unsupported operations changed parent contents: %#v", entries)
	}
}
