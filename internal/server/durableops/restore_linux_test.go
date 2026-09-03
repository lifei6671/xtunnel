//go:build linux

package durableops

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	libsqlite "github.com/libtnb/sqlite"

	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	"github.com/lifei6671/xtunnel/internal/server/datadir"
	"github.com/lifei6671/xtunnel/migrations"
	"golang.org/x/sys/unix"
)

const (
	restoreSwitchSIGKILLHelperEnv   = "XTUNNEL_RESTORE_SWITCH_SIGKILL_HELPER"
	restoreSwitchSIGKILLTargetEnv   = "XTUNNEL_RESTORE_SWITCH_SIGKILL_TARGET"
	restoreSwitchSIGKILLArchiveEnv  = "XTUNNEL_RESTORE_SWITCH_SIGKILL_ARCHIVE"
	restoreSwitchSIGKILLBoundaryEnv = "XTUNNEL_RESTORE_SWITCH_SIGKILL_BOUNDARY"
	restoreSwitchSIGKILLPipeFD      = 3
)

func TestRestoreInstallsValidatedArchiveWithoutMerging(t *testing.T) {
	target := newRestoreTarget(t)
	writeSourceFile(t, target.Path, "obsolete", []byte("must disappear"), 0o600)
	archivePath := createPublicArchiveFile(t)

	manifest, err := Restore(context.Background(), target, archivePath, sqlite.CurrentSchemaVersion(), TLSModePublic)
	if err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	if manifest.TLSMode != TLSModePublic || manifest.SchemaVersion != sqlite.CurrentSchemaVersion() {
		t.Fatalf("Restore() manifest = %#v", manifest)
	}
	if err := sqlite.ValidateBackupDatabase(context.Background(), filepath.Join(target.Path, "xtunnel.db"), sqlite.CurrentSchemaVersion()); err != nil {
		t.Fatalf("ValidateBackupDatabase(restored) error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(target.Path, "obsolete")); !os.IsNotExist(err) {
		t.Fatalf("old target file was merged into restore: %v", err)
	}
	paths, err := pathsForTarget(target)
	if err != nil {
		t.Fatalf("pathsForTarget() error = %v", err)
	}
	for _, path := range []string{paths.staging, paths.rollback, paths.journal} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("restore artifact %q remained: %v", path, err)
		}
	}
}

func TestRestoreRejectsTLSModeMismatchWithoutChangingTarget(t *testing.T) {
	target := newRestoreTarget(t)
	writeSourceFile(t, target.Path, "sentinel", []byte("old"), 0o600)
	archivePath := createPublicArchiveFile(t)

	if _, err := Restore(context.Background(), target, archivePath, sqlite.CurrentSchemaVersion(), TLSModePinned); err == nil || !strings.Contains(err.Error(), "TLS mode") {
		t.Fatalf("Restore() error = %v, want TLS mode rejection", err)
	}
	assertFileContents(t, filepath.Join(target.Path, "sentinel"), "old")
}

func TestRestoreRejectsManifestValidButUnusableDatabase(t *testing.T) {
	target := newRestoreTarget(t)
	writeSourceFile(t, target.Path, "sentinel", []byte("old"), 0o600)
	database := []byte("not-a-sqlite-database")
	manifest := testPublicManifest(database, testMasterKey)
	manifest.SchemaVersion = sqlite.CurrentSchemaVersion()
	archiveData := buildTestArchive(t, manifest, []testTarEntry{
		{name: "xtunnel.db", mode: 0o600, typeflag: tar.TypeReg, data: database},
		{name: "credentials/tunnel-token.key", mode: 0o600, typeflag: tar.TypeReg, data: testMasterKey},
	})
	archivePath := filepath.Join(t.TempDir(), "invalid-state.tar")
	if err := os.WriteFile(archivePath, archiveData, 0o600); err != nil {
		t.Fatalf("os.WriteFile(invalid archive) error = %v", err)
	}
	if _, err := Restore(context.Background(), target, archivePath, sqlite.CurrentSchemaVersion(), TLSModePublic); err == nil || !strings.Contains(err.Error(), "SQLite") {
		t.Fatalf("Restore(invalid SQLite) error = %v, want SQLite validation failure", err)
	}
	assertFileContents(t, filepath.Join(target.Path, "sentinel"), "old")
}

func TestRestoreAcceptsSchemaBeforeTunnelTokensMigration(t *testing.T) {
	target := newRestoreTarget(t)
	databasePath := filepath.Join(t.TempDir(), "v1.db")
	pool, err := sql.Open(libsqlite.DriverName, databasePath)
	if err != nil {
		t.Fatalf("sql.Open(v1 database) error = %v", err)
	}
	if _, err := pool.ExecContext(context.Background(), migrations.SchemaMigrations); err != nil {
		_ = pool.Close()
		t.Fatalf("create v1 schema error = %v", err)
	}
	if _, err := pool.ExecContext(context.Background(),
		"INSERT INTO schema_migrations(version, applied_at) VALUES (1, 1)",
	); err != nil {
		_ = pool.Close()
		t.Fatalf("record v1 migration error = %v", err)
	}
	if err := pool.Close(); err != nil {
		t.Fatalf("close v1 database error = %v", err)
	}
	database, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatalf("os.ReadFile(v1 database) error = %v", err)
	}
	manifest := testPublicManifest(database, testMasterKey)
	manifest.SchemaVersion = 1
	archiveData := buildTestArchive(t, manifest, []testTarEntry{
		{name: "xtunnel.db", mode: 0o600, typeflag: tar.TypeReg, data: database},
		{name: "credentials/tunnel-token.key", mode: 0o600, typeflag: tar.TypeReg, data: testMasterKey},
	})
	archivePath := filepath.Join(t.TempDir(), "v1.tar")
	if err := os.WriteFile(archivePath, archiveData, 0o600); err != nil {
		t.Fatalf("os.WriteFile(v1 archive) error = %v", err)
	}
	manifest, err = Restore(context.Background(), target, archivePath, sqlite.CurrentSchemaVersion(), TLSModePublic)
	if err != nil {
		t.Fatalf("Restore(v1) error = %v", err)
	}
	if manifest.SchemaVersion != 1 {
		t.Fatalf("Restore(v1) schema version = %d, want 1", manifest.SchemaVersion)
	}
	version, err := sqlite.InspectSchemaVersion(context.Background(), filepath.Join(target.Path, "xtunnel.db"))
	if err != nil || version != 1 {
		t.Fatalf("InspectSchemaVersion(restored v1) = %d, %v", version, err)
	}
}

