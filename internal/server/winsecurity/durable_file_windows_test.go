//go:build windows

package winsecurity

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

type cancelAfterFirstCheckContext struct {
	context.Context
	checks int
}

func (ctx *cancelAfterFirstCheckContext) Err() error {
	ctx.checks++
	if ctx.checks > 1 {
		return context.Canceled
	}
	return nil
}

func TestPublishForegroundFilePublishesProtectedReplacement(t *testing.T) {
	security, err := NewForegroundFileSecurity()
	if err != nil {
		t.Fatalf("NewForegroundFileSecurity() error = %v", err)
	}
	directory := filepath.Join(t.TempDir(), "managed")
	directorySecurity, err := NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := CreateForegroundDirectory(directory, directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectory() error = %v", err)
	}
	if err := PublishForegroundFile(directory, "key.bin", []byte("first"), security); err != nil {
		t.Fatalf("PublishForegroundFile(first) error = %v", err)
	}
	if err := PublishForegroundFile(directory, "key.bin", []byte("second"), security); err != nil {
		t.Fatalf("PublishForegroundFile(replace) error = %v", err)
	}
	content, err := os.ReadFile(filepath.Join(directory, "key.bin"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if got, want := string(content), "second"; got != want {
		t.Fatalf("published content = %q, want %q", got, want)
	}
	handle := openManagedFileNoFollow(t, filepath.Join(directory, "key.bin"))
	defer windows.CloseHandle(handle)
	if err := security.ValidateFile(handle); err != nil {
		t.Fatalf("ValidateFile() error = %v", err)
	}
}

func TestConsumeForegroundFileLimitWithPostDeleteRunsAfterRemoval(t *testing.T) {
	directory := newManagedFileDirectory(t)
	security, err := NewForegroundFileSecurity()
	if err != nil {
		t.Fatalf("NewForegroundFileSecurity() error = %v", err)
	}
	if err := PublishForegroundFile(directory, "journal", []byte("valid"), security); err != nil {
		t.Fatalf("PublishForegroundFile(journal) error = %v", err)
	}
	postDeleteRan := false
	if err := ConsumeForegroundFileLimitWithPostDelete(directory, "journal", 64, security, func(content []byte) error {
		if got, want := string(content), "valid"; got != want {
			return errors.New("managed file content changed before consumption")
		}
		return nil
	}, func() error {
		postDeleteRan = true
		if _, err := os.Lstat(filepath.Join(directory, "journal")); !os.IsNotExist(err) {
			return errors.New("managed file remains while post-delete callback runs")
		}
		return nil
	}); err != nil {
		t.Fatalf("ConsumeForegroundFileLimitWithPostDelete() error = %v", err)
	}
	if !postDeleteRan {
		t.Fatal("post-delete callback did not run")
	}
}

func TestConsumeForegroundFileLimitWithPostDeleteReportsFailureAfterRemoval(t *testing.T) {
	directory := newManagedFileDirectory(t)
	security, err := NewForegroundFileSecurity()
	if err != nil {
		t.Fatalf("NewForegroundFileSecurity() error = %v", err)
	}
	if err := PublishForegroundFile(directory, "journal", []byte("valid"), security); err != nil {
		t.Fatalf("PublishForegroundFile(journal) error = %v", err)
	}
	postDeleteErr := errors.New("post-delete failure")
	err = ConsumeForegroundFileLimitWithPostDelete(directory, "journal", 64, security, func([]byte) error {
		return nil
	}, func() error {
		return postDeleteErr
	})
	if !errors.Is(err, postDeleteErr) {
		t.Fatalf("ConsumeForegroundFileLimitWithPostDelete() error = %v, want post-delete failure", err)
	}
	if _, err := os.Lstat(filepath.Join(directory, "journal")); !os.IsNotExist(err) {
		t.Fatalf("post-delete failure restored managed file: %v", err)
	}
}

func TestPublishForegroundFileRejectsUnprotectedExistingFinalWithoutMutation(t *testing.T) {
	security, err := NewForegroundFileSecurity()
	if err != nil {
		t.Fatalf("NewForegroundFileSecurity() error = %v", err)
	}
	directory := newManagedFileDirectory(t)
	finalPath := filepath.Join(directory, "journal")
	if err := os.WriteFile(finalPath, []byte("untrusted"), 0o600); err != nil {
		t.Fatalf("WriteFile(untrusted final) error = %v", err)
	}
	handle := openManagedFileNoFollow(t, finalPath)
	beforeIdentity, err := foregroundFileID(handle)
	if err != nil {
		windows.CloseHandle(handle)
		t.Fatalf("foregroundFileID(untrusted final) error = %v", err)
	}
	if err := windows.CloseHandle(handle); err != nil {
		t.Fatalf("CloseHandle(untrusted final) error = %v", err)
	}
	if err := PublishForegroundFile(directory, "journal", []byte("replacement"), security); err == nil {
		t.Fatal("PublishForegroundFile(unprotected final) error = nil")
	}
	after, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("ReadFile(final after rejected replacement) error = %v", err)
	}
	if got, want := string(after), "untrusted"; got != want {
		t.Fatalf("unprotected final content = %q, want %q", got, want)
	}
	handle = openManagedFileNoFollow(t, finalPath)
	defer windows.CloseHandle(handle)
	afterIdentity, err := foregroundFileID(handle)
	if err != nil {
		t.Fatalf("foregroundFileID(final after rejected replacement) error = %v", err)
	}
	if afterIdentity != beforeIdentity {
		t.Fatalf("unprotected final identity changed: got %#v, want %#v", afterIdentity, beforeIdentity)
	}
}

func TestDeleteCreatedForegroundCandidateRejectsReplacedPathWithoutMutation(t *testing.T) {
	directory := newManagedFileDirectory(t)
	candidate, err := os.CreateTemp(directory, ".journal.tmp-*")
	if err != nil {
		t.Fatalf("CreateTemp() error = %v", err)
	}
	path := candidate.Name()
	identity, err := foregroundFileID(windows.Handle(candidate.Fd()))
	if err != nil {
		candidate.Close()
		t.Fatalf("foregroundFileID(candidate) error = %v", err)
	}
	if err := candidate.Close(); err != nil {
		t.Fatalf("Close(candidate) error = %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove(original candidate) error = %v", err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatalf("WriteFile(replacement candidate) error = %v", err)
	}
	if err := deleteCreatedForegroundCandidate(path, identity); err == nil {
		t.Fatal("deleteCreatedForegroundCandidate(replaced path) error = nil")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(replacement candidate) error = %v", err)
	}
	if got, want := string(content), "replacement"; got != want {
		t.Fatalf("replacement candidate content = %q, want %q", got, want)
	}
}

func TestValidateForegroundDirectoryTreeAcceptsManagedTreeWithoutMutation(t *testing.T) {
	parent := newManagedFileDirectory(t)
	directorySecurity, err := NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	root := filepath.Join(parent, "rollback")
	if err := CreateForegroundDirectoryChild(parent, "rollback", directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectoryChild(rollback) error = %v", err)
	}
	if err := CreateForegroundDirectoryChild(root, "credentials", directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectoryChild(credentials) error = %v", err)
	}
	fileSecurity, err := NewForegroundFileSecurity()
	if err != nil {
		t.Fatalf("NewForegroundFileSecurity() error = %v", err)
	}
	markerPath := filepath.Join(root, "credentials", "token.key")
	if err := PublishForegroundFile(filepath.Dir(markerPath), filepath.Base(markerPath), []byte("marker"), fileSecurity); err != nil {
		t.Fatalf("PublishForegroundFile(marker) error = %v", err)
	}
	if err := ValidateForegroundDirectoryTree(context.Background(), parent, "rollback"); err != nil {
		t.Fatalf("ValidateForegroundDirectoryTree() error = %v", err)
	}
	if err := ValidateForegroundDirectory(root); err != nil {
		t.Fatalf("ValidateForegroundDirectory(root) error = %v", err)
	}
	marker, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("ReadFile(marker) error = %v", err)
	}
	if got, want := string(marker), "marker"; got != want {
		t.Fatalf("marker content = %q, want %q", got, want)
	}
}

func TestValidateForegroundDirectoryTreeRejectsNestedJunctionWithoutMutation(t *testing.T) {
	parent := newManagedFileDirectory(t)
	directorySecurity, err := NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := CreateForegroundDirectoryChild(parent, "rollback", directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectoryChild(rollback) error = %v", err)
	}
	root := filepath.Join(parent, "rollback")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatalf("Mkdir(outside) error = %v", err)
	}
	markerPath := filepath.Join(outside, "marker")
	if err := os.WriteFile(markerPath, []byte("outside"), 0o600); err != nil {
		t.Fatalf("WriteFile(outside marker) error = %v", err)
	}
	junctionPath := filepath.Join(root, "nested")
	command := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", junctionPath, outside)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("mklink /J error = %v, output = %s", err, output)
	}
	if err := ValidateForegroundDirectoryTree(context.Background(), parent, "rollback"); err == nil {
		t.Fatal("ValidateForegroundDirectoryTree(junction) error = nil")
	}
	if _, err := os.Lstat(junctionPath); err != nil {
		t.Fatalf("Lstat(junction) error = %v", err)
	}
	marker, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("ReadFile(outside marker) error = %v", err)
	}
	if got, want := string(marker), "outside"; got != want {
		t.Fatalf("outside marker = %q, want %q", got, want)
	}
}

