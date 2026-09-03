//go:build windows

package gateway

import (
	"path/filepath"
	"testing"

	"github.com/lifei6671/xtunnel/internal/server/winsecurity"
)

// newGatewayTestDataDir mirrors the foreground Profile boundary so general
// Gateway lifecycle tests never depend on an inherited t.TempDir DACL.
func newGatewayTestDataDir(t *testing.T) string {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "managed-data")
	security, err := winsecurity.NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := winsecurity.CreateForegroundDirectory(dataDir, security); err != nil {
		t.Fatalf("CreateForegroundDirectory(%q) error = %v", dataDir, err)
	}
	return dataDir
}
