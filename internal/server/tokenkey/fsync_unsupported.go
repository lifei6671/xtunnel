//go:build !linux

package tokenkey

// XTunnel Server 的生产部署目标是 Linux。其他平台保留相同原子 rename 流程，
// 但目录 fsync 没有跨平台一致语义，因此只用于本地构建与单元测试。
func syncDirectory(string) error {
	return nil
}
