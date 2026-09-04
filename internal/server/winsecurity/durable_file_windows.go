//go:build windows

package winsecurity

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ForegroundFileSecurity binds a foreground profile's protected file DACL to
// the interactive user that owns the profile. Files deliberately do not carry
// inheritance flags: their children must never silently broaden this boundary.
type ForegroundFileSecurity struct {
	descriptor    *windows.SECURITY_DESCRIPTOR
	owner         *windows.SID
	expected      []accessACE
	serviceOwners []*windows.SID
}

// ForegroundDirectoryGuard keeps a validated managed data directory open
// without FILE_SHARE_DELETE. Long-lived SQLite users retain the guard so a
// pathname that was checked at startup cannot be replaced while sidecar files
// are being opened underneath it.
type ForegroundDirectoryGuard struct {
	handle windows.Handle
}

// foregroundDirectoryIdentity 绑定目录的完整卷序列号和 128-bit File ID。
// Restore 在关闭源 Handle 后执行目录提升，必须用这一身份复核最终目录，不能
// 退化到 BY_HANDLE_FILE_INFORMATION 的 64-bit FileIndex。
type foregroundDirectoryIdentity struct {
	volume uint64
	file   [16]byte
}

// foregroundFileIdentity 绑定受管文件的完整卷序列号和 128-bit File ID。
// 发布候选关闭后才能以 MoveFileEx 提升，因此必须在替换后确认最终路径仍指向
// 同一个候选对象，不能只凭路径、DACL 或旧的 64-bit FileIndex 判断。
type foregroundFileIdentity struct {
	volume uint64
	file   [16]byte
}

// foregroundFileIDInfo 对应 Windows FILE_ID_INFO，适用于 NTFS 与 ReFS。
type foregroundFileIDInfo struct {
	volume uint64
	file   [16]byte
}

// OpenForegroundDirectoryGuard validates a managed foreground directory and
// pins its identity until Close. The returned guard owns the underlying handle.
func OpenForegroundDirectoryGuard(path string) (*ForegroundDirectoryGuard, error) {
	handle, err := openValidatedForegroundDirectory(path)
	if err != nil {
		return nil, err
	}
	return &ForegroundDirectoryGuard{handle: handle}, nil
}

// Close releases the directory identity pin. It is safe to call once after all
// files that rely on the directory have been closed.
func (guard *ForegroundDirectoryGuard) Close() error {
	if guard == nil || guard.handle == 0 {
		return nil
	}
	err := windows.CloseHandle(guard.handle)
	guard.handle = 0
	return err
}

// NewForegroundFileSecurity builds the exact DACL for a Server-managed file in
// a foreground profile. Existing objects are only verified, never repaired.
func NewForegroundFileSecurity() (*ForegroundFileSecurity, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("read current Windows user SID: %w", err)
	}
	owner, err := windows.StringToSid(user.User.Sid.String())
	if err != nil {
		return nil, fmt.Errorf("copy current Windows user SID: %w", err)
	}
	descriptor, err := windows.SecurityDescriptorFromString(
		"O:" + owner.String() + "D:P(A;;FA;;;SY)(A;;FA;;;" + owner.String() + ")",
	)
	if err != nil {
		return nil, fmt.Errorf("create foreground file security descriptor: %w", err)
	}
	expected, err := descriptorACEs(descriptor)
	if err != nil {
		return nil, fmt.Errorf("inspect foreground file security descriptor: %w", err)
	}
	return &ForegroundFileSecurity{descriptor: descriptor, owner: owner, expected: expected}, nil
}

// Attributes returns descriptor-backed creation attributes. The caller must
// keep security alive until the Windows create call has returned.
func (security *ForegroundFileSecurity) Attributes() *windows.SecurityAttributes {
	return &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: security.descriptor,
	}
}

