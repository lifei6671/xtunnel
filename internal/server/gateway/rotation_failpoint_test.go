package gateway

import (
	"crypto/sha256"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
)

type persistedIdentityState int

const (
	persistedIdentityOld persistedIdentityState = iota
	persistedIdentityRejectedMismatch
	persistedIdentityNew
)

func TestRotatePinnedIdentityFilesystemFailuresConvergeSafely(t *testing.T) {
	tests := []struct {
		name                string
		failpoint           string
		stateAfterFailure   persistedIdentityState
		journalAfterFailure string
	}{
		{
			name:                "temporary certificate write",
			failpoint:           "certificate-write",
			stateAfterFailure:   persistedIdentityOld,
			journalAfterFailure: "absent",
		},
		{
			name:                "partial journal write",
			failpoint:           "journal-partial-write",
			stateAfterFailure:   persistedIdentityOld,
			journalAfterFailure: "absent",
		},
		{
			name:                "journal file sync result uncertain",
			failpoint:           "journal-file-sync",
			stateAfterFailure:   persistedIdentityOld,
			journalAfterFailure: "absent",
		},
		{
			name:                "journal directory sync",
			failpoint:           "journal-directory-sync",
			stateAfterFailure:   persistedIdentityOld,
			journalAfterFailure: "absent",
		},
		{
			name:                "key rename",
			failpoint:           "key-rename",
			stateAfterFailure:   persistedIdentityOld,
			journalAfterFailure: "valid",
		},
		{
			name:                "key replacement directory sync",
			failpoint:           "key-directory-sync",
			stateAfterFailure:   persistedIdentityRejectedMismatch,
			journalAfterFailure: "valid",
		},
		{
			name:                "certificate rename",
			failpoint:           "certificate-rename",
			stateAfterFailure:   persistedIdentityRejectedMismatch,
			journalAfterFailure: "valid",
		},
		{
			name:                "certificate replacement directory sync",
			failpoint:           "certificate-directory-sync",
			stateAfterFailure:   persistedIdentityNew,
			journalAfterFailure: "valid",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dataDir := t.TempDir()
			now := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
			before, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, now)
			if err != nil {
				t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
			}
			paths := identityPaths(dataDir)
			fileOps := rotationFailpointOps(t, paths, test.failpoint)
			audit := RotationAuditMetadata{
				EventID:     "evt_01K00000000000000000000020",
				OperationID: "op_01K00000000000000000000020",
				OccurredAt:  now.Add(time.Hour).Unix(),
				ResourceID:  "gateway.example.test",
			}

			_, err = rotatePinnedIdentity(dataDir, audit.ResourceID, now.Add(time.Hour), audit, fileOps)
			if !errors.Is(err, syscall.EIO) && !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("rotatePinnedIdentity() error = %v, want injected filesystem error", err)
			}
			assertPersistedIdentityState(t, dataDir, before.SPKIHash(), test.stateAfterFailure, nil)

			switch test.journalAfterFailure {
			case "absent":
				if _, statErr := os.Stat(paths.journal); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("rotation journal stat error = %v, want absent", statErr)
				}
				for _, temporaryPath := range []string{paths.keyTemp, paths.certTemp} {
					if _, statErr := os.Stat(temporaryPath); !errors.Is(statErr, os.ErrNotExist) {
						t.Fatalf("rolled-back temporary file %q stat error = %v, want absent", temporaryPath, statErr)
					}
				}
				if err := RecoverRotation(dataDir); err != nil {
					t.Fatalf("RecoverRotation() without journal error = %v", err)
				}
				assertPersistedIdentityState(t, dataDir, before.SPKIHash(), persistedIdentityOld, nil)
			case "valid":
				pending, exists, err := PendingRotationAuditEvent(dataDir)
				if err != nil || !exists {
					t.Fatalf("PendingRotationAuditEvent() = %#v, %t, %v", pending, exists, err)
				}
				if err := RecoverRotation(dataDir); err != nil {
					t.Fatalf("RecoverRotation() error = %v", err)
				}
				assertPersistedIdentityState(
					t,
					dataDir,
					before.SPKIHash(),
					persistedIdentityNew,
					&pending.AfterStateDigest,
				)
				for _, temporaryPath := range []string{paths.keyTemp, paths.certTemp} {
					if _, statErr := os.Stat(temporaryPath); !errors.Is(statErr, os.ErrNotExist) {
						t.Fatalf("recovered temporary file %q stat error = %v, want absent", temporaryPath, statErr)
					}
				}
			default:
				t.Fatalf("unknown journal expectation %q", test.journalAfterFailure)
			}
		})
	}
}

func rotationFailpointOps(t *testing.T, paths identityFilePaths, failpoint string) rotationFileOps {
	t.Helper()
	fileOps := defaultRotationFileOps()
	writeSyncedFile := fileOps.writeFileSync
	rename := fileOps.rename
	syncDir := fileOps.syncDirectory

	fileOps.writeFileSync = func(path string, data []byte, mode os.FileMode) error {
		switch {
		case failpoint == "certificate-write" && path == paths.certTemp:
			return syscall.ENOSPC
		case failpoint == "journal-partial-write" && path == paths.journal:
			if err := os.WriteFile(path, data[:len(data)/2], mode); err != nil {
				t.Fatalf("prepare partial rotation journal: %v", err)
			}
			return syscall.ENOSPC
		case failpoint == "journal-file-sync" && path == paths.journal:
			if err := writeSyncedFile(path, data, mode); err != nil {
				t.Fatalf("prepare synced rotation journal: %v", err)
			}
			return syscall.EIO
		default:
			return writeSyncedFile(path, data, mode)
		}
	}
	fileOps.rename = func(oldPath, newPath string) error {
		if (failpoint == "key-rename" && oldPath == paths.keyTemp) ||
			(failpoint == "certificate-rename" && oldPath == paths.certTemp) {
			return syscall.EIO
		}
		return rename(oldPath, newPath)
	}
	syncCalls := 0
	fileOps.syncDirectory = func(path string) error {
		syncCalls++
		if (failpoint == "journal-directory-sync" && syncCalls == 1) ||
			(failpoint == "key-directory-sync" && syncCalls == 2) ||
			(failpoint == "certificate-directory-sync" && syncCalls == 3) {
			return syscall.EIO
		}
		return syncDir(path)
	}
	return fileOps
}

func assertPersistedIdentityState(
	t *testing.T,
	dataDir string,
	beforeDigest [sha256.Size]byte,
	want persistedIdentityState,
	afterDigest *[sha256.Size]byte,
) {
	t.Helper()
	identity, err := LoadPinnedIdentity(dataDir)
	switch want {
	case persistedIdentityOld:
		if err != nil {
			t.Fatalf("LoadPinnedIdentity(old state) error = %v", err)
		}
		if identity.SPKIHash() != beforeDigest {
			t.Fatal("filesystem failure changed the committed identity before a durable journal")
		}
	case persistedIdentityRejectedMismatch:
		if !errors.Is(err, ErrPinnedIdentityMismatch) {
			t.Fatalf("LoadPinnedIdentity(partial replacement) error = %v, want ErrPinnedIdentityMismatch", err)
		}
	case persistedIdentityNew:
		if err != nil {
			t.Fatalf("LoadPinnedIdentity(new state) error = %v", err)
		}
		if identity.SPKIHash() == beforeDigest {
			t.Fatal("recovered identity still uses the old SPKI")
		}
		if afterDigest != nil && identity.SPKIHash() != *afterDigest {
			t.Fatal("recovered identity does not match the journal after-state")
		}
	default:
		t.Fatalf("unknown persisted identity state %d", want)
	}
}
