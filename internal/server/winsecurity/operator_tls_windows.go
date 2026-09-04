//go:build windows

package winsecurity

import (
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const xtunnelServerServiceName = "XTunnelServer"

// FILE_DELETE_CHILD is directory-specific access 0x40. x/sys exposes the
// file aliases used by CreateFile but not this directory-only name.
const fileDeleteChild windows.ACCESS_MASK = 0x00000040

// ReadOperatorTLSFiles reads an external public-TLS pair without modifying it.
// 外部证书管理器保留其 ACL 的组织方式；XTunnel 只验证启动所需的安全性质：
// SYSTEM/Administrators 拥有对象，Service SID 能读取，且任何非所有者主体
// 都不能借由文件或直接父目录修改、删除或接管对象。无法从 DACL 证明这些性质
// 时快速失败，绝不通过修复或重写 operator-owned 文件来换取启动成功。
func ReadOperatorTLSFiles(certPath, keyPath string) ([]byte, []byte, error) {
	if filepath.Clean(certPath) == filepath.Clean(keyPath) {
		return nil, nil, errors.New("public TLS certificate and private key must be distinct files")
	}
	policy, err := newOperatorTLSSecurity()
	if err != nil {
		return nil, nil, err
	}
	certPEM, err := readOperatorTLSFile(certPath, policy, true)
	if err != nil {
		return nil, nil, fmt.Errorf("read public TLS certificate: %w", err)
	}
	keyPEM, err := readOperatorTLSFile(keyPath, policy, false)
	if err != nil {
		return nil, nil, fmt.Errorf("read public TLS private key: %w", err)
	}
	return certPEM, keyPEM, nil
}

type operatorTLSSecurity struct {
	owners  []*windows.SID
	service *windows.SID
}

func newOperatorTLSSecurity() (*operatorTLSSecurity, error) {
	system, err := windows.StringToSid("S-1-5-18")
	if err != nil {
		return nil, fmt.Errorf("create SYSTEM SID: %w", err)
	}
	administrators, err := windows.StringToSid("S-1-5-32-544")
	if err != nil {
		return nil, fmt.Errorf("create Administrators SID: %w", err)
	}
	service, err := serviceSID(xtunnelServerServiceName)
	if err != nil {
		return nil, err
	}
	return &operatorTLSSecurity{
		owners:  []*windows.SID{system, administrators},
		service: service,
	}, nil
}

func readOperatorTLSFile(path string, policy *operatorTLSSecurity, certificate bool) (result []byte, resultErr error) {
	if err := validateOperatorTLSPath(path); err != nil {
		return nil, err
	}
	if policy == nil {
		return nil, errors.New("public TLS security policy is nil")
	}
	directory, name := filepath.Dir(path), filepath.Base(path)
	parents, err := openOperatorTLSParents(directory, policy)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, closeHandles(parents)) }()

	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("encode public TLS file path: %w", err)
	}
	handle, err := windows.CreateFile(
		pointer,
		windows.FILE_READ_DATA|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open public TLS file %q without following reparse points: %w", name, err)
	}
	file := os.NewFile(uintptr(handle), name)
	if file == nil {
		windows.CloseHandle(handle)
		return nil, errors.New("wrap public TLS file handle")
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	if err := validateOperatorTLSFile(handle, policy, certificate); err != nil {
		return nil, err
	}
	content, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("read public TLS file: %w", err)
	}
	return content, nil
}

