package durableops

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/application"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/repository"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	"github.com/lifei6671/xtunnel/internal/server/gateway"
	"github.com/lifei6671/xtunnel/internal/server/tokenkey"
)

var testMasterKey = bytes.Repeat([]byte{0x42}, 32)

const archiveHardExitHelperEnv = "XTUNNEL_ARCHIVE_HARD_EXIT_HELPER"

type failAfterWriter struct {
	writer    io.Writer
	remaining int
	err       error
}

func (writer *failAfterWriter) Write(data []byte) (int, error) {
	if writer.remaining <= 0 {
		return 0, writer.err
	}
	if len(data) <= writer.remaining {
		count, err := writer.writer.Write(data)
		writer.remaining -= count
		return count, err
	}
	count, err := writer.writer.Write(data[:writer.remaining])
	writer.remaining -= count
	if err != nil {
		return count, err
	}
	return count, writer.err
}

func TestCreateOwnsExclusiveOutputAndRemovesFailedArchive(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Server backup creation is supported only on Linux")
	}
	dataDir := t.TempDir()
	writeSourceFile(t, dataDir, "credentials/tunnel-token.key", testMasterKey, 0o600)
	outputPath := filepath.Join(t.TempDir(), "backup.tar")
	manifest, err := Create(context.Background(), CreateOptions{
		DataDir: dataDir, TLSMode: TLSModePublic, OutputPath: outputPath,
		BackupDatabase: func(ctx context.Context, destination string) (int, error) {
			return sqlite.CurrentSchemaVersion(), createValidSQLiteDatabase(ctx, destination)
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if manifest.SchemaVersion != sqlite.CurrentSchemaVersion() || manifest.TLSMode != TLSModePublic || len(manifest.Files) != 2 {
		t.Fatalf("Create() manifest = %#v", manifest)
	}
	originalArchive, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("os.ReadFile(first archive) error = %v", err)
	}
	if _, err := Create(context.Background(), CreateOptions{
		DataDir: dataDir, TLSMode: TLSModePublic, OutputPath: outputPath,
		BackupDatabase: func(ctx context.Context, destination string) (int, error) {
			return sqlite.CurrentSchemaVersion(), createValidSQLiteDatabase(ctx, destination)
		},
	}); err == nil || !strings.Contains(err.Error(), "file exists") {
		t.Fatalf("second Create() error = %v, want exclusive-output rejection", err)
	}
	archiveAfterConflict, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("os.ReadFile(archive after conflict) error = %v", err)
	}
	if !bytes.Equal(archiveAfterConflict, originalArchive) {
		t.Fatal("conflicting Create() overwrote the existing archive")
	}
	conflictEntries, err := os.ReadDir(filepath.Dir(outputPath))
	if err != nil {
		t.Fatalf("os.ReadDir(conflicting output parent) error = %v", err)
	}
	if len(conflictEntries) != 1 || conflictEntries[0].Name() != filepath.Base(outputPath) {
		t.Fatalf("conflicting Create() left pending output entries: %#v", conflictEntries)
	}

	failedOutput := filepath.Join(t.TempDir(), "failed.tar")
	_, err = Create(context.Background(), CreateOptions{
		DataDir: dataDir, TLSMode: TLSModePublic, OutputPath: failedOutput,
		BackupDatabase: func(_ context.Context, destination string) (int, error) {
			if err := os.WriteFile(destination, []byte("database"), 0o600); err != nil {
				return 0, err
			}
			return 0, nil
		},
	})
	if err == nil {
		t.Fatal("Create() with invalid schema succeeded")
	}
	if _, statErr := os.Lstat(failedOutput); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed output remained: %v", statErr)
	}

	publicationFailedOutput := filepath.Join(t.TempDir(), "publication-failed.tar")
	_, err = Create(context.Background(), CreateOptions{
		DataDir: dataDir, TLSMode: TLSModePublic, OutputPath: publicationFailedOutput,
		BackupDatabase: func(ctx context.Context, destination string) (int, error) {
			return sqlite.CurrentSchemaVersion(), createValidSQLiteDatabase(ctx, destination)
		},
		BeforePublish: func() error { return errors.New("lease lost") },
	})
	if err == nil || !strings.Contains(err.Error(), "publication barrier") {
		t.Fatalf("Create(publication barrier failure) error = %v", err)
	}
	if _, statErr := os.Lstat(publicationFailedOutput); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("publication-failed output remained: %v", statErr)
	}
	publicationEntries, err := os.ReadDir(filepath.Dir(publicationFailedOutput))
	if err != nil {
		t.Fatalf("os.ReadDir(publication-failed parent) error = %v", err)
	}
	if len(publicationEntries) != 0 {
		t.Fatalf("publication failure left pending outputs: %#v", publicationEntries)
	}
}

