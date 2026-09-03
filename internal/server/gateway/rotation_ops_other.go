//go:build !windows

package gateway

import (
	"errors"
	"os"
)

func defaultRotationFileOps() rotationFileOps {
	return rotationFileOps{writeFileSync: writeFileSync, rename: os.Rename, remove: os.Remove, syncDirectory: syncDirectory}
}

func rotationTemporaryExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func removePinnedIdentityFile(path string) error { return os.Remove(path) }