func TestRestoreRejectsBytesAfterCanonicalTarEnd(t *testing.T) {
	target := newRestoreTarget(t)
	archivePath := createPublicArchiveFile(t)
	archive, err := os.OpenFile(archivePath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("os.OpenFile(archive append) error = %v", err)
	}
	if _, err := archive.Write([]byte("undeclared")); err != nil {
		t.Fatalf("append undeclared archive bytes error = %v", err)
	}
	if err := archive.Close(); err != nil {
		t.Fatalf("close appended archive error = %v", err)
	}
	if _, err := Restore(context.Background(), target, archivePath, sqlite.CurrentSchemaVersion(), TLSModePublic); err == nil || !strings.Contains(err.Error(), "undeclared trailing bytes") {
		t.Fatalf("Restore(trailing bytes) error = %v", err)
	}
}

func TestRestoreDirectorySwitchSIGKILLRecovers(t *testing.T) {
	if os.Getenv(restoreSwitchSIGKILLHelperEnv) == "1" {
		runRestoreSwitchSIGKILLHelper(t)
		return
	}

	// Restore 崩溃证据只在 Linux-native /tmp 中构造，避免把 DrvFS/v9fs
	// 的可见性和持久化语义混入目录切换结论。
	t.Setenv("TMPDIR", "/tmp")
	if temporaryRoot := filepath.Clean(os.TempDir()); temporaryRoot != "/tmp" {
		t.Fatalf("os.TempDir() = %q, want /tmp", temporaryRoot)
	}

	tests := []struct {
		name               string
		boundary           string
		wantBarrier        byte
		wantJournalPhase   restorePhase
		wantTargetBefore   bool
		wantStagingBefore  bool
		wantRollbackBefore bool
		wantAdmin          bool
	}{
		{
			name:               "target to rollback rename restores old target",
			boundary:           "target-to-rollback",
			wantBarrier:        1,
			wantJournalPhase:   phasePrepared,
			wantStagingBefore:  true,
			wantRollbackBefore: true,
			wantAdmin:          true,
		},
		{
			name:               "staging to target rename completes new target",
			boundary:           "staging-to-target",
			wantBarrier:        2,
			wantJournalPhase:   phaseRollbackReady,
			wantTargetBefore:   true,
			wantRollbackBefore: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parent := t.TempDir()
			target, err := datadir.Resolve(filepath.Join(parent, "data"))
			if err != nil {
				t.Fatalf("datadir.Resolve() error = %v", err)
			}
			writeValidStateDirectory(t, target.Path, true)
			archivePath := createPublicArchiveFile(t)
			paths, err := pathsForTarget(target)
			if err != nil {
				t.Fatalf("pathsForTarget() error = %v", err)
			}

			readPipe, writePipe, err := os.Pipe()
			if err != nil {
				t.Fatalf("os.Pipe() error = %v", err)
			}
			command := exec.Command(os.Args[0], "-test.run=^TestRestoreDirectorySwitchSIGKILLRecovers$")
			command.Env = append(os.Environ(),
				restoreSwitchSIGKILLHelperEnv+"=1",
				restoreSwitchSIGKILLTargetEnv+"="+target.Path,
				restoreSwitchSIGKILLArchiveEnv+"="+archivePath,
				restoreSwitchSIGKILLBoundaryEnv+"="+test.boundary,
			)
			command.ExtraFiles = []*os.File{writePipe}
			var output bytes.Buffer
			command.Stdout = &output
			command.Stderr = &output
			if err := command.Start(); err != nil {
				_ = readPipe.Close()
				_ = writePipe.Close()
				t.Fatalf("start Restore SIGKILL helper error = %v", err)
			}
			// 父测试是 helper 和 pipe 的唯一 owner；任何断言路径都会先杀死并
			// Wait 子进程，避免超时或 pipe 错误留下阻塞的 Restore。
			waited := false
			abortChild := func() error {
				if waited {
					return nil
				}
				killErr := command.Process.Kill()
				waitErr := command.Wait()
				waited = true
				return errors.Join(killErr, waitErr)
			}
			t.Cleanup(func() {
				_ = readPipe.Close()
				_ = writePipe.Close()
				_ = abortChild()
			})
			if err := writePipe.Close(); err != nil {
				stopErr := abortChild()
				t.Fatalf("close parent Restore SIGKILL barrier writer error = %v; stop helper = %v; output = %s", err, stopErr, output.String())
			}
			if err := readPipe.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
				stopErr := abortChild()
				t.Fatalf("set Restore SIGKILL barrier deadline error = %v; stop helper = %v; output = %s", err, stopErr, output.String())
			}
			var barrier [1]byte
			if _, err := io.ReadFull(readPipe, barrier[:]); err != nil {
				stopErr := abortChild()
				t.Fatalf("wait for Restore SIGKILL barrier error = %v; stop helper = %v; output = %s", err, stopErr, output.String())
			}
			if barrier[0] != test.wantBarrier {
				stopErr := abortChild()
				t.Fatalf("Restore SIGKILL barrier = %d, want %d; stop helper = %v; output = %s", barrier[0], test.wantBarrier, stopErr, output.String())
			}
			if err := command.Process.Kill(); err != nil {
				stopErr := abortChild()
				t.Fatalf("kill Restore switch helper error = %v; stop helper = %v; output = %s", err, stopErr, output.String())
			}
			waitErr := command.Wait()
			waited = true
			var exitErr *exec.ExitError
			if !errors.As(waitErr, &exitErr) {
				t.Fatalf("Restore switch helper Wait() error = %v, want SIGKILL; output = %s", waitErr, output.String())
			}
			waitStatus, ok := exitErr.Sys().(syscall.WaitStatus)
			if !ok || !waitStatus.Signaled() || waitStatus.Signal() != syscall.SIGKILL {
				t.Fatalf("Restore switch helper WaitStatus = %#v, want SIGKILL; output = %s", exitErr.Sys(), output.String())
			}

			beforeRecovery := []struct {
				name   string
				path   string
				exists bool
			}{
				{name: "target", path: paths.target, exists: test.wantTargetBefore},
				{name: "staging", path: paths.staging, exists: test.wantStagingBefore},
				{name: "rollback", path: paths.rollback, exists: test.wantRollbackBefore},
				{name: "journal", path: paths.journal, exists: true},
			}
			for _, state := range beforeRecovery {
				_, statErr := os.Lstat(state.path)
				if state.exists && statErr != nil {
					t.Fatalf("Restore artifact %s before recovery missing: %v", state.name, statErr)
				}
				if !state.exists && !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("Restore artifact %s before recovery exists or stat failed: %v", state.name, statErr)
				}
			}
			journalData, err := os.ReadFile(paths.journal)
			if err != nil {
				t.Fatalf("os.ReadFile(Restore SIGKILL journal) error = %v", err)
			}
			journal, err := parseJournal(journalData, paths)
			if err != nil {
				t.Fatalf("parseJournal(Restore SIGKILL) error = %v", err)
			}
			if journal.Phase != test.wantJournalPhase {
				t.Fatalf("Restore SIGKILL journal phase = %q, want %q", journal.Phase, test.wantJournalPhase)
			}
			if journal.Version != restoreJournalVersionV2 {
				t.Fatalf("Restore SIGKILL journal version = %d, want %d", journal.Version, restoreJournalVersionV2)
			}

			recovered, err := RecoverPendingRestore(context.Background(), target)
			if err != nil {
				t.Fatalf("RecoverPendingRestore(after SIGKILL) error = %v", err)
			}
			if !recovered {
				t.Fatal("RecoverPendingRestore(after SIGKILL) recovered = false, want true")
			}
			assertStateHasAdmin(t, paths.target, test.wantAdmin)
			for _, path := range []string{paths.staging, paths.rollback, paths.journal} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("Restore artifact %q remained after SIGKILL recovery: %v", path, err)
				}
			}
		})
	}
}