func TestCreateFilesystemFailpointsPreservePublicationBoundary(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("atomic backup publication is supported only on Linux")
	}
	dataDir := t.TempDir()
	initializeValidBackupState(t, dataDir, false)

	tests := []struct {
		name         string
		inject       func(*archiveCreateOps)
		wantErr      error
		finalVisible bool
	}{
		{
			name: "archive write EIO",
			inject: func(ops *archiveCreateOps) {
				ops.writer = func(file *os.File) io.Writer {
					return &failAfterWriter{writer: file, remaining: 512, err: syscall.EIO}
				}
			},
			wantErr: syscall.EIO,
		},
		{
			name: "candidate fsync disk full",
			inject: func(ops *archiveCreateOps) {
				ops.syncFile = func(*os.File) error { return syscall.ENOSPC }
			},
			wantErr: syscall.ENOSPC,
		},
		{
			name: "publication rename EIO",
			inject: func(ops *archiveCreateOps) {
				ops.publish = func(*pendingOutput) error { return syscall.EIO }
			},
			wantErr: syscall.EIO,
		},
		{
			name: "published parent fsync EIO",
			inject: func(ops *archiveCreateOps) {
				ops.syncParent = func(*os.File) error { return syscall.EIO }
			},
			wantErr:      syscall.EIO,
			finalVisible: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			outputDir := t.TempDir()
			outputPath := filepath.Join(outputDir, "backup.tar")
			ops := productionArchiveCreateOps()
			test.inject(&ops)

			_, err := createWithOps(context.Background(), CreateOptions{
				DataDir: dataDir, TLSMode: TLSModePublic, OutputPath: outputPath,
				BackupDatabase: func(ctx context.Context, destination string) (int, error) {
					return sqlite.CurrentSchemaVersion(), createValidSQLiteDatabase(ctx, destination)
				},
			}, ops)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("createWithOps() error = %v, want %v", err, test.wantErr)
			}
			_, statErr := os.Lstat(outputPath)
			if test.finalVisible {
				if statErr != nil {
					t.Fatalf("published output is not visible after parent fsync failure: %v", statErr)
				}
			} else if !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("final output became visible before durable publication: %v", statErr)
			}
			entries, readErr := os.ReadDir(outputDir)
			if readErr != nil {
				t.Fatalf("os.ReadDir(output parent) error = %v", readErr)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), pendingOutputPrefix) {
					t.Fatalf("failpoint left live-process pending output %q", entry.Name())
				}
			}
		})
	}
}

