//go:build linux

package durableops

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// maxRestoreJournalSize 限制崩溃恢复读取未可信 Journal 时的内存占用。
const maxRestoreJournalSize = 64 << 10

// restorePlatform 在调用方持有 External Lock 时完成恢复事务。
// 数据先在同盘 staging 中提取、fsync 并通过可启动语义校验，随后以两次 rename
// 发布；每个不可逆边界前后都持久化 Journal，使任意崩溃点都能确定回滚或收尾。
func restorePlatform(ctx context.Context, paths restorePaths, inputPath string, currentSchemaVersion int, expectedTLSMode TLSMode) (restoreResult, error) {
	return restorePlatformWithSwitchOps(
		ctx, paths, inputPath, currentSchemaVersion, expectedTLSMode, productionRestoreSwitchOps(),
	)
}

// restoreSwitchOps 只承载 Restore 的两次目录切换及其父目录持久化屏障。
// 该组操作按值传递，测试可在真实 rename 后暂停子进程，不会污染其他 Restore。
type restoreSwitchOps struct {
	rename     func(string, string) error
	syncParent func(string) error
}

func productionRestoreSwitchOps() restoreSwitchOps {
	return restoreSwitchOps{
		rename:     os.Rename,
		syncParent: syncDirectory,
	}
}