func openOperatorTLSParents(directory string, policy *operatorTLSSecurity) ([]windows.Handle, error) {
	volume := filepath.VolumeName(directory)
	root := volume + `\`
	relative := strings.TrimPrefix(filepath.Clean(directory), root)
	components := strings.Split(relative, `\`)
	current := root
	handles := make([]windows.Handle, 0, len(components))
	fail := func(err error) ([]windows.Handle, error) {
		return nil, errors.Join(err, closeHandles(handles))
	}
	for index, component := range components {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		pointer, err := windows.UTF16PtrFromString(current)
		if err != nil {
			return fail(fmt.Errorf("encode public TLS path component: %w", err))
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
			return fail(fmt.Errorf("open public TLS path component without following reparse points: %w", err))
		}
		handles = append(handles, handle)
		var information windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
			return fail(fmt.Errorf("inspect public TLS path component: %w", err))
		}
		if information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			return fail(errors.New("public TLS path component is not a non-reparse directory"))
		}
		if index == len(components)-1 {
			if err := validateOperatorTLSSecurity(handle, policy, false, true); err != nil {
				return fail(fmt.Errorf("validate public TLS parent directory: %w", err))
			}
		}
	}
	return handles, nil
}

func validateOperatorTLSFile(handle windows.Handle, policy *operatorTLSSecurity, certificate bool) error {
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return fmt.Errorf("inspect public TLS file: %w", err)
	}
	if information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 || (information.FileAttributes&windows.FILE_ATTRIBUTE_DEVICE) != 0 || information.VolumeSerialNumber == 0 || information.FileIndexHigh == 0 && information.FileIndexLow == 0 {
		return errors.New("public TLS file is not a stable regular local-volume file")
	}
	if err := validateOperatorTLSFileIdentity(handle); err != nil {
		return err
	}
	return validateOperatorTLSSecurity(handle, policy, !certificate, false)
}

func validateOperatorTLSFileIdentity(handle windows.Handle) error {
	var information operatorTLSFileIDInfo
	if err := windows.GetFileInformationByHandleEx(
		handle,
		windows.FileIdInfo,
		(*byte)(unsafe.Pointer(&information)),
		uint32(unsafe.Sizeof(information)),
	); err != nil {
		return fmt.Errorf("read public TLS file identity: %w", err)
	}
	if information.volume == 0 {
		return errors.New("public TLS file has an invalid volume identity")
	}
	for _, value := range information.file {
		if value != 0 {
			return nil
		}
	}
	return errors.New("public TLS file has an invalid file identity")
}

type operatorTLSFileIDInfo struct {
	volume uint64
	file   [16]byte
}

func validateOperatorTLSSecurity(handle windows.Handle, policy *operatorTLSSecurity, privateKey, directory bool) error {
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read public TLS security descriptor: %w", err)
	}
	return validateOperatorTLSDescriptor(descriptor, policy, privateKey, directory)
}

func validateOperatorTLSDescriptor(descriptor *windows.SECURITY_DESCRIPTOR, policy *operatorTLSSecurity, privateKey, directory bool) error {
	if descriptor == nil || policy == nil || policy.service == nil {
		return errors.New("public TLS security descriptor is nil")
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return fmt.Errorf("read public TLS owner: %w", err)
	}
	ownerAllowed := false
	for _, allowed := range policy.owners {
		if owner != nil && allowed != nil && owner.Equals(allowed) {
			ownerAllowed = true
			break
		}
	}
	if !ownerAllowed {
		return errors.New("public TLS owner is not SYSTEM or Administrators")
	}
	entries, err := operatorTLSACEs(descriptor)
	if err != nil {
		return fmt.Errorf("read public TLS DACL: %w", err)
	}
	serviceReadable := false
	for _, entry := range entries {
		if entry.flags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		if entry.typeID != windows.ACCESS_ALLOWED_ACE_TYPE {
			return errors.New("public TLS DACL contains an ACE whose effective access cannot be proven")
		}
		if entry.sid == nil {
			return errors.New("public TLS DACL contains an ACE without a SID")
		}
		if sidIn(entry.sid, policy.owners) {
			continue
		}
		if entry.mask&operatorTLSDangerousAccess(directory) != 0 {
			return errors.New("public TLS DACL grants modification, deletion, or security-control access outside its trusted owner")
		}
		serviceSID := entry.sid.Equals(policy.service)
		if privateKey && !serviceSID && entry.mask&operatorTLSReadAccess() != 0 {
			return errors.New("public TLS private key DACL grants read access outside its trusted owner or Service SID")
		}
		if serviceSID && entry.mask&operatorTLSReadAccess() != 0 {
			serviceReadable = true
		}
	}
	if !serviceReadable {
		return errors.New("public TLS DACL does not grant the XTunnelServer Service SID read access")
	}
	return nil
}

type operatorTLSACE struct {
	typeID uint8
	flags  uint8
	mask   windows.ACCESS_MASK
	sid    *windows.SID
}

func operatorTLSACEs(descriptor *windows.SECURITY_DESCRIPTOR) ([]operatorTLSACE, error) {
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return nil, err
	}
	if dacl == nil {
		return nil, errors.New("DACL is absent")
	}
	entries := make([]operatorTLSACE, 0, dacl.AceCount)
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return nil, fmt.Errorf("read DACL ACE %d: %w", index, err)
		}
		entries = append(entries, operatorTLSACE{
			typeID: ace.Header.AceType,
			flags:  ace.Header.AceFlags,
			mask:   ace.Mask,
			sid:    (*windows.SID)(unsafe.Pointer(&ace.SidStart)),
		})
	}
	return entries, nil
}

func sidIn(candidate *windows.SID, allowed []*windows.SID) bool {
	for _, value := range allowed {
		if candidate != nil && value != nil && candidate.Equals(value) {
			return true
		}
	}
	return false
}

func operatorTLSReadAccess() windows.ACCESS_MASK {
	return windows.FILE_READ_DATA | windows.GENERIC_READ | windows.GENERIC_ALL | windows.MAXIMUM_ALLOWED
}

func operatorTLSDangerousAccess(directory bool) windows.ACCESS_MASK {
	mask := windows.ACCESS_MASK(windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_EA |
		windows.FILE_WRITE_ATTRIBUTES | windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER |
		windows.GENERIC_WRITE | windows.GENERIC_ALL | windows.MAXIMUM_ALLOWED)
	if directory {
		// 目录上的 FILE_DELETE_CHILD 允许删除不带 DELETE 权限的子文件。
		mask |= fileDeleteChild
	}
	return mask
}

func validateOperatorTLSPath(path string) error {
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("public TLS path must be absolute")
	}
	volume := filepath.VolumeName(path)
	if len(volume) != 2 || volume[1] != ':' {
		return errors.New("public TLS path must be on a local drive volume")
	}
	root := volume + `\`
	pointer, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return fmt.Errorf("encode public TLS volume root: %w", err)
	}
	if err := validateOperatorTLSDriveType(windows.GetDriveType(pointer)); err != nil {
		return err
	}
	canonical := filepath.Clean(path)
	if strings.HasPrefix(canonical, `\\`) || strings.HasPrefix(canonical, `\??\`) || strings.Contains(path[len(volume):], ":") {
		return errors.New("public TLS path must not use UNC, device namespace, or alternate data stream")
	}
	for _, component := range strings.FieldsFunc(path[len(volume):], func(character rune) bool { return character == '\\' || character == '/' }) {
		if component == "." || component == ".." {
			return errors.New("public TLS path must not contain dot components")
		}
	}
	if strings.EqualFold(filepath.Clean(filepath.Dir(path)), root) {
		return errors.New("public TLS file must not be directly beneath a volume root")
	}
	return nil
}

func validateOperatorTLSDriveType(driveType uint32) error {
	if driveType != windows.DRIVE_FIXED {
		return errors.New("public TLS path must be on a fixed local drive")
	}
	return nil
}

func serviceSID(name string) (*windows.SID, error) {
	if name == "" {
		return nil, errors.New("service name is empty")
	}
	encoded := utf16.Encode([]rune(strings.ToUpper(name)))
	bytes := make([]byte, len(encoded)*2)
	for index, value := range encoded {
		binary.LittleEndian.PutUint16(bytes[index*2:], value)
	}
	digest := sha1.Sum(bytes)
	parts := make([]any, 5)
	for index := range parts {
		parts[index] = binary.LittleEndian.Uint32(digest[index*4:])
	}
	return windows.StringToSid(fmt.Sprintf("S-1-5-80-%d-%d-%d-%d-%d", parts...))
}

func closeHandles(handles []windows.Handle) error {
	var closeErr error
	for index := len(handles) - 1; index >= 0; index-- {
		closeErr = errors.Join(closeErr, windows.CloseHandle(handles[index]))
	}
	return closeErr
}
