//go:build windows

package durableops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lifei6671/xtunnel/internal/server/datadir"
	"github.com/lifei6671/xtunnel/internal/server/winsecurity"
	"golang.org/x/sys/windows"
)

func TestWindowsRecoverPendingRestoreAllowsCleanManagedTarget(t *testing.T) {
	target := newWindowsRestoreTarget(t)
	paths, err := pathsForTarget(target)
	if err != nil {
		t.Fatalf("pathsForTarget() error = %v", err)
	}
	if recovered, err := RecoverPendingRestore(context.Background(), target); err != nil || recovered {
		t.Fatalf("RecoverPendingRestore() = (%t, %v), want (false, nil)", recovered, err)
	}
	for _, path := range []string{paths.staging, paths.rollback, paths.journal} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("clean Windows recovery changed artifact %q: %v", path, err)
		}
	}
}

func TestWindowsRecoverPendingRestoreRejectsPendingArtifactsWithoutMutation(t *testing.T) {
	target := newWindowsRestoreTarget(t)
	paths, err := pathsForTarget(target)
	if err != nil {
		t.Fatalf("pathsForTarget() error = %v", err)
	}
	directorySecurity, err := winsecurity.NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := winsecurity.CreateForegroundDirectory(paths.staging, directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectory(staging) error = %v", err)
	}
	if recovered, err := RecoverPendingRestore(context.Background(), target); err == nil || recovered {
		t.Fatalf("RecoverPendingRestore() = (%t, %v), want fail-closed error", recovered, err)
	}
	if err := winsecurity.ValidateForegroundDirectory(paths.staging); err != nil {
		t.Fatalf("pending staging changed after failed recovery: %v", err)
	}
}

func TestWindowsRecoverPendingRestoreRejectsMalformedJournalWithoutMutation(t *testing.T) {
	target := newWindowsRestoreTarget(t)
	paths, err := pathsForTarget(target)
	if err != nil {
		t.Fatalf("pathsForTarget() error = %v", err)
	}
	fileSecurity, err := winsecurity.NewForegroundFileSecurity()
	if err != nil {
		t.Fatalf("NewForegroundFileSecurity() error = %v", err)
	}
	content := []byte("malformed journal")
	if err := winsecurity.PublishForegroundFile(target.Parent, filepath.Base(paths.journal), content, fileSecurity); err != nil {
		t.Fatalf("PublishForegroundFile(journal) error = %v", err)
	}
	before, err := os.ReadFile(paths.journal)
	if err != nil {
		t.Fatalf("ReadFile(journal before recovery) error = %v", err)
	}
	if recovered, err := RecoverPendingRestore(context.Background(), target); err == nil || recovered {
		t.Fatalf("RecoverPendingRestore() = (%t, %v), want malformed Journal rejection", recovered, err)
	}
	after, err := os.ReadFile(paths.journal)
	if err != nil {
		t.Fatalf("ReadFile(journal after recovery) error = %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("failed recovery changed Journal: got %q, want %q", after, before)
	}
}

func TestWindowsRecoverPendingRestoreRejectsValidJournalAndRollbackWithoutMutation(t *testing.T) {
	target := newWindowsRestoreTarget(t)
	paths, err := pathsForTarget(target)
	if err != nil {
		t.Fatalf("pathsForTarget() error = %v", err)
	}
	directorySecurity, err := winsecurity.NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := winsecurity.CreateForegroundDirectory(paths.rollback, directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectory(rollback) error = %v", err)
	}
	journal := validWindowsRestoreJournal(t, paths)
	if err := writeJournal(paths, journal); err != nil {
		t.Fatalf("writeJournal() error = %v", err)
	}
	journalData, err := json.Marshal(journal)
	if err != nil {
		t.Fatalf("json.Marshal(journal) error = %v", err)
	}
	if recovered, err := RecoverPendingRestore(context.Background(), target); err == nil || recovered {
		t.Fatalf("RecoverPendingRestore() = (%t, %v), want valid Journal fail-closed error", recovered, err)
	}
	if err := winsecurity.ValidateForegroundDirectory(paths.rollback); err != nil {
		t.Fatalf("pending rollback changed after failed recovery: %v", err)
	}
	after, err := os.ReadFile(paths.journal)
	if err != nil {
		t.Fatalf("ReadFile(journal after recovery) error = %v", err)
	}
	if string(after) != string(journalData) {
		t.Fatalf("failed recovery changed valid Journal: got %q, want %q", after, journalData)
	}
}

func TestWindowsRecoverPreparedJournalAfterStagingCleanupRemovesJournal(t *testing.T) {
	target := newWindowsRestoreTarget(t)
	paths, err := pathsForTarget(target)
	if err != nil {
		t.Fatalf("pathsForTarget() error = %v", err)
	}
	markerPath := filepath.Join(paths.target, "old-state-marker")
	if err := os.WriteFile(markerPath, []byte("old state"), 0o600); err != nil {
		t.Fatalf("WriteFile(target marker) error = %v", err)
	}
	journal := validWindowsRestoreJournal(t, paths)
	journal.Phase = phasePrepared
	if err := writeJournal(paths, journal); err != nil {
		t.Fatalf("writeJournal(prepared) error = %v", err)
	}
	if recovered, err := RecoverPendingRestore(context.Background(), target); err != nil || !recovered {
		t.Fatalf("RecoverPendingRestore() = (%t, %v), want (true, nil)", recovered, err)
	}
	if _, err := os.Lstat(paths.journal); !os.IsNotExist(err) {
		t.Fatalf("prepared Journal remains after recovery: %v", err)
	}
	if err := winsecurity.ValidateForegroundDirectory(paths.target); err != nil {
		t.Fatalf("ValidateForegroundDirectory(target) error = %v", err)
	}
	marker, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("ReadFile(target marker) error = %v", err)
	}
	if got, want := string(marker), "old state"; got != want {
		t.Fatalf("target marker = %q, want %q", got, want)
	}
	for _, path := range []string{paths.staging, paths.rollback} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("prepared recovery changed artifact %q: %v", path, err)
		}
	}
}