func restorePlatformWithSwitchOps(
	ctx context.Context,
	paths restorePaths,
	inputPath string,
	currentSchemaVersion int,
	expectedTLSMode TLSMode,
	switchOps restoreSwitchOps,
) (result restoreResult, resultErr error) {
	if expectedTLSMode != TLSModePinned && expectedTLSMode != TLSModePublic {
		return restoreResult{}, fmt.Errorf("expected TLS mode %q is invalid", expectedTLSMode)
	}
	if _, err := recoverPlatform(ctx, paths); err != nil {
		return restoreResult{}, err
	}
	if occupied, err := anyRestoreArtifactExists(paths); err != nil {
		return restoreResult{}, err
	} else if occupied {
		return restoreResult{}, errors.New("restore staging or rollback path exists without a journal")
	}

	targetInfo, err := os.Lstat(paths.target)
	if err != nil {
		return restoreResult{}, fmt.Errorf("inspect restore target: %w", err)
	}
	if targetInfo.Mode()&os.ModeSymlink != 0 || !targetInfo.IsDir() {
		return restoreResult{}, errors.New("restore target must be a non-symbolic-link directory")
	}
	owner, ok := targetInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return restoreResult{}, errors.New("restore target owner is unavailable")
	}

	if err := os.Mkdir(paths.staging, 0o700); err != nil {
		return restoreResult{}, fmt.Errorf("create restore staging directory: %w", err)
	}
	cleanupStaging := true
	defer func() {
		if cleanupStaging {
			if err := removeDirectoryTree(paths.staging); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("remove failed restore staging directory: %w", err))
			}
		}
	}()
	input, err := openBackupInput(inputPath)
	if err != nil {
		return restoreResult{}, err
	}
	defer func() {
		if err := input.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close backup input: %w", err))
		}
	}()
	countedInput := &countingReader{reader: input}
	archive := tar.NewReader(&contextReader{ctx: ctx, reader: countedInput})
	manifest, manifestData, err := readManifest(archive, currentSchemaVersion)
	if err != nil {
		return restoreResult{}, err
	}
	if manifest.TLSMode != expectedTLSMode {
		return restoreResult{}, fmt.Errorf("backup TLS mode is %q, want %q", manifest.TLSMode, expectedTLSMode)
	}
	manifestDigest := sha256.Sum256(manifestData)
	manifestHash := hex.EncodeToString(manifestDigest[:])
	if err := extractManifestFiles(archive, paths.staging, manifest, int(owner.Uid), int(owner.Gid)); err != nil {
		return restoreResult{}, err
	}
	inputInfo, err := input.Stat()
	if err != nil {
		return restoreResult{}, fmt.Errorf("inspect consumed backup input: %w", err)
	}
	if countedInput.read != inputInfo.Size() {
		return restoreResult{}, fmt.Errorf("backup archive contains %d undeclared trailing bytes", inputInfo.Size()-countedInput.read)
	}
	if err := syncManifestDirectories(paths.staging, manifest); err != nil {
		return restoreResult{}, err
	}
	if err := validateRestoredState(ctx, paths.staging, manifest); err != nil {
		return restoreResult{}, fmt.Errorf("validate restore staging state: %w", err)
	}
	// 子目录与文件虽已归一化为 Runtime owner，但 staging 根直到完整校验通过都
	// 保持 root:0700；最后才交付根目录，避免同 UID 进程在提取期间替换中间路径。
	if err := os.Chown(paths.staging, int(owner.Uid), int(owner.Gid)); err != nil {
		return restoreResult{}, fmt.Errorf("normalize restore staging owner: %w", err)
	}
	if err := syncDirectory(paths.staging); err != nil {
		return restoreResult{}, fmt.Errorf("sync restore staging directory: %w", err)
	}

	journal := restoreJournal{
		Version: restoreJournalVersion, ManifestSHA256: manifestHash, StableTarget: paths.target,
		Manifest: manifest, Staging: paths.staging, Rollback: paths.rollback, Phase: phasePrepared,
	}
	if err := writeJournal(paths, journal, int(owner.Uid), int(owner.Gid)); err != nil {
		return restoreResult{}, err
	}
	// Journal 一旦落盘，后续失败不再由 defer 删除 staging；下次启动必须
	// 在同一外部锁下按 Journal 与三个目录的实际状态收敛。
	cleanupStaging = false

	if err := switchOps.rename(paths.target, paths.rollback); err != nil {
		return restoreResult{}, fmt.Errorf("rename restore target to rollback: %w", err)
	}
	if err := switchOps.syncParent(filepath.Dir(paths.target)); err != nil {
		return restoreResult{}, fmt.Errorf("sync stable data parent after rollback rename: %w", err)
	}
	journal.Phase = phaseRollbackReady
	if err := writeJournal(paths, journal, int(owner.Uid), int(owner.Gid)); err != nil {
		return restoreResult{}, err
	}
	if err := switchOps.rename(paths.staging, paths.target); err != nil {
		return restoreResult{}, fmt.Errorf("rename restore staging to target: %w", err)
	}
	if err := switchOps.syncParent(filepath.Dir(paths.target)); err != nil {
		return restoreResult{}, fmt.Errorf("sync stable data parent after target install: %w", err)
	}
	if err := validateRestoredState(ctx, paths.target, manifest); err != nil {
		rollbackErr := rollbackInstalledTarget(paths, &journal)
		return restoreResult{}, errors.Join(fmt.Errorf("validate installed restore target: %w", err), rollbackErr)
	}
	journal.Phase = phaseInstalled
	if err := writeJournal(paths, journal, int(owner.Uid), int(owner.Gid)); err != nil {
		return restoreResult{}, err
	}
	if err := finishInstalled(paths); err != nil {
		return restoreResult{}, err
	}
	return restoreResult{Manifest: manifest, ManifestSHA256: manifestHash}, nil
}

