package durableops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/lifei6671/xtunnel/internal/application"
	"github.com/lifei6671/xtunnel/internal/repository"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	"github.com/lifei6671/xtunnel/internal/server/gateway"
	"github.com/lifei6671/xtunnel/internal/server/tokenkey"
)

// validateRestoredState 重验恢复目录的完整白名单、字节摘要与可启动语义。
// Journal 恢复只能在本函数成功后删除旧 rollback，避免把“目录名已切换”误当成
// “新状态完整可用”。
func validateRestoredState(ctx context.Context, root string, manifest Manifest) error {
	if err := validateManifest(manifest, sqlite.CurrentSchemaVersion()); err != nil {
		return fmt.Errorf("validate restored manifest: %w", err)
	}
	// 第一层先把磁盘树与 Manifest 做封闭世界比对：除白名单文件及其必要父目录外，
	// 任何额外对象都会改变启动语义或扩大后续清理范围，因此不能只校验已知文件。
	expectedFiles := make(map[string]ManifestFile, len(manifest.Files))
	expectedDirectories := map[string]struct{}{".": {}}
	for _, file := range manifest.Files {
		expectedFiles[file.Path] = file
		for directory := filepath.ToSlash(filepath.Dir(file.Path)); directory != "."; directory = filepath.ToSlash(filepath.Dir(directory)) {
			expectedDirectories[directory] = struct{}{}
		}
	}
	seen := make(map[string]struct{}, len(expectedFiles))
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("restored path %q must not be a symbolic link", relative)
		}
		if entry.IsDir() {
			if _, ok := expectedDirectories[relative]; !ok {
				return fmt.Errorf("restored directory %q is not declared by the manifest", relative)
			}
			if info.Mode().Perm() != 0o700 {
				return fmt.Errorf("restored directory %q mode is %04o, want 0700", relative, info.Mode().Perm())
			}
			return nil
		}
		expected, ok := expectedFiles[relative]
		if !ok {
			return fmt.Errorf("restored file %q is not declared by the manifest", relative)
		}
		if !info.Mode().IsRegular() || uint32(info.Mode().Perm()) != expected.Mode || info.Size() != expected.Size {
			return fmt.Errorf("restored file %q metadata does not match the manifest", relative)
		}
		digest, size, err := digestFile(path, info)
		if err != nil {
			return fmt.Errorf("digest restored file %q: %w", relative, err)
		}
		if digest != expected.SHA256 || size != expected.Size {
			return fmt.Errorf("restored file %q content does not match the manifest", relative)
		}
		seen[relative] = struct{}{}
		return nil
	})
	if err != nil {
		return fmt.Errorf("validate restored file set: %w", err)
	}
	if len(seen) != len(expectedFiles) {
		return errors.New("restored file set is incomplete")
	}
	// 第二层验证跨文件业务关系。字节摘要正确并不代表状态可启动：数据库必须是
	// 当前程序支持的完整 Schema，主密钥必须能解开每条 Token 并匹配行元数据，
	// pinned 模式下证书还必须和私钥组成同一身份。
	if err := sqlite.ValidateBackupDatabase(ctx, filepath.Join(root, "xtunnel.db"), manifest.SchemaVersion); err != nil {
		return fmt.Errorf("validate restored SQLite database: %w", err)
	}
	masterKey, err := tokenkey.LoadOrCreate(root, true)
	if err != nil {
		return fmt.Errorf("validate restored Tunnel Token master key: %w", err)
	}
	defer clear(masterKey[:])
	protector, err := application.NewAES256GCMTokenProtector(masterKey[:])
	if err != nil {
		return fmt.Errorf("initialize restored Tunnel Token protector: %w", err)
	}
	if manifest.SchemaVersion >= sqlite.TunnelTokensSchemaVersion {
		if err := sqlite.ValidateBackupTunnelTokens(
			ctx,
			filepath.Join(root, "xtunnel.db"),
			func(metadata repository.TunnelToken) error {
				return application.ValidateProtectedConnectionToken(metadata, protector)
			},
		); err != nil {
			return fmt.Errorf("validate restored Tunnel Token ciphertexts: %w", err)
		}
	}
	if manifest.TLSMode == TLSModePinned {
		if _, err := gateway.LoadPinnedIdentity(root); err != nil {
			return fmt.Errorf("validate restored pinned Gateway identity: %w", err)
		}
	}
	return nil
}