func TestCreateDoesNotBlindlyDeleteHistoricalPendingOutput(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("atomic backup publication is supported only on Linux")
	}
	dataDir := t.TempDir()
	initializeValidBackupState(t, dataDir, false)
	outputDir := t.TempDir()
	historicalPath := filepath.Join(outputDir, pendingOutputPrefix+"historical")
	historical := []byte("private orphan requiring explicit operator review")
	if err := os.WriteFile(historicalPath, historical, 0o600); err != nil {
		t.Fatalf("os.WriteFile(historical pending output) error = %v", err)
	}
	outputPath := filepath.Join(outputDir, "backup.tar")

	if _, err := Create(context.Background(), CreateOptions{
		DataDir: dataDir, TLSMode: TLSModePublic, OutputPath: outputPath,
		BackupDatabase: func(ctx context.Context, destination string) (int, error) {
			return sqlite.CurrentSchemaVersion(), createValidSQLiteDatabase(ctx, destination)
		},
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	got, err := os.ReadFile(historicalPath)
	if err != nil {
		t.Fatalf("os.ReadFile(historical pending output) error = %v", err)
	}
	if !bytes.Equal(got, historical) {
		t.Fatalf("historical pending output = %q, want %q", got, historical)
	}
}

func TestCreateHardExitBeforePublishDoesNotExposeFinalPath(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("atomic backup publication is supported only on Linux")
	}
	if os.Getenv(archiveHardExitHelperEnv) == "1" {
		dataDir := os.Getenv("XTUNNEL_ARCHIVE_HARD_EXIT_DATA_DIR")
		outputPath := os.Getenv("XTUNNEL_ARCHIVE_HARD_EXIT_OUTPUT")
		_, err := Create(context.Background(), CreateOptions{
			DataDir: dataDir, TLSMode: TLSModePublic, OutputPath: outputPath,
			BackupDatabase: func(ctx context.Context, destination string) (int, error) {
				return sqlite.CurrentSchemaVersion(), createValidSQLiteDatabase(ctx, destination)
			},
			BeforePublish: func() error {
				// 回调代表归档内容和文件 fsync 已完成。若最终路径此时已经出现，说明
				// 崩溃窗口仍会向调用方暴露未获得 ACK 的归档。
				if _, statErr := os.Lstat(outputPath); !errors.Is(statErr, os.ErrNotExist) {
					os.Exit(76)
				}
				os.Exit(73)
				return nil
			},
		})
		if err != nil {
			os.Exit(74)
		}
		os.Exit(75)
	}

	dataDir := t.TempDir()
	initializeValidBackupState(t, dataDir, false)
	outputDir := t.TempDir()
	outputPath := filepath.Join(outputDir, "backup.tar")
	command := exec.Command(os.Args[0], "-test.run=^TestCreateHardExitBeforePublishDoesNotExposeFinalPath$")
	command.Env = append(os.Environ(),
		archiveHardExitHelperEnv+"=1",
		"XTUNNEL_ARCHIVE_HARD_EXIT_DATA_DIR="+dataDir,
		"XTUNNEL_ARCHIVE_HARD_EXIT_OUTPUT="+outputPath,
	)
	err := command.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 73 {
		t.Fatalf("hard-exit helper error = %v, want exit code 73", err)
	}
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("final output became visible before publication ACK: %v", err)
	}
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		t.Fatalf("os.ReadDir(output parent) error = %v", err)
	}
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), pendingOutputPrefix) {
		t.Fatalf("hard-exit output entries = %#v, want one hidden pending file", entries)
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatalf("pending output Info() error = %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("pending output mode = %v, want regular 0600", info.Mode())
	}
}

func TestCreateArchiveWritesDeterministicPinnedManifest(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Server backup creation is supported only on Linux")
	}
	source := t.TempDir()
	initializeValidBackupState(t, source, true)

	var output bytes.Buffer
	_, manifestHash, err := createArchive(context.Background(), &output, source, sqlite.CurrentSchemaVersion(), TLSModePinned)
	if err != nil {
		t.Fatalf("createArchive() error = %v", err)
	}

	archive := tar.NewReader(bytes.NewReader(output.Bytes()))
	manifest, manifestData, err := readManifest(archive, sqlite.CurrentSchemaVersion())
	if err != nil {
		t.Fatalf("readManifest() error = %v", err)
	}
	wantHash := sha256.Sum256(manifestData)
	if manifestHash != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("manifest hash = %q, want %x", manifestHash, wantHash)
	}
	if manifest.FormatVersion != FormatVersion || manifest.SchemaVersion != sqlite.CurrentSchemaVersion() || manifest.TLSMode != TLSModePinned {
		t.Fatalf("manifest metadata = %#v", manifest)
	}
	wantPaths := []string{
		"xtunnel.db",
		"credentials/tunnel-token.key",
		"pki/agent-gateway.key",
		"pki/agent-gateway.crt",
	}
	for index, wantPath := range wantPaths {
		if manifest.Files[index].Path != wantPath {
			t.Fatalf("manifest.Files[%d].Path = %q, want %q", index, manifest.Files[index].Path, wantPath)
		}
		header, err := archive.Next()
		if err != nil {
			t.Fatalf("archive.Next(%q) error = %v", wantPath, err)
		}
		if header.Name != wantPath || header.Typeflag != tar.TypeReg || uint32(header.Mode) != manifest.Files[index].Mode {
			t.Fatalf("archive header = %#v, want path %q mode %04o", header, wantPath, manifest.Files[index].Mode)
		}
		data, err := io.ReadAll(archive)
		if err != nil {
			t.Fatalf("read archive file %q: %v", wantPath, err)
		}
		digest := sha256.Sum256(data)
		if int64(len(data)) != manifest.Files[index].Size || hex.EncodeToString(digest[:]) != manifest.Files[index].SHA256 {
			t.Fatalf("archive file %q does not match manifest", wantPath)
		}
	}
	if _, err := archive.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("archive.Next() after files error = %v, want EOF", err)
	}
}

