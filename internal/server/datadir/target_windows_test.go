//go:build windows

package datadir

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWindowsResolveDoesNotRequireOrCreateLeaf(t *testing.T) {
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "Data")

	target, err := Resolve(dataDir)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if !strings.EqualFold(target.Path, filepath.Join(target.Parent, "Data")) || target.Leaf != "Data" || len(target.Hash) != 64 {
		t.Fatalf("Resolve() = %#v", target)
	}
	if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Resolve() touched leaf: os.Stat() error = %v", err)
	}

	lowerTarget, err := Resolve(filepath.Join(parent, "data"))
	if err != nil {
		t.Fatalf("Resolve(lowercase leaf) error = %v", err)
	}
	if lowerTarget.Hash != target.Hash {
		t.Fatalf("case-insensitive target hash = %q, want %q", lowerTarget.Hash, target.Hash)
	}
}

func TestWindowsResolveDirectChildOfVolumeRoot(t *testing.T) {
	root := filepath.VolumeName(t.TempDir()) + `\`
	dataDir := filepath.Join(root, "XTunnelM802RootTargetDoesNotExist")
	if _, err := os.Lstat(dataDir); err == nil {
		t.Skipf("test target unexpectedly exists: %s", dataDir)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("os.Lstat(test target) error = %v", err)
	}

	target, err := Resolve(dataDir)
	if err != nil {
		t.Fatalf("Resolve(volume root child) error = %v", err)
	}
	guard, err := PinParent(target)
	if err != nil {
		t.Fatalf("PinParent(volume root) error = %v", err)
	}
	if err := guard.Close(); err != nil {
		t.Fatalf("ParentGuard.Close() error = %v", err)
	}
}

func TestWindowsResolveRejectsUnsafePathForms(t *testing.T) {
	drive := filepath.VolumeName(t.TempDir())
	tests := []struct {
		name string
		path string
	}{
		{name: "relative", path: `relative\data`},
		{name: "UNC", path: `\\localhost\share\data`},
		{name: "extended namespace", path: `\\?\` + drive + `\data`},
		{name: "device namespace", path: `\\.\` + drive[:1] + `\data`},
		{name: "alternate data stream", path: filepath.Join(t.TempDir(), "data") + `:stream`},
		{name: "dot component", path: t.TempDir() + `\.\data`},
		{name: "dot dot component", path: t.TempDir() + `\child\..\data`},
		{name: "reserved device leaf", path: filepath.Join(t.TempDir(), "NUL.txt")},
		{name: "trailing dot", path: filepath.Join(t.TempDir(), "data.")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Resolve(test.path); err == nil {
				t.Fatalf("Resolve(%q) error = nil", test.path)
			}
		})
	}
}

func TestWindowsValidateCanonicalRejectsParentIdentityReplacement(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("os.Mkdir(parent) error = %v", err)
	}
	target, err := Resolve(filepath.Join(parent, "data"))
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	replaced := filepath.Join(root, "replaced")
	if err := os.Rename(parent, replaced); err != nil {
		t.Fatalf("os.Rename(parent) error = %v", err)
	}
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("os.Mkdir(replacement parent) error = %v", err)
	}
	if err := os.Mkdir(filepath.Join(parent, "data"), 0o700); err != nil {
		t.Fatalf("os.Mkdir(replacement data) error = %v", err)
	}
	replacementTarget, err := Resolve(filepath.Join(parent, "data"))
	if err != nil {
		t.Fatalf("Resolve(replacement target) error = %v", err)
	}
	if replacementTarget.Hash == target.Hash {
		t.Fatal("replacement parent produced the original Stable Target Hash")
	}
	if err := ValidateCanonical(target); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("ValidateCanonical() error = %v, want parent identity change", err)
	}
}

func TestWindowsParentGuardAllowsLeafReplacement(t *testing.T) {
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "data")
	stagingDir := filepath.Join(parent, "staging")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatalf("os.Mkdir(data) error = %v", err)
	}
	if err := os.Mkdir(stagingDir, 0o700); err != nil {
		t.Fatalf("os.Mkdir(staging) error = %v", err)
	}
	target, err := Resolve(dataDir)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	guard, err := PinParent(target)
	if err != nil {
		t.Fatalf("PinParent() error = %v", err)
	}
	defer func() {
		if err := guard.Close(); err != nil {
			t.Errorf("ParentGuard.Close() error = %v", err)
		}
	}()
	rollbackDir := filepath.Join(parent, "rollback")
	if err := os.Rename(dataDir, rollbackDir); err != nil {
		t.Fatalf("os.Rename(data, rollback) while guarded error = %v", err)
	}
	if err := os.Rename(stagingDir, dataDir); err != nil {
		t.Fatalf("os.Rename(staging, data) while guarded error = %v", err)
	}
	if err := guard.ValidateCanonical(); err != nil {
		t.Fatalf("ParentGuard.ValidateCanonical(replaced leaf) error = %v", err)
	}
}

func TestWindowsValidateCanonical(t *testing.T) {
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "data")
	target, err := Resolve(dataDir)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if err := ValidateCanonical(target); err == nil {
		t.Fatal("ValidateCanonical() missing leaf error = nil")
	}
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatalf("os.Mkdir(data) error = %v", err)
	}
	if err := ValidateCanonical(target); err != nil {
		t.Fatalf("ValidateCanonical() error = %v", err)
	}
}

func TestWindowsParentGuardPinsParentUntilClose(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("os.Mkdir(parent) error = %v", err)
	}
	dataDir := filepath.Join(parent, "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatalf("os.Mkdir(data) error = %v", err)
	}
	target, err := Resolve(dataDir)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	guard, err := PinParent(target)
	if err != nil {
		t.Fatalf("PinParent() error = %v", err)
	}
	if err := ValidateTarget(target); err != nil {
		_ = guard.Close()
		t.Fatalf("ValidateTarget() while guarded error = %v", err)
	}
	if err := guard.ValidateCanonical(); err != nil {
		_ = guard.Close()
		t.Fatalf("ParentGuard.ValidateCanonical() error = %v", err)
	}
	moved := filepath.Join(root, "moved")
	if err := os.Rename(parent, moved); err == nil {
		_ = guard.Close()
		t.Fatal("os.Rename(parent) error = nil while ParentGuard is held")
	}
	if err := guard.Close(); err != nil {
		t.Fatalf("ParentGuard.Close() error = %v", err)
	}
	if err := os.Rename(parent, moved); err != nil {
		t.Fatalf("os.Rename(parent) after Close error = %v", err)
	}
}

func TestWindowsParentGuardDoesNotRequireDeleteAccess(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "restricted-parent")
	dataDir := filepath.Join(parent, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("os.MkdirAll(data) error = %v", err)
	}
	currentUser, err := user.Current()
	if err != nil {
		t.Fatalf("user.Current() error = %v", err)
	}
	permission := currentUser.Username + ":(OI)(CI)(RX)"
	if output, err := exec.Command("icacls.exe", parent, "/inheritance:r", "/grant:r", permission).CombinedOutput(); err != nil {
		t.Fatalf("restrict parent DACL: %v: %s", err, output)
	}
	defer func() {
		if output, err := exec.Command("icacls.exe", parent, "/reset", "/t", "/c").CombinedOutput(); err != nil {
			t.Errorf("restore parent DACL: %v: %s", err, output)
		}
	}()

	target, err := Resolve(dataDir)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	guard, err := PinParent(target)
	if err != nil {
		t.Fatalf("PinParent() with read-and-execute parent DACL error = %v", err)
	}
	if err := guard.ValidateCanonical(); err != nil {
		_ = guard.Close()
		t.Fatalf("ParentGuard.ValidateCanonical() error = %v", err)
	}
	if err := guard.Close(); err != nil {
		t.Fatalf("ParentGuard.Close() error = %v", err)
	}
}

func TestWindowsParentGuardPinsAncestorUntilClose(t *testing.T) {
	root := t.TempDir()
	ancestor := filepath.Join(root, "ancestor")
	parent := filepath.Join(ancestor, "parent")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatalf("os.MkdirAll(parent) error = %v", err)
	}
	dataDir := filepath.Join(parent, "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatalf("os.Mkdir(data) error = %v", err)
	}
	target, err := Resolve(dataDir)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	guard, err := PinParent(target)
	if err != nil {
		t.Fatalf("PinParent() error = %v", err)
	}
	moved := filepath.Join(root, "moved")
	if err := os.Rename(ancestor, moved); err == nil {
		_ = guard.Close()
		t.Fatal("os.Rename(ancestor) error = nil while ParentGuard is held")
	}
	if err := guard.Close(); err != nil {
		t.Fatalf("ParentGuard.Close() error = %v", err)
	}
	if err := os.Rename(ancestor, moved); err != nil {
		t.Fatalf("os.Rename(ancestor) after Close error = %v", err)
	}
}

func TestWindowsParentGuardReleasesAfterProcessExit(t *testing.T) {
	if os.Getenv("XTUNNEL_WINDOWS_PARENT_GUARD_HELPER") == "1" {
		target, err := Resolve(os.Getenv("XTUNNEL_PARENT_GUARD_DATA_DIR"))
		if err != nil {
			t.Fatalf("Resolve() helper error = %v", err)
		}
		guard, err := PinParent(target)
		if err != nil {
			t.Fatalf("PinParent() helper error = %v", err)
		}
		defer func() { _ = guard.Close() }()
		fmt.Println("guarded")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		return
	}

	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	dataDir := filepath.Join(parent, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("os.MkdirAll(data) error = %v", err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestWindowsParentGuardReleasesAfterProcessExit$")
	command.Env = append(os.Environ(),
		"XTUNNEL_WINDOWS_PARENT_GUARD_HELPER=1",
		"XTUNNEL_PARENT_GUARD_DATA_DIR="+dataDir,
	)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe() error = %v", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe() error = %v", err)
	}
	if err := command.Start(); err != nil {
		t.Fatalf("command.Start() error = %v", err)
	}
	ready := make(chan error, 1)
	go func() {
		line, readErr := bufio.NewReader(stdout).ReadString('\n')
		if readErr == nil && line != "guarded\n" {
			readErr = fmt.Errorf("helper readiness = %q", line)
		}
		ready <- readErr
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("read helper readiness: %v", err)
		}
	case <-time.After(10 * time.Second):
		_ = command.Process.Kill()
		t.Fatal("helper did not pin the parent within 10 seconds")
	}

	moved := filepath.Join(root, "moved")
	if err := os.Rename(parent, moved); err == nil {
		_ = command.Process.Kill()
		t.Fatal("os.Rename(parent) error = nil while helper ParentGuard is held")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatalf("Process.Kill() error = %v", err)
	}
	_ = stdin.Close()
	if err := command.Wait(); err == nil {
		t.Fatal("killed helper Wait() error = nil")
	}
	if err := os.Rename(parent, moved); err != nil {
		t.Fatalf("os.Rename(parent) after process exit error = %v", err)
	}
}

func TestWindowsStableTargetHashUsesFullFileID(t *testing.T) {
	low := windowsFileIdentity{volume: 7, file: [16]byte{0: 1}}
	high := low
	high.file[15] = 1
	if stableTargetHash(low, "data") == stableTargetHash(high, "data") {
		t.Fatal("stableTargetHash() ignored the high 64 bits of the Windows File ID")
	}
	if stableTargetHash(low, "Data") != stableTargetHash(low, "data") {
		t.Fatal("stableTargetHash() is not case-insensitive for the leaf name")
	}
}

func TestWindowsValidateCanonicalRejectsReparseLeaf(t *testing.T) {
	parent := t.TempDir()
	target, err := Resolve(filepath.Join(parent, "data"))
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	realData := filepath.Join(parent, "real-data")
	if err := os.Mkdir(realData, 0o700); err != nil {
		t.Fatalf("os.Mkdir(real data) error = %v", err)
	}
	if output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", target.Path, realData).CombinedOutput(); err != nil {
		t.Fatalf("create data junction: %v: %s", err, output)
	}
	if err := ValidateCanonical(target); err == nil || !strings.Contains(err.Error(), "reparse point") {
		t.Fatalf("ValidateCanonical() error = %v, want reparse point rejection", err)
	}
}

func TestWindowsResolveRejectsReparseAncestor(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatalf("os.Mkdir(real parent) error = %v", err)
	}
	junction := filepath.Join(root, "junction")
	if output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", junction, realParent).CombinedOutput(); err != nil {
		t.Fatalf("create junction: %v: %s", err, output)
	}
	if _, err := Resolve(filepath.Join(junction, "data")); err == nil {
		t.Fatal("Resolve(reparse ancestor) error = nil")
	}
}
