//go:build linux

package externallock

import (
	"errors"
	"fmt"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func acquire(runtimeDir, targetHash string) (func() error, error) {
	var runtimeStat unix.Stat_t
	if err := unix.Lstat(runtimeDir, &runtimeStat); err != nil {
		return nil, fmt.Errorf("inspect runtime directory %q: %w", runtimeDir, err)
	}
	if runtimeStat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return nil, fmt.Errorf("runtime directory %q must be a real directory", runtimeDir)
	}
	if runtimeStat.Mode&0o777 != 0o700 {
		return nil, fmt.Errorf("runtime directory %q permissions are %04o, want 0700", runtimeDir, runtimeStat.Mode&0o777)
	}
	effectiveUID := unix.Geteuid()
	// root 需要复用固定 Runtime UID 的同一把锁执行离线维护命令。
	if effectiveUID != 0 && runtimeStat.Uid != uint32(effectiveUID) {
		return nil, fmt.Errorf("runtime directory %q is owned by uid %d, want %d", runtimeDir, runtimeStat.Uid, effectiveUID)
	}

	lockPath := filepath.Join(runtimeDir, "server-lock-"+targetHash+".lock")
	fd, err := unix.Open(lockPath, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open server external lock %q: %w", lockPath, err)
	}
	fail := func(cause error) (func() error, error) {
		return nil, errors.Join(cause, unix.Close(fd))
	}

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fail(fmt.Errorf("inspect server external lock %q: %w", lockPath, err))
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fail(fmt.Errorf("server external lock %q is not a regular file", lockPath))
	}
	// root 首次执行离线维护时创建的锁文件仍要归 Runtime UID，
	// 否则后续非 root Server 无法打开同一个 0600 文件。
	if effectiveUID == 0 && (stat.Uid != runtimeStat.Uid || stat.Gid != runtimeStat.Gid) {
		if err := unix.Fchown(fd, int(runtimeStat.Uid), int(runtimeStat.Gid)); err != nil {
			return fail(fmt.Errorf("set server external lock owner: %w", err))
		}
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		return fail(fmt.Errorf("set server external lock permissions: %w", err))
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return fail(fmt.Errorf("%w: %s", ErrAlreadyLocked, lockPath))
		}
		return fail(fmt.Errorf("lock server data target %q: %w", lockPath, err))
	}

	return func() error {
		unlockErr := unix.Flock(fd, unix.LOCK_UN)
		closeErr := unix.Close(fd)
		return errors.Join(unlockErr, closeErr)
	}, nil
}