func TestWindowsRecoverV2RollbackRestoringTargetOnlyRemovesJournal(t *testing.T) {
	target := newWindowsRestoreTarget(t)
	paths, err := pathsForTarget(target)
	if err != nil {
		t.Fatalf("pathsForTarget() error = %v", err)
	}
	markerPath := filepath.Join(paths.target, "old-state-marker")
	if err := os.WriteFile(markerPath, []byte("restored old state"), 0o600); err != nil {
		t.Fatalf("WriteFile(target marker) error = %v", err)
	}
	journal := validWindowsRestoreJournal(t, paths)
	journal.Version = restoreJournalVersionV2
	journal.Phase = phaseRollbackRestoring
	if err := writeJournal(paths, journal); err != nil {
		t.Fatalf("writeJournal(rollback_restoring) error = %v", err)
	}

	if recovered, err := RecoverPendingRestore(context.Background(), target); err != nil || !recovered {
		t.Fatalf("RecoverPendingRestore() = (%t, %v), want (true, nil)", recovered, err)
	}
	if _, err := os.Lstat(paths.journal); !os.IsNotExist(err) {
		t.Fatalf("rollback_restoring Journal remains after recovery: %v", err)
	}
	marker, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("ReadFile(target marker) error = %v", err)
	}
	if got, want := string(marker), "restored old state"; got != want {
		t.Fatalf("target marker = %q, want %q", got, want)
	}
}

func TestWindowsRecoverTargetOnlyRejectsNonRecoverablePhases(t *testing.T) {
	for _, phase := range []restorePhase{phaseRollbackReady, phaseInstalled} {
		t.Run(string(phase), func(t *testing.T) {
			target := newWindowsRestoreTarget(t)
			paths, err := pathsForTarget(target)
			if err != nil {
				t.Fatalf("pathsForTarget() error = %v", err)
			}
			markerPath := filepath.Join(paths.target, "target-marker")
			if err := os.WriteFile(markerPath, []byte("remain"), 0o600); err != nil {
				t.Fatalf("WriteFile(target marker) error = %v", err)
			}
			journal := validWindowsRestoreJournal(t, paths)
			journal.Version = restoreJournalVersionV2
			journal.Phase = phase
			if err := writeJournal(paths, journal); err != nil {
				t.Fatalf("writeJournal(%s) error = %v", phase, err)
			}
			before, err := os.ReadFile(paths.journal)
			if err != nil {
				t.Fatalf("ReadFile(Journal before recovery) error = %v", err)
			}

			if recovered, err := RecoverPendingRestore(context.Background(), target); err == nil || recovered {
				t.Fatalf("RecoverPendingRestore() = (%t, %v), want fail-closed error", recovered, err)
			}
			after, err := os.ReadFile(paths.journal)
			if err != nil {
				t.Fatalf("ReadFile(Journal after recovery) error = %v", err)
			}
			if string(after) != string(before) {
				t.Fatalf("failed recovery changed Journal: got %q, want %q", after, before)
			}
			marker, err := os.ReadFile(markerPath)
			if err != nil {
				t.Fatalf("ReadFile(target marker) error = %v", err)
			}
			if got, want := string(marker), "remain"; got != want {
				t.Fatalf("target marker = %q, want %q", got, want)
			}
		})
	}
}

