package origin

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// Winsock 的拒绝连接错误码不同于 Go 的 POSIX errno，二者均保留错误链匹配。
func isConnectionRefused(err error) bool {
	return errors.Is(err, windows.WSAECONNREFUSED) || errors.Is(err, syscall.ECONNREFUSED)
}
