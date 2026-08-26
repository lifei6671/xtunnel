package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/safego"
)

const proxyTestTimeout = 3 * time.Second

func TestProxyBidirectionalPreservesBytesAndHalfClose(t *testing.T) {
	leftProxy, leftPeer := tcpPair(t)
	rightProxy, rightPeer := tcpPair(t)
	defer leftPeer.Close()
	defer rightPeer.Close()

	result := make(chan error, 1)
	go func() { result <- ProxyBidirectional(context.Background(), leftProxy, rightProxy) }()

	request := bytes.Repeat([]byte("request-字节-"), 1024)
	if _, err := leftPeer.Write(request); err != nil {
		t.Fatalf("write request: %v", err)
	}
	if err := leftPeer.CloseWrite(); err != nil {
		t.Fatalf("left peer CloseWrite: %v", err)
	}
	gotRequest, err := io.ReadAll(rightPeer)
	if err != nil {
		t.Fatalf("read request: %v", err)
	}
	if !bytes.Equal(gotRequest, request) {
		t.Fatalf("request length/content changed: got=%d want=%d", len(gotRequest), len(request))
	}

	// 左到右已经 EOF 后，右到左仍必须保持可用，证明代理没有把单边 EOF
	// 错误升级为整个连接关闭。
	response := bytes.Repeat([]byte("response-响应-"), 1024)
	if _, err := rightPeer.Write(response); err != nil {
		t.Fatalf("write response after opposite EOF: %v", err)
	}
	if err := rightPeer.CloseWrite(); err != nil {
		t.Fatalf("right peer CloseWrite: %v", err)
	}
	gotResponse, err := io.ReadAll(leftPeer)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !bytes.Equal(gotResponse, response) {
		t.Fatalf("response length/content changed: got=%d want=%d", len(gotResponse), len(response))
	}

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("ProxyBidirectional() error = %v", err)
		}
	case <-time.After(proxyTestTimeout):
		t.Fatal("ProxyBidirectional() did not finish after both half-closes")
	}
}

func TestProxyBidirectionalCloseReadFailureDoesNotInterruptOppositeDirection(t *testing.T) {
	leftProxy, leftPeer := tcpPair(t)
	rightProxy, rightPeer := tcpPair(t)
	defer leftPeer.Close()
	defer rightPeer.Close()

	readErr := errors.New("close read failed")
	left := &failingCloseReadTCPConn{TCPConn: leftProxy, err: readErr}
	result := make(chan error, 1)
	go func() { result <- ProxyBidirectional(context.Background(), left, rightProxy) }()

	request := []byte("request")
	if _, err := leftPeer.Write(request); err != nil {
		t.Fatalf("write request: %v", err)
	}
	if err := leftPeer.CloseWrite(); err != nil {
		t.Fatalf("left peer CloseWrite: %v", err)
	}
	gotRequest, err := io.ReadAll(rightPeer)
	if err != nil {
		t.Fatalf("read request: %v", err)
	}
	if !bytes.Equal(gotRequest, request) {
		t.Fatalf("request changed: got=%q want=%q", gotRequest, request)
	}

	response := []byte("response after CloseRead failure")
	if _, err := rightPeer.Write(response); err != nil {
		t.Fatalf("write response after CloseRead failure: %v", err)
	}
	if err := rightPeer.CloseWrite(); err != nil {
		t.Fatalf("right peer CloseWrite: %v", err)
	}
	gotResponse, err := io.ReadAll(leftPeer)
	if err != nil {
		t.Fatalf("read response after CloseRead failure: %v", err)
	}
	if !bytes.Equal(gotResponse, response) {
		t.Fatalf("response changed: got=%q want=%q", gotResponse, response)
	}

	select {
	case err := <-result:
		if !errors.Is(err, readErr) {
			t.Fatalf("ProxyBidirectional() error = %v, want CloseRead error", err)
		}
	case <-time.After(proxyTestTimeout):
		t.Fatal("ProxyBidirectional() did not finish after both half-closes")
	}
}

