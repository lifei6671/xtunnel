//go:build windows

package provision

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/lifei6671/xtunnel/internal/server/datadir"
	"github.com/lifei6671/xtunnel/internal/server/externallock"
	"github.com/lifei6671/xtunnel/internal/server/pathprofile"
	"github.com/lifei6671/xtunnel/internal/server/winsecurity"
)

func initialize(dataDir string) error {
	profile, err := pathprofile.Resolve(dataDir)
	if err != nil {
		return err
	}
	return initializeProfile(profile)
}

// initializeWithRuntimeDirectory 保持目录创建在显式 init 边界内。它只在操作者
// 显式执行 init 时创建 Data 父链和目标 leaf；最后通过现有 no-follow 校验入口
// 复核，绝不让正常启动隐式补建目录。
func initializeWithRuntimeDirectory(dataDir, runtimeDir string) (resultErr error) {
	serverRoot := filepath.Dir(dataDir)
	if !strings.EqualFold(filepath.Clean(serverRoot), filepath.Clean(filepath.Dir(runtimeDir))) {
		return errors.New("server data and runtime directories must share a profile root")
	}
	return initializeProfile(pathprofile.Profile{
		DataDir:     dataDir,
		RuntimeDir:  runtimeDir,
		ManagedRoot: filepath.Dir(serverRoot),
	})
}

func initializeProfile(profile pathprofile.Profile) (resultErr error) {
	dataDir := profile.DataDir
	runtimeDir := profile.RuntimeDir
	if err := datadir.ValidateWindowsDataPath(dataDir); err != nil {
		return fmt.Errorf("validate server data directory path: %w", err)
	}
	if profile.ManagedRoot == "" {
		return errors.New("server path profile has no managed root")
	}
	security, err := winsecurity.NewForegroundDirectorySecurity()
	if err != nil {
		return fmt.Errorf("create foreground directory security policy: %w", err)
	}
	if err := createDirectoryTree(dataDir, profile.ManagedRoot, security); err != nil {
		return fmt.Errorf("create server data directory: %w", err)
	}
	target, err := datadir.Resolve(dataDir)
	if err != nil {
		return fmt.Errorf("resolve stable server data target: %w", err)
	}
	if err := datadir.ValidateCanonical(target); err != nil {
		return fmt.Errorf("validate server data directory: %w", err)
	}
	if err := createDirectoryTree(runtimeDir, profile.ManagedRoot, security); err != nil {
		return fmt.Errorf("create server runtime directory: %w", err)
	}

	// Acquire 会逐级以 OPEN_REPARSE_POINT 打开 Runtime，并校验所有对象都是普通
	// 目录。短暂获取/释放同一把锁可复用这条安全路径，同时不触碰 SQLite、密钥或
	// Listener；遗留的空锁文件是设计允许的可复用对象。
	lock, err := externallock.Acquire(runtimeDir, target.Hash)
	if err != nil {
		return fmt.Errorf("validate server runtime directory: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, lock.Close())
	}()
	return nil
}

func createDirectoryTree(
	path string,
	managedRoot string,
	security *winsecurity.ForegroundDirectorySecurity,
) (resultErr error) {
	volume := filepath.VolumeName(path)
	root := volume + `\`
	if len(volume) != 2 || volume[1] != ':' || !filepath.IsAbs(path) ||
		strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, `\??\`) ||
		strings.Contains(path[len(volume):], ":") {
		return fmt.Errorf("runtime directory must be an absolute Windows path: %q", path)
	}
	rootPointer, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return fmt.Errorf("encode runtime volume root: %w", err)
	}
	if windows.GetDriveType(rootPointer) != windows.DRIVE_FIXED {
		return fmt.Errorf("runtime directory volume %q is not a fixed local drive", volume)
	}
	relative := strings.TrimPrefix(filepath.Clean(path), root)
	paths := []string{root}
	current := root
	if relative != "" && relative != "." {
		for component := range strings.SplitSeq(relative, `\`) {
			if component == "" || component == "." || component == ".." {
				return fmt.Errorf("runtime directory contains an unsafe path component: %q", path)
			}
			current = filepath.Join(current, component)
			paths = append(paths, current)
		}
	}
	handles := make([]windows.Handle, 0, len(paths))
	defer func() {
		for _, handle := range slices.Backward(handles) {
			resultErr = errors.Join(resultErr, windows.CloseHandle(handle))
		}
	}()

	for index, componentPath := range paths {
		managed := isManagedDirectory(componentPath, managedRoot)
		if index != 0 {
			pointer, err := windows.UTF16PtrFromString(componentPath)
			if err != nil {
				return fmt.Errorf("encode directory component %q: %w", componentPath, err)
			}
			attributes := (*windows.SecurityAttributes)(nil)
			if managed {
				attributes = security.Attributes()
			}
			err = windows.CreateDirectory(pointer, attributes)
			if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
				return fmt.Errorf("create directory component %q: %w", componentPath, err)
			}
		}
		pointer, err := windows.UTF16PtrFromString(componentPath)
		if err != nil {
			return fmt.Errorf("encode directory component %q: %w", componentPath, err)
		}
		handle, err := windows.CreateFile(
			pointer,
			windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
			nil,
			windows.OPEN_EXISTING,
			windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
			0,
		)
		if err != nil {
			return fmt.Errorf("open directory component %q without following reparse points: %w", componentPath, err)
		}
		handles = append(handles, handle)
		var information windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
			return fmt.Errorf("inspect directory component %q: %w", componentPath, err)
		}
		if information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
			information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			return fmt.Errorf("directory component %q must be a non-reparse directory", componentPath)
		}
		if managed {
			if err := security.ValidateDirectory(handle); err != nil {
				return fmt.Errorf("validate protected directory component %q: %w", componentPath, err)
			}
		}
	}
	return nil
}

func isManagedDirectory(path, managedRoot string) bool {
	cleanPath := filepath.Clean(path)
	cleanRoot := filepath.Clean(managedRoot)
	return strings.EqualFold(cleanPath, cleanRoot) || strings.HasPrefix(
		strings.ToLower(cleanPath),
		strings.ToLower(cleanRoot+`\`),
	)
}
