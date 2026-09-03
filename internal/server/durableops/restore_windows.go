//go:build windows

package durableops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/lifei6671/xtunnel/internal/server/winsecurity"
	"golang.org/x/sys/windows"
)

// maxRestoreJournalSize 限制启动期读取未可信 Journal 时的内存占用。
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

// recoverPlatform 允许已经由 init 创建并保护的前台目录在无恢复残留时正常启动。
// prepared 的 target + staging 组合和 prepared、V2 rollback_restoring 的 target-only
// 尾项是唯一已开启的 Journal 收敛。前者在仍持有 Journal 的已验证 Handle 时按
// “安全删除 staging → 同步 parent → 删除 Journal → 再同步 parent”收敛；后两者
// 也在 Journal 删除前后同步 parent。
// prepared 从不移动旧 target，rollback_restoring 只表示旧 target 已恢复且 Journal
// 尚未删除。其余 Journal、staging 或 rollback 组合均保留现场并拒绝启动，绝不猜测
// 删除或提升哪一侧。
func recoverPlatform(ctx context.Context, paths restorePaths) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	parent := filepath.Dir(paths.target)
	if err := winsecurity.ValidateForegroundDirectory(parent); err != nil {
		return false, fmt.Errorf("validate Windows Restore parent: %w", err)
	}
	targetExists, err := winsecurity.ForegroundDirectoryExists(paths.target)
	if err != nil {
		return false, fmt.Errorf("validate Windows Restore target: %w", err)
	}

	journalName := filepath.Base(paths.journal)
	security, securityErr := winsecurity.NewForegroundFileSecurity()
	if securityErr != nil {
		return false, fmt.Errorf("create Windows Restore Journal security: %w", securityErr)
	}
	recoveredPreparedTail := false
	err = winsecurity.ConsumeForegroundFileLimitWithPostDelete(parent, journalName, maxRestoreJournalSize, security, func(journalData []byte) error {
		journal, parseErr := parseJournal(journalData, paths)
		if parseErr != nil {
			return fmt.Errorf("validate Windows Restore Journal: %w", parseErr)
		}
		currentTargetExists, checkErr := winsecurity.ForegroundDirectoryExists(paths.target)
		if checkErr != nil {
			return fmt.Errorf("validate Windows Restore target directory: %w", checkErr)
		}
		stagingExists, checkErr := winsecurity.ForegroundDirectoryExists(paths.staging)
		if checkErr != nil {
			return fmt.Errorf("validate Windows Restore staging directory: %w", checkErr)
		}
		rollbackExists, checkErr := winsecurity.ForegroundDirectoryExists(paths.rollback)
		if checkErr != nil {
			return fmt.Errorf("validate Windows Restore rollback directory: %w", checkErr)
		}
		preparedStagingTail := journal.Phase == phasePrepared && stagingExists
		journalOnlyTail := (journal.Phase == phasePrepared ||
			(journal.Version == restoreJournalVersionV2 && journal.Phase == phaseRollbackRestoring)) && !stagingExists
		if !currentTargetExists || rollbackExists || (!preparedStagingTail && !journalOnlyTail) {
			return errors.New("Windows Restore Journal requires unavailable recovery primitives")
		}
		if preparedStagingTail {
			// 删除阶段会逐节点重新校验身份；Journal Handle 在整个 callback 中保持打开，
			// 因而 staging 清理和后续 Journal 删除不会跨越未经验证的 Journal 替换。
			if _, err := winsecurity.RemoveForegroundDirectoryTree(ctx, parent, filepath.Base(paths.staging)); err != nil {
				return fmt.Errorf("remove prepared Windows Restore staging directory: %w", err)
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// staging 已删的重试也必须先完成此同步，避免直接从 target-only 尾项
		// 越过上次未完成的目录项屏障。
		if err := winsecurity.SyncForegroundDirectory(parent); err != nil {
			return fmt.Errorf("sync Windows Restore parent before Journal cleanup: %w", err)
		}
		recoveredPreparedTail = true
		return nil
	}, func() error {
		// 此回调在 Journal DELETE Handle 已关闭且已验证 parent Handle 仍持有时运行。
		// 失败表示 Journal 可能已删除但最后一个目录项屏障未完成，不能报告恢复成功。
		if err := winsecurity.SyncForegroundDirectory(parent); err != nil {
			return fmt.Errorf("sync Windows Restore parent after Journal cleanup: %w", err)
		}
		return nil
	})
	if err == nil {
		return recoveredPreparedTail, nil
	}
	if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) && !errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return false, fmt.Errorf("consume Windows Restore Journal: %w", err)
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
			return false, fmt.Errorf("Windows Restore %s directory exists without a Journal", artifact.name)
		}
	}
	return false, nil
}
