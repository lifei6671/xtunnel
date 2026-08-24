//go:build linux

package externallock

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestAcquireRejectsSecondProcess(t *testing.T) {
	if os.Getenv("XTUNNEL_LOCK_HELPER") == "1" {
		runtimeDir := os.Getenv("XTUNNEL_LOCK_RUNTIME_DIR")
		lock, err := Acquire(runtimeDir, "target")
		if err != nil {
			t.Fatalf("Acquire() helper error = %v", err)
		}
		t.Cleanup(func() {
			if err := lock.Close(); err != nil {
				t.Errorf("Lock.Close() helper error = %v", err)
			}
		})
		fmt.Println("locked")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			t.Fatalf("read release signal error = %v", err)
		}
		if line != "release\n" {
			t.Fatalf("release signal = %q", line)
		}
		return
	}

	runtimeDir := newRuntimeDir(t)
	command := exec.Command(os.Args[0], "-test.run=^TestAcquireRejectsSecondProcess$")
	command.Env = append(os.Environ(),
		"XTUNNEL_LOCK_HELPER=1",
		"XTUNNEL_LOCK_RUNTIME_DIR="+runtimeDir,
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
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "locked\n" {
		t.Fatalf("helper readiness = %q, error = %v", line, err)
	}

	if _, err := Acquire(runtimeDir, "target"); !errors.Is(err, ErrAlreadyLocked) {
		t.Fatalf("second Acquire() error = %v, want ErrAlreadyLocked", err)
	}
	if _, err := stdin.Write([]byte("release\n")); err != nil {
		t.Fatalf("stdin.Write() error = %v", err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatalf("stdin.Close() error = %v", err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("command.Wait() error = %v", err)
	}

	lock, err := Acquire(runtimeDir, "target")
	if err != nil {
		t.Fatalf("Acquire() after release error = %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("Lock.Close() error = %v", err)
	}
}

func TestAcquireRejectsSymlinkLockFile(t *testing.T) {
	runtimeDir := newRuntimeDir(t)
	target := filepath.Join(t.TempDir(), "target")
	lockPath := filepath.Join(runtimeDir, "server-lock-target.lock")
	if err := os.Symlink(target, lockPath); err != nil {
		t.Fatalf("os.Symlink() error = %v", err)
	}
	if _, err := Acquire(runtimeDir, "target"); err == nil {
		t.Fatal("Acquire() error = nil")
	}
}

func TestAcquireLeavesPrivateLockFile(t *testing.T) {
	runtimeDir := newRuntimeDir(t)
	lock, err := Acquire(runtimeDir, "target")
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("Lock.Close() error = %v", err)
	}

	info, err := os.Lstat(filepath.Join(runtimeDir, "server-lock-target.lock"))
	if err != nil {
		t.Fatalf("os.Lstat() error = %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("lock file permissions = %04o, want 0600", info.Mode().Perm())
	}
}

func TestAcquireAllowsRootToUseRuntimeUIDDirectory(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}
	runtimeDir := newRuntimeDir(t)
	if err := os.Chown(runtimeDir, 65534, 65534); err != nil {
		t.Fatalf("os.Chown() error = %v", err)
	}
	lock, err := Acquire(runtimeDir, "target")
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("Lock.Close() error = %v", err)
	}
	var stat unix.Stat_t
	lockPath := filepath.Join(runtimeDir, "server-lock-target.lock")
	if err := unix.Lstat(lockPath, &stat); err != nil {
		t.Fatalf("unix.Lstat() error = %v", err)
	}
	if stat.Uid != 65534 || stat.Gid != 65534 {
		t.Fatalf("lock owner = %d:%d, want 65534:65534", stat.Uid, stat.Gid)
	}
}

func newRuntimeDir(t *testing.T) string {
	t.Helper()
	runtimeDir := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatalf("os.Mkdir() error = %v", err)
	}
	return runtimeDir
}
