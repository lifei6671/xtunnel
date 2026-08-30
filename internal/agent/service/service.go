// Package service installs, runs, and removes the native XTunnel Agent service.
package service

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const (
	UnitName             = "xtunnel-agent.service"
	BinaryPath           = "/usr/local/bin/xtunnel-agent"
	UnitPath             = "/etc/systemd/system/xtunnel-agent.service"
	CredentialDirectory  = "/etc/xtunnel/credentials"
	CredentialSourcePath = "/etc/xtunnel/credentials/agent.token"
	ManagedUnitMarker    = "# Managed by xtunnel-agent service install"
)

var ErrUnsupported = errors.New("agent service management is only supported on Linux and Windows")

func validateServiceArchitecture(goarch string) error {
	if goarch == "amd64" || goarch == "arm64" {
		return nil
	}
	return fmt.Errorf("agent service management is not supported on architecture %s", goarch)
}

//go:embed xtunnel-agent.service
var unitFile []byte

func parseSystemdVersion(output string) (int, error) {
	text := strings.TrimSpace(output)
	text = strings.TrimSpace(strings.TrimPrefix(text, "systemd"))
	end := 0
	for end < len(text) && text[end] >= '0' && text[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, fmt.Errorf("unable to determine systemd version")
	}
	version, err := strconv.Atoi(text[:end])
	if err != nil {
		return 0, fmt.Errorf("parse systemd version: %w", err)
	}
	return version, nil
}

func inspectManagedUnit(path string) (exists bool, managed bool, err error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("inspect service unit: %w", err)
	}
	if !info.Mode().IsRegular() {
		return true, false, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return true, false, fmt.Errorf("read service unit: %w", err)
	}
	return true, bytes.HasPrefix(content, []byte(ManagedUnitMarker)), nil
}

func atomicWriteFile(path string, mode os.FileMode, uid, gid int, write func(*os.File) error) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", filepath.Base(path), err)
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			_ = temporary.Close()
		}
		_ = os.Remove(temporaryPath)
	}()

	if err := temporary.Chmod(mode); err != nil {
		return fmt.Errorf("set temporary file mode for %s: %w", filepath.Base(path), err)
	}
	if uid >= 0 && gid >= 0 {
		if err := temporary.Chown(uid, gid); err != nil {
			return fmt.Errorf("set temporary file owner for %s: %w", filepath.Base(path), err)
		}
	}
	if err := write(temporary); err != nil {
		return fmt.Errorf("write temporary file for %s: %w", filepath.Base(path), err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync temporary file for %s: %w", filepath.Base(path), err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary file for %s: %w", filepath.Base(path), err)
	}
	closed = true
	if err := replaceFile(temporaryPath, path); err != nil {
		return fmt.Errorf("publish %s: %w", filepath.Base(path), err)
	}
	if err := syncDirectory(directory); err != nil {
		return fmt.Errorf("sync directory for %s: %w", filepath.Base(path), err)
	}
	return nil
}

func atomicWriteBytes(path string, content []byte, mode os.FileMode, uid, gid int) error {
	return atomicWriteFile(path, mode, uid, gid, func(destination *os.File) error {
		_, err := destination.Write(content)
		return err
	})
}

func atomicCopyFile(path, source string, mode os.FileMode, uid, gid int) error {
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open current executable: %w", err)
	}
	defer input.Close()
	return atomicWriteFile(path, mode, uid, gid, func(destination *os.File) error {
		_, err := io.Copy(destination, input)
		return err
	})
}

func activateManagedService(
	ctx context.Context,
	systemctl string,
	run func(context.Context, string, ...string) (string, error),
) error {
	_, err := activateManagedServiceTracked(ctx, systemctl, run)
	return err
}

type serviceActivationProgress struct {
	enableAttempted  bool
	restartAttempted bool
}

func activateManagedServiceTracked(
	ctx context.Context,
	systemctl string,
	run func(context.Context, string, ...string) (string, error),
) (serviceActivationProgress, error) {
	var progress serviceActivationProgress
	if _, err := run(ctx, systemctl, "daemon-reload"); err != nil {
		return progress, fmt.Errorf("reload systemd units: %w", err)
	}
	progress.enableAttempted = true
	if _, err := run(ctx, systemctl, "enable", UnitName); err != nil {
		return progress, fmt.Errorf("enable Agent service: %w", err)
	}
	progress.restartAttempted = true
	if _, err := run(ctx, systemctl, "restart", UnitName); err != nil {
		return progress, fmt.Errorf("restart Agent service: %w", err)
	}
	if _, err := run(ctx, systemctl, "is-active", "--quiet", UnitName); err != nil {
		return progress, fmt.Errorf("verify Agent service is active: %w", err)
	}
	return progress, nil
}

// Install installs the current executable and starts the managed native service.
func Install(ctx context.Context, token string) error {
	if err := validateServiceArchitecture(runtime.GOARCH); err != nil {
		return err
	}
	return install(ctx, token)
}

// UninstallResult describes cleanup that cannot finish until the next reboot.
type UninstallResult struct {
	BinaryRemovalPendingReboot bool
}

// Uninstall stops the managed service and removes its registration and installed binary.
func Uninstall(ctx context.Context) (UninstallResult, error) {
	if err := validateServiceArchitecture(runtime.GOARCH); err != nil {
		return UninstallResult{}, err
	}
	return uninstall(ctx)
}

// Name returns the platform service name shown by service management commands.
func Name() string {
	return platformName()
}

// RunIfManagedService runs callback under the native service dispatcher when
// the current process was started by it. The callback receives the platform's
// persistent service log writer. Foreground processes return handled=false without
// invoking the callback.
func RunIfManagedService(callback func(context.Context, string, io.Writer) error) (handled bool, err error) {
	return runIfManagedService(callback)
}