func runRestoreSwitchSIGKILLHelper(t *testing.T) {
	t.Helper()
	targetPath := os.Getenv(restoreSwitchSIGKILLTargetEnv)
	archivePath := os.Getenv(restoreSwitchSIGKILLArchiveEnv)
	if targetPath == "" || archivePath == "" {
		t.Fatal("Restore SIGKILL helper paths are missing")
	}
	crashAfterRename := 0
	switch os.Getenv(restoreSwitchSIGKILLBoundaryEnv) {
	case "target-to-rollback":
		crashAfterRename = 1
	case "staging-to-target":
		crashAfterRename = 2
	default:
		t.Fatal("Restore SIGKILL helper boundary is invalid")
	}
	target, err := datadir.Resolve(targetPath)
	if err != nil {
		t.Fatalf("datadir.Resolve(Restore SIGKILL helper) error = %v", err)
	}
	paths, err := pathsForTarget(target)
	if err != nil {
		t.Fatalf("pathsForTarget(Restore SIGKILL helper) error = %v", err)
	}
	barrier := os.NewFile(uintptr(restoreSwitchSIGKILLPipeFD), "restore-switch-sigkill-barrier")
	if barrier == nil {
		t.Fatal("Restore SIGKILL helper barrier is unavailable")
	}
	defer func() { _ = barrier.Close() }()

	// 先执行真实 rename，再通知父进程并阻塞。父进程因此杀死的是
	// 目录项已切换、但该次父目录 fsync 尚未开始的真实子进程。
	switchOps := productionRestoreSwitchOps()
	renameCalls := 0
	switchOps.rename = func(oldPath, newPath string) error {
		if err := os.Rename(oldPath, newPath); err != nil {
			return err
		}
		renameCalls++
		if renameCalls != crashAfterRename {
			return nil
		}
		if _, err := barrier.Write([]byte{byte(renameCalls)}); err != nil {
			return err
		}
		select {}
	}
	if _, err := restorePlatformWithSwitchOps(
		context.Background(), paths, archivePath, sqlite.CurrentSchemaVersion(), TLSModePublic, switchOps,
	); err != nil {
		t.Fatalf("restorePlatformWithSwitchOps(SIGKILL helper) error = %v", err)
	}
	t.Fatal("restorePlatformWithSwitchOps(SIGKILL helper) returned before SIGKILL")
}

