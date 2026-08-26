//go:build !linux

package tokenkey

import "os"

// Windows 不提供 Unix mode bit 语义；生产 Linux 路径在对应实现中强制精确权限。
func directoryPermissionsValid(os.FileMode) bool { return true }

func keyPermissionsValid(os.FileMode) bool { return true }
