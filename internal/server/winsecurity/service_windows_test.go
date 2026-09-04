//go:build windows

package winsecurity

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestServiceDescriptorRightsAndOwners(t *testing.T) {
	service, err := serviceSID(xtunnelServerServiceName)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name               string
		kind               ServiceObjectKind
		directory, runtime bool
		access             windows.ACCESS_MASK
		count              int
	}{
		{"binary", ServiceBinary, false, false, windows.FILE_GENERIC_READ | windows.FILE_GENERIC_EXECUTE, 3},
		{"config", ServiceConfig, false, false, windows.FILE_GENERIC_READ, 3},
		{"config_directory", ServiceConfig, true, false, windows.FILE_GENERIC_READ | windows.FILE_GENERIC_EXECUTE, 3},
		{"data_root", ServiceData, true, false, 0x1301bf, 4},
		{"data_child", ServiceData, true, true, 0x1301bf, 4},
		{"data_file", ServiceData, false, true, 0x1301bf, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			descriptor, _, owners, entries, err := serviceDescriptor(tc.kind, tc.directory, tc.runtime)
			if err != nil {
				t.Fatal(err)
			}
			control, _, err := descriptor.Control()
			if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
				t.Fatalf("unprotected descriptor: control=%x err=%v", control, err)
			}
			if len(entries) != tc.count {
				t.Fatalf("ACE count=%d, want %d", len(entries), tc.count)
			}
			foundService, foundOwnerRights := false, false
			for _, ace := range entries {
				flags := uint8(0)
				if tc.directory {
					flags = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
				}
				if ace.flags != flags {
					t.Fatalf("inheritance flags=%x, want %x", ace.flags, flags)
				}
				switch ace.sid.String() {
				case "S-1-5-18", "S-1-5-32-544":
					if ace.mask != 0x1f01ff {
						t.Fatalf("trusted principal mask=%x", ace.mask)
					}
				case "S-1-3-4":
					foundOwnerRights = true
					if ace.mask != windows.READ_CONTROL {
						t.Fatalf("owner rights=%x", ace.mask)
					}
				default:
					if !ace.sid.Equals(service) || ace.mask != tc.access {
						t.Fatalf("unexpected service ACE SID=%s mask=%x", ace.sid.String(), ace.mask)
					}
					if ace.mask&(windows.WRITE_DAC|windows.WRITE_OWNER) != 0 {
						t.Fatal("service may rewrite security")
					}
					foundService = true
				}
			}
			if !foundService || foundOwnerRights != (tc.kind == ServiceData) {
				t.Fatalf("service=%v owner-rights=%v", foundService, foundOwnerRights)
			}
			localService, _ := windows.StringToSid("S-1-5-19")
			if sidIn(localService, owners) != tc.runtime {
				t.Fatalf("LocalService owner acceptance=%v, runtime=%v", sidIn(localService, owners), tc.runtime)
			}
		})
	}
}

func TestServiceSharedAncestorRejectsReparseWriteRights(t *testing.T) {
	system, err := windows.StringToSid("S-1-5-18")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, rights, flags, principal string
		osAncestor, wantError          bool
	}{
		{"shared_write_data", "0x2", "", "BU", false, true},
		{"shared_write_attributes", "0x100", "", "BU", false, true},
		{"shared_both", "0x102", "", "BU", false, true},
		{"shared_generic_write", "GW", "", "BU", false, true},
		{"shared_read_execute", "FRFX", "", "BU", false, false},
		{"shared_trusted_write", "0x102", "", "SY", false, false},
		{"shared_inherit_only", "0x102", "OICIIO", "BU", false, false},
		{"os_write_data", "0x2", "", "BU", true, false},
		{"os_write_attributes", "0x100", "", "BU", true, false},
		{"os_delete_child", "0x40", "", "BU", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			descriptor, err := windows.SecurityDescriptorFromString("O:SYD:P(A;" + tc.flags + ";" + tc.rights + ";;;" + tc.principal + ")")
			if err != nil {
				t.Fatal(err)
			}
			entries, err := operatorTLSACEs(descriptor)
			if err != nil {
				t.Fatal(err)
			}
			before := descriptor.String()
			err = validateServiceAncestorACEs(entries, []*windows.SID{system}, tc.osAncestor)
			if (err != nil) != tc.wantError {
				t.Fatalf("permission decision err=%v, wantError=%v", err, tc.wantError)
			}
			if descriptor.String() != before {
				t.Fatal("permission validation changed input descriptor")
			}
		})
	}
}

