//go:build windows

package datadir

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func validateTarget(target Target) error {
	directory, err := validateTargetParent(target, false)
	if err != nil {
		return err
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close stable server data parent: %w", err)
	}
	return nil
}

func validateCanonical(target Target) (resultErr error) {
	directory, err := validateTargetParent(target, false)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, directory.Close())
	}()
	return validateCanonicalWithParent(target, directory)
}

func pinParent(target Target) (*ParentGuard, error) {
	// 最终父目录 Handle 拒绝共享删除；Windows 同时会拒绝在其仍打开时改名
	// 任一祖先目录。Guard 由 Server Storage 持有到 SQLite 等持久化资源关闭
	// 后，避免校验返回后的路径替换绕过目标 Hash 锁。
	directory, err := validateTargetParent(target, true)
	if err != nil {
		return nil, err
	}
	return &ParentGuard{
		validateCanonical: func() error { return validateCanonicalWithParent(target, directory) },
		close:             directory.Close,
	}, nil
}

func validateCanonicalWithParent(target Target, directory *verifiedWindowsDirectory) (resultErr error) {
	pointer, err := windows.UTF16PtrFromString(target.Path)
	if err != nil {
		return fmt.Errorf("encode server data directory path: %w", err)
	}
	handle, err := windows.CreateFile(
		pointer,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		directoryOpenFlags,
		0,
	)
	if err != nil {
		return fmt.Errorf("open server data directory %q without following reparse points: %w", target.Path, err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, windows.CloseHandle(handle))
	}()

	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return fmt.Errorf("inspect server data directory %q: %w", target.Path, err)
	}
	if information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("server data directory %q must not be a reparse point", target.Path)
	}
	if information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return fmt.Errorf("server data path %q is not a directory", target.Path)
	}
	leafIdentity, err := fileIdentity(handle)
	if err != nil {
		return fmt.Errorf("read server data directory identity %q: %w", target.Path, err)
	}
	if leafIdentity.volume != target.parentVolume {
		return fmt.Errorf("server data directory %q is on a different volume than its stable parent", target.Path)
	}
	canonicalPath, err := finalPathName(handle)
	if err != nil {
		return fmt.Errorf("resolve canonical server data directory %q: %w", target.Path, err)
	}
	if !strings.EqualFold(filepath.Clean(canonicalPath), filepath.Clean(target.Path)) {
		return fmt.Errorf("server data directory resolved to %q, want stable target %q", canonicalPath, target.Path)
	}
	return nil
}

func validateTargetParent(target Target, pin bool) (*verifiedWindowsDirectory, error) {
	if target.Parent == "" || target.Leaf == "" || target.Path == "" || target.Hash == "" ||
		!target.parentBound {
		return nil, errors.New("stable data target is incomplete")
	}
	if err := validateWindowsLeaf(target.Leaf); err != nil {
		return nil, fmt.Errorf("stable data target leaf is invalid: %w", err)
	}
	if !strings.EqualFold(filepath.Clean(target.Path), filepath.Join(filepath.Clean(target.Parent), target.Leaf)) {
		return nil, errors.New("stable data target is not a direct child of its parent")
	}

	directory, err := openVerifiedDirectory(target.Parent, pin)
	if err != nil {
		return nil, fmt.Errorf("reopen stable data parent: %w", err)
	}
	fail := func(cause error) (*verifiedWindowsDirectory, error) {
		return nil, errors.Join(cause, directory.Close())
	}
	if !strings.EqualFold(filepath.Clean(directory.path), filepath.Clean(target.Parent)) {
		return fail(errors.New("stable data parent canonical path changed"))
	}
	if directory.identity.volume != target.parentVolume || directory.identity.file != target.parentFile {
		return fail(errors.New("stable data parent identity changed"))
	}
	if target.Hash != stableTargetHash(directory.identity, target.Leaf) {
		return fail(errors.New("stable data target hash is invalid"))
	}
	return directory, nil
}
