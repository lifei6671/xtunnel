package bootstrap

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	baseconfig "github.com/lifei6671/xtunnel/internal/config"
)

func TestParseAdminCreateOptions(t *testing.T) {
	passwordFile := filepath.Join(t.TempDir(), "password")
	options, err := parseAdminCreateOptions(
		"xtunnel-server",
		[]string{"--username", "admin", "--password-file", passwordFile, "--set", "logging.level=warn"},
		[]string{"OTHER=value"},
		&bytes.Buffer{},
	)
	if err != nil {
		t.Fatalf("parseAdminCreateOptions() error = %v", err)
	}
	if options.username != "admin" || options.passwordFile != passwordFile {
		t.Fatalf("admin options = %#v", options)
	}
	if options.config.CLI["logging.level"] != "warn" {
		t.Fatalf("CLI overrides = %#v", options.config.CLI)
	}
	if len(options.config.Environment) != 1 || options.config.Environment[0] != "OTHER=value" {
		t.Fatalf("Environment = %#v", options.config.Environment)
	}
}

func TestParseAdminCreateOptionsRejectsPasswordArgumentAndInvalidCommands(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "password argument", args: []string{"--username", "admin", "--password", "secret"}, want: "flag provided but not defined"},
		{name: "missing username", args: nil, want: "username must not be empty"},
		{name: "positional", args: []string{"--username", "admin", "extra"}, want: "unexpected positional arguments"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseAdminCreateOptions("xtunnel-server", test.args, nil, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parseAdminCreateOptions() error = %v, want substring %q", err, test.want)
			}
		})
	}
	var stderr bytes.Buffer
	exitCode := executeWithRun("xtunnel-server", []string{"admin", "delete"}, nil, &stderr, func(context.Context, baseconfig.Options, io.Writer) error {
		t.Fatal("invalid admin command invoked Server runner")
		return nil
	})
	if exitCode != 1 || !strings.Contains(stderr.String(), "expected admin create") {
		t.Fatalf("executeWithRun(admin delete) = %d, stderr = %q", exitCode, stderr.String())
	}
}

func TestReadAdminPasswordFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(path, []byte("file password\n"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	password, err := readAdminPassword(path, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("readAdminPassword() error = %v", err)
	}
	if password != "file password" {
		t.Fatal("readAdminPassword() did not preserve the password file contents")
	}
	if _, err := readAdminPassword(filepath.Join(t.TempDir(), "missing"), &bytes.Buffer{}); err == nil {
		t.Fatal("readAdminPassword() accepted a missing file")
	}
}
