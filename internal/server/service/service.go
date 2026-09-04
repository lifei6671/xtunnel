// Package service installs and removes the native XTunnel Server service.
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
	UnitName          = "xtunnel-server.service"
	BinaryPath        = "/usr/local/bin/xtunnel-server"
	ConfigPath        = "/etc/xtunnel/server.yaml"
	UnitPath          = "/etc/systemd/system/xtunnel-server.service"
	DataDirectory     = "/var/lib/xtunnel/data"
	ManagedUnitMarker = "# Managed by xtunnel-server service install"
)

var ErrUnsupported = errors.New("Server service management requires Linux or Windows amd64")

//go:embed xtunnel-server.service
var unitFile []byte

//go:embed xtunnel-server-legacy.service
var legacyUnitFile []byte

// Install installs the current executable and supplied configuration, then
// enables and starts the managed native service.
func Install(ctx context.Context, configSource string) error {
	if err := validateServicePlatform(runtime.GOOS, runtime.GOARCH); err != nil {
		return err
	}
	return install(ctx, configSource)
}

// Uninstall stops the managed service and removes its registration and installed
// binary. Server configuration, credentials, and data remain.
func Uninstall(ctx context.Context) error {
	if err := validateServicePlatform(runtime.GOOS, runtime.GOARCH); err != nil {
		return err
	}
	return uninstall(ctx)
}

func validateServicePlatform(goos, goarch string) error {
	if goos == "windows" && goarch == "amd64" {
		return nil
	}
	if goos != "linux" {
		return ErrUnsupported
	}
	if goarch != "amd64" && goarch != "arm64" {
		return fmt.Errorf("Server service management is not supported on architecture %s", goarch)
	}
	return nil
}

func parseSystemdVersion(output string) (int, error) {
	text := strings.TrimSpace(output)
	text = strings.TrimSpace(strings.TrimPrefix(text, "systemd"))
	end := 0
	for end < len(text) && text[end] >= '0' && text[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, errors.New("unable to determine systemd version")
	}
	version, err := strconv.Atoi(text[:end])
	if err != nil {
		return 0, fmt.Errorf("parse systemd version: %w", err)
	}
	return version, nil
}

func hasExactManagedMarker(content []byte) bool {
	firstLine, remainder, found := bytes.Cut(content, []byte{'\n'})
	if !found || len(remainder) == 0 {
		return false
	}
	firstLine = bytes.TrimSuffix(firstLine, []byte{'\r'})
	return bytes.Equal(firstLine, []byte(ManagedUnitMarker))
}

// inspectOwnedUnit 允许接管上一版官方 Shell 包装写入的精确 Unit。旧 Unit 没有
// managed marker，因此只能按完整规范化字节匹配；任何人工修改都继续视为外来对象。
func inspectOwnedUnit(path string) (exists bool, owned bool, err error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("inspect Server service unit ownership: %w", err)
	}
	if !info.Mode().IsRegular() {
		return true, false, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return true, false, fmt.Errorf("read Server service unit for legacy ownership: %w", err)
	}
	if hasExactManagedMarker(content) {
		return true, true, nil
	}
	normalize := func(content []byte) string {
		return strings.ReplaceAll(string(content), "\r\n", "\n")
	}
	return true, normalize(content) == normalize(legacyUnitFile), nil
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
	if err := os.Rename(temporaryPath, path); err != nil {
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
		return fmt.Errorf("open install source %s: %w", filepath.Base(source), err)
	}
	defer input.Close()
	return atomicWriteFile(path, mode, uid, gid, func(destination *os.File) error {
		_, err := io.Copy(destination, input)
		return err
	})
}