func TestProxyBidirectionalContextCancelUnblocksBothDirections(t *testing.T) {
	leftProxy, leftPeer := tcpPair(t)
	rightProxy, rightPeer := tcpPair(t)
	defer leftPeer.Close()
	defer rightPeer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- ProxyBidirectional(ctx, leftProxy, rightProxy) }()

	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ProxyBidirectional(cancel) error = %v, want context.Canceled", err)
		}
	case <-time.After(proxyTestTimeout):
		t.Fatal("ProxyBidirectional(cancel) left blocked goroutines")
	}
}

func TestProxyBidirectionalOriginResetUnblocksOppositeDirection(t *testing.T) {
	leftProxy, leftPeer := tcpPair(t)
	rightProxy, rightPeer := tcpPair(t)
	defer leftPeer.Close()
	result := make(chan error, 1)
	go func() { result <- ProxyBidirectional(context.Background(), leftProxy, rightProxy) }()

	// Linger=0 让测试端发出 TCP RST，模拟 Origin 进程崩溃。任一方向遇到致命
	// IO 错误后，统一关闭路径必须解除另一个仍阻塞的 io.Copy。
	if err := rightPeer.SetLinger(0); err != nil {
		t.Fatalf("right peer SetLinger(0): %v", err)
	}
	if err := rightPeer.Close(); err != nil {
		t.Fatalf("right peer reset close: %v", err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("ProxyBidirectional(origin reset) error = nil")
		}
	case <-time.After(proxyTestTimeout):
		t.Fatal("ProxyBidirectional(origin reset) left the opposite copy blocked")
	}
}

func TestProxyBidirectionalPanicInterruptsOppositeDirection(t *testing.T) {
	leftProxy, leftPeer := tcpPair(t)
	rightProxy, rightPeer := tcpPair(t)
	defer leftPeer.Close()
	defer rightPeer.Close()

	result := make(chan error, 1)
	go func() {
		result <- ProxyBidirectional(context.Background(), &panicReadTCPConn{Conn: leftProxy, tcp: leftProxy}, rightProxy)
	}()

	select {
	case err := <-result:
		if !errors.Is(err, safego.ErrPanic) {
			t.Fatalf("ProxyBidirectional(panic) error = %v, want safego.ErrPanic", err)
		}
	case <-time.After(proxyTestTimeout):
		t.Fatal("ProxyBidirectional(panic) left the opposite copy blocked")
	}
}

func TestProxyBidirectionalRejectsConnectionWithoutHalfClose(t *testing.T) {
	left, leftPeer := net.Pipe()
	right, rightPeer := net.Pipe()
	defer left.Close()
	defer leftPeer.Close()
	defer right.Close()
	defer rightPeer.Close()

	if err := ProxyBidirectional(context.Background(), left, right); !errors.Is(err, ErrHalfCloseUnsupported) {
		t.Fatalf("ProxyBidirectional(net.Pipe) error = %v, want ErrHalfCloseUnsupported", err)
	}
}

func TestProxyBidirectionalRejectsInvalidInput(t *testing.T) {
	left, peer := net.Pipe()
	defer left.Close()
	defer peer.Close()
	for name, test := range map[string]struct {
		ctx   context.Context
		left  net.Conn
		right net.Conn
	}{
		"nil context":    {ctx: nil, left: left, right: left},
		"nil left":       {ctx: context.Background(), right: left},
		"nil right":      {ctx: context.Background(), left: left},
		"both nil conns": {ctx: context.Background()},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ProxyBidirectional(test.ctx, test.left, test.right); !errors.Is(err, ErrInvalidConnection) {
				t.Fatalf("ProxyBidirectional() error = %v, want ErrInvalidConnection", err)
			}
		})
	}
}

func TestProxyOneWayAttemptsCloseWriteWhenCloseReadFails(t *testing.T) {
	readSide, readPeer := net.Pipe()
	writeSide, writePeer := net.Pipe()
	defer readSide.Close()
	defer writeSide.Close()
	defer writePeer.Close()
	if err := readPeer.Close(); err != nil {
		t.Fatalf("close read peer: %v", err)
	}

	readErr := errors.New("close read failed")
	source := &failingCloseReader{Conn: readSide, err: readErr}
	destination := &trackingCloseWriter{Conn: writeSide}
	result := make(chan copyResult, 1)
	proxyOneWay("test", destination, source, result)

	got := <-result
	if got.err != nil {
		t.Fatalf("proxyOneWay() fatal error = %v, want nil", got.err)
	}
	if !errors.Is(got.cleanupErr, readErr) {
		t.Fatalf("proxyOneWay() cleanup error = %v, want CloseRead error", got.cleanupErr)
	}
	if destination.calls != 1 {
		t.Fatalf("destination CloseWrite calls = %d, want 1", destination.calls)
	}
}