func TestValidateForegroundDirectoryTreeRejectsCanceledContextWithoutMutation(t *testing.T) {
	parent := newManagedFileDirectory(t)
	directorySecurity, err := NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := CreateForegroundDirectoryChild(parent, "rollback", directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectoryChild(rollback) error = %v", err)
	}
	root := filepath.Join(parent, "rollback")
	markerPath := filepath.Join(root, "marker")
	if err := os.WriteFile(markerPath, []byte("must remain"), 0o600); err != nil {
		t.Fatalf("WriteFile(marker) error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ValidateForegroundDirectoryTree(ctx, parent, "rollback"); err == nil {
		t.Fatal("ValidateForegroundDirectoryTree(canceled context) error = nil")
	}
	marker, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("ReadFile(marker) error = %v", err)
	}
	if got, want := string(marker), "must remain"; got != want {
		t.Fatalf("marker content = %q, want %q", got, want)
	}
}

func TestSyncForegroundDirectoryFlushesManagedDirectory(t *testing.T) {
	directory := newManagedFileDirectory(t)
	if err := SyncForegroundDirectory(directory); err != nil {
		t.Fatalf("SyncForegroundDirectory() error = %v", err)
	}
	if err := ValidateForegroundDirectory(directory); err != nil {
		t.Fatalf("ValidateForegroundDirectory() error = %v", err)
	}
}

