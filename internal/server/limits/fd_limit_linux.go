//go:build linux

package limits

import "golang.org/x/sys/unix"

func currentFDLimit() (limit uint64, supported bool, err error) {
	var resource unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &resource); err != nil {
		return 0, true, err
	}
	return resource.Cur, true, nil
}
