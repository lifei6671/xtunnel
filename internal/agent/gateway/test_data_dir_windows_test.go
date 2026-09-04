//go:build windows

package gateway

import (
	"path/filepath"
	"testing"

	"github.com/lifei6671/xtunnel/internal/server/winsecurity"
)

// newGatewayTestDataDir 创建 Server 写入 pinned Gateway 身份前所需的受保护
// 前台目录边界。
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
