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

func TestOperatorTLSSecurityPolicyAcceptsOnlyDocumentedDescriptors(t *testing.T) {
	policy, err := newOperatorTLSSecurity()
	if err != nil {
		t.Fatalf("newOperatorTLSSecurity() error = %v", err)
	}
	service, err := serviceSID(xtunnelServerServiceName)
	if err != nil {
		t.Fatalf("serviceSID(XTunnelServer) error = %v", err)
	}
	validKey := "O:SYD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;GR;;;" + service.String() + ")"
	validCert := validKey + "(A;;GR;;;BU)"
	for _, test := range []struct {
		name     string
		sddl     string
		expected []accessACE
		wantErr  bool
	}{
		{name: "system owned private key", sddl: validKey, expected: policy.privateKey},
		{name: "administrators owned certificate", sddl: "O:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;GR;;;" + service.String() + ")(A;;GR;;;BU)", expected: policy.certificate},
		{name: "private key users read", sddl: validCert, expected: policy.privateKey, wantErr: true},
		{name: "unknown owner", sddl: "O:BUD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;GR;;;" + service.String() + ")", expected: policy.privateKey, wantErr: true},
		{name: "inherited dacl", sddl: "O:SYD:(A;;FA;;;SY)(A;;FA;;;BA)(A;;GR;;;" + service.String() + ")", expected: policy.privateKey, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			descriptor, err := windows.SecurityDescriptorFromString(test.sddl)
			if err != nil {
				t.Fatalf("SecurityDescriptorFromString() error = %v", err)
			}
			err = validateOperatorTLSDescriptor(descriptor, policy.owners, test.expected)
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
