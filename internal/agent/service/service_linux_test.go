//go:build linux

package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRequireSupportedSystemd(t *testing.T) {
	tests := []struct {
		name       string
		output     string
		runErr     error
		wantErr    string
		wantSystem string
	}{
		{name: "minimum supported version", output: "249", wantSystem: "/usr/bin/systemctl"},
		{name: "newer version", output: "257 (257.5-2)", wantSystem: "/usr/bin/systemctl"},
		{name: "old version", output: "248", wantErr: "systemd 249 or newer is required; found 248", wantSystem: "/usr/bin/systemctl"},
		{name: "query failure", runErr: errors.New("injected query failure"), wantErr: "query running systemd: injected query failure", wantSystem: "/usr/bin/systemctl"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			run := func(_ context.Context, path string, args ...string) (string, error) {
				calls++
				if path != test.wantSystem {
					t.Fatalf("systemctl path = %q, want %q", path, test.wantSystem)
				}
				wantArgs := []string{"show", "--property=Version", "--value"}
				if strings.Join(args, "\x00") != strings.Join(wantArgs, "\x00") {
					t.Fatalf("systemctl args = %q, want %q", args, wantArgs)
				}
				return test.output, test.runErr
			}

			err := requireSupportedSystemd(context.Background(), test.wantSystem, run)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("requireSupportedSystemd() error = %v", err)
				}
			} else if err == nil || err.Error() != test.wantErr {
				t.Fatalf("requireSupportedSystemd() error = %v, want %q", err, test.wantErr)
			}
			if calls != 1 {
				t.Fatalf("systemctl calls = %d, want 1", calls)
			}
		})
	}
}

func TestApplyLinuxInstallRollsBackEveryFailureStage(t *testing.T) {
	tests := []struct {
		name          string
		failPublishAt int
		failActivate  bool
	}{
		{name: "binary publication", failPublishAt: 0},
		{name: "credential publication", failPublishAt: 1},
		{name: "unit publication", failPublishAt: 2},
		{name: "service activation", failPublishAt: -1, failActivate: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			paths := []string{
				filepath.Join(directory, "xtunnel-agent"),
				filepath.Join(directory, "agent.token"),
				filepath.Join(directory, "xtunnel-agent.service"),
			}
			oldContents := []string{"old-binary", "old-token", "old-unit"}
			publications := make([]linuxInstallPublication, 0, len(paths))
			for index, path := range paths {
				if err := os.WriteFile(path, []byte(oldContents[index]), 0o600); err != nil {
					t.Fatalf("os.WriteFile(old) error = %v", err)
				}
				index := index
				path := path
				publications = append(publications, linuxInstallPublication{
					path:  path,
					label: "publish test file",
					publish: func() error {
						if err := atomicWriteBytes(path, []byte("new-"+oldContents[index]), 0o600, -1, -1); err != nil {
							return err
						}
						if test.failPublishAt == index {
							return errors.New("injected publication failure")
						}
						return nil
					},
				})
			}

			beforeRestoreCalled := false
			afterRestoreCalled := false
			err := applyLinuxInstall(
				publications,
				func() (serviceActivationProgress, error) {
					if test.failActivate {
						return serviceActivationProgress{enableAttempted: true, restartAttempted: true}, errors.New("injected activation failure")
					}
					return serviceActivationProgress{}, nil
				},
				func(serviceActivationProgress) error {
					beforeRestoreCalled = true
					return nil
				},
				func(serviceActivationProgress) error {
					afterRestoreCalled = true
					return nil
				},
			)
			if err == nil {
				t.Fatal("applyLinuxInstall() error = nil, want injected failure")
			}
			if test.failActivate != beforeRestoreCalled || test.failActivate != afterRestoreCalled {
				t.Fatalf("recovery callbacks = (%v, %v), want activation recovery = %v", beforeRestoreCalled, afterRestoreCalled, test.failActivate)
			}
			assertLinuxInstallFiles(t, paths, oldContents)
			assertNoLinuxInstallSnapshots(t, directory)
		})
	}
}

func TestApplyLinuxInstallRemovesFilesFromFailedFirstInstall(t *testing.T) {
	directory := t.TempDir()
	paths := []string{
		filepath.Join(directory, "xtunnel-agent"),
		filepath.Join(directory, "agent.token"),
		filepath.Join(directory, "xtunnel-agent.service"),
	}
	publications := make([]linuxInstallPublication, 0, len(paths))
	for _, path := range paths {
		path := path
		publications = append(publications, linuxInstallPublication{
			path:  path,
			label: "publish test file",
			publish: func() error {
				return atomicWriteBytes(path, []byte("new"), 0o600, -1, -1)
			},
		})
	}
	err := applyLinuxInstall(
		publications,
		func() (serviceActivationProgress, error) {
			return serviceActivationProgress{enableAttempted: true}, errors.New("injected activation failure")
		},
		func(serviceActivationProgress) error { return nil },
		func(serviceActivationProgress) error { return nil },
	)
	if err == nil {
		t.Fatal("applyLinuxInstall() error = nil, want injected failure")
	}
	for _, path := range paths {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("os.Stat(%s) error = %v, want os.ErrNotExist", path, err)
		}
	}
	assertNoLinuxInstallSnapshots(t, directory)
}

func TestApplyLinuxInstallReportsRollbackFailure(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "xtunnel-agent")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(old) error = %v", err)
	}
	err := applyLinuxInstall(
		[]linuxInstallPublication{{
			path:  path,
			label: "publish binary",
			publish: func() error {
				if err := os.RemoveAll(path); err != nil {
					return err
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					return err
				}
				return errors.New("injected publication failure")
			},
		}},
		func() (serviceActivationProgress, error) { return serviceActivationProgress{}, nil },
		func(serviceActivationProgress) error { return nil },
		func(serviceActivationProgress) error { return nil },
	)
	if err == nil || !strings.Contains(err.Error(), "roll back Agent install files") {
		t.Fatalf("applyLinuxInstall() error = %v, want explicit rollback failure", err)
	}
}

func assertLinuxInstallFiles(t *testing.T, paths, contents []string) {
	t.Helper()
	for index, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("os.ReadFile(%s) error = %v", path, err)
		}
		if string(content) != contents[index] {
			t.Fatalf("content of %s = %q, want %q", path, content, contents[index])
		}
	}
}

func assertNoLinuxInstallSnapshots(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("os.ReadDir() error = %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".rollback-") {
			t.Fatalf("rollback snapshot %s was not removed", entry.Name())
		}
	}
}
