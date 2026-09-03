//go:build !linux && !windows

package externallock

import "errors"

func runtimeDirectory() (string, error) {
	return "", errors.New("server runtime directory is only supported on Linux and Windows")
}
