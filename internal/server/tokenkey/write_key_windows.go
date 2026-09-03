//go:build windows

package tokenkey

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lifei6671/xtunnel/internal/server/winsecurity"
)

// writeKeyAtomicallyPlatform publishes the Token master key with the Windows
// foreground profile DACL. The publisher performs no-follow final verification
// and Write Through replacement; it never weakens the requirement to Unix mode
// bits or ordinary os.Rename semantics.
func writeKeyAtomicallyPlatform(directoryPath, keyPath string, key []byte) error {
	if filepath.Dir(keyPath) != directoryPath {
		return fmt.Errorf("tunnel token master key path must be directly beneath its credential directory")
	}
	security, err := winsecurity.NewForegroundFileSecurity()
	if err != nil {
		return fmt.Errorf("create tunnel token master key security policy: %w", err)
	}
	if err := winsecurity.PublishForegroundFile(directoryPath, filepath.Base(keyPath), key, security); err != nil {
		return fmt.Errorf("publish tunnel token master key: %w", err)
	}
	return nil
}

func loadExistingPlatform(directoryPath, keyPath string) (Key, bool, error) {
	if filepath.Dir(keyPath) != directoryPath {
		return Key{}, false, fmt.Errorf("tunnel token master key path must be directly beneath its credential directory")
	}
	if _, err := os.Lstat(directoryPath); errors.Is(err, os.ErrNotExist) {
		return Key{}, false, nil
	} else if err != nil {
		return Key{}, false, fmt.Errorf("inspect tunnel token credential directory: %w", err)
	}
	content, err := winsecurity.ReadForegroundFile(directoryPath, filepath.Base(keyPath))
	if errors.Is(err, os.ErrNotExist) {
		return Key{}, false, nil
	}
	if err != nil {
		return Key{}, false, fmt.Errorf("read tunnel token master key: %w", err)
	}
	if len(content) != Size {
		clear(content)
		return Key{}, false, fmt.Errorf("%w: key file has invalid length", ErrUnavailable)
	}
	var key Key
	copy(key[:], content)
	clear(content)
	return key, true, nil
}

func createCredentialDirectoryPlatform(dataDir, directoryPath string) error {
	if filepath.Dir(directoryPath) != dataDir {
		return fmt.Errorf("tunnel token credential directory must be directly beneath the server data directory")
	}
	if err := winsecurity.ValidateForegroundDirectory(dataDir); err != nil {
		return fmt.Errorf("validate server data directory before creating token credentials: %w", err)
	}
	security, err := winsecurity.NewForegroundDirectorySecurity()
	if err != nil {
		return fmt.Errorf("create tunnel token credential directory security policy: %w", err)
	}
	if err := winsecurity.CreateForegroundDirectory(directoryPath, security); err != nil {
		return fmt.Errorf("create tunnel token credential directory: %w", err)
	}
	return nil
}
