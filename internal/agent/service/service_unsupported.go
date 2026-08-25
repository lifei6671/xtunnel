//go:build !linux && !windows

package service

import (
	"context"
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

func runIfManagedService(func(context.Context, string) error) (bool, error) {
	return false, nil
}

func replaceFile(source, destination string) error {
	return os.Rename(source, destination)
}
