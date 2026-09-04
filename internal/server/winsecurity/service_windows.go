//go:build windows

package winsecurity

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/windows"
)

// ServiceObjectKind identifies the frozen access policy of a managed SCM object.
type ServiceObjectKind uint8

const (
	ServiceBinary ServiceObjectKind = iota + 1
	ServiceConfig
	ServiceData
)

// NewServiceDirectorySecurity creates an installation directory policy. Installer
// objects are owned by Administrators; existing SYSTEM-owned objects are accepted.
func NewServiceDirectorySecurity(kind ServiceObjectKind) (*ForegroundDirectorySecurity, error) {
	descriptor, owner, owners, expected, err := serviceDescriptor(kind, true, false)
	if err != nil {
		return nil, err
	}
	return &ForegroundDirectorySecurity{descriptor: descriptor, owner: owner, expected: expected, serviceOwners: owners}, nil
}

// NewServiceFileSecurity creates the exact policy for an installed Binary/Config
// or a runtime managed data file. Runtime files accept LS, SYSTEM and BA owners.
func NewServiceFileSecurity(kind ServiceObjectKind) (*ForegroundFileSecurity, error) {
	descriptor, owner, owners, expected, err := serviceDescriptor(kind, false, kind == ServiceData)
	if err != nil {
		return nil, err
	}
	return &ForegroundFileSecurity{descriptor: descriptor, owner: owner, expected: expected, serviceOwners: owners, inherited: kind == ServiceData}, nil
}

func serviceDescriptor(kind ServiceObjectKind, directory, runtimeObject bool) (*windows.SECURITY_DESCRIPTOR, *windows.SID, []*windows.SID, []accessACE, error) {
	policy, err := newOperatorTLSSecurity()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	owner := policy.owners[1]
	owners := policy.owners
	if runtimeObject {
		localService, err := windows.StringToSid("S-1-5-19")
		if err != nil {
			return nil, nil, nil, nil, err
		}
		owners = append(owners, localService)
		user, err := windows.GetCurrentProcessToken().GetTokenUser()
		if err != nil {
			return nil, nil, nil, nil, err
		}
		if user.User.Sid.Equals(localService) {
			owner = localService
		}
	}
	access := "FR"
	switch kind {
	case ServiceBinary:
		access = "FRFX"
	case ServiceConfig:
		if directory {
			access = "FRFX"
		}
	case ServiceData:
		access = "0x1301bf"
	default:
		return nil, nil, nil, nil, errors.New("invalid service object kind")
	}
	flags := ""
	if directory {
		flags = "OICI"
	}
	control := "P"
	if runtimeObject {
		flags += "ID"
		control = ""
	}
	sddl := "O:" + owner.String() + "D:" + control + "(A;" + flags + ";FA;;;SY)(A;" + flags + ";FA;;;BA)(A;" + flags + ";" + access + ";;;" + policy.service.String() + ")"
	// OWNER RIGHTS 的显式 READ_CONTROL 取代 Owner 隐含 WRITE_DAC。
	// 运行时子对象只能继承受保护根的精确权限。父 OW_RC 会拒绝显式 SD 创建，
	// 因此运行时不重设 Owner/DACL；同句柄验证继承结果后才能写入内容。
	if kind == ServiceData {
		sddl += "(A;" + flags + ";RC;;;OW)"
	}
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("create service descriptor: %w", err)
	}
	expected, err := descriptorACEs(descriptor)
	return descriptor, owner, owners, expected, err
}

// NewDirectorySecurityForPath chooses the service policy only for the fixed
// ProgramData data/runtime trees and a token authorized for this exact service.
func NewDirectorySecurityForPath(path string) (*ForegroundDirectorySecurity, error) {
	service, root, err := serviceDataPath(path)
	if err != nil {
		return nil, err
	}
	if !service {
		return NewForegroundDirectorySecurity()
	}
	if err := validateServiceStorageToken(); err != nil {
		return nil, err
	}
	descriptor, owner, owners, expected, err := serviceDescriptor(ServiceData, true, !root)
	if err != nil {
		return nil, err
	}
	return &ForegroundDirectorySecurity{descriptor: descriptor, owner: owner, expected: expected, serviceOwners: owners, inherited: !root}, nil
}

