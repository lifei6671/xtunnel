package datadir

import (
	"fmt"
	"os"
	"path/filepath"
)

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
