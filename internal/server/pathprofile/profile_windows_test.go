//go:build windows

package pathprofile

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestResolveWindowsProfiles(t *testing.T) {
	localAppData, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		t.Fatalf("KnownFolderPath(LocalAppData) error = %v", err)
	}
	programData, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		t.Fatalf("KnownFolderPath(ProgramData) error = %v", err)
	}
	foregroundData := filepath.Join(localAppData, "XTunnel", "Server", "data")
	serviceData := filepath.Join(programData, "XTunnel", "Server", "data")

	for _, test := range []struct {
		name    string
		input   string
		data    string
		runtime string
	}{
		{
			name:    "automatic foreground",
			input:   AutomaticDataDir,
			data:    foregroundData,
			runtime: filepath.Join(localAppData, "XTunnel", "Server", "runtime"),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile, err := Resolve(test.input)
			if err != nil {
				t.Fatalf("Resolve(%q) error = %v", test.input, err)
			}
			if profile.DataDir != test.data || profile.RuntimeDir != test.runtime {
				t.Fatalf("Resolve(%q) = %#v, want data=%q runtime=%q", test.input, profile, test.data, test.runtime)
			}
		})
	}

	if _, err := Resolve(filepath.Join(t.TempDir(), "data")); err == nil {
		t.Fatal("Resolve(custom data directory) error = nil")
	}
	service, err := ResolveService(AutomaticDataDir)
	if err != nil {
		t.Fatalf("ResolveService(auto) error = %v", err)
	}
	if service.DataDir != serviceData || service.RuntimeDir != filepath.Join(programData, "XTunnel", "Server", "runtime") {
		t.Fatalf("ResolveService(auto) = %#v", service)
	}
}