func TestRecoverPendingRestoreConvergesInterruptedRenameStates(t *testing.T) {
	rollbackTargetAdmin := false
	tests := []struct {
		name           string
		phase          restorePhase
		targetExists   bool
		stagingExists  bool
		rollbackExists bool
		targetAdmin    *bool
		wantAdmin      bool
	}{
		{name: "prepared before first rename rolls back staging", phase: phasePrepared, targetExists: true, stagingExists: true, wantAdmin: true},
		{name: "prepared after first rename restores rollback", phase: phasePrepared, stagingExists: true, rollbackExists: true, wantAdmin: true},
		{name: "prepared after staging cleanup removes journal", phase: phasePrepared, targetExists: true, wantAdmin: true},
		{name: "rollback ready before second rename restores rollback", phase: phaseRollbackReady, stagingExists: true, rollbackExists: true, wantAdmin: true},
		{name: "rollback ready after second rename finishes install", phase: phaseRollbackReady, targetExists: true, rollbackExists: true},
		{name: "rollback ready after invalid target removal restores rollback", phase: phaseRollbackReady, rollbackExists: true, wantAdmin: true},
		{name: "rollback restoring before invalid target removal restores rollback", phase: phaseRollbackRestoring, targetExists: true, rollbackExists: true, targetAdmin: &rollbackTargetAdmin, wantAdmin: true},
		{name: "rollback restoring after cleanup removes journal", phase: phaseRollbackRestoring, targetExists: true, wantAdmin: true},
		{name: "installed cleans rollback", phase: phaseInstalled, targetExists: true, rollbackExists: true},
		{name: "installed after invalid target removal restores rollback", phase: phaseInstalled, rollbackExists: true, wantAdmin: true},
		{name: "installed after rollback cleanup removes journal", phase: phaseInstalled, targetExists: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parent := t.TempDir()
			target, err := datadir.Resolve(filepath.Join(parent, "data"))
			if err != nil {
				t.Fatalf("datadir.Resolve() error = %v", err)
			}
			paths, err := pathsForTarget(target)
			if err != nil {
				t.Fatalf("pathsForTarget() error = %v", err)
			}
			var manifest Manifest
			if test.targetExists {
				targetAdmin := test.wantAdmin
				if test.targetAdmin != nil {
					targetAdmin = *test.targetAdmin
				}
				manifest = writeValidStateDirectory(t, paths.target, targetAdmin)
			}
			if test.stagingExists {
				manifest = writeValidStateDirectory(t, paths.staging, false)
			}
			if test.rollbackExists {
				rollbackManifest := writeValidStateDirectory(t, paths.rollback, true)
				if manifest.FormatVersion == 0 {
					// rollback-only 表示恢复新 target 的清理阶段再次崩溃；真实
					// Journal 仍携带原新状态 Manifest，这里只需提供合法绑定。
					manifest = rollbackManifest
				}
			}
			manifestData, err := json.Marshal(manifest)
			if err != nil {
				t.Fatalf("json.Marshal(manifest) error = %v", err)
			}
			manifestDigest := sha256.Sum256(manifestData)
			journal := restoreJournal{
				Version: restoreJournalVersion, ManifestSHA256: hex.EncodeToString(manifestDigest[:]), Manifest: manifest, StableTarget: paths.target,
				Staging: paths.staging, Rollback: paths.rollback, Phase: test.phase,
			}
			if err := writeJournal(paths, journal, os.Getuid(), os.Getgid()); err != nil {
				t.Fatalf("writeJournal() error = %v", err)
			}

			recovered, err := RecoverPendingRestore(context.Background(), target)
			if err != nil {
				t.Fatalf("RecoverPendingRestore() error = %v", err)
			}
			if !recovered {
				t.Fatal("RecoverPendingRestore() recovered = false, want true")
			}
			assertStateHasAdmin(t, paths.target, test.wantAdmin)
			for _, path := range []string{paths.staging, paths.rollback, paths.journal} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("recovery artifact %q remained: %v", path, err)
				}
			}
		})
	}
}

func TestRecoverPendingRestoreRejectsUnprovableTargetOnlyJournal(t *testing.T) {
	for _, test := range []struct {
		name    string
		version int
		phase   restorePhase
	}{
		{name: "version 1 rollback ready", version: restoreJournalVersionV1, phase: phaseRollbackReady},
		{name: "version 1 installed", version: restoreJournalVersionV1, phase: phaseInstalled},
		{name: "version 2 rollback ready", version: restoreJournalVersionV2, phase: phaseRollbackReady},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := newRestoreTarget(t)
			paths, err := pathsForTarget(target)
			if err != nil {
				t.Fatalf("pathsForTarget() error = %v", err)
			}
			if err := os.Remove(paths.target); err != nil {
				t.Fatalf("os.Remove(empty target) error = %v", err)
			}
			manifest := writeValidStateDirectory(t, paths.target, false)
			manifestData, err := json.Marshal(manifest)
			if err != nil {
				t.Fatalf("json.Marshal(manifest) error = %v", err)
			}
			digest := sha256.Sum256(manifestData)
			journal := restoreJournal{
				Version: test.version, ManifestSHA256: hex.EncodeToString(digest[:]), Manifest: manifest,
				StableTarget: paths.target, Staging: paths.staging, Rollback: paths.rollback, Phase: test.phase,
			}
			if err := writeJournal(paths, journal, os.Getuid(), os.Getgid()); err != nil {
				t.Fatalf("writeJournal() error = %v", err)
			}
			before, err := os.ReadFile(paths.journal)
			if err != nil {
				t.Fatalf("ReadFile(Journal before recovery) error = %v", err)
			}

			if recovered, err := RecoverPendingRestore(context.Background(), target); err == nil || recovered {
				t.Fatalf("RecoverPendingRestore() = (%t, %v), want target-only rejection", recovered, err)
			}
			after, err := os.ReadFile(paths.journal)
			if err != nil {
				t.Fatalf("ReadFile(Journal after recovery) error = %v", err)
			}
			if !bytes.Equal(after, before) {
				t.Fatal("target-only Journal changed after rejected recovery")
			}
			assertStateHasAdmin(t, paths.target, false)
		})
	}
}

