//go:build windows

package gateway

import (
	"fmt"

	"github.com/lifei6671/xtunnel/internal/server/winsecurity"
)

func readPublicIdentityFiles(certPath, keyPath string) ([]byte, []byte, error) {
	certPEM, keyPEM, err := winsecurity.ReadOperatorTLSFiles(certPath, keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("validate public gateway TLS files: %w", err)
	}
	return certPEM, keyPEM, nil
}
