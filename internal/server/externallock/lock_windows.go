//go:build windows

package externallock

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"unsafe"

	"github.com/lifei6671/xtunnel/internal/server/winsecurity"
	"golang.org/x/sys/windows"
)

type fileIDInfo struct {
	volumeSerial uint64
	fileID       [16]byte
}

// acquire 使用由内核随进程退出释放的 byte-range lock。Runtime 祖先与锁文件
// Handle 均不共享删除，确保持锁期间路径和锁对象不能被 rename 或替换。
func acquire(runtimeDir, targetHash string) (func() error, error) {
	if len(targetHash) != 64 || strings.Trim(targetHash, "0123456789abcdef") != "" {
		return nil, errors.New("stable data target hash must be 64 lowercase hexadecimal characters")
	}
	directoryHandles, volume, err := openRuntimeDirectory(runtimeDir)
	if err != nil {
		return nil, err
	}
	closeDirectories := func() error {
		var closeErr error
		for _, directoryHandle := range slices.Backward(directoryHandles) {
			closeErr = errors.Join(closeErr, windows.CloseHandle(directoryHandle))
		}
		return closeErr
	}
	failDirectory := func(cause error) (func() error, error) {
		return nil, errors.Join(cause, closeDirectories())
	}

	// 普通启动也必须复核受管 Runtime，不能依赖此前 init 的权限快照。
	// 只校验已固定的最终目录；祖先路径继续由 no-follow Handle 链保护。
	directorySecurity, err := winsecurity.NewDirectorySecurityForPath(runtimeDir)
	if err != nil {
		return failDirectory(err)
	}
	if err := directorySecurity.ValidateDirectory(directoryHandles[len(directoryHandles)-1]); err != nil {
		return failDirectory(fmt.Errorf("validate server runtime security: %w", err))
	}
	fileSecurity, err := winsecurity.NewFileSecurityForPath(runtimeDir)
	if err != nil {
		return failDirectory(err)
	}

	lockPath := filepath.Join(runtimeDir, "server-lock-"+targetHash+".lock")
	pointer, err := windows.UTF16PtrFromString(lockPath)
	if err != nil {
		return failDirectory(fmt.Errorf("encode server external lock path: %w", err))
	}
	lockHandle, err := windows.CreateFile(
		pointer,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		fileSecurity.Attributes(),
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	runtime.KeepAlive(fileSecurity)
	if err != nil {
		return failDirectory(fmt.Errorf("open server external lock %q: %w", lockPath, err))
	}
	failLock := func(cause error) (func() error, error) {
		return nil, errors.Join(cause, windows.CloseHandle(lockHandle), closeDirectories())
	}
	// OPEN_ALWAYS 对既有文件不会应用创建权限；同一 Handle 验证失败时
	// 保留文件及其 ACL，关闭资源，不接管对象，也不进入 LockFileEx。
	if err := fileSecurity.ValidateFile(lockHandle); err != nil {
		return failLock(fmt.Errorf("validate server external lock security: %w", err))
	}

	lockVolume, err := volumeForHandle(lockHandle)
	if err != nil {
		return failLock(fmt.Errorf("read server external lock identity %q: %w", lockPath, err))
	}
	if lockVolume != volume {
		return failLock(fmt.Errorf("server external lock %q is on a different volume than its runtime directory", lockPath))
	}

	overlapped := new(windows.Overlapped)
	if err := windows.LockFileEx(
		lockHandle,
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		overlapped,
	); err != nil {
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return failLock(fmt.Errorf("%w: %s", ErrAlreadyLocked, lockPath))
		}
		return failLock(fmt.Errorf("lock server data target %q: %w", lockPath, err))
	}

	return func() error {
		unlockErr := windows.UnlockFileEx(lockHandle, 0, 1, 0, overlapped)
		lockCloseErr := windows.CloseHandle(lockHandle)
		return errors.Join(unlockErr, lockCloseErr, closeDirectories())
	}, nil
}