func TestWindowsRecoverV2RollbackRestoringRejectsNonTargetOnlyState(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		setup func(t *testing.T, paths restorePaths)
	}{
		{
			name: "staging remains",
			setup: func(t *testing.T, paths restorePaths) {
				t.Helper()
				security, err := winsecurity.NewForegroundDirectorySecurity()
				if err != nil {
					t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
				}
				if err := winsecurity.CreateForegroundDirectory(paths.staging, security); err != nil {
					t.Fatalf("CreateForegroundDirectory(staging) error = %v", err)
				}
			},
		},
		{
			name: "rollback remains",
			setup: func(t *testing.T, paths restorePaths) {
				t.Helper()
				security, err := winsecurity.NewForegroundDirectorySecurity()
				if err != nil {
					t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
				}
				if err := winsecurity.CreateForegroundDirectory(paths.rollback, security); err != nil {
					t.Fatalf("CreateForegroundDirectory(rollback) error = %v", err)
				}
			},
		},
		{
			name: "target missing",
			setup: func(t *testing.T, paths restorePaths) {
				t.Helper()
				if err := os.Remove(paths.target); err != nil {
					t.Fatalf("Remove(target) error = %v", err)
				}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			target := newWindowsRestoreTarget(t)
			paths, err := pathsForTarget(target)
			if err != nil {
				t.Fatalf("pathsForTarget() error = %v", err)
			}
			journal := validWindowsRestoreJournal(t, paths)
			journal.Version = restoreJournalVersionV2
			journal.Phase = phaseRollbackRestoring
			if err := writeJournal(paths, journal); err != nil {
				t.Fatalf("writeJournal(rollback_restoring) error = %v", err)
			}
			before, err := os.ReadFile(paths.journal)
			if err != nil {
				t.Fatalf("ReadFile(Journal before recovery) error = %v", err)
			}
			testCase.setup(t, paths)

			if recovered, err := RecoverPendingRestore(context.Background(), target); err == nil || recovered {
				t.Fatalf("RecoverPendingRestore() = (%t, %v), want fail-closed error", recovered, err)
			}
			after, err := os.ReadFile(paths.journal)
			if err != nil {
				t.Fatalf("ReadFile(Journal after recovery) error = %v", err)
			}
			if string(after) != string(before) {
				t.Fatalf("failed recovery changed Journal: got %q, want %q", after, before)
			}
		})
	}
}

func TestWindowsRecoverV2RollbackRestoringRejectsV1JournalAndCanceledContext(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		version int
		context func() context.Context
	}{
		{
			name:    "version one",
			version: restoreJournalVersionV1,
			context: func() context.Context { return context.Background() },
		},
		{
			name:    "canceled context",
			version: restoreJournalVersionV2,
			context: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			target := newWindowsRestoreTarget(t)
			paths, err := pathsForTarget(target)
			if err != nil {
				t.Fatalf("pathsForTarget() error = %v", err)
			}
			journal := validWindowsRestoreJournal(t, paths)
			journal.Version = testCase.version
			journal.Phase = phaseRollbackRestoring
			journalData, err := json.Marshal(journal)
			if err != nil {
				t.Fatalf("json.Marshal(rollback_restoring Journal) error = %v", err)
			}
			security, err := winsecurity.NewForegroundFileSecurity()
			if err != nil {
				t.Fatalf("NewForegroundFileSecurity() error = %v", err)
			}
			if err := winsecurity.PublishForegroundFile(target.Parent, filepath.Base(paths.journal), journalData, security); err != nil {
				t.Fatalf("PublishForegroundFile(rollback_restoring Journal) error = %v", err)
			}

			if recovered, err := RecoverPendingRestore(testCase.context(), target); err == nil || recovered {
				t.Fatalf("RecoverPendingRestore() = (%t, %v), want rejection", recovered, err)
			}
			after, err := os.ReadFile(paths.journal)
			if err != nil {
				t.Fatalf("ReadFile(Journal after rejected recovery) error = %v", err)
			}
			if string(after) != string(journalData) {
				t.Fatalf("rejected recovery changed Journal: got %q, want %q", after, journalData)
			}
		})
	}
}