func TestSyncForegroundDirectoryRejectsUnprotectedDirectory(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "unprotected")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatalf("Mkdir(unprotected) error = %v", err)
	}
	if err := SyncForegroundDirectory(directory); err == nil {
		t.Fatal("SyncForegroundDirectory(unprotected) error = nil")
	}
	if _, err := os.Lstat(directory); err != nil {
		t.Fatalf("Lstat(unprotected) error = %v", err)
	}
}

func TestSyncForegroundDirectoryRejectsUnsafeTargets(t *testing.T) {
	parent := newManagedFileDirectory(t)
	normalFile := filepath.Join(parent, "normal-file")
	if err := os.WriteFile(normalFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile(normal file) error = %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatalf("Mkdir(outside) error = %v", err)
	}
	junction := filepath.Join(parent, "junction")
	if output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", junction, outside).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J error = %v, output = %s", err, output)
	}

	for _, path := range []string{"relative", normalFile, junction} {
		if err := SyncForegroundDirectory(path); err == nil {
			t.Fatalf("SyncForegroundDirectory(%q) error = nil", path)
		}
	}
	if _, err := os.Lstat(junction); err != nil {
		t.Fatalf("Lstat(junction) error = %v", err)
	}
}

func TestRemoveForegroundDirectoryTreeDeletesManagedTree(t *testing.T) {
	parent := newManagedFileDirectory(t)
	directorySecurity, err := NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := CreateForegroundDirectoryChild(parent, "rollback", directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectoryChild(rollback) error = %v", err)
	}
	root := filepath.Join(parent, "rollback")
	if err := CreateForegroundDirectoryChild(root, "credentials", directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectoryChild(credentials) error = %v", err)
	}
	fileSecurity, err := NewForegroundFileSecurity()
	if err != nil {
		t.Fatalf("NewForegroundFileSecurity() error = %v", err)
	}
	if err := PublishForegroundFile(filepath.Join(root, "credentials"), "token.key", []byte("retired"), fileSecurity); err != nil {
		t.Fatalf("PublishForegroundFile(retired token) error = %v", err)
	}
	if err := PublishForegroundFile(parent, "current.key", []byte("current"), fileSecurity); err != nil {
		t.Fatalf("PublishForegroundFile(current token) error = %v", err)
	}

	removed, err := RemoveForegroundDirectoryTree(context.Background(), parent, "rollback")
	if err != nil {
		t.Fatalf("RemoveForegroundDirectoryTree() error = %v", err)
	}
	if !removed {
		t.Fatal("RemoveForegroundDirectoryTree() removed = false, want true")
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("Lstat(removed root) error = %v, want not exist", err)
	}
	current, err := ReadForegroundFile(parent, "current.key")
	if err != nil {
		t.Fatalf("ReadForegroundFile(current key) error = %v", err)
	}
	if got, want := string(current), "current"; got != want {
		t.Fatalf("current key = %q, want %q", got, want)
	}
}