// recoverPlatform 根据已落盘 phase 和 target/staging/rollback 的实际组合收敛状态。
// 原则是：尚未证明新 target 完整有效时优先恢复旧 rollback；只有重新通过完整
// 状态校验才允许删除 rollback。调用方持有 External Lock，期间没有并发切换者。
func recoverPlatform(ctx context.Context, paths restorePaths) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	removedTemporaryJournals, err := cleanupRestoreJournalTemps(paths)
	if err != nil {
		return false, err
	}
	journalInfo, err := os.Lstat(paths.journal)
	if errors.Is(err, os.ErrNotExist) {
		recovered, recoverErr := recoverWithoutJournal(paths)
		return removedTemporaryJournals || recovered, recoverErr
	}
	if err != nil {
		return false, fmt.Errorf("inspect restore journal: %w", err)
	}
	if journalInfo.Mode()&os.ModeSymlink != 0 || !journalInfo.Mode().IsRegular() || journalInfo.Mode().Perm() != 0o600 {
		return false, errors.New("restore journal must be a regular non-symbolic-link 0600 file")
	}
	journalFile, err := openRegularNoFollow(paths.journal)
	if err != nil {
		return false, fmt.Errorf("open restore journal without following links: %w", err)
	}
	openedJournalInfo, statErr := journalFile.Stat()
	if statErr != nil || !os.SameFile(journalInfo, openedJournalInfo) || openedJournalInfo.Mode().Perm() != 0o600 {
		closeErr := journalFile.Close()
		return false, errors.Join(errors.New("restore journal changed while being opened"), statErr, closeErr)
	}
	data, readErr := io.ReadAll(io.LimitReader(journalFile, maxRestoreJournalSize+1))
	closeErr := journalFile.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return false, fmt.Errorf("read restore journal: %w", err)
	}
	if len(data) > maxRestoreJournalSize {
		return false, errors.New("restore journal exceeds maximum size")
	}
	journal, err := parseJournal(data, paths)
	if err != nil {
		return false, err
	}
	targetExists, err := directoryExists(paths.target)
	if err != nil {
		return false, err
	}
	stagingExists, err := directoryExists(paths.staging)
	if err != nil {
		return false, err
	}
	rollbackExists, err := directoryExists(paths.rollback)
	if err != nil {
		return false, err
	}

	// Journal 写入与目录 rename 之间可能在任一系统调用后崩溃，因此 phase 不能
	// 单独决定动作。下面显式枚举可达组合；未枚举组合可能来自篡改或人工操作，
	// 必须 fail closed 并保留现场，而不是猜测删除哪一侧。
	switch journal.Phase {
	case phasePrepared:
		if targetExists && stagingExists && !rollbackExists {
			return true, rollbackPrepared(paths)
		}
		if targetExists && !stagingExists && !rollbackExists {
			// prepared 尚未承诺移动旧 target。这个组合来自 rollbackPrepared 已经
			// 持久化删除 staging、但尚未删除 Journal 时再次崩溃；旧 target
			// 始终未被替换，因此只需收掉 Journal，不能拿新 Manifest 重验旧数据。
			return true, removePreparedJournal(paths)
		}
		if !targetExists && stagingExists && rollbackExists {
			// target -> rollback 可能已落盘，但 phase 更新未落盘。
			// 优先回滚到旧 target，不将未标记可提交的 staging 猜测为新状态。
			if err := restoreRollbackOnly(paths, &journal); err != nil {
				return false, err
			}
			return true, finishRollbackRestoration(paths, true)
		}
	case phaseRollbackReady:
		if !targetExists && stagingExists && rollbackExists {
			// 两次 rename 之间尚未有新 target，固定优先恢复旧目录；
			// 只有已观测到 target 的状态才可视为新数据已发布。
			if err := restoreRollbackOnly(paths, &journal); err != nil {
				return false, err
			}
			return true, finishRollbackRestoration(paths, true)
		}
		if targetExists && !stagingExists && rollbackExists {
			if err := validateRestoredState(ctx, paths.target, journal.Manifest); err != nil {
				if rollbackErr := rollbackInstalledTarget(paths, &journal); rollbackErr != nil {
					return false, errors.Join(fmt.Errorf("validate interrupted restore target: %w", err), rollbackErr)
				}
				return true, nil
			}
			journal.Version = restoreJournalVersion
			journal.Phase = phaseInstalled
			if err := writeJournal(paths, journal, os.Getuid(), os.Getgid()); err != nil {
				return false, err
			}
			return true, finishInstalled(paths)
		}
		if !targetExists && !stagingExists && rollbackExists {
			if err := restoreRollbackOnly(paths, &journal); err != nil {
				return false, err
			}
			return true, finishRollbackRestoration(paths, false)
		}
		if targetExists && !stagingExists && !rollbackExists {
			if journal.Version == restoreJournalVersionV1 {
				return false, errors.New("legacy version 1 rollback_ready Journal has ambiguous target-only state")
			}
			return false, errors.New("version 2 rollback_ready Journal cannot have a target-only state")
		}
	case phaseRollbackRestoring:
		if targetExists && !stagingExists && rollbackExists {
			// rollback_restoring 已在删除新 target 前持久化。此组合表示崩溃发生在
			// 回滚承诺之后、删除之前；不得重新验证或前向提交该 target。
			if err := removeDirectoryTree(paths.target); err != nil {
				return false, fmt.Errorf("remove rollback-restoring target: %w", err)
			}
			if err := syncDirectory(filepath.Dir(paths.target)); err != nil {
				return false, fmt.Errorf("sync restore parent after removing rollback-restoring target: %w", err)
			}
			if err := moveRollbackToTarget(paths); err != nil {
				return false, err
			}
			return true, finishRollbackRestoration(paths, false)
		}
		if !targetExists && stagingExists && rollbackExists {
			if err := restoreRollbackOnly(paths, &journal); err != nil {
				return false, err
			}
			return true, finishRollbackRestoration(paths, true)
		}
		if !targetExists && !stagingExists && rollbackExists {
			if err := restoreRollbackOnly(paths, &journal); err != nil {
				return false, err
			}
			return true, finishRollbackRestoration(paths, false)
		}
		if targetExists && stagingExists && !rollbackExists {
			return true, finishRollbackRestoration(paths, true)
		}
		if targetExists && !stagingExists && !rollbackExists {
			return true, finishRollbackRestoration(paths, false)
		}
	case phaseInstalled:
		if targetExists && !stagingExists && rollbackExists {
			if err := validateRestoredState(ctx, paths.target, journal.Manifest); err != nil {
				// installed 已经承诺新 target 完整有效。进程取消只表示本轮未能完成
				// 重验，不能把它解释为内容损坏并反向恢复旧数据；保留全部现场，
				// 让下一次持锁启动继续收敛。
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return false, fmt.Errorf("validate installed restore target: %w", err)
				}
				if rollbackErr := rollbackInstalledTarget(paths, &journal); rollbackErr != nil {
					return false, errors.Join(fmt.Errorf("validate installed restore target: %w", err), rollbackErr)
				}
				return true, nil
			}
			return true, finishInstalled(paths)
		}
		if !targetExists && !stagingExists && rollbackExists {
			if err := restoreRollbackOnly(paths, &journal); err != nil {
				return false, err
			}
			return true, finishRollbackRestoration(paths, false)
		}
		if targetExists && !stagingExists && !rollbackExists {
			if journal.Version == restoreJournalVersionV1 {
				return false, errors.New("legacy version 1 installed Journal has ambiguous target-only state")
			}
			if err := validateRestoredState(ctx, paths.target, journal.Manifest); err != nil {
				return false, fmt.Errorf("validate completed restore target: %w", err)
			}
			return true, removeCompletedJournal(paths)
		}
	}
	return false, fmt.Errorf("restore journal phase %q conflicts with target=%t staging=%t rollback=%t", journal.Phase, targetExists, stagingExists, rollbackExists)
}