// ValidateFile verifies a no-follow handle is a regular file with this exact
// foreground profile owner and protected DACL.
func (security *ForegroundFileSecurity) ValidateFile(handle windows.Handle) error {
	if security == nil || security.descriptor == nil || security.owner == nil {
		return errors.New("foreground file security is uninitialized")
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return fmt.Errorf("inspect managed file: %w", err)
	}
	if information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 ||
		information.FileAttributes&windows.FILE_ATTRIBUTE_DEVICE != 0 ||
		information.VolumeSerialNumber == 0 ||
		information.FileIndexHigh == 0 && information.FileIndexLow == 0 {
		return errors.New("managed file is not a regular non-reparse file")
	}
	if _, err := foregroundFileID(handle); err != nil {
		return fmt.Errorf("inspect managed file identity: %w", err)
	}
	descriptor, err := windows.GetSecurityInfo(
		handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return fmt.Errorf("read file security descriptor: %w", err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return fmt.Errorf("read file owner: %w", err)
	}
	if owner == nil || (len(security.serviceOwners) == 0 && !owner.Equals(security.owner)) ||
		(len(security.serviceOwners) != 0 && !sidIn(owner, security.serviceOwners)) {
		if len(security.serviceOwners) != 0 {
			return errors.New("file owner is not permitted by the service profile")
		}
		return errors.New("file owner does not match the current Windows user")
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return fmt.Errorf("read file security descriptor control: %w", err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("file DACL is not protected")
	}
	actual, err := descriptorACEs(descriptor)
	if err != nil {
		return fmt.Errorf("read file DACL: %w", err)
	}
	if !sameACEs(actual, security.expected) {
		return errors.New("file DACL does not match the foreground profile")
	}
	runtime.KeepAlive(security)
	return nil
}

// Apply sets the foreground owner's exact protected DACL on a newly created
// file, then validates the same no-follow handle before any sensitive content
// is written. Existing files must only be validated, never repaired.
func (security *ForegroundFileSecurity) Apply(handle windows.Handle) error {
	if security == nil || security.descriptor == nil || security.owner == nil {
		return errors.New("foreground file security is uninitialized")
	}
	dacl, _, err := security.descriptor.DACL()
	if err != nil {
		return fmt.Errorf("read foreground file DACL: %w", err)
	}
	if dacl == nil {
		return errors.New("foreground file DACL is absent")
	}
	if err := windows.SetSecurityInfo(
		handle, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		security.owner, nil, dacl, nil,
	); err != nil {
		return fmt.Errorf("set foreground file security: %w", err)
	}
	return security.ValidateFile(handle)
}

// CreateForegroundDirectory creates a new managed foreground directory or
// verifies an existing one. It deliberately refuses to take over a directory
// whose owner, DACL, or object type does not already match this profile.
func CreateForegroundDirectory(path string, security *ForegroundDirectorySecurity) error {
	if path == "" || !filepath.IsAbs(path) || security == nil {
		return errors.New("managed foreground directory requires an absolute path and security policy")
	}
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("encode managed foreground directory path: %w", err)
	}
	if err := windows.CreateDirectory(pointer, security.Attributes()); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return fmt.Errorf("create managed foreground directory: %w", err)
	}
	handle, err := openForegroundDirectoryNoFollow(path)
	if err != nil {
		return fmt.Errorf("open managed foreground directory: %w", err)
	}
	defer windows.CloseHandle(handle)
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return fmt.Errorf("inspect managed foreground directory: %w", err)
	}
	if information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return errors.New("managed foreground directory is not a non-reparse directory")
	}
	if err := security.ValidateDirectory(handle); err != nil {
		return fmt.Errorf("validate managed foreground directory: %w", err)
	}
	return nil
}

