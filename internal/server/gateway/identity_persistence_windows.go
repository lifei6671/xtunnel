//go:build windows

package gateway

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lifei6671/xtunnel/internal/server/winsecurity"
)

// validatePinnedIdentityFiles refuses to inspect a Windows pinned identity
// through os.Stat: that would follow a reparse point before the protected
// no-follow reader has validated the object boundary.
func validatePinnedIdentityFiles(dataDir, keyPath, certPath string) error {
	if err := validatePinnedIdentityDataDirectory(dataDir); err != nil {
		return fmt.Errorf("validate server data directory before reading gateway pinned identity: %w", err)
	}
	if _, err := readPinnedIdentityFile(keyPath); errors.Is(err, os.ErrNotExist) {
		return ErrIdentityMissing
	} else if err != nil {
		return fmt.Errorf("inspect gateway private key: %w", err)
	}
	if _, err := readPinnedIdentityFile(certPath); errors.Is(err, os.ErrNotExist) {
		return ErrIdentityMissing
	} else if err != nil {
		return fmt.Errorf("inspect gateway certificate: %w", err)
	}
	return nil
}

func validatePinnedIdentityDataDirectory(dataDir string) error {
	return winsecurity.ValidateForegroundDirectory(dataDir)
}

// The selected Windows profile owns every pinned identity object. Public
// TLS deliberately bypasses this adapter because its files remain operator
// owned and are validated by a separate M8-03 boundary.
func createPinnedIdentityDirectory(dataDir, directory string) error {
	if filepath.Dir(directory) != dataDir {
		return errors.New("gateway PKI directory must be directly beneath the server data directory")
	}
	if err := winsecurity.ValidateForegroundDirectory(dataDir); err != nil {
		return fmt.Errorf("validate server data directory before creating gateway PKI: %w", err)
	}
	security, err := winsecurity.NewDirectorySecurityForPath(directory)
	if err != nil {
		return fmt.Errorf("create gateway PKI directory security policy: %w", err)
	}
	if err := winsecurity.CreateForegroundDirectory(directory, security); err != nil {
		return err
	}
	return nil
}

func readPinnedIdentityFile(path string) ([]byte, error) {
	return winsecurity.ReadForegroundFile(filepath.Dir(path), filepath.Base(path))
}

func writePinnedIdentity(dataDir, keyPath, certPath string, certificate tlsCertificate) error {
	directory := filepath.Dir(keyPath)
	if filepath.Dir(keyPath) != filepath.Dir(certPath) || filepath.Dir(directory) != dataDir {
		return errors.New("gateway pinned identity files must share a PKI directory directly beneath the server data directory")
	}
	if err := validatePinnedIdentityDataDirectory(dataDir); err != nil {
		return fmt.Errorf("validate server data directory before publishing gateway pinned identity: %w", err)
	}
	keyPEM, certPEM, err := pinnedIdentityPEM(certificate)
	if err != nil {
		return err
	}
	security, err := winsecurity.NewFileSecurityForPath(directory)
	if err != nil {
		return fmt.Errorf("create gateway pinned identity security policy: %w", err)
	}
	if err := winsecurity.PublishForegroundFile(directory, filepath.Base(keyPath), keyPEM, security); err != nil {
		return fmt.Errorf("publish gateway private key: %w", err)
	}
	if err := winsecurity.PublishForegroundFile(directory, filepath.Base(certPath), certPEM, security); err != nil {
		return fmt.Errorf("publish gateway certificate: %w", err)
	}
	return nil
}

func replacePinnedCertificate(dataDir, directory, certPath string, certificate tlsCertificate) error {
	if filepath.Dir(directory) != dataDir || filepath.Dir(certPath) != directory {
		return errors.New("gateway pinned certificate must be directly beneath its PKI directory")
	}
	if err := validatePinnedIdentityDataDirectory(dataDir); err != nil {
		return fmt.Errorf("validate server data directory before renewing gateway pinned certificate: %w", err)
	}
	_, certPEM, err := pinnedIdentityPEM(certificate)
	if err != nil {
		return err
	}
	security, err := winsecurity.NewFileSecurityForPath(directory)
	if err != nil {
		return fmt.Errorf("create gateway pinned certificate security policy: %w", err)
	}
	return winsecurity.PublishForegroundFile(directory, filepath.Base(certPath), certPEM, security)
}
