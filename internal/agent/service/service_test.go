package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddedUnitContract(t *testing.T) {
	content := string(unitFile)
	checks := []string{
		ManagedUnitMarker + "\n",
		"LoadCredential=xtunnel-agent.token:/etc/xtunnel/credentials/agent.token",
		"ExecStart=/usr/local/bin/xtunnel-agent run",
		"User=xtunnel-agent",
		"Group=xtunnel-agent",
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

func TestValidateServiceArchitecture(t *testing.T) {
	tests := []struct {
		goarch  string
		wantErr bool
	}{
		{goarch: "amd64"},
		{goarch: "arm64"},
		{goarch: "386", wantErr: true},
		{goarch: "wasm", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.goarch, func(t *testing.T) {
			err := validateServiceArchitecture(test.goarch)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateServiceArchitecture(%q) error = %v, wantErr = %v", test.goarch, err, test.wantErr)
			}
		})
	}
}

func TestInspectManagedUnit(t *testing.T) {
	directory := t.TempDir()
	tests := []struct {
		name        string
		content     *string
		wantExists  bool
		wantManaged bool
	}{
		{name: "missing"},
		{name: "managed", content: stringPointer(ManagedUnitMarker + "\n[Unit]\n"), wantExists: true, wantManaged: true},
		{name: "unmanaged", content: stringPointer("[Unit]\nDescription=foreign\n"), wantExists: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(directory, strings.ReplaceAll(test.name, " ", "-")+".service")
			if test.content != nil {
				if err := os.WriteFile(path, []byte(*test.content), 0o644); err != nil {
					t.Fatalf("os.WriteFile() error = %v", err)
				}
			}
			exists, managed, err := inspectManagedUnit(path)
			if err != nil {
				t.Fatalf("inspectManagedUnit() error = %v", err)
			}
			if exists != test.wantExists || managed != test.wantManaged {
				t.Fatalf("inspectManagedUnit() = (%v, %v), want (%v, %v)", exists, managed, test.wantExists, test.wantManaged)
			}
		})
	}
}

func TestAtomicWriteBytes(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "agent.token")
	if err := atomicWriteBytes(path, []byte("xta_test_secret"), 0o600, -1, -1); err != nil {
		t.Fatalf("atomicWriteBytes() error = %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	if string(content) != "xta_test_secret" {
		t.Fatalf("file content = %q, want Token", content)
	}
	if err := atomicWriteBytes(path, []byte("xta_replaced_secret"), 0o600, -1, -1); err != nil {
		t.Fatalf("atomicWriteBytes(existing file) error = %v", err)
	}
	content, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile(replaced file) error = %v", err)
	}
	if string(content) != "xta_replaced_secret" {
		t.Fatalf("replaced file content = %q", content)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("os.ReadDir() error = %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "agent.token" {
		t.Fatalf("directory entries = %#v, want only published file", entries)
	}
}

func TestActivateManagedServiceCommandOrder(t *testing.T) {
	var commands []string
	run := func(_ context.Context, path string, args ...string) (string, error) {
		commands = append(commands, strings.Join(append([]string{path}, args...), " "))
		return "", nil
	}
	if err := activateManagedService(context.Background(), "/usr/bin/systemctl", run); err != nil {
		t.Fatalf("activateManagedService() error = %v", err)
	}
	want := []string{
		"/usr/bin/systemctl daemon-reload",
		"/usr/bin/systemctl enable xtunnel-agent.service",
		"/usr/bin/systemctl restart xtunnel-agent.service",
		"/usr/bin/systemctl is-active --quiet xtunnel-agent.service",
	}
	if strings.Join(commands, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands = %#v, want %#v", commands, want)
	}
}

func stringPointer(value string) *string {
	return &value
}