// CreateForegroundDirectoryChild 仅在已验证的受管父目录下创建全新目录。Restore
// staging/rollback 不得复用既有目录：若同名对象存在，即使其 DACL 正确也必须由
// Journal 状态机决定后续动作，创建原语不能静默接管或清理。
func CreateForegroundDirectoryChild(parent, name string, security *ForegroundDirectorySecurity) (resultErr error) {
	if parent == "" || !filepath.IsAbs(parent) || security == nil {
		return errors.New("managed foreground child directory requires an absolute parent and security policy")
	}
	if err := validateManagedLeafName(name); err != nil {
		return fmt.Errorf("validate managed child directory name: %w", err)
	}
	parentHandle, err := openValidatedForegroundDirectory(parent)
	if err != nil {
		return fmt.Errorf("validate managed child directory parent: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(parentHandle)) }()
	path := filepath.Join(parent, name)
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("encode managed child directory path: %w", err)
	}
	if err := windows.CreateDirectory(pointer, security.Attributes()); err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return errors.New("managed child directory already exists")
		}
		return fmt.Errorf("create managed child directory: %w", err)
	}
	handle, err := openValidatedForegroundDirectory(path)
	if err != nil {
		return fmt.Errorf("re-open managed child directory: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(handle)) }()
	if err := security.ValidateDirectory(handle); err != nil {
		return fmt.Errorf("validate managed child directory: %w", err)
	}
	if _, err := foregroundDirectoryID(handle); err != nil {
		return fmt.Errorf("read managed child directory identity: %w", err)
	}
	return nil
}

// MoveForegroundDirectory 在同一受保护父目录内执行不覆盖的 Restore 目录提升。
// 它先通过 no-follow Handle 记录 source 身份，再关闭该 Handle 以便 MoveFileEx
// 改名，最后重开 destination 并匹配完整 FILE_ID_INFO。调用方必须在整个事务内
// 持有 ParentGuard 与 External Lock；该原语不替代 Journal 状态机的持久化边界。
func MoveForegroundDirectory(parent, sourceName, destinationName string, security *ForegroundDirectorySecurity) (resultErr error) {
	if parent == "" || !filepath.IsAbs(parent) || security == nil {
		return errors.New("managed foreground directory move requires an absolute parent and security policy")
	}
	if err := validateManagedLeafName(sourceName); err != nil {
		return fmt.Errorf("validate managed source directory name: %w", err)
	}
	if err := validateManagedLeafName(destinationName); err != nil || sourceName == destinationName {
		return errors.New("managed foreground directory move requires distinct strict leaf names")
	}
	parentHandle, err := openValidatedForegroundDirectory(parent)
	if err != nil {
		return fmt.Errorf("validate managed directory move parent: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(parentHandle)) }()

	sourcePath := filepath.Join(parent, sourceName)
	sourceHandle, err := openValidatedForegroundDirectory(sourcePath)
	if err != nil {
		return fmt.Errorf("open managed source directory: %w", err)
	}
	if err := security.ValidateDirectory(sourceHandle); err != nil {
		windows.CloseHandle(sourceHandle)
		return fmt.Errorf("validate managed source directory: %w", err)
	}
	sourceIdentity, err := foregroundDirectoryID(sourceHandle)
	if err != nil {
		windows.CloseHandle(sourceHandle)
		return fmt.Errorf("read managed source directory identity: %w", err)
	}
	if err := windows.CloseHandle(sourceHandle); err != nil {
		return fmt.Errorf("close managed source directory before move: %w", err)
	}

	destinationPath := filepath.Join(parent, destinationName)
	destinationHandle, destinationErr := openForegroundDirectoryNoFollow(destinationPath)
	if destinationErr == nil {
		closeErr := windows.CloseHandle(destinationHandle)
		return errors.Join(errors.New("managed destination directory already exists"), closeErr)
	}
	if !errors.Is(destinationErr, windows.ERROR_FILE_NOT_FOUND) && !errors.Is(destinationErr, windows.ERROR_PATH_NOT_FOUND) {
		return fmt.Errorf("inspect managed destination directory: %w", destinationErr)
	}

	from, err := windows.UTF16PtrFromString(sourcePath)
	if err != nil {
		return fmt.Errorf("encode managed source directory path: %w", err)
	}
	to, err := windows.UTF16PtrFromString(destinationPath)
	if err != nil {
		return fmt.Errorf("encode managed destination directory path: %w", err)
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return fmt.Errorf("promote managed directory: %w", err)
	}

	destinationHandle, err = openValidatedForegroundDirectory(destinationPath)
	if err != nil {
		return fmt.Errorf("re-open promoted managed directory: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(destinationHandle)) }()
	if err := security.ValidateDirectory(destinationHandle); err != nil {
		return fmt.Errorf("validate promoted managed directory: %w", err)
	}
	destinationIdentity, err := foregroundDirectoryID(destinationHandle)
	if err != nil {
		return fmt.Errorf("read promoted managed directory identity: %w", err)
	}
	if destinationIdentity != sourceIdentity {
		return errors.New("promoted managed directory identity changed")
	}
	if sourceExists, err := ForegroundDirectoryExists(sourcePath); err != nil {
		return fmt.Errorf("verify moved source directory absence: %w", err)
	} else if sourceExists {
		return errors.New("managed source directory still exists after move")
	}
	return nil
}

// ValidateForegroundDirectory verifies a caller-supplied data root before a
// managed child is created or used beneath it. A path check alone cannot make
// a future CreateTemp or replacement safe against a reparse-point takeover.
func ValidateForegroundDirectory(path string) error {
	handle, err := openValidatedForegroundDirectory(path)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	return nil
}

// SyncForegroundDirectory 在受保护、无跟随的目录 Handle 上请求 Windows 刷新
// 目录元数据。它只提供底层 API 成功的运行时屏障；调用方不得把该结果表述为对
// 任意硬件、文件系统或突然断电场景的物理持久化证明。
func SyncForegroundDirectory(path string) (resultErr error) {
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("managed foreground directory sync requires an absolute path")
	}
	security, err := NewForegroundDirectorySecurity()
	if err != nil {
		return fmt.Errorf("create managed foreground directory sync security: %w", err)
	}
	handle, err := openForegroundDirectoryForSync(path)
	if err != nil {
		return fmt.Errorf("open managed foreground directory for sync: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(handle)) }()
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return fmt.Errorf("inspect managed foreground directory for sync: %w", err)
	}
	if information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return errors.New("managed foreground directory sync target is not a non-reparse directory")
	}
	if err := security.ValidateDirectory(handle); err != nil {
		return fmt.Errorf("validate managed foreground directory for sync: %w", err)
	}
	if err := windows.FlushFileBuffers(handle); err != nil {
		return fmt.Errorf("flush managed foreground directory metadata: %w", err)
	}
	return nil
}

// ForegroundDirectoryExists 以 no-follow Handle 判断受管目录是否存在。存在时
// 必须同时满足前台 Profile 的精确 DACL；调用方不能把未受保护、重解析或普通
// 文件对象误当成不存在的恢复目录。
func ForegroundDirectoryExists(path string) (bool, error) {
	if path == "" || !filepath.IsAbs(path) {
		return false, errors.New("managed foreground directory requires an absolute path")
	}
	handle, err := openValidatedForegroundDirectory(path)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, windows.CloseHandle(handle)
}

// foregroundTreeNode 是删除前完成 no-follow 验证的节点计划。删除阶段必须重新
// 打开同一路径并匹配此完整身份，不能把预检结果直接当作路径删除授权。
type foregroundTreeNode struct {
	path        string
	directory   bool
	directoryID foregroundDirectoryIdentity
	fileID      foregroundFileIdentity
}

// ValidateForegroundDirectoryTree 对 Restore staging/rollback 树执行只读 no-follow
// 预检。它与删除使用相同的计划收集逻辑，但不持有或暴露可用于删除的权限。
func ValidateForegroundDirectoryTree(ctx context.Context, parent, rootLeaf string) error {
	_, err := collectForegroundDirectoryTree(ctx, parent, rootLeaf)
	return err
}

// RemoveForegroundDirectoryTree 删除受保护 parent 下的一个已验证目录树。它先完整
// 预检并记录每个节点的完整 File ID，随后按后序重新打开、复核并在同一 Handle 上
// 标记删除；任何路径替换、Reparse、权限漂移或取消都会停止操作，绝不路径式补偿。
// 返回 false, nil 仅表示 root 在开始预检时不存在。
// 删除阶段发生错误时，先前已标记的受管节点可能已被删除；调用方必须在可重试的
// 收敛状态中使用本原语，不能把错误解释为整棵树未变。
func RemoveForegroundDirectoryTree(ctx context.Context, parent, rootLeaf string) (removed bool, resultErr error) {
	if ctx == nil {
		return false, errors.New("managed directory tree removal requires a context")
	}
	if parent == "" || !filepath.IsAbs(parent) {
		return false, errors.New("managed directory tree removal requires an absolute parent")
	}
	if err := validateManagedLeafName(rootLeaf); err != nil {
		return false, fmt.Errorf("validate managed directory tree removal root name: %w", err)
	}
	parentHandle, err := openValidatedForegroundDirectory(parent)
	if err != nil {
		return false, fmt.Errorf("open managed directory tree removal parent: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(parentHandle)) }()

	rootPath := filepath.Join(parent, rootLeaf)
	rootHandle, err := openForegroundTreeNodeNoFollow(rootPath)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return false, nil
		}
		return false, fmt.Errorf("open managed directory tree removal root: %w", err)
	}
	if err := windows.CloseHandle(rootHandle); err != nil {
		return false, fmt.Errorf("close managed directory tree removal root: %w", err)
	}
	plan, err := collectForegroundDirectoryTreeWithParent(ctx, parent, rootLeaf, parentHandle)
	if err != nil {
		return false, err
	}
	if err := removeForegroundTreePlan(ctx, plan); err != nil {
		return false, err
	}
	return true, nil
}

func collectForegroundDirectoryTree(ctx context.Context, parent, rootLeaf string) (plan []foregroundTreeNode, resultErr error) {
	if ctx == nil {
		return nil, errors.New("managed directory tree validation requires a context")
	}
	if parent == "" || !filepath.IsAbs(parent) {
		return nil, errors.New("managed directory tree validation requires an absolute parent")
	}
	if err := validateManagedLeafName(rootLeaf); err != nil {
		return nil, fmt.Errorf("validate managed directory tree root name: %w", err)
	}
	parentHandle, err := openValidatedForegroundDirectory(parent)
	if err != nil {
		return nil, fmt.Errorf("open managed directory tree parent: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(parentHandle)) }()
	return collectForegroundDirectoryTreeWithParent(ctx, parent, rootLeaf, parentHandle)
}

// collectForegroundDirectoryTreeWithParent 由调用方持有经过验证的 parent
// Handle。RemoveForegroundDirectoryTree 在预检和逐节点删除之间持续持有它，避免
// 父目录在路径解析期间被替换为其他目录。
func collectForegroundDirectoryTreeWithParent(ctx context.Context, parent, rootLeaf string, parentHandle windows.Handle) ([]foregroundTreeNode, error) {
	parentIdentity, err := foregroundDirectoryID(parentHandle)
	if err != nil {
		return nil, fmt.Errorf("inspect managed directory tree parent identity: %w", err)
	}
	directorySecurity, err := NewForegroundDirectorySecurity()
	if err != nil {
		return nil, fmt.Errorf("create managed directory tree security: %w", err)
	}
	fileSecurity, err := NewForegroundFileSecurity()
	if err != nil {
		return nil, fmt.Errorf("create managed file tree security: %w", err)
	}
	return collectForegroundTreeNode(ctx, filepath.Join(parent, rootLeaf), parentIdentity.volume, true, directorySecurity, fileSecurity)
}

func collectForegroundTreeNode(ctx context.Context, path string, expectedVolume uint64, requireDirectory bool, directorySecurity *ForegroundDirectorySecurity, fileSecurity *ForegroundFileSecurity) (plan []foregroundTreeNode, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	handle, err := openForegroundTreeNodeNoFollow(path)
	if err != nil {
		return nil, fmt.Errorf("open managed directory tree node %q: %w", path, err)
	}
	file := os.NewFile(uintptr(handle), filepath.Base(path))
	if file == nil {
		windows.CloseHandle(handle)
		return nil, errors.New("wrap managed directory tree node handle")
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()

	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return nil, fmt.Errorf("inspect managed directory tree node %q: %w", path, err)
	}
	if information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || information.FileAttributes&windows.FILE_ATTRIBUTE_DEVICE != 0 {
		return nil, fmt.Errorf("managed directory tree node %q is a reparse point or device", path)
	}
	if information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		if requireDirectory {
			return nil, fmt.Errorf("managed directory tree root %q is not a directory", path)
		}
		if err := fileSecurity.ValidateFile(handle); err != nil {
			return nil, fmt.Errorf("validate managed directory tree file %q: %w", path, err)
		}
		identity, err := foregroundFileID(handle)
		if err != nil {
			return nil, fmt.Errorf("inspect managed directory tree file identity %q: %w", path, err)
		}
		if identity.volume != expectedVolume {
			return nil, fmt.Errorf("managed directory tree file %q crosses the parent volume", path)
		}
		return []foregroundTreeNode{{path: path, fileID: identity}}, nil
	}
	if err := directorySecurity.ValidateDirectory(handle); err != nil {
		return nil, fmt.Errorf("validate managed directory tree directory %q: %w", path, err)
	}
	identity, err := foregroundDirectoryID(handle)
	if err != nil {
		return nil, fmt.Errorf("inspect managed directory tree directory identity %q: %w", path, err)
	}
	if identity.volume != expectedVolume {
		return nil, fmt.Errorf("managed directory tree directory %q crosses the parent volume", path)
	}
	entries, err := file.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("enumerate managed directory tree directory %q: %w", path, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if err := validateManagedLeafName(entry.Name()); err != nil {
			return nil, fmt.Errorf("validate managed directory tree entry %q: %w", entry.Name(), err)
		}
		children, err := collectForegroundTreeNode(ctx, filepath.Join(path, entry.Name()), expectedVolume, false, directorySecurity, fileSecurity)
		if err != nil {
			return nil, err
		}
		plan = append(plan, children...)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append(plan, foregroundTreeNode{path: path, directory: true, directoryID: identity}), nil
}

func removeForegroundTreePlan(ctx context.Context, plan []foregroundTreeNode) error {
	directorySecurity, err := NewForegroundDirectorySecurity()
	if err != nil {
		return fmt.Errorf("create managed directory tree removal security: %w", err)
	}
	fileSecurity, err := NewForegroundFileSecurity()
	if err != nil {
		return fmt.Errorf("create managed file tree removal security: %w", err)
	}
	for _, node := range plan {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := removeForegroundTreeNode(node, directorySecurity, fileSecurity); err != nil {
			return err
		}
	}
	return nil
}

func removeForegroundTreeNode(node foregroundTreeNode, directorySecurity *ForegroundDirectorySecurity, fileSecurity *ForegroundFileSecurity) (resultErr error) {
	handle, err := openForegroundTreeNodeForDeletion(node.path)
	if err != nil {
		return fmt.Errorf("open managed directory tree removal node %q: %w", node.path, err)
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(handle)) }()
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return fmt.Errorf("inspect managed directory tree removal node %q: %w", node.path, err)
	}
	if information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || information.FileAttributes&windows.FILE_ATTRIBUTE_DEVICE != 0 {
		return fmt.Errorf("managed directory tree removal node %q is a reparse point or device", node.path)
	}
	isDirectory := information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if isDirectory != node.directory {
		return fmt.Errorf("managed directory tree removal node %q type changed after preflight", node.path)
	}
	if node.directory {
		if err := directorySecurity.ValidateDirectory(handle); err != nil {
			return fmt.Errorf("validate managed directory tree removal directory %q: %w", node.path, err)
		}
		identity, err := foregroundDirectoryID(handle)
		if err != nil {
			return fmt.Errorf("inspect managed directory tree removal directory identity %q: %w", node.path, err)
		}
		if identity != node.directoryID {
			return fmt.Errorf("managed directory tree removal directory %q identity changed after preflight", node.path)
		}
	} else {
		if err := fileSecurity.ValidateFile(handle); err != nil {
			return fmt.Errorf("validate managed directory tree removal file %q: %w", node.path, err)
		}
		identity, err := foregroundFileID(handle)
		if err != nil {
			return fmt.Errorf("inspect managed directory tree removal file identity %q: %w", node.path, err)
		}
		if identity != node.fileID {
			return fmt.Errorf("managed directory tree removal file %q identity changed after preflight", node.path)
		}
	}
	if err := markForegroundFileForDeletion(handle); err != nil {
		return fmt.Errorf("mark managed directory tree node %q for deletion: %w", node.path, err)
	}
	return nil
}

// openValidatedForegroundDirectory keeps a no-delete handle open after
// verifying the profile boundary. Publishers retain that handle until their
// candidate is durable and the final object has been revalidated, so an
// untrusted principal cannot replace the checked parent directory mid-flight.
func openValidatedForegroundDirectory(path string) (windows.Handle, error) {
	security, err := NewDirectorySecurityForPath(path)
	if err != nil {
		return 0, err
	}
	handle, err := openForegroundDirectoryNoFollow(path)
	if err != nil {
		return 0, fmt.Errorf("open managed foreground directory: %w", err)
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		windows.CloseHandle(handle)
		return 0, fmt.Errorf("inspect managed foreground directory: %w", err)
	}
	if information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		windows.CloseHandle(handle)
		return 0, errors.New("managed foreground directory is not a non-reparse directory")
	}
	if err := security.ValidateDirectory(handle); err != nil {
		windows.CloseHandle(handle)
		return 0, err
	}
	return handle, nil
}

// ReadForegroundFile opens an existing managed file through no-follow handles
// and rechecks both the parent directory and final file security before any
// content is returned to a Server startup path.
func ReadForegroundFile(directory, name string) ([]byte, error) {
	return readForegroundFile(directory, name, -1)
}

// ReadForegroundFileLimit 与 ReadForegroundFile 使用相同的 no-follow、DACL
// 和身份验证，但会在分配内容前限制不可信持久化元数据的最大大小。
func ReadForegroundFileLimit(directory, name string, maximum int64) ([]byte, error) {
	if maximum < 0 {
		return nil, errors.New("managed file read limit must not be negative")
	}
	return readForegroundFile(directory, name, maximum)
}

func readForegroundFile(directory, name string, maximum int64) ([]byte, error) {
	if directory == "" || !filepath.IsAbs(directory) {
		return nil, errors.New("managed file read requires an absolute directory and leaf name")
	}
	if err := validateManagedLeafName(name); err != nil {
		return nil, fmt.Errorf("validate managed file name: %w", err)
	}
	directoryHandle, err := openValidatedForegroundDirectory(directory)
	if err != nil {
		return nil, fmt.Errorf("validate managed file directory: %w", err)
	}
	defer windows.CloseHandle(directoryHandle)
	security, err := NewFileSecurityForPath(directory)
	if err != nil {
		return nil, err
	}
	handle, err := openFileNoFollow(filepath.Join(directory, name))
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), name)
	if file == nil {
		windows.CloseHandle(handle)
		return nil, errors.New("wrap managed file handle")
	}
	defer file.Close()
	if err := security.ValidateFile(handle); err != nil {
		return nil, fmt.Errorf("validate managed file: %w", err)
	}
	if maximum >= 0 {
		info, err := file.Stat()
		if err != nil {
			return nil, fmt.Errorf("inspect managed file size: %w", err)
		}
		if info.Size() > maximum {
			return nil, fmt.Errorf("managed file exceeds maximum size of %d bytes", maximum)
		}
	}
	content, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("read managed file: %w", err)
	}
	return content, nil
}

