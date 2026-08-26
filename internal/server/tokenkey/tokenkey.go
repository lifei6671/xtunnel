// Package tokenkey 管理 Tunnel Connection Token 的服务端静态加密主密钥。
package tokenkey

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	// Size 是 AES-256 主密钥的固定字节数。
	Size = 32

	credentialDirectoryName = "credentials"
	masterKeyFilename       = "tunnel-token.key"
)

var (
	// ErrUnavailable 表示已有 Token 密文无法取得原始主密钥，或现有密钥材料不安全。
	// 调用方必须阻止 Server 继续启动，不能生成新密钥掩盖不可恢复的数据。
	ErrUnavailable = errors.New("tunnel token master key is unavailable")
)

// Key 是 Tunnel Token AES-256-GCM 保护器使用的固定长度主密钥。
// 它必须独立于 Gateway TLS Private Key，并与 SQLite 一起备份和恢复。
type Key [Size]byte

// LoadOrCreate 加载既有主密钥；仅在数据库尚无 Tunnel Token 时允许首次创建。
func LoadOrCreate(dataDir string, encryptedTokensExist bool) (Key, error) {
	return loadOrCreate(dataDir, encryptedTokensExist, rand.Reader)
}

func loadOrCreate(dataDir string, encryptedTokensExist bool, random io.Reader) (Key, error) {
	if !filepath.IsAbs(dataDir) {
		return Key{}, fmt.Errorf("tunnel token master key data directory must be absolute")
	}
	if random == nil {
		return Key{}, fmt.Errorf("generate tunnel token master key: random source is nil")
	}

	directoryPath := filepath.Join(dataDir, credentialDirectoryName)
	keyPath := filepath.Join(directoryPath, masterKeyFilename)
	key, found, err := loadExisting(directoryPath, keyPath)
	if err != nil {
		return Key{}, err
	}
	if found {
		return key, nil
	}
	if encryptedTokensExist {
		// 数据库密文只能由原始密钥解密。此时生成替代密钥会把数据损坏
		// 伪装成一次普通解密失败，因此必须保持磁盘不变并快速失败。
		return Key{}, fmt.Errorf("%w: key file is missing while encrypted tokens exist", ErrUnavailable)
	}

	if err := createCredentialDirectory(dataDir, directoryPath); err != nil {
		return Key{}, err
	}
	// 正常启动受 External Lock 串行化；再次检查仍可避免未来调用方在目录创建
	// 边界发生竞争时覆盖已经安全发布的主密钥。
	key, found, err = loadExisting(directoryPath, keyPath)
	if err != nil {
		return Key{}, err
	}
	if found {
		return key, nil
	}
	if _, err := io.ReadFull(random, key[:]); err != nil {
		return Key{}, fmt.Errorf("generate tunnel token master key: %w", err)
	}
	if err := writeKeyAtomically(directoryPath, keyPath, key[:]); err != nil {
		return Key{}, err
	}
	return key, nil
}

func loadExisting(directoryPath, keyPath string) (Key, bool, error) {
	directoryInfo, err := os.Lstat(directoryPath)
	if errors.Is(err, os.ErrNotExist) {
		return Key{}, false, nil
	}
	if err != nil {
		return Key{}, false, fmt.Errorf("inspect tunnel token credential directory: %w", err)
	}
	if directoryInfo.Mode()&os.ModeSymlink != 0 || !directoryInfo.IsDir() {
		return Key{}, false, fmt.Errorf("%w: credential path is not a regular directory", ErrUnavailable)
	}
	if !directoryPermissionsValid(directoryInfo.Mode()) {
		return Key{}, false, fmt.Errorf("%w: credential directory permissions are %04o, want 0700", ErrUnavailable, directoryInfo.Mode().Perm())
	}

	keyInfo, err := os.Lstat(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		return Key{}, false, nil
	}
	if err != nil {
		return Key{}, false, fmt.Errorf("inspect tunnel token master key: %w", err)
	}
	if keyInfo.Mode()&os.ModeSymlink != 0 || !keyInfo.Mode().IsRegular() {
		return Key{}, false, fmt.Errorf("%w: key path is not a regular file", ErrUnavailable)
	}
	if !keyPermissionsValid(keyInfo.Mode()) {
		return Key{}, false, fmt.Errorf("%w: key file permissions are %04o, want 0600", ErrUnavailable, keyInfo.Mode().Perm())
	}
	if keyInfo.Size() != Size {
		return Key{}, false, fmt.Errorf("%w: key file has invalid length", ErrUnavailable)
	}

	content, err := os.ReadFile(keyPath)
	if err != nil {
		return Key{}, false, fmt.Errorf("read tunnel token master key: %w", err)
	}
	if len(content) != Size {
		return Key{}, false, fmt.Errorf("%w: key file changed while reading", ErrUnavailable)
	}
	var key Key
	copy(key[:], content)
	clear(content)
	return key, true, nil
}

func createCredentialDirectory(dataDir, directoryPath string) error {
	if err := os.Mkdir(directoryPath, 0o700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create tunnel token credential directory: %w", err)
		}
		// Server 持有覆盖 Data Directory 的外部锁，正常情况下不会并发创建；
		// 即便路径已出现，也重新走完整类型和权限校验，绝不直接信任。
		_, _, inspectErr := loadExisting(directoryPath, filepath.Join(directoryPath, masterKeyFilename))
		if inspectErr != nil {
			return inspectErr
		}
		return nil
	}
	if err := os.Chmod(directoryPath, 0o700); err != nil {
		return fmt.Errorf("set tunnel token credential directory permissions: %w", err)
	}
	if err := syncDirectory(dataDir); err != nil {
		return fmt.Errorf("sync server data directory after creating token credentials: %w", err)
	}
	return nil
}

func writeKeyAtomically(directoryPath, keyPath string, key []byte) (resultErr error) {
	temporary, err := os.CreateTemp(directoryPath, masterKeyFilename+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary tunnel token master key: %w", err)
	}
	temporaryPath := temporary.Name()
	closed := false
	renamed := false
	defer func() {
		if !closed {
			if err := temporary.Close(); resultErr == nil && err != nil {
				resultErr = fmt.Errorf("close temporary tunnel token master key: %w", err)
			}
		}
		if !renamed {
			_ = os.Remove(temporaryPath)
		}
	}()

	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("set temporary tunnel token master key permissions: %w", err)
	}
	if _, err := temporary.Write(key); err != nil {
		return fmt.Errorf("write temporary tunnel token master key: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync temporary tunnel token master key: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary tunnel token master key before rename: %w", err)
	}
	closed = true
	// 外部锁保证只有一个 Server 可以进入此处，私有 0700 目录又阻止其他用户
	// 注入目标文件；同目录 rename 让崩溃时只会看到完整旧文件或完整新文件。
	if err := os.Rename(temporaryPath, keyPath); err != nil {
		return fmt.Errorf("publish tunnel token master key: %w", err)
	}
	renamed = true
	if err := syncDirectory(directoryPath); err != nil {
		return fmt.Errorf("sync tunnel token credential directory: %w", err)
	}
	return nil
}
