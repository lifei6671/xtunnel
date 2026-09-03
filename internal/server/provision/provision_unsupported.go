//go:build !windows

package provision

import "errors"

var errUnsupported = errors.New("server init is only supported on Windows")

func initialize(string) error {
	return errUnsupported
}
