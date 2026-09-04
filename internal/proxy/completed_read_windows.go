package proxy

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// isDisconnectedRead 接收已经拆开的叶错误，识别读半边已断开的系统状态。
// Winsock 的 WSAENOTCONN 与 Go 的 POSIX ENOTCONN 数值不同；二者都表示已完成
// 断开。此判断不用于复制错误，也不接受连接 reset/abort 等真实传输失败。
func isDisconnectedRead(err error) bool {
	return errors.Is(err, syscall.ENOTCONN) ||
		errors.Is(err, windows.WSAENOTCONN)
}
