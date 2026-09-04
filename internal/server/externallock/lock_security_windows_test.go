//go:build windows

package externallock

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/lifei6671/xtunnel/internal/server/winsecurity"
)

func TestWindowsAcquireRejectsRuntimeDACLDriftWithoutMutation(t *testing.T) {
	for _, protected := range []bool{true, false} {
		name := "inherited"
		if protected {
			name = "broad protected"
		}
		t.Run(name, func(t *testing.T) {
			runtimeDir := newWindowsRuntimeDir(t)
			setWindowsLockTestDACL(t, runtimeDir, protected)
			before := windowsLockTestSecurity(t, runtimeDir)
			if lock, err := Acquire(runtimeDir, windowsTestTargetHash); err == nil {
				_ = lock.Close()
				t.Fatal("Acquire(unsafe runtime DACL) succeeded")
			}
			if got := windowsLockTestSecurity(t, runtimeDir); got != before {
				t.Fatalf("runtime security changed: got %q, want %q", got, before)
			}
			entries, err := os.ReadDir(runtimeDir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("rejected runtime must remain empty: entries = %v, error = %v", entries, err)
			}
		})
	}
}

func TestWindowsAcquireRejectsExistingLockDACLWithoutMutation(t *testing.T) {
	for _, protected := range []bool{true, false} {
		name := "inherited"
		if protected {
			name = "broad protected"
		}
		t.Run(name, func(t *testing.T) {
			runtimeDir := newWindowsRuntimeDir(t)
			path := filepath.Join(runtimeDir, "server-lock-"+windowsTestTargetHash+".lock")
			// 管理员令牌的默认文件 Owner 可能是 Administrators。
			// 先经正式入口固定正确 Owner，再仅改变 DACL，确保命中目标拒绝分支。
			lock, err := Acquire(runtimeDir, windowsTestTargetHash)
			if err != nil {
				t.Fatal(err)
			}
			if err := lock.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("existing lock contents"), 0o600); err != nil {
				t.Fatal(err)
			}
			setWindowsLockTestDACL(t, path, protected)
			assertWindowsLockRejectedWithoutMutation(t, runtimeDir, path, "DACL")
		})
	}
}

func TestWindowsAcquireCreatesProtectedLockAndReusesIdentity(t *testing.T) {
	runtimeDir := newWindowsRuntimeDir(t)
	path := filepath.Join(runtimeDir, "server-lock-"+windowsTestTargetHash+".lock")
	lock, err := Acquire(runtimeDir, windowsTestTargetHash)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	// 通过正式文件读取入口验证新锁的 Owner 与精确 DACL；再用对象身份确认重开没有替换锁。
	if _, err := winsecurity.ReadForegroundFile(runtimeDir, filepath.Base(path)); err != nil {
		t.Fatalf("new lock security validation failed: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeSecurity := windowsLockTestSecurity(t, path)
	lock, err = Acquire(runtimeDir, windowsTestTargetHash)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("reacquiring changed lock file identity")
	}
	if got := windowsLockTestSecurity(t, path); got != beforeSecurity {
		t.Fatalf("reacquiring changed lock security: got %q, want %q", got, beforeSecurity)
	}
}

func TestWindowsAcquireRejectsOwnerMismatchWithoutMutation(t *testing.T) {
	for _, target := range []string{"runtime", "lock"} {
		t.Run(target, func(t *testing.T) {
			runtimeDir := newWindowsRuntimeDir(t)
			path := runtimeDir
			if target == "lock" {
				lock, err := Acquire(runtimeDir, windowsTestTargetHash)
				if err != nil {
					t.Fatal(err)
				}
				if err := lock.Close(); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(runtimeDir, "server-lock-"+windowsTestTargetHash+".lock")
			}
			owner, err := windows.StringToSid("S-1-5-32-544")
			if err != nil {
				t.Fatal(err)
			}
			// 仅使用当前令牌已有权限；普通用户无法分配其他 Owner 时明确记录覆盖缺口。
			if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, owner, nil, nil, nil); err != nil {
				if errors.Is(err, windows.ERROR_INVALID_OWNER) || errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
					t.Skipf("current token cannot assign Administrators owner without extra privileges: %v", err)
				}
				t.Fatal(err)
			}
			assertWindowsLockRejectedWithoutMutation(t, runtimeDir, path, "owner")
		})
	}
}

func setWindowsLockTestDACL(t *testing.T, path string, protected bool) {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	information := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	if protected {
		information = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, information, nil, nil, dacl, nil); err != nil {
		t.Fatalf("set temporary object DACL: %v", err)
	}
}

func windowsLockTestSecurity(t *testing.T, path string) string {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor.String()
}

func assertWindowsLockRejectedWithoutMutation(t *testing.T, runtimeDir, path, reason string) {
	t.Helper()
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeSecurity := windowsLockTestSecurity(t, path)
	var beforeContent []byte
	if !before.IsDir() {
		beforeContent, err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	lock, err := Acquire(runtimeDir, windowsTestTargetHash)
	if err == nil {
		_ = lock.Close()
		t.Fatalf("Acquire(%s mismatch) succeeded", reason)
	}
	if !strings.Contains(err.Error(), reason) {
		t.Fatalf("Acquire() error = %v, want %s rejection", err, reason)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("rejected acquisition replaced existing object")
	}
	if got := windowsLockTestSecurity(t, path); got != beforeSecurity {
		t.Fatalf("rejected acquisition changed security: got %q, want %q", got, beforeSecurity)
	}
	if !before.IsDir() {
		content, err := os.ReadFile(path)
		if err != nil || string(content) != string(beforeContent) {
			t.Fatalf("rejected acquisition changed content: got %q, want %q, error = %v", content, beforeContent, err)
		}
	} else if entries, err := os.ReadDir(path); err != nil || len(entries) != 0 {
		t.Fatalf("rejected runtime must remain empty: entries = %v, error = %v", entries, err)
	}
}
