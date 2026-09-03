// Package provision 准备 Server 前台启动所需的平台目录。
package provision

// Initialize 创建并校验运行时目录及给定的 Server 数据目录。它只用于显式的
// 初始化命令；日常 Server 启动仍只验证既有目录，避免启动过程改变持久化边界。
func Initialize(dataDir string) error {
	return initialize(dataDir)
}
