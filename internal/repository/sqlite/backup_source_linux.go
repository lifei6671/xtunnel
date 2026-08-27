//go:build linux

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	libsqlite "github.com/libtnb/sqlite"
	"golang.org/x/sys/unix"
)

// BackupSource 固定一次离线维护操作使用的 SQLite 主库 inode。路径只在
// OpenBackupSource 中通过 openat2 安全解析；后续 Schema 检查和 Online Backup
// 都经 /proc/self/fd 重新引用同一打开文件，不会因目录项替换而静默切到另一数据库。
//
// 固定主库 inode 还不等于固定 WAL 路径，因此每个操作在打开连接前后及完成后都
// 用 path 重新打开并与 info 比对。只要主库目录项在维护窗口内发生 rename/replace，
// 候选备份就会失败并清理，避免把旧主库与错误的 sidecar 组合成成功快照。
type BackupSource struct {
	// file 固定最初安全打开的主库 inode，并拥有其关闭责任。
	file *os.File
	// info 保存首次打开时的文件身份，供 SameFile 前后复核。
	info os.FileInfo
	// path 是清理后的原始权威路径，仅用于重新打开并核对目录项身份。
	path string
}

// OpenBackupSource 安全打开绝对 SQLite 路径，拒绝任意路径分量中的符号链接、
// magic link 和越出根目录的解析结果，并要求最终对象是普通文件。
func OpenBackupSource(databasePath string) (*BackupSource, error) {
	if !filepath.IsAbs(databasePath) {
		return nil, errors.New("SQLite backup source path must be absolute")
	}
	file, info, err := openBackupSourceFile(databasePath)
	if err != nil {
		return nil, err
	}
	return &BackupSource{file: file, info: info, path: filepath.Clean(databasePath)}, nil
}

// openBackupSourceFile 相对已打开的文件系统根使用 openat2 完成一次不可分割的安全
// 路径解析。调用方取得返回文件的 owner；失败路径在返回前关闭已创建的 FD。
func openBackupSourceFile(databasePath string) (*os.File, os.FileInfo, error) {
	rootFD, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open filesystem root for SQLite backup source: %w", err)
	}
	defer unix.Close(rootFD)
	// RESOLVE_BENEATH 配合 NO_SYMLINKS/NO_MAGICLINKS，把每个路径分量都约束在
	// rootFD 下；O_NOFOLLOW 再明确拒绝最终分量被替换成符号链接。
	fd, err := unix.Openat2(rootFD, strings.TrimPrefix(filepath.Clean(databasePath), "/"), &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("securely open SQLite backup source: %w", err)
	}
	fail := func(cause error) (*os.File, os.FileInfo, error) {
		return nil, nil, errors.Join(cause, unix.Close(fd))
	}
	file := os.NewFile(uintptr(fd), databasePath)
	if file == nil {
		return fail(errors.New("adopt SQLite backup source descriptor"))
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("inspect SQLite backup source: %w", err)
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, nil, errors.New("SQLite backup source is not a regular file")
	}
	return file, info, nil
}

// Close 释放固定源 inode。关闭后该 BackupSource 不能再次检查或备份；重复关闭安全。
func (source *BackupSource) Close() error {
	if source == nil || source.file == nil {
		return nil
	}
	err := source.file.Close()
	source.file = nil
	if err != nil {
		return fmt.Errorf("close SQLite backup source: %w", err)
	}
	return nil
}

// InspectSchemaVersion 通过固定 inode 的只读连接检查 Schema，绝不执行 Migration。
// 检查完成后再次核对原始路径，确保检查结论仍对应调用方指定的数据库身份。
func (source *BackupSource) InspectSchemaVersion(ctx context.Context) (version int, resultErr error) {
	pool, err := source.openPool(ctx)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err := pool.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close fixed SQLite schema source pool: %w", err))
		}
	}()
	version, resultErr = inspectSchemaVersion(ctx, pool)
	if resultErr != nil {
		return 0, resultErr
	}
	if err := source.verifyPathIdentity(); err != nil {
		return 0, err
	}
	return version, nil
}

// BackupSQLite 从固定源 inode 创建自包含 SQLite 副本。复制完成后再次核对原始
// 路径；身份变化时拒绝并删除主文件及 sidecar，使竞态候选永远不会交给归档层。
func (source *BackupSource) BackupSQLite(ctx context.Context, destinationPath string) (resultErr error) {
	pool, err := source.openPool(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err := pool.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close fixed SQLite backup source pool: %w", err))
		}
	}()
	if err := backupSQLiteFromPool(ctx, pool, destinationPath); err != nil {
		return err
	}
	if err := source.verifyPathIdentity(); err != nil {
		cleanupErr := error(nil)
		for _, path := range []string{destinationPath, destinationPath + "-journal", destinationPath + "-wal", destinationPath + "-shm"} {
			if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove rejected SQLite backup artifact: %w", removeErr))
			}
		}
		return errors.Join(err, cleanupErr)
	}
	return nil
}

// openPool 经 /proc/self/fd 为固定主库创建单连接只读池。打开前后同时检查固定 FD
// 本身和原始路径：前者防止 owner 内部身份异常，后者防止目录项与 WAL 关联在
// Ping/建连期间被替换。失败时本函数负责关闭尚未交给调用方的连接池。
func (source *BackupSource) openPool(ctx context.Context) (*sql.DB, error) {
	if source == nil || source.file == nil {
		return nil, errors.New("SQLite backup source is closed")
	}
	current, err := source.file.Stat()
	if err != nil || !os.SameFile(source.info, current) || !current.Mode().IsRegular() {
		return nil, errors.Join(errors.New("SQLite backup source identity changed"), err)
	}
	if err := source.verifyPathIdentity(); err != nil {
		return nil, err
	}
	fdPath := fmt.Sprintf("/proc/self/fd/%d", source.file.Fd())
	pool, err := sql.Open(libsqlite.DriverName, readOnlyDatabaseDSN(fdPath))
	if err != nil {
		return nil, fmt.Errorf("open fixed SQLite backup source: %w", err)
	}
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	if err := pool.PingContext(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("verify fixed SQLite backup source: %w", err), pool.Close())
	}
	current, err = source.file.Stat()
	if err != nil || !os.SameFile(source.info, current) || !current.Mode().IsRegular() {
		return nil, errors.Join(errors.New("SQLite backup source identity changed while opening"), err, pool.Close())
	}
	if err := source.verifyPathIdentity(); err != nil {
		return nil, errors.Join(err, pool.Close())
	}
	return pool, nil
}

// verifyPathIdentity 按与首次打开相同的 openat2 规则重新解析权威路径，并用
// os.SameFile 比对 inode 身份。候选 FD 始终在本函数退出前关闭，关闭错误不丢失。
func (source *BackupSource) verifyPathIdentity() (resultErr error) {
	candidate, info, err := openBackupSourceFile(source.path)
	if err != nil {
		return fmt.Errorf("SQLite backup source path changed: %w", err)
	}
	defer func() {
		if err := candidate.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close SQLite backup source identity check: %w", err))
		}
	}()
	if !os.SameFile(source.info, info) {
		return errors.New("SQLite backup source path now refers to a different file")
	}
	return nil
}