func TestRemoveForegroundDirectoryTreeRejectsNestedJunctionWithoutDeletion(t *testing.T) {
	parent := newManagedFileDirectory(t)
	directorySecurity, err := NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := CreateForegroundDirectoryChild(parent, "rollback", directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectoryChild(rollback) error = %v", err)
	}
	root := filepath.Join(parent, "rollback")
	fileSecurity, err := NewForegroundFileSecurity()
	if err != nil {
		t.Fatalf("NewForegroundFileSecurity() error = %v", err)
	}
	protectedPath := filepath.Join(root, "protected.key")
	if err := PublishForegroundFile(root, "protected.key", []byte("retain"), fileSecurity); err != nil {
		t.Fatalf("PublishForegroundFile(protected key) error = %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatalf("Mkdir(outside) error = %v", err)
	}
	outsideMarker := filepath.Join(outside, "marker")
	if err := os.WriteFile(outsideMarker, []byte("outside"), 0o600); err != nil {
		t.Fatalf("WriteFile(outside marker) error = %v", err)
	}
	junctionPath := filepath.Join(root, "nested")
	if output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", junctionPath, outside).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J error = %v, output = %s", err, output)
	}

	removed, err := RemoveForegroundDirectoryTree(context.Background(), parent, "rollback")
	if err == nil {
		t.Fatal("RemoveForegroundDirectoryTree(junction) error = nil")
	}
	if removed {
		t.Fatal("RemoveForegroundDirectoryTree(junction) removed = true, want false")
	}
	if _, err := os.Lstat(junctionPath); err != nil {
		t.Fatalf("Lstat(junction) error = %v", err)
	}
	protected, err := os.ReadFile(protectedPath)
	if err != nil {
		t.Fatalf("ReadFile(protected key) error = %v", err)
	}
	if got, want := string(protected), "retain"; got != want {
		t.Fatalf("protected key = %q, want %q", got, want)
	}
	outsideContent, err := os.ReadFile(outsideMarker)
	if err != nil {
		t.Fatalf("ReadFile(outside marker) error = %v", err)
	}
	if got, want := string(outsideContent), "outside"; got != want {
		t.Fatalf("outside marker = %q, want %q", got, want)
	}
}

