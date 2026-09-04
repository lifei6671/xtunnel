package workpool

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestWorkTLSCloseConcurrentDeadline 记录真实 TLS 的关闭期限竞争：Work.Close
// 已串行化 Close，但另一个取消 owner 仍可在 closeNotify 设置未来期限后覆盖它。
// 调度通道将覆盖固定在 TLS 写告警之前；这不是通过重复运行碰运气的竞态测试。
func TestWorkTLSCloseConcurrentDeadline(t *testing.T) {
	certificateServer := httptest.NewTLSServer(nil)
	certificate := certificateServer.TLS.Certificates[0]
	certificateServer.Close()
	for _, overwrite := range []bool{false, true} {
		name := "single_owner"
		if overwrite {
			name = "late_cancel_owner_deadline"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			serverWire, clientWire := net.Pipe()
			defer serverWire.Close()
			defer clientWire.Close()
			wire := &closeDeadlineWire{Conn: serverWire, reached: make(chan struct{}), resume: make(chan struct{}), ctx: ctx}
			server := tls.Server(wire, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13, SessionTicketsDisabled: true})
			// 仅使用 httptest 自带证书完成内存传输握手；本测试不验证 PKI 信任策略。
			client := tls.Client(clientWire, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
			handshake := make(chan error, 1)
			go func() { handshake <- server.HandshakeContext(ctx) }()
			clientErr := client.HandshakeContext(ctx)
			serverErr := <-handshake
			if err := errors.Join(clientErr, serverErr); err != nil {
				t.Fatal(err)
			}
			pool := newTestPool(t, 2, 2)
			work := registerTestWork(t, pool, 1, server)
			readDone := make(chan error, 1)
			go func() {
				var b [1]byte
				_, err := client.Read(b[:])
				readDone <- err
			}()
			cancelContext, cancelOwner := context.WithCancel(ctx)
			ownerDone := make(chan error, 1)
			wire.armed.Store(true)
			go func() {
				<-cancelContext.Done()
				select {
				case <-wire.reached:
					var err error
					if overwrite {
						err = server.SetDeadline(time.Now())
					}
					close(wire.resume)
					ownerDone <- err
				case <-ctx.Done():
					ownerDone <- ctx.Err()
				}
			}()
			// 与 ActiveWork 一致，先发取消，让并行代理 owner 与 Work.Close 相遇。
			cancelOwner()
			closeErr := work.Close()
			ownerErr := <-ownerDone
			peerErr := <-readDone
			if ownerErr != nil {
				t.Fatal(ownerErr)
			}
			if !errors.Is(peerErr, io.EOF) {
				t.Fatalf("peer close result: %v", peerErr)
			}
			if overwrite {
				if !errors.Is(closeErr, os.ErrDeadlineExceeded) || !strings.Contains(closeErr.Error(), "closeNotify") {
					t.Fatalf("late owner deadline must surface TLS closeNotify timeout: %v", closeErr)
				}
			} else if closeErr != nil {
				t.Fatalf("single owner TLS close: %v", closeErr)
			}
			if work.Close() != closeErr || wire.closed.Load() != 1 || pool.Snapshot().Total != 0 {
				t.Fatal("Work.Close did not retain exactly-once result and resource release")
			}
		})
	}
}

// closeDeadlineWire 只拦截握手后的未来写期限；TLS closeNotify 的真实写与错误
// 都由标准库执行。两个 goroutine 在测试返回前通过结果通道完成回收。
type closeDeadlineWire struct {
	net.Conn
	ctx     context.Context
	armed   atomic.Bool
	closed  atomic.Int32
	once    sync.Once
	reached chan struct{}
	resume  chan struct{}
}

func (wire *closeDeadlineWire) SetWriteDeadline(deadline time.Time) error {
	if err := wire.Conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	if wire.armed.Load() && time.Until(deadline) > time.Second {
		wire.once.Do(func() {
			close(wire.reached)
			select {
			case <-wire.resume:
			case <-wire.ctx.Done():
			}
		})
	}
	return nil
}

func (wire *closeDeadlineWire) Close() error {
	wire.closed.Add(1)
	return wire.Conn.Close()
}
