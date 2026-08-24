//go:build !linux

package externallock

import (
	"errors"
)

func acquire(string, string) (func() error, error) {
	return nil, errors.New("server external lock is only supported on Linux in XTunnel V0.1")
}