// ConsumeForegroundFileLimit reads and validates one managed file through a
// no-follow handle that also owns DELETE access. The callback decides whether
// its complete content is acceptable while that exact handle remains open;
// only callback success marks the same object for deletion. This preserves the
// file identity from content validation through Journal cleanup.
func ConsumeForegroundFileLimit(directory, name string, maximum int64, security *ForegroundFileSecurity, consume func([]byte) error) (resultErr error) {
	return consumeForegroundFileLimit(directory, name, maximum, security, consume, nil)
}

// ConsumeForegroundFileLimitWithPostDelete consumes one managed file and then
// runs postDelete after the marked file Handle has closed, while the validated
// parent directory Handle remains held. postDelete failures report an
// uncertain post-delete result: the file may already be absent and callers
// must not claim successful completion.
func ConsumeForegroundFileLimitWithPostDelete(directory, name string, maximum int64, security *ForegroundFileSecurity, consume func([]byte) error, postDelete func() error) (resultErr error) {
	if postDelete == nil {
		return errors.New("managed file post-delete consumption requires a callback")
	}
	return consumeForegroundFileLimit(directory, name, maximum, security, consume, postDelete)
}

func consumeForegroundFileLimit(directory, name string, maximum int64, security *ForegroundFileSecurity, consume func([]byte) error, postDelete func() error) (resultErr error) {
	if directory == "" || !filepath.IsAbs(directory) || security == nil || consume == nil {
		return errors.New("managed file consumption requires an absolute directory, security policy, and validator")
	}
	if err := validateManagedLeafName(name); err != nil {
		return fmt.Errorf("validate managed file name: %w", err)
	}
	directoryHandle, err := openValidatedForegroundDirectory(directory)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(directoryHandle)) }()
	path := filepath.Join(directory, name)
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		pointer,
		windows.DELETE|windows.FILE_READ_DATA|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(handle), filepath.Base(path))
	if file == nil {
		windows.CloseHandle(handle)
		return errors.New("wrap managed file handle for consumption")
	}
	fileClosed := false
	defer func() {
		if !fileClosed {
			resultErr = errors.Join(resultErr, file.Close())
		}
	}()
	if err := security.ValidateFile(handle); err != nil {
		return fmt.Errorf("validate managed file: %w", err)
	}
	if maximum >= 0 {
		info, err := file.Stat()
		if err != nil {
			return fmt.Errorf("inspect managed file size: %w", err)
		}
		if info.Size() > maximum {
			return fmt.Errorf("managed file exceeds maximum size of %d bytes", maximum)
		}
	}
	content, err := io.ReadAll(file)
	if err != nil {
		return fmt.Errorf("read managed file: %w", err)
	}
	if err := consume(content); err != nil {
		return fmt.Errorf("validate managed file content: %w", err)
	}
	if err := markForegroundFileForDeletion(handle); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close managed file after deletion mark: %w", err)
	}
	fileClosed = true
	if postDelete != nil {
		if err := postDelete(); err != nil {
			return fmt.Errorf("complete managed file post-delete action: %w", err)
		}
	}
	return nil
}

