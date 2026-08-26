// Package proxy 提供进入 RAW 后不再解释业务字节的双向流代理。
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/lifei6671/xtunnel/internal/safego"
)

var (
	// ErrInvalidConnection 表示调用方没有提供完整的双向连接。
	ErrInvalidConnection = errors.New("bidirectional proxy connection is invalid")
	// ErrHalfCloseUnsupported 表示目标连接无法在保留反向流的同时发送写端 EOF。
	ErrHalfCloseUnsupported = errors.New("bidirectional proxy connection does not support half-close")
)

type closeWriter interface {
	CloseWrite() error
}

type closeReader interface {
	CloseRead() error
}

type copyResult struct {
	direction  string
	err        error
	cleanupErr error
}

// ProxyBidirectional 在两个连接之间逐字节转发，直到双向都收到 EOF、发生 fatal error
// 或 Context 取消。
//
// 单向 EOF 只关闭目标连接的写半边，反方向仍可继续传输。fatal error 与 Context
// Cancel 共用一次性中断路径：先把两端 Deadline 推到当前时间，再 Close 两端，以解除
// 所有阻塞 Read/Write；函数在两个复制 goroutine 都退出前绝不返回。
func ProxyBidirectional(ctx context.Context, left, right net.Conn) error {
	if ctx == nil || left == nil || right == nil {
		return ErrInvalidConnection
	}
	if _, ok := left.(closeWriter); !ok {
		return fmt.Errorf("%w: left", ErrHalfCloseUnsupported)
	}
	if _, ok := right.(closeWriter); !ok {
		return fmt.Errorf("%w: right", ErrHalfCloseUnsupported)
	}

	results := make(chan copyResult, 2)
	startProxyOneWay("left_to_right", right, left, results)
	startProxyOneWay("right_to_left", left, right, results)

	var (
		closeOnce   sync.Once
		closeErr    error
		copyErrs    []error
		cleanupErrs []error
		terminal    error
	)
	interrupt := func() {
		closeOnce.Do(func() {
			now := time.Now()
			leftDeadlineErr := left.SetDeadline(now)
			rightDeadlineErr := right.SetDeadline(now)
			leftCloseErr := left.Close()
			rightCloseErr := right.Close()
			closeErr = errors.Join(
				wrapNetworkError("set left deadline", leftDeadlineErr),
				wrapNetworkError("set right deadline", rightDeadlineErr),
				wrapNetworkError("close left", leftCloseErr),
				wrapNetworkError("close right", rightCloseErr),
			)
		})
	}

	completed := 0
	for completed < 2 {
		select {
		case result := <-results:
			completed++
			if result.cleanupErr != nil {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("proxy %s: %w", result.direction, result.cleanupErr))
			}
			if result.err != nil {
				copyErrs = append(copyErrs, fmt.Errorf("proxy %s: %w", result.direction, result.err))
				if terminal == nil {
					terminal = result.err
					interrupt()
				}
			}
		case <-ctx.Done():
			if terminal == nil {
				terminal = ctx.Err()
				interrupt()
			}
		}
	}
	// 正常双向 EOF 也由同一个 CloseOnce 完成最终资源回收；此时 Deadline 只用于
	// 保证任何并发尾部 IO 都不会在 Close 前重新阻塞。
	interrupt()
	if terminal != nil {
		return errors.Join(terminal, errors.Join(copyErrs...), errors.Join(cleanupErrs...), closeErr)
	}
	return errors.Join(errors.Join(cleanupErrs...), closeErr)
}

func startProxyOneWay(direction string, destination, source net.Conn, results chan<- copyResult) {
	safego.Go(
		func(err error) {
			results <- copyResult{direction: direction, err: err}
		},
		nil,
		func() {
			proxyOneWay(direction, destination, source, results)
		},
	)
}

func proxyOneWay(direction string, destination, source net.Conn, results chan<- copyResult) {
	_, err := io.Copy(destination, source)
	var cleanupErr error
	if err == nil {
		// io.Copy 返回 nil 代表源端正常 EOF。先关闭可选的读半边，再关闭目标写半边，
		// 让对端看到 EOF，但继续保留目标到源端的反向响应路径。
		if reader, ok := source.(closeReader); ok {
			if closeErr := reader.CloseRead(); !isCompletedCloseRead(closeErr) {
				cleanupErr = fmt.Errorf("close source read half: %w", closeErr)
			}
		}
		if closeErr := destination.(closeWriter).CloseWrite(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			err = fmt.Errorf("close destination write half: %w", closeErr)
		}
	}
	results <- copyResult{direction: direction, err: err, cleanupErr: cleanupErr}
}

func isCompletedCloseRead(err error) bool {
	// io.Copy 返回 nil 已经证明源端读到了正常 EOF。Linux 在 TCP 对端完成
	// Half-Close 后再次 shutdown(SHUT_RD) 可能返回 ENOTCONN；这表示读半边
	// 已经没有可关闭的连接，是 CloseRead 的幂等终态，不应把成功的数据转发
	// 重新判为失败。其他错误仍需延迟到反向复制结束后报告，避免掩盖真实清理故障。
	return err == nil || errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.ENOTCONN)
}

func wrapNetworkError(operation string, err error) error {
	if err == nil || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}
