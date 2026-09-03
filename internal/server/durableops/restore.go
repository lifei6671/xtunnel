package durableops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/lifei6671/xtunnel/internal/server/datadir"
)

// restorePhase 是目录切换已持久化到哪一步的恢复状态机标记。
// phase 只描述最后一次已 fsync 的承诺；恢复时还必须与三个目录的真实状态交叉判断。
type restorePhase string

const (
	restoreJournalVersionV1 = 1
	restoreJournalVersionV2 = 2
	restoreJournalVersion   = restoreJournalVersionV2
)

// restorePhase 的合法值按目录发布顺序单向推进。
const (
	// phasePrepared 表示 staging 已完整验证且 Journal 已落盘，旧 target 尚未承诺移走。
	phasePrepared restorePhase = "prepared"
	// phaseRollbackReady 表示旧 target 已作为 rollback 持久化，可开始发布 staging。
	phaseRollbackReady restorePhase = "rollback_ready"
	// phaseRollbackRestoring 表示回滚意图已先于 rollback -> target 改名持久化。
	// V2 以它区分“旧 target 已恢复但 Journal 尚未清理”的 target-only 尾项。
	phaseRollbackRestoring restorePhase = "rollback_restoring"
	// phaseInstalled 表示新 target 已验证，剩余工作仅是删除 rollback 和 Journal。
	phaseInstalled restorePhase = "installed"
)

// restoreJournal 把恢复意图、目录身份、Manifest 和状态机阶段绑定在一起。
// 它是崩溃恢复的唯一决策依据；缺失或自相矛盾时不得猜测提交新数据。
type restoreJournal struct {
	// Version 选择 Journal 结构和恢复状态机语义。
	Version int `json:"version"`
	// ManifestSHA256 把 Journal phase 与本次归档声明绑定，避免内容串换。
	ManifestSHA256 string `json:"manifest_sha256"`
	// Manifest 让启动恢复无需依赖原始备份文件即可重验已安装 target。
	Manifest Manifest `json:"manifest"`
	// StableTarget、Staging、Rollback 必须与当前 target 派生路径精确相同。
	StableTarget string `json:"stable_target"`
	Staging      string `json:"staging"`
	Rollback     string `json:"rollback"`
	// Phase 是最后一次持久化的目录切换承诺。
	Phase restorePhase `json:"phase"`
}

// restoreResult 携带已成功安装的 Manifest 及其规范 JSON 摘要，供上层审计记录使用。
type restoreResult struct {
	// Manifest 是最终安装且重验通过的状态声明。
	Manifest Manifest
	// ManifestSHA256 是用于审计关联的规范 Manifest JSON 摘要。
	ManifestSHA256 string
}

// Restore 在调用方持有 Stable Target External Lock 期间安装备份。
// 该函数不获锁；它先收敛旧 Journal，再使用同盘 staging/rollback 切换目录。
func Restore(ctx context.Context, target datadir.Target, inputPath string, currentSchemaVersion int, expectedTLSMode TLSMode) (Manifest, error) {
	if ctx == nil {
		return Manifest{}, errors.New("restore context is nil")
	}
	paths, err := pathsForTarget(target)
	if err != nil {
		return Manifest{}, err
	}
	result, err := restorePlatform(ctx, paths, inputPath, currentSchemaVersion, expectedTLSMode)
	return result.Manifest, err
}

// RecoverPendingRestore 在打开正式数据目录前，根据 Journal phase 和
// target/staging/rollback 的真实存在状态完成或回滚上次切换。
// 调用方必须已持有与 Server 相同的 External Lock。
func RecoverPendingRestore(ctx context.Context, target datadir.Target) (bool, error) {
	if ctx == nil {
		return false, errors.New("restore recovery context is nil")
	}
	paths, err := pathsForTarget(target)
	if err != nil {
		return false, err
	}
	return recoverPlatform(ctx, paths)
}

// parseJournal 严格解析并验证 Journal 与当前 Stable Target 的绑定。
// Manifest 摘要防止阶段信息被拼接到另一份恢复内容上；未知字段和尾随值一律拒绝。
func parseJournal(data []byte, paths restorePaths) (restoreJournal, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var journal restoreJournal
	if err := decoder.Decode(&journal); err != nil {
		return restoreJournal{}, fmt.Errorf("parse restore journal: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return restoreJournal{}, fmt.Errorf("parse restore journal: %w", err)
	}
	if (journal.Version != restoreJournalVersionV1 && journal.Version != restoreJournalVersionV2) ||
		journal.StableTarget != paths.target || journal.Staging != paths.staging || journal.Rollback != paths.rollback {
		return restoreJournal{}, errors.New("restore journal paths or version do not match the stable target")
	}
	if !validSHA256(journal.ManifestSHA256) {
		return restoreJournal{}, errors.New("restore journal manifest SHA-256 is invalid")
	}
	if err := validateManifest(journal.Manifest, journal.Manifest.SchemaVersion); err != nil {
		return restoreJournal{}, fmt.Errorf("validate restore journal manifest: %w", err)
	}
	manifestData, err := json.Marshal(journal.Manifest)
	if err != nil {
		return restoreJournal{}, fmt.Errorf("marshal restore journal manifest: %w", err)
	}
	digest := sha256.Sum256(manifestData)
	if hex.EncodeToString(digest[:]) != journal.ManifestSHA256 {
		return restoreJournal{}, errors.New("restore journal manifest does not match its SHA-256")
	}
	if journal.Phase != phasePrepared && journal.Phase != phaseRollbackReady &&
		journal.Phase != phaseRollbackRestoring && journal.Phase != phaseInstalled {
		return restoreJournal{}, fmt.Errorf("restore journal phase %q is invalid", journal.Phase)
	}
	if journal.Phase == phaseRollbackRestoring && journal.Version != restoreJournalVersionV2 {
		return restoreJournal{}, errors.New("restore journal rollback_restoring phase requires version 2")
	}
	return journal, nil
}

// validSHA256 检查固定长度的小写十六进制编码，避免接受多种等价格式。
func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

// pathExists 只判断受管路径是否存在，并把符号链接视为安全错误而非普通存在状态。
func pathExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("restore path %q must not be a symbolic link", path)
	}
	return true, nil
}