// PublishForegroundFile writes a complete candidate, flushes it, and performs
// a same-directory Replace Existing + Write Through publication. A failure
// before replacement only removes the caller-owned candidate, preserving the
// prior final file. The final object is re-opened without following reparse
// points and revalidated before success is reported. An existing final file
// must already satisfy the managed-file boundary; publication never takes over
// an inherited-DACL or reparse-point object.
func PublishForegroundFile(directory, name string, content []byte, security *ForegroundFileSecurity) (resultErr error) {
	if directory == "" || !filepath.IsAbs(directory) {
		return errors.New("managed file publication requires an absolute directory and leaf name")
	}
	if err := validateManagedLeafName(name); err != nil {
		return fmt.Errorf("validate managed file name: %w", err)
	}
	if security == nil {
		return errors.New("managed file publication requires foreground file security")
	}
	directoryHandle, err := openValidatedForegroundDirectory(directory)
	if err != nil {
		return fmt.Errorf("validate managed file directory: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(directoryHandle)) }()
	return publishFileCandidate(directory, name, content, security, true)
}

// publishFileCandidate 在调用方固定父目录期间发布同目录候选。安装不能覆盖，运行时受管 Secret 才允许原子替换。
func publishFileCandidate(directory, name string, content []byte, security *ForegroundFileSecurity, replace bool) (resultErr error) {
	finalPath := filepath.Join(directory, name)
	if err := validateExistingForegroundFile(finalPath, security); err != nil {
		return fmt.Errorf("validate existing managed file: %w", err)
	}
	// 在不可预测名称上以 CREATE_NEW 一次设置完整 SD。LocalService 只有
	// Modify，不能依赖 Owner 隐含 WRITE_DAC 或创建后重新接管安全描述符。
	temporaryPath := filepath.Join(directory, "."+name+".tmp-"+rand.Text())
	temporary, err := createSecuredFile(temporaryPath, security)
	if err != nil {
		return fmt.Errorf("create managed file candidate: %w", err)
	}
	candidateIdentity, err := foregroundFileID(windows.Handle(temporary.Fd()))
	if err != nil {
		return errors.Join(err, markForegroundFileForDeletion(windows.Handle(temporary.Fd())), temporary.Close())
	}
	published := false
	defer func() {
		if temporary != nil {
			if !published {
				resultErr = errors.Join(resultErr, markForegroundFileForDeletion(windows.Handle(temporary.Fd())))
			}
			resultErr = errors.Join(resultErr, temporary.Close())
		} else if !published {
			resultErr = errors.Join(resultErr, deleteOwnedForegroundCandidate(temporaryPath, security, candidateIdentity))
		}
	}()
	if err := security.ValidateFile(windows.Handle(temporary.Fd())); err != nil {
		return fmt.Errorf("validate managed file candidate: %w", err)
	}
	if _, err := temporary.Write(content); err != nil {
		return fmt.Errorf("write managed file candidate: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("flush managed file candidate: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close managed file candidate: %w", err)
	}
	temporary = nil

	from, err := windows.UTF16PtrFromString(temporaryPath)
	if err != nil {
		return fmt.Errorf("encode managed file candidate path: %w", err)
	}
	to, err := windows.UTF16PtrFromString(finalPath)
	if err != nil {
		return fmt.Errorf("encode managed file final path: %w", err)
	}
	flags := uint32(windows.MOVEFILE_WRITE_THROUGH)
	if replace {
		flags |= windows.MOVEFILE_REPLACE_EXISTING
	}
	if err := windows.MoveFileEx(from, to, flags); err != nil {
		return fmt.Errorf("publish managed file candidate: %w", err)
	}
	published = true

	handle, err := openFileNoFollow(finalPath)
	if err != nil {
		return fmt.Errorf("re-open published managed file: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(handle)) }()
	if err := security.ValidateFile(handle); err != nil {
		return fmt.Errorf("revalidate published managed file: %w", err)
	}
	finalIdentity, err := foregroundFileID(handle)
	if err != nil {
		return fmt.Errorf("inspect published managed file identity: %w", err)
	}
	if finalIdentity != candidateIdentity {
		return errors.New("published managed file identity does not match its candidate")
	}
	return nil
}

// ReplaceForegroundFile promotes an already protected candidate to a managed
// final name. Rotation keeps its Journal until every such replacement has
// completed, so a crash cannot expose a mismatched key and certificate pair.
func ReplaceForegroundFile(directory, candidateName, finalName string, security *ForegroundFileSecurity) (resultErr error) {
	if directory == "" || !filepath.IsAbs(directory) || security == nil {
		return errors.New("managed file replacement requires an absolute directory and security policy")
	}
	if err := validateManagedLeafName(candidateName); err != nil {
		return fmt.Errorf("validate managed file candidate name: %w", err)
	}
	if err := validateManagedLeafName(finalName); err != nil || candidateName == finalName {
		return errors.New("managed file replacement requires distinct strict leaf names")
	}
	directoryHandle, err := openValidatedForegroundDirectory(directory)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(directoryHandle)) }()
	candidatePath := filepath.Join(directory, candidateName)
	handle, err := openFileNoFollow(candidatePath)
	if err != nil {
		return fmt.Errorf("open managed file candidate: %w", err)
	}
	if err := security.ValidateFile(handle); err != nil {
		windows.CloseHandle(handle)
		return fmt.Errorf("validate managed file candidate: %w", err)
	}
	candidateIdentity, err := foregroundFileID(handle)
	if err != nil {
		windows.CloseHandle(handle)
		return fmt.Errorf("inspect managed file candidate identity: %w", err)
	}
	if err := windows.CloseHandle(handle); err != nil {
		return fmt.Errorf("close managed file candidate: %w", err)
	}
	from, err := windows.UTF16PtrFromString(candidatePath)
	if err != nil {
		return err
	}
	finalPath := filepath.Join(directory, finalName)
	if err := validateExistingForegroundFile(finalPath, security); err != nil {
		return fmt.Errorf("validate existing managed file: %w", err)
	}
	to, err := windows.UTF16PtrFromString(finalPath)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return fmt.Errorf("replace managed file: %w", err)
	}
	handle, err = openFileNoFollow(finalPath)
	if err != nil {
		return fmt.Errorf("re-open replaced managed file: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(handle)) }()
	if err := security.ValidateFile(handle); err != nil {
		return fmt.Errorf("revalidate replaced managed file: %w", err)
	}
	finalIdentity, err := foregroundFileID(handle)
	if err != nil {
		return fmt.Errorf("inspect replaced managed file identity: %w", err)
	}
	if finalIdentity != candidateIdentity {
		return errors.New("replaced managed file identity does not match its candidate")
	}
	return nil
}

// DeleteForegroundFile marks only a revalidated managed leaf for deletion on
// its own no-follow handle. Unlike a path-based DeleteFile call, the validated
// object cannot be swapped between the security check and deletion.
func DeleteForegroundFile(directory, name string, security *ForegroundFileSecurity) (resultErr error) {
	if directory == "" || !filepath.IsAbs(directory) || security == nil {
		return errors.New("managed file deletion requires an absolute directory and security policy")
	}
	if err := validateManagedLeafName(name); err != nil {
		return fmt.Errorf("validate managed file name: %w", err)
	}
	directoryHandle, err := openValidatedForegroundDirectory(directory)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(directoryHandle)) }()
	path := filepath.Join(directory, name)
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		pointer,
		windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(handle)) }()
	if err := security.ValidateFile(handle); err != nil {
		return err
	}
	if err := markForegroundFileForDeletion(handle); err != nil {
		return err
	}
	return nil
}

