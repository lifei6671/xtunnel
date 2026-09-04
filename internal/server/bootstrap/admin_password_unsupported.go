//go:build !linux && !windows

package bootstrap

import (
	"errors"
	"io"
	"os"
)

func readAdminPasswordFromTTY(*os.File, io.Writer) (string, error) {
	return "", errors.New("interactive admin password input is only supported on Linux and Windows; use --password-file")
}
