//go:build windows

package tokenkey

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/lifei6671/xtunnel/internal/server/winsecurity"
)

// TestLoadOrCreateRejectsUnprotectedWindowsKey 确认主密钥的实际调用链不会
// 因为数据库尚无密文而接管低权限替换的正式文件。受管密钥只能由 Windows
// no-follow Publisher 创建或替换；拒绝后原始内容必须保留，避免静默换钥。
func TestLoadOrCreateRejectsUnprotectedWindowsKey(t *testing.T) {
	dataDir := absoluteTempDir(t)
	directory := filepath.Join(dataDir, credentialDirectoryName)
	directorySecurity, err := winsecurity.NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := winsecurity.CreateForegroundDirectory(directory, directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectory(credentials) error = %v", err)
	}
	keyPath := filepath.Join(directory, masterKeyFilename)
	want := bytes.Repeat([]byte{0x7a}, Size)
	if err := os.WriteFile(keyPath, want, 0o600); err != nil {
		t.Fatalf("os.WriteFile(unprotected key) error = %v", err)
	}
	before, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("os.Stat(unprotected key before rejection) error = %v", err)
	}

	_, err = loadOrCreate(dataDir, false, bytes.NewReader(bytes.Repeat([]byte{0x5a}, Size)))
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("loadOrCreate() error = %v, want ErrUnavailable", err)
	}
	after, readErr := os.ReadFile(keyPath)
	if readErr != nil {
		t.Fatalf("os.ReadFile(unprotected key after rejection) error = %v", readErr)
	}
	if !bytes.Equal(after, want) {
		t.Fatalf("unprotected key content = %x, want %x", after, want)
	}
	afterInfo, statErr := os.Stat(keyPath)
	if statErr != nil {
		t.Fatalf("os.Stat(unprotected key after rejection) error = %v", statErr)
	}
	if !os.SameFile(before, afterInfo) {
		t.Fatal("loadOrCreate() replaced the unprotected key")
	}
}

// TestLoadOrCreateRejectsUnmanagedCredentialDirectory 确认即使尚未生成正式
// 密钥，既有 credentials 目录只要不满足受管 Protected DACL 也不能被接管或
// 当成新的初始化位置。这样低权限目录无法诱导首次启动发布新的主密钥。
func TestLoadOrCreateRejectsUnmanagedCredentialDirectory(t *testing.T) {
	dataDir := absoluteTempDir(t)
	directory := filepath.Join(dataDir, credentialDirectoryName)
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatalf("os.Mkdir(credentials) error = %v", err)
	}
	before, err := os.Stat(directory)
	if err != nil {
		t.Fatalf("os.Stat(credentials before rejection) error = %v", err)
	}

	_, err = loadOrCreate(dataDir, false, bytes.NewReader(bytes.Repeat([]byte{0x5a}, Size)))
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("loadOrCreate() error = %v, want ErrUnavailable", err)
	}
	if _, statErr := os.Stat(filepath.Join(directory, masterKeyFilename)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unmanaged credentials directory published a key: os.Stat() error = %v", statErr)
	}
	after, err := os.Stat(directory)
	if err != nil {
		t.Fatalf("os.Stat(credentials after rejection) error = %v", err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("loadOrCreate() replaced the unmanaged credentials directory")
	}
}

// TestLoadOrCreateRejectsCredentialsPathReplacedByRegularFile 确认 credentials
// 在完成受管创建后若被替换为普通文件，读取阶段不会把该路径当作缺失目录而重建。
// 这条路径必须 fail-closed，并保持替换对象不变，避免路径类型降级诱导新主密钥发布。
func TestLoadOrCreateRejectsCredentialsPathReplacedByRegularFile(t *testing.T) {
	dataDir := absoluteTempDir(t)
	directory := filepath.Join(dataDir, credentialDirectoryName)
	directorySecurity, err := winsecurity.NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := winsecurity.CreateForegroundDirectory(directory, directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectory(credentials) error = %v", err)
	}
	if err := os.Remove(directory); err != nil {
		t.Fatalf("os.Remove(protected credentials directory) error = %v", err)
	}
	sentinel := []byte("unmanaged credentials path")
	if err := os.WriteFile(directory, sentinel, 0o600); err != nil {
		t.Fatalf("os.WriteFile(credentials replacement) error = %v", err)
	}
	before, err := os.Stat(directory)
	if err != nil {
		t.Fatalf("os.Stat(credentials replacement before rejection) error = %v", err)
	}

	_, err = loadOrCreate(dataDir, false, bytes.NewReader(bytes.Repeat([]byte{0x5a}, Size)))
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("loadOrCreate() error = %v, want ErrUnavailable", err)
	}
	after, err := os.ReadFile(directory)
	if err != nil {
		t.Fatalf("os.ReadFile(credentials replacement after rejection) error = %v", err)
	}
	if !bytes.Equal(after, sentinel) {
		t.Fatal("loadOrCreate() changed the credentials replacement bytes")
	}
	afterInfo, err := os.Stat(directory)
	if err != nil {
		t.Fatalf("os.Stat(credentials replacement after rejection) error = %v", err)
	}
	if !os.SameFile(before, afterInfo) {
		t.Fatal("loadOrCreate() replaced the credentials path")
	}
}

