//go:build linux

package durableops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// removeDirectoryTree 通过目录 FD 逐层删除受管 sibling，并用 statx mount ID
// 拒绝 root 或任意后代挂载点。普通 RemoveAll 会穿过 nested bind mount，不能用于
// 删除承载旧 Server 数据的 rollback。
func removeDirectoryTree(path string) error {
	parentPath := filepath.Dir(path)
	parentFD, err := unix.Open(parentPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open removal parent: %w", err)
	}
	defer unix.Close(parentFD)
	rootFD, err := unix.Openat(parentFD, filepath.Base(path), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open removal root without following links: %w", err)
	}
	rootOpen := true
	defer func() {
		if rootOpen {
			_ = unix.Close(rootFD)
		}
	}()
	parentMount, err := mountID(parentFD)
	if err != nil {
		return fmt.Errorf("inspect removal parent mount: %w", err)
	}
	rootMount, err := mountID(rootFD)
	if err != nil {
		return fmt.Errorf("inspect removal root mount: %w", err)
	}
	if rootMount != parentMount {
		return errors.New("refuse to remove a restore directory that is a mount point")
	}
	if err := validateDirectoryTree(rootFD, rootMount); err != nil {
		return err
	}
	if err := removeDirectoryContents(rootFD, rootMount); err != nil {
		return err
	}
	if err := unix.Close(rootFD); err != nil {
		return fmt.Errorf("close emptied removal root: %w", err)
	}
	rootOpen = false
	if err := unix.Unlinkat(parentFD, filepath.Base(path), unix.AT_REMOVEDIR); err != nil {
		return fmt.Errorf("remove empty restore directory: %w", err)
	}
	return nil
}

// validateDirectoryTree 在删除任何字节前完成全树预检。
// 只允许同一 mount ID 上的普通文件和目录，因此后续阶段不会穿过 bind mount、
// 删除设备节点或跟随符号链接。directoryFD 的所有权仍归调用方。
func validateDirectoryTree(directoryFD int, rootMount uint64) error {
	entries, err := readDirectoryEntries(directoryFD)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		stat, err := statRemovalEntry(directoryFD, entry.Name())
		if err != nil {
			return err
		}
		if stat.Mask&unix.STATX_MNT_ID == 0 || stat.Mnt_id != rootMount {
			return fmt.Errorf("refuse to cross mount boundary at restore entry %q", entry.Name())
		}
		switch stat.Mode & unix.S_IFMT {
		case unix.S_IFREG:
		case unix.S_IFDIR:
			childFD, err := unix.Openat(directoryFD, entry.Name(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err != nil {
				return fmt.Errorf("open restore directory %q for validation: %w", entry.Name(), err)
			}
			childMount, mountErr := mountID(childFD)
			validateErr := error(nil)
			if mountErr == nil && childMount == rootMount {
				validateErr = validateDirectoryTree(childFD, rootMount)
			}
			closeErr := unix.Close(childFD)
			if mountErr != nil || childMount != rootMount {
				return errors.Join(fmt.Errorf("refuse to cross mount boundary at restore directory %q", entry.Name()), mountErr, closeErr)
			}
			if err := errors.Join(validateErr, closeErr); err != nil {
				return err
			}
		default:
			return fmt.Errorf("refuse to remove special restore entry %q", entry.Name())
		}
	}
	return nil
}

// removeDirectoryContents 在预检成功后仅用父目录 FD 相对删除后代。
// 删除阶段会重新检查条目类型，但不会再次比较 mount ID；rootMount 只沿递归传递，
// 当前不参与判断。因此本阶段依赖预检后目录树未被特权进程改挂载或替换这一前置条件。
func removeDirectoryContents(directoryFD int, rootMount uint64) error {
	entries, err := readDirectoryEntries(directoryFD)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		stat, err := statRemovalEntry(directoryFD, entry.Name())
		if err != nil {
			return err
		}
		switch stat.Mode & unix.S_IFMT {
		case unix.S_IFREG:
			if err := unix.Unlinkat(directoryFD, entry.Name(), 0); err != nil {
				return fmt.Errorf("remove restore file %q: %w", entry.Name(), err)
			}
		case unix.S_IFDIR:
			childFD, err := unix.Openat(directoryFD, entry.Name(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err != nil {
				return fmt.Errorf("open restore directory %q: %w", entry.Name(), err)
			}
			removeErr := removeDirectoryContents(childFD, rootMount)
			closeErr := unix.Close(childFD)
			if err := errors.Join(removeErr, closeErr); err != nil {
				return fmt.Errorf("empty restore directory %q: %w", entry.Name(), err)
			}
			if err := unix.Unlinkat(directoryFD, entry.Name(), unix.AT_REMOVEDIR); err != nil {
				return fmt.Errorf("remove restore directory %q: %w", entry.Name(), err)
			}
		default:
			return fmt.Errorf("restore removal tree changed after validation at %q", entry.Name())
		}
	}
	return nil
}

// readDirectoryEntries 为每次遍历创建独立的目录 file description。
// 返回前关闭临时 FD，调用方仍持有并继续使用原 directoryFD。
func readDirectoryEntries(directoryFD int) ([]os.DirEntry, error) {
	// dup 会共享目录流 offset，先验证再删除时第二次读取会直接落在 EOF。
	// 重新 openat(".") 获得独立 file description，两个阶段才能各自完整遍历。
	readFD, err := unix.Openat(directoryFD, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("duplicate restore directory descriptor: %w", err)
	}
	directory := os.NewFile(uintptr(readFD), "restore-directory")
	entries, readErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, fmt.Errorf("list restore directory: %w", err)
	}
	return entries, nil
}

// statRemovalEntry 相对固定父 FD 读取条目类型和 mount ID，且不触发 automount
// 或跟随符号链接。
func statRemovalEntry(directoryFD int, name string) (unix.Statx_t, error) {
	var stat unix.Statx_t
	if err := unix.Statx(
		directoryFD,
		name,
		unix.AT_SYMLINK_NOFOLLOW|unix.AT_NO_AUTOMOUNT,
		unix.STATX_TYPE|unix.STATX_MNT_ID,
		&stat,
	); err != nil {
		return unix.Statx_t{}, fmt.Errorf("inspect restore removal entry %q: %w", name, err)
	}
	return stat, nil
}

// mountID 读取已打开对象的内核 mount ID；内核不返回该字段时快速失败，
// 而不是退化为可能跨挂载删除的路径判断。
func mountID(fileDescriptor int) (uint64, error) {
	var stat unix.Statx_t
	if err := unix.Statx(fileDescriptor, "", unix.AT_EMPTY_PATH|unix.AT_NO_AUTOMOUNT, unix.STATX_MNT_ID, &stat); err != nil {
		return 0, err
	}
	if stat.Mask&unix.STATX_MNT_ID == 0 {
		return 0, errors.New("kernel did not return a mount ID")
	}
	return stat.Mnt_id, nil
}