func TestWindowsRecoverPreparedJournalCleansManagedStagingBeforeRemovingJournal(t *testing.T) {
	target := newWindowsRestoreTarget(t)
	paths, err := pathsForTarget(target)
	if err != nil {
		t.Fatalf("pathsForTarget() error = %v", err)
	}
	directorySecurity, err := winsecurity.NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := winsecurity.CreateForegroundDirectory(paths.staging, directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectory(staging) error = %v", err)
	}
	if err := winsecurity.CreateForegroundDirectoryChild(paths.staging, "nested", directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectoryChild(staging nested) error = %v", err)
	}
	fileSecurity, err := winsecurity.NewForegroundFileSecurity()
	if err != nil {
		t.Fatalf("NewForegroundFileSecurity() error = %v", err)
	}
	if err := winsecurity.PublishForegroundFile(filepath.Join(paths.staging, "nested"), "staging-marker", []byte("staging state"), fileSecurity); err != nil {
		t.Fatalf("PublishForegroundFile(staging marker) error = %v", err)
	}
	markerPath := filepath.Join(paths.target, "target-marker")
	if err := os.WriteFile(markerPath, []byte("old state"), 0o600); err != nil {
		t.Fatalf("WriteFile(target marker) error = %v", err)
	}
	journal := validWindowsRestoreJournal(t, paths)
	journal.Phase = phasePrepared
	if err := writeJournal(paths, journal); err != nil {
		t.Fatalf("writeJournal(prepared) error = %v", err)
	}
	if recovered, err := RecoverPendingRestore(context.Background(), target); err != nil || !recovered {
		t.Fatalf("RecoverPendingRestore() = (%t, %v), want (true, nil)", recovered, err)
	}
	for _, path := range []string{paths.staging, paths.journal} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("prepared recovery left %q: %v", path, err)
		}
	}
	marker, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("ReadFile(target marker) error = %v", err)
	}
	if got, want := string(marker), "old state"; got != want {
		t.Fatalf("target marker = %q, want %q", got, want)
	}
	if recovered, err := RecoverPendingRestore(context.Background(), target); err != nil || recovered {
		t.Fatalf("RecoverPendingRestore(after cleanup) = (%t, %v), want (false, nil)", recovered, err)
	}
}

func TestWindowsRecoverPreparedJournalKeepsStateWhenStagingContainsJunction(t *testing.T) {
	target := newWindowsRestoreTarget(t)
	paths, err := pathsForTarget(target)
	if err != nil {
		t.Fatalf("pathsForTarget() error = %v", err)
	}
	directorySecurity, err := winsecurity.NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := winsecurity.CreateForegroundDirectory(paths.staging, directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectory(staging) error = %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatalf("Mkdir(outside) error = %v", err)
	}
	outsideMarker := filepath.Join(outside, "marker")
	if err := os.WriteFile(outsideMarker, []byte("must remain"), 0o600); err != nil {
		t.Fatalf("WriteFile(outside marker) error = %v", err)
	}
	junctionPath := filepath.Join(paths.staging, "nested")
	if output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", junctionPath, outside).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J error = %v, output = %s", err, output)
	}
	markerPath := filepath.Join(paths.target, "target-marker")
	if err := os.WriteFile(markerPath, []byte("old state"), 0o600); err != nil {
		t.Fatalf("WriteFile(target marker) error = %v", err)
	}
	journal := validWindowsRestoreJournal(t, paths)
	journal.Phase = phasePrepared
	if err := writeJournal(paths, journal); err != nil {
		t.Fatalf("writeJournal(prepared) error = %v", err)
	}
	before, err := os.ReadFile(paths.journal)
	if err != nil {
		t.Fatalf("ReadFile(prepared Journal) error = %v", err)
	}

	if recovered, err := RecoverPendingRestore(context.Background(), target); err == nil || recovered {
		t.Fatalf("RecoverPendingRestore() = (%t, %v), want junction rejection", recovered, err)
	}
	after, err := os.ReadFile(paths.journal)
	if err != nil {
		t.Fatalf("ReadFile(prepared Journal after recovery) error = %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("junction rejection changed Journal: got %q, want %q", after, before)
	}
	if _, err := os.Lstat(paths.staging); err != nil {
		t.Fatalf("Lstat(staging) error = %v, want retained staging", err)
	}
	marker, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("ReadFile(target marker) error = %v", err)
	}
	if got, want := string(marker), "old state"; got != want {
		t.Fatalf("target marker = %q, want %q", got, want)
	}
	outsideValue, err := os.ReadFile(outsideMarker)
	if err != nil {
		t.Fatalf("ReadFile(outside marker) error = %v", err)
	}
	if got, want := string(outsideValue), "must remain"; got != want {
		t.Fatalf("outside marker = %q, want %q", got, want)
	}
}

