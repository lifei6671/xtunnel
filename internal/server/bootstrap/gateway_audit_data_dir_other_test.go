//go:build !windows

package bootstrap

import "testing"

func newGatewayAuditTestDataDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}
