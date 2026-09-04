//go:build !windows

package gateway

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func validatePinnedIdentityFiles(_ string, keyPath, certPath string) error {
	info, err := os.Stat(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		return ErrIdentityMissing
	}
	if err != nil {
		return fmt.Errorf("inspect gateway private key: %w", err)
	}
	if !privateKeyPermissionsValid(info.Mode()) {
		return fmt.Errorf("gateway private key permissions are %04o, want 0600", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(keyPath), certFileName)); errors.Is(err, os.ErrNotExist) {
		return ErrIdentityMissing
	} else if err != nil {
		return fmt.Errorf("inspect gateway certificate: %w", err)
	}
	return nil
}

func validatePinnedIdentityDataDirectory(string) error { return nil }

func createPinnedIdentityDirectory(_ string, directory string) error {
	return os.MkdirAll(directory, 0o700)
}

func readPinnedIdentityFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func writePinnedIdentity(_ string, keyPath, certPath string, certificate tlsCertificate) error {
	return writeIdentity(keyPath, certPath, certificate)
}

func replacePinnedCertificate(_ string, directory, certPath string, certificate tlsCertificate) error {
	return replaceCertificateAtomically(directory, certPath, certificate)
}