func TestWindowsRecoverPreparedJournalKeepsStateWhenStagingIsUnprotectedOrCanceled(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		setup func(t *testing.T, paths restorePaths)
		ctx   func() context.Context
	}{
		{
			name: "unprotected staging child",
			setup: func(t *testing.T, paths restorePaths) {
				t.Helper()
				security, err := winsecurity.NewForegroundDirectorySecurity()
				if err != nil {
					t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
				}
				if err := winsecurity.CreateForegroundDirectory(paths.staging, security); err != nil {
					t.Fatalf("CreateForegroundDirectory(staging) error = %v", err)
				}
				if err := os.WriteFile(filepath.Join(paths.staging, "unprotected"), []byte("must remain"), 0o600); err != nil {
					t.Fatalf("WriteFile(unprotected staging child) error = %v", err)
				}
			},
			ctx: func() context.Context { return context.Background() },
		},
		{
			name: "canceled context",
			setup: func(t *testing.T, paths restorePaths) {
				t.Helper()
				security, err := winsecurity.NewForegroundDirectorySecurity()
				if err != nil {
					t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
				}
				if err := winsecurity.CreateForegroundDirectory(paths.staging, security); err != nil {
					t.Fatalf("CreateForegroundDirectory(staging) error = %v", err)
				}
			},
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			target := newWindowsRestoreTarget(t)
			paths, err := pathsForTarget(target)
			if err != nil {
				t.Fatalf("pathsForTarget() error = %v", err)
			}
			testCase.setup(t, paths)
			markerPath := filepath.Join(paths.target, "target-marker")
			if err := os.WriteFile(markerPath, []byte("old state"), 0o600); err != nil {
				t.Fatalf("WriteFile(target marker) error = %v", err)
			}
			journal := validWindowsRestoreJournal(t, paths)
			journal.Phase = phasePrepared
			if err := writeJournal(paths, journal); err != nil {
				t.Fatalf("writeJournal(prepared) error = %v", err)
			}
			before, err := os.ReadFile(paths.journal)
			if err != nil {
				t.Fatalf("ReadFile(prepared Journal) error = %v", err)
			}

			if recovered, err := RecoverPendingRestore(testCase.ctx(), target); err == nil || recovered {
				t.Fatalf("RecoverPendingRestore() = (%t, %v), want rejection", recovered, err)
			}
			after, err := os.ReadFile(paths.journal)
			if err != nil {
				t.Fatalf("ReadFile(prepared Journal after recovery) error = %v", err)
			}
			if string(after) != string(before) {
				t.Fatalf("rejected prepared recovery changed Journal: got %q, want %q", after, before)
			}
			if _, err := os.Lstat(paths.staging); err != nil {
				t.Fatalf("Lstat(staging) error = %v, want retained staging", err)
			}
			marker, err := os.ReadFile(markerPath)
			if err != nil {
				t.Fatalf("ReadFile(target marker) error = %v", err)
			}
			if got, want := string(marker), "old state"; got != want {
				t.Fatalf("target marker = %q, want %q", got, want)
			}
		})
	}
}

