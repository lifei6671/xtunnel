//go:build linux

package gateway

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	rotationSIGKILLHelperEnv   = "XTUNNEL_GATEWAY_SIGKILL_HELPER"
	rotationSIGKILLDataDirEnv  = "XTUNNEL_GATEWAY_SIGKILL_DATA_DIR"
	rotationSIGKILLBoundaryEnv = "XTUNNEL_GATEWAY_SIGKILL_BOUNDARY"
)

func TestRotatePinnedIdentityRecoversAfterSIGKILL(t *testing.T) {
	if os.Getenv(rotationSIGKILLHelperEnv) == "1" {
		runRotationSIGKILLHelper()
		return
	}

	// t.TempDir 必须落在 Linux-native /tmp，避免把 WSL DrvFS 的 rename/fsync
	// 行为误当成 Linux Server 文件系统证据。
	t.Setenv("TMPDIR", "/tmp")
	tests := []struct {
		name              string
		boundary          string
		intermediateState persistedIdentityState
	}{
		{
			name:              "after key replacement",
			boundary:          "key",
			intermediateState: persistedIdentityRejectedMismatch,
		},
		{
			name:              "after certificate replacement",
			boundary:          "certificate",
			intermediateState: persistedIdentityNew,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dataDir := t.TempDir()
			relative, err := filepath.Rel("/tmp", dataDir)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				t.Fatalf("t.TempDir() = %q, want a directory below /tmp", dataDir)
			}

			now := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
			before, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, now)
			if err != nil {
				t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
			}

			runRotationUntilSIGKILLBoundary(t, dataDir, test.boundary)
			paths := identityPaths(dataDir)
			journal, exists, err := readJournal(dataDir, paths.journal)
			if err != nil || !exists {
				t.Fatalf("readJournal() after SIGKILL = %#v, %t, %v", journal, exists, err)
			}
			if journal.Version != 2 || journal.Audit == nil {
				t.Fatalf("rotation journal after SIGKILL = %#v, want valid v2 audit journal", journal)
			}
			if err := validateRotationAuditJournal(journal.Audit); err != nil {
				t.Fatalf("validateRotationAuditJournal() after SIGKILL error = %v", err)
			}
			assertPersistedIdentityState(
				t,
				dataDir,
				before.SPKIHash(),
				test.intermediateState,
				&journal.Audit.AfterStateDigest,
			)

			if err := RecoverRotation(dataDir); err != nil {
				t.Fatalf("RecoverRotation() after SIGKILL error = %v", err)
			}
			assertPersistedIdentityState(
				t,
				dataDir,
				before.SPKIHash(),
				persistedIdentityNew,
				&journal.Audit.AfterStateDigest,
			)
			for _, temporaryPath := range []string{paths.keyTemp, paths.certTemp} {
				if _, statErr := os.Stat(temporaryPath); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("recovered temporary file %q stat error = %v, want absent", temporaryPath, statErr)
				}
			}
		})
	}
}