func TestValidateRestoredStateRejectsWellFormedWrongTokenKey(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("durable state validation is supported only on Linux")
	}
	root := t.TempDir()
	initializeValidBackupState(t, root, false)
	seedValidTunnelToken(t, root)
	writeSourceFile(t, root, "credentials/tunnel-token.key", bytes.Repeat([]byte{0x99}, tokenkey.Size), 0o600)
	manifest := manifestForExistingState(t, root, false)
	if err := validateRestoredState(context.Background(), root, manifest); err == nil || !strings.Contains(err.Error(), "ciphertexts") {
		t.Fatalf("validateRestoredState(wrong Token Key) error = %v", err)
	}
}

func TestCreateArchiveRejectsSymbolicLinkSource(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Server backup creation is supported only on Linux")
	}
	source := t.TempDir()
	writeSourceFile(t, source, "outside", []byte("database"), 0o600)
	if err := os.Symlink(filepath.Join(source, "outside"), filepath.Join(source, "xtunnel.db")); err != nil {
		t.Fatalf("os.Symlink() error = %v", err)
	}
	writeSourceFile(t, source, "credentials/tunnel-token.key", testMasterKey, 0o600)

	var output bytes.Buffer
	if _, _, err := createArchive(context.Background(), &output, source, 1, TLSModePublic); err == nil || !strings.Contains(err.Error(), "non-symbolic-link") {
		t.Fatalf("createArchive() error = %v, want symbolic-link rejection", err)
	}
}

func TestCopySnapshotRejectsSymbolicLinkInIntermediateDirectory(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("secure data-dir traversal is supported only on Linux")
	}
	source := t.TempDir()
	outside := t.TempDir()
	writeSourceFile(t, outside, "tunnel-token.key", testMasterKey, 0o600)
	if err := os.Symlink(outside, filepath.Join(source, "credentials")); err != nil {
		t.Fatalf("os.Symlink(credentials) error = %v", err)
	}
	if err := copySnapshotFile(context.Background(), source, t.TempDir(), archiveFileRules[1]); err == nil {
		t.Fatal("copySnapshotFile() followed a symbolic-link intermediate directory")
	}
}

func TestCreateRejectsPendingGatewayRotationArtifacts(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Server backup creation is supported only on Linux")
	}
	dataDir := t.TempDir()
	initializeValidBackupState(t, dataDir, true)
	writeSourceFile(t, dataDir, "pki/agent-gateway.rotation.json", []byte("pending"), 0o600)
	_, err := Create(context.Background(), CreateOptions{
		DataDir: dataDir, TLSMode: TLSModePinned, OutputPath: filepath.Join(t.TempDir(), "backup.tar"),
		BackupDatabase: func(ctx context.Context, destination string) (int, error) {
			return sqlite.CurrentSchemaVersion(), createValidSQLiteDatabase(ctx, destination)
		},
	})
	if err == nil || !strings.Contains(err.Error(), "reconciled Gateway identity") {
		t.Fatalf("Create(pending rotation) error = %v", err)
	}
}

