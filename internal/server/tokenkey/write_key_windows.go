//go:build windows

package tokenkey

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lifei6671/xtunnel/internal/server/winsecurity"
)

// writeKeyAtomicallyPlatform publishes the Token master key with the Windows
// selected profile DACL. The publisher performs no-follow final verification
// and Write Through replacement; it never weakens the requirement to Unix mode
// bits or ordinary os.Rename semantics.
func writeKeyAtomicallyPlatform(directoryPath, keyPath string, key []byte) error {
	if filepath.Dir(keyPath) != directoryPath {
		return fmt.Errorf("tunnel token master key path must be directly beneath its credential directory")
	}
	// 读取和创建阶段之后，Data Directory 仍可能因外部变更失去受保护边界。
	// 在最终发布点再次验证其直接父目录，避免把新的主密钥写入一个已不可信的
	// 根目录；Publisher 继续负责 credentials/key 自身的 no-follow 提交验证。
	if err := winsecurity.ValidateForegroundDirectory(filepath.Dir(directoryPath)); err != nil {
		return fmt.Errorf("validate server data directory before publishing tunnel token master key: %w", err)
	}
	security, err := winsecurity.NewFileSecurityForPath(directoryPath)
	if err != nil {
		return fmt.Errorf("create tunnel token master key security policy: %w", err)
	}
	if err := winsecurity.PublishForegroundFile(directoryPath, filepath.Base(keyPath), key, security); err != nil {
		return fmt.Errorf("publish tunnel token master key: %w", err)
	}
	return nil
}

func loadExistingPlatform(directoryPath, keyPath string) (Key, bool, error) {
	if filepath.Dir(keyPath) != directoryPath {
		return Key{}, false, fmt.Errorf("tunnel token master key path must be directly beneath its credential directory")
	}
	// 受管 credentials/key 不能脱离受保护的 Data Directory 独立成立。先验证
	// 直接父目录，再检查子目录和正式文件，避免高权限替换根目录后仍继续使用
	// 表面受管的凭据对象；失败必须收敛为不可用，禁止回落到新建密钥。
	if err := winsecurity.ValidateForegroundDirectory(filepath.Dir(directoryPath)); err != nil {
		return Key{}, false, fmt.Errorf("%w: validate tunnel token data directory: %w", ErrUnavailable, err)
	}
	if _, err := os.Lstat(directoryPath); errors.Is(err, os.ErrNotExist) {
		return Key{}, false, nil
	} else if err != nil {
		return Key{}, false, fmt.Errorf("inspect tunnel token credential directory: %w", err)
	}
	content, err := winsecurity.ReadForegroundFile(directoryPath, filepath.Base(keyPath))
	if errors.Is(err, os.ErrNotExist) {
		return Key{}, false, nil
	}
	if err != nil {
		// Windows 无法证明受管目录或正式密钥仍满足对应 Profile DACL、no-follow
		// 与对象身份约束时，已有密文同样不能安全地继续使用。保留底层错误供
		// 诊断，同时统一为 ErrUnavailable，禁止调用方把它误当成可新建密钥。
		return Key{}, false, fmt.Errorf("%w: read tunnel token master key: %w", ErrUnavailable, err)
	}
	if len(content) != Size {
		clear(content)
		return Key{}, false, fmt.Errorf("%w: key file has invalid length", ErrUnavailable)
	}
	var key Key
	copy(key[:], content)
	clear(content)
	return key, true, nil
}

func createCredentialDirectoryPlatform(dataDir, directoryPath string) error {
	if filepath.Dir(directoryPath) != dataDir {
		return fmt.Errorf("tunnel token credential directory must be directly beneath the server data directory")
	}
	if err := winsecurity.ValidateForegroundDirectory(dataDir); err != nil {
		return fmt.Errorf("validate server data directory before creating token credentials: %w", err)
	}
	security, err := winsecurity.NewDirectorySecurityForPath(directoryPath)
	if err != nil {
		return fmt.Errorf("create tunnel token credential directory security policy: %w", err)
	}
	if err := winsecurity.CreateForegroundDirectory(directoryPath, security); err != nil {
		return fmt.Errorf("create tunnel token credential directory: %w", err)
	}
	return nil
}