func TestWindowsRecoverPreparedStagingKeepsStateWhenJournalIsUnprotected(t *testing.T) {
	target := newWindowsRestoreTarget(t)
	paths, err := pathsForTarget(target)
	if err != nil {
		t.Fatalf("pathsForTarget() error = %v", err)
	}
	markerPath := filepath.Join(paths.target, "old-state-marker")
	if err := os.WriteFile(markerPath, []byte("old state"), 0o600); err != nil {
		t.Fatalf("WriteFile(target marker) error = %v", err)
	}
	directorySecurity, err := winsecurity.NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := winsecurity.CreateForegroundDirectory(paths.staging, directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectory(staging) error = %v", err)
	}
	journal := validWindowsRestoreJournal(t, paths)
	journal.Phase = phasePrepared
	journalData, err := json.Marshal(journal)
	if err != nil {
		t.Fatalf("json.Marshal(prepared Journal) error = %v", err)
	}
	if err := os.WriteFile(paths.journal, journalData, 0o600); err != nil {
		t.Fatalf("WriteFile(unprotected Journal) error = %v", err)
	}
	before, err := os.ReadFile(paths.journal)
	if err != nil {
		t.Fatalf("ReadFile(unprotected Journal) error = %v", err)
	}
	if recovered, err := RecoverPendingRestore(context.Background(), target); err == nil || recovered {
		t.Fatalf("RecoverPendingRestore() = (%t, %v), want Journal security rejection", recovered, err)
	}
	after, err := os.ReadFile(paths.journal)
	if err != nil {
		t.Fatalf("ReadFile(unprotected Journal after recovery) error = %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("unprotected Journal changed after rejection: got %q, want %q", after, before)
	}
	if _, err := os.Lstat(paths.staging); err != nil {
		t.Fatalf("Lstat(staging) error = %v, want retained staging", err)
	}
	marker, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("ReadFile(target marker) error = %v", err)
	}
	if got, want := string(marker), "old state"; got != want {
		t.Fatalf("target marker = %q, want %q", got, want)
	}
}

func TestWindowsRecoverPreparedStagingRejectsRollbackWithoutMutation(t *testing.T) {
	target := newWindowsRestoreTarget(t)
	paths, err := pathsForTarget(target)
	if err != nil {
		t.Fatalf("pathsForTarget() error = %v", err)
	}
	directorySecurity, err := winsecurity.NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	for _, path := range []string{paths.staging, paths.rollback} {
		if err := winsecurity.CreateForegroundDirectory(path, directorySecurity); err != nil {
			t.Fatalf("CreateForegroundDirectory(%q) error = %v", path, err)
		}
	}
	markerPath := filepath.Join(paths.target, "target-marker")
	if err := os.WriteFile(markerPath, []byte("old state"), 0o600); err != nil {
		t.Fatalf("WriteFile(target marker) error = %v", err)
	}
	journal := validWindowsRestoreJournal(t, paths)
	journal.Phase = phasePrepared
	if err := writeJournal(paths, journal); err != nil {
		t.Fatalf("writeJournal(prepared) error = %v", err)
	}
	before, err := os.ReadFile(paths.journal)
	if err != nil {
		t.Fatalf("ReadFile(prepared Journal) error = %v", err)
	}

	if recovered, err := RecoverPendingRestore(context.Background(), target); err == nil || recovered {
		t.Fatalf("RecoverPendingRestore() = (%t, %v), want rollback rejection", recovered, err)
	}
	after, err := os.ReadFile(paths.journal)
	if err != nil {
		t.Fatalf("ReadFile(prepared Journal after recovery) error = %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("rollback rejection changed Journal: got %q, want %q", after, before)
	}
	for _, path := range []string{paths.staging, paths.rollback} {
		if err := winsecurity.ValidateForegroundDirectory(path); err != nil {
			t.Fatalf("ValidateForegroundDirectory(%q) error = %v", path, err)
		}
	}
	marker, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("ReadFile(target marker) error = %v", err)
	}
	if got, want := string(marker), "old state"; got != want {
		t.Fatalf("target marker = %q, want %q", got, want)
	}
}

