//go:build windows

package pathprofile

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// resolve 只接受当前用户前台 Profile 或未来 SCM 使用的固定 Service Profile。
// 这让两个身份永远不共享 SQLite、密钥、Journal 与锁目录；调用方不能通过任意
// 自定义路径把 Service 数据意外暴露给前台进程。
func resolveForeground(dataDir string) (Profile, error) {
	localAppData, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return Profile{}, fmt.Errorf("resolve Windows LocalAppData known folder: %w", err)
	}
	foregroundData := filepath.Join(localAppData, "XTunnel", "Server", "data")
	foregroundRuntime := filepath.Join(localAppData, "XTunnel", "Server", "runtime")

	switch {
	case dataDir == AutomaticDataDir, strings.EqualFold(filepath.Clean(dataDir), foregroundData):
		return Profile{
			DataDir:     foregroundData,
			RuntimeDir:  foregroundRuntime,
			ManagedRoot: filepath.Join(localAppData, "XTunnel"),
		}, nil
	default:
		return Profile{}, fmt.Errorf(
			"Windows foreground server.data_dir must be %q or %q, got %q",
			AutomaticDataDir, foregroundData, dataDir,
		)
	}
}

func resolveService(dataDir string) (Profile, error) {
	programData, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return Profile{}, fmt.Errorf("resolve Windows ProgramData known folder: %w", err)
	}
	serviceData := filepath.Join(programData, "XTunnel", "Server", "data")
	serviceRuntime := filepath.Join(programData, "XTunnel", "Server", "runtime")
	if dataDir == AutomaticDataDir || strings.EqualFold(filepath.Clean(dataDir), serviceData) {
		return Profile{
			DataDir:     serviceData,
			RuntimeDir:  serviceRuntime,
			ManagedRoot: filepath.Join(programData, "XTunnel"),
		}, nil
	}
	return Profile{}, fmt.Errorf("Windows service server.data_dir must be %q or %q, got %q", AutomaticDataDir, serviceData, dataDir)
}