func TestRemoveForegroundDirectoryTreeRejectsUnprotectedChildBeforeDeletion(t *testing.T) {
	parent := newManagedFileDirectory(t)
	directorySecurity, err := NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := CreateForegroundDirectoryChild(parent, "rollback", directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectoryChild(rollback) error = %v", err)
	}
	root := filepath.Join(parent, "rollback")
	fileSecurity, err := NewForegroundFileSecurity()
	if err != nil {
		t.Fatalf("NewForegroundFileSecurity() error = %v", err)
	}
	protectedPath := filepath.Join(root, "protected.key")
	if err := PublishForegroundFile(root, "protected.key", []byte("retain"), fileSecurity); err != nil {
		t.Fatalf("PublishForegroundFile(protected key) error = %v", err)
	}
	unprotectedPath := filepath.Join(root, "unprotected.key")
	if err := os.WriteFile(unprotectedPath, []byte("reject"), 0o600); err != nil {
		t.Fatalf("WriteFile(unprotected key) error = %v", err)
	}

	removed, err := RemoveForegroundDirectoryTree(context.Background(), parent, "rollback")
	if err == nil {
		t.Fatal("RemoveForegroundDirectoryTree(unprotected child) error = nil")
	}
	if removed {
		t.Fatal("RemoveForegroundDirectoryTree(unprotected child) removed = true, want false")
	}
	protected, err := os.ReadFile(protectedPath)
	if err != nil {
		t.Fatalf("ReadFile(protected key) error = %v", err)
	}
	if got, want := string(protected), "retain"; got != want {
		t.Fatalf("protected key = %q, want %q", got, want)
	}
	unprotected, err := os.ReadFile(unprotectedPath)
	if err != nil {
		t.Fatalf("ReadFile(unprotected key) error = %v", err)
	}
	if got, want := string(unprotected), "reject"; got != want {
		t.Fatalf("unprotected key = %q, want %q", got, want)
	}
}

func TestRemoveForegroundDirectoryTreeRejectsIdentityChangedAfterPreflight(t *testing.T) {
	parent := newManagedFileDirectory(t)
	directorySecurity, err := NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := CreateForegroundDirectoryChild(parent, "rollback", directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectoryChild(rollback) error = %v", err)
	}
	root := filepath.Join(parent, "rollback")
	fileSecurity, err := NewForegroundFileSecurity()
	if err != nil {
		t.Fatalf("NewForegroundFileSecurity() error = %v", err)
	}
	markerPath := filepath.Join(root, "protected.key")
	if err := PublishForegroundFile(root, "protected.key", []byte("before"), fileSecurity); err != nil {
		t.Fatalf("PublishForegroundFile(before) error = %v", err)
	}
	parentHandle, err := openValidatedForegroundDirectory(parent)
	if err != nil {
		t.Fatalf("openValidatedForegroundDirectory(parent) error = %v", err)
	}
	defer windows.CloseHandle(parentHandle)
	plan, err := collectForegroundDirectoryTreeWithParent(context.Background(), parent, "rollback", parentHandle)
	if err != nil {
		t.Fatalf("collectForegroundDirectoryTreeWithParent() error = %v", err)
	}
	if err := os.Remove(markerPath); err != nil {
		t.Fatalf("Remove(original marker) error = %v", err)
	}
	if err := PublishForegroundFile(root, "protected.key", []byte("replacement"), fileSecurity); err != nil {
		t.Fatalf("PublishForegroundFile(replacement) error = %v", err)
	}

	if err := removeForegroundTreePlan(context.Background(), plan); err == nil {
		t.Fatal("removeForegroundTreePlan(identity changed) error = nil")
	}
	replacement, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("ReadFile(replacement marker) error = %v", err)
	}
	if got, want := string(replacement), "replacement"; got != want {
		t.Fatalf("replacement marker = %q, want %q", got, want)
	}
	if err := ValidateForegroundDirectory(root); err != nil {
		t.Fatalf("ValidateForegroundDirectory(root) error = %v", err)
	}
}

