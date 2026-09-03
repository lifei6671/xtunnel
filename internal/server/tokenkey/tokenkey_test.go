package tokenkey

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateCreatesStableIndependentKey(t *testing.T) {
	dataDir := absoluteTempDir(t)
	want := bytes.Repeat([]byte{0x5a}, Size)

	created, err := loadOrCreate(dataDir, false, bytes.NewReader(want))
	if err != nil {
		t.Fatalf("loadOrCreate(first) error = %v", err)
	}
	if !bytes.Equal(created[:], want) {
		t.Fatal("loadOrCreate(first) returned unexpected key")
	}

	loaded, err := loadOrCreate(dataDir, true, errReader{})
	if err != nil {
		t.Fatalf("loadOrCreate(restart) error = %v", err)
	}
	if loaded != created {
		t.Fatal("loadOrCreate(restart) changed the existing key")
	}
	content, err := os.ReadFile(keyPath(dataDir))
	if err != nil {
		t.Fatalf("os.ReadFile(key) error = %v", err)
	}
	if !bytes.Equal(content, want) {
		t.Fatal("key file content does not match generated key")
	}
	entries, err := os.ReadDir(filepath.Dir(keyPath(dataDir)))
	if err != nil {
		t.Fatalf("os.ReadDir(credentials) error = %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != masterKeyFilename {
		t.Fatalf("credential directory entries = %v, want only %q", entries, masterKeyFilename)
	}
}

func TestLoadOrCreateRejectsMissingKeyWhenCiphertextExists(t *testing.T) {
	dataDir := absoluteTempDir(t)

	_, err := loadOrCreate(dataDir, true, bytes.NewReader(make([]byte, Size)))
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("loadOrCreate() error = %v, want ErrUnavailable", err)
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, credentialDirectoryName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("missing-key failure mutated credential directory: os.Stat() error = %v", statErr)
	}
}

func TestLoadOrCreateRejectsCorruptExistingKey(t *testing.T) {
	tests := []struct {
		name    string
		content []byte
	}{
		{name: "empty", content: nil},
		{name: "truncated", content: bytes.Repeat([]byte{0x21}, Size-1)},
		{name: "oversized", content: bytes.Repeat([]byte{0x21}, Size+1)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dataDir := absoluteTempDir(t)
			if _, err := loadOrCreate(dataDir, false, bytes.NewReader(make([]byte, Size))); err != nil {
				t.Fatalf("loadOrCreate(initial) error = %v", err)
			}
			if err := os.WriteFile(keyPath(dataDir), test.content, 0o600); err != nil {
				t.Fatalf("os.WriteFile(key) error = %v", err)
			}

			_, err := loadOrCreate(dataDir, false, bytes.NewReader(make([]byte, Size)))
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("loadOrCreate() error = %v, want ErrUnavailable", err)
			}
			content, readErr := os.ReadFile(keyPath(dataDir))
			if readErr != nil {
				t.Fatalf("os.ReadFile(key) error = %v", readErr)
			}
			if !bytes.Equal(content, test.content) {
				t.Fatal("corrupt key was silently replaced")
			}
		})
	}
}

func TestLoadOrCreateRejectsUnsafePaths(t *testing.T) {
	t.Run("relative data directory", func(t *testing.T) {
		_, err := loadOrCreate("relative", false, bytes.NewReader(make([]byte, Size)))
		if err == nil {
			t.Fatal("loadOrCreate() accepted a relative data directory")
		}
	})

	t.Run("credential symlink", func(t *testing.T) {
		dataDir := absoluteTempDir(t)
		target := absoluteTempDir(t)
		if err := os.Symlink(target, filepath.Join(dataDir, credentialDirectoryName)); err != nil {
			t.Skipf("os.Symlink() is unavailable: %v", err)
		}
		_, err := loadOrCreate(dataDir, false, bytes.NewReader(make([]byte, Size)))
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("loadOrCreate() error = %v, want ErrUnavailable", err)
		}
	})
}

func TestLoadOrCreatePropagatesRandomFailureWithoutPublishingKey(t *testing.T) {
	dataDir := absoluteTempDir(t)
	_, err := loadOrCreate(dataDir, false, errReader{})
	if err == nil {
		t.Fatal("loadOrCreate() accepted random source failure")
	}
	if _, statErr := os.Stat(keyPath(dataDir)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("random failure published a key: os.Stat() error = %v", statErr)
	}
}

func absoluteTempDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join(t.TempDir(), "managed-data"))
	if err != nil {
		t.Fatalf("filepath.Abs() error = %v", err)
	}
	prepareTestDataDir(t, path)
	return path
}

func keyPath(dataDir string) string {
	return filepath.Join(dataDir, credentialDirectoryName, masterKeyFilename)
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) {
	return 0, errors.New("injected random failure")
}