// recoverWithoutJournal 只清理由 Journal 发布前失败留下的无害 staging。
// rollback 没有 Journal 就无法证明来源和提交阶段，必须保留并要求人工处理。
func recoverWithoutJournal(paths restorePaths) (bool, error) {
	targetExists, err := directoryExists(paths.target)
	if err != nil {
		return false, err
	}
	stagingExists, err := directoryExists(paths.staging)
	if err != nil {
		return false, err
	}
	rollbackExists, err := directoryExists(paths.rollback)
	if err != nil {
		return false, err
	}
	if targetExists && stagingExists && !rollbackExists {
		if err := removeDirectoryTree(paths.staging); err != nil {
			return false, fmt.Errorf("remove orphan pre-journal restore staging: %w", err)
		}
		return true, syncDirectory(filepath.Dir(paths.target))
	}
	if stagingExists || rollbackExists {
		return false, fmt.Errorf("restore artifacts exist without a journal: target=%t staging=%t rollback=%t", targetExists, stagingExists, rollbackExists)
	}
	return false, nil
}

// openBackupInput 要求归档是 0600 普通文件，并比较打开前后的文件身份。
// 中间路径和最终分量均不允许符号链接；返回 FD 由恢复流程关闭。
func openBackupInput(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect backup input: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, errors.New("backup input must be a regular non-symbolic-link 0600 file")
	}
	file, err := openAbsoluteRegularNoSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("open backup input: %w", err)
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(info, after) {
		closeErr := file.Close()
		return nil, errors.Join(errors.New("backup input changed while being opened"), err, closeErr)
	}
	return file, nil
}