func TestProxyOneWayIgnoresDisconnectedCloseRead(t *testing.T) {
	readSide, readPeer := net.Pipe()
	writeSide, writePeer := net.Pipe()
	defer readSide.Close()
	defer writeSide.Close()
	defer writePeer.Close()
	if err := readPeer.Close(); err != nil {
		t.Fatalf("close read peer: %v", err)
	}

	// 使用与 net.TCPConn.CloseRead 在 Linux 上一致的错误包装链，确保判断依赖
	// errors.Is 的系统错误语义，而不是脆弱的错误字符串。
	disconnectedErr := &net.OpError{
		Op:  "close",
		Net: "tcp",
		Err: os.NewSyscallError("shutdown", syscall.ENOTCONN),
	}
	source := &failingCloseReader{Conn: readSide, err: disconnectedErr}
	destination := &trackingCloseWriter{Conn: writeSide}
	result := make(chan copyResult, 1)
	proxyOneWay("test", destination, source, result)

	got := <-result
	if got.err != nil {
		t.Fatalf("proxyOneWay() fatal error = %v，want nil", got.err)
	}
	if got.cleanupErr != nil {
		t.Fatalf("proxyOneWay() cleanup error = %v，want nil", got.cleanupErr)
	}
	if destination.calls != 1 {
		t.Fatalf("destination CloseWrite calls = %d，want 1", destination.calls)
	}
}

func TestProxyOneWayReportsCloseWriteFailureAsFatal(t *testing.T) {
	readSide, readPeer := net.Pipe()
	writeSide, writePeer := net.Pipe()
	defer readSide.Close()
	defer writeSide.Close()
	defer writePeer.Close()
	if err := readPeer.Close(); err != nil {
		t.Fatalf("close read peer: %v", err)
	}

	writeErr := errors.New("close write failed")
	destination := &trackingCloseWriter{Conn: writeSide, err: writeErr}
	result := make(chan copyResult, 1)
	proxyOneWay("test", destination, readSide, result)

	got := <-result
	if !errors.Is(got.err, writeErr) {
		t.Fatalf("proxyOneWay() fatal error = %v, want CloseWrite error", got.err)
	}
	if got.cleanupErr != nil {
		t.Fatalf("proxyOneWay() cleanup error = %v, want nil", got.cleanupErr)
	}
}

type failingCloseReader struct {
	net.Conn
	err error
}

type panicReadTCPConn struct {
	net.Conn
	tcp *net.TCPConn
}

func (*panicReadTCPConn) Read([]byte) (int, error) {
	panic("injected proxy read panic")
}

func (connection *panicReadTCPConn) CloseWrite() error {
	return connection.tcp.CloseWrite()
}

func (connection *failingCloseReader) CloseRead() error { return connection.err }

type failingCloseReadTCPConn struct {
	*net.TCPConn
	err error
}

func (connection *failingCloseReadTCPConn) CloseRead() error { return connection.err }

type trackingCloseWriter struct {
	net.Conn
	calls int
	err   error
}

func (connection *trackingCloseWriter) CloseWrite() error {
	connection.calls++
	return connection.err
}

func tcpPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("ListenTCP: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	type dialResult struct {
		connection *net.TCPConn
		err        error
	}
	dialed := make(chan dialResult, 1)
	go func() {
		connection, dialErr := net.DialTCP("tcp", nil, listener.Addr().(*net.TCPAddr))
		dialed <- dialResult{connection: connection, err: dialErr}
	}()
	accepted, err := listener.AcceptTCP()
	if err != nil {
		t.Fatalf("AcceptTCP: %v", err)
	}
	peer := <-dialed
	if peer.err != nil {
		accepted.Close()
		t.Fatalf("DialTCP: %v", peer.err)
	}
	return accepted, peer.connection
}
