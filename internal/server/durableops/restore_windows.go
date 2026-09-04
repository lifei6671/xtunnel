//go:build windows

package durableops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lifei6671/xtunnel/internal/server/winsecurity"
)

// maxRestoreJournalSize 保持 Windows Journal 的兼容大小上限，供受保护 Journal
// 读写原语和测试共享；Windows Preview 启动期不再读取或自动收敛该 Journal。
const maxRestoreJournalSize = 64 << 10

// writeJournal publishes one complete, path-bound Restore Journal through the
// protected managed-file primitive. Restore has not enabled Windows switching
// yet, but phase writers must already refuse an invalid record rather than
// leave a Journal that startup cannot safely interpret.
func writeJournal(paths restorePaths, journal restoreJournal) error {
	data, err := json.Marshal(journal)
	if err != nil {
		return fmt.Errorf("encode Windows Restore Journal: %w", err)
	}
	if _, err := parseJournal(data, paths); err != nil {
		return fmt.Errorf("validate Windows Restore Journal before publication: %w", err)
	}
	security, err := winsecurity.NewForegroundFileSecurity()
	if err != nil {
		return fmt.Errorf("create Windows Restore Journal security: %w", err)
	}
	if err := winsecurity.PublishForegroundFile(filepath.Dir(paths.target), filepath.Base(paths.journal), data, security); err != nil {
		return fmt.Errorf("publish Windows Restore Journal: %w", err)
	}
	return nil
}

// restorePlatform 在 Windows 保持关闭，直到受保护 staging 提取、目录提升、
// 可证明的持久化屏障和无链接树删除共同完成。不能用普通路径 API 代替这些
// 边界，否则一次崩溃会让三阶段 Journal 无法安全收敛。
func restorePlatform(context.Context, restorePaths, string, int, TLSMode) (restoreResult, error) {
	return restoreResult{}, ErrUnsupported
}

// recoverPlatform 只接受没有任何 Restore 残留的 Windows Data Root。Windows
// Preview 不实现目录切换或崩溃恢复；即使 Journal 看似已处于可安全删除的尾项，也
// 必须保留现场并拒绝启动，避免把未来的恢复协议误当成当前受支持的运行时能力。
func recoverPlatform(ctx context.Context, paths restorePaths) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	parent := filepath.Dir(paths.target)
	if err := winsecurity.ValidateDataParentDirectory(parent); err != nil {
		return false, fmt.Errorf("validate Windows Restore parent: %w", err)
	}
	targetExists, err := winsecurity.ForegroundDirectoryExists(paths.target)
	if err != nil {
		return false, fmt.Errorf("validate Windows Restore target: %w", err)
	}

	if !targetExists {
		return false, errors.New("Windows Restore target directory is missing")
	}

	for _, artifact := range []struct {
		name string
		path string
	}{
		{name: "staging", path: paths.staging},
		{name: "rollback", path: paths.rollback},
	} {
		exists, checkErr := winsecurity.ForegroundDirectoryExists(artifact.path)
		if checkErr != nil {
			return false, fmt.Errorf("validate Windows Restore %s directory: %w", artifact.name, checkErr)
		}
		if exists {
			return false, fmt.Errorf("Windows Restore %s directory remains; manual maintenance is required", artifact.name)
		}
	}
	if _, err := os.Lstat(paths.journal); err == nil {
		return false, errors.New("Windows Restore Journal remains; manual maintenance is required")
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("inspect Windows Restore Journal: %w", err)
	}
	return false, nil
}
