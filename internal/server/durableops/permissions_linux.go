//go:build linux

package durableops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// platformSupported 把依赖 openat2/statx 的持久化操作限制在 Linux。
func platformSupported() bool {
	return true
}

// openAbsoluteRegularNoSymlinks 从根目录 FD 重新解析绝对路径，禁止路径任一层
// 出现符号链接或 magic link；返回的 FD 由调用方拥有并关闭。
func openAbsoluteRegularNoSymlinks(path string) (*os.File, error) {
	rootFD, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(rootFD)
	fd, err := unix.Openat2(rootFD, strings.TrimPrefix(filepath.Clean(path), "/"), &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, errors.Join(errors.New("securely opened path is not a regular file"), err, unix.Close(fd))
	}
	return os.NewFile(uintptr(fd), path), nil
}

// createExclusiveOutput 先固定无符号链接的父目录 FD，再相对它以 O_EXCL 创建文件。
// 返回父目录 FD 是为了让失败清理与 fsync 始终作用于创建时的目录身份；调用方
// 必须关闭两个返回文件。
func createExclusiveOutput(path string) (*os.File, *os.File, error) {
	rootFD, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	defer unix.Close(rootFD)
	parentRelative := strings.TrimPrefix(filepath.Clean(filepath.Dir(path)), "/")
	parentFD, err := unix.Openat2(rootFD, parentRelative, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, nil, err
	}
	fd, err := unix.Openat2(parentFD, filepath.Base(path), &unix.OpenHow{
		Flags:   unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL | unix.O_CLOEXEC | unix.O_NOFOLLOW,
		Mode:    0o600,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		_ = unix.Close(parentFD)
		return nil, nil, err
	}
	output := os.NewFile(uintptr(fd), path)
	parent := os.NewFile(uintptr(parentFD), filepath.Dir(path))
	if output == nil || parent == nil {
		if output != nil {
			_ = output.Close()
		} else {
			_ = unix.Close(fd)
		}
		if parent != nil {
			_ = parent.Close()
		} else {
			_ = unix.Close(parentFD)
		}
		return nil, nil, errors.New("adopt backup output descriptors")
	}
	return output, parent, nil
}

// removeExclusiveOutput 只相对创建时固定的父目录 FD 删除半成品。
// 即使外部调用方在归档期间改名或替换输出父路径，也不会重新解析绝对路径并
// 误删替代目录中的同名文件。
func removeExclusiveOutput(parent *os.File, name string) error {
	if parent == nil || name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return errors.New("backup output cleanup target is invalid")
	}
	if err := unix.Unlinkat(int(parent.Fd()), name, 0); err != nil {
		return err
	}
	return nil
}

// openRegularBeneath 把 relative 限制在 root 的同一挂载内，并拒绝所有符号链接。
// 它用于捕获 data-dir 敏感文件，避免路径穿越和跨挂载读取。
func openRegularBeneath(root, relative string) (*os.File, error) {
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(rootFD)
	fd, err := unix.Openat2(rootFD, filepath.FromSlash(relative), &unix.OpenHow{
		Flags: unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS |
			unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV,
	})
	if err != nil {
		return nil, err
	}
	fail := func(cause error) (*os.File, error) {
		return nil, errors.Join(cause, unix.Close(fd))
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fail(err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fail(errors.New("securely opened path is not a regular file"))
	}
	file := os.NewFile(uintptr(fd), relative)
	if file == nil {
		return fail(errors.New("adopt securely opened file descriptor"))
	}
	return file, nil
}

// sourceModeValid 在 Linux 上要求源文件权限与持久化契约精确一致。
func sourceModeValid(mode os.FileMode, want uint32) bool {
	return uint32(mode.Perm()) == want
}

// openRegularNoFollow 禁止跟随最终路径分量，并在 FD 上确认普通文件类型。
// 需要限制中间路径分量时应使用 openAbsoluteRegularNoSymlinks 或 openRegularBeneath。
func openRegularNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	fail := func(cause error) (*os.File, error) {
		return nil, errors.Join(cause, unix.Close(fd))
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fail(fmt.Errorf("inspect securely opened file: %w", err))
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fail(errors.New("securely opened path is not a regular file"))
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		return fail(errors.New("adopt securely opened file descriptor"))
	}
	return file, nil
}