// pinInheritedDirectory 保持从卷根到实际父目录的 no-follow 身份链。固定根必须
// 满足 Protected DACL，后代必须逐层满足精确继承矩阵，不能仅凭叶子四条 ACE
// 推断继承来源。调用方在创建/读取/发布和最终验证结束后逆序释放这条链。
func pinInheritedDirectory(path string) ([]windows.Handle, error) {
	service, _, err := serviceDataPath(path)
	if err != nil || !service {
		return nil, err
	}
	return pinServiceAncestors(filepath.Join(path, ".inheritance-pin"), false)
}

func pinRequiredInheritedDirectory(path string) ([]windows.Handle, error) {
	service, _, err := serviceDataPath(path)
	if err != nil {
		return nil, err
	}
	if !service {
		return nil, errors.New("inherited service policy requires the fixed Data or Runtime tree")
	}
	return pinServiceAncestors(filepath.Join(path, ".inheritance-pin"), false)
}

// NewFileSecurityForPath binds runtime file creation/validation to its parent.
func NewFileSecurityForPath(parent string) (*ForegroundFileSecurity, error) {
	service, _, err := serviceDataPath(parent)
	if err != nil {
		return nil, err
	}
	if !service {
		return NewForegroundFileSecurity()
	}
	if err := validateServiceStorageToken(); err != nil {
		return nil, err
	}
	return NewServiceFileSecurity(ServiceData)
}

// ValidateDataParentDirectory validates the read-only parent used by startup's
// Restore-residue check. Service 的固定 Server 根按 Config 目录权限验证；
// 此入口只读，不把该根加入运行时可创建 Data/Runtime 子对象的权限选择。
func ValidateDataParentDirectory(path string) error {
	service, err := isServiceDataParentPath(path)
	if err != nil {
		return err
	}
	if !service {
		return ValidateForegroundDirectory(path)
	}
	if err := validateServiceStorageToken(); err != nil {
		return err
	}
	return ValidateServiceObject(path, ServiceConfig, true)
}

func isServiceDataParentPath(path string) (bool, error) {
	programData, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil {
		return false, fmt.Errorf("resolve service ProgramData: %w", err)
	}
	root := filepath.Join(programData, "XTunnel", "Server")
	if !strings.EqualFold(filepath.Clean(path), root) {
		return false, nil
	}
	if err := validateOperatorTLSPath(path); err != nil {
		return false, err
	}
	return true, nil
}

