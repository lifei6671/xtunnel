//go:build linux

package gateway

import "os"

// privateKeyPermissionsValid 在支持 Unix 权限的生产平台强制私钥恰为 0600。
func privateKeyPermissionsValid(mode os.FileMode) bool {
	return mode.Perm() == 0o600
}
