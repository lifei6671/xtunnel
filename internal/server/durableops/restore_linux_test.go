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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	libsqlite "github.com/libtnb/sqlite"

	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	"github.com/lifei6671/xtunnel/internal/server/datadir"
	"github.com/lifei6671/xtunnel/migrations"
	"golang.org/x/sys/unix"
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

func TestRecoverPendingRestoreConvergesInterruptedRenameStates(t *testing.T) {
	tests := []struct {
		name           string
		phase          restorePhase
		targetExists   bool
		stagingExists  bool
		rollbackExists bool
		wantAdmin      bool
	}{
		{name: "prepared before first rename rolls back staging", phase: phasePrepared, targetExists: true, stagingExists: true, wantAdmin: true},
		{name: "prepared after first rename restores rollback", phase: phasePrepared, stagingExists: true, rollbackExists: true, wantAdmin: true},
		{name: "rollback ready before second rename restores rollback", phase: phaseRollbackReady, stagingExists: true, rollbackExists: true, wantAdmin: true},
		{name: "rollback ready after second rename finishes install", phase: phaseRollbackReady, targetExists: true, rollbackExists: true},
		{name: "installed cleans rollback", phase: phaseInstalled, targetExists: true, rollbackExists: true},
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
				manifest = writeValidStateDirectory(t, paths.target, test.wantAdmin)
			}
			if test.stagingExists {
				manifest = writeValidStateDirectory(t, paths.staging, false)
			}
			if test.rollbackExists {
				writeValidStateDirectory(t, paths.rollback, true)
			}
			manifestData, err := json.Marshal(manifest)
			if err != nil {
				t.Fatalf("json.Marshal(manifest) error = %v", err)
			}
			manifestDigest := sha256.Sum256(manifestData)
			journal := restoreJournal{
				Version: 1, ManifestSHA256: hex.EncodeToString(manifestDigest[:]), Manifest: manifest, StableTarget: paths.target,
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
		Version: 1, ManifestSHA256: hex.EncodeToString(digest[:]), Manifest: manifest,
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