func TestServiceDataPathSelection(t *testing.T) {
	base, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(base, "XTunnel", "Server", "data")
	for _, tc := range []struct {
		path          string
		service, root bool
	}{
		{data, true, true}, {strings.ToUpper(data), true, true}, {filepath.Join(data, "credentials"), true, false},
		{filepath.Join(base, "XTunnel", "Server", "runtime"), true, true},
		{data + "-other", false, false}, {filepath.Join(base, "XTunnel", "Server"), false, false}, {t.TempDir(), false, false},
	} {
		service, root, err := serviceDataPath(tc.path)
		if err != nil || service != tc.service || root != tc.root {
			t.Fatalf("path=%q got=%v/%v/%v", tc.path, service, root, err)
		}
	}
	if _, _, err := serviceDataPath(data + `\..\data`); err == nil {
		t.Fatal("accepted dot-component service path")
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		user, err := windows.GetCurrentProcessToken().GetTokenUser()
		if err != nil {
			t.Fatal(err)
		}
		if user.User.Sid.String() != "S-1-5-18" && user.User.Sid.String() != "S-1-5-19" {
			if _, err := NewDirectorySecurityForPath(data); err == nil {
				t.Fatal("ordinary user selected service policy")
			}
			if _, err := NewFileSecurityForPath(data); err == nil {
				t.Fatal("ordinary user selected service file policy")
			}
		}
	}
}

func TestServiceCreateFileUsesInitialDescriptor(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("requires elevated token to assign Administrators owner")
	}
	directory := t.TempDir()
	security, err := NewServiceFileSecurity(ServiceData)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "runtime-key")
	file, err := createSecuredFile(path, security)
	if err != nil {
		t.Fatal(err)
	}
	if err := security.ValidateFile(windows.Handle(file.Fd())); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("managed-content")); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if replacement, err := createSecuredFile(path, security); err == nil {
		replacement.Close()
		t.Fatal("CREATE_NEW overwrote existing object")
	} else if !errors.Is(err, windows.ERROR_FILE_EXISTS) {
		t.Fatalf("unexpected collision: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "managed-content" {
		t.Fatalf("existing object changed: %q %v", content, err)
	}
	// 本测试只修改隔离临时文件；实际 LocalService/SQLite 的访问证据由 SCM Smoke 提供。
}

func TestInstallationPublicationRejectsCollisionAndCleansCandidate(t *testing.T) {
	directory := newManagedFileDirectory(t)
	security, err := NewForegroundFileSecurity()
	if err != nil {
		t.Fatal(err)
	}
	if err := publishFileCandidate(directory, "config", []byte("first"), security, false); err != nil {
		t.Fatal(err)
	}
	handle := openManagedFileNoFollow(t, filepath.Join(directory, "config"))
	before, err := foregroundFileID(handle)
	if closeErr := windows.CloseHandle(handle); err != nil || closeErr != nil {
		t.Fatal(errors.Join(err, closeErr))
	}
	if err := publishFileCandidate(directory, "config", []byte("replacement"), security, false); err == nil {
		t.Fatal("installation publication replaced existing config")
	}
	content, err := os.ReadFile(filepath.Join(directory, "config"))
	if err != nil || string(content) != "first" {
		t.Fatalf("existing config changed: %q %v", content, err)
	}
	handle = openManagedFileNoFollow(t, filepath.Join(directory, "config"))
	defer windows.CloseHandle(handle)
	after, err := foregroundFileID(handle)
	if err != nil || before != after {
		t.Fatalf("existing identity changed: %v", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 || entries[0].Name() != "config" {
		t.Fatalf("candidate cleanup: %v %v", entries, err)
	}
}
