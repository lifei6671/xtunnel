//go:build windows

package gateway

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lifei6671/xtunnel/internal/server/winsecurity"
)

func defaultRotationFileOps() rotationFileOps {
	return rotationFileOps{
		writeFileSync: writePinnedRotationFile,
		rename:        replacePinnedRotationFile,
		remove:        removePinnedIdentityFile,
		syncDirectory: syncPinnedRotationDirectory,
	}
}

// syncPinnedRotationDirectory is intentionally a no-op on Windows. Every
// successful Journal publication and replacement already uses MoveFileEx with
// WRITE_THROUGH, while DeleteFile has no equivalent supported directory flush.
// A stale Journal after a sudden power loss is safe: the committed audit event
// is reconciled idempotently before any new rotation can start.
func syncPinnedRotationDirectory(string) error { return nil }

func writePinnedRotationFile(path string, data []byte, _ os.FileMode) error {
	security, err := winsecurity.NewForegroundFileSecurity()
	if err != nil {
		return err
	}
	return winsecurity.PublishForegroundFile(filepath.Dir(path), filepath.Base(path), data, security)
}

func replacePinnedRotationFile(source, destination string) error {
	if filepath.Dir(source) != filepath.Dir(destination) {
		return errors.New("gateway rotation replacement crosses PKI directories")
	}
	security, err := winsecurity.NewForegroundFileSecurity()
	if err != nil {
		return err
	}
	return winsecurity.ReplaceForegroundFile(filepath.Dir(source), filepath.Base(source), filepath.Base(destination), security)
}

func rotationTemporaryExists(path string) (bool, error) {
	_, err := readPinnedIdentityFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read gateway rotation temporary file: %w", err)
	}
	return true, nil
}

func removePinnedIdentityFile(path string) error {
	security, err := winsecurity.NewForegroundFileSecurity()
	if err != nil {
		return err
	}
	return winsecurity.DeleteForegroundFile(filepath.Dir(path), filepath.Base(path), security)
}