// extractManifestFiles 严格按 Manifest 顺序提取到 root 独占的 staging。
// 每个文件在关闭前完成权限、owner、内容摘要和 fsync；最后要求 tar 流恰好结束，
// 不接受未声明条目。staging 根 owner 在完整校验后才由调用方交付给 Runtime。
func extractManifestFiles(archive *tar.Reader, staging string, manifest Manifest, uid, gid int) error {
	for _, expected := range manifest.Files {
		header, err := archive.Next()
		if err != nil {
			return fmt.Errorf("read backup file %q header: %w", expected.Path, err)
		}
		if err := validateArchiveHeader(expected, header); err != nil {
			return err
		}
		path := filepath.Join(staging, filepath.FromSlash(expected.Path))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("create restore directory for %q: %w", expected.Path, err)
		}
		if err := normalizeDirectoryChain(staging, filepath.Dir(path), uid, gid); err != nil {
			return err
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(expected.Mode))
		if err != nil {
			return fmt.Errorf("create restored file %q: %w", expected.Path, err)
		}
		digest := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(file, digest), archive)
		chmodErr := file.Chmod(os.FileMode(expected.Mode))
		chownErr := file.Chown(uid, gid)
		syncErr := file.Sync()
		closeErr := file.Close()
		if err := errors.Join(copyErr, chmodErr, chownErr, syncErr, closeErr); err != nil {
			return fmt.Errorf("write restored file %q: %w", expected.Path, err)
		}
		if written != expected.Size || hex.EncodeToString(digest.Sum(nil)) != expected.SHA256 {
			return fmt.Errorf("restored file %q content does not match its manifest", expected.Path)
		}
	}
	if header, err := archive.Next(); err == nil {
		return fmt.Errorf("backup archive contains unexpected entry %q", header.Name)
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("read end of backup archive: %w", err)
	}
	return nil
}

// normalizeDirectoryChain 将已创建的中间目录收敛为 Runtime owner 和 0700。
// 遍历在 staging 根停止，不提前放开根目录所有权，从而避免提取期路径替换。
func normalizeDirectoryChain(staging, directory string, uid, gid int) error {
	for path := directory; path != staging; path = filepath.Dir(path) {
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("inspect restored directory: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("restored directory is not a regular directory")
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("set restored directory permissions: %w", err)
		}
		if err := os.Chown(path, uid, gid); err != nil {
			return fmt.Errorf("normalize restored directory owner: %w", err)
		}
	}
	return nil
}