func TestRemoveForegroundDirectoryTreeStopsAfterCancellationDuringDeletion(t *testing.T) {
	parent := newManagedFileDirectory(t)
	directorySecurity, err := NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := CreateForegroundDirectoryChild(parent, "rollback", directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectoryChild(rollback) error = %v", err)
	}
	root := filepath.Join(parent, "rollback")
	fileSecurity, err := NewForegroundFileSecurity()
	if err != nil {
		t.Fatalf("NewForegroundFileSecurity() error = %v", err)
	}
	for _, name := range []string{"first.key", "second.key"} {
		if err := PublishForegroundFile(root, name, []byte(name), fileSecurity); err != nil {
			t.Fatalf("PublishForegroundFile(%q) error = %v", name, err)
		}
	}
	plan, err := collectForegroundDirectoryTree(context.Background(), parent, "rollback")
	if err != nil {
		t.Fatalf("collectForegroundDirectoryTree() error = %v", err)
	}
	if len(plan) != 3 || plan[0].directory || plan[1].directory || !plan[2].directory {
		t.Fatalf("tree removal plan = %#v, want two files followed by root", plan)
	}
	ctx := &cancelAfterFirstCheckContext{Context: context.Background()}
	if err := removeForegroundTreePlan(ctx, plan); !errors.Is(err, context.Canceled) {
		t.Fatalf("removeForegroundTreePlan(canceled) error = %v, want context canceled", err)
	}
	if _, err := os.Lstat(plan[0].path); !os.IsNotExist(err) {
		t.Fatalf("Lstat(first deleted plan node) error = %v, want not exist", err)
	}
	remaining, err := os.ReadFile(plan[1].path)
	if err != nil {
		t.Fatalf("ReadFile(remaining plan node) error = %v", err)
	}
	if got, want := string(remaining), filepath.Base(plan[1].path); got != want {
		t.Fatalf("remaining plan node = %q, want %q", got, want)
	}
	if err := ValidateForegroundDirectory(root); err != nil {
		t.Fatalf("ValidateForegroundDirectory(root) error = %v", err)
	}
}

