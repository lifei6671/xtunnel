//go:build windows

package datadir

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
)

const directoryOpenFlags = windows.FILE_FLAG_BACKUP_SEMANTICS | windows.FILE_FLAG_OPEN_REPARSE_POINT

type windowsFileIdentity struct {
	volume uint64
	file   [16]byte
}

// windowsFileIDInfo 对应 FILE_ID_INFO。Windows Server 可能使用 ReFS，
// BY_HANDLE_FILE_INFORMATION 的 64-bit FileIndex 在该文件系统上不保证唯一，
// 因此身份与 Hash 必须消费完整 128-bit File ID。
type windowsFileIDInfo struct {
	volumeSerial uint64
	fileID       [16]byte
}

// verifiedWindowsDirectory 持有从卷根到目标目录的全部 Handle，使组件检查、
// canonical path 与最终身份读取基于同一次遍历；调用方必须在完成后整体 Close。
type verifiedWindowsDirectory struct {
	handles  []windows.Handle
	path     string
	identity windowsFileIdentity
}

func (directory *verifiedWindowsDirectory) Close() error {
	var closeErr error
	for index := len(directory.handles) - 1; index >= 0; index-- {
		closeErr = errors.Join(closeErr, windows.CloseHandle(directory.handles[index]))
	}
	directory.handles = nil
	return closeErr
}

// resolve 在 leaf 不存在时只解析其父目录对象。结果随后必须在取得 External
// Lock 后由 PinParent 重新打开并核对同一身份；本阶段全部 Handle 在最终父目录
// canonical path 与身份读取完成后释放。
func resolve(dataDir string) (Target, error) {
	cleanPath, leaf, parent, err := validateWindowsDataPath(dataDir)
	if err != nil {
		return Target{}, err
	}

	directory, err := openVerifiedDirectory(parent, false)
	if err != nil {
		return Target{}, fmt.Errorf("open stable server data parent %q: %w", parent, err)
	}
	if err := directory.Close(); err != nil {
		return Target{}, fmt.Errorf("close stable server data parent %q: %w", parent, err)
	}

	stablePath := filepath.Join(directory.path, leaf)
	if !strings.EqualFold(filepath.Clean(cleanPath), filepath.Clean(stablePath)) {
		return Target{}, fmt.Errorf("server data directory resolved to %q, want input %q", stablePath, dataDir)
	}
	hash := stableTargetHash(directory.identity, leaf)
	return Target{
		Path:         stablePath,
		Parent:       directory.path,
		Leaf:         leaf,
		Hash:         hash,
		parentVolume: directory.identity.volume,
		parentFile:   directory.identity.file,
		parentBound:  true,
	}, nil
}

