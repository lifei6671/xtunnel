//go:build !linux

package gateway

import "os"

// Server 的离线维护命令仅支持 Linux；非 Linux 平台保留可编译的文件写入路径，
// 供配置和单元测试使用，真正的 External Lock 会在命令入口处拒绝执行。
func writeFileSync(path string, data []byte, mode os.FileMode) error {
	if err := os.WriteFile(path, data, mode); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func syncDirectory(string) error {
	return nil
}
