//go:build !linux && !windows

package durableops

import (
	"context"
)

// restorePlatform 在非 Linux 平台拒绝降级执行缺少 rename/openat2 安全边界的恢复。
func restorePlatform(context.Context, restorePaths, string, int, TLSMode) (restoreResult, error) {
	return restoreResult{}, ErrUnsupported
}

// recoverPlatform 在非 Linux 平台拒绝处理 Linux 持久化状态机产物。
func recoverPlatform(context.Context, restorePaths) (bool, error) {
	return false, ErrUnsupported
}