func validateWindowsDataPath(dataDir string) (cleanPath, leaf, parent string, err error) {
	if dataDir == "" || !utf8.ValidString(dataDir) {
		return "", "", "", errors.New("server data directory must be valid UTF-8")
	}
	namespacePath := strings.ReplaceAll(dataDir, "/", `\`)
	lowerPath := strings.ToLower(namespacePath)
	if strings.HasPrefix(lowerPath, `\\`) || strings.HasPrefix(lowerPath, `\??\`) {
		return "", "", "", fmt.Errorf("server data directory must not use UNC or a device namespace: %q", dataDir)
	}
	volume := filepath.VolumeName(dataDir)
	if len(volume) != 2 || volume[1] != ':' || !filepath.IsAbs(dataDir) {
		return "", "", "", fmt.Errorf("server data directory must be an absolute local drive path: %q", dataDir)
	}
	if strings.Contains(dataDir[len(volume):], ":") {
		return "", "", "", fmt.Errorf("server data directory must not use an alternate data stream: %q", dataDir)
	}
	for _, component := range strings.FieldsFunc(dataDir[len(volume):], func(r rune) bool { return r == '\\' || r == '/' }) {
		if component == "." || component == ".." {
			return "", "", "", fmt.Errorf("server data directory must not contain dot path components: %q", dataDir)
		}
	}

	cleanPath = filepath.Clean(dataDir)
	root := volume + `\`
	if strings.EqualFold(cleanPath, root) {
		return "", "", "", fmt.Errorf("server data directory must name a leaf directory: %q", dataDir)
	}
	leaf = filepath.Base(cleanPath)
	if err := validateWindowsLeaf(leaf); err != nil {
		return "", "", "", fmt.Errorf("invalid server data directory leaf %q: %w", leaf, err)
	}
	parent = filepath.Dir(cleanPath)

	rootPointer, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return "", "", "", fmt.Errorf("encode server data volume root: %w", err)
	}
	if driveType := windows.GetDriveType(rootPointer); driveType != windows.DRIVE_FIXED {
		return "", "", "", fmt.Errorf("server data directory volume %q is not a fixed local drive", volume)
	}
	return cleanPath, leaf, parent, nil
}

func validateWindowsLeaf(leaf string) error {
	if leaf == "" || leaf == "." || leaf == ".." {
		return errors.New("leaf must name a directory")
	}
	if strings.TrimRight(leaf, " .") != leaf {
		return errors.New("leaf must not end in a space or dot")
	}
	if strings.IndexFunc(leaf, func(r rune) bool {
		return r < 32 || strings.ContainsRune(`<>:"/\|?*`, r)
	}) >= 0 {
		return errors.New("leaf contains a reserved Windows path character")
	}
	base := strings.ToUpper(strings.SplitN(leaf, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" ||
		(len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9') {
		return errors.New("leaf is a reserved Windows device name")
	}
	return nil
}

func openVerifiedDirectory(path string, pinFinal bool) (*verifiedWindowsDirectory, error) {
	volume := filepath.VolumeName(path)
	root := volume + `\`
	relative := strings.TrimPrefix(filepath.Clean(path), root)
	components := make([]string, 0, 8)
	if relative != "" && relative != "." {
		components = strings.Split(relative, `\`)
	}

	paths := make([]string, 0, len(components)+1)
	paths = append(paths, root)
	current := root
	for _, component := range components {
		current = filepath.Join(current, component)
		paths = append(paths, current)
	}

	handles := make([]windows.Handle, 0, len(paths))
	closeAll := func() error {
		var closeErr error
		for index := len(handles) - 1; index >= 0; index-- {
			closeErr = errors.Join(closeErr, windows.CloseHandle(handles[index]))
		}
		return closeErr
	}
	fail := func(cause error) (*verifiedWindowsDirectory, error) {
		return nil, errors.Join(cause, closeAll())
	}

	for index, componentPath := range paths {
		pointer, err := windows.UTF16PtrFromString(componentPath)
		if err != nil {
			return fail(fmt.Errorf("encode directory path %q: %w", componentPath, err))
		}
		desiredAccess := uint32(windows.FILE_READ_ATTRIBUTES)
		shareMode := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE)
		// 最终父目录只请求读取/列举，不请求 DELETE；同时拒绝
		// FILE_SHARE_DELETE，使后续 rename/replace 在 Guard 生命周期内由
		// 内核拒绝。卷根本身不能重命名，因此保留共享删除以免扩大权限影响。
		if pinFinal && index == len(paths)-1 && !strings.EqualFold(componentPath, root) {
			desiredAccess |= windows.FILE_LIST_DIRECTORY
			shareMode = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE
		}
		handle, err := windows.CreateFile(
			pointer,
			desiredAccess,
			shareMode,
			nil,
			windows.OPEN_EXISTING,
			directoryOpenFlags,
			0,
		)
		if err != nil {
			return fail(fmt.Errorf("open directory component %q without following reparse points: %w", componentPath, err))
		}
		handles = append(handles, handle)

		var information windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
			return fail(fmt.Errorf("inspect directory component %q: %w", componentPath, err))
		}
		if information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return fail(fmt.Errorf("directory component %q must not be a reparse point", componentPath))
		}
		if information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			return fail(fmt.Errorf("directory component %q is not a directory", componentPath))
		}
	}

	finalHandle := handles[len(handles)-1]
	canonicalPath, err := finalPathName(finalHandle)
	if err != nil {
		return fail(fmt.Errorf("resolve canonical directory %q: %w", path, err))
	}
	identity, err := fileIdentity(finalHandle)
	if err != nil {
		return fail(fmt.Errorf("read canonical directory identity %q: %w", path, err))
	}

	return &verifiedWindowsDirectory{handles: handles, path: canonicalPath, identity: identity}, nil
}

func fileIdentity(handle windows.Handle) (windowsFileIdentity, error) {
	var fileIDInfo windowsFileIDInfo
	if err := windows.GetFileInformationByHandleEx(
		handle,
		windows.FileIdInfo,
		(*byte)(unsafe.Pointer(&fileIDInfo)),
		uint32(unsafe.Sizeof(fileIDInfo)),
	); err != nil {
		return windowsFileIdentity{}, err
	}
	identity := windowsFileIdentity{volume: fileIDInfo.volumeSerial, file: fileIDInfo.fileID}
	if !identity.valid() {
		return windowsFileIdentity{}, errors.New("Windows returned an invalid 128-bit file identity")
	}
	return identity, nil
}

func finalPathName(handle windows.Handle) (string, error) {
	buffer := make([]uint16, 32768)
	length, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
	if err != nil {
		return "", err
	}
	if length == 0 || length >= uint32(len(buffer)) {
		return "", errors.New("canonical directory path exceeds the Windows path limit")
	}
	path := windows.UTF16ToString(buffer[:length])
	if strings.HasPrefix(path, `\\?\UNC\`) {
		return "", errors.New("canonical directory unexpectedly resolved to UNC")
	}
	path = strings.TrimPrefix(path, `\\?\`)
	return filepath.Clean(path), nil
}

func stableTargetHash(identity windowsFileIdentity, leaf string) string {
	const domain = "xtunnel-windows-stable-target-v1\x00"
	normalizedLeaf := strings.ToUpper(leaf)
	material := make([]byte, len(domain)+8+len(identity.file)+len(normalizedLeaf))
	copy(material, domain)
	binary.LittleEndian.PutUint64(material[len(domain):], identity.volume)
	copy(material[len(domain)+8:], identity.file[:])
	copy(material[len(domain)+8+len(identity.file):], normalizedLeaf)
	digest := sha256.Sum256([]byte(material))
	return fmt.Sprintf("%x", digest)
}

func (identity windowsFileIdentity) valid() bool {
	for _, value := range identity.file {
		if value != 0 {
			return true
		}
	}
	return false
}