func TestWriteJournalFilesystemFailpointsPreserveRecoverablePhase(t *testing.T) {
	tests := []struct {
		name      string
		inject    func(*journalWriteOps)
		wantErr   error
		wantPhase restorePhase
	}{
		{
			name: "temporary write disk full keeps old phase",
			inject: func(ops *journalWriteOps) {
				ops.write = func(*os.File, []byte) (int, error) { return 0, syscall.ENOSPC }
			},
			wantErr:   syscall.ENOSPC,
			wantPhase: phasePrepared,
		},
		{
			name: "temporary fsync EIO keeps old phase",
			inject: func(ops *journalWriteOps) {
				ops.syncFile = func(*os.File) error { return syscall.EIO }
			},
			wantErr:   syscall.EIO,
			wantPhase: phasePrepared,
		},
		{
			name: "publish rename EIO keeps old phase",
			inject: func(ops *journalWriteOps) {
				ops.rename = func(string, string) error { return syscall.EIO }
			},
			wantErr:   syscall.EIO,
			wantPhase: phasePrepared,
		},
		{
			name: "parent fsync EIO leaves new visible phase",
			inject: func(ops *journalWriteOps) {
				ops.syncParent = func(string) error { return syscall.EIO }
			},
			wantErr:   syscall.EIO,
			wantPhase: phaseRollbackReady,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parent := t.TempDir()
			target, err := datadir.Resolve(filepath.Join(parent, "data"))
			if err != nil {
				t.Fatalf("datadir.Resolve() error = %v", err)
			}
			paths, err := pathsForTarget(target)
			if err != nil {
				t.Fatalf("pathsForTarget() error = %v", err)
			}
			manifest := writeValidStateDirectory(t, paths.target, false)
			writeRestoreJournalForTest(t, paths, manifest, phasePrepared)

			manifestData, err := json.Marshal(manifest)
			if err != nil {
				t.Fatalf("json.Marshal(manifest) error = %v", err)
			}
			digest := sha256.Sum256(manifestData)
			journal := restoreJournal{
				Version: restoreJournalVersion, ManifestSHA256: hex.EncodeToString(digest[:]), Manifest: manifest,
				StableTarget: paths.target, Staging: paths.staging, Rollback: paths.rollback,
				Phase: phaseRollbackReady,
			}
			ops := productionJournalWriteOps()
			test.inject(&ops)
			err = writeJournalWithOps(paths, journal, os.Getuid(), os.Getgid(), ops)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("writeJournalWithOps() error = %v, want %v", err, test.wantErr)
			}
			data, err := os.ReadFile(paths.journal)
			if err != nil {
				t.Fatalf("os.ReadFile(journal) error = %v", err)
			}
			got, err := parseJournal(data, paths)
			if err != nil {
				t.Fatalf("parseJournal() error = %v", err)
			}
			if got.Phase != test.wantPhase {
				t.Fatalf("journal phase = %q, want %q", got.Phase, test.wantPhase)
			}
			entries, err := os.ReadDir(parent)
			if err != nil {
				t.Fatalf("os.ReadDir(parent) error = %v", err)
			}
			temporaryPrefix := filepath.Base(paths.journal) + ".tmp-"
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), temporaryPrefix) {
					t.Fatalf("journal failpoint left temporary file %q", entry.Name())
				}
			}
		})
	}
}

func TestRecoverPendingRestoreRollsBackCorruptInstalledTarget(t *testing.T) {
	target := newRestoreTarget(t)
	paths, err := pathsForTarget(target)
	if err != nil {
		t.Fatalf("pathsForTarget() error = %v", err)
	}
	if err := os.Remove(paths.target); err != nil {
		t.Fatalf("os.Remove(empty target) error = %v", err)
	}
	manifest := writeValidStateDirectory(t, paths.target, false)
	writeValidStateDirectory(t, paths.rollback, true)
	if err := os.WriteFile(filepath.Join(paths.target, "xtunnel.db"), []byte("corrupt"), 0o600); err != nil {
		t.Fatalf("corrupt installed database error = %v", err)
	}
	writeRestoreJournalForTest(t, paths, manifest, phaseRollbackReady)
	recovered, err := RecoverPendingRestore(context.Background(), target)
	if err != nil || !recovered {
		t.Fatalf("RecoverPendingRestore(corrupt target) = %t, %v", recovered, err)
	}
	assertStateHasAdmin(t, paths.target, true)
}

func TestRecoverPendingRestoreRollsBackInstalledTargetWithWrongTokenKey(t *testing.T) {
	target := newRestoreTarget(t)
	paths, err := pathsForTarget(target)
	if err != nil {
		t.Fatalf("pathsForTarget() error = %v", err)
	}
	if err := os.Remove(paths.target); err != nil {
		t.Fatalf("os.Remove(empty target) error = %v", err)
	}
	if err := os.Mkdir(paths.target, 0o700); err != nil {
		t.Fatalf("os.Mkdir(target) error = %v", err)
	}
	initializeValidBackupState(t, paths.target, false)
	seedValidTunnelToken(t, paths.target)
	writeSourceFile(t, paths.target, "credentials/tunnel-token.key", bytes.Repeat([]byte{0xA7}, 32), 0o600)
	manifest := manifestForExistingState(t, paths.target, false)
	writeValidStateDirectory(t, paths.rollback, true)
	writeRestoreJournalForTest(t, paths, manifest, phaseInstalled)

	recovered, err := RecoverPendingRestore(context.Background(), target)
	if err != nil || !recovered {
		t.Fatalf("RecoverPendingRestore(wrong Token Key) = %t, %v", recovered, err)
	}
	assertStateHasAdmin(t, paths.target, true)
}