func openRuntimeDirectory(runtimeDir string) ([]windows.Handle, uint64, error) {
	if runtimeDir == "" || !filepath.IsAbs(runtimeDir) {
		return nil, 0, fmt.Errorf("runtime directory must be an absolute Windows path: %q", runtimeDir)
	}
	namespacePath := strings.ReplaceAll(runtimeDir, "/", `\`)
	if strings.HasPrefix(namespacePath, `\\`) || strings.HasPrefix(namespacePath, `\??\`) {
		return nil, 0, fmt.Errorf("runtime directory must not use UNC or a device namespace: %q", runtimeDir)
	}
	volumeName := filepath.VolumeName(runtimeDir)
	if len(volumeName) != 2 || volumeName[1] != ':' || strings.Contains(runtimeDir[len(volumeName):], ":") {
		return nil, 0, fmt.Errorf("runtime directory must be on a local drive and must not use ADS: %q", runtimeDir)
	}
	root := volumeName + `\`
	rootPointer, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return nil, 0, fmt.Errorf("encode runtime volume root: %w", err)
	}
	if windows.GetDriveType(rootPointer) != windows.DRIVE_FIXED {
		return nil, 0, fmt.Errorf("runtime directory volume %q is not a fixed local drive", volumeName)
	}

	relative := strings.TrimPrefix(filepath.Clean(runtimeDir), root)
	paths := []string{root}
	current := root
	if relative != "" && relative != "." {
		for component := range strings.SplitSeq(relative, `\`) {
			if component == "." || component == ".." {
				return nil, 0, fmt.Errorf("runtime directory must not contain dot path components: %q", runtimeDir)
			}
			current = filepath.Join(current, component)
			paths = append(paths, current)
		}
	}

	handles := make([]windows.Handle, 0, len(paths))
	closeAll := func() error {
		var closeErr error
		for _, handle := range slices.Backward(handles) {
			closeErr = errors.Join(closeErr, windows.CloseHandle(handle))
		}
		return closeErr
	}
	for index, path := range paths {
		pointer, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return nil, 0, errors.Join(fmt.Errorf("encode runtime directory component %q: %w", path, err), closeAll())
		}
		access := uint32(windows.FILE_READ_ATTRIBUTES)
		if index == len(paths)-1 {
			access |= windows.READ_CONTROL
		}
		handle, err := windows.CreateFile(
			pointer,
			access,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
			nil,
			windows.OPEN_EXISTING,
			windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
			0,
		)
		if err != nil {
			return nil, 0, errors.Join(fmt.Errorf("open runtime directory component %q: %w", path, err), closeAll())
		}
		handles = append(handles, handle)
		var information windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
			return nil, 0, errors.Join(fmt.Errorf("inspect runtime directory component %q: %w", path, err), closeAll())
		}
		if information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
			information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			return nil, 0, errors.Join(fmt.Errorf("runtime directory component %q must be a non-reparse directory", path), closeAll())
		}
	}
	volume, err := volumeForHandle(handles[len(handles)-1])
	if err != nil {
		return nil, 0, errors.Join(fmt.Errorf("read runtime directory identity: %w", err), closeAll())
	}
	return handles, volume, nil
}

func volumeForHandle(handle windows.Handle) (uint64, error) {
	var information fileIDInfo
	if err := windows.GetFileInformationByHandleEx(
		handle,
		windows.FileIdInfo,
		(*byte)(unsafe.Pointer(&information)),
		uint32(unsafe.Sizeof(information)),
	); err != nil {
		return 0, err
	}
	return validateFileIDInfo(information)
}

// validateFileIDInfo 只接受可用于 Stable Target 锁定边界的完整对象身份。
// Volume Serial 或 128-bit File ID 为零时，调用方无法把当前 Handle 与一个
// 稳定的本地卷对象关联，必须在取得锁前失败，不能只凭路径或卷号继续执行。
func validateFileIDInfo(information fileIDInfo) (uint64, error) {
	if information.volumeSerial == 0 {
		return 0, errors.New("Windows file identity has an invalid volume serial")
	}
	if information.fileID == [16]byte{} {
		return 0, errors.New("Windows file identity has an invalid file ID")
	}
	return information.volumeSerial, nil
}
