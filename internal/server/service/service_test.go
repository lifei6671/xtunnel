package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddedUnitContract(t *testing.T) {
	content := strings.ReplaceAll(string(unitFile), "\r\n", "\n")
	checks := []string{
		ManagedUnitMarker + "\n",
		"ExecStart=/usr/local/bin/xtunnel-server --config /etc/xtunnel/server.yaml",
		"User=xtunnel-server",
		"Group=xtunnel-server",
		"StateDirectory=xtunnel",
		"LimitNOFILE=1048576",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Fatalf("embedded unit missing %q", check)
		}
	}
	if !strings.HasPrefix(content, ManagedUnitMarker+"\n") {
		t.Fatal("managed marker is not the first line")
	}
}

func TestParseSystemdVersion(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int
		wantErr bool
	}{
		{name: "plain", input: "249\n", want: 249},
		{name: "version suffix", input: "252 (252.38-1~deb12u1)", want: 252},
		{name: "systemd prefix", input: "systemd 257", want: 257},
		{name: "missing", input: "", wantErr: true},
		{name: "nonnumeric", input: "unknown", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseSystemdVersion(test.input)
			if test.wantErr {
				if err == nil {
					t.Fatalf("parseSystemdVersion() = %d, want error", got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("parseSystemdVersion() = %d, %v; want %d, nil", got, err, test.want)
			}
		})
	}
}

func TestValidateServicePlatform(t *testing.T) {
	tests := []struct {
		name    string
		goos    string
		goarch  string
		wantErr string
	}{
		{name: "linux amd64", goos: "linux", goarch: "amd64"},
		{name: "linux arm64", goos: "linux", goarch: "arm64"},
		{name: "windows amd64", goos: "windows", goarch: "amd64"},
		{name: "windows arm64 runtime unsupported", goos: "windows", goarch: "arm64", wantErr: ErrUnsupported.Error()},
		{name: "linux unsupported architecture", goos: "linux", goarch: "386", wantErr: "architecture 386"},
		{name: "unsupported operating system", goos: "darwin", goarch: "arm64", wantErr: ErrUnsupported.Error()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateServicePlatform(test.goos, test.goarch)
			if test.wantErr == "" && err != nil {
				t.Fatalf("validateServicePlatform() error = %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("validateServicePlatform() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestInspectOwnedUnitAcceptsOnlyManagedOrExactLegacyUnit(t *testing.T) {
	directory := t.TempDir()
	tests := []struct {
		name       string
		kind       string
		content    []byte
		wantExists bool
		wantOwned  bool
	}{
		{name: "missing"},
		{name: "managed", kind: "file", content: unitFile, wantExists: true, wantOwned: true},
		{name: "managed crlf", kind: "file", content: []byte(strings.ReplaceAll(string(unitFile), "\n", "\r\n")), wantExists: true, wantOwned: true},
		{name: "exact legacy", kind: "file", content: legacyUnitFile, wantExists: true, wantOwned: true},
		{name: "modified legacy", kind: "file", content: append(append([]byte{}, legacyUnitFile...), []byte("# local change\n")...), wantExists: true},
		{name: "marker suffix", kind: "file", content: []byte(ManagedUnitMarker + " forged\n[Unit]\n"), wantExists: true},
		{name: "marker only", kind: "file", content: []byte(ManagedUnitMarker), wantExists: true},
		{name: "foreign", kind: "file", content: []byte("[Unit]\nDescription=foreign\n"), wantExists: true},
		{name: "directory", kind: "directory", wantExists: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(directory, strings.ReplaceAll(test.name, " ", "-")+".service")
			switch test.kind {
			case "file":
				if err := os.WriteFile(path, test.content, 0o644); err != nil {
					t.Fatalf("os.WriteFile() error = %v", err)
				}
			case "directory":
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatalf("os.Mkdir() error = %v", err)
				}
			}
			exists, owned, err := inspectOwnedUnit(path)
			if err != nil || exists != test.wantExists || owned != test.wantOwned {
				t.Fatalf("inspectOwnedUnit() = (%v, %v, %v), want exists=%v owned=%v", exists, owned, err, test.wantExists, test.wantOwned)
			}
		})
	}
}

func TestAtomicWriteBytes(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "server.yaml")
	if err := atomicWriteBytes(path, []byte("first"), 0o640, -1, -1); err != nil {
		t.Fatalf("atomicWriteBytes() error = %v", err)
	}
	if err := atomicWriteBytes(path, []byte("second"), 0o640, -1, -1); err != nil {
		t.Fatalf("atomicWriteBytes(replace) error = %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	if string(content) != "second" {
		t.Fatalf("content = %q, want second", content)
	}
}
