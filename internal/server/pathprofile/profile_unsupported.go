//go:build !linux && !windows

package pathprofile

import "errors"

func resolveForeground(string) (Profile, error) {
	return Profile{}, errors.New("server path profiles are only supported on Linux and Windows")
}

func resolveService(string) (Profile, error) {
	return Profile{}, errors.New("server path profiles are only supported on Linux and Windows")
}
