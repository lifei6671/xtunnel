package bootstrap

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	baseconfig "github.com/lifei6671/xtunnel/internal/config"
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
	var stderr bytes.Buffer
	exitCode := executeWithRun("xtunnel-server", []string{"backup", "unknown"}, nil, &stderr, func(context.Context, baseconfig.Options, io.Writer) error {
		t.Fatal("invalid backup command invoked Server runner")
		return nil
	})
	if exitCode != 1 || !strings.Contains(stderr.String(), "unknown backup command") {
		t.Fatalf("executeWithRun(backup unknown) = %d, stderr = %q", exitCode, stderr.String())
	}
}