func TestRecoverPendingRestoreCancellationPreservesInstalledTarget(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "canceled", err: context.Canceled},
		{name: "deadline exceeded", err: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := newRestoreTarget(t)
			paths, err := pathsForTarget(target)
			if err != nil {
				t.Fatalf("pathsForTarget() error = %v", err)
			}
			if err := os.Remove(paths.target); err != nil {
				t.Fatalf("os.Remove(empty target) error = %v", err)
			}
			manifest := writeValidStateDirectory(t, paths.target, false)
			writeValidStateDirectory(t, paths.rollback, true)
			writeRestoreJournalForTest(t, paths, manifest, phaseInstalled)

			ctx := &failWhenObservedContext{done: make(chan struct{}), err: test.err}
			recovered, err := RecoverPendingRestore(ctx, target)
			if recovered || !errors.Is(err, test.err) {
				t.Fatalf("RecoverPendingRestore(installed) = %t, %v, want false, %v", recovered, err, test.err)
			}
			assertStateHasAdmin(t, paths.target, false)
			assertStateHasAdmin(t, paths.rollback, true)
			if _, err := os.Lstat(paths.journal); err != nil {
				t.Fatalf("installed restore journal was not preserved: %v", err)
			}
		})
	}
}

func TestFinishInstalledSyncsRollbackRemovalBeforeDeletingJournal(t *testing.T) {
	target := newRestoreTarget(t)
	paths, err := pathsForTarget(target)
	if err != nil {
		t.Fatalf("pathsForTarget() error = %v", err)
	}
	writeValidStateDirectory(t, paths.rollback, true)
	if err := os.WriteFile(paths.journal, []byte("journal"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(journal) error = %v", err)
	}

	syncCalls := 0
	err = finishInstalledWithSync(paths, func(parent string) error {
		syncCalls++
		if parent != filepath.Dir(paths.target) {
			t.Fatalf("sync parent = %q, want %q", parent, filepath.Dir(paths.target))
		}
		if _, err := os.Lstat(paths.rollback); !os.IsNotExist(err) {
			t.Fatalf("sync call %d observed rollback: %v", syncCalls, err)
		}
		_, journalErr := os.Lstat(paths.journal)
		if syncCalls == 1 && journalErr != nil {
			t.Fatalf("first sync did not preserve journal: %v", journalErr)
		}
		if syncCalls == 2 && !os.IsNotExist(journalErr) {
			t.Fatalf("second sync observed journal: %v", journalErr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("finishInstalledWithSync() error = %v", err)
	}
	if syncCalls != 2 {
		t.Fatalf("finishInstalledWithSync() sync calls = %d, want 2", syncCalls)
	}
}

func TestRollbackPreparedSyncsStagingRemovalBeforeDeletingJournal(t *testing.T) {
	target := newRestoreTarget(t)
	paths, err := pathsForTarget(target)
	if err != nil {
		t.Fatalf("pathsForTarget() error = %v", err)
	}
	writeValidStateDirectory(t, paths.staging, false)
	if err := os.WriteFile(paths.journal, []byte("journal"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(journal) error = %v", err)
	}

	syncCalls := 0
	err = rollbackPreparedWithSync(paths, func(parent string) error {
		syncCalls++
		if parent != filepath.Dir(paths.target) {
			t.Fatalf("sync parent = %q, want %q", parent, filepath.Dir(paths.target))
		}
		if _, err := os.Lstat(paths.staging); !os.IsNotExist(err) {
			t.Fatalf("sync call %d observed staging: %v", syncCalls, err)
		}
		_, journalErr := os.Lstat(paths.journal)
		if syncCalls == 1 && journalErr != nil {
			t.Fatalf("first sync did not preserve journal: %v", journalErr)
		}
		if syncCalls == 2 && !os.IsNotExist(journalErr) {
			t.Fatalf("second sync observed journal: %v", journalErr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("rollbackPreparedWithSync() error = %v", err)
	}
	if syncCalls != 2 {
		t.Fatalf("rollbackPreparedWithSync() sync calls = %d, want 2", syncCalls)
	}
}

func TestRollbackPreparedSyncFailureRemainsRecoverable(t *testing.T) {
	target := newRestoreTarget(t)
	paths, err := pathsForTarget(target)
	if err != nil {
		t.Fatalf("pathsForTarget() error = %v", err)
	}
	if err := os.Remove(paths.target); err != nil {
		t.Fatalf("os.Remove(empty target) error = %v", err)
	}
	writeValidStateDirectory(t, paths.target, true)
	manifest := writeValidStateDirectory(t, paths.staging, false)
	writeRestoreJournalForTest(t, paths, manifest, phasePrepared)

	syncCalls := 0
	err = rollbackPreparedWithSync(paths, func(string) error {
		syncCalls++
		if syncCalls == 1 {
			return errors.New("simulated sync failure after staging removal")
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "simulated sync failure") {
		t.Fatalf("rollbackPreparedWithSync(sync failure) error = %v", err)
	}
	if _, err := os.Lstat(paths.staging); !os.IsNotExist(err) {
		t.Fatalf("failed rollback left staging: %v", err)
	}
	if _, err := os.Lstat(paths.journal); err != nil {
		t.Fatalf("failed rollback did not preserve journal: %v", err)
	}

	recovered, err := RecoverPendingRestore(context.Background(), target)
	if err != nil || !recovered {
		t.Fatalf("RecoverPendingRestore(after sync failure) = %t, %v", recovered, err)
	}
	assertStateHasAdmin(t, paths.target, true)
	if _, err := os.Lstat(paths.journal); !os.IsNotExist(err) {
		t.Fatalf("recovered prepared journal remains: %v", err)
	}
}

func TestRecoverWithoutJournalCleansOnlySafeOrphanStaging(t *testing.T) {
	target := newRestoreTarget(t)
	paths, err := pathsForTarget(target)
	if err != nil {
		t.Fatalf("pathsForTarget() error = %v", err)
	}
	writeValidStateDirectory(t, paths.staging, false)
	recovered, err := RecoverPendingRestore(context.Background(), target)
	if err != nil || !recovered {
		t.Fatalf("RecoverPendingRestore(orphan staging) = %t, %v", recovered, err)
	}
	if _, err := os.Lstat(paths.staging); !os.IsNotExist(err) {
		t.Fatalf("orphan staging remains: %v", err)
	}
	writeValidStateDirectory(t, paths.rollback, true)
	if _, err := RecoverPendingRestore(context.Background(), target); err == nil || !strings.Contains(err.Error(), "without a journal") {
		t.Fatalf("RecoverPendingRestore(orphan rollback) error = %v", err)
	}
	assertStateHasAdmin(t, paths.rollback, true)
}

func TestRecoverWithoutJournalCleansOnlyRegularPrivateJournalTemps(t *testing.T) {
	t.Run("regular private temporary journal", func(t *testing.T) {
		target := newRestoreTarget(t)
		paths, err := pathsForTarget(target)
		if err != nil {
			t.Fatalf("pathsForTarget() error = %v", err)
		}
		temporary := paths.journal + ".tmp-crash"
		if err := os.WriteFile(temporary, []byte("partial"), 0o600); err != nil {
			t.Fatalf("os.WriteFile(temporary journal) error = %v", err)
		}

		recovered, err := RecoverPendingRestore(context.Background(), target)
		if err != nil || !recovered {
			t.Fatalf("RecoverPendingRestore(temporary journal) = %t, %v", recovered, err)
		}
		if _, err := os.Lstat(temporary); !os.IsNotExist(err) {
			t.Fatalf("temporary journal remains: %v", err)
		}
	})

	t.Run("symbolic link temporary journal fails closed", func(t *testing.T) {
		target := newRestoreTarget(t)
		paths, err := pathsForTarget(target)
		if err != nil {
			t.Fatalf("pathsForTarget() error = %v", err)
		}
		outside := filepath.Join(filepath.Dir(paths.target), "outside-journal")
		if err := os.WriteFile(outside, []byte("must-survive"), 0o600); err != nil {
			t.Fatalf("os.WriteFile(outside journal) error = %v", err)
		}
		temporary := paths.journal + ".tmp-link"
		if err := os.Symlink(outside, temporary); err != nil {
			t.Fatalf("os.Symlink(temporary journal) error = %v", err)
		}

		if _, err := RecoverPendingRestore(context.Background(), target); err == nil || !strings.Contains(err.Error(), "regular 0600") {
			t.Fatalf("RecoverPendingRestore(symbolic temporary journal) error = %v", err)
		}
		assertFileContents(t, outside, "must-survive")
		if _, err := os.Lstat(temporary); err != nil {
			t.Fatalf("suspicious temporary journal was not preserved: %v", err)
		}
	})
}

func TestRemoveDirectoryTreeRefusesNestedBindMount(t *testing.T) {
	root := filepath.Join(t.TempDir(), "rollback")
	nested := filepath.Join(root, "nested")
	outside := t.TempDir()
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatalf("os.MkdirAll(nested) error = %v", err)
	}
	writeSourceFile(t, root, "a-before-mount", []byte("old-data"), 0o600)
	writeSourceFile(t, outside, "sentinel", []byte("outside"), 0o600)
	if err := unix.Mount(outside, nested, "", unix.MS_BIND, ""); err != nil {
		if err == unix.EPERM || err == unix.EACCES {
			t.Skipf("bind mount is unavailable: %v", err)
		}
		t.Fatalf("unix.Mount(bind) error = %v", err)
	}
	t.Cleanup(func() {
		if err := unix.Unmount(nested, unix.MNT_DETACH); err != nil {
			t.Errorf("unix.Unmount(bind) error = %v", err)
		}
	})
	if err := removeDirectoryTree(root); err == nil || !strings.Contains(err.Error(), "mount boundary") {
		t.Fatalf("removeDirectoryTree(bind mount) error = %v", err)
	}
	assertFileContents(t, filepath.Join(outside, "sentinel"), "outside")
	assertFileContents(t, filepath.Join(root, "a-before-mount"), "old-data")
}

func TestRecoverPendingRestoreRejectsMalformedAndSymbolicLinkJournals(t *testing.T) {
	tests := []struct {
		name  string
		write func(t *testing.T, paths restorePaths)
		want  string
	}{
		{
			name: "malformed",
			write: func(t *testing.T, paths restorePaths) {
				t.Helper()
				if err := os.WriteFile(paths.journal, []byte("{"), 0o600); err != nil {
					t.Fatalf("os.WriteFile(journal) error = %v", err)
				}
			},
			want: "parse restore journal",
		},
		{
			name: "symbolic link",
			write: func(t *testing.T, paths restorePaths) {
				t.Helper()
				outside := filepath.Join(filepath.Dir(paths.target), "outside")
				if err := os.WriteFile(outside, []byte("{}"), 0o600); err != nil {
					t.Fatalf("os.WriteFile(outside) error = %v", err)
				}
				if err := os.Symlink(outside, paths.journal); err != nil {
					t.Fatalf("os.Symlink(journal) error = %v", err)
				}
			},
			want: "non-symbolic-link",
		},
		{
			name: "oversized",
			write: func(t *testing.T, paths restorePaths) {
				t.Helper()
				if err := os.WriteFile(paths.journal, bytes.Repeat([]byte("x"), maxRestoreJournalSize+1), 0o600); err != nil {
					t.Fatalf("os.WriteFile(journal) error = %v", err)
				}
			},
			want: "maximum size",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := newRestoreTarget(t)
			paths, err := pathsForTarget(target)
			if err != nil {
				t.Fatalf("pathsForTarget() error = %v", err)
			}
			test.write(t, paths)
			if _, err := RecoverPendingRestore(context.Background(), target); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("RecoverPendingRestore() error = %v, want %q", err, test.want)
			}
		})
	}
}

func newRestoreTarget(t *testing.T) datadir.Target {
	t.Helper()
	parent := t.TempDir()
	target, err := datadir.Resolve(filepath.Join(parent, "data"))
	if err != nil {
		t.Fatalf("datadir.Resolve() error = %v", err)
	}
	if err := os.Mkdir(target.Path, 0o700); err != nil {
		t.Fatalf("os.Mkdir(target) error = %v", err)
	}
	return target
}

func createPublicArchiveFile(t *testing.T) string {
	t.Helper()
	source := t.TempDir()
	initializeValidBackupState(t, source, false)
	var output bytes.Buffer
	if _, _, err := createArchive(context.Background(), &output, source, sqlite.CurrentSchemaVersion(), TLSModePublic); err != nil {
		t.Fatalf("createArchive(valid public state) error = %v", err)
	}
	path := filepath.Join(t.TempDir(), "backup.tar")
	if err := os.WriteFile(path, output.Bytes(), 0o600); err != nil {
		t.Fatalf("os.WriteFile(backup) error = %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("os.Chmod(backup) error = %v", err)
	}
	return path
}

func writeValidStateDirectory(t *testing.T, path string, withAdmin bool) Manifest {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("os.Mkdir(%q) error = %v", path, err)
	}
	initializeValidBackupState(t, path, false)
	if withAdmin {
		store, err := sqlite.Open(context.Background(), path)
		if err != nil {
			t.Fatalf("sqlite.Open(state admin) error = %v", err)
		}
		if err := store.CreateFirstAdmin(context.Background(), "old-admin", "old-password"); err != nil {
			t.Fatalf("CreateFirstAdmin(state admin) error = %v", err)
		}
		if err := store.Close(); err != nil {
			t.Fatalf("Store.Close(state admin) error = %v", err)
		}
	}
	manifest := Manifest{FormatVersion: FormatVersion, SchemaVersion: sqlite.CurrentSchemaVersion(), TLSMode: TLSModePublic}
	for _, rule := range archiveFileRules {
		if rule.pinned {
			continue
		}
		filePath := filepath.Join(path, filepath.FromSlash(rule.path))
		info, err := os.Lstat(filePath)
		if err != nil {
			t.Fatalf("os.Lstat(state file) error = %v", err)
		}
		digest, size, err := digestFile(filePath, info)
		if err != nil {
			t.Fatalf("digestFile(state file) error = %v", err)
		}
		manifest.Files = append(manifest.Files, ManifestFile{Path: rule.path, Size: size, Mode: rule.mode, SHA256: digest})
	}
	return manifest
}

func writeRestoreJournalForTest(t *testing.T, paths restorePaths, manifest Manifest, phase restorePhase) {
	t.Helper()
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("json.Marshal(manifest) error = %v", err)
	}
	digest := sha256.Sum256(manifestData)
	journal := restoreJournal{
		Version: restoreJournalVersion, ManifestSHA256: hex.EncodeToString(digest[:]), Manifest: manifest,
		StableTarget: paths.target, Staging: paths.staging, Rollback: paths.rollback, Phase: phase,
	}
	if err := writeJournal(paths, journal, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("writeJournal() error = %v", err)
	}
}

func assertStateHasAdmin(t *testing.T, path string, want bool) {
	t.Helper()
	store, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("sqlite.Open(assert state) error = %v", err)
	}
	hasAdmin, checkErr := store.HasAdmin(context.Background())
	closeErr := store.Close()
	if checkErr != nil || closeErr != nil {
		t.Fatalf("inspect state admin = %v, close = %v", checkErr, closeErr)
	}
	if hasAdmin != want {
		t.Fatalf("state HasAdmin() = %t, want %t", hasAdmin, want)
	}
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile(%q) error = %v", path, err)
	}
	if string(data) != want {
		t.Fatalf("file %q = %q, want %q", path, data, want)
	}
}

// failWhenObservedContext 在第一个 Done 观察点模拟启动恢复途中收到取消或超时。
// recoverPlatform 的入口 Err 检查仍返回 nil，SQLite 校验开始观察 Done 后才取消。
type failWhenObservedContext struct {
	done chan struct{}
	err  error
	once sync.Once
}

func (ctx *failWhenObservedContext) Deadline() (time.Time, bool) {
	return time.Time{}, false
}

func (ctx *failWhenObservedContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.done) })
	return ctx.done
}

func (ctx *failWhenObservedContext) Err() error {
	select {
	case <-ctx.done:
		return ctx.err
	default:
		return nil
	}
}

func (*failWhenObservedContext) Value(any) any {
	return nil
}