func serviceDataPath(path string) (service, root bool, err error) {
	programData, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil {
		return false, false, fmt.Errorf("resolve service ProgramData: %w", err)
	}
	for _, leaf := range []string{"data", "runtime"} {
		base := filepath.Join(programData, "XTunnel", "Server", leaf)
		clean := filepath.Clean(path)
		if strings.EqualFold(clean, base) || strings.HasPrefix(strings.ToLower(clean), strings.ToLower(base)+`\`) {
			if err := validateOperatorTLSPath(path); err != nil {
				return false, false, err
			}
			return true, strings.EqualFold(clean, base), nil
		}
	}
	return false, false, nil
}

func validateServiceStorageToken() error {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	if user.User.Sid.String() == "S-1-5-18" {
		return nil
	}
	if token.IsElevated() {
		return nil
	}
	if user.User.Sid.String() != "S-1-5-19" {
		return errors.New("service storage requires elevated maintenance or XTunnelServer LocalService token")
	}
	sid, err := serviceSID(xtunnelServerServiceName)
	if err != nil {
		return err
	}
	groups, err := token.GetTokenGroups()
	if err != nil {
		return err
	}
	for _, group := range groups.AllGroups() {
		if group.Sid.Equals(sid) && group.Attributes&windows.SE_GROUP_ENABLED != 0 && group.Attributes&windows.SE_GROUP_USE_FOR_DENY_ONLY == 0 {
			return nil
		}
	}
	return errors.New("LocalService token does not carry enabled XTunnelServer SID")
}

// ValidateServiceAncestors performs read-only preflight of all existing parents.
// Missing parents are allowed; creation must repeat the check while holding pins.
func ValidateServiceAncestors(path string) error {
	handles, err := pinServiceAncestors(path, true)
	return errors.Join(err, closeHandles(handles))
}

func pinServiceAncestors(path string, allowMissing bool) ([]windows.Handle, error) {
	if err := validateOperatorTLSPath(path); err != nil {
		return nil, err
	}
	policy, err := newOperatorTLSSecurity()
	if err != nil {
		return nil, err
	}
	trustedInstaller, err := serviceSID("TrustedInstaller")
	if err != nil {
		return nil, err
	}
	programFiles, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
	if err != nil {
		return nil, err
	}
	programData, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil {
		return nil, err
	}
	root := filepath.VolumeName(path) + `\`
	current := root
	paths := []string{root}
	for _, component := range strings.Split(strings.TrimPrefix(filepath.Dir(filepath.Clean(path)), root), `\`) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		paths = append(paths, current)
	}
	var handles []windows.Handle
	for _, parent := range paths {
		handle, err := openForegroundDirectoryNoFollow(parent)
		if err != nil {
			if allowMissing && (errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)) {
				return handles, nil
			}
			return handles, fmt.Errorf("open service ancestor: %w", err)
		}
		handles = append(handles, handle)
		var info windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
			return handles, err
		}
		if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			return handles, errors.New("service ancestor is not a non-reparse directory")
		}
		if _, err := foregroundDirectoryID(handle); err != nil {
			return handles, err
		}
		managed, _, err := serviceDataPath(parent)
		if err != nil {
			return handles, err
		}
		if managed {
			security, err := NewDirectorySecurityForPath(parent)
			if err != nil {
				return handles, err
			}
			if err := security.ValidateDirectory(handle); err != nil {
				return handles, err
			}
			continue
		}
		// TrustedInstaller 仅是操作系统 KnownFolder 及其祖先的可信维护者；
		// 共享 XTunnel 与私有 Server 对象仍严格限定 SYSTEM/Administrators。
		trusted := policy.owners
		osAncestor := false
		for _, known := range []string{programFiles, programData} {
			if strings.EqualFold(parent, known) || strings.HasPrefix(strings.ToLower(known), strings.ToLower(strings.TrimRight(parent, `\`))+`\`) {
				trusted = append(trusted, trustedInstaller)
				osAncestor = true
				break
			}
		}
		descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			return handles, err
		}
		owner, _, err := descriptor.Owner()
		if err != nil {
			return handles, err
		}
		if !sidIn(owner, trusted) {
			return handles, errors.New("service ancestor owner is not trusted")
		}
		entries, err := operatorTLSACEs(descriptor)
		if err != nil {
			return handles, err
		}
		if err := validateServiceAncestorACEs(entries, trusted, osAncestor); err != nil {
			return handles, err
		}
	}
	return handles, nil
}

func validateServiceAncestorACEs(entries []operatorTLSACE, trusted []*windows.SID, osAncestor bool) error {
	dangerous := windows.ACCESS_MASK(windows.DELETE | fileDeleteChild | windows.WRITE_DAC | windows.WRITE_OWNER | windows.GENERIC_ALL | windows.GENERIC_WRITE | windows.MAXIMUM_ALLOWED)
	if !osAncestor {
		// 产品共享祖先即使禁止删除，也不能允许原地变成 Reparse Point。
		// FSCTL_SET_REPARSE_POINT 接受 WRITE_DATA 或 WRITE_ATTRIBUTES 任一位；
		// no-share-delete 的路径固定不能阻止这类原地修改。系统 KnownFolder
		// 及其祖先保持 OS 管理的创建子对象权限，不套用产品私有目录策略。
		dangerous |= windows.FILE_WRITE_DATA | windows.FILE_WRITE_ATTRIBUTES
	}
	for _, ace := range entries {
		if ace.flags&windows.INHERIT_ONLY_ACE != 0 || ace.typeID == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.typeID != windows.ACCESS_ALLOWED_ACE_TYPE {
			return errors.New("service ancestor has unsupported ACE")
		}
		if !sidIn(ace.sid, trusted) && ace.mask&dangerous != 0 {
			return errors.New("service ancestor permits untrusted replacement or security control")
		}
	}
	return nil
}

// ValidateServiceObject validates a managed object without changing its ACL.
func ValidateServiceObject(path string, kind ServiceObjectKind, directory bool) (resultErr error) {
	parents, err := pinServiceAncestors(path, false)
	defer func() { resultErr = errors.Join(resultErr, closeHandles(parents)) }()
	if err != nil {
		return err
	}
	if directory {
		security, err := NewServiceDirectorySecurity(kind)
		if err != nil {
			return err
		}
		handle, err := openForegroundDirectoryNoFollow(path)
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(handle)) }()
		var info windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
			return err
		}
		if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			return errors.New("service object is not a non-reparse directory")
		}
		if _, err := foregroundDirectoryID(handle); err != nil {
			return err
		}
		return security.ValidateDirectory(handle)
	}
	security, err := NewServiceFileSecurity(kind)
	if err != nil {
		return err
	}
	handle, err := openFileNoFollow(path)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(handle)) }()
	return security.ValidateFile(handle)
}

// CreateServiceDirectory creates with its complete SD or validates an existing
// object. The pinned parent chain prevents path replacement during creation.
func CreateServiceDirectory(path string, kind ServiceObjectKind) (resultErr error) {
	parents, err := pinServiceAncestors(path, false)
	defer func() { resultErr = errors.Join(resultErr, closeHandles(parents)) }()
	if err != nil {
		return err
	}
	security, err := NewServiceDirectorySecurity(kind)
	if err != nil {
		return err
	}
	return CreateForegroundDirectory(path, security)
}

// CreateServiceSharedDirectory prepares an XTunnel ancestor shared with Agent.
// 普通用户只有读取和遍历；Server 自有文件另外显式设置精确 Protected DACL。
// 既有共享目录仅验证安全性质，不能接管或收窄其他组件的合法读取权限。
func CreateServiceSharedDirectory(path string) (resultErr error) {
	parents, err := pinServiceAncestors(path, false)
	defer func() { resultErr = errors.Join(resultErr, closeHandles(parents)) }()
	if err != nil {
		return err
	}
	descriptor, err := windows.SecurityDescriptorFromString("O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FRFX;;;BU)")
	if err != nil {
		return err
	}
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	security := &ForegroundDirectorySecurity{descriptor: descriptor}
	err = windows.CreateDirectory(pointer, security.Attributes())
	runtime.KeepAlive(security)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return err
	}
	verified, err := pinServiceAncestors(filepath.Join(path, ".shared-validation"), false)
	return errors.Join(err, closeHandles(verified))
}

// CreateServiceFile flushes a fully secured sibling candidate before publishing
// it with a non-replacing Write Through move. Existing objects are never changed.
func CreateServiceFile(path string, kind ServiceObjectKind, content []byte) (resultErr error) {
	parents, err := pinServiceAncestors(path, false)
	defer func() { resultErr = errors.Join(resultErr, closeHandles(parents)) }()
	if err != nil {
		return err
	}
	security, err := NewServiceFileSecurity(kind)
	if err != nil {
		return err
	}
	return publishFileCandidate(filepath.Dir(path), filepath.Base(path), content, security, false)
}

func createSecuredFile(path string, security *ForegroundFileSecurity) (*os.File, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(pointer, windows.DELETE|windows.FILE_WRITE_DATA|windows.FILE_READ_DATA|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, windows.FILE_SHARE_READ, security.Attributes(), windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	runtime.KeepAlive(security)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		return nil, errors.Join(errors.New("wrap service file"), windows.CloseHandle(handle))
	}
	return file, nil
}

// ReadServiceFile reads a verified Binary/Config using the same no-follow handle.
func ReadServiceFile(path string, kind ServiceObjectKind) (content []byte, resultErr error) {
	parents, err := pinServiceAncestors(path, false)
	defer func() { resultErr = errors.Join(resultErr, closeHandles(parents)) }()
	if err != nil {
		return nil, err
	}
	security, err := NewServiceFileSecurity(kind)
	if err != nil {
		return nil, err
	}
	handle, err := openFileNoFollow(path)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		return nil, errors.Join(errors.New("wrap service read file"), windows.CloseHandle(handle))
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	if err := security.ValidateFile(handle); err != nil {
		return nil, err
	}
	return io.ReadAll(file)
}

// DeleteServiceFile validates and marks precisely the opened managed object.
func DeleteServiceFile(path string, kind ServiceObjectKind) (resultErr error) {
	parents, err := pinServiceAncestors(path, false)
	defer func() { resultErr = errors.Join(resultErr, closeHandles(parents)) }()
	if err != nil {
		return err
	}
	security, err := NewServiceFileSecurity(kind)
	if err != nil {
		return err
	}
	handle, err := openForegroundTreeNodeForDeletion(path)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(handle)) }()
	if err := security.ValidateFile(handle); err != nil {
		return err
	}
	return markForegroundFileForDeletion(handle)
}
