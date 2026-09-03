//go:build !windows

package tokenkey

import (
	"os"
	"testing"
)

func prepareTestDataDir(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("os.Mkdir(%q) error = %v", path, err)
	}
}
