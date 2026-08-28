package httpingress

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

const webSocketIdleTimeout = time.Hour

// webSocketIdleOwner 让 Client 与 Tunnel backend 共用一个 sliding idle 窗口。
// 任一方向成功传输字节都会同时推进两端 Deadline；完整生命周期没有总时限，
// 但双方都无字节进展满一小时后，阻塞 IO 会超时并由 ReverseProxy 关闭两端。
type webSocketIdleOwner struct {
	timeout time.Duration

	mu              sync.Mutex
	client          net.Conn
	backend         net.Conn
	deadline        time.Time
	deadlineVersion uint64
	applying        bool
}

func newWebSocketIdleOwner(timeout time.Duration) *webSocketIdleOwner {
	return &webSocketIdleOwner{timeout: timeout}
}

func (owner *webSocketIdleOwner) bindClient(connection net.Conn) error {
	owner.mu.Lock()
	owner.client = connection
	owner.mu.Unlock()
	return owner.touch()
}

func (owner *webSocketIdleOwner) bindBackend(connection net.Conn) error {
	owner.mu.Lock()
	owner.backend = connection
	owner.mu.Unlock()
	return owner.touch()
}

func (owner *webSocketIdleOwner) touch() error {
	owner.mu.Lock()
	if owner.timeout <= 0 {
		owner.mu.Unlock()
		return errors.New("websocket idle timeout is invalid")
	}
	candidate := time.Now().Add(owner.timeout)
	if owner.deadline.Before(candidate) {
		owner.deadline = candidate
	}
	owner.deadlineVersion++
	if owner.applying {
		owner.mu.Unlock()
		return nil
	}
	owner.applying = true
	owner.mu.Unlock()

	// 只有一个 applier 串行发布 Deadline；其他方向只更新期望版本。SetDeadline
	// 始终在 owner 锁外执行，随后若发现新版本就再次发布，因此较早 touch 不能在
	// 较晚 touch 之后把窗口写回更早时间。
	for {
		owner.mu.Lock()
		client := owner.client
		backend := owner.backend
		deadline := owner.deadline
		version := owner.deadlineVersion
		owner.mu.Unlock()

		var clientErr, backendErr error
		if client != nil {
			clientErr = client.SetDeadline(deadline)
		}
		if backend != nil {
			backendErr = backend.SetDeadline(deadline)
		}
		applyErr := errors.Join(clientErr, backendErr)

		owner.mu.Lock()
		if applyErr != nil || owner.deadlineVersion == version {
			owner.applying = false
			owner.mu.Unlock()
			return applyErr
		}
		owner.mu.Unlock()
	}
}

// webSocketActivityConn 保留 net.Conn/CloseWrite 能力，只在真实字节进展后续期。
// SetDeadline 由 owner 在锁外调用，因此并发复制不会把网络 IO 放进 owner 锁内。
type webSocketActivityConn struct {
	net.Conn
	idle *webSocketIdleOwner
}

func (connection *webSocketActivityConn) Read(buffer []byte) (int, error) {
	count, readErr := connection.Conn.Read(buffer)
	if count == 0 {
		return count, readErr
	}
	return count, errors.Join(readErr, connection.idle.touch())
}

func (connection *webSocketActivityConn) Write(buffer []byte) (int, error) {
	count, writeErr := connection.Conn.Write(buffer)
	if count == 0 {
		return count, writeErr
	}
	return count, errors.Join(writeErr, connection.idle.touch())
}

func (connection *webSocketActivityConn) CloseWrite() error {
	closeWriter, ok := connection.Conn.(interface{ CloseWrite() error })
	if !ok {
		return fmt.Errorf("websocket close write: %w", http.ErrNotSupported)
	}
	return closeWriter.CloseWrite()
}

// webSocketResponseWriter 在 ReverseProxy Hijack 时接管客户端连接；Unwrap 保留
// 普通错误响应需要的可选 ResponseWriter 能力，Hijack 则返回带 activity tracking
// 的 net.Conn，使前后端共用同一个 idle owner。
type webSocketResponseWriter struct {
	http.ResponseWriter
	idle *webSocketIdleOwner
}

func (writer *webSocketResponseWriter) Unwrap() http.ResponseWriter {
	return writer.ResponseWriter
}

func (writer *webSocketResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	connection, buffered, err := http.NewResponseController(writer.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	wrapped := &webSocketActivityConn{Conn: connection, idle: writer.idle}
	if err := writer.idle.bindClient(wrapped); err != nil {
		return nil, nil, errors.Join(err, connection.Close())
	}
	return wrapped, buffered, nil
}