// deleteOwnedForegroundCandidate reopens a candidate only with DELETE rights,
// validates both its protected DACL and its creation identity, then marks that
// exact handle for deletion. A failed reopen or mismatch deliberately leaves
// the path untouched instead of deleting whichever object now has that name.
func deleteOwnedForegroundCandidate(path string, security *ForegroundFileSecurity, expected foregroundFileIdentity) (resultErr error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		pointer,
		windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(handle)) }()
	if err := security.ValidateFile(handle); err != nil {
		return err
	}
	actual, err := foregroundFileID(handle)
	if err != nil {
		return fmt.Errorf("inspect managed file candidate identity: %w", err)
	}
	if actual != expected {
		return errors.New("managed file candidate identity changed before cleanup")
	}
	return markForegroundFileForDeletion(handle)
}

// deleteCreatedForegroundCandidate only removes the exact object whose ID was
// captured immediately after CreateTemp. It covers failures before the
// candidate DACL can be applied, when the file is still empty but its path
// must not be reused for a path-based cleanup.
func deleteCreatedForegroundCandidate(path string, expected foregroundFileIdentity) (resultErr error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		pointer,
		windows.DELETE|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(handle)) }()
	actual, err := foregroundFileID(handle)
	if err != nil {
		return fmt.Errorf("inspect newly created managed file candidate identity: %w", err)
	}
	if actual != expected {
		return errors.New("newly created managed file candidate identity changed before cleanup")
	}
	return markForegroundFileForDeletion(handle)
}

