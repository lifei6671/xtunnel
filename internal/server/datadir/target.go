// Package datadir 负责 Server 数据目录的稳定寻址和获锁后校验。
package datadir

// Target 描述不依赖 leaf 当前是否存在的稳定数据目标。
type Target struct {
	Path   string
	Parent string
	Leaf   string
	Hash   string

	// Windows 必须把 Stable Target 绑定到父目录对象，而不是只相信可被替换的
	// 路径字符串。Linux 继续由 realpath 语义校验，这两个字段保持为零值。
	parentVolume uint64
	parentFile   [16]byte
	parentBound  bool
}

// Resolve 计算平台对应的稳定数据目标。该阶段只访问父目录，不读取、创建或
// 解析正式数据目录 leaf，因此 Restore 中间态缺少 leaf 时仍能取得同一把锁。
func Resolve(dataDir string) (Target, error) {
	return resolve(dataDir)
}
