//go:build windows

// windows-server-startup 仅用于一次性提升权限 CI 的 Server 启动失败调查。
// 它保留产品 SCM 身份与启动路径，将 Dispatcher 返回后的 stderr 留在受管
// Runtime 中供管理员读取；调查结果不构成产品 SCM 验收通过证据。
package main

import (
	"bytes"
	"os"
	"path/filepath"

	"github.com/lifei6671/xtunnel/internal/server/bootstrap"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

func main() {
	os.Exit(run())
}

func run() int {
	managed, err := svc.IsWindowsService()
	if err != nil {
		return 1
	}
	if !managed {
		return bootstrap.Execute(os.Args[0], os.Args[1:], os.Environ(), os.Stderr)
	}

	var diagnostic bytes.Buffer
	code := bootstrap.Execute(os.Args[0], os.Args[1:], os.Environ(), &diagnostic)
	programData, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil {
		return 1
	}
	path := filepath.Join(programData, "XTunnel", "Server", "runtime", "scm-startup-diagnostic.txt")
	// 独占创建保留首个故障，后续 SCM recovery 不覆盖它。目录由正式安装器
	// 创建并保护，文件继承现有 Service SID 与 OWNER RIGHTS ACL，不改权限。
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return 1
	}
	if _, err := file.Write(diagnostic.Bytes()); err != nil {
		_ = file.Close()
		return 1
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return 1
	}
	if err := file.Close(); err != nil {
		return 1
	}
	// 调查脚本只在完成标记出现后读取报告，避免读到刚创建的空文件或部分内容。
	complete, err := os.OpenFile(path+".complete", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return 1
	}
	if err := complete.Close(); err != nil {
		return 1
	}
	return code
}
