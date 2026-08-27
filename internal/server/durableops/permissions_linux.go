//go:build linux

package durableops

import (
	"crypto/rand"
	"encoding/hex"
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

// createPendingOutput 先固定无符号链接的父目录 FD，再在其中创建随机、隐藏且
// O_EXCL 的 0600 候选。最终文件名此时尚不存在；返回对象的 FD、候选名和最终名
// 共同绑定同一个目录身份，调用方必须关闭 file 与 parent。
func createPendingOutput(path string) (*pendingOutput, error) {
	rootFD, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(rootFD)
	cleaned := filepath.Clean(path)
	finalName := filepath.Base(cleaned)
	if finalName == "." || finalName == ".." || finalName == string(filepath.Separator) {
		return nil, errors.New("backup output name is invalid")
	}
	parentRelative := strings.TrimPrefix(filepath.Dir(cleaned), "/")
	if parentRelative == "" {
		parentRelative = "."
	}
	parentFD, err := unix.Openat2(rootFD, parentRelative, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, err
	}

	var candidateName string
	fd := -1
	for attempt := 0; attempt < 128; attempt++ {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			_ = unix.Close(parentFD)
			return nil, fmt.Errorf("generate pending backup output name: %w", err)
		}
		candidateName = pendingOutputPrefix + hex.EncodeToString(nonce[:])
		fd, err = unix.Openat2(parentFD, candidateName, &unix.OpenHow{
			Flags:   unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL | unix.O_CLOEXEC | unix.O_NOFOLLOW,
			Mode:    0o600,
			Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
		})
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			_ = unix.Close(parentFD)
			return nil, err
		}
		break
	}
	if fd < 0 {
		_ = unix.Close(parentFD)
		return nil, errors.New("allocate unique pending backup output name")
	}
	output := os.NewFile(uintptr(fd), candidateName)
	parent := os.NewFile(uintptr(parentFD), filepath.Dir(cleaned))
	if output == nil || parent == nil {
		cleanupErr := unix.Unlinkat(parentFD, candidateName, 0)
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
		return nil, errors.Join(errors.New("adopt pending backup output descriptors"), cleanupErr)
	}
	return &pendingOutput{file: output, parent: parent, name: candidateName, finalName: finalName}, nil
}

// publishPendingOutput 使用同一父目录 FD 的 renameat2(RENAME_NOREPLACE) 发布候选。
// 最终名已存在时由内核原子拒绝，禁止并发 Create 或外部写入被覆盖。
func publishPendingOutput(output *pendingOutput) error {
	if output == nil || output.parent == nil || !validOutputName(output.name) || !validOutputName(output.finalName) {
		return errors.New("pending backup output is invalid")
	}
	return unix.Renameat2(
		int(output.parent.Fd()), output.name,
		int(output.parent.Fd()), output.finalName,
		unix.RENAME_NOREPLACE,
	)
}

// removePendingOutput 只相对创建时固定的父目录 FD 删除半成品。
// 即使外部调用方在归档期间改名或替换输出父路径，也不会重新解析绝对路径并
// 误删替代目录中的同名文件。
func removePendingOutput(parent *os.File, name string) error {
	if parent == nil || !validOutputName(name) {
		return errors.New("backup output cleanup target is invalid")
	}
	if err := unix.Unlinkat(int(parent.Fd()), name, 0); err != nil {
		return err
	}
	return nil
}

// validOutputName 只接受单个普通路径分量，避免 FD-relative rename/unlink 被用于
// 解析子目录或特殊目录项。
func validOutputName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name
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