func TestCreatePendingOutputRejectsSymbolicLinkParent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("secure output creation is supported only on Linux")
	}
	realParent := t.TempDir()
	linkParent := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(realParent, linkParent); err != nil {
		t.Fatalf("os.Symlink(output parent) error = %v", err)
	}
	pending, err := createPendingOutput(filepath.Join(linkParent, "backup.tar"))
	if pending != nil {
		if pending.file != nil {
			_ = pending.file.Close()
		}
		if pending.parent != nil {
			_ = pending.parent.Close()
		}
	}
	if err == nil {
		t.Fatal("createPendingOutput() followed a symbolic-link parent")
	}
}

func TestPublishPendingOutputDoesNotReplaceExistingTarget(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("atomic backup publication is supported only on Linux")
	}
	outputPath := filepath.Join(t.TempDir(), "backup.tar")
	first, err := createPendingOutput(outputPath)
	if err != nil {
		t.Fatalf("createPendingOutput(first) error = %v", err)
	}
	defer first.parent.Close()
	second, err := createPendingOutput(outputPath)
	if err != nil {
		_ = first.file.Close()
		t.Fatalf("createPendingOutput(second) error = %v", err)
	}
	defer second.parent.Close()

	for _, item := range []struct {
		name    string
		pending *pendingOutput
		data    []byte
	}{
		{name: "first", pending: first, data: []byte("first-complete-archive")},
		{name: "second", pending: second, data: []byte("second-complete-archive")},
	} {
		if _, err := item.pending.file.Write(item.data); err != nil {
			t.Fatalf("%s pending Write() error = %v", item.name, err)
		}
		if err := item.pending.file.Sync(); err != nil {
			t.Fatalf("%s pending Sync() error = %v", item.name, err)
		}
		if err := item.pending.file.Close(); err != nil {
			t.Fatalf("%s pending Close() error = %v", item.name, err)
		}
		item.pending.file = nil
	}
	if err := publishPendingOutput(first); err != nil {
		t.Fatalf("publishPendingOutput(first) error = %v", err)
	}
	if err := publishPendingOutput(second); !errors.Is(err, os.ErrExist) {
		t.Fatalf("publishPendingOutput(second) error = %v, want file exists", err)
	}
	if err := removePendingOutput(second.parent, second.name); err != nil {
		t.Fatalf("removePendingOutput(second) error = %v", err)
	}
	content, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("os.ReadFile(published output) error = %v", err)
	}
	if string(content) != "first-complete-archive" {
		t.Fatalf("published output = %q, want first candidate", content)
	}
}

func TestFailedOutputCleanupStaysBoundToOpenedParent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("FD-relative output cleanup is supported only on Linux")
	}
	container := t.TempDir()
	originalParent := filepath.Join(container, "output")
	if err := os.Mkdir(originalParent, 0o700); err != nil {
		t.Fatalf("os.Mkdir(original parent) error = %v", err)
	}
	pending, err := createPendingOutput(filepath.Join(originalParent, "backup.tar"))
	if err != nil {
		t.Fatalf("createPendingOutput() error = %v", err)
	}
	if err := pending.file.Close(); err != nil {
		_ = pending.parent.Close()
		t.Fatalf("output.Close() error = %v", err)
	}
	movedParent := filepath.Join(container, "moved-output")
	if err := os.Rename(originalParent, movedParent); err != nil {
		_ = pending.parent.Close()
		t.Fatalf("os.Rename(original parent) error = %v", err)
	}
	if err := os.Mkdir(originalParent, 0o700); err != nil {
		_ = pending.parent.Close()
		t.Fatalf("os.Mkdir(replacement parent) error = %v", err)
	}
	victim := filepath.Join(originalParent, pending.name)
	if err := os.WriteFile(victim, []byte("must-survive"), 0o600); err != nil {
		_ = pending.parent.Close()
		t.Fatalf("os.WriteFile(victim) error = %v", err)
	}
	if err := removePendingOutput(pending.parent, pending.name); err != nil {
		_ = pending.parent.Close()
		t.Fatalf("removePendingOutput() error = %v", err)
	}
	if err := pending.parent.Close(); err != nil {
		t.Fatalf("parent.Close() error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(movedParent, pending.name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("original failed output remains or stat failed: %v", err)
	}
	content, err := os.ReadFile(victim)
	if err != nil || string(content) != "must-survive" {
		t.Fatalf("replacement victim = %q, %v", content, err)
	}
}

