package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestScanUsesExactPathAndDigestAllowlist(t *testing.T) {
	root := t.TempDir()
	content := []byte("xta_0123456789abcdefghijklmnop")
	path := filepath.Join(root, "placeholder.txt")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	allowlist := map[string]string{"placeholder.txt": hex.EncodeToString(digest[:])}
	if err := scan(root, allowlist); err != nil {
		t.Fatalf("scan() rejected exact allowlist: %v", err)
	}
	if err := os.WriteFile(path, append(content, 'x'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := scan(root, allowlist); err == nil {
		t.Fatal("scan() accepted changed allowlisted content")
	}
}

func TestScanRejectsUnclassifiedSecret(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "log.txt"), []byte("Authorization: Bearer credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := scan(root, nil); err == nil {
		t.Fatal("scan() accepted an unclassified credential")
	}
}

func TestScanRejectsUnconsumedAllowlistEntry(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "safe.txt"), []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := scan(root, map[string]string{"missing.txt": "00"}); err == nil {
		t.Fatal("scan() accepted an allowlist entry that was not consumed")
	}
}

func TestScanRejectsSymbolicLink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(target, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	if err := scan(root, nil); err == nil {
		t.Fatal("scan() accepted a symbolic link")
	}
}
