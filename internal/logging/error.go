package logging

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
)

// ErrorDetail 返回可以安全写入日志的底层原因。fallback 必须是调用点的固定描述，
// 不能来自错误文本或远端输入。包装错误可能包含 Token、Header 或地址，因此只提取
// 类型化的系统错误和已知状态，不调用任意 error 的 Error 方法。
func ErrorDetail(err error, fallback string) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		// Errno 文本由操作系统按数值生成，不包含连接地址、认证材料或包装文本。
		return fmt.Sprintf("system error %d: %s", uintptr(errno), errno.Error())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "operation timed out"
	}
	if errors.Is(err, context.Canceled) {
		return "operation canceled"
	}
	if errors.Is(err, net.ErrClosed) {
		return "network connection already closed"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		if dns.IsTimeout {
			return "DNS lookup timed out"
		}
		if dns.IsNotFound {
			return "DNS name not found"
		}
		return "DNS lookup failed"
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return "network operation timed out"
	}
	var record tls.RecordHeaderError
	if errors.As(err, &record) {
		return "invalid TLS record received"
	}
	var authority x509.UnknownAuthorityError
	if errors.As(err, &authority) {
		return "TLS certificate authority is not trusted"
	}
	var hostname x509.HostnameError
	if errors.As(err, &hostname) {
		return "TLS certificate does not match server name"
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return "peer closed connection before the message was complete"
	}
	if errors.Is(err, io.EOF) {
		return "peer closed connection (EOF)"
	}
	return fallback
}