func TestValidateManifestRejectsUnsafeOrIncompleteFileSets(t *testing.T) {
	validFile := func(path string, mode uint32, data []byte) ManifestFile {
		digest := sha256.Sum256(data)
		return ManifestFile{Path: path, Size: int64(len(data)), Mode: mode, SHA256: hex.EncodeToString(digest[:])}
	}
	valid := Manifest{
		FormatVersion: FormatVersion,
		SchemaVersion: 1,
		TLSMode:       TLSModePublic,
		Files: []ManifestFile{
			validFile("xtunnel.db", 0o600, []byte("value")),
			validFile("credentials/tunnel-token.key", 0o600, testMasterKey),
		},
	}
	tests := []struct {
		name   string
		mutate func(*Manifest)
		want   string
	}{
		{name: "path traversal", mutate: func(manifest *Manifest) { manifest.Files[0].Path = "../xtunnel.db" }, want: "not allowed"},
		{name: "absolute path", mutate: func(manifest *Manifest) {
			manifest.Files[0].Path = filepath.Join(string(filepath.Separator), "xtunnel.db")
		}, want: "not allowed"},
		{name: "duplicate", mutate: func(manifest *Manifest) { manifest.Files[1] = manifest.Files[0] }, want: "duplicated"},
		{name: "noncanonical order", mutate: func(manifest *Manifest) { manifest.Files[0], manifest.Files[1] = manifest.Files[1], manifest.Files[0] }, want: "canonical archive order"},
		{name: "secret mode", mutate: func(manifest *Manifest) { manifest.Files[1].Mode = 0o644 }, want: "mode"},
		{name: "future schema", mutate: func(manifest *Manifest) { manifest.SchemaVersion = 2 }, want: "supported range"},
		{name: "missing master key", mutate: func(manifest *Manifest) { manifest.Files = manifest.Files[:1] }, want: "missing required"},
		{name: "public contains pinned key", mutate: func(manifest *Manifest) {
			manifest.Files = append(manifest.Files, validFile("pki/agent-gateway.key", 0o600, []byte("key")))
		}, want: "must not contain"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := valid
			manifest.Files = append([]ManifestFile(nil), valid.Files...)
			test.mutate(&manifest)
			if err := validateManifest(manifest, 1); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateManifest() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestReadManifestRequiresCanonicalJSON(t *testing.T) {
	manifest := testPublicManifest([]byte("database"), testMasterKey)
	canonical, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("json.Marshal(manifest) error = %v", err)
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, canonical, "", "  "); err != nil {
		t.Fatalf("json.Indent(manifest) error = %v", err)
	}
	tests := []struct {
		name      string
		data      []byte
		wantError bool
	}{
		{name: "canonical", data: canonical},
		{name: "leading whitespace", data: append([]byte(" "), canonical...), wantError: true},
		{name: "trailing newline", data: append(append([]byte(nil), canonical...), '\n'), wantError: true},
		{name: "indented", data: indented.Bytes(), wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			archiveData := buildTestArchiveWithManifestData(t, test.data, nil)
			parsed, manifestData, err := readManifest(tar.NewReader(bytes.NewReader(archiveData)), 1)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "not canonical") {
					t.Fatalf("readManifest() error = %v, want canonical rejection", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("readManifest() error = %v", err)
			}
			if !bytes.Equal(manifestData, canonical) || parsed.SchemaVersion != manifest.SchemaVersion {
				t.Fatalf("readManifest() = %#v, %q", parsed, manifestData)
			}
		})
	}
}

func TestArchiveEntryMustBeRegularAndMatchManifest(t *testing.T) {
	manifest := testPublicManifest([]byte("database"), testMasterKey)
	tests := []struct {
		name     string
		typeflag byte
		entry    string
		want     string
	}{
		{name: "symbolic link", typeflag: tar.TypeSymlink, entry: "xtunnel.db", want: "does not match"},
		{name: "hard link", typeflag: tar.TypeLink, entry: "xtunnel.db", want: "does not match"},
		{name: "device", typeflag: tar.TypeChar, entry: "xtunnel.db", want: "does not match"},
		{name: "fifo", typeflag: tar.TypeFifo, entry: "xtunnel.db", want: "does not match"},
		{name: "unexpected path", typeflag: tar.TypeReg, entry: "../xtunnel.db", want: "does not match"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			archiveData := buildTestArchive(t, manifest, []testTarEntry{
				{name: test.entry, mode: 0o600, typeflag: test.typeflag, data: []byte("database")},
				{name: "credentials/tunnel-token.key", mode: 0o600, typeflag: tar.TypeReg, data: testMasterKey},
			})
			archive := tar.NewReader(bytes.NewReader(archiveData))
			parsed, _, err := readManifest(archive, 1)
			if err != nil {
				t.Fatalf("readManifest() error = %v", err)
			}
			header, err := archive.Next()
			if err != nil {
				t.Fatalf("archive.Next() error = %v", err)
			}
			if err := validateArchiveHeader(parsed.Files[0], header); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateArchiveHeader() error = %v, want %q", err, test.want)
			}
		})
	}
}

