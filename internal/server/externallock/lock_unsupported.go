//go:build !linux && !windows

package externallock

import (
	"errors"
)

func acquire(string, string) (func() error, error) {
	return nil, errors.New("server external lock is only supported on Linux and Windows")
}
