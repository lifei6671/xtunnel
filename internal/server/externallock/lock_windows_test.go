//go:build windows

package externallock

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const windowsTestTargetHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestValidateFileIDInfo(t *testing.T) {
	tests := []struct {
		name        string
		information fileIDInfo
		wantVolume  uint64
		wantError   bool
	}{
		{
			name:        "stable identity",
			information: fileIDInfo{volumeSerial: 7, fileID: [16]byte{1}},
			wantVolume:  7,
		},
		{
			name:        "zero volume serial",
			information: fileIDInfo{fileID: [16]byte{1}},
			wantError:   true,
		},
		{
			name:        "zero file ID",
			information: fileIDInfo{volumeSerial: 7},
			wantError:   true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := validateFileIDInfo(test.information)
			if (err != nil) != test.wantError {
				t.Fatalf("validateFileIDInfo() error = %v, want error = %t", err, test.wantError)
			}
			if err == nil && got != test.wantVolume {
				t.Fatalf("validateFileIDInfo() = %d, want %d", got, test.wantVolume)
			}
		})
	}
}

func TestWindowsRuntimeDirectoryUsesProgramDataKnownFolder(t *testing.T) {
	programData, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		t.Fatalf("KnownFolderPath(ProgramData) error = %v", err)
	}
	runtimeDir, err := RuntimeDirectory()
	if err != nil {
		t.Fatalf("RuntimeDirectory() error = %v", err)
	}
	want := filepath.Join(programData, "XTunnel", "Server", "runtime")
	if !strings.EqualFold(runtimeDir, want) {
		t.Fatalf("RuntimeDirectory() = %q, want %q", runtimeDir, want)
	}
}

func TestWindowsAcquireRejectsSecondProcessAndReleasesAfterExit(t *testing.T) {
	if os.Getenv("XTUNNEL_WINDOWS_LOCK_HELPER") == "1" {
		lock, err := Acquire(os.Getenv("XTUNNEL_LOCK_RUNTIME_DIR"), windowsTestTargetHash)
		if err != nil {
			t.Fatalf("Acquire() helper error = %v", err)
		}
		defer func() {
			if err := lock.Close(); err != nil {
				t.Errorf("Lock.Close() helper error = %v", err)
			}
		}()
		fmt.Println("locked")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		return
	}

	runtimeDir := newWindowsRuntimeDir(t)
	command := exec.Command(os.Args[0], "-test.run=^TestWindowsAcquireRejectsSecondProcessAndReleasesAfterExit$")
	command.Env = append(os.Environ(),
		"XTUNNEL_WINDOWS_LOCK_HELPER=1",
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
	ready := make(chan error, 1)
	go func() {
		line, readErr := bufio.NewReader(stdout).ReadString('\n')
		if readErr == nil && line != "locked\n" {
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
		t.Fatal("helper did not acquire the lock within 10 seconds")
	}

	if _, err := Acquire(runtimeDir, windowsTestTargetHash); !errors.Is(err, ErrAlreadyLocked) {
		t.Fatalf("second Acquire() error = %v, want ErrAlreadyLocked", err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatalf("Process.Kill() error = %v", err)
	}
	_ = stdin.Close()
	if err := command.Wait(); err == nil {
		t.Fatal("killed helper Wait() error = nil")
	}

	lock, err := Acquire(runtimeDir, windowsTestTargetHash)
	if err != nil {
		t.Fatalf("Acquire() after process exit error = %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("Lock.Close() error = %v", err)
	}
}

func TestWindowsAcquirePinsRuntimeAndLockFile(t *testing.T) {
	runtimeDir := newWindowsRuntimeDir(t)
	lock, err := Acquire(runtimeDir, windowsTestTargetHash)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	lockPath := filepath.Join(runtimeDir, "server-lock-"+windowsTestTargetHash+".lock")
	if err := os.Rename(lockPath, lockPath+".moved"); err == nil {
		t.Fatal("os.Rename(lock file) error = nil while lock is held")
	}
	if err := os.Rename(runtimeDir, runtimeDir+"-moved"); err == nil {
		t.Fatal("os.Rename(runtime directory) error = nil while lock is held")
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("Lock.Close() error = %v", err)
	}
}

func TestWindowsAcquireRejectsReparseRuntimeAndLock(t *testing.T) {
	root := t.TempDir()
	realRuntime := filepath.Join(root, "real-runtime")
	if err := os.Mkdir(realRuntime, 0o700); err != nil {
		t.Fatalf("os.Mkdir(real runtime) error = %v", err)
	}
	junctionRuntime := filepath.Join(root, "runtime-junction")
	createJunction(t, junctionRuntime, realRuntime)
	if _, err := Acquire(junctionRuntime, windowsTestTargetHash); err == nil {
		t.Fatal("Acquire(reparse runtime) error = nil")
	}

	runtimeDir := newWindowsRuntimeDir(t)
	lockPath := filepath.Join(runtimeDir, "server-lock-"+windowsTestTargetHash+".lock")
	createJunction(t, lockPath, realRuntime)
	if _, err := Acquire(runtimeDir, windowsTestTargetHash); err == nil {
		t.Fatalf("Acquire(reparse lock) error = %v", err)
	}
}

func TestWindowsAcquireSeparatesTargetHashes(t *testing.T) {
	runtimeDir := newWindowsRuntimeDir(t)
	first, err := Acquire(runtimeDir, windowsTestTargetHash)
	if err != nil {
		t.Fatalf("Acquire(first) error = %v", err)
	}
	defer func() {
		if err := first.Close(); err != nil {
			t.Errorf("first Lock.Close() error = %v", err)
		}
	}()
	secondHash := "1123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	second, err := Acquire(runtimeDir, secondHash)
	if err != nil {
		t.Fatalf("Acquire(second target) error = %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("second Lock.Close() error = %v", err)
	}
}

func newWindowsRuntimeDir(t *testing.T) string {
	t.Helper()
	runtimeDir := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatalf("os.Mkdir(runtime) error = %v", err)
	}
	return runtimeDir
}

func createJunction(t *testing.T, junction, target string) {
	t.Helper()
	if output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", junction, target).CombinedOutput(); err != nil {
		t.Fatalf("create junction: %v: %s", err, output)
	}
}