func TestWindowsWriteRestoreJournalPublishesProtectedReplacement(t *testing.T) {
	target := newWindowsRestoreTarget(t)
	paths, err := pathsForTarget(target)
	if err != nil {
		t.Fatalf("pathsForTarget() error = %v", err)
	}
	prepared := validWindowsRestoreJournal(t, paths)
	prepared.Phase = phasePrepared
	if err := writeJournal(paths, prepared); err != nil {
		t.Fatalf("writeJournal(prepared) error = %v", err)
	}
	rollbackReady := validWindowsRestoreJournal(t, paths)
	if err := writeJournal(paths, rollbackReady); err != nil {
		t.Fatalf("writeJournal(rollback_ready) error = %v", err)
	}
	security, err := winsecurity.NewForegroundFileSecurity()
	if err != nil {
		t.Fatalf("NewForegroundFileSecurity() error = %v", err)
	}
	content, err := winsecurity.ReadForegroundFileLimit(target.Parent, filepath.Base(paths.journal), maxRestoreJournalSize)
	if err != nil {
		t.Fatalf("ReadForegroundFileLimit(journal) error = %v", err)
	}
	parsed, err := parseJournal(content, paths)
	if err != nil {
		t.Fatalf("parseJournal() error = %v", err)
	}
	if parsed.Phase != phaseRollbackReady {
		t.Fatalf("published Journal phase = %q, want %q", parsed.Phase, phaseRollbackReady)
	}
	handle := openWindowsRestoreJournal(t, paths.journal)
	defer windows.CloseHandle(handle)
	if err := security.ValidateFile(handle); err != nil {
		t.Fatalf("ValidateFile(published Journal) error = %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(target.Parent, "."+filepath.Base(paths.journal)+".tmp-*"))
	if err != nil {
		t.Fatalf("Glob(Journal candidates) error = %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("Journal candidate files remain: %v", matches)
	}
}

func TestWindowsWriteRestoreJournalRejectsInvalidReplacementWithoutMutation(t *testing.T) {
	target := newWindowsRestoreTarget(t)
	paths, err := pathsForTarget(target)
	if err != nil {
		t.Fatalf("pathsForTarget() error = %v", err)
	}
	journal := validWindowsRestoreJournal(t, paths)
	if err := writeJournal(paths, journal); err != nil {
		t.Fatalf("writeJournal(initial) error = %v", err)
	}
	before, err := os.ReadFile(paths.journal)
	if err != nil {
		t.Fatalf("ReadFile(initial Journal) error = %v", err)
	}
	journal.StableTarget = filepath.Join(target.Parent, "another-data")
	if err := writeJournal(paths, journal); err == nil {
		t.Fatal("writeJournal(path-mismatched Journal) error = nil")
	}
	after, err := os.ReadFile(paths.journal)
	if err != nil {
		t.Fatalf("ReadFile(Journal after rejected replacement) error = %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("rejected Journal replacement changed final content: got %q, want %q", after, before)
	}
}

func validWindowsRestoreJournal(t *testing.T, paths restorePaths) restoreJournal {
	t.Helper()
	manifest := Manifest{
		FormatVersion: FormatVersion,
		SchemaVersion: 1,
		TLSMode:       TLSModePublic,
		Files: []ManifestFile{
			{Path: "xtunnel.db", Size: 1, Mode: 0o600, SHA256: strings.Repeat("a", sha256.Size*2)},
			{Path: "credentials/tunnel-token.key", Size: 32, Mode: 0o600, SHA256: strings.Repeat("b", sha256.Size*2)},
		},
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("json.Marshal(manifest) error = %v", err)
	}
	digest := sha256.Sum256(manifestData)
	return restoreJournal{
		Version:        restoreJournalVersion,
		ManifestSHA256: hex.EncodeToString(digest[:]),
		Manifest:       manifest,
		StableTarget:   paths.target,
		Staging:        paths.staging,
		Rollback:       paths.rollback,
		Phase:          phaseRollbackReady,
	}
}

func openWindowsRestoreJournal(t *testing.T, path string) windows.Handle {
	t.Helper()
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatalf("UTF16PtrFromString(%q) error = %v", path, err)
	}
	handle, err := windows.CreateFile(pointer, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatalf("CreateFile(Journal) error = %v", err)
	}
	return handle
}

func newWindowsRestoreTarget(t *testing.T) datadir.Target {
	t.Helper()
	parent := filepath.Join(t.TempDir(), "server")
	directorySecurity, err := winsecurity.NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := winsecurity.CreateForegroundDirectory(parent, directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectory(parent) error = %v", err)
	}
	data := filepath.Join(parent, "data")
	if err := winsecurity.CreateForegroundDirectory(data, directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectory(data) error = %v", err)
	}
	target, err := datadir.Resolve(data)
	if err != nil {
		t.Fatalf("datadir.Resolve() error = %v", err)
	}
	return target
}