func runRotationSIGKILLHelper() {
	dataDir := os.Getenv(rotationSIGKILLDataDirEnv)
	boundary := os.Getenv(rotationSIGKILLBoundaryEnv)
	ready := os.NewFile(uintptr(3), "rotation-sigkill-ready")
	block := os.NewFile(uintptr(4), "rotation-sigkill-block")
	if dataDir == "" || ready == nil || block == nil {
		os.Exit(70)
	}
	defer ready.Close()
	defer block.Close()

	paths := identityPaths(dataDir)
	fileOps := defaultRotationFileOps()
	rename := fileOps.rename
	fileOps.rename = func(oldPath, newPath string) error {
		if err := rename(oldPath, newPath); err != nil {
			return err
		}
		hitBoundary := (boundary == "key" && oldPath == paths.keyTemp && newPath == paths.key) ||
			(boundary == "certificate" && oldPath == paths.certTemp && newPath == paths.cert)
		if !hitBoundary {
			return nil
		}

		// 通知字节只在真实 os.Rename 成功后发送。随后子进程阻塞在父进程拥有的
		// Pipe 上，确保 SIGKILL 精确落在 rename 与后续目录 fsync 之间。
		if _, err := ready.Write([]byte{1}); err != nil {
			return fmt.Errorf("notify rotation crash boundary: %w", err)
		}
		var release [1]byte
		if _, err := block.Read(release[:]); err != nil {
			return fmt.Errorf("wait at rotation crash boundary: %w", err)
		}
		return errors.New("rotation crash barrier was unexpectedly released")
	}

	now := time.Date(2026, time.September, 1, 1, 0, 0, 0, time.UTC)
	audit := RotationAuditMetadata{
		EventID:     "evt_01K00000000000000000000021",
		OperationID: "op_01K00000000000000000000021",
		OccurredAt:  now.Unix(),
		ResourceID:  "gateway.example.test",
	}
	if _, err := rotatePinnedIdentity(dataDir, audit.ResourceID, now, audit, fileOps); err != nil {
		os.Exit(71)
	}
	os.Exit(72)
}

func runRotationUntilSIGKILLBoundary(t *testing.T, dataDir, boundary string) {
	t.Helper()
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe(ready) error = %v", err)
	}
	blockReader, blockWriter, err := os.Pipe()
	if err != nil {
		_ = readyReader.Close()
		_ = readyWriter.Close()
		t.Fatalf("os.Pipe(block) error = %v", err)
	}

	command := exec.Command(os.Args[0], "-test.run=^TestRotatePinnedIdentityRecoversAfterSIGKILL$")
	command.Env = append(os.Environ(),
		rotationSIGKILLHelperEnv+"=1",
		rotationSIGKILLDataDirEnv+"="+dataDir,
		rotationSIGKILLBoundaryEnv+"="+boundary,
	)
	command.ExtraFiles = []*os.File{readyWriter, blockReader}
	if err := command.Start(); err != nil {
		_ = readyReader.Close()
		_ = readyWriter.Close()
		_ = blockReader.Close()
		_ = blockWriter.Close()
		t.Fatalf("start rotation SIGKILL helper error = %v", err)
	}
	_ = readyWriter.Close()
	_ = blockReader.Close()

	// 父测试唯一拥有子进程与剩余 Pipe 端。Wait goroutine 只投递一次有界结果；
	// 任一路径都先 SIGKILL、等待回收，再关闭阻塞 Pipe，避免孤儿进程或泄漏 FD。
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- command.Wait()
	}()
	waited := false
	defer func() {
		_ = readyReader.Close()
		if !waited {
			_ = command.Process.Signal(syscall.SIGKILL)
			select {
			case <-waitDone:
				waited = true
			case <-time.After(5 * time.Second):
				t.Errorf("rotation SIGKILL helper did not exit during cleanup")
			}
		}
		_ = blockWriter.Close()
	}()

	if err := readyReader.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set rotation crash barrier deadline error = %v", err)
	}
	var signal [1]byte
	if _, err := io.ReadFull(readyReader, signal[:]); err != nil {
		t.Fatalf("wait for rotation crash boundary error = %v", err)
	}
	if signal[0] != 1 {
		t.Fatalf("rotation crash barrier signal = %d, want 1", signal[0])
	}
	if err := command.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("send SIGKILL to rotation helper error = %v", err)
	}

	var waitErr error
	select {
	case waitErr = <-waitDone:
		waited = true
	case <-time.After(5 * time.Second):
		t.Fatal("rotation SIGKILL helper did not exit after SIGKILL")
	}
	exitError, ok := waitErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("rotation SIGKILL helper Wait() error = %v, want signal exit", waitErr)
	}
	waitStatus, ok := exitError.Sys().(syscall.WaitStatus)
	if !ok || !waitStatus.Signaled() || waitStatus.Signal() != syscall.SIGKILL {
		t.Fatalf("rotation helper wait status = %#v, want SIGKILL", exitError.Sys())
	}
}
