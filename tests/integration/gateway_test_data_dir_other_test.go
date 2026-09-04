//go:build !windows

package integration

import "testing"

func newGatewayTestDataDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}
