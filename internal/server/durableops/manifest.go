// Package durableops 负责 Server 备份归档与可恢复的数据目录切换。
package durableops

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/lifei6671/xtunnel/internal/server/datadir"
)

// 归档格式常量固定 Manifest 的版本与首条目名称。
const (
	// FormatVersion 是 V0.1 备份 Manifest 格式版本。
	FormatVersion = 1
	// manifestName 固定为归档首条目；恢复端依靠这个位置约束拒绝前置的
	// PAX 元数据或其他会改变后续解析语义的条目。
	manifestName = "manifest.json"
)

// 平台能力错误由所有非 Linux 持久化入口统一返回。
var (
	// ErrUnsupported 表示当前平台不支持 Server Backup、Restore 或崩溃恢复操作。
	ErrUnsupported = errors.New("server durable operations are unsupported on this platform")
)

// TLSMode 记录备份是否包含由 data-dir 持有的 Gateway Identity。
type TLSMode string

// TLSMode 的合法值决定 Gateway Identity 是否进入原子备份边界。
const (
	// TLSModePinned 表示备份必须携带并校验 data-dir 内的 Gateway 私钥和证书。
	TLSModePinned TLSMode = "pinned"
	// TLSModePublic 表示 Gateway 身份由公开信任链提供，归档不得携带本地身份文件。
	TLSModePublic TLSMode = "public"
)

// Manifest 描述备份归档内可恢复的完整文件集。
type Manifest struct {
	// FormatVersion 选择 Manifest/归档布局，恢复端只接受当前冻结版本。
	FormatVersion int `json:"format_version"`
	// SchemaVersion 记录捕获时 SQLite Schema，用于阻止新版本备份恢复到旧程序。
	SchemaVersion int `json:"schema_version"`
	// TLSMode 决定 Gateway Identity 是否属于本次原子恢复状态。
	TLSMode TLSMode `json:"tls_mode"`
	// Files 按 archiveFileRules 的规范顺序列出完整且唯一的恢复文件集。
	Files []ManifestFile `json:"files"`
}

// ManifestFile 记录单个归档文件的长度、权限与内容摘要。
type ManifestFile struct {
	// Path 是相对 data-dir 的规范 POSIX 路径，只能来自固定白名单。
	Path string `json:"path"`
	// Size 是归档条目和恢复后文件必须同时匹配的字节数。
	Size int64 `json:"size"`
	// Mode 是恢复后的精确 Unix 权限，而非仅用于创建时的建议值。
	Mode uint32 `json:"mode"`
	// SHA256 是小写十六进制内容摘要，用于连接 Manifest 与真实字节。
	SHA256 string `json:"sha256"`
}

// archiveFileRule 是备份协议的文件白名单和资源上限。
// 顺序同时决定 Manifest 与 USTAR 条目顺序，不能按文件系统遍历结果生成归档。
type archiveFileRule struct {
	// path 是相对 data-dir 的规范归档路径。
	path string
	// mode 是源、归档 Header 与恢复结果必须一致的权限。
	mode uint32
	// required 表示对应 TLS 模式下缺失该文件即不能形成有效备份。
	required bool
	// pinned 表示该文件只属于本地固定 Gateway Identity 模式。
	pinned bool
	// minSize/maxSize 在读取内容前建立资源边界并拒绝明显无效的状态。
	minSize int64
	maxSize int64
}

// archiveFileRules 冻结 V0.1 可恢复状态的唯一文件集合。
// 新增条目会改变持久化契约，必须同步格式版本、方案和兼容性决策。
var archiveFileRules = []archiveFileRule{
	{path: "xtunnel.db", mode: 0o600, required: true, minSize: 1, maxSize: 64 << 30},
	{path: "credentials/tunnel-token.key", mode: 0o600, required: true, minSize: 32, maxSize: 32},
	{path: "pki/agent-gateway.key", mode: 0o600, required: true, pinned: true, minSize: 1, maxSize: 64 << 10},
	{path: "pki/agent-gateway.crt", mode: 0o644, required: true, pinned: true, minSize: 1, maxSize: 1 << 20},
}

