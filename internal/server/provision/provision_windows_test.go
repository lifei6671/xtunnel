//go:build windows

package provision

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInitializeWithRuntimeDirectoryCreatesAndReusesDirectories(t *testing.T) {
	root := t.TempDir()
	serverRoot := filepath.Join(root, "XTunnel", "Server")
	dataDir := filepath.Join(serverRoot, "data")
	runtimeDir := filepath.Join(serverRoot, "runtime")

	for range 2 {
		if err := initializeWithRuntimeDirectory(dataDir, runtimeDir); err != nil {
			t.Fatalf("initializeWithRuntimeDirectory() error = %v", err)
		}
	}
	for _, path := range []string{dataDir, runtimeDir} {
		information, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat(%q) error = %v", path, err)
		}
		if !information.IsDir() {
			t.Fatalf("%q is not a directory", path)
		}
	}
}

func TestInitializeWithRuntimeDirectoryRejectsFileDataTarget(t *testing.T) {
	root := t.TempDir()
	serverRoot := filepath.Join(root, "XTunnel", "Server")
	if err := os.MkdirAll(serverRoot, 0o700); err != nil {
		t.Fatalf("MkdirAll(server root) error = %v", err)
	}
	dataPath := filepath.Join(serverRoot, "data")
	if err := os.WriteFile(dataPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile(data target) error = %v", err)
	}
	if err := initializeWithRuntimeDirectory(dataPath, filepath.Join(serverRoot, "runtime")); err == nil {
		t.Fatal("initializeWithRuntimeDirectory() error = nil")
	}
}

func TestInitializeWithRuntimeDirectoryRejectsExistingUnprotectedManagedRoot(t *testing.T) {
	root := t.TempDir()
	managedRoot := filepath.Join(root, "XTunnel")
	if err := os.Mkdir(managedRoot, 0o700); err != nil {
		t.Fatalf("Mkdir(managed root) error = %v", err)
	}
	serverRoot := filepath.Join(managedRoot, "Server")
	if err := initializeWithRuntimeDirectory(filepath.Join(serverRoot, "data"), filepath.Join(serverRoot, "runtime")); err == nil {
		t.Fatal("initializeWithRuntimeDirectory() error = nil, want protected ACL rejection")
	}
	if _, err := os.Stat(serverRoot); !os.IsNotExist(err) {
		t.Fatalf("Stat(server root) error = %v, want not exist", err)
	}
}

func TestInitializeWithRuntimeDirectoryRejectsInvalidDataPathBeforeDirectoryCreation(t *testing.T) {
	runtimeDir := filepath.Join(t.TempDir(), "runtime")
	if err := initializeWithRuntimeDirectory(`\\localhost\share\data`, runtimeDir); err == nil {
		t.Fatal("initializeWithRuntimeDirectory() error = nil")
	}
	if _, err := os.Stat(runtimeDir); !os.IsNotExist(err) {
		t.Fatalf("Stat(runtime directory) error = %v, want not exist", err)
	}
}
