//go:build !windows

package proxy

import (
	"errors"
	"syscall"
)

// isDisconnectedRead 接收已经拆开的叶错误，识别读半边已断开的系统状态。
func isDisconnectedRead(err error) bool {
	return errors.Is(err, syscall.ENOTCONN)
}