type testTarEntry struct {
	name     string
	mode     int64
	typeflag byte
	data     []byte
}

func testPublicManifest(database, masterKey []byte) Manifest {
	databaseDigest := sha256.Sum256(database)
	keyDigest := sha256.Sum256(masterKey)
	return Manifest{
		FormatVersion: FormatVersion,
		SchemaVersion: 1,
		TLSMode:       TLSModePublic,
		Files: []ManifestFile{
			{Path: "xtunnel.db", Size: int64(len(database)), Mode: 0o600, SHA256: hex.EncodeToString(databaseDigest[:])},
			{Path: "credentials/tunnel-token.key", Size: int64(len(masterKey)), Mode: 0o600, SHA256: hex.EncodeToString(keyDigest[:])},
		},
	}
}

func buildTestArchive(t *testing.T, manifest Manifest, entries []testTarEntry) []byte {
	t.Helper()
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("json.Marshal(manifest) error = %v", err)
	}
	return buildTestArchiveWithManifestData(t, manifestData, entries)
}

func buildTestArchiveWithManifestData(t *testing.T, manifestData []byte, entries []testTarEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	archive := tar.NewWriter(&output)
	if err := archive.WriteHeader(&tar.Header{Name: manifestName, Mode: 0o600, Size: int64(len(manifestData)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("write manifest header: %v", err)
	}
	if _, err := archive.Write(manifestData); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	for _, entry := range entries {
		if err := archive.WriteHeader(&tar.Header{Name: entry.name, Mode: entry.mode, Size: int64(len(entry.data)), Typeflag: entry.typeflag, Linkname: "xtunnel.db"}); err != nil {
			t.Fatalf("write entry header: %v", err)
		}
		if entry.typeflag == tar.TypeReg {
			if _, err := archive.Write(entry.data); err != nil {
				t.Fatalf("write entry data: %v", err)
			}
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatalf("archive.Close() error = %v", err)
	}
	return output.Bytes()
}

func writeSourceFile(t *testing.T, root, relative string, data []byte, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("os.MkdirAll(%q) error = %v", relative, err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v", relative, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("os.Chmod(%q) error = %v", relative, err)
	}
}

func initializeValidBackupState(t *testing.T, root string, pinned bool) {
	t.Helper()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("os.Chmod(valid backup root) error = %v", err)
	}
	store, err := sqlite.Open(context.Background(), root)
	if err != nil {
		t.Fatalf("sqlite.Open(valid backup state) error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Store.Close(valid backup state) error = %v", err)
	}
	normalizeTestSQLite(t, root)
	if _, err := tokenkey.LoadOrCreate(root, false); err != nil {
		t.Fatalf("tokenkey.LoadOrCreate(valid backup state) error = %v", err)
	}
	if pinned {
		if _, err := gateway.LoadOrCreatePinnedIdentity(
			root,
			"gateway.example.test",
			true,
			time.Date(2026, time.August, 27, 0, 0, 0, 0, time.UTC),
		); err != nil {
			t.Fatalf("LoadOrCreatePinnedIdentity(valid backup state) error = %v", err)
		}
	}
}

func seedValidTunnelToken(t *testing.T, root string) {
	t.Helper()
	key, err := tokenkey.LoadOrCreate(root, false)
	if err != nil {
		t.Fatalf("tokenkey.LoadOrCreate(seed token) error = %v", err)
	}
	protector, err := application.NewAES256GCMTokenProtector(key[:])
	clear(key[:])
	if err != nil {
		t.Fatalf("NewAES256GCMTokenProtector() error = %v", err)
	}
	store, err := sqlite.Open(context.Background(), root)
	if err != nil {
		t.Fatalf("sqlite.Open(seed token) error = %v", err)
	}
	const tunnelID = "tun_01J00000000000000000000000"
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		return transaction.Tunnels().Create(context.Background(), repository.Tunnel{
			ID: tunnelID, Name: "backup-validation", Version: 1, CreatedAt: 1, UpdatedAt: 1,
		})
	}); err != nil {
		_ = store.Close()
		t.Fatalf("create seed Tunnel error = %v", err)
	}
	service := application.NewConnectionTokenService(store, protector)
	if _, err := service.Issue(context.Background(), application.IssueConnectionTokenInput{
		TunnelID: tunnelID,
		Endpoint: &protocolv1.GatewayEndpoint{Host: "gateway.example.test", Port: 7443},
		TLSTrust: &protocolv1.TlsTrustDescriptor{Mode: &protocolv1.TlsTrustDescriptor_PublicCa{
			PublicCa: &protocolv1.PublicCATrust{},
		}},
	}); err != nil {
		_ = store.Close()
		t.Fatalf("Issue(seed token) error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Store.Close(seed token) error = %v", err)
	}
	normalizeTestSQLite(t, root)
}

