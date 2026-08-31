//go:build !linux && !windows

package service

import (
	"context"
	"io"
	"os"
)

func install(context.Context, string) error {
	return ErrUnsupported
}

func uninstall(context.Context) (UninstallResult, error) {
	return UninstallResult{}, ErrUnsupported
}

func platformName() string {
	return "xtunnel-agent"
}

func runIfManagedService(
	func() (string, bool, error),
	func(context.Context, string, io.Writer) error,
) (bool, error) {
	return false, nil
}

func replaceFile(source, destination string) error {
	return os.Rename(source, destination)
}