// syncManifestDirectories 按最深目录到 staging 根的顺序 fsync。
// 先持久化子目录条目再持久化父目录，确保后续 rename 发布的目录树在崩溃后完整。
func syncManifestDirectories(staging string, manifest Manifest) error {
	directories := map[string]struct{}{staging: {}}
	for _, file := range manifest.Files {
		for directory := filepath.Dir(filepath.Join(staging, filepath.FromSlash(file.Path))); directory != staging; directory = filepath.Dir(directory) {
			directories[directory] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(directories))
	for directory := range directories {
		ordered = append(ordered, directory)
	}
	slices.SortFunc(ordered, func(left, right string) int { return len(right) - len(left) })
	for _, directory := range ordered {
		if err := syncDirectory(directory); err != nil {
			return fmt.Errorf("sync restored directory %q: %w", directory, err)
		}
	}
	return nil
}

// anyRestoreArtifactExists 检查新事务是否会复用旧 staging/rollback 名称。
// Journal 已由 recoverPlatform 单独处理，因此任一残留都阻止开始新恢复。
func anyRestoreArtifactExists(paths restorePaths) (bool, error) {
	for _, path := range []string{paths.staging, paths.rollback} {
		exists, err := pathExists(path)
		if err != nil {
			return false, fmt.Errorf("inspect restore artifact: %w", err)
		}
		if exists {
			return true, nil
		}
	}
	return false, nil
}

// directoryExists 把恢复状态机路径归类为“安全目录”或“不存在”；
// 文件、符号链接等第三种状态直接报错，避免被状态组合逻辑误判。
func directoryExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect restore path %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, fmt.Errorf("restore path %q must be a non-symbolic-link directory", path)
	}
	return true, nil
}

// rollbackPrepared 收敛尚未移走旧 target 的 prepared 事务：删除未提交 staging，
// 再删除 Journal 并 fsync 父目录。旧 target 始终保持在线。
func rollbackPrepared(paths restorePaths) error {
	return rollbackPreparedWithSync(paths, syncDirectory)
}

// rollbackPreparedWithSync 先持久化 staging 删除，再删除 Journal 并再次同步父目录。
// 测试通过注入同步点验证：只要 Journal 消失，未提交 staging 的删除就一定已经落盘。
func rollbackPreparedWithSync(paths restorePaths, syncParent func(string) error) error {
	parent := filepath.Dir(paths.target)
	if err := removeDirectoryTree(paths.staging); err != nil {
		return fmt.Errorf("remove uncommitted restore staging: %w", err)
	}
	if err := syncParent(parent); err != nil {
		return fmt.Errorf("sync restore parent after removing uncommitted staging: %w", err)
	}
	if err := os.Remove(paths.journal); err != nil {
		return fmt.Errorf("remove rolled-back restore journal: %w", err)
	}
	return syncParent(parent)
}

// removePreparedJournal 收敛 staging 已持久化删除、旧 target 从未移动的 prepared
// 尾声。此时 Journal 是唯一残留目录项，删除并同步后即可回到正常启动状态。
func removePreparedJournal(paths restorePaths) error {
	if err := os.Remove(paths.journal); err != nil {
		return fmt.Errorf("remove completed prepared restore journal: %w", err)
	}
	return syncDirectory(filepath.Dir(paths.target))
}

// finishInstalled 在新 target 已重验通过后删除旧 rollback，随后删除 Journal。
// 两次父目录项变化分别持久化，保证 Journal 消失时 rollback 的删除一定已经落盘。
func finishInstalled(paths restorePaths) error {
	return finishInstalledWithSync(paths, syncDirectory)
}

// finishInstalledWithSync 保留 sync 注入点，使崩溃一致性的两道目录屏障可以被
// 确定性测试；生产路径始终传入 syncDirectory。
func finishInstalledWithSync(paths restorePaths, syncParent func(string) error) error {
	parent := filepath.Dir(paths.target)
	if err := removeDirectoryTree(paths.rollback); err != nil {
		return fmt.Errorf("remove restore rollback directory: %w", err)
	}
	if err := syncParent(parent); err != nil {
		return fmt.Errorf("sync restore parent after removing rollback: %w", err)
	}
	if err := os.Remove(paths.journal); err != nil {
		return fmt.Errorf("remove installed restore journal: %w", err)
	}
	return syncParent(parent)
}