func manifestForExistingState(t *testing.T, root string, pinned bool) Manifest {
	t.Helper()
	manifest := Manifest{FormatVersion: FormatVersion, SchemaVersion: sqlite.CurrentSchemaVersion(), TLSMode: TLSModePublic}
	if pinned {
		manifest.TLSMode = TLSModePinned
	}
	for _, rule := range archiveFileRules {
		if rule.pinned && !pinned {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(rule.path))
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) && !rule.required {
			continue
		}
		if err != nil {
			t.Fatalf("os.Lstat(existing state %q) error = %v", rule.path, err)
		}
		digest, size, err := digestFile(path, info)
		if err != nil {
			t.Fatalf("digestFile(existing state %q) error = %v", rule.path, err)
		}
		manifest.Files = append(manifest.Files, ManifestFile{Path: rule.path, Size: size, Mode: rule.mode, SHA256: digest})
	}
	return manifest
}

func normalizeTestSQLite(t *testing.T, root string) {
	t.Helper()
	databasePath := filepath.Join(root, "xtunnel.db")
	backupPath := filepath.Join(root, "xtunnel.db.backup")
	if err := sqlite.BackupSQLite(context.Background(), databasePath, backupPath); err != nil {
		t.Fatalf("sqlite.BackupSQLite(test state) error = %v", err)
	}
	for _, path := range []string{databasePath, databasePath + "-wal", databasePath + "-shm"} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatalf("os.Remove(test SQLite artifact) error = %v", err)
		}
	}
	if err := os.Rename(backupPath, databasePath); err != nil {
		t.Fatalf("os.Rename(test SQLite backup) error = %v", err)
	}
	if err := os.Chmod(databasePath, 0o600); err != nil {
		t.Fatalf("os.Chmod(valid backup database) error = %v", err)
	}
}

func createValidSQLiteDatabase(ctx context.Context, destination string) error {
	store, err := sqlite.Open(ctx, filepath.Dir(destination))
	if err != nil {
		return err
	}
	return store.Close()
}
