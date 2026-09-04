//go:build windows

package bootstrap

import (
	"path/filepath"
	"testing"

	"github.com/lifei6671/xtunnel/internal/server/winsecurity"
)

// newGatewayAuditTestDataDir 为会写入 pinned Gateway 身份的测试创建受保护的
// 前台目录边界，避免依赖 t.TempDir 继承的 ACL。
func newGatewayAuditTestDataDir(t *testing.T) string {
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
