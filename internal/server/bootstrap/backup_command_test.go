package bootstrap

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestParseBackupCommandOptionsRequiresAbsoluteArchivePath(t *testing.T) {
	absolute := filepath.Join(t.TempDir(), "backup.tar")
	tests := []struct {
		name      string
		operation string
		args      []string
		wantPath  string
		wantError bool
	}{
		{name: "create", operation: "create", args: []string{"--output", absolute}, wantPath: absolute},
		{name: "restore", operation: "restore", args: []string{"--input", absolute}, wantPath: absolute},
		{name: "stdout", operation: "create", args: []string{"--output", "-"}, wantError: true},
		{name: "relative", operation: "restore", args: []string{"--input", "backup.tar"}, wantError: true},
		{name: "missing", operation: "create", wantError: true},
		{name: "positional", operation: "create", args: []string{"--output", absolute, "extra"}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options, err := parseBackupCommandOptions("xtunnel-server", test.operation, test.args, nil, &bytes.Buffer{})
			if test.wantError {
				if err == nil {
					t.Fatalf("parseBackupCommandOptions() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseBackupCommandOptions() error = %v", err)
			}
			if options.path != test.wantPath {
				t.Fatalf("archive path = %q, want %q", options.path, test.wantPath)
			}
		})
	}
}

func TestBackupCommandDispatchRejectsUnknownOperation(t *testing.T) {
	if !isBackupCommand([]string{"backup", "create"}) {
		t.Fatal("isBackupCommand() = false")
	}
	if err := runBackupCommand(t.Context(), "xtunnel-server", []string{"unknown"}, nil, &bytes.Buffer{}); err == nil {
		t.Fatal("runBackupCommand(unknown) error = nil")
	}
}
