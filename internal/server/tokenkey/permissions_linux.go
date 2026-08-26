//go:build linux

package tokenkey

import "os"

func directoryPermissionsValid(mode os.FileMode) bool {
	return mode.Perm() == 0o700
}

func keyPermissionsValid(mode os.FileMode) bool {
	return mode.Perm() == 0o600
}
