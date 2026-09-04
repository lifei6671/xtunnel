//go:build windows

package winsecurity

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestServiceSIDUsesDocumentedServiceNameAlgorithm(t *testing.T) {
	sid, err := serviceSID("ALG")
	if err != nil {
		t.Fatalf("serviceSID(ALG) error = %v", err)
	}
	const want = "S-1-5-80-2387347252-3645287876-2469496166-3824418187-3586569773"
	if got := sid.String(); got != want {
		t.Fatalf("serviceSID(ALG) = %q, want %q", got, want)
	}
}

func TestOperatorTLSSecurityPolicyValidatesSecurityProperties(t *testing.T) {
	policy, err := newOperatorTLSSecurity()
	if err != nil {
		t.Fatalf("newOperatorTLSSecurity() error = %v", err)
	}
	service, err := serviceSID(xtunnelServerServiceName)
	if err != nil {
		t.Fatalf("serviceSID(XTunnelServer) error = %v", err)
	}
	validKey := "O:SYD:(A;;FA;;;SY)(A;;GR;;;BA)(A;;GR;;;" + service.String() + ")"
	validCert := validKey + "(A;;GR;;;BU)"
	for _, test := range []struct {
		name       string
		sddl       string
		privateKey bool
		directory  bool
		wantErr    bool
	}{
		{name: "system owned private key with split owner ACEs", sddl: validKey, privateKey: true},
		{name: "administrators owned certificate with users read", sddl: "O:BAD:(A;;FA;;;SY)(A;;GR;;;BA)(A;;GR;;;" + service.String() + ")(A;;GR;;;BU)"},
		{name: "certificate accepts inherited service read", sddl: "O:SYD:AI(A;ID;GR;;;" + service.String() + ")(A;;GR;;;BU)"},
		{name: "private key users read", sddl: validCert, privateKey: true, wantErr: true},
		{name: "untrusted file write", sddl: validCert + "(A;;GW;;;BU)", wantErr: true},
		{name: "service SID write", sddl: validKey + "(A;;GW;;;" + service.String() + ")", privateKey: true, wantErr: true},
		{name: "untrusted parent delete child", sddl: validCert + "(A;;0x40;;;BU)", directory: true, wantErr: true},
		{name: "service SID has no read", sddl: "O:SYD:(A;;FA;;;SY)(A;;GR;;;BU)", privateKey: true, wantErr: true},
		{name: "unknown owner", sddl: "O:BUD:(A;;FA;;;SY)(A;;GR;;;" + service.String() + ")", privateKey: true, wantErr: true},
		{name: "deny ACE cannot prove effective access", sddl: validKey + "(D;;GW;;;BU)", privateKey: true, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			descriptor, err := windows.SecurityDescriptorFromString(test.sddl)
			if err != nil {
				t.Fatalf("SecurityDescriptorFromString() error = %v", err)
			}
			err = validateOperatorTLSDescriptor(descriptor, policy, test.privateKey, test.directory)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateOperatorTLSDescriptor() error = %v, wantErr %t", err, test.wantErr)
			}
		})
	}
}

func TestReadOperatorTLSFilesRejectsUnmanagedFilesWithoutMutation(t *testing.T) {
	directory := t.TempDir()
	certPath := filepath.Join(directory, "public.crt")
	keyPath := filepath.Join(directory, "public.key")
	cert := []byte("certificate")
	key := []byte("private key")
	if err := os.WriteFile(certPath, cert, 0o600); err != nil {
		t.Fatalf("WriteFile(cert) error = %v", err)
	}
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		t.Fatalf("WriteFile(key) error = %v", err)
	}
	certBefore, keyBefore := sha256.Sum256(cert), sha256.Sum256(key)
	certInfoBefore, err := os.Stat(certPath)
	if err != nil {
		t.Fatalf("Stat(cert before rejection) error = %v", err)
	}
	keyInfoBefore, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("Stat(key before rejection) error = %v", err)
	}
	if _, _, err := ReadOperatorTLSFiles(certPath, keyPath); err == nil {
		t.Fatal("ReadOperatorTLSFiles() error = nil, want unmanaged parent rejection")
	}
	certAfter, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("ReadFile(cert after rejection) error = %v", err)
	}
	keyAfter, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("ReadFile(key after rejection) error = %v", err)
	}
	if sha256.Sum256(certAfter) != certBefore || sha256.Sum256(keyAfter) != keyBefore {
		t.Fatal("ReadOperatorTLSFiles() changed operator-owned TLS bytes")
	}
	certInfoAfter, err := os.Stat(certPath)
	if err != nil {
		t.Fatalf("Stat(cert after rejection) error = %v", err)
	}
	keyInfoAfter, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("Stat(key after rejection) error = %v", err)
	}
	if !os.SameFile(certInfoBefore, certInfoAfter) || !os.SameFile(keyInfoBefore, keyInfoAfter) {
		t.Fatal("ReadOperatorTLSFiles() replaced operator-owned TLS files")
	}
}

func TestValidateOperatorTLSPathRejectsUnsafeWindowsForms(t *testing.T) {
	volume := filepath.VolumeName(t.TempDir())
	for _, path := range []string{"", "relative\\key.pem", `\\server\share\key.pem`, `C:\certs\key.pem:alternate`, `C:\certs\..\key.pem`, volume + `\key.pem`} {
		t.Run(path, func(t *testing.T) {
			if err := validateOperatorTLSPath(path); err == nil {
				t.Fatalf("validateOperatorTLSPath(%q) error = nil", path)
			}
		})
	}
}

func TestValidateOperatorTLSDriveTypeRejectsNonFixedVolumes(t *testing.T) {
	if err := validateOperatorTLSDriveType(windows.DRIVE_FIXED); err != nil {
		t.Fatalf("validateOperatorTLSDriveType(DRIVE_FIXED) error = %v", err)
	}
	for _, driveType := range []uint32{windows.DRIVE_REMOTE, windows.DRIVE_REMOVABLE, windows.DRIVE_CDROM, windows.DRIVE_UNKNOWN} {
		if err := validateOperatorTLSDriveType(driveType); err == nil {
			t.Fatalf("validateOperatorTLSDriveType(%d) error = nil", driveType)
		}
	}
}