// validateManifest 在接触归档条目或恢复目录前验证完整机器契约。
// 白名单、规范顺序和精确权限共同阻止路径穿越、重复覆盖及宽权限恢复。
func validateManifest(manifest Manifest, currentSchemaVersion int) error {
	if manifest.FormatVersion != FormatVersion {
		return fmt.Errorf("backup format version is %d, want %d", manifest.FormatVersion, FormatVersion)
	}
	if currentSchemaVersion < 1 {
		return errors.New("current schema version must be positive")
	}
	if manifest.SchemaVersion < 1 || manifest.SchemaVersion > currentSchemaVersion {
		return fmt.Errorf("backup schema version %d is outside supported range 1..%d", manifest.SchemaVersion, currentSchemaVersion)
	}
	if manifest.TLSMode != TLSModePinned && manifest.TLSMode != TLSModePublic {
		return fmt.Errorf("backup TLS mode %q is invalid", manifest.TLSMode)
	}

	seen := make(map[string]struct{}, len(manifest.Files))
	lastRuleIndex := -1
	for _, file := range manifest.Files {
		rule, ruleIndex, ok := archiveRule(file.Path)
		if !ok || filepath.IsAbs(file.Path) || strings.Contains(file.Path, "\\") || file.Path != filepath.ToSlash(filepath.Clean(file.Path)) {
			return fmt.Errorf("backup file path %q is not allowed", file.Path)
		}
		if _, ok := seen[file.Path]; ok {
			return fmt.Errorf("backup file path %q is duplicated", file.Path)
		}
		seen[file.Path] = struct{}{}
		if ruleIndex <= lastRuleIndex {
			return fmt.Errorf("backup file %q is outside canonical archive order", file.Path)
		}
		lastRuleIndex = ruleIndex
		if rule.pinned && manifest.TLSMode != TLSModePinned {
			return fmt.Errorf("public TLS backup must not contain %q", file.Path)
		}
		if file.Size < rule.minSize || file.Size > rule.maxSize {
			return fmt.Errorf("backup file %q size %d is outside supported range %d..%d", file.Path, file.Size, rule.minSize, rule.maxSize)
		}
		if file.Mode != rule.mode {
			return fmt.Errorf("backup file %q mode is %04o, want %04o", file.Path, file.Mode, rule.mode)
		}
		decoded, err := hex.DecodeString(file.SHA256)
		if err != nil || len(decoded) != sha256.Size || file.SHA256 != strings.ToLower(file.SHA256) {
			return fmt.Errorf("backup file %q SHA-256 is invalid", file.Path)
		}
	}

	for _, rule := range archiveFileRules {
		required := rule.required && (!rule.pinned || manifest.TLSMode == TLSModePinned)
		if required {
			if _, ok := seen[rule.path]; !ok {
				return fmt.Errorf("backup is missing required file %q", rule.path)
			}
		}
	}
	return nil
}

// archiveRule 返回白名单规则及其规范序号；序号用于验证归档顺序。
func archiveRule(path string) (archiveFileRule, int, bool) {
	for index, rule := range archiveFileRules {
		if rule.path == path {
			return rule, index, true
		}
	}
	return archiveFileRule{}, 0, false
}

// restorePaths 固定一次稳定数据目录切换涉及的四个同级路径。
// staging 与 rollback 必须和 target 同盘，目录 rename 才能作为原子发布边界。
type restorePaths struct {
	// target 是 Server 启动时使用的稳定 data leaf。
	target string
	// staging 承载尚未发布但已逐步持久化的新状态。
	staging string
	// rollback 在切换期间保留旧 target，直到新 target 重验通过。
	rollback string
	// journal 记录解释三个目录组合所需的恢复状态机信息。
	journal string
}

// pathsForTarget 从已解析的 Stable Target 派生恢复路径，并再次校验父目录、
// leaf 与 hash 的绑定关系，防止调用方把 Journal 或清理操作导向其他目录。
func pathsForTarget(target datadir.Target) (restorePaths, error) {
	if target.Parent == "" || target.Leaf == "" || target.Path == "" || target.Hash == "" {
		return restorePaths{}, errors.New("stable data target is incomplete")
	}
	canonicalParent, err := filepath.EvalSymlinks(target.Parent)
	if err != nil {
		return restorePaths{}, fmt.Errorf("resolve stable data parent: %w", err)
	}
	canonicalParent, err = filepath.Abs(canonicalParent)
	if err != nil {
		return restorePaths{}, fmt.Errorf("make stable data parent absolute: %w", err)
	}
	if filepath.Clean(canonicalParent) != filepath.Clean(target.Parent) {
		return restorePaths{}, errors.New("stable data parent is not canonical")
	}
	parentInfo, err := os.Lstat(canonicalParent)
	if err != nil {
		return restorePaths{}, fmt.Errorf("inspect stable data parent: %w", err)
	}
	if parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
		return restorePaths{}, errors.New("stable data parent must be a non-symbolic-link directory")
	}
	if filepath.Base(target.Leaf) != target.Leaf || target.Leaf == "." || target.Leaf == ".." {
		return restorePaths{}, errors.New("stable data target leaf is invalid")
	}
	wantTarget := filepath.Join(canonicalParent, target.Leaf)
	if filepath.Clean(target.Path) != wantTarget {
		return restorePaths{}, errors.New("stable data target is not a direct child of its parent")
	}
	digest := sha256.Sum256([]byte(target.Path))
	wantHash := fmt.Sprintf("%x", digest)
	if target.Hash != wantHash {
		return restorePaths{}, errors.New("stable data target hash is invalid")
	}
	prefix := ".xtunnel-restore-" + target.Hash
	paths := restorePaths{
		target:   target.Path,
		staging:  filepath.Join(canonicalParent, prefix+".staging"),
		rollback: filepath.Join(canonicalParent, prefix+".rollback"),
		journal:  filepath.Join(canonicalParent, prefix+".journal"),
	}
	for _, path := range []string{paths.target, paths.staging, paths.rollback, paths.journal} {
		if filepath.Dir(path) != canonicalParent || slices.Contains([]string{"", ".", ".."}, filepath.Base(path)) {
			return restorePaths{}, errors.New("restore path is not a direct child of the stable data parent")
		}
	}
	return paths, nil
}