func TestRemoveForegroundDirectoryTreeRejectsCanceledContextWithoutDeletion(t *testing.T) {
	parent := newManagedFileDirectory(t)
	directorySecurity, err := NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := CreateForegroundDirectoryChild(parent, "rollback", directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectoryChild(rollback) error = %v", err)
	}
	root := filepath.Join(parent, "rollback")
	fileSecurity, err := NewForegroundFileSecurity()
	if err != nil {
		t.Fatalf("NewForegroundFileSecurity() error = %v", err)
	}
	markerPath := filepath.Join(root, "protected.key")
	if err := PublishForegroundFile(root, "protected.key", []byte("retain"), fileSecurity); err != nil {
		t.Fatalf("PublishForegroundFile(marker) error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	removed, err := RemoveForegroundDirectoryTree(ctx, parent, "rollback")
	if err == nil {
		t.Fatal("RemoveForegroundDirectoryTree(canceled context) error = nil")
	}
	if removed {
		t.Fatal("RemoveForegroundDirectoryTree(canceled context) removed = true, want false")
	}
	marker, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("ReadFile(marker) error = %v", err)
	}
	if got, want := string(marker), "retain"; got != want {
		t.Fatalf("marker content = %q, want %q", got, want)
	}
}

func TestReplaceAndDeleteForegroundFileKeepManagedBoundary(t *testing.T) {
	security, err := NewForegroundFileSecurity()
	if err != nil {
		t.Fatalf("NewForegroundFileSecurity() error = %v", err)
	}
	directory := newManagedFileDirectory(t)
	if err := PublishForegroundFile(directory, "identity.rotate", []byte("replacement"), security); err != nil {
		t.Fatalf("PublishForegroundFile(candidate) error = %v", err)
	}
	if err := ReplaceForegroundFile(directory, "identity.rotate", "identity.key", security); err != nil {
		t.Fatalf("ReplaceForegroundFile() error = %v", err)
	}
	content, err := ReadForegroundFile(directory, "identity.key")
	if err != nil {
		t.Fatalf("ReadForegroundFile(replaced) error = %v", err)
	}
	if got, want := string(content), "replacement"; got != want {
		t.Fatalf("replaced content = %q, want %q", got, want)
	}
	if err := DeleteForegroundFile(directory, "identity.key", security); err != nil {
		t.Fatalf("DeleteForegroundFile() error = %v", err)
	}
	if _, err := ReadForegroundFile(directory, "identity.key"); !os.IsNotExist(err) {
		t.Fatalf("ReadForegroundFile(removed) error = %v, want not exist", err)
	}
}

func TestCreateAndMoveForegroundDirectoryChildKeepsIdentityAndDACL(t *testing.T) {
	parent := newManagedFileDirectory(t)
	security, err := NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := CreateForegroundDirectoryChild(parent, "target", security); err != nil {
		t.Fatalf("CreateForegroundDirectoryChild(target) error = %v", err)
	}
	if err := CreateForegroundDirectoryChild(parent, "staging", security); err != nil {
		t.Fatalf("CreateForegroundDirectoryChild(staging) error = %v", err)
	}
	targetPath := filepath.Join(parent, "target")
	stagingPath := filepath.Join(parent, "staging")
	targetIdentity := managedDirectoryIdentity(t, targetPath)
	stagingIdentity := managedDirectoryIdentity(t, stagingPath)
	if err := os.WriteFile(filepath.Join(targetPath, "marker"), []byte("old"), 0o600); err != nil {
		t.Fatalf("WriteFile(target marker) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(stagingPath, "marker"), []byte("new"), 0o600); err != nil {
		t.Fatalf("WriteFile(staging marker) error = %v", err)
	}

	if err := MoveForegroundDirectory(parent, "target", "rollback", security); err != nil {
		t.Fatalf("MoveForegroundDirectory(target, rollback) error = %v", err)
	}
	if got := managedDirectoryIdentity(t, filepath.Join(parent, "rollback")); got != targetIdentity {
		t.Fatalf("rollback identity = %#v, want %#v", got, targetIdentity)
	}
	if err := MoveForegroundDirectory(parent, "staging", "target", security); err != nil {
		t.Fatalf("MoveForegroundDirectory(staging, target) error = %v", err)
	}
	if got := managedDirectoryIdentity(t, targetPath); got != stagingIdentity {
		t.Fatalf("target identity = %#v, want %#v", got, stagingIdentity)
	}
	for _, path := range []string{targetPath, filepath.Join(parent, "rollback")} {
		if err := ValidateForegroundDirectory(path); err != nil {
			t.Fatalf("ValidateForegroundDirectory(%q) error = %v", path, err)
		}
	}
	content, err := os.ReadFile(filepath.Join(targetPath, "marker"))
	if err != nil {
		t.Fatalf("ReadFile(target marker) error = %v", err)
	}
	if string(content) != "new" {
		t.Fatalf("target marker = %q, want new", content)
	}
}

func TestMoveForegroundDirectoryRejectsExistingDestinationWithoutMutation(t *testing.T) {
	parent := newManagedFileDirectory(t)
	security, err := NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	for _, name := range []string{"source", "destination"} {
		if err := CreateForegroundDirectoryChild(parent, name, security); err != nil {
			t.Fatalf("CreateForegroundDirectoryChild(%q) error = %v", name, err)
		}
	}
	sourcePath := filepath.Join(parent, "source")
	destinationPath := filepath.Join(parent, "destination")
	sourceIdentity := managedDirectoryIdentity(t, sourcePath)
	destinationIdentity := managedDirectoryIdentity(t, destinationPath)
	if err := MoveForegroundDirectory(parent, "source", "destination", security); err == nil {
		t.Fatal("MoveForegroundDirectory(existing destination) error = nil")
	}
	if got := managedDirectoryIdentity(t, sourcePath); got != sourceIdentity {
		t.Fatalf("source identity changed: got %#v, want %#v", got, sourceIdentity)
	}
	if got := managedDirectoryIdentity(t, destinationPath); got != destinationIdentity {
		t.Fatalf("destination identity changed: got %#v, want %#v", got, destinationIdentity)
	}
}

func TestMoveForegroundDirectoryRejectsReparseSourceWithoutMutation(t *testing.T) {
	parent := newManagedFileDirectory(t)
	security, err := NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	realDirectory := filepath.Join(t.TempDir(), "real")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatalf("os.Mkdir(real directory) error = %v", err)
	}
	markerPath := filepath.Join(realDirectory, "marker")
	if err := os.WriteFile(markerPath, []byte("must remain"), 0o600); err != nil {
		t.Fatalf("WriteFile(real marker) error = %v", err)
	}
	sourcePath := filepath.Join(parent, "source")
	if output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", sourcePath, realDirectory).CombinedOutput(); err != nil {
		t.Fatalf("create source junction: %v: %s", err, output)
	}

	if err := MoveForegroundDirectory(parent, "source", "destination", security); err == nil {
		t.Fatal("MoveForegroundDirectory(reparse source) error = nil")
	}
	if _, err := os.Lstat(sourcePath); err != nil {
		t.Fatalf("Lstat(source junction) error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(parent, "destination")); !os.IsNotExist(err) {
		t.Fatalf("destination changed after reparse rejection: %v", err)
	}
	content, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("ReadFile(real marker) error = %v", err)
	}
	if string(content) != "must remain" {
		t.Fatalf("real marker = %q, want unchanged", content)
	}
}

