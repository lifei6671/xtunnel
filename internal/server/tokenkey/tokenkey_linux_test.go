//go:build linux

package tokenkey

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateCreatesStrictLinuxPermissions(t *testing.T) {
	dataDir := absoluteTempDir(t)
	if _, err := loadOrCreate(dataDir, false, bytes.NewReader(make([]byte, Size))); err != nil {
		t.Fatalf("loadOrCreate() error = %v", err)
	}
	directoryInfo, err := os.Stat(filepath.Join(dataDir, credentialDirectoryName))
	if err != nil {
		t.Fatalf("os.Stat(credentials) error = %v", err)
	}
	if directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("credential directory permissions = %04o, want 0700", directoryInfo.Mode().Perm())
	}
	keyInfo, err := os.Stat(keyPath(dataDir))
	if err != nil {
		t.Fatalf("os.Stat(key) error = %v", err)
	}
	if keyInfo.Mode().Perm() != 0o600 {
		t.Fatalf("key permissions = %04o, want 0600", keyInfo.Mode().Perm())
	}
}

func TestLoadOrCreateRejectsUnsafeLinuxPermissions(t *testing.T) {
	tests := []struct {
		name          string
		directoryMode os.FileMode
		keyMode       os.FileMode
	}{
		{name: "credential directory", directoryMode: 0o750, keyMode: 0o600},
		{name: "key file", directoryMode: 0o700, keyMode: 0o640},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dataDir := absoluteTempDir(t)
			directory := filepath.Join(dataDir, credentialDirectoryName)
			if err := os.Mkdir(directory, 0o700); err != nil {
				t.Fatalf("os.Mkdir(credentials) error = %v", err)
			}
			if err := os.WriteFile(keyPath(dataDir), make([]byte, Size), 0o600); err != nil {
				t.Fatalf("os.WriteFile(key) error = %v", err)
			}
			if err := os.Chmod(directory, test.directoryMode); err != nil {
				t.Fatalf("os.Chmod(credentials) error = %v", err)
			}
			if err := os.Chmod(keyPath(dataDir), test.keyMode); err != nil {
				t.Fatalf("os.Chmod(key) error = %v", err)
			}

			_, err := loadOrCreate(dataDir, false, bytes.NewReader(make([]byte, Size)))
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("loadOrCreate() error = %v, want ErrUnavailable", err)
			}
		})
	}
}
