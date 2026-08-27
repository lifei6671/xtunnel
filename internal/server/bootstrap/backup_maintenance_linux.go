//go:build linux

package bootstrap

import (
	"errors"
	"os"
)

// requireBackupMaintenanceRoot 强制 Linux 维护命令以 root 运行。备份需要读取
// 0600 的凭据与身份文件，恢复还会执行 owner/mode 归一化，普通用户不可降级执行。
func requireBackupMaintenanceRoot() error {
	if os.Geteuid() != 0 {
		return errors.New("Server backup maintenance requires root")
	}
	return nil
}
