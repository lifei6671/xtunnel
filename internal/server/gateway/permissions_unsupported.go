//go:build !linux

package gateway

import "os"

// Server 的 External Lock 与离线维护入口仅支持 Linux；非 Linux 不能从 Go 的 FileMode
// 得到可比的 ACL 权限，因此不把伪造的 0666 视为生产校验证据。
func privateKeyPermissionsValid(os.FileMode) bool {
	return true
}
