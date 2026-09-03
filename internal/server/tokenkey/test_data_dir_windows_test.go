//go:build windows

package tokenkey

import (
	"testing"

	"github.com/lifei6671/xtunnel/internal/server/winsecurity"
)

func prepareTestDataDir(t *testing.T, path string) {
	t.Helper()
	security, err := winsecurity.NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := winsecurity.CreateForegroundDirectory(path, security); err != nil {
		t.Fatalf("CreateForegroundDirectory(%q) error = %v", path, err)
	}
}