func TestForegroundFileSecurityRejectsInheritedFile(t *testing.T) {
	security, err := NewForegroundFileSecurity()
	if err != nil {
		t.Fatalf("NewForegroundFileSecurity() error = %v", err)
	}
	path := filepath.Join(t.TempDir(), "inherited")
	if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	handle := openManagedFileNoFollow(t, path)
	defer windows.CloseHandle(handle)
	if err := security.ValidateFile(handle); err == nil {
		t.Fatal("ValidateFile() error = nil, want inherited DACL rejection")
	}
}

func TestValidateManagedLeafNameRejectsWindowsSpecialNames(t *testing.T) {
	for _, name := range []string{
		"", ".", "..", "key:alternate", "CON", "LPT9", "name.", "name ", "nested\\key", "line\nkey",
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateManagedLeafName(name); err == nil {
				t.Fatalf("validateManagedLeafName(%q) error = nil", name)
			}
		})
	}
	for _, name := range []string{"tunnel-token.key", ".candidate-1", "agent-gateway.crt.rotate"} {
		t.Run("accept_"+name, func(t *testing.T) {
			if err := validateManagedLeafName(name); err != nil {
				t.Fatalf("validateManagedLeafName(%q) error = %v", name, err)
			}
		})
	}
}

func openManagedFileNoFollow(t *testing.T, path string) windows.Handle {
	t.Helper()
	handle, err := openFileNoFollow(path)
	if err != nil {
		t.Fatalf("openFileNoFollow(%q) error = %v", path, err)
	}
	return handle
}

func newManagedFileDirectory(t *testing.T) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "managed")
	security, err := NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := CreateForegroundDirectory(directory, security); err != nil {
		t.Fatalf("CreateForegroundDirectory() error = %v", err)
	}
	return directory
}

func managedDirectoryIdentity(t *testing.T, path string) foregroundDirectoryIdentity {
	t.Helper()
	handle, err := openForegroundDirectoryNoFollow(path)
	if err != nil {
		t.Fatalf("openForegroundDirectoryNoFollow(%q) error = %v", path, err)
	}
	defer windows.CloseHandle(handle)
	identity, err := foregroundDirectoryID(handle)
	if err != nil {
		t.Fatalf("foregroundDirectoryID(%q) error = %v", path, err)
	}
	return identity
}
