//go:build !linux

package service

func syncDirectory(string) error {
	return nil
}