// rollbackInstalledTarget 先持久化 V2 回滚意图，再删除未通过重验的新 target 并恢复旧
// rollback。这样在 rollback -> target 后、Journal 清理前崩溃时，target-only 状态仍可
// 被 rollback_restoring 唯一解释为旧状态。
func rollbackInstalledTarget(paths restorePaths, journal *restoreJournal) error {
	if err := markRollbackRestoring(paths, journal); err != nil {
		return err
	}
	if err := removeDirectoryTree(paths.target); err != nil {
		return fmt.Errorf("remove invalid installed restore target: %w", err)
	}
	if err := syncDirectory(filepath.Dir(paths.target)); err != nil {
		return fmt.Errorf("sync restore parent after removing invalid target: %w", err)
	}
	if err := moveRollbackToTarget(paths); err != nil {
		return err
	}
	return finishRollbackRestoration(paths, false)
}

// restoreRollbackOnly 在 target 缺失时，先把回滚意图持久化为 V2，再把唯一可信的
// rollback 原子改名回来。Journal 留到 caller 处理 staging 后删除，确保崩溃时
// target-only 组合仍由 rollback_restoring 解释为旧状态。
func restoreRollbackOnly(paths restorePaths, journal *restoreJournal) error {
	if err := markRollbackRestoring(paths, journal); err != nil {
		return err
	}
	return moveRollbackToTarget(paths)
}

func markRollbackRestoring(paths restorePaths, journal *restoreJournal) error {
	if journal == nil {
		return errors.New("restore Journal is nil")
	}
	journal.Version = restoreJournalVersion
	journal.Phase = phaseRollbackRestoring
	if err := writeJournal(paths, *journal, os.Getuid(), os.Getgid()); err != nil {
		return fmt.Errorf("persist rollback restoration intent: %w", err)
	}
	return nil
}

func moveRollbackToTarget(paths restorePaths) error {
	if err := os.Rename(paths.rollback, paths.target); err != nil {
		return fmt.Errorf("restore rollback directory as stable target: %w", err)
	}
	if err := syncDirectory(filepath.Dir(paths.target)); err != nil {
		return fmt.Errorf("sync restore parent after rollback: %w", err)
	}
	return nil
}

// finishRollbackRestoration 在旧 target 已恢复后先删除未提交 staging，再删除 Journal。
// 每次目录项变化后同步父目录，保证 Journal 消失时旧状态已完整恢复且 staging 已清理。
func finishRollbackRestoration(paths restorePaths, stagingExists bool) error {
	parent := filepath.Dir(paths.target)
	if stagingExists {
		if err := removeDirectoryTree(paths.staging); err != nil {
			return fmt.Errorf("remove rolled-back restore staging: %w", err)
		}
		if err := syncDirectory(parent); err != nil {
			return fmt.Errorf("sync restore parent after removing rolled-back staging: %w", err)
		}
	}
	if err := os.Remove(paths.journal); err != nil {
		return fmt.Errorf("remove rolled-back restore journal: %w", err)
	}
	return syncDirectory(parent)
}

// removeCompletedJournal 处理新 target 已提交且 rollback 已清理、只剩 Journal 的尾声。
func removeCompletedJournal(paths restorePaths) error {
	if err := os.Remove(paths.journal); err != nil {
		return fmt.Errorf("remove completed restore journal: %w", err)
	}
	return syncDirectory(filepath.Dir(paths.target))
}

// writeJournal 以同目录临时文件、fsync、rename、父目录 fsync 发布新 phase。
// 调用方只有在本函数成功后才能把对应目录操作视为可恢复承诺；临时文件失败时
// 会尽力清理，但绝不覆盖一个未持久化成功的 Journal 内容。
func writeJournal(paths restorePaths, journal restoreJournal, uid, gid int) error {
	return writeJournalWithOps(paths, journal, uid, gid, productionJournalWriteOps())
}

