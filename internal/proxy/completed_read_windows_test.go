package proxy

import (
	"errors"
	"net"
	"os"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func TestCompletedCloseReadUsesNativeWinsockNotConnected(t *testing.T) {
	// net 初始化 Winsock；独立未连接 socket 确定地产生本机 shutdown errno，
	// 不依赖真实 TCP 完成 FIN 时的调度时序，也不连接任何外部服务。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	socket, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := syscall.Closesocket(socket); err != nil {
			t.Errorf("close socket: %v", err)
		}
	})
	shutdownErr := syscall.Shutdown(socket, syscall.SHUT_RD)
	if !errors.Is(shutdownErr, windows.WSAENOTCONN) {
		t.Fatalf("native disconnected shutdown = %v, want WSAENOTCONN", shutdownErr)
	}
	wrapped := &net.OpError{Op: "close", Net: "tcp", Err: os.NewSyscallError("shutdown", shutdownErr)}
	readSide, readPeer := net.Pipe()
	writeSide, writePeer := net.Pipe()
	defer readSide.Close()
	defer writeSide.Close()
	defer writePeer.Close()
	if err := readPeer.Close(); err != nil {
		t.Fatal(err)
	}
	destination := &trackingCloseWriter{Conn: writeSide}
	result := make(chan copyResult, 1)
	proxyOneWay("native", destination, &failingCloseReader{Conn: readSide, err: wrapped}, result)
	got := <-result
	if got.err != nil || got.cleanupErr != nil || destination.calls != 1 {
		t.Fatalf("EOF cleanup = %#v, CloseWrite calls=%d", got, destination.calls)
	}
	for _, failure := range []error{windows.WSAECONNRESET, windows.WSAECONNABORTED, windows.WSAETIMEDOUT} {
		if isCompletedCloseRead(&net.OpError{Op: "close", Net: "tcp", Err: failure}) {
			t.Fatalf("real socket failure classified as completed: %v", failure)
		}
		if isCompletedCloseRead(errors.Join(wrapped, failure)) {
			t.Fatalf("completed read suppressed joined socket failure: %v", failure)
		}
	}
}
