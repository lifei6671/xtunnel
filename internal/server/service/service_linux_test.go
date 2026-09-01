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
		name    string
		output  string
		runErr  error
		wantErr string
	}{
		{name: "minimum", output: "249"},
		{name: "newer", output: "257 (257.5-2)"},
		{name: "old", output: "248", wantErr: "systemd 249 or newer is required; found 248"},
		{name: "query failure", runErr: errors.New("injected"), wantErr: "query running systemd: injected"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			err := requireSupportedSystemd(context.Background(), "/usr/bin/systemctl", func(_ context.Context, path string, args ...string) (string, error) {
				calls++
				if path != "/usr/bin/systemctl" || strings.Join(args, " ") != "show --property=Version --value" {
					t.Fatalf("command = %s %v", path, args)
				}
				return test.output, test.runErr
			})
			if test.wantErr == "" && err != nil {
				t.Fatalf("requireSupportedSystemd() error = %v", err)
			}
			if test.wantErr != "" && (err == nil || err.Error() != test.wantErr) {
				t.Fatalf("requireSupportedSystemd() error = %v, want %q", err, test.wantErr)
			}
			if calls != 1 {
				t.Fatalf("calls = %d, want 1", calls)
			}
		})
	}
}

func TestActivateManagedServiceCommandOrder(t *testing.T) {
	var commands []string
	progress, err := activateManagedService(context.Background(), "/usr/bin/systemctl", func(_ context.Context, path string, args ...string) (string, error) {
		commands = append(commands, strings.Join(append([]string{path}, args...), " "))
		return "", nil
	})
	if err != nil {
		t.Fatalf("activateManagedService() error = %v", err)
	}
	if !progress.enableAttempted || !progress.restartAttempted {
		t.Fatalf("progress = %#v", progress)
	}
	want := []string{
		"/usr/bin/systemctl daemon-reload",
		"/usr/bin/systemctl enable xtunnel-server.service",
		"/usr/bin/systemctl restart xtunnel-server.service",
		"/usr/bin/systemctl is-active --quiet xtunnel-server.service",
	}
	if strings.Join(commands, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands = %#v, want %#v", commands, want)
	}
}

func TestInspectPreviousServiceState(t *testing.T) {
	tests := []struct {
		name          string
		unitFileState string
		activeState   string
		want          previousServiceState
		wantErr       string
	}{
		{name: "enabled active", unitFileState: "enabled", activeState: "active", want: previousServiceState{unitExisted: true, enablement: "enabled", active: true}},
		{name: "runtime enabled active", unitFileState: "enabled-runtime", activeState: "active", want: previousServiceState{unitExisted: true, enablement: "enabled-runtime", active: true}},
		{name: "disabled inactive", unitFileState: "disabled", activeState: "inactive", want: previousServiceState{unitExisted: true, enablement: "disabled"}},
		{name: "transitional", unitFileState: "enabled", activeState: "activating", wantErr: "transitional ActiveState"},
		{name: "unknown enablement", unitFileState: "bad", activeState: "inactive", wantErr: "unsupported previous Server UnitFileState"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, err := inspectPreviousServiceState(context.Background(), "/usr/bin/systemctl", true, func(_ context.Context, _ string, args ...string) (string, error) {
				switch strings.Join(args, " ") {
				case "show --property=UnitFileState --value xtunnel-server.service":
					return test.unitFileState, nil
				case "show --property=ActiveState --value xtunnel-server.service":
					return test.activeState, nil
				default:
					t.Fatalf("unexpected systemctl args: %v", args)
					return "", nil
				}
			})
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("inspectPreviousServiceState() error = %v, want substring %q", err, test.wantErr)
				}
				return
			}
			if err != nil || state != test.want {
				t.Fatalf("inspectPreviousServiceState() = (%#v, %v), want %#v", state, err, test.want)
			}
		})
	}
}