// journalWriteOps 只封装 Journal 发布的故障边界。生产路径使用真实系统调用；
// M7 Failpoint 测试传入局部副本，确定性证明旧 phase 与新 phase 的可恢复边界。
type journalWriteOps struct {
	write      func(*os.File, []byte) (int, error)
	syncFile   func(*os.File) error
	rename     func(string, string) error
	remove     func(string) error
	syncParent func(string) error
}

func productionJournalWriteOps() journalWriteOps {
	return journalWriteOps{
		write:      func(file *os.File, data []byte) (int, error) { return file.Write(data) },
		syncFile:   func(file *os.File) error { return file.Sync() },
		rename:     os.Rename,
		remove:     os.Remove,
		syncParent: syncDirectory,
	}
}

func writeJournalWithOps(paths restorePaths, journal restoreJournal, uid, gid int, ops journalWriteOps) error {
	data, err := json.Marshal(journal)
	if err != nil {
		return fmt.Errorf("marshal restore journal: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(paths.journal), filepath.Base(paths.journal)+".tmp-")
	if err != nil {
		return fmt.Errorf("create restore journal temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	writeErr := func() error {
		if _, err := ops.write(temporary, data); err != nil {
			return err
		}
		if err := temporary.Chmod(0o600); err != nil {
			return err
		}
		if err := temporary.Chown(uid, gid); err != nil {
			return err
		}
		return ops.syncFile(temporary)
	}()
	closeErr := temporary.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		cleanupErr := ops.remove(temporaryPath)
		return fmt.Errorf("write restore journal temporary file: %w", errors.Join(err, cleanupErr))
	}
	if err := ops.rename(temporaryPath, paths.journal); err != nil {
		cleanupErr := ops.remove(temporaryPath)
		return fmt.Errorf("publish restore journal: %w", errors.Join(err, cleanupErr))
	}
	if err := ops.syncParent(filepath.Dir(paths.target)); err != nil {
		return fmt.Errorf("sync restore journal parent: %w", err)
	}
	return nil
}

// cleanupRestoreJournalTemps 清理由 writeJournal 在原子 rename 前留下的同目录临时
// 文件。调用方持有 Stable Target External Lock；这里只接受固定前缀的 0600 普通
// 文件，并通过已打开父目录 FD 相对 unlink，既不跟随链接，也不重新解析父路径。
func cleanupRestoreJournalTemps(paths restorePaths) (removed bool, resultErr error) {
	parentPath := filepath.Dir(paths.journal)
	parent, err := os.Open(parentPath)
	if err != nil {
		return false, fmt.Errorf("open restore journal parent for temporary cleanup: %w", err)
	}
	defer func() {
		if err := parent.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close restore journal temporary cleanup parent: %w", err))
		}
	}()

	entries, err := parent.ReadDir(-1)
	if err != nil {
		return false, fmt.Errorf("list restore journal temporary files: %w", err)
	}
	prefix := filepath.Base(paths.journal) + ".tmp-"
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || len(name) == len(prefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return false, fmt.Errorf("inspect restore journal temporary file %q: %w", name, err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return false, fmt.Errorf("restore journal temporary entry %q must be a regular 0600 file", name)
		}
		if err := unix.Unlinkat(int(parent.Fd()), name, 0); err != nil {
			return false, fmt.Errorf("remove restore journal temporary file %q: %w", name, err)
		}
		removed = true
	}
	if removed {
		if err := parent.Sync(); err != nil {
			return true, fmt.Errorf("sync restore journal parent after temporary cleanup: %w", err)
		}
	}
	return removed, nil
}

// syncDirectory 持久化目录条目变化，并把 Sync 与 Close 错误一起返回。
func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}
