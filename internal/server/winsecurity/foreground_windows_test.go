//go:build windows

package winsecurity

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestForegroundDirectorySecurityValidatesCreatedDirectory(t *testing.T) {
	security, err := NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	path := filepath.Join(t.TempDir(), "managed")
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatalf("UTF16PtrFromString() error = %v", err)
	}
	if err := windows.CreateDirectory(pointer, security.Attributes()); err != nil {
		t.Fatalf("CreateDirectory() error = %v", err)
	}
	handle := openDirectoryNoFollow(t, path)
	defer windows.CloseHandle(handle)
	if err := security.ValidateDirectory(handle); err != nil {
		t.Fatalf("ValidateDirectory() error = %v", err)
	}
}

func TestForegroundDirectorySecurityRejectsInheritedDirectory(t *testing.T) {
	security, err := NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	path := filepath.Join(t.TempDir(), "inherited")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	handle := openDirectoryNoFollow(t, path)
	defer windows.CloseHandle(handle)
	if err := security.ValidateDirectory(handle); err == nil {
		t.Fatal("ValidateDirectory() error = nil, want protected foreground DACL rejection")
	}
}

func openDirectoryNoFollow(t *testing.T, path string) windows.Handle {
	t.Helper()
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatalf("UTF16PtrFromString() error = %v", err)
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
		t.Fatalf("CreateFile(%q) error = %v", path, err)
	}
	return handle
}