func TestRollbackRestoresPreviousEnablementAndActivity(t *testing.T) {
	tests := []struct {
		name     string
		previous previousServiceState
		before   []string
		after    []string
	}{
		{
			name:     "first install",
			previous: previousServiceState{},
			before:   []string{"disable --now xtunnel-server.service"},
			after:    []string{"daemon-reload"},
		},
		{
			name:     "disabled inactive upgrade",
			previous: previousServiceState{unitExisted: true, enablement: "disabled"},
			before:   []string{"stop xtunnel-server.service"},
			after:    []string{"daemon-reload", "disable xtunnel-server.service", "stop xtunnel-server.service"},
		},
		{
			name:     "enabled active upgrade",
			previous: previousServiceState{unitExisted: true, enablement: "enabled", active: true},
			before:   []string{"stop xtunnel-server.service"},
			after:    []string{"daemon-reload", "enable xtunnel-server.service", "restart xtunnel-server.service", "is-active --quiet xtunnel-server.service"},
		},
		{
			name:     "runtime-enabled active upgrade",
			previous: previousServiceState{unitExisted: true, enablement: "enabled-runtime", active: true},
			before:   []string{"stop xtunnel-server.service"},
			after:    []string{"daemon-reload", "disable xtunnel-server.service", "enable --runtime xtunnel-server.service", "restart xtunnel-server.service", "is-active --quiet xtunnel-server.service"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var before, after []string
			recordBefore := func(_ context.Context, _ string, args ...string) (string, error) {
				before = append(before, strings.Join(args, " "))
				return "", nil
			}
			recordAfter := func(_ context.Context, _ string, args ...string) (string, error) {
				after = append(after, strings.Join(args, " "))
				return "", nil
			}
			progress := serviceActivationProgress{enableAttempted: true, restartAttempted: true}
			if err := rollbackLinuxActivationBeforeRestore(context.Background(), "/usr/bin/systemctl", test.previous, progress, recordBefore); err != nil {
				t.Fatalf("rollbackLinuxActivationBeforeRestore() error = %v", err)
			}
			if err := rollbackLinuxActivationAfterRestore(context.Background(), "/usr/bin/systemctl", test.previous, progress, recordAfter); err != nil {
				t.Fatalf("rollbackLinuxActivationAfterRestore() error = %v", err)
			}
			if strings.Join(before, "\n") != strings.Join(test.before, "\n") {
				t.Fatalf("before commands = %#v, want %#v", before, test.before)
			}
			if strings.Join(after, "\n") != strings.Join(test.after, "\n") {
				t.Fatalf("after commands = %#v, want %#v", after, test.after)
			}
		})
	}
}

func TestApplyLinuxInstallRollsBackPublicationAndActivationFailures(t *testing.T) {
	for _, failActivation := range []bool{false, true} {
		name := "publication"
		if failActivation {
			name = "activation"
		}
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			paths := []string{filepath.Join(directory, "server"), filepath.Join(directory, "server.yaml"), filepath.Join(directory, "server.service")}
			for _, path := range paths {
				if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
					t.Fatalf("os.WriteFile() error = %v", err)
				}
			}
			publications := make([]linuxInstallPublication, 0, len(paths))
			for index, path := range paths {
				index, path := index, path
				publications = append(publications, linuxInstallPublication{
					path: path, label: "publish",
					publish: func() error {
						if err := atomicWriteBytes(path, []byte("new"), 0o600, -1, -1); err != nil {
							return err
						}
						if !failActivation && index == 1 {
							return errors.New("injected publication failure")
						}
						return nil
					},
				})
			}
			before, after := false, false
			err := applyLinuxInstall(
				publications,
				func() (serviceActivationProgress, error) {
					if failActivation {
						return serviceActivationProgress{enableAttempted: true, restartAttempted: true}, errors.New("injected activation failure")
					}
					return serviceActivationProgress{}, nil
				},
				func(serviceActivationProgress) error { before = true; return nil },
				func(serviceActivationProgress) error { after = true; return nil },
			)
			if err == nil {
				t.Fatal("applyLinuxInstall() error = nil")
			}
			if before != failActivation || after != failActivation {
				t.Fatalf("recovery callbacks = (%v,%v), want %v", before, after, failActivation)
			}
			for _, path := range paths {
				content, readErr := os.ReadFile(path)
				if readErr != nil || string(content) != "old" {
					t.Fatalf("restored %s = %q, %v", path, content, readErr)
				}
			}
		})
	}
}

func TestRejectLegacyServerLayout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "xtunnel.db")
	if err := os.WriteFile(path, []byte("legacy"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	if err := rejectLegacyServerLayout([]string{path}); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("rejectLegacyServerLayout() error = %v", err)
	}
}

func TestEnsureInstallDirectoryRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("os.Mkdir() error = %v", err)
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("os.Symlink() error = %v", err)
	}
	if err := ensureInstallDirectory(link, 0o700); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("ensureInstallDirectory() error = %v", err)
	}
}

func TestInspectOwnedUnitRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "managed.service")
	if err := os.WriteFile(target, unitFile, 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	link := filepath.Join(directory, "xtunnel-server.service")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("os.Symlink() error = %v", err)
	}
	exists, owned, err := inspectOwnedUnit(link)
	if err != nil || !exists || owned {
		t.Fatalf("inspectOwnedUnit(symlink) = (%v, %v, %v), want (true, false, nil)", exists, owned, err)
	}
}

func TestSecureInstallDirectoryRepairsExistingMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "xtunnel")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("os.Mkdir() error = %v", err)
	}
	if err := secureInstallDirectory(path, 0o755, -1, -1); err != nil {
		t.Fatalf("secureInstallDirectory() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("os.Stat() error = %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("directory mode = %o, want 755", info.Mode().Perm())
	}
}
