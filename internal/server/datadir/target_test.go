package datadir

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveDoesNotRequireOrCreateLeaf(t *testing.T) {
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "data")

	target, err := Resolve(dataDir)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if target.Path != dataDir || target.Parent != parent || target.Leaf != "data" || len(target.Hash) != 64 {
		t.Fatalf("Resolve() = %#v", target)
	}
	if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Resolve() touched leaf: os.Stat() error = %v", err)
	}
}

func TestResolveRejectsInvalidParent(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(filePath, nil, 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	tests := []struct {
		name string
		path string
	}{
		{name: "relative", path: "relative/data"},
		{name: "missing parent", path: filepath.Join(t.TempDir(), "missing", "data")},
		{name: "file parent", path: filepath.Join(filePath, "data")},
		{name: "root", path: filepath.VolumeName(filePath) + string(filepath.Separator)},
		{name: "dot leaf", path: filepath.Join(t.TempDir(), "data") + string(filepath.Separator) + "."},
		{name: "dot dot leaf", path: filepath.Join(t.TempDir(), "data") + string(filepath.Separator) + ".."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Resolve(test.path); err == nil {
				t.Fatalf("Resolve(%q) error = nil", test.path)
			}
		})
	}
}

func TestResolveRejectsSymlinkAncestorDotDotPath(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "real")
	realChild := filepath.Join(realParent, "child")
	if err := os.MkdirAll(realChild, 0o700); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(realChild, link); err != nil {
		t.Skipf("os.Symlink() unavailable: %v", err)
	}

	ambiguousPath := link + string(filepath.Separator) + ".." + string(filepath.Separator) + "data"
	if _, err := Resolve(ambiguousPath); err == nil {
		t.Fatal("Resolve() error = nil")
	}
}

func TestResolveCanonicalizesSymlinkAncestor(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "real")
	realChild := filepath.Join(realParent, "child")
	if err := os.MkdirAll(realChild, 0o700); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(realParent, link); err != nil {
		t.Skipf("os.Symlink() unavailable: %v", err)
	}

	target, err := Resolve(filepath.Join(link, "child", "data"))
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	want, err := Resolve(filepath.Join(realChild, "data"))
	if err != nil {
		t.Fatalf("Resolve(canonical path) error = %v", err)
	}
	if target.Path != want.Path || target.Hash != want.Hash {
		t.Fatalf("Resolve() = %#v, want target %#v", target, want)
	}
}

func TestResolveRejectsDirectSymlinkParent(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatalf("os.Mkdir() error = %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(realParent, link); err != nil {
		t.Skipf("os.Symlink() unavailable: %v", err)
	}
	if _, err := Resolve(filepath.Join(link, "data")); err == nil {
		t.Fatal("Resolve() error = nil")
	}
}

func TestValidateCanonical(t *testing.T) {
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "data")
	target, err := Resolve(dataDir)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if err := ValidateCanonical(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ValidateCanonical() error = %v, want os.ErrNotExist", err)
	}
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatalf("os.Mkdir() error = %v", err)
	}
	if err := ValidateCanonical(target); err != nil {
		t.Fatalf("ValidateCanonical() error = %v", err)
	}
}

func TestValidateCanonicalRejectsNonDirectory(t *testing.T) {
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "data")
	target, err := Resolve(dataDir)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if err := os.WriteFile(dataDir, nil, 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	if err := ValidateCanonical(target); err == nil {
		t.Fatal("ValidateCanonical() error = nil")
	}
}

func TestValidateCanonicalRejectsSymlinkLeaf(t *testing.T) {
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "data")
	target, err := Resolve(dataDir)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	realDataDir := filepath.Join(parent, "real-data")
	if err := os.Mkdir(realDataDir, 0o700); err != nil {
		t.Fatalf("os.Mkdir() error = %v", err)
	}
	if err := os.Symlink(realDataDir, dataDir); err != nil {
		t.Skipf("os.Symlink() unavailable: %v", err)
	}
	if err := ValidateCanonical(target); err == nil {
		t.Fatal("ValidateCanonical() error = nil")
	}
}
