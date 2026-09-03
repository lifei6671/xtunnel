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

	"golang.org/x/sys/windows"
)

const xtunnelServerServiceName = "XTunnelServer"

// ReadOperatorTLSFiles reads an external public-TLS pair without modifying it.
// The M8 Windows policy deliberately accepts only a small, auditable ACL: the
// operator retains SYSTEM/Administrators ownership, XTunnelServer has read
// access, and the certificate may additionally be readable by Builtin Users.
// Unknown, inherited, or deny ACEs fail closed instead of being interpreted as
// an effective-access calculation that could miss group or parent-directory
// privileges.
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
	owners      []*windows.SID
	parentACEs  []accessACE
	certificate []accessACE
	privateKey  []accessACE
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
	parent, err := externalTLSACEs(service, false)
	if err != nil {
		return nil, err
	}
	certificate, err := externalTLSACEs(service, true)
	if err != nil {
		return nil, err
	}
	return &operatorTLSSecurity{
		owners:      []*windows.SID{system, administrators},
		parentACEs:  parent,
		certificate: certificate,
		privateKey:  parent,
	}, nil
}

func externalTLSACEs(service *windows.SID, certificate bool) ([]accessACE, error) {
	if service == nil {
		return nil, errors.New("XTunnelServer service SID is nil")
	}
	sddl := "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;GR;;;" + service.String() + ")"
	if certificate {
		sddl += "(A;;GR;;;BU)"
	}
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, fmt.Errorf("create public TLS security descriptor: %w", err)
	}
	return descriptorACEs(descriptor)
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
			if err := validateOperatorTLSSecurity(handle, policy.owners, policy.parentACEs); err != nil {
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
	expected := policy.privateKey
	if certificate {
		expected = policy.certificate
	}
	return validateOperatorTLSSecurity(handle, policy.owners, expected)
}

func validateOperatorTLSSecurity(handle windows.Handle, owners []*windows.SID, expected []accessACE) error {
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read public TLS security descriptor: %w", err)
	}
	return validateOperatorTLSDescriptor(descriptor, owners, expected)
}

func validateOperatorTLSDescriptor(descriptor *windows.SECURITY_DESCRIPTOR, owners []*windows.SID, expected []accessACE) error {
	if descriptor == nil {
		return errors.New("public TLS security descriptor is nil")
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return fmt.Errorf("read public TLS owner: %w", err)
	}
	ownerAllowed := false
	for _, allowed := range owners {
		if owner != nil && allowed != nil && owner.Equals(allowed) {
			ownerAllowed = true
			break
		}
	}
	if !ownerAllowed {
		return errors.New("public TLS owner is not SYSTEM or Administrators")
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return fmt.Errorf("read public TLS security descriptor control: %w", err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("public TLS DACL is not protected")
	}
	actual, err := descriptorACEs(descriptor)
	if err != nil {
		return fmt.Errorf("read public TLS DACL: %w", err)
	}
	if !sameACEs(actual, expected) {
		return errors.New("public TLS DACL does not match the strict external-file policy")
	}
	return nil
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
