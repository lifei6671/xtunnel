//go:build !windows

package gateway

import "testing"

func newGatewayTestDataDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}
