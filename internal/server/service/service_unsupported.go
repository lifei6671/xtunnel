//go:build !linux

package service

import "context"

func install(context.Context, string) error {
	return ErrUnsupported
}

func uninstall(context.Context) error {
	return ErrUnsupported
}