func markForegroundFileForDeletion(handle windows.Handle) error {
	disposition := byte(1)
	if err := windows.SetFileInformationByHandle(
		handle,
		windows.FileDispositionInfo,
		&disposition,
		uint32(unsafe.Sizeof(disposition)),
	); err != nil {
		return fmt.Errorf("mark managed file for deletion: %w", err)
	}
	return nil
}

func validateManagedLeafName(name string) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name ||
		strings.ContainsAny(name, `\\/:`) || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return errors.New("managed file name is not a strict Windows leaf")
	}
	for _, character := range name {
		if character < 0x20 {
			return errors.New("managed file name contains a control character")
		}
	}
	base := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" ||
		(len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9') {
		return errors.New("managed file name is a reserved Windows device")
	}
	return nil
}

func openFileNoFollow(path string) (windows.Handle, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(
		pointer, windows.FILE_READ_DATA|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL,
		windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0,
	)
}

// validateExistingForegroundFile accepts a missing final file, but never
// promotes over a present object unless it already has the exact protected
// profile DACL and a stable 128-bit identity. Both Journal phase updates and
// Gateway rotation use this before MOVEFILE_REPLACE_EXISTING.
func validateExistingForegroundFile(path string, security *ForegroundFileSecurity) error {
	handle, err := openFileNoFollow(path)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return nil
	}
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	if err := security.ValidateFile(handle); err != nil {
		return err
	}
	if _, err := foregroundFileID(handle); err != nil {
		return fmt.Errorf("inspect managed file identity: %w", err)
	}
	return nil
}

