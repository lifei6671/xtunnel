//go:build windows

package winsecurity

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// 以当前非提升用户代替 Service SID，保留 Modify/OW_RC 的同构内核权限。
// 夹具只在临时目录内创建，不改变生产策略的路径、Owner 或 SID 选择。
func TestServiceInheritedCreationWithModifyOnly(t *testing.T) {
	if windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("requires non-elevated token so BA cannot mask missing rights")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid
	parentSD := "O:" + sid.String() + "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1301bf;;;" + sid.String() + ")(A;OICI;RC;;;OW)"
	descriptor, err := windows.SecurityDescriptorFromString(parentSD)
	if err != nil {
		t.Fatal(err)
	}
	parentPolicy := &ForegroundDirectorySecurity{descriptor: descriptor, owner: sid}
	parent := filepath.Join(t.TempDir(), "protected")
	pointer, _ := windows.UTF16PtrFromString(parent)
	if err := windows.CreateDirectory(pointer, parentPolicy.Attributes()); err != nil {
		t.Fatal(err)
	}
	runtime.KeepAlive(parentPolicy)
	parentHandle, err := openForegroundDirectoryNoFollow(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(parentHandle)
	childSD, err := windows.SecurityDescriptorFromString(strings.Replace(parentSD, "D:P", "D:", 1))
	if err != nil {
		t.Fatal(err)
	}
	childExpected, err := descriptorACEs(childSD)
	if err != nil {
		t.Fatal(err)
	}
	for index := range childExpected {
		childExpected[index].flags |= windows.INHERITED_ACE
	}
	childPolicy := &ForegroundDirectorySecurity{descriptor: childSD, owner: sid, serviceOwners: []*windows.SID{sid}, expected: childExpected, inherited: true}
	child := filepath.Join(parent, "child")
	pointer, _ = windows.UTF16PtrFromString(child)
	if err := windows.CreateDirectory(pointer, parentPolicy.Attributes()); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("explicit SD bypassed Owner Rights: %v", err)
	}
	if childPolicy.Attributes() != nil {
		t.Fatal("runtime directory supplied explicit SD")
	}
	if err := windows.CreateDirectory(pointer, childPolicy.Attributes()); err != nil {
		t.Fatal(err)
	}
	childHandle, err := openForegroundDirectoryNoFollow(child)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(childHandle)
	if err := childPolicy.ValidateDirectory(childHandle); err != nil {
		t.Fatal(err)
	}
	fileSD, err := windows.SecurityDescriptorFromString(strings.ReplaceAll(strings.Replace(parentSD, "D:P", "D:", 1), "OICI", "ID"))
	if err != nil {
		t.Fatal(err)
	}
	fileExpected, err := descriptorACEs(fileSD)
	if err != nil {
		t.Fatal(err)
	}
	filePolicy := &ForegroundFileSecurity{descriptor: fileSD, owner: sid, serviceOwners: []*windows.SID{sid}, expected: fileExpected, inherited: true}
	if filePolicy.Attributes() != nil {
		t.Fatal("runtime file supplied explicit SD")
	}
	path := filepath.Join(child, "proof")
	file, err := createSecuredFile(path, filePolicy)
	if err != nil {
		t.Fatal(err)
	}
	if err := filePolicy.ValidateFile(windows.Handle(file.Fd())); err != nil {
		file.Close()
		t.Fatal(err)
	}
	before, err := foregroundFileID(windows.Handle(file.Fd()))
	if err != nil {
		file.Close()
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("verified-before-write")); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	handle, err := openFileNoFollow(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := filePolicy.ValidateFile(handle); err != nil {
		windows.CloseHandle(handle)
		t.Fatal(err)
	}
	after, err := foregroundFileID(handle)
	if err := errors.Join(err, windows.CloseHandle(handle)); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("reopened identity changed")
	}
	for _, target := range []string{parent, child, path} {
		pointer, _ := windows.UTF16PtrFromString(target)
		for _, access := range []uint32{windows.WRITE_DAC, windows.WRITE_OWNER} {
			handle, err := windows.CreateFile(pointer, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
			if err == nil {
				windows.CloseHandle(handle)
				t.Fatalf("security control 0x%x accepted for %s", access, filepath.Base(target))
			}
			if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				t.Fatalf("unexpected security-control failure: %v", err)
			}
		}
	}
	if err := os.Rename(parent, parent+"-moved"); err == nil {
		t.Fatal("pinned parent was replaced")
	}
	// 子对象即使 Owner 正确且无 Protected 位，额外继承的读取 ACE 也必须拒绝。
	driftSD, err := windows.SecurityDescriptorFromString(parentSD + "(A;OICI;FR;;;BU)")
	if err != nil {
		t.Fatal(err)
	}
	driftPolicy := &ForegroundDirectorySecurity{descriptor: driftSD, owner: sid}
	driftParent := filepath.Join(t.TempDir(), "drift")
	pointer, _ = windows.UTF16PtrFromString(driftParent)
	if err := windows.CreateDirectory(pointer, driftPolicy.Attributes()); err != nil {
		t.Fatal(err)
	}
	runtime.KeepAlive(driftPolicy)
	driftFile, err := createSecuredFile(filepath.Join(driftParent, "proof"), filePolicy)
	if err != nil {
		t.Fatal(err)
	}
	defer driftFile.Close()
	driftHandle := windows.Handle(driftFile.Fd())
	beforeSD, err := windows.GetSecurityInfo(driftHandle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if err := filePolicy.ValidateFile(driftHandle); err == nil || !strings.Contains(err.Error(), "DACL") {
		t.Fatalf("extra inherited ACE accepted: %v", err)
	}
	afterSD, err := windows.GetSecurityInfo(driftHandle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil || beforeSD.String() != afterSD.String() {
		t.Fatalf("drift validation modified descriptor: %v", err)
	}
}

func TestInheritedPolicyRejectsUnboundPathsAndDescriptorDrift(t *testing.T) {
	security, err := NewServiceFileSecurity(ServiceData)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := PublishForegroundFile(directory, "proof", []byte("must-not-write"), security); err == nil {
		t.Fatal("service policy accepted directory outside fixed roots")
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("rejected publication changed directory: %v %v", entries, err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	// 测试使用正常前台受管文件作为“受保护但没有继承来源”的负向对象。
	foreground, err := NewForegroundFileSecurity()
	if err != nil {
		t.Fatal(err)
	}
	file, err := createSecuredFile(filepath.Join(directory, "protected"), foreground)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	security.owner = user.User.Sid
	security.serviceOwners = []*windows.SID{user.User.Sid}
	before, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if err := security.ValidateFile(windows.Handle(file.Fd())); err == nil {
		t.Fatal("inherited policy accepted protected descriptor")
	}
	after, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil || before.String() != after.String() {
		t.Fatalf("rejection repaired descriptor: %v", err)
	}
}
