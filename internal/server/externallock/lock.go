// Package externallock 实现 Server Stable Data Target 的进程互斥锁。
package externallock

import (
	"errors"
	"fmt"
)

// ErrAlreadyLocked 表示另一个 Server 已经持有同一 Stable Data Target 的锁。
var ErrAlreadyLocked = errors.New("server data target is already locked")

// RuntimeDirectory 返回平台固定的 Server 运行时目录。调用方必须传播解析错误，
// Windows 不得回退到环境变量或假定系统盘符。
func RuntimeDirectory() (string, error) {
	return runtimeDirectory()
}

// Lock 持有进程全生命周期的非阻塞 OS 文件锁。
type Lock struct {
	release func() error
}

// Acquire 在 runtimeDir 中获取指定 Stable Data Target Hash 的独占锁。
func Acquire(runtimeDir, targetHash string) (*Lock, error) {
	release, err := acquire(runtimeDir, targetHash)
	if err != nil {
		return nil, err
	}
	return &Lock{release: release}, nil
}

// Close 释放文件锁并关闭底层文件描述符。锁文件会保留供后续进程复用。
func (lock *Lock) Close() error {
	if err := lock.release(); err != nil {
		return fmt.Errorf("release server external lock: %w", err)
	}
	return nil
}
