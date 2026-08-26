//go:build !linux

package limits

func currentFDLimit() (limit uint64, supported bool, err error) {
	// XTunnel Standalone V0.1 的 Server 部署范围只包含 Linux。其他平台保留可编译
	// 的开发路径，但不伪造不同内核的 FD 语义，因此显式返回 unsupported/no-op。
	return 0, false, nil
}
