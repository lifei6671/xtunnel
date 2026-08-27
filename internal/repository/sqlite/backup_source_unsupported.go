//go:build !linux

package sqlite

import (
	"context"
	"errors"
)

// errBackupSourceUnsupported 是非 Linux 平台统一的快速失败结果。安全源固定依赖
// Linux openat2 和 /proc/self/fd，其他平台不得退化为普通路径打开。
var errBackupSourceUnsupported = errors.New("secure SQLite backup source is supported only on Linux")

// BackupSource 是非 Linux 的编译期占位类型，仅用于保持调用方 API 一致；它不持有
// 文件或数据库资源，也不能执行任何维护操作。
type BackupSource struct{}

// OpenBackupSource 在非 Linux 平台快速失败，避免绕过 openat2 路径和 inode 约束。
func OpenBackupSource(string) (*BackupSource, error) {
	return nil, errBackupSourceUnsupported
}

// Close 为未成功创建资源的占位对象提供幂等清理语义。
func (*BackupSource) Close() error {
	return nil
}

// InspectSchemaVersion 在非 Linux 平台快速失败，且不会打开或迁移数据库。
func (*BackupSource) InspectSchemaVersion(context.Context) (int, error) {
	return 0, errBackupSourceUnsupported
}

// BackupSQLite 在非 Linux 平台快速失败，且不会创建目标文件。
func (*BackupSource) BackupSQLite(context.Context, string) error {
	return errBackupSourceUnsupported
}
