//go:build windows

// Package winsecurity implements the Windows security descriptor rules for
// Server-managed foreground files. Callers must still open filesystem objects
// with FILE_FLAG_OPEN_REPARSE_POINT before asking this package to validate them.
package winsecurity

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ForegroundDirectorySecurity binds the current interactive user's SID to the
// protected ACL used for directories managed by the foreground Server profile.
// The descriptor remains owned by this value so its memory stays alive while a
// Windows create call consumes SecurityAttributes.
type ForegroundDirectorySecurity struct {
	descriptor *windows.SECURITY_DESCRIPTOR
	owner      *windows.SID
	expected   []accessACE
}

type accessACE struct {
	typeID uint8
	flags  uint8
	mask   windows.ACCESS_MASK
	sid    *windows.SID
}

// NewForegroundDirectorySecurity returns the exact protected directory ACL:
// SYSTEM and the current interactive user each have Full Control, including
// child files and directories. No inherited or broad principal is accepted.
func NewForegroundDirectorySecurity() (*ForegroundDirectorySecurity, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("read current Windows user SID: %w", err)
	}
	owner, err := windows.StringToSid(user.User.Sid.String())
	if err != nil {
		return nil, fmt.Errorf("copy current Windows user SID: %w", err)
	}
	descriptor, err := windows.SecurityDescriptorFromString(
		"O:" + owner.String() + "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;" + owner.String() + ")",
	)
	if err != nil {
		return nil, fmt.Errorf("create foreground directory security descriptor: %w", err)
	}
	expected, err := descriptorACEs(descriptor)
	if err != nil {
		return nil, fmt.Errorf("inspect foreground directory security descriptor: %w", err)
	}
	return &ForegroundDirectorySecurity{descriptor: descriptor, owner: owner, expected: expected}, nil
}

// Attributes returns descriptor-backed creation attributes. The receiver must
// stay reachable until CreateDirectory returns.
func (security *ForegroundDirectorySecurity) Attributes() *windows.SecurityAttributes {
	return &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: security.descriptor,
	}
}

// ValidateDirectory verifies a no-follow directory handle has the exact owner
// and protected DACL used by the foreground profile. It never repairs a
// pre-existing object because that would silently take over another boundary.
func (security *ForegroundDirectorySecurity) ValidateDirectory(handle windows.Handle) error {
	if security == nil || security.descriptor == nil || security.owner == nil {
		return errors.New("foreground directory security is uninitialized")
	}
	descriptor, err := windows.GetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return fmt.Errorf("read directory security descriptor: %w", err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return fmt.Errorf("read directory owner: %w", err)
	}
	if owner == nil || !owner.Equals(security.owner) {
		return errors.New("directory owner does not match the current Windows user")
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return fmt.Errorf("read directory security descriptor control: %w", err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("directory DACL is not protected")
	}
	actual, err := descriptorACEs(descriptor)
	if err != nil {
		return fmt.Errorf("read directory DACL: %w", err)
	}
	if !sameACEs(actual, security.expected) {
		return errors.New("directory DACL does not match the foreground profile")
	}
	runtime.KeepAlive(security)
	return nil
}

func descriptorACEs(descriptor *windows.SECURITY_DESCRIPTOR) ([]accessACE, error) {
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return nil, err
	}
	if dacl == nil {
		return nil, errors.New("DACL is absent")
	}
	entries := make([]accessACE, 0, dacl.AceCount)
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		err := windows.GetAce(dacl, index, &ace)
		if err != nil {
			return nil, fmt.Errorf("read DACL ACE %d: %w", index, err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return nil, fmt.Errorf("DACL ACE %d is not an allow ACE", index)
		}
		entries = append(entries, accessACE{
			typeID: ace.Header.AceType,
			flags:  ace.Header.AceFlags,
			mask:   ace.Mask,
			sid:    (*windows.SID)(unsafe.Pointer(&ace.SidStart)),
		})
	}
	return entries, nil
}

func sameACEs(actual, expected []accessACE) bool {
	if len(actual) != len(expected) {
		return false
	}
	matched := make([]bool, len(expected))
	for _, candidate := range actual {
		found := false
		for index, wanted := range expected {
			if !matched[index] && candidate.typeID == wanted.typeID && candidate.flags == wanted.flags &&
				candidate.mask == wanted.mask && candidate.sid != nil && wanted.sid != nil && candidate.sid.Equals(wanted.sid) {
				matched[index] = true
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
