//go:build windows

package integration

import (
	"path/filepath"
	"testing"

	"github.com/lifei6671/xtunnel/internal/server/winsecurity"
)

// newGatewayTestDataDir 为端到端测试中的 pinned Gateway 身份创建受保护的
// 前台目录边界，保证测试覆盖与生产写入策略一致。
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
