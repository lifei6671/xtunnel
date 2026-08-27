//go:build !linux

package bootstrap

import "errors"

// requireBackupMaintenanceRoot 在非 Linux 平台始终拒绝维护命令；V0.1 的
// openat2、SO_PEERCRED、owner 和原子目录交换契约没有跨平台兼容实现。
func requireBackupMaintenanceRoot() error {
	return errors.New("Server backup maintenance is only supported on Linux in XTunnel V0.1")
}