func openForegroundDirectoryNoFollow(path string) (windows.Handle, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(
		pointer, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0,
	)
}

func openForegroundDirectoryForSync(path string) (windows.Handle, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(
		pointer,
		windows.GENERIC_WRITE|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
}

func openForegroundTreeNodeNoFollow(path string) (windows.Handle, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(
		pointer,
		windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
}

// openForegroundTreeNodeForDeletion 为已完成预检的节点申请删除权限。调用方必须在
// 同一 Handle 上重新验证节点身份与 DACL 后才能设置删除标记，不能把路径当作授权。
func openForegroundTreeNodeForDeletion(path string) (windows.Handle, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(
		pointer,
		windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
}

func foregroundDirectoryID(handle windows.Handle) (foregroundDirectoryIdentity, error) {
	var information foregroundFileIDInfo
	if err := windows.GetFileInformationByHandleEx(
		handle,
		windows.FileIdInfo,
		(*byte)(unsafe.Pointer(&information)),
		uint32(unsafe.Sizeof(information)),
	); err != nil {
		return foregroundDirectoryIdentity{}, err
	}
	identity := foregroundDirectoryIdentity{volume: information.volume, file: information.file}
	if identity.volume == 0 {
		return foregroundDirectoryIdentity{}, errors.New("managed directory has an invalid volume identity")
	}
	for _, value := range identity.file {
		if value != 0 {
			return identity, nil
		}
	}
	return foregroundDirectoryIdentity{}, errors.New("managed directory has an invalid file identity")
}

func foregroundFileID(handle windows.Handle) (foregroundFileIdentity, error) {
	var information foregroundFileIDInfo
	if err := windows.GetFileInformationByHandleEx(
		handle,
		windows.FileIdInfo,
		(*byte)(unsafe.Pointer(&information)),
		uint32(unsafe.Sizeof(information)),
	); err != nil {
		return foregroundFileIdentity{}, err
	}
	identity := foregroundFileIdentity{volume: information.volume, file: information.file}
	if identity.volume == 0 {
		return foregroundFileIdentity{}, errors.New("managed file has an invalid volume identity")
	}
	for _, value := range identity.file {
		if value != 0 {
			return identity, nil
		}
	}
	return foregroundFileIdentity{}, errors.New("managed file has an invalid file identity")
}
