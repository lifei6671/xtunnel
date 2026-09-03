//go:build !windows

package gateway

import (
	"fmt"
	"os"
)

func readPublicIdentityFiles(certPath, keyPath string) ([]byte, []byte, error) {
	info, err := os.Stat(keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect public gateway private key: %w", err)
	}
	if !privateKeyPermissionsValid(info.Mode()) {
		return nil, nil, fmt.Errorf("public gateway private key permissions are %04o, want 0600", info.Mode().Perm())
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read public gateway certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read public gateway private key: %w", err)
	}
	return certPEM, keyPEM, nil
}
