package datadir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrPendingRestore 表示当前稳定数据目标仍有待恢复的 Restore Journal。
var ErrPendingRestore = errors.New("pending restore journal")

// CheckPendingRestore 在打开 SQLite 前拒绝仍有 Restore Journal 的目标。
// M3-12 将在相同位置接入正式的完成或回滚状态机。
func CheckPendingRestore(target Target) error {
	journalPath := filepath.Join(target.Parent, ".xtunnel-restore-"+target.Hash+".journal")
	_, err := os.Lstat(journalPath)
	switch {
	case err == nil:
		return fmt.Errorf("%w: %s", ErrPendingRestore, journalPath)
	case errors.Is(err, os.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("inspect restore journal %q: %w", journalPath, err)
	}
}

// ValidateCanonical 在取得 External Lock 后校验正式数据目录。
func ValidateCanonical(target Target) error {
	info, err := os.Lstat(target.Path)
	if err != nil {
		return fmt.Errorf("inspect server data directory %q: %w", target.Path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("server data directory %q must not be a symbolic link", target.Path)
	}
	if !info.IsDir() {
		return fmt.Errorf("server data path %q is not a directory", target.Path)
	}

	canonicalPath, err := filepath.EvalSymlinks(target.Path)
	if err != nil {
		return fmt.Errorf("resolve server data directory %q: %w", target.Path, err)
	}
	canonicalPath, err = filepath.Abs(canonicalPath)
	if err != nil {
		return fmt.Errorf("make server data directory absolute: %w", err)
	}
	if filepath.Clean(canonicalPath) != filepath.Clean(target.Path) {
		return fmt.Errorf("server data directory resolved to %q, want stable target %q", canonicalPath, target.Path)
	}
	return nil
}