// TestLoadOrCreateRejectsUnprotectedDataDirectoryWithManagedCredentials 确认
// credentials 与主密钥即使各自仍满足受管 DACL，也不能脱离受保护的 data root
// 独立继续使用。根目录失去边界时，读取必须 fail-closed 且不得接管任何对象。
func TestLoadOrCreateRejectsUnprotectedDataDirectoryWithManagedCredentials(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "unmanaged-data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatalf("os.Mkdir(unprotected data directory) error = %v", err)
	}
	directory := filepath.Join(dataDir, credentialDirectoryName)
	directorySecurity, err := winsecurity.NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := winsecurity.CreateForegroundDirectory(directory, directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectory(credentials) error = %v", err)
	}
	fileSecurity, err := winsecurity.NewForegroundFileSecurity()
	if err != nil {
		t.Fatalf("NewForegroundFileSecurity() error = %v", err)
	}
	keyPath := filepath.Join(directory, masterKeyFilename)
	want := bytes.Repeat([]byte{0x7a}, Size)
	if err := winsecurity.PublishForegroundFile(directory, masterKeyFilename, want, fileSecurity); err != nil {
		t.Fatalf("PublishForegroundFile(key) error = %v", err)
	}
	dataInfoBefore, err := os.Stat(dataDir)
	if err != nil {
		t.Fatalf("os.Stat(unprotected data directory before rejection) error = %v", err)
	}
	directoryInfoBefore, err := os.Stat(directory)
	if err != nil {
		t.Fatalf("os.Stat(credentials before rejection) error = %v", err)
	}
	keyInfoBefore, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("os.Stat(key before rejection) error = %v", err)
	}

	_, err = loadOrCreate(dataDir, false, errReader{})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("loadOrCreate() error = %v, want ErrUnavailable", err)
	}
	after, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("os.ReadFile(key after rejection) error = %v", err)
	}
	if !bytes.Equal(after, want) {
		t.Fatal("loadOrCreate() changed the managed key beneath the unprotected data directory")
	}
	dataInfoAfter, err := os.Stat(dataDir)
	if err != nil {
		t.Fatalf("os.Stat(unprotected data directory after rejection) error = %v", err)
	}
	directoryInfoAfter, err := os.Stat(directory)
	if err != nil {
		t.Fatalf("os.Stat(credentials after rejection) error = %v", err)
	}
	keyInfoAfter, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("os.Stat(key after rejection) error = %v", err)
	}
	if !os.SameFile(dataInfoBefore, dataInfoAfter) || !os.SameFile(directoryInfoBefore, directoryInfoAfter) || !os.SameFile(keyInfoBefore, keyInfoAfter) {
		t.Fatal("loadOrCreate() replaced an object beneath the unprotected data directory")
	}
}

// TestWriteKeyAtomicallyPlatformRejectsUnprotectedDataDirectory 确认最终发布点
// 也会复验 Data Directory。即使 credentials 已受管，失去受保护根目录后也不得
// 写入新的主密钥，避免创建阶段到发布阶段之间的根目录替换越过安全边界。
func TestWriteKeyAtomicallyPlatformRejectsUnprotectedDataDirectory(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "unmanaged-data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatalf("os.Mkdir(unprotected data directory) error = %v", err)
	}
	directory := filepath.Join(dataDir, credentialDirectoryName)
	directorySecurity, err := winsecurity.NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := winsecurity.CreateForegroundDirectory(directory, directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectory(credentials) error = %v", err)
	}
	keyPath := filepath.Join(directory, masterKeyFilename)

	err = writeKeyAtomicallyPlatform(directory, keyPath, bytes.Repeat([]byte{0x5a}, Size))
	if err == nil {
		t.Fatal("writeKeyAtomicallyPlatform() accepted an unprotected data directory")
	}
	if _, statErr := os.Stat(keyPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("writeKeyAtomicallyPlatform() published a key: os.Stat() error = %v", statErr)
	}
}
